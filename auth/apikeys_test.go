package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPIKeysAuthenticateAndPersist(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAccountPool(dir, []AuthConfig{{AuthType: AuthMethodSocial, RefreshToken: "synthetic-private-refresh-token"}}, 2, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	p.SetBuiltinKey("synthetic-builtin-client-key-value")
	if id, ok := p.AuthenticateKey("synthetic-builtin-client-key-value"); !ok || id != BuiltinKeyID {
		t.Fatal("builtin key rejected")
	}
	if _, _, err = p.CreateAPIKey("  "); err == nil {
		t.Fatal("empty name accepted")
	}
	id, raw, err := p.CreateAPIKey("团队 A")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, apiKeyPrefix) || len(raw) < 40 {
		t.Fatalf("weak key %q", raw)
	}
	if got, ok := p.AuthenticateKey(raw); !ok || got != id {
		t.Fatal("created key rejected")
	}
	if _, ok := p.AuthenticateKey(raw + "x"); ok {
		t.Fatal("wrong key accepted")
	}
	for _, k := range p.APIKeys() {
		if b, _ := json.Marshal(k); strings.Contains(string(b), raw) || strings.Contains(string(b), "hash") {
			t.Fatal("key material exposed in view")
		}
	}
	if err = p.UpdateAPIKey(BuiltinKeyID, APIKeyPatch{Enabled: ptr(false)}); err == nil {
		t.Fatal("builtin key changed from the page")
	}
	if err = p.UpdateAPIKey(id, APIKeyPatch{Version: 1, Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.AuthenticateKey(raw); ok {
		t.Fatal("disabled key accepted")
	}
	if err = p.UpdateAPIKey(id, APIKeyPatch{Version: 1, Enabled: ptr(true)}); err != ErrKeyConflict {
		t.Fatalf("stale update: %v", err)
	}
	if err = p.UpdateAPIKey(id, APIKeyPatch{Version: 2, Enabled: ptr(true), Name: ptr("团队 B")}); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if strings.Contains(string(data), raw) {
		t.Fatal("raw key written to disk")
	}
	reopened, err := NewAccountPool(dir, nil, 2, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, ok := reopened.AuthenticateKey(raw); !ok || got != id {
		t.Fatal("key lost on restart")
	}
	if err = reopened.DeleteAPIKey(id, 3); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.AuthenticateKey(raw); ok {
		t.Fatal("deleted key accepted")
	}
	if _, err = reopened.MetricsForKey(id); err != nil {
		t.Fatal("deleted key usage no longer viewable")
	}
	if _, err = reopened.MetricsForKey("missing"); err != ErrKeyMissing {
		t.Fatal("unknown key accepted")
	}
}

func TestMetricsSplitByKey(t *testing.T) {
	p, _ := managedTestPool(t, 3)
	p.SetBuiltinKey("synthetic-builtin-client-key-value")
	id, _, err := p.CreateAPIKey("team")
	if err != nil {
		t.Fatal(err)
	}
	acquire := func(key string) *AccountLease {
		ctx, cancel := context.WithTimeout(WithAPIKeyID(context.Background(), key), time.Second)
		defer cancel()
		l, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	acquire(id).Release(RequestOutcome{Success: true, Model: "m1", InputTokens: 100, OutputTokens: 7})
	acquire(BuiltinKeyID).Release(RequestOutcome{Success: true, Model: "m2", InputTokens: 5})
	// Last, because a 429 puts the only test account into cooldown.
	acquire(id).Release(RequestOutcome{UpstreamStatus: 429, Model: "m1"})
	all := p.Metrics()
	if h := all.Hours[23]; h.Requests != 3 || h.InputTokens != 105 {
		t.Fatalf("pool totals %+v", h)
	}
	team, err := p.MetricsForKey(id)
	if err != nil {
		t.Fatal(err)
	}
	if h := team.Hours[23]; h.Requests != 2 || h.Failed != 1 || h.InputTokens != 100 || h.OutputTokens != 7 {
		t.Fatalf("key hour %+v", h)
	}
	if d := team.Days[metricDayRetention-1]; d.Requests != 2 {
		t.Fatalf("key day %+v", d)
	}
	if len(team.Models24h) != 1 || team.Models24h[0].Name != "m1" || len(team.Errors24h) != 1 {
		t.Fatalf("key breakdown %+v %+v", team.Models24h, team.Errors24h)
	}
	for _, k := range p.APIKeys() {
		if k.ID == id && (k.Today.Requests != 2 || k.Last24h.InputTokens != 100 || k.LastUsed == nil) {
			t.Fatalf("key view %+v", k)
		}
		if k.ID == BuiltinKeyID && k.Today.Requests != 1 {
			t.Fatalf("builtin view %+v", k)
		}
	}
}
