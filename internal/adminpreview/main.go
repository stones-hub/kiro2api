// Temporary synthetic preview of the admin page; not part of the release.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"kiro2api/auth"
	"kiro2api/config"
	"kiro2api/server"
	"kiro2api/types"
)

func hist(r *rand.Rand, n int, center int) []int64 {
	c := make([]int64, 15)
	for i := 0; i < n; i++ {
		j := center + r.Intn(3) - 1
		if r.Intn(12) == 0 {
			j += 3
		}
		c[min(max(j, 0), 14)]++
	}
	return c
}

func bucket(r *rand.Rand, n int) map[string]any {
	failed, canceled := n/25+r.Intn(3), r.Intn(3)
	return map[string]any{
		"requests": n, "succeeded": n - failed - canceled, "failed": failed, "canceled": canceled,
		"rejected": r.Intn(2), "empty": r.Intn(2), "credits": float64(n) * (0.6 + r.Float64()*0.4),
		"errors":     map[string]int{"upstream_429": failed / 2, "stream": failed - failed/2, "canceled": canceled, "empty": 1},
		"models":     map[string]int{"claude-opus-5-5": n * 6 / 10, "claude-sonnet-4-5": n * 3 / 10, "claude-haiku-4-5": n - n*6/10 - n*3/10},
		"first_byte": map[string]any{"counts": hist(r, n, 4)}, "duration": map[string]any{"counts": hist(r, n, 7)},
	}
}

func main() {
	if len(os.Args) > 1 {
		if err := os.Chdir(os.Args[1]); err != nil {
			panic(err)
		}
	}
	dir, err := os.MkdirTemp("", "kiro-admin-preview-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	r := rand.New(rand.NewSource(7))
	tz := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Now()
	hours, days := map[string]any{}, map[string]any{}
	for i := 0; i < 30; i++ {
		t := now.Add(-time.Duration(i) * time.Hour)
		if i == 5 {
			continue // a quiet hour leaves a gap in the latency lines
		}
		hours[t.In(tz).Format("2006-01-02T15")] = bucket(r, 20+r.Intn(60)+(t.In(tz).Hour()%12)*4)
	}
	for i := 0; i < 20; i++ {
		days[now.AddDate(0, 0, -i).In(tz).Format("2006-01-02")] = bucket(r, 600+r.Intn(900))
	}
	doc := map[string]any{"version": 1, "started_at": now.AddDate(0, 0, -20).UTC(), "accounts": []any{},
		"metrics": map[string]any{"since": now.Add(-30 * time.Hour).UTC(), "hours": hours, "days": days}}
	b, _ := json.Marshal(doc)
	if err = os.WriteFile(filepath.Join(dir, "accounts.json"), b, 0600); err != nil {
		panic(err)
	}
	reset := now.Add(9 * 24 * time.Hour)
	p, err := auth.NewAccountPool(dir, nil, 2, func(ctx context.Context, cfg auth.AuthConfig, old *types.TokenInfo) (types.TokenInfo, *auth.CreditSnapshot, error) {
		n := int(cfg.RefreshToken[len(cfg.RefreshToken)-1] - '0')
		remaining := []float64{1684.72, 1256.30, 883.5, 0, 2000}[n%5]
		return types.TokenInfo{AccessToken: "preview-only", RefreshToken: cfg.RefreshToken, ExpiresAt: now.Add(time.Hour)}, &auth.CreditSnapshot{Email: fmt.Sprintf("account.%d@example.com", n+1), Plan: []string{"KIRO PRO+", "KIRO STUDENT", "KIRO PRO", "KIRO STUDENT", "KIRO FREE"}[n%5], Limit: 2000, Used: 2000 - remaining, Remaining: remaining, ResetAt: &reset, CheckedAt: time.Now()}, nil
	})
	if err != nil {
		panic(err)
	}
	defer p.Close()
	for i, name := range []string{"主账号 · 日常服务", "开发团队", "备用账号", "历史账号", "测试账号"} {
		if _, _, err = p.Import([]auth.ImportAccount{{Name: name, Concurrency: 2 + i%3, AuthConfig: auth.AuthConfig{AuthType: "Social", RefreshToken: fmt.Sprintf("synthetic-preview-token-%d", i), Disabled: i == 4}}}); err != nil {
			panic(err)
		}
	}
	p.SetBuiltinKey("synthetic-preview-builtin-client-key")
	previewKey, _, _ := p.CreateAPIKey("Sub2API 渠道")
	_, _, _ = p.CreateAPIKey("团队 A")
	// Keep a trickle of leases open so the live chart moves.
	go func() {
		for {
			key := auth.BuiltinKeyID
			if r.Intn(3) > 0 {
				key = previewKey
			}
			ctx, cancel := context.WithTimeout(auth.WithAPIKeyID(context.Background(), key), time.Second)
			l, e := p.Acquire(ctx)
			cancel()
			if e == nil {
				hold := time.Duration(2+r.Intn(8)) * time.Second
				affinity := []string{"", "kept", "kept", "moved"}[r.Intn(4)]
				go func() {
					time.Sleep(hold)
					l.Release(auth.RequestOutcome{Success: true, Model: "claude-opus-5-5", Status: 200, UpstreamStatus: 200, Duration: hold, FirstByte: hold / 4, Stream: true, InputTokens: 120000 + int(hold/time.Millisecond), CacheReadTokens: 80000, OutputTokens: 600, Credits: 0.12 + float64(hold)/float64(time.Minute), Metered: true, Affinity: affinity})
				}()
			}
			time.Sleep(time.Duration(500+r.Intn(1500)) * time.Millisecond)
		}
	}()
	// A fake ListAvailableModels: the built-in list, Sonnet 5.5 added, GLM 5 gone, Haiku with a new limit.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models := []map[string]any{}
		for _, m := range config.CatalogSnapshot().Models {
			if m.ID == "glm-5" {
				continue
			}
			out := m.MaxOutput
			if m.ID == "claude-haiku-4.5" {
				out = 128000
			}
			models = append(models, map[string]any{"modelId": m.ID, "modelName": m.Name, "rateMultiplier": m.RateMultiplier, "tokenLimits": map[string]int{"maxInputTokens": m.MaxInput, "maxOutputTokens": out}})
		}
		models = append(models, map[string]any{"modelId": "claude-sonnet-5.5", "modelName": "Claude Sonnet 5.5", "description": "Claude Sonnet 5.5 with 1M context window", "rateMultiplier": 1.3, "tokenLimits": map[string]int{"maxInputTokens": 1000000, "maxOutputTokens": 64000}})
		_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
	}))
	defer upstream.Close()
	auth.ListModelsURL = upstream.URL + "/ListAvailableModels"
	secret := "synthetic-preview-independent-admin-secret"
	port := os.Getenv("PORT")
	if port == "" {
		port = "18700"
	}
	addr := "127.0.0.1:" + port
	h, err := server.NewAdminRouter(p, secret, []string{"http://" + addr, "http://localhost:" + port}, "./static/admin")
	if err != nil {
		panic(err)
	}
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Kiro-Admin-Token", secret)
		h.ServeHTTP(w, r)
	})}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	fmt.Println("Synthetic preview: http://" + addr + "/")
	if err = srv.ListenAndServe(); err != http.ErrServerClosed {
		panic(err)
	}
}
