package auth

import (
	"sort"
	"time"
)

// Pool-wide observability. Everything here is aggregated in fixed-size buckets so
// memory and accounts.json stay bounded: no per-request samples are retained.

// Upper bounds (ms) of the latency histogram; one extra slot counts anything slower.
var latencyBoundsMs = [...]int64{100, 250, 500, 1000, 2000, 3000, 5000, 8000, 12000, 20000, 30000, 60000, 120000, 300000}

const (
	metricHourRetention = 48 * time.Hour
	metricDayRetention  = 31
	maxBucketModels     = 64
	maxReportedModels   = 10
	liveSampleCapacity  = 720 // one hour at the 5 second maintenance tick
)

type latencyHistogram struct {
	Counts []int64 `json:"counts,omitempty"`
}

func (h *latencyHistogram) add(d time.Duration) {
	if len(h.Counts) < len(latencyBoundsMs)+1 {
		h.Counts = append(h.Counts, make([]int64, len(latencyBoundsMs)+1-len(h.Counts))...)
	}
	ms := d.Milliseconds()
	h.Counts[sort.Search(len(latencyBoundsMs), func(i int) bool { return ms <= latencyBoundsMs[i] })]++
}

func (h *latencyHistogram) merge(o latencyHistogram) {
	for i, c := range o.Counts {
		if i >= len(latencyBoundsMs)+1 {
			break
		}
		if c > 0 {
			if len(h.Counts) < len(latencyBoundsMs)+1 {
				h.Counts = append(h.Counts, make([]int64, len(latencyBoundsMs)+1-len(h.Counts))...)
			}
			h.Counts[i] += c
		}
	}
}

func (h latencyHistogram) total() int64 {
	var n int64
	for _, c := range h.Counts {
		n += c
	}
	return n
}

// quantile interpolates linearly inside the bucket holding the requested rank.
func (h latencyHistogram) quantile(q float64) int64 {
	n := h.total()
	if n == 0 {
		return 0
	}
	rank := q * float64(n)
	seen := 0.0
	for i, c := range h.Counts {
		if c == 0 {
			continue
		}
		if seen+float64(c) >= rank {
			lo := int64(0)
			if i > 0 {
				lo = latencyBoundsMs[i-1]
			}
			if i >= len(latencyBoundsMs) {
				return lo
			}
			frac := (rank - seen) / float64(c)
			return lo + int64(frac*float64(latencyBoundsMs[i]-lo))
		}
		seen += float64(c)
	}
	return latencyBoundsMs[len(latencyBoundsMs)-1]
}

type metricBucket struct {
	Requests  int64   `json:"requests"`
	Succeeded int64   `json:"succeeded"`
	Failed    int64   `json:"failed"`
	Canceled  int64   `json:"canceled"`
	Rejected  int64   `json:"rejected,omitempty"`
	Empty     int64   `json:"empty,omitempty"`
	Credits   float64 `json:"credits,omitempty"`
	// Credits reported by Kiro's meteringEvent for requests in this bucket.
	MeteredCredits float64 `json:"metered_credits,omitempty"`
	// Token totals; older buckets lack them.
	InputTokens  int64            `json:"input_tokens,omitempty"`
	OutputTokens int64            `json:"output_tokens,omitempty"`
	Errors       map[string]int64 `json:"errors,omitempty"`
	Models       map[string]int64 `json:"models,omitempty"`
	FirstByte    latencyHistogram `json:"first_byte"`
	Duration     latencyHistogram `json:"duration"`
}

type poolMetrics struct {
	Since time.Time                `json:"since"`
	Hours map[string]*metricBucket `json:"hours"`
	Days  map[string]*metricBucket `json:"days"`
	// The same series per client API key, keyed by key ID. Credits are only
	// measured per account, so key buckets never carry them.
	Keys map[string]*keyMetrics `json:"keys,omitempty"`
}

type keyMetrics struct {
	Hours map[string]*metricBucket `json:"hours"`
	Days  map[string]*metricBucket `json:"days"`
}

type LiveSample struct {
	At       time.Time `json:"at"`
	Active   int       `json:"active"`
	Waiting  int       `json:"waiting"`
	Capacity int       `json:"capacity"`
}

