package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/discount"
	"kiro2api/internal/testutil"
	"kiro2api/types"
	"kiro2api/utils"
)

// The route prepares the discount before choosing an account, so a continued
// conversation returns to its account and keeps the discount; when it has to
// move, the discount is withdrawn because Kiro's cache stays behind.
func TestConversationAffinityRoutesAndPricesByAccount(t *testing.T) {
	oldClient, oldCache := utils.SharedHTTPClient, localDiscountCache
	t.Cleanup(func() { utils.SharedHTTPClient = oldClient; localDiscountCache = oldCache })
	var err error
	if localDiscountCache, err = discount.New(5*time.Minute, 100, 1024); err != nil {
		t.Fatal(err)
	}
	fixture := testutil.EventFrame("assistantResponseEvent", `{"content":"OK"}`)
	utils.SharedHTTPClient = &http.Client{Transport: cacheTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(fixture))}, nil
	})}
	p, err := auth.NewAccountPool(t.TempDir(), []auth.AuthConfig{
		{AuthType: "Social", RefreshToken: "synthetic-affinity-one-refresh-token"},
		{AuthType: "Social", RefreshToken: "synthetic-affinity-two-refresh-token"},
	}, 1, func(ctx context.Context, cfg auth.AuthConfig, old *types.TokenInfo) (types.TokenInfo, *auth.CreditSnapshot, error) {
		return types.TokenInfo{AccessToken: "fake", ExpiresAt: time.Now().Add(time.Hour)}, &auth.CreditSnapshot{Remaining: 10, Limit: 10, CheckedAt: time.Now()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready := 0
		for _, a := range p.View().Accounts {
			if a.Status == "ready" {
				ready++
			}
		}
		if ready == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accounts never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	as := auth.NewManagedAuthService(p)
	req := types.AnthropicRequest{Model: "claude-opus-4-8", MaxTokens: 16, Messages: []types.AnthropicRequestMessage{
		{Role: "user", Content: strings.Repeat("The notebook is on a wooden shelf. ", 250)},
	}}
	// run mirrors the /v1/messages route order: prepare, acquire, handle, release.
	run := func() (account, affinity string, cacheRead float64) {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("X-API-Key", "local-test-key")
		prepareCacheDiscount(c, req)
		token, release, ok := acquireRequestAccount(c, as, req.Model)
		if !ok {
			t.Fatalf("acquire failed: %d %s", recorder.Code, recorder.Body.String())
		}
		r := accountMeasurement(c)
		account, affinity = r.lease.AccountID(), r.outcome.Affinity
		handleNonStreamRequest(c, req, token.TokenInfo)
		release()
		if recorder.Code != 200 {
			t.Fatalf("status %d", recorder.Code)
		}
		usage := decodeUsage(t, recorder.Body.Bytes())
		return account, affinity, usage["cache_read_input_tokens"].(float64)
	}

	first, aff, read := run()
	if aff != "" || read != 0 {
		t.Fatalf("first turn: affinity %q read %v", aff, read)
	}
	again, aff, read := run()
	if again != first || aff != "kept" || read <= 0 {
		t.Fatalf("continued turn: account %s (want %s) affinity %q read %v", again, first, aff, read)
	}

	// Take the conversation's account out of service: the turn must move and pay full input.
	var version int
	for _, a := range p.View().Accounts {
		if a.ID == first {
			version = a.Version
		}
	}
	off := false
	if err := p.Update(first, auth.AccountPatch{Version: version, Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	moved, aff, read := run()
	if moved == first || aff != "moved" || read != 0 {
		t.Fatalf("moved turn: account %s affinity %q read %v", moved, aff, read)
	}
	// The conversation now belongs to the new account and earns the discount there.
	next, aff, read := run()
	if next != moved || aff != "kept" || read <= 0 {
		t.Fatalf("after move: account %s affinity %q read %v", next, aff, read)
	}
}

func decodeUsage(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var data map[string]any
	if err := utils.SafeUnmarshal(body, &data); err != nil {
		t.Fatal(err)
	}
	return data["usage"].(map[string]any)
}
