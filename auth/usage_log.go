package auth

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Per-request usage log. Each finished request appends one JSON line to
// usage/usage-YYYY-MM-DD.jsonl (Beijing date) next to accounts.json. A single
// writer goroutine owns the files, so Release never blocks on disk; when the
// buffer is full the entry is dropped and counted instead.

const (
	usageRetentionDays = 31
	usageBuffer        = 4096
	maxUsagePage       = 200
)

type UsageEntry struct {
	Time      time.Time `json:"time"`
	KeyID     string    `json:"key_id,omitempty"`
	AccountID string    `json:"account_id"`
	Account   string    `json:"account,omitempty"`
	Model     string    `json:"model,omitempty"`
	Stream    bool      `json:"stream,omitempty"`
	Status    int       `json:"status,omitempty"`
	Upstream  int       `json:"upstream_status,omitempty"`
	// ok, failed or canceled.
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
	// Input excludes cache reads; both are the values reported to the client.
	InputTokens     int64 `json:"input_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	// Credits Kiro charged, from meteringEvent; absent when Kiro sent none.
	Credits     *float64 `json:"credits,omitempty"`
	DurationMs  int64    `json:"duration_ms,omitempty"`
	FirstByteMs int64    `json:"first_byte_ms,omitempty"`
	// Affinity: kept or moved for continued conversations, see RequestOutcome.
	Affinity string `json:"affinity,omitempty"`
}

type UsageQuery struct {
	Date      string
	KeyID     string
	AccountID string
	Model     string
	Result    string
	Limit     int
	Offset    int
}

type UsageTotals struct {
	Requests        int64   `json:"requests"`
	Succeeded       int64   `json:"succeeded"`
	Failed          int64   `json:"failed"`
	Canceled        int64   `json:"canceled"`
	InputTokens     int64   `json:"input_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	Credits         float64 `json:"credits"`
	// Requests that carried a credit figure; the rest were not metered by Kiro.
	Metered int64 `json:"metered"`
}

type UsagePage struct {
	Date    string       `json:"date"`
	Entries []UsageEntry `json:"entries"`
	Total   int          `json:"total"`
	Totals  UsageTotals  `json:"totals"`
	// Distinct models and keys of the whole day, for the filter menus.
	Models  []string `json:"models"`
	KeyIDs  []string `json:"key_ids"`
	Dropped int64    `json:"dropped"`
}

func (p *AccountPool) usageDir() string { return filepath.Join(filepath.Dir(p.path), "usage") }

func (p *AccountPool) logUsageLocked(keyID string, a *managedAccount, o RequestOutcome, now time.Time) {
	e := UsageEntry{Time: now.UTC(), KeyID: keyID, AccountID: a.ID, Account: a.Email, Model: o.Model, Stream: o.Stream,
		Status: o.Status, Upstream: o.UpstreamStatus, Error: outcomeClass(o),
		CacheReadTokens: int64(max(o.CacheReadTokens, 0)), OutputTokens: int64(max(o.OutputTokens, 0)),
		DurationMs: o.Duration.Milliseconds(), FirstByteMs: o.FirstByte.Milliseconds(), Affinity: o.Affinity}
	if e.Account == "" {
		e.Account = a.Name
	}
	e.InputTokens = max(int64(o.InputTokens)-e.CacheReadTokens, 0)
	switch {
	case o.Canceled:
		e.Result = "canceled"
	case o.Success:
		e.Result = "ok"
	default:
		e.Result = "failed"
	}
	if o.Metered {
		c := o.Credits
		e.Credits = &c
	}
	select {
	case p.usageCh <- e:
	default:
		p.usageDropped.Add(1)
	}
}

