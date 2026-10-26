package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/internal/testutil"
	"kiro2api/types"
	"kiro2api/utils"
)

// retryUpstream is a fake Kiro that answers by attempt number and records the
// access token of every call, which identifies the account that served it.
type retryUpstream struct {
	mu    sync.Mutex
	calls []string
	reply func(call int, token string) (int, []byte)
}

func (u *retryUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	u.mu.Lock()
	u.calls = append(u.calls, token)
	call := len(u.calls)
	u.mu.Unlock()
	status, body := u.reply(call, token)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
}

// restart forgets earlier calls so each request can be judged on its own.
func (u *retryUpstream) restart() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = nil
}

func (u *retryUpstream) tokens() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string{}, u.calls...)
}

var retryOKFrame = testutil.EventFrame("assistantResponseEvent", `{"content":"OK"}`)
var retryErrorBody = []byte(`{"message":"synthetic upstream failure"}`)

// installRetryUpstream points the bridge at a fake Kiro for the test's duration.
func installRetryUpstream(t *testing.T, reply func(call int, token string) (int, []byte)) *retryUpstream {
	t.Helper()
	old := utils.SharedHTTPClient
	t.Cleanup(func() { utils.SharedHTTPClient = old })
	u := &retryUpstream{reply: reply}
	utils.SharedHTTPClient = &http.Client{Transport: u}
	return u
}

