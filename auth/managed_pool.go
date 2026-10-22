package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"kiro2api/logger"
	"kiro2api/types"
	"kiro2api/utils"
)

type AccountProvider func(context.Context, AuthConfig, *types.TokenInfo) (types.TokenInfo, *CreditSnapshot, error)
type accountRuntime struct {
	token       types.TokenInfo
	active      int
	refreshing  bool
	nextRefresh time.Time
	cooldown    time.Time
	lastError   string
	// Consecutive 402/429 answers; each one doubles the cooldown. A successful
	// request resets it. A credential refresh does not end a rate-limit cooldown.
	strikes     int
	limitReason string
}

const maxLimitCooldown = 30 * time.Minute

// An exhausted monthly request count is retried after this long, or at the
// official reset if that comes sooner.
const monthlyLimitCooldown = 6 * time.Hour

// refreshSpacing is the minimum gap between credential refreshes across the pool,
// so a restart or "refresh all" does not hit Kiro's rate limit with every account.
var refreshSpacing = 400 * time.Millisecond

const freePlanReason = "官方订阅已变为免费版（KIRO FREE），已自动停用"

// isFreePlan reports an official snapshot of a free subscription, e.g. "KIRO FREE".
func isFreePlan(u *CreditSnapshot) bool {
	return u != nil && strings.Contains(strings.ToUpper(u.Plan), "FREE")
}

func limitCooldown(strikes int) time.Duration {
	if strikes > 6 {
		return maxLimitCooldown
	}
	return min(time.Minute<<max(strikes-1, 0), maxLimitCooldown)
}

// usableLocked reports whether the account may receive a new request, ignoring concurrency.
func usableLocked(a *managedAccount, rt *accountRuntime, now time.Time) bool {
	return rt.lastError == "" && a.Usage != nil && a.Usage.Remaining > 0 && rt.token.AccessToken != "" && !now.After(rt.token.ExpiresAt) && !now.Before(rt.cooldown)
}

type AccountPool struct {
	mu      sync.Mutex
	doc     managedDocument
	runtime map[string]*accountRuntime
	// One HTTP client per proxy, keyed by proxy ID; rebuilt on commit.
	clients      map[string]proxyClient
	path         string
	provider     AccountProvider
	changed      chan struct{}
	refreshQueue chan string
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	waiting      int
	// Start time of the next allowed refresh; see refreshSpacing.
	refreshMu        sync.Mutex
	nextRefreshStart time.Time
	dirty            bool
	storageError     bool
	closed           bool
	// In-memory ring of concurrency samples; lost on restart by design.
	live     [liveSampleCapacity]LiveSample
	liveNext int
	liveLen  int
	// Client key lookup published by syncKeysLocked; read without p.mu.
	keyIndex        atomic.Pointer[map[[sha256.Size]byte]string]
	builtinHash     [sha256.Size]byte
	builtinHint     string
	builtinSet      bool
	builtinLastUsed *time.Time
	// Per-request usage log; see usage_log.go.
	usageCh      chan UsageEntry
	usageDropped atomic.Int64
	// Requests waiting for their conversation's account, by account ID. Their
	// slots are held back from other requests; see AcquirePreferred.
	stickyWaiters map[string]int
}

var ErrPoolBusy = errors.New("账号并发已满，请稍后重试")
var ErrPoolUnavailable = errors.New("没有可用的上游账号，请检查额度或账号状态")

const maxWaitingRequests = 256

func NewAccountPool(dir string, bootstrap []AuthConfig, defaultLimit int, provider AccountProvider) (*AccountPool, error) {
	doc, err := loadAccountDocument(dir, bootstrap, defaultLimit)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		provider = FetchManagedAccount
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &AccountPool{doc: doc, path: filepath.Join(dir, "accounts.json"), runtime: map[string]*accountRuntime{}, clients: map[string]proxyClient{}, provider: provider, changed: make(chan struct{}), refreshQueue: make(chan string, 1024), usageCh: make(chan UsageEntry, usageBuffer), ctx: ctx, cancel: cancel, stickyWaiters: map[string]int{}}
	for _, a := range doc.Accounts {
		p.runtime[a.ID] = &accountRuntime{}
	}
	p.syncClientsLocked()
	p.syncKeysLocked()
	for i := 0; i < 2; i++ {
		p.wg.Add(1)
		go p.refreshWorker()
	}
	p.wg.Add(1)
	go p.maintain()
	p.wg.Add(1)
	go p.usageWriter()
	p.mu.Lock()
	for _, a := range p.doc.Accounts {
		if a.DeletedAt == nil && !a.Credentials.Disabled {
			p.scheduleLocked(a.ID)
		}
	}
	p.mu.Unlock()
	return p, nil
}

