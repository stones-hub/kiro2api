// Package discount tracks successful input prefixes for local billing discounts.
// It does not cache responses and makes no claim about Kiro's backend cache.
package discount

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"sync"
	"time"

	"kiro2api/types"
	"kiro2api/utils"
)

const maxRequestPrefixes = 128

type point struct {
	key    [32]byte
	tokens int
}
type entry struct {
	key     [32]byte
	expires time.Time
	// account served the request that recorded this prefix; "" when unknown.
	account string
}

type Cache struct {
	mu                sync.Mutex
	entries           map[[32]byte]*list.Element
	lru               *list.List
	secret            [32]byte
	ttl               time.Duration
	capacity, minimum int
	policy            Policy
	now               func() time.Time
}

func New(ttl time.Duration, capacity, minimum int) (*Cache, error) {
	return NewWithPolicy(ttl, capacity, minimum, Policy{MaxReadPercent: 100, MinUncachedTokens: 4})
}

// Policy sets the local billing discount after a successful prefix match.
type Policy struct {
	MaxReadPercent    int
	MinUncachedTokens int
	// FixedPercentOnHit grants a billing allowance based on the whole input,
	// independent of how many prefix tokens matched. This is not upstream usage.
	FixedPercentOnHit bool
}

func NewWithPolicy(ttl time.Duration, capacity, minimum int, policy Policy) (*Cache, error) {
	if ttl <= 0 || capacity <= 0 || minimum < 1 {
		return nil, fmt.Errorf("invalid local cache limits")
	}
	if policy.MaxReadPercent < 0 || policy.MaxReadPercent > 100 || policy.MinUncachedTokens < 0 {
		return nil, fmt.Errorf("invalid local cache discount policy")
	}
	c := &Cache{entries: make(map[[32]byte]*list.Element), lru: list.New(), ttl: ttl,
		capacity: capacity, minimum: minimum, now: time.Now, policy: policy}
	_, err := rand.Read(c.secret[:])
	return c, err
}

type Attempt struct {
	cache       *Cache
	points      []point
	InputTokens int
	ReadTokens  int
	committed   bool
	// preferred is the account behind the longest matched prefix; account is
	// the one bound to serve this request.
	preferred, account string
	// started is when Kiro reads and refreshes its prompt cache for this
	// request, so the recorded prefix expires ttl after it, not after the end.
	started time.Time
}

func writeSegment(h hash.Hash, kind string, value any) error {
	data, err := json.Marshal(value) // deterministic map-key ordering
	if err != nil {
		return err
	}
	_, _ = h.Write([]byte(kind))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(data)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(data)
	return nil
}

func cleanBlock(block any) any {
	if original, ok := block.(map[string]any); ok {
		copy := make(map[string]any, len(original))
		for key, value := range original {
			if key != "cache_control" {
				copy[key] = value
			}
		}
		return copy
	}
	return block
}

func contentBlocks(content any) ([]any, error) {
	switch value := content.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": value}}, nil
	case []any:
		result := make([]any, len(value))
		for i, block := range value {
			result[i] = cleanBlock(block)
		}
		return result, nil
	default:
		data, err := json.Marshal(content)
		if err != nil {
			return nil, err
		}
		var blocks []any
		if err := json.Unmarshal(data, &blocks); err != nil {
			return nil, err
		}
		for i, block := range blocks {
			blocks[i] = cleanBlock(block)
		}
		return blocks, nil
	}
}