func hourKey(t time.Time) string { return t.In(accountTimezone).Format("2006-01-02T15") }
func dayKey(t time.Time) string  { return t.In(accountTimezone).Format("2006-01-02") }

func (p *AccountPool) metricsLocked(now time.Time) *poolMetrics {
	m := p.doc.Metrics
	if m == nil {
		m = &poolMetrics{Since: now.UTC()}
		p.doc.Metrics = m
	}
	if m.Hours == nil {
		m.Hours = map[string]*metricBucket{}
	}
	if m.Days == nil {
		m.Days = map[string]*metricBucket{}
	}
	return m
}

// bucketsLocked returns the current hour and day buckets, pruning expired ones
// whenever a new hour starts.
func (p *AccountPool) bucketsLocked(now time.Time) (*metricBucket, *metricBucket) {
	m := p.metricsLocked(now)
	hk, dk := hourKey(now), dayKey(now)
	h := m.Hours[hk]
	if h == nil {
		h = &metricBucket{}
		m.Hours[hk] = h
		cutoff := hourKey(now.Add(-metricHourRetention))
		for k := range m.Hours {
			if k < cutoff {
				delete(m.Hours, k)
			}
		}
		dayCutoff := dayKey(now.AddDate(0, 0, -metricDayRetention))
		for k := range m.Days {
			if k < dayCutoff {
				delete(m.Days, k)
			}
		}
		for id, km := range m.Keys {
			for k := range km.Hours {
				if k < cutoff {
					delete(km.Hours, k)
				}
			}
			for k := range km.Days {
				if k < dayCutoff {
					delete(km.Days, k)
				}
			}
			if (len(km.Hours) == 0 && len(km.Days) == 0) || !p.knownKeyLocked(id) {
				delete(m.Keys, id)
			}
		}
	}
	d := m.Days[dk]
	if d == nil {
		d = &metricBucket{}
		m.Days[dk] = d
	}
	p.dirty = true
	return h, d
}

// targetBucketsLocked returns the pool's hour and day buckets, plus the key's
// when the request came with a known client key.
func (p *AccountPool) targetBucketsLocked(keyID string, now time.Time) []*metricBucket {
	h, d := p.bucketsLocked(now)
	out := []*metricBucket{h, d}
	if keyID == "" || !p.knownKeyLocked(keyID) {
		return out
	}
	m := p.doc.Metrics
	if m.Keys == nil {
		m.Keys = map[string]*keyMetrics{}
	}
	km := m.Keys[keyID]
	if km == nil {
		km = &keyMetrics{}
		m.Keys[keyID] = km
	}
	if km.Hours == nil {
		km.Hours = map[string]*metricBucket{}
	}
	if km.Days == nil {
		km.Days = map[string]*metricBucket{}
	}
	for _, slot := range []struct {
		series map[string]*metricBucket
		key    string
	}{{km.Hours, hourKey(now)}, {km.Days, dayKey(now)}} {
		b := slot.series[slot.key]
		if b == nil {
			b = &metricBucket{}
			slot.series[slot.key] = b
		}
		out = append(out, b)
	}
	p.touchKeyLocked(keyID, now)
	return out
}

func (b *metricBucket) addError(class string) {
	if b.Errors == nil {
		b.Errors = map[string]int64{}
	}
	b.Errors[class]++
}

func (b *metricBucket) addModel(model string) {
	if b.Models == nil {
		b.Models = map[string]int64{}
	}
	// One slot stays reserved for "other" so the map never exceeds the cap.
	if _, ok := b.Models[model]; !ok && len(b.Models) >= maxBucketModels-1 {
		model = "other"
	}
	b.Models[model]++
}

// outcomeClass buckets a finished request for the error breakdown; "" means a clean success.
func outcomeClass(o RequestOutcome) string {
	switch {
	case o.Canceled:
		return "canceled"
	case o.Success && o.Empty:
		return "empty"
	case o.Success:
		return ""
	case o.ProxyError:
		return "proxy"
	}
	switch s := o.UpstreamStatus; {
	case s == 429:
		return "upstream_429"
	case s == 402:
		return "upstream_402"
	case s == 401 || s == 403:
		return "upstream_auth"
	case s >= 500:
		return "upstream_5xx"
	case s >= 400:
		return "upstream_4xx"
	case s > 0:
		return "stream"
	}
	if o.Status >= 400 && o.Status < 500 {
		return "request"
	}
	return "network"
}

