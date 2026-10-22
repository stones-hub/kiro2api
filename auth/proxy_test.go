package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro2api/internal/testutil"
	"kiro2api/types"
	"kiro2api/utils"
)

// proxiedTestPool's provider performs a real HTTP call so the account route is exercised.
func proxiedTestPool(t *testing.T, dir string, target string) *AccountPool {
	t.Helper()
	provider := func(ctx context.Context, cfg AuthConfig, old *types.TokenInfo) (types.TokenInfo, *CreditSnapshot, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		resp, err := utils.DoRequest(req)
		if err != nil {
			return types.TokenInfo{}, nil, err
		}
		resp.Body.Close()
		return managedTestProvider(ctx, cfg, old)
	}
	p, err := NewAccountPool(dir, []AuthConfig{{AuthType: AuthMethodSocial, RefreshToken: "synthetic-private-refresh-token"}}, 2, provider)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitAccount(t *testing.T, p *AccountPool, ok func(AccountView) bool) AccountView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a := p.View().Accounts[0]
		if !a.Refreshing && ok(a) {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("account never reached expected state: %+v", a)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProxyRoutingLifecycleAndPersistence(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	socks := testutil.NewSOCKS5Server(t, "proxy-user", "private-proxy-password")
	dir := t.TempDir()
	p := proxiedTestPool(t, dir, target.URL)
	id := waitAccount(t, p, func(a AccountView) bool { return a.Status == "ready" }).ID
	if socks.Connects.Load() != 0 {
		t.Fatal("direct account used the proxy")
	}

	proxyID, err := p.CreateProxy(ProxyInput{Name: ptr("东京出口"), Host: ptr("127.0.0.1"), Port: ptr(socks.Port()), Username: ptr("proxy-user"), Password: ptr("private-proxy-password"), AccountIDs: &[]string{id}})
	if err != nil {
		t.Fatal(err)
	}
	a := waitAccount(t, p, func(a AccountView) bool { return socks.Connects.Load() > 0 })
	if a.ProxyID != proxyID || a.Status != "ready" {
		t.Fatalf("account not rechecked through proxy: %+v", a)
	}
	lease := testAcquire(t, p)
	if lease.Client == nil {
		t.Fatal("lease lost the proxy route")
	}
	before := socks.Connects.Load()
	lease.Client.CloseIdleConnections()
	resp, err := lease.Client.Get(target.URL)
	if err != nil || socks.Connects.Load() != before+1 {
		t.Fatalf("lease client not proxied: %v", err)
	}
	resp.Body.Close()
	lease.Release(RequestOutcome{Success: true})

	// Credentials never leave the account document.
	view, _ := json.Marshal(p.View())
	if strings.Contains(string(view), "private-proxy-password") || !strings.Contains(string(view), `"has_password":true`) {
		t.Fatalf("proxy view leaks or hides password state: %s", view)
	}
	if err = p.DeleteProxy(proxyID, 1); !errors.Is(err, ErrProxyInUse) {
		t.Fatalf("deleting a used proxy must fail, got %v", err)
	}

	// A broken proxy puts the account on hold instead of silently going direct.
	if err = p.UpdateProxy(proxyID, ProxyInput{Version: 1, Password: ptr("wrong-password")}); err != nil {
		t.Fatal(err)
	}
	a = waitAccount(t, p, func(a AccountView) bool { return a.Status == "error" })
	if !strings.Contains(a.Error, "代理认证失败") {
		t.Fatalf("unexpected error: %q", a.Error)
	}
	if _, err = p.Acquire(context.Background()); err != ErrPoolUnavailable {
		t.Fatal("account with failing proxy still received work")
	}
	if err = p.UpdateProxy(proxyID, ProxyInput{Version: 2, Password: ptr("private-proxy-password")}); err != nil {
		t.Fatal(err)
	}
	waitAccount(t, p, func(a AccountView) bool { return a.Status == "ready" })

	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := proxiedTestPool(t, dir, target.URL)
	defer reopened.Close()
	if v := reopened.View(); len(v.Proxies) != 1 || v.Proxies[0].Name != "东京出口" || v.Accounts[0].ProxyID != proxyID {
		t.Fatalf("proxy config not persisted: %+v", v.Proxies)
	}
	// Unchecking every account detaches them; the proxy can then be removed.
	if err = reopened.UpdateProxy(proxyID, ProxyInput{Version: 3, AccountIDs: &[]string{}}); err != nil {
		t.Fatal(err)
	}
	if reopened.View().Accounts[0].ProxyID != "" {
		t.Fatal("unchecked account kept proxy")
	}
	if err = reopened.DeleteProxy(proxyID, 4); err != nil {
		t.Fatal(err)
	}
}

func TestProxyValidationAndRequestFailure(t *testing.T) {
	p, id := managedTestPool(t, 2)
	for _, in := range []ProxyInput{
		{Host: ptr("socks5://1.2.3.4"), Port: ptr(1080)},
		{Host: ptr("1.2.3.4"), Port: ptr(0)},
		{Host: ptr("1.2.3.4"), Port: ptr(1080), Password: ptr("no-user")},
		{Host: ptr("1.2.3.4"), Port: ptr(1080), AccountIDs: &[]string{"missing"}},
	} {
		if _, err := p.CreateProxy(in); err == nil {
			t.Fatalf("invalid proxy accepted: %+v", in)
		}
	}
	if len(p.Proxies()) != 0 {
		t.Fatal("rejected proxy persisted")
	}
	if err := p.Update(id, AccountPatch{Version: 1, ProxyID: ptr("missing")}); !errors.Is(err, ErrProxyMissing) {
		t.Fatalf("unknown proxy assigned: %v", err)
	}
	proxyID, err := p.CreateProxy(ProxyInput{Host: ptr("::1"), Port: ptr(testutil.ClosedPort(t))})
	if err != nil {
		t.Fatal(err)
	}
	if v := p.Proxies()[0]; v.Host != "::1" || v.Accounts != 0 {
		t.Fatalf("unexpected view %+v", v)
	}
	if _, err = p.TestProxy(context.Background(), proxyID); err == nil || strings.Contains(err.Error(), "::1") {
		t.Fatalf("closed proxy passed test or leaked address: %v", err)
	}
	// A request that fails at the proxy takes the account out of rotation.
	l := testAcquire(t, p)
	l.Release(RequestOutcome{ProxyError: true, Status: 502})
	if _, err = p.Acquire(context.Background()); err != ErrPoolUnavailable {
		t.Fatal("proxy failure did not pause the account")
	}
	if outcomeClass(RequestOutcome{ProxyError: true, Status: 502}) != "proxy" {
		t.Fatal("proxy failures not classified")
	}
}

func TestDanglingProxyReferenceRefusesToLoad(t *testing.T) {
	dir := t.TempDir()
	doc := `{"version":1,"started_at":"2026-09-30T00:00:00Z","accounts":[{"id":"a","name":"a","credentials":{"auth":"Social","refreshToken":"synthetic-private-refresh-token"},"concurrency":2,"proxy_id":"gone","version":1}]}`
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := NewAccountPool(dir, nil, 2, managedTestProvider); err == nil {
		p.Close()
		t.Fatal("account with missing proxy would connect directly")
	}
}

func TestProxyProbeReachesUpstream(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer target.Close()
	old := proxyProbeURL
	proxyProbeURL = target.URL
	t.Cleanup(func() { proxyProbeURL = old })
	socks := testutil.NewSOCKS5Server(t, "", "")
	p, _ := managedTestPool(t, 2)
	id, err := p.CreateProxy(ProxyInput{Host: ptr("127.0.0.1"), Port: ptr(socks.Port())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.TestProxy(context.Background(), id); err != nil || socks.Connects.Load() != 1 {
		t.Fatalf("probe failed: %v", err)
	}
}
