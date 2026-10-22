package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFetchUpstreamModelsPagesAndSaves(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if r.URL.Query().Get("nextToken") == "" {
			_, _ = w.Write([]byte(`{"models":[{"modelId":"auto","tokenLimits":{"maxInputTokens":1,"maxOutputTokens":1}}],"nextToken":"p2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"modelId":"claude-sonnet-5.5","tokenLimits":{"maxInputTokens":1,"maxOutputTokens":1}}]}`))
	}))
	defer srv.Close()
	old := ListModelsURL
	ListModelsURL = srv.URL + "/ListAvailableModels"
	t.Cleanup(func() { ListModelsURL = old })

	p, id := managedTestPool(t, 2)
	deadline := time.Now().Add(3 * time.Second)
	for p.View().Accounts[0].Status != "ready" {
		if time.Now().After(deadline) {
			t.Fatal("account never ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := p.FetchUpstreamModels(context.Background(), "missing"); err != ErrAccountMissing {
		t.Fatalf("unknown account: %v", err)
	}
	up, err := p.FetchUpstreamModels(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if up.AccountID != id || len(up.Models) != 2 || len(auth) != 2 || auth[0] != "Bearer private-access-value" {
		t.Fatalf("fetch %+v %v", up, auth)
	}
	if err = p.SaveModelCatalog([]byte(`{"models":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err = p.SaveModelCatalog([]byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(p.ModelCatalogPath())
	prev, _ := os.ReadFile(strings.TrimSuffix(p.ModelCatalogPath(), "models.json") + "models.previous.json")
	if string(cur) != `{"v":2}` || string(prev) != `{"models":[]}` {
		t.Fatal("catalog save or backup wrong")
	}
}
