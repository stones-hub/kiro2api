package utils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"kiro2api/internal/testutil"
)

func TestSOCKS5ClientRoutesAndClassifiesErrors(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	socks := testutil.NewSOCKS5Server(t, "proxy-user", "proxy-secret")

	get := func(client *http.Client) error {
		req, _ := http.NewRequestWithContext(WithHTTPClient(context.Background(), client), "GET", target.URL, nil)
		resp, err := DoRequest(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(NewHTTPClient(&SOCKS5Proxy{Address: socks.Addr, Username: "proxy-user", Password: "proxy-secret"})); err != nil {
		t.Fatal(err)
	}
	if socks.Connects.Load() != 1 {
		t.Fatal("request did not go through the SOCKS5 proxy")
	}
	// A nil client keeps the shared direct client.
	if err := get(nil); err != nil || socks.Connects.Load() != 1 {
		t.Fatalf("direct request was proxied or failed: %v", err)
	}

	cases := map[string]*SOCKS5Proxy{
		"代理认证失败":  {Address: socks.Addr, Username: "proxy-user", Password: "wrong-secret"},
		"代理拒绝连接":  {Address: "127.0.0.1:" + strconv.Itoa(testutil.ClosedPort(t))},
		"代理认证失败 ": {Address: socks.Addr}, // server requires credentials
	}
	for want, cfg := range cases {
		err := get(NewHTTPClient(cfg))
		if !IsProxyError(err) {
			t.Fatalf("%s: not classified as proxy error: %v", want, err)
		}
		msg := ProxyErrorMessage(err)
		if !strings.HasPrefix(msg, strings.TrimSpace(want)) || strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "secret") {
			t.Fatalf("%s: unsafe or wrong message %q", want, msg)
		}
	}
	if IsProxyError(context.Canceled) {
		t.Fatal("cancellation counted as proxy failure")
	}
}