func (p *AccountPool) notifyLocked() { close(p.changed); p.changed = make(chan struct{}) }
func (p *AccountPool) indexLocked(id string) int {
	for i := range p.doc.Accounts {
		if p.doc.Accounts[i].ID == id {
			return i
		}
	}
	return -1
}
func (p *AccountPool) scheduleLocked(id string) bool {
	rt := p.runtime[id]
	if rt == nil || rt.refreshing || p.closed {
		return false
	}
	i := p.indexLocked(id)
	if i < 0 || p.doc.Accounts[i].DeletedAt != nil {
		return false
	}
	select {
	case p.refreshQueue <- id:
		rt.refreshing = true
		p.notifyLocked()
		return true
	default:
		return false
	}
}

func (p *AccountPool) maintain() {
	defer p.wg.Done()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-tick.C:
			p.mu.Lock()
			p.sampleLocked(time.Now())
			for _, a := range p.doc.Accounts {
				rt := p.runtime[a.ID]
				if a.DeletedAt == nil && !a.Credentials.Disabled && time.Now().After(rt.nextRefresh) {
					p.scheduleLocked(a.ID)
				}
			}
			if p.dirty {
				if err := writeAccountDocument(p.path, p.doc); err != nil {
					p.storageError = true
				} else {
					p.storageError = false
					p.dirty = false
				}
			}
			p.mu.Unlock()
		}
	}
}

// waitRefreshSlot blocks until this worker may start the next refresh.
func (p *AccountPool) waitRefreshSlot() bool {
	p.refreshMu.Lock()
	start := p.nextRefreshStart
	if now := time.Now(); start.Before(now) {
		start = now
	}
	p.nextRefreshStart = start.Add(refreshSpacing)
	p.refreshMu.Unlock()
	if d := time.Until(start); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-p.ctx.Done():
			return false
		case <-t.C:
		}
	}
	return true
}

func (p *AccountPool) refreshWorker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case id := <-p.refreshQueue:
			if !p.waitRefreshSlot() {
				return
			}
			p.mu.Lock()
			i := p.indexLocked(id)
			if i < 0 || p.doc.Accounts[i].DeletedAt != nil {
				p.mu.Unlock()
				continue
			}
			cfg := p.doc.Accounts[i].Credentials
			old := p.runtime[id].token
			route := p.doc.Accounts[i].ProxyID
			client, routed := p.clientLocked(&p.doc.Accounts[i])
			p.mu.Unlock()
			var token types.TokenInfo
			var usage *CreditSnapshot
			err := errors.New("账号代理不存在")
			if routed {
				ctx, cancel := context.WithTimeout(utils.WithHTTPClient(p.ctx, client), 35*time.Second)
				token, usage, err = p.provider(ctx, cfg, &old)
				cancel()
			}
			p.mu.Lock()
			i = p.indexLocked(id)
			rt := p.runtime[id]
			if i < 0 || rt == nil {
				p.mu.Unlock()
				continue
			}
			rt.refreshing = false
			// A result obtained over a route the account no longer uses proves nothing.
			if p.doc.Accounts[i].DeletedAt != nil || p.doc.Accounts[i].Credentials.RefreshToken != cfg.RefreshToken || p.doc.Accounts[i].ProxyID != route {
				if p.doc.Accounts[i].DeletedAt == nil {
					p.scheduleLocked(id)
				}
				p.notifyLocked()
				p.mu.Unlock()
				continue
			}
			// Persist a rotated refresh token before accepting the new credential.
			if token.RefreshToken != "" && token.RefreshToken != cfg.RefreshToken {
				next := cloneDocument(p.doc)
				next.Accounts[i].Credentials.RefreshToken = token.RefreshToken
				if err := p.commitLocked(next); err != nil {
					rt.lastError = "刷新凭据保存失败"
					rt.nextRefresh = time.Now().Add(time.Minute)
					p.notifyLocked()
					p.mu.Unlock()
					continue
				}
			}
			now := time.Now()
			rt.nextRefresh = now.Add(5 * time.Minute)
			if err != nil {
				rt.lastError = safeAccountError(err)
				rt.nextRefresh = now.Add(time.Minute)
			} else {

				rt.token = token
				rt.lastError = ""
				if rt.limitReason == "" {
					rt.cooldown = time.Time{}
				}
				p.recordCreditsLocked(p.doc.Accounts[i].Usage, usage, now)
				p.doc.Accounts[i].Usage = usage
				if usage != nil && usage.Email != "" {
					p.doc.Accounts[i].Email = usage.Email
				}
				// A subscription that lapsed to the free plan leaves rotation at once and
				// stays disabled across restarts until someone re-enables it.
				if isFreePlan(usage) && !p.doc.Accounts[i].Credentials.Disabled {
					next := cloneDocument(p.doc)
					next.Accounts[i].Credentials.Disabled = true
					next.Accounts[i].DisabledReason = freePlanReason
					next.Accounts[i].Version++
					if cerr := p.commitLocked(next); cerr != nil {
						rt.lastError = "账号已变为免费版，但自动停用保存失败"
					} else {
						logger.Warn("账号订阅变为免费版，已自动停用", logger.String("account_id", id), logger.String("plan", usage.Plan))
					}
				}
				// An exhausted account only needs checking again around its reset.
				if usage != nil && usage.Remaining <= 0 {
					rt.nextRefresh = now.Add(30 * time.Minute)
					if usage.ResetAt != nil && usage.ResetAt.After(now) && usage.ResetAt.Before(rt.nextRefresh) {
						rt.nextRefresh = usage.ResetAt.Add(time.Minute)
					}
				}
				if !token.ExpiresAt.IsZero() && token.ExpiresAt.Add(-time.Minute).Before(rt.nextRefresh) {
					rt.nextRefresh = token.ExpiresAt.Add(-time.Minute)
				}
				p.dirty = true
			}
			p.notifyLocked()
			p.mu.Unlock()
		}
	}
}

