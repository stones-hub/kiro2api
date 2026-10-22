package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLatencyHistogramQuantiles(t *testing.T) {
	var h latencyHistogram
	if h.quantile(.5) != 0 {
		t.Fatal("empty histogram must report zero")
	}
	for i := 0; i < 90; i++ {
		h.add(400 * time.Millisecond) // (250, 500]
	}
	for i := 0; i < 10; i++ {
		h.add(10 * time.Second) // (8000, 12000]
	}
	if p50 := h.quantile(.5); p50 <= 250 || p50 > 500 {
		t.Fatalf("p50 = %d", p50)
	}
	if p95 := h.quantile(.95); p95 <= 8000 || p95 > 12000 {
		t.Fatalf("p95 = %d", p95)
	}
	var slow latencyHistogram
	slow.add(time.Hour)
	if slow.quantile(.99) != latencyBoundsMs[len(latencyBoundsMs)-1] {
		t.Fatal("overflow bucket must report the last bound")
	}
}

func TestOutcomeClass(t *testing.T) {
	cases := map[string]RequestOutcome{
		"":              {Success: true, UpstreamStatus: 200},
		"empty":         {Success: true, Empty: true, UpstreamStatus: 200},
		"canceled":      {Canceled: true, UpstreamStatus: 429},
		"upstream_429":  {UpstreamStatus: 429},
		"upstream_402":  {UpstreamStatus: 402},
		"upstream_auth": {UpstreamStatus: 403},
		"upstream_5xx":  {UpstreamStatus: 503},
		"upstream_4xx":  {UpstreamStatus: 400},
		"stream":        {UpstreamStatus: 200, Status: 200},
		"request":       {Status: 400},
		"network":       {Status: 500},
	}
	for want, o := range cases {
		if got := outcomeClass(o); got != want {
			t.Errorf("%+v: got %q want %q", o, got, want)
		}
	}
}