// Prepare takes a snapshot before generation. Concurrent requests cannot warm
// each other until a successful response has been completed and committed.
func (c *Cache) Prepare(scope string, req *types.CountTokensRequest) (*Attempt, error) {
	e := utils.NewTokenEstimator()
	a := &Attempt{cache: c, InputTokens: e.EstimateTokens(req)}
	h := hmac.New(sha256.New, c.secret[:])
	if err := writeSegment(h, "scope", scope); err != nil {
		return nil, err
	}
	if err := writeSegment(h, "model", req.Model); err != nil {
		return nil, err
	}
	tokens := 0
	checkpoint := func() {
		if tokens < c.minimum {
			return
		}
		var key [32]byte
		copy(key[:], h.Sum(nil))
		p := point{key: key, tokens: tokens}
		if len(a.points) == maxRequestPrefixes {
			copy(a.points, a.points[1:])
			a.points[len(a.points)-1] = p
		} else {
			a.points = append(a.points, p)
		}
	}
	tools := make([]types.AnthropicTool, len(req.Tools))
	copy(tools, req.Tools)
	for i := range tools {
		tools[i].CacheControl = nil
	}
	if err := writeSegment(h, "tools", tools); err != nil {
		return nil, err
	}
	tokens += e.EstimateTokens(&types.CountTokensRequest{Tools: tools}) - 4
	checkpoint()
	for _, system := range req.System {
		system.CacheControl = nil
		if err := writeSegment(h, "system", system); err != nil {
			return nil, err
		}
		tokens += e.EstimateTokens(&types.CountTokensRequest{System: []types.AnthropicSystemMessage{system}}) - 4
		checkpoint()
	}
	if err := writeSegment(h, "system_end", nil); err != nil {
		return nil, err
	}
	for _, message := range req.Messages {
		if err := writeSegment(h, "role", message.Role); err != nil {
			return nil, err
		}
		tokens += 3
		blocks, err := contentBlocks(message.Content)
		if err != nil {
			return nil, err
		}
		for _, block := range blocks {
			if err := writeSegment(h, "content", block); err != nil {
				return nil, err
			}
			tokens += e.EstimateTokens(&types.CountTokensRequest{Messages: []types.AnthropicRequestMessage{
				{Role: message.Role, Content: []any{block}},
			}}) - 7
			checkpoint()
		}
		if err := writeSegment(h, "message_end", nil); err != nil {
			return nil, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	a.started = now
	for _, p := range a.points {
		if element := c.entries[p.key]; element != nil {
			if !element.Value.(entry).expires.After(now) {
				delete(c.entries, p.key)
				c.lru.Remove(element)
			} else {
				a.ReadTokens = p.tokens
				a.preferred = element.Value.(entry).account
				c.lru.MoveToFront(element)
			}
		}
	}
	// Reserve the estimator's four request-overhead tokens and never increase
	// the total input charged by the bridge.
	if limit := a.InputTokens - 4; a.ReadTokens > limit {
		a.ReadTokens = limit
	}
	if a.ReadTokens < 0 {
		a.ReadTokens = 0
	}
	// Divide before multiplication so even a very large estimate cannot overflow.
	percentLimit := (a.InputTokens/100)*c.policy.MaxReadPercent + (a.InputTokens%100)*c.policy.MaxReadPercent/100
	ordinaryLimit := max(a.InputTokens-c.policy.MinUncachedTokens, 0)
	if c.policy.FixedPercentOnHit && a.ReadTokens > 0 {
		a.ReadTokens = percentLimit
	}
	a.ReadTokens = min(a.ReadTokens, percentLimit, ordinaryLimit)
	return a, nil
}

// Preferred returns the account that served the matched prefix, so the request
// can be routed back to it; "" when nothing matched or the account is unknown.
func (a *Attempt) Preferred() string {
	if a == nil {
		return ""
	}
	return a.preferred
}

// Bind records the account that will serve the request. Kiro keeps its prompt
// cache per account, so a prefix earned on a different account is not
// discounted: the request pays Kiro's full price there.
func (a *Attempt) Bind(account string) {
	if a == nil {
		return
	}
	a.account = account
	if account != "" && a.preferred != "" && a.preferred != account {
		a.ReadTokens = 0
	}
}

// Affinity reports "kept" when a matched request stayed on its prefix's
// account, "moved" when it had to switch, and "" when there was no match.
func (a *Attempt) Affinity() string {
	switch {
	case a == nil || a.preferred == "" || a.account == "":
		return ""
	case a.preferred == a.account:
		return "kept"
	default:
		return "moved"
	}
}

func (a *Attempt) Commit() {
	if a == nil || a.committed {
		return
	}
	a.committed = true
	c := a.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	// Measured on production: Kiro's cache goes cold about 5 minutes after the
	// previous turn started, so a long turn leaves less time for the next one.
	expires := a.started.Add(c.ttl)
	for _, p := range a.points {
		// The conversation now lives on the account that just served it.
		e := entry{key: p.key, expires: expires, account: a.account}
		if element := c.entries[p.key]; element != nil {
			// A later-started turn already refreshed this prefix; an earlier
			// turn finishing after it must not shorten or re-home it.
			if element.Value.(entry).expires.After(expires) {
				c.lru.MoveToFront(element)
				continue
			}
			element.Value = e
			c.lru.MoveToFront(element)
		} else {
			c.entries[p.key] = c.lru.PushFront(e)
		}
		for c.lru.Len() > c.capacity {
			last := c.lru.Back()
			delete(c.entries, last.Value.(entry).key)
			c.lru.Remove(last)
		}
	}
}

func (a *Attempt) Usage(upstream *types.KiroTokenUsage, inputEstimate, outputEstimate int) map[string]any {
	if a == nil {
		return upstream.AnthropicUsage(inputEstimate, outputEstimate)
	}
	if upstream.Valid() {
		outputEstimate = *upstream.OutputTokens
	}
	return map[string]any{
		"input_tokens": a.InputTokens - a.ReadTokens, "output_tokens": outputEstimate,
		"cache_read_input_tokens": a.ReadTokens, "cache_creation_input_tokens": 0,
		"cache_usage_source": "local_estimate",
	}
}