func (p *AccountPool) Refresh(id string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, ErrPoolUnavailable
	}
	n := 0
	found := id == ""
	for _, a := range p.doc.Accounts {
		if a.DeletedAt != nil || (id != "" && a.ID != id) {
			continue
		}
		found = true
		// A manual refresh may inspect a disabled account, but never enables it.
		if id == "" && a.Credentials.Disabled {
			continue
		}
		if a.Usage != nil && time.Since(a.Usage.CheckedAt) < 10*time.Second {
			continue
		}
		if p.scheduleLocked(a.ID) {
			n++
		}
	}
	if !found {
		return n, ErrAccountMissing
	}
	return n, nil
}

func (p *AccountPool) commitLocked(next managedDocument) error {
	if p.closed {
		return ErrPoolUnavailable
	}
	if previous, err := os.ReadFile(p.path); err == nil {
		if err = atomicPrivateFile(filepath.Join(filepath.Dir(p.path), "accounts.previous.json"), previous); err != nil {
			p.storageError = true
			return errors.New("账号备份失败，变更未生效")
		}
	}
	if err := writeAccountDocument(p.path, next); err != nil {
		p.storageError = true
		return errors.New("账号保存失败，变更未生效")
	}
	p.doc = next
	p.dirty = false
	p.storageError = false
	p.syncClientsLocked()
	p.syncKeysLocked()
	p.notifyLocked()
	return nil
}

