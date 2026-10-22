package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/types"
)

func TestAdminKeysAndKeyAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p, err := auth.NewAccountPool(t.TempDir(), nil, 2, func(ctx context.Context, cfg auth.AuthConfig, old *types.TokenInfo) (types.TokenInfo, *auth.CreditSnapshot, error) {
		return types.TokenInfo{AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)}, &auth.CreditSnapshot{Limit: 10, Remaining: 10, CheckedAt: time.Now()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetBuiltinKey("synthetic-builtin-client-key-value")
	secret := strings.Repeat("private-admin-key-", 3)
	origin := "https://panel.example.com:8443"
	h, err := NewAdminRouter(p, secret, []string{origin}, "../static/admin")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Kiro-Admin-Token", secret)
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	var status map[string]any
	_ = json.Unmarshal(call("GET", "/api/status", "", "").Body.Bytes(), &status)
	csrf := status["csrf_token"].(string)
	if w := call("POST", "/api/keys", `{"name":"team"}`, "wrong"); w.Code != 403 {
		t.Fatal("key created without CSRF")
	}
	created := call("POST", "/api/keys", `{"name":"team"}`, csrf)
	if created.Code != 201 {
		t.Fatal(created.Body.String())
	}
	var out struct{ ID, Key string }
	_ = json.Unmarshal(created.Body.Bytes(), &out)
	listing := call("GET", "/api/keys", "", "")
	if strings.Contains(listing.Body.String(), out.Key) || !strings.Contains(listing.Body.String(), `"builtin":true`) {
		t.Fatal("listing leaks key or misses builtin")
	}

	api := gin.New()
	api.Use(KeyAuthMiddleware(p, []string{"/v1"}))
	api.GET("/v1/whoami", func(c *gin.Context) { c.String(200, c.GetString("api_key_id")) })
	who := func(header, value string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/whoami", nil)
		if header != "" {
			req.Header.Set(header, value)
		}
		w := httptest.NewRecorder()
		api.ServeHTTP(w, req)
		return w
	}
	if w := who("Authorization", "Bearer "+out.Key); w.Code != 200 || w.Body.String() != out.ID {
		t.Fatalf("created key: %d %s", w.Code, w.Body.String())
	}
	if w := who("x-api-key", "synthetic-builtin-client-key-value"); w.Code != 200 || w.Body.String() != auth.BuiltinKeyID {
		t.Fatal("builtin key rejected")
	}
	if w := who("", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("missing key accepted")
	}
	if w := call("DELETE", "/api/keys/"+out.ID, `{"version":1}`, csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := who("x-api-key", out.Key); w.Code != http.StatusUnauthorized {
		t.Fatal("deleted key accepted")
	}
	if w := call("GET", "/api/metrics?key="+out.ID, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"key_id":"`+out.ID) {
		t.Fatal("key metrics unavailable", w.Code)
	}
	if w := call("GET", "/api/metrics?key=missing", "", ""); w.Code != 404 {
		t.Fatal("unknown key metrics", w.Code)
	}
	if w := call("PATCH", "/api/keys/env", `{"version":0,"enabled":false}`, csrf); w.Code != 400 {
		t.Fatal("builtin key edited", w.Code)
	}
}