func (p *AccountPool) recordOutcomeLocked(o RequestOutcome, keyID string, now time.Time) {
	class := outcomeClass(o)
	for _, b := range p.targetBucketsLocked(keyID, now) {
		b.Requests++
		if o.Metered {
			b.MeteredCredits += o.Credits
		}
		b.InputTokens += int64(max(o.InputTokens, 0))
		b.OutputTokens += int64(max(o.OutputTokens, 0))
		switch {
		case o.Canceled:
			b.Canceled++
		case o.Success:
			b.Succeeded++
		default:
			b.Failed++
		}
		if o.Success && o.Empty {
			b.Empty++
		}
		if class != "" {
			b.addError(class)
		}
		if o.Model != "" {
			b.addModel(o.Model)
		}
		// Latency only describes successful requests; quick 429s would hide slowness.
		if o.Success && !o.Canceled {
			if o.Duration > 0 {
				b.Duration.add(o.Duration)
			}
			if o.FirstByte > 0 {
				b.FirstByte.add(o.FirstByte)
			}
		}
	}
}

// recordRejectionLocked counts requests the pool refused before any account was leased.
func (p *AccountPool) recordRejectionLocked(class, keyID string, now time.Time) {
	for _, b := range p.targetBucketsLocked(keyID, now) {
		b.Rejected++
		b.addError(class)
	}
}

func (p *AccountPool) recordRejection(class, keyID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.recordRejectionLocked(class, keyID, time.Now())
	}
}

// recordCreditsLocked attributes the growth of an account's official usage to the
// current hour. A drop means the quota was reset and is not consumption.
func (p *AccountPool) recordCreditsLocked(old, next *CreditSnapshot, now time.Time) {
	if old == nil || next == nil {
		return
	}
	delta := next.Used - old.Used
	if delta <= 0 || (next.Limit > 0 && delta > next.Limit) {
		return
	}
	h, d := p.bucketsLocked(now)
	h.Credits += delta
	d.Credits += delta
}

func (p *AccountPool) sampleLocked(now time.Time) {
	s := LiveSample{At: now.UTC(), Waiting: p.waiting}
	for _, a := range p.doc.Accounts {
		rt := p.runtime[a.ID]
		s.Active += rt.active
		if a.DeletedAt == nil && !a.Credentials.Disabled && usableLocked(&a, rt, now) {
			s.Capacity += a.Concurrency
		}
	}
	p.live[p.liveNext] = s
	p.liveNext = (p.liveNext + 1) % liveSampleCapacity
	p.liveLen = min(p.liveLen+1, liveSampleCapacity)
}