func (p *AccountPool) Import(items []ImportAccount) ([]string, int, error) {
	if len(items) == 0 || len(items) > 100 {
		return nil, 0, errors.New("每次可导入 1–100 个账号")
	}
	prepared := make([]managedAccount, 0, len(items))
	for i, in := range items {
		a, err := newManagedAccount(in, 2)
		if err != nil {
			return nil, 0, fmt.Errorf("第 %d 个账号：%w", i+1, err)
		}
		prepared = append(prepared, a)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := cloneDocument(p.doc)
	seen := map[string]bool{}
	active := 0
	for _, a := range next.Accounts {
		if a.DeletedAt == nil {
			seen[a.Credentials.RefreshToken] = true
			active++
		}
	}
	ids := []string{}
	duplicates := 0
	for _, a := range prepared {
		if seen[a.Credentials.RefreshToken] {
			duplicates++
			continue
		}
		seen[a.Credentials.RefreshToken] = true
		next.Accounts = append(next.Accounts, a)
		ids = append(ids, a.ID)
	}
	if active+len(ids) > 500 {
		return nil, 0, errors.New("最多管理 500 个账号")
	}
	if len(ids) == 0 {
		return ids, duplicates, nil
	}
	if err := p.commitLocked(next); err != nil {
		return nil, 0, err
	}
	for _, id := range ids {
		p.runtime[id] = &accountRuntime{}
		if !p.doc.Accounts[p.indexLocked(id)].Credentials.Disabled {
			p.scheduleLocked(id)
		}
	}
	return ids, duplicates, nil
}

func (p *AccountPool) Update(id string, patch AccountPatch) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.indexLocked(id)
	if i < 0 || p.doc.Accounts[i].DeletedAt != nil {
		return ErrAccountMissing
	}
	if patch.Version != p.doc.Accounts[i].Version {
		return ErrAccountConflict
	}
	next := cloneDocument(p.doc)
	a := &next.Accounts[i]
	if patch.Name != nil {
		a.Name = *patch.Name
	}
	if patch.Enabled != nil {
		a.Credentials.Disabled = !*patch.Enabled
		a.DisabledReason = ""
	}
	if patch.Concurrency != nil {
		a.Concurrency = *patch.Concurrency
	}
	if patch.RefreshToken != nil {
		a.Credentials.RefreshToken = *patch.RefreshToken
	}
	rerouted := false
	if patch.ProxyID != nil && *patch.ProxyID != a.ProxyID {
		if *patch.ProxyID != "" && proxyIndex(&next, *patch.ProxyID) < 0 {
			return ErrProxyMissing
		}
		a.ProxyID = *patch.ProxyID
		rerouted = true
	}
	in, err := normalizeImport(ImportAccount{AuthConfig: a.Credentials, Name: a.Name, Email: a.Email, Concurrency: a.Concurrency}, 2)
	if err != nil {
		return err
	}
	if a.Concurrency == 0 {
		return errors.New("账号并发上限必须在 1–100 之间")
	}
	a.Name = in.Name
	a.Credentials = in.AuthConfig
	for j, other := range next.Accounts {
		if j != i && other.DeletedAt == nil && other.Credentials.RefreshToken == a.Credentials.RefreshToken {
			return errors.New("该凭据已属于另一个账号")
		}
	}
	a.Version++
	if patch.RefreshToken != nil {
		a.Usage = nil
	}
	if err = p.commitLocked(next); err != nil {
		return err
	}
	rt := p.runtime[id]
	if patch.RefreshToken != nil {
		rt.token = types.TokenInfo{}
		rt.lastError = ""
		rt.cooldown = time.Time{}
		rt.limitReason = ""
		rt.nextRefresh = time.Time{}
	}
	if rerouted {
		// Recheck over the new route; a proxy failure must not linger either way.
		rt.lastError = ""
		rt.nextRefresh = time.Time{}
	}
	if !a.Credentials.Disabled && (patch.RefreshToken != nil || rerouted || rt.token.AccessToken == "" || time.Now().After(rt.nextRefresh)) {
		p.scheduleLocked(id)
	}
	return nil
}

