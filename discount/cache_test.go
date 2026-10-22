package discount

import (
	"strings"
	"sync"
	"testing"
	"time"

	"kiro2api/types"
	"kiro2api/utils"
)

func testCache(t *testing.T, capacity int) *Cache {
	t.Helper()
	c, err := New(5*time.Minute, capacity, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func longRequest() *types.CountTokensRequest {
	return &types.CountTokensRequest{Model: "claude-opus-4.8", Messages: []types.AnthropicRequestMessage{
		{Role: "user", Content: strings.Repeat("The blue notebook is on the wooden shelf. ", 250)},
	}}
}

func prepare(t *testing.T, c *Cache, scope string, req *types.CountTokensRequest) *Attempt {
	t.Helper()
	a, err := c.Prepare(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDiscountRequiresPriorSuccessAndPreservesInputTotal(t *testing.T) {
	c, req := testCache(t, 100), longRequest()
	first := prepare(t, c, "user-a", req)
	concurrent := prepare(t, c, "user-a", req)
	if first.ReadTokens != 0 || concurrent.ReadTokens != 0 {
		t.Fatal("in-flight requests received an unearned discount")
	}
	first.Commit()
	second := prepare(t, c, "user-a", req)
	if second.ReadTokens <= 1024 || second.ReadTokens >= second.InputTokens {
		t.Fatalf("unexpected discount: %+v", second)
	}
	for _, attempt := range []*Attempt{first, second} {
		usage := attempt.Usage(nil, attempt.InputTokens, 4)
		if usage["input_tokens"].(int)+usage["cache_read_input_tokens"].(int) != utils.NewTokenEstimator().EstimateTokens(req) {
			t.Fatal("total input changed")
		}
		if usage["cache_creation_input_tokens"] != 0 || usage["cache_usage_source"] != "local_estimate" {
			t.Fatal("local discount incorrectly represented")
		}
	}
	if prepare(t, c, "user-b", req).ReadTokens != 0 {
		t.Fatal("discount leaked across scopes")
	}
	req.Model = "claude-sonnet-4.6"
	if prepare(t, c, "user-a", req).ReadTokens != 0 {
		t.Fatal("discount leaked across models")
	}
}

func TestOnlyTheUnchangedPrefixReceivesADiscount(t *testing.T) {
	c := testCache(t, 100)
	req := longRequest()
	prefix := req.Messages[0].Content.(string)
	req.System = []types.AnthropicSystemMessage{{Type: "text", Text: prefix}}
	req.Messages = []types.AnthropicRequestMessage{{Role: "user", Content: "first question"}}
	prepare(t, c, "user-a", req).Commit()
	req.Messages[0].Content = "a completely different question"
	a := prepare(t, c, "user-a", req)
	expected := utils.NewTokenEstimator().EstimateTokens(&types.CountTokensRequest{System: req.System}) - 4
	if a.ReadTokens != expected {
		t.Fatalf("changed suffix was discounted: got %d, want %d", a.ReadTokens, expected)
	}
	req.System[0].Text = "CHANGED " + prefix
	if prepare(t, c, "user-a", req).ReadTokens != 0 {
		t.Fatal("matching suffix counted as matching prefix")
	}
}

func TestGrowingConversationMatchesPreviousMessagePrefix(t *testing.T) {
	c, req := testCache(t, 100), longRequest()
	first := prepare(t, c, "user-a", req)
	first.Commit()
	req.Messages = append(req.Messages, types.AnthropicRequestMessage{Role: "assistant", Content: "OK"}, types.AnthropicRequestMessage{Role: "user", Content: "continue"})
	a := prepare(t, c, "user-a", req)
	if a.ReadTokens != first.InputTokens-4 {
		t.Fatalf("history prefix lost: got %d", a.ReadTokens)
	}
	req.Messages[0].Role = "assistant"
	if prepare(t, c, "user-a", req).ReadTokens != 0 {
		t.Fatal("message roles were ignored")
	}
}

func TestTTLShortPromptsAndBoundedMemory(t *testing.T) {
	c, req := testCache(t, 2), longRequest()
	now := time.Now()
	c.now = func() time.Time { return now }
	prepare(t, c, "a", req).Commit()
	now = now.Add(5 * time.Minute)
	if prepare(t, c, "a", req).ReadTokens != 0 {
		t.Fatal("expired prefix discounted")
	}
	for _, scope := range []string{"a", "b", "c"} {
		prepare(t, c, scope, req).Commit()
	}
	if len(c.entries) != 2 || prepare(t, c, "a", req).ReadTokens != 0 {
		t.Fatal("LRU memory bound failed")
	}
	req.Messages[0].Content = "hello"
	prepare(t, c, "short", req).Commit()
	if prepare(t, c, "short", req).ReadTokens != 0 {
		t.Fatal("tiny prompt received a discount")
	}
}

func TestCanonicalizationAndCacheHintsDoNotCreateFalseHits(t *testing.T) {
	c, req := testCache(t, 100), longRequest()
	text := req.Messages[0].Content.(string)
	prepare(t, c, "a", req).Commit()
	req.Messages[0].Content = []types.ContentBlock{{Type: "text", Text: &text, CacheControl: &types.CacheControl{Type: "ephemeral"}}}
	if prepare(t, c, "a", req).ReadTokens == 0 {
		t.Fatal("equivalent text encodings did not match")
	}
	req.Tools = []types.AnthropicTool{{Name: "lookup", Description: "new tool", InputSchema: map[string]any{"type": "object"}}}
	if prepare(t, c, "a", req).ReadTokens != 0 {
		t.Fatal("changing tools did not invalidate the prefix")
	}
}

func TestConcurrentDiscountLookups(t *testing.T) {
	c, req := testCache(t, 100), longRequest()
	var group sync.WaitGroup
	for i := 0; i < 24; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			a, err := c.Prepare("a", req)
			if err == nil {
				a.Commit()
			}
		}()
	}
	group.Wait()
	if a := prepare(t, c, "a", req); a.ReadTokens == 0 || a.ReadTokens >= a.InputTokens {
		t.Fatal("concurrent cache state corrupted")
	}
}

func TestDiscountFollowsServingAccount(t *testing.T) {
	c, req := testCache(t, 100), longRequest()
	first := prepare(t, c, "user-a", req)
	if first.Preferred() != "" || first.Affinity() != "" {
		t.Fatal("first request claimed an account")
	}
	first.Bind("acct-a")
	first.Commit()

	kept := prepare(t, c, "user-a", req)
	if kept.Preferred() != "acct-a" {
		t.Fatalf("preferred %q", kept.Preferred())
	}
	kept.Bind("acct-a")
	if kept.ReadTokens == 0 || kept.Affinity() != "kept" {
		t.Fatalf("same account lost discount: %+v %q", kept.ReadTokens, kept.Affinity())
	}

	moved := prepare(t, c, "user-a", req)
	moved.Bind("acct-b")
	if moved.ReadTokens != 0 || moved.Affinity() != "moved" {
		t.Fatalf("switched account kept discount: %d %q", moved.ReadTokens, moved.Affinity())
	}
	usage := moved.Usage(nil, moved.InputTokens, 1)
	if usage["input_tokens"].(int) != moved.InputTokens || usage["cache_read_input_tokens"].(int) != 0 {
		t.Fatalf("moved usage %v", usage)
	}
	// The conversation now lives on the account that served it.
	moved.Commit()
	if next := prepare(t, c, "user-a", req); next.Preferred() != "acct-b" {
		t.Fatalf("prefix not rebound, preferred %q", next.Preferred())
	}

	// Without a managed pool nothing is bound and the discount is unchanged.
	legacy := prepare(t, c, "user-a", req)
	legacy.Bind("")
	if legacy.ReadTokens == 0 {
		t.Fatal("unbound request lost discount")
	}
	var none *Attempt
	none.Bind("x")
	if none.Preferred() != "" || none.Affinity() != "" {
		t.Fatal("nil attempt")
	}
}

// Kiro's cache goes cold ttl after the previous turn started, so a long turn
// must not extend the local discount past that.
func TestTTLCountsFromRequestStart(t *testing.T) {
	c, req := testCache(t, 100), longRequest()
	now := time.Now()
	c.now = func() time.Time { return now }
	start := now
	long := prepare(t, c, "a", req)
	now = now.Add(time.Minute) // the turn ran for a minute
	long.Bind("acct-a")
	long.Commit()
	now = start.Add(5*time.Minute - time.Second)
	if prepare(t, c, "a", req).ReadTokens == 0 {
		t.Fatal("prefix expired early")
	}
	now = start.Add(5*time.Minute + time.Second)
	if prepare(t, c, "a", req).ReadTokens != 0 {
		t.Fatal("prefix outlived Kiro's cache window")
	}

	// An earlier-started turn finishing last must not shorten or re-home the prefix.
	now = start.Add(time.Hour)
	seed := prepare(t, c, "b", req)
	seed.Bind("acct-a")
	seed.Commit()
	older := prepare(t, c, "b", req)
	now = now.Add(30 * time.Second)
	newer := prepare(t, c, "b", req)
	newer.Bind("acct-b")
	newer.Commit()
	older.Bind("acct-a")
	older.Commit()
	if p := prepare(t, c, "b", req).Preferred(); p != "acct-b" {
		t.Fatalf("stale commit re-homed prefix to %q", p)
	}
}