// SeriesPoint is one hour or day of the series. Tracked counts the requests covered
// by the error/model buckets; day totals come from per-account stats and may predate them.
type SeriesPoint struct {
	Key       string  `json:"key"`
	Requests  int64   `json:"requests"`
	Tracked   int64   `json:"tracked"`
	Succeeded int64   `json:"succeeded"`
	Failed    int64   `json:"failed"`
	Canceled  int64   `json:"canceled"`
	Rejected  int64   `json:"rejected"`
	Empty     int64   `json:"empty"`
	Credits   float64 `json:"credits"`
	// Exact per-request credits from Kiro; for a key view this is the key's own spend.
	MeteredCredits float64 `json:"metered_credits"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	FirstByteP50   int64   `json:"first_byte_p50_ms"`
	FirstByteP95   int64   `json:"first_byte_p95_ms"`
	DurationP50    int64   `json:"duration_p50_ms"`
	DurationP95    int64   `json:"duration_p95_ms"`
}

type NamedCount struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type LatencySummary struct {
	Samples          int64 `json:"samples"`
	FirstByteSamples int64 `json:"first_byte_samples"`
	FirstByteP50     int64 `json:"first_byte_p50_ms"`
	FirstByteP95     int64 `json:"first_byte_p95_ms"`
	DurationP50      int64 `json:"duration_p50_ms"`
	DurationP95      int64 `json:"duration_p95_ms"`
}

type Forecast struct {
	Accounts         int        `json:"accounts"`
	Remaining        float64    `json:"remaining"`
	Limit            float64    `json:"limit"`
	BurnPerHour      float64    `json:"burn_per_hour"`
	WindowHours      float64    `json:"window_hours"`
	ExhaustAt        *time.Time `json:"exhaust_at,omitempty"`
	NextResetAt      *time.Time `json:"next_reset_at,omitempty"`
	NextResetCredits float64    `json:"next_reset_credits"`
}

type MetricsView struct {
	// KeyID is set when the series cover one client key. Credits, the forecast
	// and live samples always describe the whole pool.
	KeyID       string         `json:"key_id,omitempty"`
	GeneratedAt time.Time      `json:"generated_at"`
	Since       time.Time      `json:"since"`
	Hours       []SeriesPoint  `json:"hours"`
	Days        []SeriesPoint  `json:"days"`
	Errors24h   []NamedCount   `json:"errors_24h"`
	ErrorsDays  []NamedCount   `json:"errors_days"`
	Models24h   []NamedCount   `json:"models_24h"`
	ModelsDays  []NamedCount   `json:"models_days"`
	Latency24h  LatencySummary `json:"latency_24h"`
	Live        []LiveSample   `json:"live"`
	Forecast    Forecast       `json:"forecast"`
}

func (s *SeriesPoint) fill(b *metricBucket) {
	s.Requests, s.Succeeded, s.Failed, s.Canceled = b.Requests, b.Succeeded, b.Failed, b.Canceled
	s.Tracked = b.Requests
	s.Rejected, s.Empty, s.Credits = b.Rejected, b.Empty, b.Credits
	s.InputTokens, s.OutputTokens = b.InputTokens, b.OutputTokens
	s.MeteredCredits = b.MeteredCredits
	s.FirstByteP50, s.FirstByteP95 = b.FirstByte.quantile(.5), b.FirstByte.quantile(.95)
	s.DurationP50, s.DurationP95 = b.Duration.quantile(.5), b.Duration.quantile(.95)
}

func mergeCounts(dst, src map[string]int64) {
	for k, v := range src {
		dst[k] += v
	}
}

func rankCounts(m map[string]int64, limit int) []NamedCount {
	out := make([]NamedCount, 0, len(m))
	for k, v := range m {
		if v > 0 {
			out = append(out, NamedCount{Name: k, Count: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		var rest int64
		for _, n := range out[limit:] {
			rest += n.Count
		}
		out = append(out[:limit], NamedCount{Name: "other", Count: rest})
	}
	return out
}

func (p *AccountPool) Metrics() MetricsView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.metricsViewLocked("")
}

// MetricsForKey limits the request, error, model and latency series to one client key.
func (p *AccountPool) MetricsForKey(keyID string) (MetricsView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.knownKeyLocked(keyID) {
		return MetricsView{}, ErrKeyMissing
	}
	return p.metricsViewLocked(keyID), nil
}

func (p *AccountPool) metricsViewLocked(keyID string) MetricsView {
	now := time.Now()
	m := p.doc.Metrics
	if m == nil {
		m = &poolMetrics{Since: now.UTC()}
	}
	v := MetricsView{KeyID: keyID, GeneratedAt: now.UTC(), Since: m.Since, Hours: []SeriesPoint{}, Days: []SeriesPoint{}, Live: []LiveSample{}}
	hours, days := m.Hours, m.Days
	if keyID != "" {
		hours, days = nil, nil
		if km := m.Keys[keyID]; km != nil {
			hours, days = km.Hours, km.Days
		}
	}

	errors24, models24 := map[string]int64{}, map[string]int64{}
	var firstByte, duration latencyHistogram
	var burned float64
	for i := 23; i >= 0; i-- {
		pt := SeriesPoint{Key: hourKey(now.Add(-time.Duration(i) * time.Hour))}
		if b := hours[pt.Key]; b != nil {
			pt.fill(b)
			mergeCounts(errors24, b.Errors)
			mergeCounts(models24, b.Models)
			firstByte.merge(b.FirstByte)
			duration.merge(b.Duration)
		}
		if b := m.Hours[pt.Key]; b != nil {
			pt.Credits = b.Credits
			burned += b.Credits
		}
		v.Hours = append(v.Hours, pt)
	}

	// Per-account daily stats predate these buckets, so they stay authoritative for counts.
	daily := map[string]RequestStats{}
	for _, a := range p.doc.Accounts {
		for k, s := range a.Daily {
			d := daily[k]
			d.Requests += s.Requests
			d.Succeeded += s.Succeeded
			d.Failed += s.Failed
			d.Canceled += s.Canceled
			daily[k] = d
		}
	}
	errorsDays, modelsDays := map[string]int64{}, map[string]int64{}
	local := now.In(accountTimezone)
	for i := metricDayRetention - 1; i >= 0; i-- {
		pt := SeriesPoint{Key: dayKey(local.AddDate(0, 0, -i))}
		if b := days[pt.Key]; b != nil {
			pt.fill(b)
			mergeCounts(errorsDays, b.Errors)
			mergeCounts(modelsDays, b.Models)
		}
		if b := m.Days[pt.Key]; b != nil {
			pt.Credits = b.Credits
		}
		// Hard-deleted accounts take their Daily stats with them; keep the bucket then.
		// Per-account stats cannot be split by key, so a key view uses its own buckets.
		if s := daily[pt.Key]; keyID == "" && s.Requests >= pt.Requests {
			pt.Requests, pt.Succeeded, pt.Failed, pt.Canceled = s.Requests, s.Succeeded, s.Failed, s.Canceled
		}
		v.Days = append(v.Days, pt)
	}
	v.Errors24h, v.ErrorsDays = rankCounts(errors24, 0), rankCounts(errorsDays, 0)
	v.Models24h, v.ModelsDays = rankCounts(models24, maxReportedModels), rankCounts(modelsDays, maxReportedModels)
	v.Latency24h = LatencySummary{
		Samples: duration.total(), FirstByteSamples: firstByte.total(),
		FirstByteP50: firstByte.quantile(.5), FirstByteP95: firstByte.quantile(.95),
		DurationP50: duration.quantile(.5), DurationP95: duration.quantile(.95),
	}

	start := (p.liveNext - p.liveLen + liveSampleCapacity) % liveSampleCapacity
	for i := 0; i < p.liveLen; i++ {
		v.Live = append(v.Live, p.live[(start+i)%liveSampleCapacity])
	}

	v.Forecast = p.forecastLocked(now, burned, now.Sub(m.Since).Hours())
	return v
}

// forecastLocked projects when the pool's official credits run out at the recent
// burn rate. A reset is assumed to restore at least the credits already used, which
// makes the estimate conservative (it may predict exhaustion slightly early).
func (p *AccountPool) forecastLocked(now time.Time, burned, observedHours float64) Forecast {
	type reset struct {
		at      time.Time
		credits float64
	}
	f := Forecast{WindowHours: min(max(observedHours, 0), 24)}
	var resets []reset
	for _, a := range p.doc.Accounts {
		if a.DeletedAt != nil || a.Credentials.Disabled || a.Usage == nil {
			continue
		}
		f.Accounts++
		f.Remaining += max(a.Usage.Remaining, 0)
		f.Limit += a.Usage.Limit
		if a.Usage.ResetAt != nil && a.Usage.ResetAt.After(now) {
			resets = append(resets, reset{*a.Usage.ResetAt, max(a.Usage.Limit-max(a.Usage.Remaining, 0), 0)})
		}
	}
	sort.Slice(resets, func(i, j int) bool { return resets[i].at.Before(resets[j].at) })
	if len(resets) > 0 {
		at := resets[0].at.UTC()
		f.NextResetAt = &at
		for _, r := range resets {
			if r.at.Sub(resets[0].at) < time.Hour {
				f.NextResetCredits += r.credits
			}
		}
	}
	// Less than an hour of history gives a meaningless rate.
	if f.WindowHours < 1 || burned <= 0 {
		return f
	}
	f.BurnPerHour = burned / f.WindowHours
	t, left := now, f.Remaining
	for _, r := range resets {
		if hours := left / f.BurnPerHour; t.Add(time.Duration(hours * float64(time.Hour))).Before(r.at) {
			break
		}
		left += r.credits - f.BurnPerHour*r.at.Sub(t).Hours()
		t = r.at
	}
	exhaust := t.Add(time.Duration(left / f.BurnPerHour * float64(time.Hour))).UTC()
	f.ExhaustAt = &exhaust
	return f
}