// AccountBatch changes many accounts in one saved commit. Action "update" sets
// any of Concurrency, ProxyID ("" means direct) and Enabled; "enable",
// "disable" and "delete" need no fields. Either every account changes or none.
type AccountBatch struct {
	IDs         []string `json:"ids"`
	Action      string   `json:"action"`
	Concurrency *int     `json:"concurrency,omitempty"`
	ProxyID     *string  `json:"proxy_id,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
}

func (p *AccountPool) Batch(in AccountBatch) (int, error) {
	if len(in.IDs) == 0 || len(in.IDs) > 500 {
		return 0, errors.New("请选择 1–500 个账号")
	}
	switch in.Action {
	case "enable":
		in.Enabled, in.Concurrency, in.ProxyID = ptrTo(true), nil, nil
	case "disable":
		in.Enabled, in.Concurrency, in.ProxyID = ptrTo(false), nil, nil
	case "update":
		if in.Enabled == nil && in.Concurrency == nil && in.ProxyID == nil {
			return 0, errors.New("没有要修改的设置")
		}
		if in.Concurrency != nil && (*in.Concurrency < 1 || *in.Concurrency > 100) {
			return 0, errors.New("账号并发上限必须在 1–100 之间")
		}
	case "delete":
	default:
		return 0, errors.New("不支持的批量操作")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if in.ProxyID != nil && *in.ProxyID != "" && proxyIndex(&p.doc, *in.ProxyID) < 0 {
		return 0, ErrProxyMissing
	}
	next := cloneDocument(p.doc)
	now := time.Now().UTC()
	seen := map[string]bool{}
	rerouted := []string{}
	for _, id := range in.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		i := p.indexLocked(id)
		if i < 0 || next.Accounts[i].DeletedAt != nil {
			return 0, fmt.Errorf("%w: %s", ErrAccountMissing, id)
		}
		a := &next.Accounts[i]
		if in.Action == "delete" {
			a.DeletedAt = &now
			a.Credentials = AuthConfig{}
			a.ProxyID = ""
			a.Version++
			continue
		}
		if in.Enabled != nil {
			a.Credentials.Disabled = !*in.Enabled
			a.DisabledReason = ""
		}
		if in.Concurrency != nil {
			a.Concurrency = *in.Concurrency
		}
		if in.ProxyID != nil && *in.ProxyID != a.ProxyID {
			a.ProxyID = *in.ProxyID
			rerouted = append(rerouted, id)
		}
		a.Version++
	}
	if err := p.commitLocked(next); err != nil {
		return 0, err
	}
	moved := map[string]bool{}
	for _, id := range rerouted {
		moved[id] = true
	}
	for id := range seen {
		rt := p.runtime[id]
		if rt == nil {
			continue
		}
		if in.Action == "delete" {
			rt.token = types.TokenInfo{}
			continue
		}
		if moved[id] {
			// Recheck over the new route; a proxy failure must not linger either way.
			rt.lastError = ""
			rt.nextRefresh = time.Time{}
		}
		a := p.doc.Accounts[p.indexLocked(id)]
		if !a.Credentials.Disabled && (moved[id] || rt.token.AccessToken == "" || time.Now().After(rt.nextRefresh)) {
			p.scheduleLocked(id)
		}
	}
	return len(seen), nil
}

func ptrTo[T any](v T) *T { return &v }

func (p *AccountPool) Delete(id string, version int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.indexLocked(id)
	if i < 0 || p.doc.Accounts[i].DeletedAt != nil {
		return ErrAccountMissing
	}
	if version != p.doc.Accounts[i].Version {
		return ErrAccountConflict
	}
	next := cloneDocument(p.doc)
	now := time.Now().UTC()
	next.Accounts[i].DeletedAt = &now
	next.Accounts[i].Credentials = AuthConfig{}
	next.Accounts[i].ProxyID = ""
	next.Accounts[i].Version++
	if err := p.commitLocked(next); err != nil {
		return err
	}
	p.runtime[id].token = types.TokenInfo{}
	return nil
}

// Restore brings a deleted account back with freshly supplied credentials. Its
// name, position in the pool, concurrency and statistics are kept; it returns
// on a direct connection and is re-checked right away.
func (p *AccountPool) Restore(id string, in AccountRestore) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.indexLocked(id)
	if i < 0 {
		return ErrAccountMissing
	}
	a := p.doc.Accounts[i]
	if a.DeletedAt == nil {
		return errors.New("账号未被删除，无需还原")
	}
	if in.Version != a.Version {
		return ErrAccountConflict
	}
	concurrency := a.Concurrency
	if concurrency < 1 || concurrency > 100 {
		concurrency = 2
	}
	norm, err := normalizeImport(ImportAccount{Name: a.Name, Email: in.Email, Provider: in.Provider, AuthConfig: in.AuthConfig, Concurrency: concurrency}, 2)
	if err != nil {
		return err
	}
	if norm.Email != "" && a.Email != "" && !strings.EqualFold(norm.Email, a.Email) {
		return errors.New("凭据邮箱与原账号不一致，请确认后再还原")
	}
	active := 0
	for _, o := range p.doc.Accounts {
		if o.DeletedAt == nil {
			active++
			if o.Credentials.RefreshToken == norm.RefreshToken {
				return errors.New("该凭据已属于另一个账号")
			}
		}
	}
	if active >= 500 {
		return errors.New("最多管理 500 个账号")
	}
	next := cloneDocument(p.doc)
	r := &next.Accounts[i]
	r.DeletedAt = nil
	r.DisabledReason = ""
	r.Credentials = norm.AuthConfig
	r.Concurrency = concurrency
	r.Usage = nil
	r.Version++
	if err = p.commitLocked(next); err != nil {
		return err
	}
	// Leases taken before the deletion may still be running; keep their count.
	rt := p.runtime[id]
	if rt == nil {
		rt = &accountRuntime{}
		p.runtime[id] = rt
	}
	*rt = accountRuntime{active: rt.active}
	if !p.doc.Accounts[i].Credentials.Disabled {
		p.scheduleLocked(id)
	}
	return nil
}

// allocationOrderLocked returns account indices in allocation order: types in
// PlanOrder first, in that order, then every other account. Ties keep pool order.
func (p *AccountPool) allocationOrderLocked() []int {
	idx := make([]int, len(p.doc.Accounts))
	for i := range idx {
		idx[i] = i
	}
	if len(p.doc.PlanOrder) == 0 {
		return idx
	}
	rankOf := p.planRankLocked()
	sort.SliceStable(idx, func(a, b int) bool { return rankOf(idx[a]) < rankOf(idx[b]) })
	return idx
}

// planRankLocked maps an account index to its PlanOrder position; accounts of
// unlisted types share the last rank.
func (p *AccountPool) planRankLocked() func(int) int {
	rank := make(map[string]int, len(p.doc.PlanOrder))
	for i, t := range p.doc.PlanOrder {
		rank[t] = i
	}
	return func(i int) int {
		if r, ok := rank[planType(p.doc.Accounts[i].Usage)]; ok {
			return r
		}
		return len(p.doc.PlanOrder)
	}
}

// freeSlotsLocked is the concurrency an account can still give to a request
// that does not hold a conversation there: slots reserved for requests waiting
// to return to this account are not offered to others.
func (p *AccountPool) freeSlotsLocked(i int) int {
	a := &p.doc.Accounts[i]
	active := 0
	if rt := p.runtime[a.ID]; rt != nil {
		active = rt.active
	}
	return a.Concurrency - active - p.stickyWaiters[a.ID]
}

// spreadOrderLocked keeps the plan type priority but, within one type, puts
// the account with the most free slots first. New conversations then spread
// out and leave room on accounts whose conversations will come back.
func (p *AccountPool) spreadOrderLocked() []int {
	idx := make([]int, len(p.doc.Accounts))
	free := make([]int, len(p.doc.Accounts))
	for i := range idx {
		idx[i] = i
		free[i] = p.freeSlotsLocked(i)
	}
	rankOf := p.planRankLocked()
	sort.SliceStable(idx, func(a, b int) bool {
		if ra, rb := rankOf(idx[a]), rankOf(idx[b]); ra != rb {
			return ra < rb
		}
		return free[idx[a]] > free[idx[b]]
	})
	return idx
}

// SetPlanOrder sets which account types receive requests first. An empty list
// restores plain pool order.
func (p *AccountPool) SetPlanOrder(order []string) error {
	order, err := normalizePlanOrder(order)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := cloneDocument(p.doc)
	next.PlanOrder = order
	return p.commitLocked(next)
}

type PoolView struct {
	Accounts     []AccountView `json:"accounts"`
	Proxies      []ProxyView   `json:"proxies"`
	PlanOrder    []string      `json:"plan_order"`
	Waiting      int           `json:"waiting"`
	StartedAt    time.Time     `json:"stats_since"`
	StorageError bool          `json:"storage_error"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

var accountTimezone = time.FixedZone("Asia/Shanghai", 8*3600)

func (p *AccountPool) View() PoolView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	today := now.In(accountTimezone).Format("2006-01-02")
	v := PoolView{Accounts: []AccountView{}, Proxies: p.proxyViewsLocked(), PlanOrder: append([]string{}, p.doc.PlanOrder...), Waiting: p.waiting, StartedAt: p.doc.StartedAt, StorageError: p.storageError, UpdatedAt: now.UTC()}
	for _, a := range p.doc.Accounts {
		rt := p.runtime[a.ID]
		status := "ready"
		switch {
		case a.DeletedAt != nil:
			status = "deleted"
		case a.Credentials.Disabled:
			status = "disabled"
		case rt.lastError != "":
			status = "error"
		case a.Usage != nil && a.Usage.Remaining <= 0:
			status = "exhausted"
		case now.Before(rt.cooldown):
			status = "cooldown"
		case rt.token.AccessToken == "" || now.After(rt.token.ExpiresAt):
			status = "checking"
		case rt.active >= a.Concurrency:
			status = "busy"
		}
		errText := rt.lastError
		if errText == "" && rt.limitReason != "" && now.Before(rt.cooldown) {
			errText = rt.limitReason
		}
		view := AccountView{ID: a.ID, Name: a.Name, Email: a.Email, Auth: a.Credentials.AuthType, PlanType: planType(a.Usage), Enabled: !a.Credentials.Disabled && a.DeletedAt == nil, Concurrency: a.Concurrency, ProxyID: a.ProxyID, DisabledReason: a.DisabledReason, Active: rt.active, Version: a.Version, Status: status, Error: errText, Refreshing: rt.refreshing, CreatedAt: a.CreatedAt, DeletedAt: a.DeletedAt, Totals: a.Totals, Today: a.Daily[today], LastUsed: a.LastUsed, LastModel: a.LastModel, LastStatus: a.LastStatus}
		if a.Usage != nil {
			usage := *a.Usage
			view.Usage = &usage
		}
		if !rt.token.ExpiresAt.IsZero() {
			expiry := rt.token.ExpiresAt
			view.TokenExpiresAt = &expiry
		}
		v.Accounts = append(v.Accounts, view)
	}
	return v
}