// retryTestPool builds a ready managed pool of accounts that each carry their
// own access token, so the fake upstream can tell them apart.
func retryTestPool(t *testing.T, accounts int) (*auth.AccountPool, *auth.AuthService) {
	t.Helper()
	var boot []auth.AuthConfig
	for i := 0; i < accounts; i++ {
		boot = append(boot, auth.AuthConfig{AuthType: "Social", RefreshToken: fmt.Sprintf("synthetic-retry-%d-refresh-token", i)})
	}
	p, err := auth.NewAccountPool(t.TempDir(), boot, 1, func(_ context.Context, cfg auth.AuthConfig, _ *types.TokenInfo) (types.TokenInfo, *auth.CreditSnapshot, error) {
		return types.TokenInfo{AccessToken: "access-" + cfg.RefreshToken, ExpiresAt: time.Now().Add(time.Hour)},
			&auth.CreditSnapshot{Remaining: 10, Limit: 10, CheckedAt: time.Now()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		ready := 0
		for _, a := range p.View().Accounts {
			if a.Status == "ready" {
				ready++
			}
		}
		if ready == accounts {
			return p, auth.NewManagedAuthService(p)
		}
		if time.Now().After(deadline) {
			t.Fatal("accounts never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type routeResult struct {
	status int
	body   string
	header http.Header
	// first is the account the request started on, last the one that answered.
	first, last string
}

// runRoute mirrors what the /v1 routes do around a handler: prepare the
// conversation prefix, take an account, run the handler, release.
func runRoute(t *testing.T, as *auth.AuthService, req types.AnthropicRequest, protocol string) routeResult {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	path := "/v1/messages"
	if protocol == "openai" {
		path = "/v1/chat/completions"
	}
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Request.Header.Set("X-API-Key", "local-test-key")
	c.Set("request_stream", req.Stream)
	prepareCacheDiscount(c, req)
	token, release, ok := acquireRequestAccount(c, as, req.Model)
	if !ok {
		return routeResult{status: recorder.Code, body: recorder.Body.String(), header: recorder.Header()}
	}
	first := accountMeasurement(c).lease.AccountID()
	switch {
	case protocol == "openai" && req.Stream:
		handleOpenAIStreamRequest(c, req, token.TokenInfo)
	case protocol == "openai":
		handleOpenAINonStreamRequest(c, req, token.TokenInfo)
	case req.Stream:
		handleStreamRequest(c, req, token)
	default:
		handleNonStreamRequest(c, req, token.TokenInfo)
	}
	last := accountMeasurement(c).lease.AccountID()
	release()
	return routeResult{status: recorder.Code, body: recorder.Body.String(), header: recorder.Header(), first: first, last: last}
}

func retryRequest(text string, stream bool) types.AnthropicRequest {
	return types.AnthropicRequest{Model: "claude-opus-4-8", MaxTokens: 16, Stream: stream,
		Messages: []types.AnthropicRequestMessage{{Role: "user", Content: strings.Repeat(text+" ", 250)}}}
}

// A request whose account failed before anything reached the client is sent
// once more on another account; the client only ever sees the second answer.
func TestUpstreamAccountFailureRetriesOnAnotherAccount(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	localDiscountCache = configuredDiscountCache()
	// 5xx does not cool an account down, so one pool serves every shape.
	upstream := installRetryUpstream(t, func(call int, _ string) (int, []byte) {
		if call == 1 {
			return http.StatusInternalServerError, retryErrorBody
		}
		return http.StatusOK, retryOKFrame
	})
	_, as := retryTestPool(t, 2)
	for _, protocol := range []string{"anthropic", "openai"} {
		for _, stream := range []bool{false, true} {
			name := fmt.Sprintf("%s stream=%v", protocol, stream)
			upstream.restart()
			got := runRoute(t, as, retryRequest("Upstream retry "+name, stream), protocol)
			if got.status != http.StatusOK {
				t.Fatalf("%s: client saw %d %s", name, got.status, got.body)
			}
			tokens := upstream.tokens()
			if len(tokens) != 2 {
				t.Fatalf("%s: upstream calls %v", name, tokens)
			}
			if tokens[0] == tokens[1] {
				t.Fatalf("%s: retried on the same account %s", name, tokens[0])
			}
			if got.first == got.last {
				t.Fatalf("%s: request stayed on the failed account %s", name, got.first)
			}
			// The client gets the retry's content, not an empty success.
			if !strings.Contains(got.body, "OK") {
				t.Fatalf("%s: client got no content: %s", name, got.body)
			}
		}
	}
}

// The account that failed carries the failure and its cooldown; the account
// that retried carries the success.
func TestRetryAccountingSplitsBetweenBothAccounts(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	localDiscountCache = configuredDiscountCache()
	installRetryUpstream(t, func(call int, _ string) (int, []byte) {
		if call == 1 {
			return http.StatusTooManyRequests, retryErrorBody
		}
		return http.StatusOK, retryOKFrame
	})
	pool, as := retryTestPool(t, 2)
	got := runRoute(t, as, retryRequest("Accounting after retry.", false), "anthropic")
	if got.status != http.StatusOK {
		t.Fatalf("client saw %d %s", got.status, got.body)
	}
	for _, a := range pool.View().Accounts {
		switch a.ID {
		case got.first:
			if a.Totals.Failed != 1 || a.Totals.Succeeded != 0 {
				t.Fatalf("failed account totals %+v", a.Totals)
			}
			if a.Status != "cooldown" && a.Status != "error" {
				t.Fatalf("rate limited account stayed in rotation: %s", a.Status)
			}
		case got.last:
			if a.Totals.Succeeded != 1 || a.Totals.Failed != 0 {
				t.Fatalf("retry account totals %+v", a.Totals)
			}
			if a.Active != 0 {
				t.Fatalf("retry lease leaked: active %d", a.Active)
			}
		}
	}
	if got.first == got.last {
		t.Fatal("request stayed on the rate limited account")
	}
	for _, a := range pool.View().Accounts {
		if a.Active != 0 {
			t.Fatalf("lease leaked on %s: active %d", a.ID, a.Active)
		}
	}
}

// What Kiro rejects about the request itself cannot be fixed by another
// account, so the client gets that answer straight away.
func TestRequestRejectedByKiroIsNotRetried(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	localDiscountCache = configuredDiscountCache()
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		upstream := installRetryUpstream(t, func(int, string) (int, []byte) { return status, retryErrorBody })
		_, as := retryTestPool(t, 2)
		for _, protocol := range []string{"anthropic", "openai"} {
			for _, stream := range []bool{false, true} {
				upstream.restart()
				got := runRoute(t, as, retryRequest(fmt.Sprintf("Rejected %d %s %v", status, protocol, stream), stream), protocol)
				if got.status != status {
					t.Fatalf("%d %s stream=%v: client saw %d %s", status, protocol, stream, got.status, got.body)
				}
				if calls := upstream.tokens(); len(calls) != 1 {
					t.Fatalf("%d: a rejected request was sent again: %v", status, calls)
				}
				if got.first != got.last {
					t.Fatal("a rejected request changed accounts")
				}
				// A stream decides before committing to SSE, so the failure is
				// still an ordinary JSON error with its real status.
				if !strings.HasPrefix(strings.TrimSpace(got.body), "{") || strings.Contains(got.body, "data:") {
					t.Fatalf("%d stream=%v: failure was written as a stream: %s", status, stream, got.body)
				}
			}
		}
	}
}

func TestRetryHappensAtMostOnce(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	localDiscountCache = configuredDiscountCache()
	upstream := installRetryUpstream(t, func(int, string) (int, []byte) {
		return http.StatusInternalServerError, retryErrorBody
	})
	_, as := retryTestPool(t, 3)
	got := runRoute(t, as, retryRequest("Everything is down.", false), "anthropic")
	// The second failure is the one the client is told about.
	if got.status != http.StatusBadGateway || !strings.Contains(got.body, "upstream_error") {
		t.Fatalf("client saw %d %s", got.status, got.body)
	}
	if calls := upstream.tokens(); len(calls) != 2 {
		t.Fatalf("want exactly one retry, got %d upstream calls: %v", len(calls), calls)
	}
}

// With nowhere to move the request, the client gets the real upstream error
// rather than a pool error, and without queueing for a free account.
func TestNoOtherAccountKeepsTheOriginalFailure(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	localDiscountCache = configuredDiscountCache()
	upstream := installRetryUpstream(t, func(int, string) (int, []byte) {
		return http.StatusTooManyRequests, retryErrorBody
	})
	_, as := retryTestPool(t, 1)
	started := time.Now()
	got := runRoute(t, as, retryRequest("Only one account.", false), "anthropic")
	if got.status != http.StatusTooManyRequests || !strings.Contains(got.body, "upstream_rate_limited") {
		t.Fatalf("client saw %d %s", got.status, got.body)
	}
	if calls := upstream.tokens(); len(calls) != 1 {
		t.Fatalf("upstream calls %v", calls)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("request queued for an account that cannot come: %v", elapsed)
	}
}

// After a retry the conversation belongs to the account that answered it.
func TestConversationMovesToTheRetryAccount(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	localDiscountCache = configuredDiscountCache()
	failures := 0
	upstream := installRetryUpstream(t, func(_ int, _ string) (int, []byte) {
		if failures == 0 {
			failures++
			return http.StatusInternalServerError, retryErrorBody
		}
		return http.StatusOK, retryOKFrame
	})
	_, as := retryTestPool(t, 2)
	req := retryRequest("The conversation follows its account.", false)
	moved := runRoute(t, as, req, "anthropic")
	if moved.status != http.StatusOK || moved.first == moved.last {
		t.Fatalf("first turn: %d %s on %s", moved.status, moved.body, moved.last)
	}
	upstream.restart()
	next := runRoute(t, as, req, "anthropic")
	if next.status != http.StatusOK {
		t.Fatalf("second turn: %d %s", next.status, next.body)
	}
	if next.first != moved.last || next.last != moved.last {
		t.Fatalf("second turn went to %s/%s, want the retry account %s", next.first, next.last, moved.last)
	}
	if calls := upstream.tokens(); len(calls) != 1 {
		t.Fatalf("second turn needed %d upstream calls: %v", len(calls), calls)
	}
}

// Affinity does not depend on reporting a discount: with the switch off a
// conversation still returns to its account and usage looks untouched.
func TestStickyConversationWithoutDiscountReporting(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	t.Setenv("KIRO_LOCAL_CACHE_DISCOUNT", "false")
	localDiscountCache = configuredDiscountCache()
	installRetryUpstream(t, func(int, string) (int, []byte) { return http.StatusOK, retryOKFrame })
	_, as := retryTestPool(t, 3)
	req := retryRequest("A conversation with no discount reported.", false)
	home := ""
	for turn := 0; turn < 3; turn++ {
		got := runRoute(t, as, req, "anthropic")
		if got.status != http.StatusOK {
			t.Fatalf("turn %d: %d %s", turn, got.status, got.body)
		}
		if turn == 0 {
			home = got.last
		} else if got.last != home {
			t.Fatalf("turn %d left its account: %s, want %s", turn, got.last, home)
		}
		usage := decodeUsage(t, []byte(got.body))
		for _, field := range []string{"cache_read_input_tokens", "cache_creation_input_tokens", "cache_usage_source"} {
			if value, present := usage[field]; present {
				t.Fatalf("turn %d reported %s=%v with the discount switched off", turn, field, value)
			}
		}
		if h := got.header.Get("X-Kiro-Cache-Accounting"); h != "" {
			t.Fatalf("turn %d announced local accounting: %s", turn, h)
		}
	}
}

func TestAccountFailureStatusClassification(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429, 500, 502, 503, 529} {
		if !accountFailureStatus(status) {
			t.Fatalf("%d should move the request to another account", status)
		}
	}
	for _, status := range []int{400, 404, 413, 422} {
		if accountFailureStatus(status) {
			t.Fatalf("%d is the request's own problem and must not be retried", status)
		}
	}
}

// A retry must go out through the account it moved to. A direct lease carries
// no client, so the previous account's proxy must not be left in the request
// context: the retry would both leave through the wrong exit and blame the
// healthy account for the proxy failure.
func TestRetryOnDirectAccountLeavesTheFailedAccountProxyBehind(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	localDiscountCache = configuredDiscountCache()
	upstream := installRetryUpstream(t, func(int, string) (int, []byte) { return http.StatusOK, retryOKFrame })
	pool, as := retryTestPool(t, 2)
	accessOf := map[string]string{}
	for i, a := range pool.View().Accounts {
		accessOf[a.ID] = fmt.Sprintf("access-synthetic-retry-%d-refresh-token", i)
	}
	req := retryRequest("The retry must use its own exit.", false)

	// Settle the conversation on one account while both are still direct.
	warm := runRoute(t, as, req, "anthropic")
	if warm.status != http.StatusOK {
		t.Fatalf("warm turn: %d %s", warm.status, warm.body)
	}
	home := warm.last

	// Send that account through a proxy that refuses connections. The fake
	// provider keeps it usable, so the failure happens on the request itself.
	if _, err := pool.CreateProxy(auth.ProxyInput{Host: ptrTo("127.0.0.1"),
		Port: ptrTo(testutil.ClosedPort(t)), AccountIDs: &[]string{home}}); err != nil {
		t.Fatal(err)
	}
	upstream.restart()
	got := runRoute(t, as, req, "anthropic")
	if got.status != http.StatusOK {
		t.Fatalf("client saw %d %s", got.status, got.body)
	}
	if got.first != home {
		t.Fatalf("the conversation left its account before failing: %s", got.first)
	}
	if got.last == home {
		t.Fatal("request stayed on the account whose proxy is dead")
	}
	// The proxied attempt never reaches the shared client; the retry must.
	tokens := upstream.tokens()
	if len(tokens) != 1 || tokens[0] != accessOf[got.last] {
		t.Fatalf("retry did not go out directly: %v, want one call as %s", tokens, got.last)
	}
	for _, a := range pool.View().Accounts {
		switch a.ID {
		case home:
			if a.Totals.Failed != 1 || (a.Status != "error" && a.Status != "cooldown") {
				t.Fatalf("broken proxy did not pause its account: %+v", a)
			}
		case got.last:
			if a.Totals.Succeeded != 1 || a.Totals.Failed != 0 {
				t.Fatalf("retry account totals %+v", a.Totals)
			}
			if a.Status != "ready" || strings.Contains(a.Error, "代理") {
				t.Fatalf("healthy direct account blamed for the proxy failure: status %q error %q", a.Status, a.Error)
			}
		}
	}
}
