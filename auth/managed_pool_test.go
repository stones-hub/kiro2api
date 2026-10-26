package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro2api/types"
)

func managedTestProvider(ctx context.Context, cfg AuthConfig, old *types.TokenInfo) (types.TokenInfo, *CreditSnapshot, error) {
	return types.TokenInfo{AccessToken: "private-access-value", RefreshToken: cfg.RefreshToken, ExpiresAt: time.Now().Add(time.Hour)}, &CreditSnapshot{Email: "owner@example.com", Limit: 2000, Used: 100, Remaining: 1900, CheckedAt: time.Now()}, nil
}
func managedTestPool(t *testing.T, limit int) (*AccountPool, string) {
	t.Helper()
	p, err := NewAccountPool(t.TempDir(), []AuthConfig{{AuthType: AuthMethodSocial, RefreshToken: "synthetic-private-refresh-token"}}, limit, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	id := p.View().Accounts[0].ID
	return p, id
}
func testAcquire(t *testing.T, p *AccountPool) *AccountLease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestManagedConcurrencyLimitAndCanceledWait(t *testing.T) {
	p, id := managedTestPool(t, 2)
	first := testAcquire(t, p)
	second := testAcquire(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx); err != context.DeadlineExceeded {
		t.Fatalf("expected queue cancellation, got %v", err)
	}
	if p.View().Waiting != 0 || p.View().Accounts[0].Active != 2 {
		t.Fatal("wait leaked or concurrency exceeded")
	}
	if err := p.Update(id, AccountPatch{Version: 1, Concurrency: ptr(1)}); err != nil {
		t.Fatal(err)
	}
	first.Release(RequestOutcome{Success: true, Model: "claude-opus-5-5", InputTokens: 100, OutputTokens: 3, CacheReadTokens: 40})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel2()
	if _, err := p.Acquire(ctx2); err != context.DeadlineExceeded {
		t.Fatal("lowered limit allowed new work before previous request finished")
	}
	second.Release(RequestOutcome{Canceled: true})
	second.Release(RequestOutcome{Success: true})
	l := testAcquire(t, p)
	l.Release(RequestOutcome{Success: true})
	v := p.View().Accounts[0]
	if v.Active != 0 || v.Totals.Requests != 3 || v.Totals.Canceled != 1 || v.Totals.CacheReadTokens != 40 {
		t.Fatalf("invalid completion stats: %+v", v.Totals)
	}
}
func ptr[T any](v T) *T { return &v }

func TestManagedDisableDeleteAndPersistence(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAccountPool(dir, []AuthConfig{{AuthType: AuthMethodSocial, RefreshToken: "synthetic-private-refresh-token"}}, 2, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	id := p.View().Accounts[0].ID
	l := testAcquire(t, p)
	if err = p.Update(id, AccountPatch{Version: 1, Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Acquire(context.Background()); err != ErrPoolUnavailable {
		t.Fatal("disabled account received request")
	}
	if err = p.Update(id, AccountPatch{Version: 1, Concurrency: ptr(5)}); err != ErrAccountConflict {
		t.Fatal("stale write was accepted")
	}
	if err = p.Delete(id, 2); err != nil {
		t.Fatal(err)
	}
	l.Release(RequestOutcome{Success: true, InputTokens: 200})
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "synthetic-private-refresh-token") {
		t.Fatal("deleted credential kept in current account document")
	}
	newPool, err := NewAccountPool(dir, []AuthConfig{{RefreshToken: "different-bootstrap-secret"}}, 2, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer newPool.Close()
	v := newPool.View()
	if len(v.Accounts) != 1 || v.Accounts[0].DeletedAt == nil || v.Accounts[0].Totals.InputTokens != 200 {
		t.Fatal("deleted account restored or stats lost on restart")
	}
}

func TestManagedImportAtomicDuplicateAndSafeView(t *testing.T) {
	p, id := managedTestPool(t, 2)
	testAcquire(t, p).Release(RequestOutcome{Success: true})
	items := []ImportAccount{{Email: "[new@example.com](mailto:new@example.com)", Provider: "Github", AuthConfig: AuthConfig{RefreshToken: "new-synthetic-private-refresh-token"}}, {AuthConfig: AuthConfig{RefreshToken: "short"}}}
	if _, _, err := p.Import(items); err == nil || len(p.View().Accounts) != 1 {
		t.Fatal("invalid batch partially persisted")
	}
	ids, duplicates, err := p.Import([]ImportAccount{items[0], items[0]})
	if err != nil || len(ids) != 1 || duplicates != 1 {
		t.Fatalf("import %v %d %v", ids, duplicates, err)
	}
	view, _ := json.Marshal(p.View())
	for _, secret := range []string{"private-refresh-token", "private-access-value", "clientSecret", "refreshToken", "accessToken"} {
		if strings.Contains(string(view), secret) {
			t.Fatalf("credential leaked: %s", secret)
		}
	}
	if err = p.Update(id, AccountPatch{Version: 1, RefreshToken: ptr("new-synthetic-private-refresh-token")}); err == nil {
		t.Fatal("cross-account duplicate credential allowed")
	}
	// Persistence failure must leave the effective runtime configuration
	// untouched. A directory where the backup file belongs makes the save fail
	// without touching p.path, which the pool treats as fixed once it is
	// running and reads from its own goroutines.
	backup := filepath.Join(filepath.Dir(p.path), "accounts.previous.json")
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(backup, 0700); err != nil {
		t.Fatal(err)
	}
	err = p.Update(id, AccountPatch{Version: 1, Concurrency: ptr(9)})
	if err == nil || p.View().Accounts[0].Concurrency != 2 {
		t.Fatal("failed persistence changed live concurrency")
	}
}

func TestManagedImportAcceptsBuilderIDExport(t *testing.T) {
	p, _ := managedTestPool(t, 2)
	items := []ImportAccount{{
		Email:    "builder@example.com",
		Provider: "BuilderId",
		AuthConfig: AuthConfig{
			RefreshToken: "builder-id-refresh-token-with-enough-length",
			ClientID:     "builder-client-id",
			ClientSecret: "builder-client-secret",
		},
	}}
	ids, duplicates, err := p.Import(items)
	if err != nil {
		t.Fatalf("Builder ID export was rejected: %v", err)
	}
	if len(ids) != 1 || duplicates != 0 {
		t.Fatalf("unexpected import result: ids=%d duplicates=%d", len(ids), duplicates)
	}
	view := p.View()
	if len(view.Accounts) != 2 || view.Accounts[1].Auth != AuthMethodIdC {
		t.Fatalf("Builder ID export was not normalized to IdC: %+v", view.Accounts)
	}
}

func TestManagedParallelAcquireNeverExceedsLimit(t *testing.T) {
	p, _ := managedTestPool(t, 3)
	testAcquire(t, p).Release(RequestOutcome{Success: true})
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			l, err := p.Acquire(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if p.View().Accounts[0].Active > 3 {
				t.Error("limit exceeded")
			}
			time.Sleep(time.Millisecond)
			l.Release(RequestOutcome{Success: true})
		}()
	}
	wg.Wait()
	v := p.View().Accounts[0]
	if v.Active != 0 || v.Totals.Requests != 31 {
		t.Fatal("concurrency or completion accounting leaked")
	}
}

func TestManagedCorruptStoreDoesNotReimport(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "accounts.json")
	_ = os.WriteFile(file, []byte(`{"broken"`), 0600)
	if p, err := NewAccountPool(dir, nil, 2, managedTestProvider); err == nil {
		p.Close()
		t.Fatal("corrupt store was ignored")
	}
	b, _ := os.ReadFile(file)
	if string(b) != `{"broken"` {
		t.Fatal("corrupt document overwritten")
	}
}

func TestManagedRestoreDeletedAccount(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAccountPool(dir, []AuthConfig{{AuthType: AuthMethodSocial, RefreshToken: "synthetic-private-refresh-token"}}, 3, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	id := p.View().Accounts[0].ID
	testAcquire(t, p).Release(RequestOutcome{Success: true, InputTokens: 50})
	if err = p.Restore(id, AccountRestore{Version: 1, AuthConfig: AuthConfig{RefreshToken: "replacement-refresh-token-value"}}); err == nil {
		t.Fatal("live account restored")
	}
	if err = p.Delete(id, 1); err != nil {
		t.Fatal(err)
	}
	if err = p.Restore(id, AccountRestore{Version: 1, AuthConfig: AuthConfig{RefreshToken: "replacement-refresh-token-value"}}); err != ErrAccountConflict {
		t.Fatalf("stale restore accepted: %v", err)
	}
	if err = p.Restore(id, AccountRestore{Version: 2, AuthConfig: AuthConfig{RefreshToken: "short"}}); err == nil {
		t.Fatal("invalid credential accepted")
	}
	// The test provider reports owner@example.com, so a different email must be refused.
	if err = p.Restore(id, AccountRestore{Version: 2, Email: "someone@example.com", AuthConfig: AuthConfig{RefreshToken: "replacement-refresh-token-value"}}); err == nil {
		t.Fatal("credential for a different email accepted")
	}
	if _, _, err = p.Import([]ImportAccount{{AuthConfig: AuthConfig{RefreshToken: "already-used-refresh-token"}}}); err != nil {
		t.Fatal(err)
	}
	if err = p.Restore(id, AccountRestore{Version: 2, AuthConfig: AuthConfig{RefreshToken: "already-used-refresh-token"}}); err == nil {
		t.Fatal("credential of another live account accepted")
	}
	if err = p.Restore(id, AccountRestore{Version: 2, Email: "OWNER@example.com", Provider: "Github", AuthConfig: AuthConfig{RefreshToken: "replacement-refresh-token-value"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		v := p.View().Accounts[0]
		if v.ID != id {
			t.Fatal("restored account lost its position")
		}
		if v.Status == "ready" {
			if v.DeletedAt != nil || v.Concurrency != 3 || v.Totals.InputTokens != 50 || v.Version != 3 {
				t.Fatalf("restore lost settings or stats: %+v", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restored account never became ready: %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewAccountPool(dir, nil, 2, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if v := reopened.View().Accounts[0]; v.DeletedAt != nil {
		t.Fatal("restore not persisted")
	}
}

func TestManagedFreePlanAutoDisable(t *testing.T) {
	var plan atomic.Value
	plan.Store("KIRO POWER")
	provider := func(ctx context.Context, cfg AuthConfig, old *types.TokenInfo) (types.TokenInfo, *CreditSnapshot, error) {
		return types.TokenInfo{AccessToken: "x", RefreshToken: cfg.RefreshToken, ExpiresAt: time.Now().Add(time.Hour)}, &CreditSnapshot{Plan: plan.Load().(string), Limit: 50, Remaining: 50, CheckedAt: time.Now()}, nil
	}
	dir := t.TempDir()
	p, err := NewAccountPool(dir, []AuthConfig{{AuthType: AuthMethodSocial, RefreshToken: "synthetic-private-refresh-token"}}, 2, provider)
	if err != nil {
		t.Fatal(err)
	}
	id := p.View().Accounts[0].ID
	wait := func(ok func(AccountView) bool) AccountView {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			v := p.View().Accounts[0]
			if !v.Refreshing && ok(v) {
				return v
			}
			if time.Now().After(deadline) {
				t.Fatalf("unexpected state: %+v", v)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait(func(v AccountView) bool { return v.Status == "ready" })
	plan.Store("KIRO FREE")
	p.mu.Lock()
	p.doc.Accounts[0].Usage.CheckedAt = time.Time{}
	p.mu.Unlock()
	if _, err = p.Refresh(id); err != nil {
		t.Fatal(err)
	}
	v := wait(func(v AccountView) bool { return v.Status == "disabled" })
	if v.Enabled || v.DisabledReason == "" || v.Version != 2 {
		t.Fatalf("free account not auto-disabled: %+v", v)
	}
	if _, err = p.Acquire(context.Background()); err != ErrPoolUnavailable {
		t.Fatal("free account still received work")
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewAccountPool(dir, nil, 2, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if v = reopened.View().Accounts[0]; v.Enabled || v.DisabledReason == "" {
		t.Fatal("auto-disable not persisted")
	}
	// Re-enabling by hand clears the reason.
	plan.Store("KIRO POWER")
	if err = reopened.Update(id, AccountPatch{Version: 2, Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if v = reopened.View().Accounts[0]; !v.Enabled || v.DisabledReason != "" {
		t.Fatalf("manual enable kept reason: %+v", v)
	}
}

func TestManagedMonthlyLimitSurvivesRefresh(t *testing.T) {
	p, id := managedTestPool(t, 2)
	testAcquire(t, p).Release(RequestOutcome{Success: true})
	testAcquire(t, p).Release(RequestOutcome{UpstreamStatus: 402, MonthlyLimit: true, Status: 500})
	v := p.View().Accounts[0]
	if v.Status != "cooldown" || !strings.Contains(v.Error, "MONTHLY_REQUEST_COUNT") {
		t.Fatalf("monthly limit not paused: %+v", v)
	}
	// The usage API still reports credits; a refresh must not end the pause.
	p.mu.Lock()
	p.doc.Accounts[0].Usage.CheckedAt = time.Time{}
	p.mu.Unlock()
	if _, err := p.Refresh(id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.View().Accounts[0].Refreshing {
		if time.Now().After(deadline) {
			t.Fatal("refresh never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx); err != ErrPoolUnavailable && err != context.DeadlineExceeded {
		t.Fatalf("paused account received work: %v", err)
	}
	p.mu.Lock()
	until := p.runtime[id].cooldown
	p.mu.Unlock()
	if d := time.Until(until); d < 5*time.Hour {
		t.Fatalf("monthly pause too short: %v", d)
	}
	// A plain 402 without the monthly reason keeps the short cooldown.
	p2, id2 := managedTestPool(t, 2)
	testAcquire(t, p2).Release(RequestOutcome{UpstreamStatus: 402, Status: 500})
	p2.mu.Lock()
	short := time.Until(p2.runtime[id2].cooldown)
	p2.mu.Unlock()
	if short > 2*time.Minute {
		t.Fatalf("ordinary 402 got the monthly pause: %v", short)
	}
}

func TestRefreshSpacing(t *testing.T) {
	old := refreshSpacing
	refreshSpacing = 50 * time.Millisecond
	t.Cleanup(func() { refreshSpacing = old })
	var mu sync.Mutex
	var starts []time.Time
	provider := func(ctx context.Context, cfg AuthConfig, prev *types.TokenInfo) (types.TokenInfo, *CreditSnapshot, error) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		return managedTestProvider(ctx, cfg, prev)
	}
	boot := []AuthConfig{}
	for i := 0; i < 6; i++ {
		boot = append(boot, AuthConfig{AuthType: AuthMethodSocial, RefreshToken: fmt.Sprintf("synthetic-spacing-refresh-token-%d", i)})
	}
	p, err := NewAccountPool(t.TempDir(), boot, 2, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(starts)
		mu.Unlock()
		if n == 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d refreshes ran", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < 40*time.Millisecond {
			t.Fatalf("refreshes %d and %d only %v apart", i-1, i, gap)
		}
	}
}

func TestPlanOrderAllocatesTypesInOrder(t *testing.T) {
	old := refreshSpacing
	refreshSpacing = time.Millisecond
	t.Cleanup(func() { refreshSpacing = old })
	plans := map[string]string{"synthetic-plan-power-refresh-token": "KIRO POWER", "synthetic-plan-pro-refresh-token": "KIRO PRO", "synthetic-plan-student-refresh-token": "KIRO STUDENT"}
	provider := func(ctx context.Context, cfg AuthConfig, prev *types.TokenInfo) (types.TokenInfo, *CreditSnapshot, error) {
		token, usage, err := managedTestProvider(ctx, cfg, prev)
		usage.Plan = plans[cfg.RefreshToken]
		return token, usage, err
	}
	boot := []AuthConfig{
		{AuthType: AuthMethodSocial, RefreshToken: "synthetic-plan-power-refresh-token"},
		{AuthType: AuthMethodSocial, RefreshToken: "synthetic-plan-pro-refresh-token"},
		{AuthType: AuthMethodSocial, RefreshToken: "synthetic-plan-student-refresh-token"},
	}
	dir := t.TempDir()
	p, err := NewAccountPool(dir, boot, 1, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ready := 0
		for _, a := range p.View().Accounts {
			if a.Status == "ready" {
				ready++
			}
		}
		if ready == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accounts never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	planOf := map[string]string{}
	for _, a := range p.View().Accounts {
		planOf[a.ID] = a.PlanType
	}
	if planOf[p.View().Accounts[2].ID] != "STUDENT" {
		t.Fatalf("plan type not derived: %v", planOf)
	}
	if err = p.SetPlanOrder([]string{"student", " Pro "}); err != nil {
		t.Fatal(err)
	}
	if err = p.SetPlanOrder([]string{"PRO", "pro"}); err == nil {
		t.Fatal("duplicate type accepted")
	}
	var got []string
	var leases []*AccountLease
	for i := 0; i < 3; i++ {
		l := testAcquire(t, p)
		leases = append(leases, l)
		got = append(got, planOf[l.id])
	}
	if strings.Join(got, ",") != "STUDENT,PRO,POWER" {
		t.Fatalf("allocation order %v", got)
	}
	for _, l := range leases {
		l.Release(RequestOutcome{Success: true})
	}
	if l := testAcquire(t, p); planOf[l.id] != "STUDENT" {
		t.Fatal("released preferred account was not reused first")
	} else {
		l.Release(RequestOutcome{Success: true})
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewAccountPool(dir, nil, 1, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if o := reopened.View().PlanOrder; strings.Join(o, ",") != "STUDENT,PRO" {
		t.Fatalf("plan order not persisted: %v", o)
	}
}

func TestAcquirePreferredStaysThenMoves(t *testing.T) {
	oldRefresh, oldSticky := refreshSpacing, stickyWait
	refreshSpacing, stickyWait = time.Millisecond, 80*time.Millisecond
	t.Cleanup(func() { refreshSpacing, stickyWait = oldRefresh, oldSticky })
	boot := []AuthConfig{
		{AuthType: AuthMethodSocial, RefreshToken: "synthetic-sticky-one-refresh-token"},
		{AuthType: AuthMethodSocial, RefreshToken: "synthetic-sticky-two-refresh-token"},
	}
	p, err := NewAccountPool(t.TempDir(), boot, 1, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	deadline := time.Now().Add(3 * time.Second)
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
	second := p.View().Accounts[1].ID
	acquire := func(pref string) *AccountLease {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		l, err := p.AcquirePreferred(ctx, pref)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}

	// A free preferred account wins over pool order.
	l := acquire(second)
	if l.AccountID() != second {
		t.Fatalf("preferred account skipped: %s", l.AccountID())
	}

	// Busy preferred account: the request waits for it rather than moving.
	got := make(chan string, 1)
	go func() { w := acquire(second); got <- w.AccountID(); w.Release(RequestOutcome{Success: true}) }()
	time.Sleep(20 * time.Millisecond)
	l.Release(RequestOutcome{Success: true})
	if id := <-got; id != second {
		t.Fatalf("request moved while its account was briefly busy: %s", id)
	}

	// Still busy after stickyWait: the request moves to another account.
	l = acquire(second)
	started := time.Now()
	moved := acquire(second)
	if moved.AccountID() == second || time.Since(started) < stickyWait {
		t.Fatalf("moved=%s after %v", moved.AccountID(), time.Since(started))
	}
	moved.Release(RequestOutcome{Success: true})
	l.Release(RequestOutcome{Success: true})

	// Unknown or deleted preference falls back to normal order immediately.
	if l := acquire("no-such-account"); l == nil {
		t.Fatal("no lease")
	} else {
		l.Release(RequestOutcome{Success: true})
	}
}

func readyPool(t *testing.T, n, limit int) *AccountPool {
	t.Helper()
	old := refreshSpacing
	refreshSpacing = time.Millisecond
	t.Cleanup(func() { refreshSpacing = old })
	var boot []AuthConfig
	for i := 0; i < n; i++ {
		boot = append(boot, AuthConfig{AuthType: AuthMethodSocial, RefreshToken: fmt.Sprintf("synthetic-ready-%d-refresh-token", i)})
	}
	p, err := NewAccountPool(t.TempDir(), boot, limit, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitReady(t, p, n)
	return p
}

func waitReady(t *testing.T, p *AccountPool, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ready := 0
		for _, a := range p.View().Accounts {
			if a.Status == "ready" {
				ready++
			}
		}
		if ready == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("accounts never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNewConversationsSpreadAcrossAccounts(t *testing.T) {
	p := readyPool(t, 2, 2)
	first := testAcquire(t, p)
	second := testAcquire(t, p)
	if first.AccountID() == second.AccountID() {
		t.Fatal("new conversation stacked on a busy account while another was idle")
	}
	first.Release(RequestOutcome{Success: true})
	second.Release(RequestOutcome{Success: true})
}

func TestHeldSlotGoesToReturningConversation(t *testing.T) {
	oldSticky := stickyWait
	stickyWait = 2 * time.Second
	t.Cleanup(func() { stickyWait = oldSticky })
	p := readyPool(t, 2, 1)
	a := testAcquire(t, p)
	b := testAcquire(t, p)
	home := a.AccountID()
	acquire := func(pref string, out chan<- *AccountLease) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		l, err := p.AcquirePreferred(ctx, pref)
		if err != nil {
			out <- nil
			return
		}
		out <- l
	}
	returning, fresh := make(chan *AccountLease, 1), make(chan *AccountLease, 1)
	go acquire(home, returning)
	time.Sleep(30 * time.Millisecond) // the returning turn is waiting and holds home's next slot
	go acquire("", fresh)
	time.Sleep(30 * time.Millisecond)
	a.Release(RequestOutcome{Success: true})
	got := <-returning
	if got == nil || got.AccountID() != home {
		t.Fatalf("returning conversation lost its account: %v", got)
	}
	select {
	case l := <-fresh:
		t.Fatalf("new request took a slot while none was free: %s", l.AccountID())
	case <-time.After(50 * time.Millisecond):
	}
	b.Release(RequestOutcome{Success: true})
	if l := <-fresh; l == nil || l.AccountID() == home {
		t.Fatalf("new request placement: %v", l)
	} else {
		l.Release(RequestOutcome{Success: true})
	}
	got.Release(RequestOutcome{Success: true})
	if n := len(p.stickyWaiters); n != 0 {
		t.Fatalf("held slots leaked: %v", p.stickyWaiters)
	}
}

// New conversations must not all start on the same account: among accounts with
// equally many free slots the pool picks at random.
func TestIdlePoolSpreadsNewConversationsAtRandom(t *testing.T) {
	p := readyPool(t, 3, 1)
	seen := map[string]int{}
	for i := 0; i < 60; i++ {
		l := testAcquire(t, p)
		seen[l.AccountID()]++
		l.Release(RequestOutcome{Success: true})
	}
	if len(seen) < 2 {
		t.Fatalf("new conversations stacked on one account: %v", seen)
	}
}

func TestAcquireOtherSkipsFailedAccountAndNeverWaits(t *testing.T) {
	p := readyPool(t, 2, 1)
	first := testAcquire(t, p)
	// A retry leaves the account that just failed and takes the free one.
	other, err := p.AcquireOther(context.Background(), first.AccountID())
	if err != nil {
		t.Fatal(err)
	}
	if other.AccountID() == first.AccountID() {
		t.Fatal("retry returned to the account that failed")
	}
	// Nothing else is free, so a retry gives up at once instead of queueing.
	started := time.Now()
	if _, err := p.AcquireOther(context.Background(), first.AccountID()); err != ErrPoolUnavailable {
		t.Fatalf("err %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("retry waited for a busy pool: %v", elapsed)
	}
	other.Release(RequestOutcome{Success: true})
	first.Release(RequestOutcome{Success: true})

	// A single-account pool has nowhere to move a failed request to.
	single, id := managedTestPool(t, 2)
	waitReady(t, single, 1)
	if _, err := single.AcquireOther(context.Background(), id); err != ErrPoolUnavailable {
		t.Fatalf("single account pool moved a request: %v", err)
	}
}

// AcquireOther must take the account with the most free slots, like a new
// conversation, rather than the first usable one in pool order.
func TestAcquireOtherPrefersTheMostFreeAccount(t *testing.T) {
	p := readyPool(t, 3, 4)
	ids := []string{}
	for _, a := range p.View().Accounts {
		ids = append(ids, a.ID)
	}
	busy := []*AccountLease{}
	for _, want := range []string{ids[0], ids[0], ids[0], ids[2], ids[2]} {
		l, err := p.AcquirePreferred(context.Background(), want)
		if err != nil {
			t.Fatal(err)
		}
		if l.AccountID() != want {
			t.Fatalf("setup placed a request on %s, want %s", l.AccountID(), want)
		}
		busy = append(busy, l)
	}
	// Free slots now: ids[0]=1, ids[1]=4, ids[2]=2. Excluding the roomiest
	// account must fall to the next roomiest, never to pool order.
	l, err := p.AcquireOther(context.Background(), ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if l.AccountID() != ids[2] {
		t.Fatalf("retry picked %s, want the next roomiest %s", l.AccountID(), ids[2])
	}
	l.Release(RequestOutcome{Success: true})
	for _, b := range busy {
		b.Release(RequestOutcome{Success: true})
	}
}