type AccountLease struct {
	pool  *AccountPool
	id    string
	keyID string // client API key that made the request; "" when unknown
	Token *types.TokenWithUsage
	// Client carries the account's proxy route; nil means the shared direct client.
	Client *http.Client
	once   sync.Once
}
type RequestOutcome struct {
	Success         bool
	Canceled        bool
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	Model           string
	Status          int
	UpstreamStatus  int
	// MonthlyLimit marks a 402 with reason MONTHLY_REQUEST_COUNT.
	MonthlyLimit bool
	// ProxyError marks a request that failed while connecting through the account's proxy.
	ProxyError bool
	// Affinity is "kept" or "moved" when the request continued a known
	// conversation, telling whether it stayed on that conversation's account.
	Affinity string
	// Empty marks a completed stream that produced no text or tool output.
	Empty     bool
	Duration  time.Duration
	FirstByte time.Duration
	Stream    bool
	// Credits is the sum of Kiro's meteringEvent values; Metered says any arrived.
	Credits float64
	Metered bool
}

// stickyWait bounds how long a request waits for the busy account that holds
// its conversation before it moves to another account. Kiro's prompt cache is
// per account, so moving roughly doubles the credits of the next turn. It is
// set once at startup (KIRO_STICKY_WAIT_SECONDS); 0 disables waiting.
var stickyWait = 10 * time.Second