func TestMetricsRecordAndPersist(t *testing.T) {
	dir := t.TempDir()
	p, err := NewAccountPool(dir, []AuthConfig{{AuthType: AuthMethodSocial, RefreshToken: "synthetic-private-refresh-token"}}, 2, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	testAcquire(t, p).Release(RequestOutcome{Success: true, Model: "claude-opus-5-5", UpstreamStatus: 200, Duration: 3 * time.Second, FirstByte: 800 * time.Millisecond})
	testAcquire(t, p).Release(RequestOutcome{Success: true, Empty: true, Model: "claude-opus-5-5", UpstreamStatus: 200, Duration: time.Second})
	testAcquire(t, p).Release(RequestOutcome{Model: "claude-sonnet-4-5", UpstreamStatus: 429, Duration: 50 * time.Millisecond})
	p.recordRejection("pool_busy", "")

	m := p.Metrics()
	now := m.Hours[len(m.Hours)-1]
	if len(m.Hours) != 24 || len(m.Days) != metricDayRetention {
		t.Fatalf("series lengths %d/%d", len(m.Hours), len(m.Days))
	}
	if now.Requests != 3 || now.Succeeded != 2 || now.Failed != 1 || now.Empty != 1 || now.Rejected != 1 {
		t.Fatalf("hour bucket %+v", now)
	}
	if today := m.Days[len(m.Days)-1]; today.Requests != 3 || today.Failed != 1 {
		t.Fatalf("day bucket %+v", today)
	}
	errs := map[string]int64{}
	for _, e := range m.Errors24h {
		errs[e.Name] = e.Count
	}
	if errs["upstream_429"] != 1 || errs["empty"] != 1 || errs["pool_busy"] != 1 {
		t.Fatalf("errors %+v", m.Errors24h)
	}
	if len(m.Models24h) != 2 || m.Models24h[0].Name != "claude-opus-5-5" || m.Models24h[0].Count != 2 {
		t.Fatalf("models %+v", m.Models24h)
	}
	// The failed 429 must not pull latency down.
	if m.Latency24h.Samples != 2 || m.Latency24h.FirstByteSamples != 1 || m.Latency24h.DurationP50 < 500 {
		t.Fatalf("latency %+v", m.Latency24h)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc managedDocument
	if err := json.Unmarshal(b, &doc); err != nil || doc.Metrics == nil || len(doc.Metrics.Hours) != 1 {
		t.Fatalf("metrics not persisted: %v", err)
	}
	reopened, err := NewAccountPool(dir, nil, 2, managedTestProvider)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Metrics().Hours[23]; got.Requests != 3 || got.Rejected != 1 {
		t.Fatalf("metrics lost across restart: %+v", got)
	}
}

func TestMetricsPruneAndModelCap(t *testing.T) {
	p, _ := managedTestPool(t, 2)
	now := time.Now()
	p.mu.Lock()
	m := p.metricsLocked(now)
	m.Hours[hourKey(now.Add(-72*time.Hour))] = &metricBucket{Requests: 9}
	m.Days[dayKey(now.AddDate(0, 0, -40))] = &metricBucket{Requests: 9}
	p.mu.Unlock()

	for i := 0; i < maxBucketModels+5; i++ {
		p.mu.Lock()
		p.recordOutcomeLocked(RequestOutcome{Success: true, Model: "model-" + string(rune('A'+i))}, "", now)
		p.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(m.Hours) != 1 || len(m.Days) != 1 {
		t.Fatalf("expired buckets kept: %d hours, %d days", len(m.Hours), len(m.Days))
	}
	h := m.Hours[hourKey(now)]
	if len(h.Models) != maxBucketModels || h.Models["other"] == 0 {
		t.Fatalf("model map not capped: %d", len(h.Models))
	}
}

// A hard-deleted account drops its Daily stats; the day bucket must still count its requests.
func TestDaySeriesKeepsBucketWhenDailyShrinks(t *testing.T) {
	p, _ := managedTestPool(t, 2)
	now := time.Now()
	p.mu.Lock()
	p.metricsLocked(now).Days[dayKey(now)] = &metricBucket{Requests: 5, Succeeded: 4, Failed: 1}
	p.mu.Unlock()
	if today := p.Metrics().Days[metricDayRetention-1]; today.Requests != 5 || today.Failed != 1 || today.Tracked != 5 {
		t.Fatalf("day point %+v", today)
	}
}

func TestCreditsTrackUsageGrowthOnly(t *testing.T) {
	p, _ := managedTestPool(t, 2)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordCreditsLocked(&CreditSnapshot{Used: 100, Limit: 2000}, &CreditSnapshot{Used: 130, Limit: 2000}, now)
	p.recordCreditsLocked(&CreditSnapshot{Used: 130, Limit: 2000}, &CreditSnapshot{Used: 0, Limit: 2000}, now) // reset
	p.recordCreditsLocked(nil, &CreditSnapshot{Used: 500, Limit: 2000}, now)
	if got := p.doc.Metrics.Hours[hourKey(now)].Credits; got != 30 {
		t.Fatalf("credits = %v", got)
	}
}

func TestForecast(t *testing.T) {
	p, _ := managedTestPool(t, 2)
	testAcquire(t, p).Release(RequestOutcome{Success: true})
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	a := &p.doc.Accounts[0]
	a.Usage = &CreditSnapshot{Limit: 2000, Used: 100, Remaining: 1900, CheckedAt: now}

	f := p.forecastLocked(now, 0, 10)
	if f.ExhaustAt != nil || f.Remaining != 1900 || f.Accounts != 1 {
		t.Fatalf("no burn must give no exhaustion estimate: %+v", f)
	}
	if f = p.forecastLocked(now, 100, 0.5); f.ExhaustAt != nil {
		t.Fatal("under an hour of history must not forecast")
	}

	f = p.forecastLocked(now, 100, 10) // 10 credits/hour
	if f.BurnPerHour != 10 || f.ExhaustAt == nil || absHours(f.ExhaustAt.Sub(now)-190*time.Hour) > 0.01 {
		t.Fatalf("plain forecast %+v", f)
	}

	reset := now.Add(24 * time.Hour)
	a.Usage.ResetAt = &reset
	f = p.forecastLocked(now, 100, 10)
	// 1900 - 240 burned before the reset, +100 restored, then 1760 / 10 per hour.
	if f.NextResetAt == nil || f.NextResetCredits != 100 || absHours(f.ExhaustAt.Sub(reset)-176*time.Hour) > 0.01 {
		t.Fatalf("reset forecast %+v", f)
	}

	f = p.forecastLocked(now, 24*1000, 24) // 1000/hour exhausts in 1.9h, before the reset
	if absHours(f.ExhaustAt.Sub(now)-114*time.Minute) > 0.01 {
		t.Fatalf("early exhaustion %+v", f)
	}
}

func TestLiveSamplesRing(t *testing.T) {
	p, _ := managedTestPool(t, 2)
	l := testAcquire(t, p)
	defer l.Release(RequestOutcome{Success: true})
	p.mu.Lock()
	start := time.Now()
	for i := 0; i < liveSampleCapacity+3; i++ {
		p.sampleLocked(start.Add(time.Duration(i) * time.Second))
	}
	p.mu.Unlock()
	live := p.Metrics().Live
	if len(live) != liveSampleCapacity {
		t.Fatalf("ring length %d", len(live))
	}
	for i := 1; i < len(live); i++ {
		if !live[i].At.After(live[i-1].At) {
			t.Fatal("samples out of order")
		}
	}
	if last := live[len(live)-1]; last.Active != 1 || last.Capacity != 2 {
		t.Fatalf("sample %+v", last)
	}
}

func absHours(d time.Duration) float64 {
	if d < 0 {
		d = -d
	}
	return d.Hours()
}