func (p *AccountPool) usageWriter() {
	defer p.wg.Done()
	dir := p.usageDir()
	var f *os.File
	day := ""
	write := func(e UsageEntry) {
		if d := dayKey(e.Time); d != day || f == nil {
			if f != nil {
				_ = f.Close()
				f = nil
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				p.usageDropped.Add(1)
				return
			}
			nf, err := os.OpenFile(filepath.Join(dir, "usage-"+d+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				p.usageDropped.Add(1)
				return
			}
			f, day = nf, d
			pruneUsage(dir, time.Now())
		}
		b, _ := json.Marshal(e)
		if _, err := f.Write(append(b, '\n')); err != nil {
			p.usageDropped.Add(1)
		}
	}
	for {
		select {
		case e := <-p.usageCh:
			write(e)
		case <-p.ctx.Done():
			for {
				select {
				case e := <-p.usageCh:
					write(e)
				default:
					if f != nil {
						_ = f.Close()
					}
					return
				}
			}
		}
	}
}

func pruneUsage(dir string, now time.Time) {
	cutoff := "usage-" + dayKey(now.AddDate(0, 0, -usageRetentionDays)) + ".jsonl"
	names, _ := filepath.Glob(filepath.Join(dir, "usage-*.jsonl"))
	for _, n := range names {
		if filepath.Base(n) < cutoff {
			_ = os.Remove(n)
		}
	}
}

// UsageLogs reads one Beijing day, newest first, filtered and paged.
func (p *AccountPool) UsageLogs(q UsageQuery) (UsagePage, error) {
	if q.Date == "" {
		q.Date = dayKey(time.Now())
	}
	if _, err := time.Parse("2006-01-02", q.Date); err != nil {
		return UsagePage{}, errors.New("日期格式应为 YYYY-MM-DD")
	}
	if q.Limit <= 0 || q.Limit > maxUsagePage {
		q.Limit = 100
	}
	q.Offset = max(q.Offset, 0)
	page := UsagePage{Date: q.Date, Entries: []UsageEntry{}, Models: []string{}, KeyIDs: []string{}, Dropped: p.usageDropped.Load()}
	f, err := os.Open(filepath.Join(p.usageDir(), "usage-"+q.Date+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return page, nil
	}
	if err != nil {
		return page, errors.New("使用日志读取失败")
	}
	defer f.Close()
	models, keys := map[string]bool{}, map[string]bool{}
	var matched []UsageEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e UsageEntry
		// A line still being written is skipped rather than failing the page.
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Model != "" {
			models[e.Model] = true
		}
		if e.KeyID != "" {
			keys[e.KeyID] = true
		}
		if (q.KeyID != "" && e.KeyID != q.KeyID) || (q.AccountID != "" && e.AccountID != q.AccountID) ||
			(q.Model != "" && e.Model != q.Model) || (q.Result != "" && e.Result != q.Result) {
			continue
		}
		t := &page.Totals
		t.Requests++
		switch e.Result {
		case "ok":
			t.Succeeded++
		case "canceled":
			t.Canceled++
		default:
			t.Failed++
		}
		t.InputTokens += e.InputTokens
		t.CacheReadTokens += e.CacheReadTokens
		t.OutputTokens += e.OutputTokens
		if e.Credits != nil {
			t.Credits += *e.Credits
			t.Metered++
		}
		matched = append(matched, e)
	}
	for m := range models {
		page.Models = append(page.Models, m)
	}
	for k := range keys {
		page.KeyIDs = append(page.KeyIDs, k)
	}
	sort.Strings(page.Models)
	sort.Strings(page.KeyIDs)
	page.Total = len(matched)
	for i := len(matched) - 1 - q.Offset; i >= 0 && len(page.Entries) < q.Limit; i-- {
		page.Entries = append(page.Entries, matched[i])
	}
	return page, nil
}

// UsageDates lists the days that have a log file, newest first.
func (p *AccountPool) UsageDates() []string {
	names, _ := filepath.Glob(filepath.Join(p.usageDir(), "usage-*.jsonl"))
	out := []string{}
	for _, n := range names {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(n), "usage-"), ".jsonl"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}