// SetStickyWait changes stickyWait; call it before the pool serves requests.
func SetStickyWait(d time.Duration) {
	if d >= 0 {
		stickyWait = d
	}
}

func (p *AccountPool) Acquire(ctx context.Context) (*AccountLease, error) {
	return p.AcquirePreferred(ctx, "")
}

// AcquirePreferred tries the preferred account first. While it is healthy but
// at its concurrency limit the request waits up to stickyWait for it, and the
// next slot freed there is held for it rather than given to other requests.
// When the account is unusable for any other reason, or the wait runs out, the
// request takes the least loaded account like a new conversation.
func (p *AccountPool) AcquirePreferred(ctx context.Context, preferred string) (*AccountLease, error) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	var stickyC <-chan time.Time
	if preferred != "" && stickyWait > 0 {
		sticky := time.NewTimer(stickyWait)
		defer sticky.Stop()
		stickyC = sticky.C
	}
	// holding: this request is counted in stickyWaiters[preferred].
	registered, holding := false, false
	unhold := func() { // p.mu held
		if !holding {
			return
		}
		holding = false
		if p.stickyWaiters[preferred]--; p.stickyWaiters[preferred] <= 0 {
			delete(p.stickyWaiters, preferred)
		}
		// The held slot is open to everyone again.
		p.notifyLocked()
	}
	keyID := apiKeyIDFrom(ctx)
	defer func() {
		if registered || holding {
			p.mu.Lock()
			if registered {
				p.waiting--
			}
			unhold()
			p.mu.Unlock()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrPoolUnavailable
		}
		if stickyC == nil {
			// The wait ran out: stop reserving a slot before placing elsewhere.
			unhold()
		}
		now := time.Now()
		pending, held := false, false
		order := p.spreadOrderLocked()
		if preferred != "" {
			if at := p.indexLocked(preferred); at >= 0 {
				rest := make([]int, 0, len(order))
				for _, i := range order {
					if i != at {
						rest = append(rest, i)
					}
				}
				order = append([]int{at}, rest...)
			}
		}
		for _, i := range order {
			a := &p.doc.Accounts[i]
			if a.DeletedAt != nil || a.Credentials.Disabled {
				continue
			}
			rt := p.runtime[a.ID]
			if now.After(rt.nextRefresh) {
				p.scheduleLocked(a.ID)
			}
			if rt.refreshing {
				pending = true
			}
			if rt.lastError != "" || a.Usage == nil || a.Usage.Remaining <= 0 || rt.token.AccessToken == "" || now.After(rt.token.ExpiresAt) || now.Before(rt.cooldown) {
				continue
			}
			// The conversation's own account may use a held slot; any other
			// placement must leave held slots alone.
			own := a.ID == preferred && stickyC != nil
			if (own && rt.active >= a.Concurrency) || (!own && p.freeSlotsLocked(i) <= 0) {
				pending = true
				if own {
					// Wait for the conversation's account instead of moving.
					if !holding {
						holding = true
						p.stickyWaiters[preferred]++
					}
					held = true
					break
				}
				continue
			}
			client, routed := p.clientLocked(a)
			if !routed {
				continue
			}
			unhold()
			rt.active++
			lease := &AccountLease{pool: p, id: a.ID, keyID: keyID, Token: tokenWithSnapshot(rt.token, a.Usage), Client: client}
			p.mu.Unlock()
			return lease, nil
		}
		if !held {
			// The account became unusable or the wait ran out.
			unhold()
		}
		if !pending {
			p.recordRejectionLocked("pool_unavailable", keyID, now)
			p.mu.Unlock()
			return nil, ErrPoolUnavailable
		}
		if !registered {
			if p.waiting >= maxWaitingRequests {
				p.recordRejectionLocked("pool_busy", keyID, now)
				p.mu.Unlock()
				return nil, ErrPoolBusy
			}
			p.waiting++
			registered = true
		}
		wake := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.ctx.Done():
			return nil, ErrPoolUnavailable
		case <-timer.C:
			p.recordRejection("pool_busy", keyID)
			return nil, ErrPoolBusy
		case <-stickyC:
			stickyC = nil
		case <-wake:
		}
	}
}

