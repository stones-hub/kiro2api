package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/internal/testutil"
	"kiro2api/types"
)

func TestAdminProxyAPIAndRequestRouting(t *testing.T) {
	p, err := auth.NewAccountPool(t.TempDir(), []auth.AuthConfig{{AuthType: "Social", RefreshToken: "synthetic-refresh-token-value"}}, 2, func(ctx context.Context, cfg auth.AuthConfig, old *types.TokenInfo) (types.TokenInfo, *auth.CreditSnapshot, error) {
		return types.TokenInfo{AccessToken: "fake", ExpiresAt: time.Now().Add(time.Hour)}, &auth.CreditSnapshot{Remaining: 10, Limit: 10, CheckedAt: time.Now()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	secret := strings.Repeat("private-admin-key-", 3)
	origin := "https://panel.example.com:8443"
	h, err := NewAdminRouter(p, secret, []string{origin}, "../static/admin")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Kiro-Admin-Token", secret)
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	var status map[string]any
	_ = json.Unmarshal(call("GET", "/api/status", "").Body.Bytes(), &status)
	csrf := status["csrf_token"].(string)
	write := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Kiro-Admin-Token", secret)
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := call("POST", "/api/proxies", `{"host":"127.0.0.1","port":1080}`); w.Code != 403 {
		t.Fatal("proxy write without CSRF accepted")
	}
	accountID := p.View().Accounts[0].ID
	closed := testutil.ClosedPort(t)
	created := write("POST", "/api/proxies", `{"name":"出口 A","host":"127.0.0.1","port":`+strconv.Itoa(closed)+`,"username":"u","password":"private-proxy-password","account_ids":["`+accountID+`"]}`)
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	var out map[string]string
	_ = json.Unmarshal(created.Body.Bytes(), &out)
	proxyID := out["id"]
	for _, path := range []string{"/api/proxies", "/api/accounts"} {
		if body := call("GET", path, "").Body.String(); strings.Contains(body, "private-proxy-password") || !strings.Contains(body, proxyID) {
			t.Fatalf("%s leaks password or misses proxy: %s", path, body)
		}
	}
	if w := write("POST", "/api/proxies/"+proxyID+"/test", `{}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":false`) || strings.Contains(w.Body.String(), strconv.Itoa(closed)) {
		t.Fatalf("closed proxy test: %d %s", w.Code, w.Body.String())
	}
	if w := write("DELETE", "/api/proxies/"+proxyID, `{"version":1}`); w.Code != 409 {
		t.Fatalf("used proxy deleted: %d", w.Code)
	}
	if w := write("PATCH", "/api/proxies/"+proxyID, `{"version":7,"name":"x"}`); w.Code != 409 {
		t.Fatalf("stale proxy edit accepted: %d", w.Code)
	}
	if w := write("PATCH", "/api/proxies/missing", `{"version":1}`); w.Code != 404 {
		t.Fatalf("missing proxy: %d", w.Code)
	}

	// Assigning the proxy bumped the account version; switching back to direct frees the proxy.
	if w := write("PATCH", "/api/accounts/"+accountID, `{"version":2,"proxy_id":""}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := write("DELETE", "/api/proxies/"+proxyID, `{"version":1}`); w.Code != 200 {
		t.Fatalf("unused proxy not deleted: %d %s", w.Code, w.Body.String())
	}
}

func TestUpstreamRequestUsesLeaseProxyAndReportsProxyFailure(t *testing.T) {
	socks := testutil.NewSOCKS5Server(t, "", "")
	p, err := auth.NewAccountPool(t.TempDir(), []auth.AuthConfig{{AuthType: "Social", RefreshToken: "synthetic-refresh-token-value"}}, 1, func(ctx context.Context, cfg auth.AuthConfig, old *types.TokenInfo) (types.TokenInfo, *auth.CreditSnapshot, error) {
		return types.TokenInfo{AccessToken: "fake", ExpiresAt: time.Now().Add(time.Hour)}, &auth.CreditSnapshot{Remaining: 10, Limit: 10, CheckedAt: time.Now()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	accountID := p.View().Accounts[0].ID
	closed := testutil.ClosedPort(t)
	proxyID, err := p.CreateProxy(auth.ProxyInput{Host: ptrTo("127.0.0.1"), Port: ptrTo(closed), AccountIDs: &[]string{accountID}})
	if err != nil {
		t.Fatal(err)
	}
	as := auth.NewManagedAuthService(p)
	deadline := time.Now().Add(5 * time.Second)
	// The refresh over the dead proxy fails; clear it by pointing the proxy at a live server.
	for p.View().Accounts[0].Refreshing {
		if time.Now().After(deadline) {
			t.Fatal("refresh never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = p.UpdateProxy(proxyID, auth.ProxyInput{Version: 1, Port: ptrTo(socks.Port())}); err != nil {
		t.Fatal(err)
	}
	for p.View().Accounts[0].Status != "ready" {
		if time.Now().After(deadline) {
			t.Fatalf("account not ready: %+v", p.View().Accounts[0])
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Break the proxy again for the request itself (no recheck happens mid-lease).
	r := gin.New()
	r.POST("/v1/messages", func(c *gin.Context) {
		token, release, ok := acquireRequestAccount(c, as, "claude-sonnet-4-5")
		if !ok {
			return
		}
		defer release()
		socks.Close()
		req := types.AnthropicRequest{Model: "claude-sonnet-4-5", MaxTokens: 16, Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "hi"}}}
		_, _ = executeCodeWhispererRequest(c, req, token.TokenInfo, false)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}")))
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatalf("proxy failure response: %d %s", w.Code, w.Body.String())
	}
	a := p.View().Accounts[0]
	if a.Status != "error" && a.Status != "cooldown" || a.Totals.Failed != 1 {
		t.Fatalf("proxy failure did not pause account: %+v", a)
	}
	m := p.Metrics()
	found := false
	for _, e := range m.Errors24h {
		found = found || e.Name == "proxy"
	}
	if !found {
		t.Fatalf("proxy error not in metrics: %+v", m.Errors24h)
	}
}

func ptrTo[T any](v T) *T { return &v }
