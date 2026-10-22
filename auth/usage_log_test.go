package auth

import (
	"context"
	"testing"
	"time"
)

func TestUsageLogRecordsAndFilters(t *testing.T) {
	p, id := managedTestPool(t, 3)
	p.SetBuiltinKey("synthetic-builtin-client-key-value")
	kid, _, err := p.CreateAPIKey("team")
	if err != nil {
		t.Fatal(err)
	}
	run := func(key string, o RequestOutcome) {
		ctx, cancel := context.WithTimeout(WithAPIKeyID(context.Background(), key), time.Second)
		defer cancel()
		l, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		l.Release(o)
	}
	run(kid, RequestOutcome{Success: true, Model: "m1", InputTokens: 1000, CacheReadTokens: 400, OutputTokens: 9, Credits: 0.25, Metered: true, Stream: true})
	run(BuiltinKeyID, RequestOutcome{Success: true, Model: "m2", InputTokens: 50, OutputTokens: 2})
	run(kid, RequestOutcome{Canceled: true, Model: "m1", Credits: 0.05, Metered: true})
	var page UsagePage
	deadline := time.Now().Add(2 * time.Second)
	for {
		if page, err = p.UsageLogs(UsageQuery{}); err != nil {
			t.Fatal(err)
		}
		if page.Total == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if page.Total != 3 || page.Entries[0].Result != "canceled" || page.Entries[2].AccountID != id {
		t.Fatalf("page %+v", page)
	}
	first := page.Entries[2]
	if first.InputTokens != 600 || first.CacheReadTokens != 400 || first.Credits == nil || *first.Credits != 0.25 || !first.Stream {
		t.Fatalf("entry %+v", first)
	}
	if page.Entries[1].Credits != nil {
		t.Fatal("unmetered request got credits")
	}
	team, _ := p.UsageLogs(UsageQuery{KeyID: kid})
	if team.Total != 2 || team.Totals.Credits < 0.2999 || team.Totals.Credits > 0.3001 || team.Totals.Metered != 2 || team.Totals.Canceled != 1 {
		t.Fatalf("key totals %+v", team.Totals)
	}
	if m := p.Metrics().Hours[23]; m.MeteredCredits < 0.2999 {
		t.Fatalf("metered credits not in metrics: %v", m.MeteredCredits)
	}
	if lim, _ := p.UsageLogs(UsageQuery{Limit: 1, Offset: 1}); len(lim.Entries) != 1 || lim.Entries[0].Model != "m2" {
		t.Fatal("paging wrong")
	}
	if _, err = p.UsageLogs(UsageQuery{Date: "../x"}); err == nil {
		t.Fatal("bad date accepted")
	}
	if d := p.UsageDates(); len(d) != 1 {
		t.Fatalf("dates %v", d)
	}
}
