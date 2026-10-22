package server

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro2api/auth"
	"kiro2api/types"
)

func TestAdminBoundaryCSRFCRUDAndSecretRedaction(t *testing.T) {
	p, err := auth.NewAccountPool(t.TempDir(), nil, 2, func(ctx context.Context, cfg auth.AuthConfig, old *types.TokenInfo) (types.TokenInfo, *auth.CreditSnapshot, error) {
		return types.TokenInfo{AccessToken: "access-secret-test", RefreshToken: cfg.RefreshToken, ExpiresAt: time.Now().Add(time.Hour)}, &auth.CreditSnapshot{Limit: 2000, Remaining: 2000, CheckedAt: time.Now()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	secret := strings.Repeat("private-admin-key-", 3)
	h, err := NewAdminRouter(p, secret, []string{"https://panel.example.com:8443"}, "../static/admin")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, key, origin, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Kiro-Admin-Token", key)
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := call("GET", "/api/accounts", "", "public-client-key", "", ""); w.Code != 401 {
		t.Fatal("API key authorized management")
	}
	status := call("GET", "/api/status", "", secret, "", "")
	var config map[string]any
	_ = json.Unmarshal(status.Body.Bytes(), &config)
	csrf := config["csrf_token"].(string)
	body := `{"accounts":[{"email":"test@example.com","refreshToken":"private-new-refresh-token-value","provider":"Github","concurrency":1}]}`
	for _, origin := range []string{"", "https://evil.example.com"} {
		if w := call("POST", "/api/accounts", body, secret, origin, csrf); w.Code != 403 {
			t.Fatal("cross-site write allowed")
		}
	}
	if w := call("POST", "/api/accounts", body, secret, "https://panel.example.com:8443", ""); w.Code != 403 {
		t.Fatal("missing CSRF token accepted")
	}
	created := call("POST", "/api/accounts", body, secret, "https://panel.example.com:8443", csrf)
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	id := p.View().Accounts[0].ID
	if w := call("PATCH", "/api/accounts/"+id, `{"version":1,"concurrency":3}`, secret, "https://panel.example.com:8443", csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call("PATCH", "/api/accounts/"+id, `{"version":1,"concurrency":10}`, secret, "https://panel.example.com:8443", csrf); w.Code != 409 {
		t.Fatal("stale edit overwrote concurrency")
	}
	read := call("GET", "/api/accounts", "", secret, "", "")
	for _, value := range []string{secret, "private-new-refresh-token-value", "access-secret-test"} {
		if strings.Contains(read.Body.String(), value) {
			t.Fatal("secret exposed in management API")
		}
	}
	if read.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("management response exposes CORS")
	}
	if w := call("DELETE", "/api/accounts/"+id, `{"version":2}`, secret, "https://panel.example.com:8443", csrf); w.Code != 200 || p.View().Accounts[0].DeletedAt == nil {
		t.Fatal("delete failed")
	}
	for _, path := range []string{"/", "/assets/admin.js", "/assets/admin.css"} {
		w := call("GET", path, "", secret, "", "")
		if w.Code != 200 {
			t.Fatal("asset missing", path, w.Code)
		}
	}
}

func TestAccountLeaseCoversStreamingLifetimeAndCancel(t *testing.T) {
	p, err := auth.NewAccountPool(t.TempDir(), []auth.AuthConfig{{AuthType: "Social", RefreshToken: "synthetic-refresh-token-value"}}, 1, func(ctx context.Context, cfg auth.AuthConfig, old *types.TokenInfo) (types.TokenInfo, *auth.CreditSnapshot, error) {
		return types.TokenInfo{AccessToken: "fake", ExpiresAt: time.Now().Add(time.Hour)}, &auth.CreditSnapshot{Remaining: 10, Limit: 10, CheckedAt: time.Now()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	as := auth.NewManagedAuthService(p)
	done := make(chan struct{})
	started := make(chan struct{})
	r := gin.New()
	r.GET("/stream", func(c *gin.Context) {
		_, release, ok := acquireRequestAccount(c, as, "test-model")
		if !ok {
			return
		}
		defer func() { release(); close(done) }()
		c.Header("Content-Type", "text/event-stream")
		c.String(200, "data: started\n\n")
		c.Writer.Flush()
		close(started)
		<-c.Request.Context().Done()
	})
	ts := httptest.NewServer(r)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/stream", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	if p.View().Accounts[0].Active != 1 {
		t.Fatal("stream released account before finishing")
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer waitCancel()
	if _, err = p.Acquire(waitCtx); err != context.DeadlineExceeded {
		t.Fatal("second request exceeded stream concurrency")
	}
	cancel()
	_, _ = io.Copy(io.Discard, resp.Body)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled stream not released")
	}
	if p.View().Accounts[0].Active != 0 || p.View().Accounts[0].Totals.Canceled != 1 {
		t.Fatal("cancel leaked slot or lost stats")
	}
}
