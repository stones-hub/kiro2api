package auth

import (
	"errors"
	"fmt"
	"testing"
)

func TestAccountBatch(t *testing.T) {
	boot := []AuthConfig{}
	for i := 0; i < 3; i++ {
		boot = append(boot, AuthConfig{AuthType: AuthMethodSocial, RefreshToken: fmt.Sprintf("synthetic-batch-refresh-token-%d", i)})
	}
	p, err := NewAccountPool(t.TempDir(), boot, 2, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	v := p.View().Accounts
	ids := []string{v[0].ID, v[1].ID}
	if _, err = p.Batch(AccountBatch{IDs: ids, Action: "update"}); err == nil {
		t.Fatal("empty update accepted")
	}
	if _, err = p.Batch(AccountBatch{IDs: ids, Action: "update", Concurrency: ptr(0)}); err == nil {
		t.Fatal("bad concurrency accepted")
	}
	if _, err = p.Batch(AccountBatch{IDs: ids, Action: "update", ProxyID: ptr("missing")}); !errors.Is(err, ErrProxyMissing) {
		t.Fatalf("missing proxy: %v", err)
	}
	// One unknown ID rejects the whole batch.
	if _, err = p.Batch(AccountBatch{IDs: append(ids, "nope"), Action: "disable"}); !errors.Is(err, ErrAccountMissing) || !p.View().Accounts[0].Enabled {
		t.Fatalf("partial batch applied: %v", err)
	}
	pid, err := p.CreateProxy(ProxyInput{Host: ptr("127.0.0.1"), Port: ptr(9)})
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.Batch(AccountBatch{IDs: append(ids, ids[0]), Action: "update", Concurrency: ptr(7), ProxyID: ptr(pid), Enabled: ptr(false)})
	if err != nil || n != 2 {
		t.Fatalf("update %d %v", n, err)
	}
	v = p.View().Accounts
	for _, a := range v[:2] {
		if a.Concurrency != 7 || a.ProxyID != pid || a.Enabled || a.Version != 2 {
			t.Fatalf("not applied: %+v", a)
		}
	}
	if v[2].Concurrency != 2 || !v[2].Enabled {
		t.Fatal("unselected account changed")
	}
	if _, err = p.Batch(AccountBatch{IDs: ids, Action: "enable"}); err != nil || !p.View().Accounts[1].Enabled {
		t.Fatalf("enable: %v", err)
	}
	if _, err = p.Batch(AccountBatch{IDs: ids, Action: "update", ProxyID: ptr("")}); err != nil || p.View().Accounts[0].ProxyID != "" {
		t.Fatalf("back to direct: %v", err)
	}
	if _, err = p.Batch(AccountBatch{IDs: ids, Action: "delete"}); err != nil {
		t.Fatal(err)
	}
	if v = p.View().Accounts; v[0].DeletedAt == nil || v[1].DeletedAt == nil || v[2].DeletedAt != nil {
		t.Fatal("delete wrong accounts")
	}
	if _, err = p.Batch(AccountBatch{IDs: ids, Action: "enable"}); !errors.Is(err, ErrAccountMissing) {
		t.Fatal("deleted accounts edited")
	}
	if _, err = p.Batch(AccountBatch{IDs: ids, Action: "wipe"}); err == nil {
		t.Fatal("unknown action accepted")
	}
}