// AccountID identifies the account serving the lease.
func (l *AccountLease) AccountID() string {
	if l == nil {
		return ""
	}
	return l.id
}

func (s *RequestStats) add(o RequestOutcome) {
	s.Requests++
	if o.Canceled {
		s.Canceled++
	} else if o.Success {
		s.Succeeded++
	} else {
		s.Failed++
	}
	s.InputTokens += int64(max(o.InputTokens, 0))
	s.OutputTokens += int64(max(o.OutputTokens, 0))
	s.CacheReadTokens += int64(max(o.CacheReadTokens, 0))
}
func (l *AccountLease) Release(o RequestOutcome) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		p := l.pool
		p.mu.Lock()
		defer p.mu.Unlock()
		rt := p.runtime[l.id]
		rt.active--
		i := p.indexLocked(l.id)
		if i < 0 {
			p.notifyLocked()
			return
		}
		a := &p.doc.Accounts[i]
		now := time.Now()
		p.recordOutcomeLocked(o, l.keyID, now)
		p.logUsageLocked(l.keyID, a, o, now)
		a.LastUsed = &now
		a.LastModel = o.Model
		a.LastStatus = o.Status
		a.Totals.add(o)
		day := now.In(accountTimezone).Format("2006-01-02")
		d := a.Daily[day]
		d.add(o)
		a.Daily[day] = d
		cutoff := now.AddDate(0, 0, -31).In(accountTimezone).Format("2006-01-02")
		for k := range a.Daily {
			if k < cutoff {
				delete(a.Daily, k)
			}
		}
		// A broken proxy takes the account out of rotation until a refresh over it succeeds.
		if !o.Canceled && o.ProxyError {
			rt.lastError = "代理连接失败，已暂停分配，恢复后自动启用"
			rt.cooldown = now.Add(30 * time.Second)
			rt.nextRefresh = rt.cooldown
		}
		if !o.Canceled && o.UpstreamStatus >= 400 {
			switch o.UpstreamStatus {
			case 401, 403:
				rt.token = types.TokenInfo{}
				rt.lastError = "上游认证失败，请刷新或更换凭据"
			case 402:
				rt.lastError = "上游额度不足（HTTP 402）"
			case 429:
				rt.lastError = "上游限流（HTTP 429）"
			}
			if o.UpstreamStatus == 401 || o.UpstreamStatus == 403 || o.UpstreamStatus == 402 || o.UpstreamStatus == 429 {
				rt.cooldown = now.Add(time.Minute)
				rt.nextRefresh = rt.cooldown
			}
			// The usage API can still show credits while the request count is used up,
			// so a refresh must not end this pause; only a successful request does.
			if o.MonthlyLimit {
				until := now.Add(monthlyLimitCooldown)
				if u := a.Usage; u != nil && u.ResetAt != nil && u.ResetAt.After(now) && u.ResetAt.Before(until) {
					until = u.ResetAt.Add(time.Minute)
				}
				rt.cooldown = until
				rt.lastError = ""
				rt.limitReason = "上游月度请求次数已用完（MONTHLY_REQUEST_COUNT），" + until.In(accountTimezone).Format("01-02 15:04") + " 前暂停分配"
				rt.nextRefresh = now.Add(5 * time.Minute)
				logger.Warn("账号月度请求次数已用完，暂停分配", logger.String("account_id", l.id), logger.String("until", until.UTC().Format(time.RFC3339)))
			}
		}
		if o.Success && !o.Canceled {
			rt.limitReason = ""
		}
		p.dirty = true
		p.notifyLocked()
	})
}

func (p *AccountPool) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	p.notifyLocked()
	p.mu.Unlock()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return writeAccountDocument(p.path, p.doc)
}
