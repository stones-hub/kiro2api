package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/discount"
	"kiro2api/logger"
	"kiro2api/types"
	"kiro2api/utils"
)

// The lease belongs to the entire HTTP handler, including the final streaming write.
type accountRequest struct {
	lease   *auth.AccountLease
	outcome auth.RequestOutcome
	once    sync.Once
	// started includes time spent queued for an account, as the client sees it.
	started   time.Time
	firstByte sync.Once
}

// release reports the result to the pool exactly once, whichever path gets
// here first. status is what the client is told; 0 takes whatever the handler
// has written so far.
func (r *accountRequest) release(c *gin.Context, status int) {
	r.once.Do(func() {
		if status == 0 {
			status = c.Writer.Status()
		}
		r.outcome.Status = status
		r.outcome.Canceled = c.Request.Context().Err() != nil
		r.outcome.Duration = time.Since(r.started)
		r.lease.Release(r.outcome)
	})
}

// accountSwitcher owns every lease one HTTP request takes: the account it
// started on and, after an upstream failure, the single account it retried on.
type accountSwitcher struct {
	pool    *auth.AccountPool
	model   string
	attempt *discount.Attempt
	// base is the request context before any account was bound to it. Each
	// account's route is wrapped onto this one, so a retry on a direct account
	// does not inherit the previous account's proxy: WithHTTPClient leaves the
	// context untouched for a direct lease and would keep the stale client.
	base context.Context
	// started is when the request began queueing for its first account.
	started time.Time
	held    []*accountRequest
	// moved records the one retry a request is allowed.
	moved bool
}

const accountSwitcherKey = "account_switcher"

func acquireRequestAccount(c *gin.Context, service *auth.AuthService, model string) (*types.TokenWithUsage, func(), bool) {
	pool := service.ManagedPool()
	if pool == nil {
		token, err := service.GetTokenWithUsage()
		if err != nil {
			respondError(c, http.StatusServiceUnavailable, "%s", err.Error())
			return nil, func() {}, false
		}
		return token, func() {}, true
	}
	// A prefix prepared by the route points at the account holding Kiro's
	// prompt cache for this conversation.
	var attempt *discount.Attempt
	if v, ok := c.Get("discount_attempt"); ok {
		attempt, _ = v.(*discount.Attempt)
	}
	s := &accountSwitcher{pool: pool, model: model, attempt: attempt, base: c.Request.Context(), started: time.Now()}
	lease, err := pool.AcquirePreferred(s.base, attempt.Preferred())
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, auth.ErrPoolBusy) {
			status = http.StatusTooManyRequests
			c.Header("Retry-After", "2")
		}
		respondError(c, status, "%s", err.Error())
		return nil, func() {}, false
	}
	token := s.adopt(c, lease)
	c.Set(accountSwitcherKey, s)
	return token, func() {
		for _, r := range s.held {
			r.release(c, 0)
		}
	}, true
}

// adopt makes lease the account serving the request from here on: its
// statistics, the conversation's home and the proxy route all follow it.
func (s *accountSwitcher) adopt(c *gin.Context, lease *auth.AccountLease) *types.TokenWithUsage {
	s.attempt.Bind(lease.AccountID())
	r := &accountRequest{lease: lease, started: s.started,
		outcome: auth.RequestOutcome{Model: s.model, Stream: c.GetBool("request_stream"), Affinity: s.attempt.Affinity()}}
	s.held = append(s.held, r)
	c.Set("account_request", r)
	// Upstream calls built from this context leave through the account's proxy.
	// Always wrap the untouched context: a direct lease adds nothing, so
	// wrapping the current one would keep the previous account's proxy.
	c.Request = c.Request.WithContext(utils.WithHTTPClient(s.base, lease.Client))
	return lease.Token
}

// switchAccountAfterFailure moves a request whose upstream call failed to
// another account for one more try, and reports the token to use. The failed
// account is released first, carrying that failure, so the pool cools it down
// and the management page attributes the failure to it while the retry counts
// against the new account. A request moves at most once; when no other account
// is free right now the original failure stands.
func switchAccountAfterFailure(c *gin.Context, status int) (types.TokenInfo, bool) {
	v, ok := c.Get(accountSwitcherKey)
	if !ok {
		return types.TokenInfo{}, false
	}
	s, _ := v.(*accountSwitcher)
	if s == nil || s.moved || len(s.held) == 0 {
		return types.TokenInfo{}, false
	}
	// There is nothing to retry for a client that has already left.
	if c.Request.Context().Err() != nil {
		return types.TokenInfo{}, false
	}
	failed := s.held[len(s.held)-1]
	lease, err := s.pool.AcquireOther(c.Request.Context(), failed.lease.AccountID())
	if err != nil {
		logger.Warn("上游失败且没有其他可用账号，返回原始错误",
			addReqFields(c, logger.String("account_id", failed.lease.AccountID()), logger.Err(err))...)
		return types.TokenInfo{}, false
	}
	s.moved = true
	failed.release(c, status)
	token := s.adopt(c, lease)
	logger.Warn("上游失败，换号重试一次",
		addReqFields(c,
			logger.String("failed_account_id", failed.lease.AccountID()),
			logger.String("retry_account_id", lease.AccountID()),
			logger.Int("failed_status", status))...)
	return token.TokenInfo, true
}

func accountMeasurement(c *gin.Context) *accountRequest {
	v, ok := c.Get("account_request")
	if !ok {
		return nil
	}
	r, _ := v.(*accountRequest)
	return r
}
func recordAccountUsage(c *gin.Context, usage map[string]any) {
	r := accountMeasurement(c)
	if r == nil {
		return
	}
	integer := func(k string) int {
		switch v := usage[k].(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		}
		return 0
	}
	r.outcome.CacheReadTokens = integer("cache_read_input_tokens")
	r.outcome.InputTokens = integer("input_tokens") + integer("cache_creation_input_tokens") + r.outcome.CacheReadTokens
	r.outcome.OutputTokens = integer("output_tokens")
}

// recordAccountCredits stores what Kiro's meteringEvent frames charged.
func recordAccountCredits(c *gin.Context, credits float64, ok bool) {
	if r := accountMeasurement(c); r != nil && ok {
		r.outcome.Credits, r.outcome.Metered = credits, true
	}
}
func recordAccountMonthlyLimit(c *gin.Context) {
	if r := accountMeasurement(c); r != nil {
		r.outcome.MonthlyLimit = true
	}
}
func recordAccountProxyError(c *gin.Context) {
	if r := accountMeasurement(c); r != nil {
		r.outcome.ProxyError = true
	}
}
func recordAccountSuccess(c *gin.Context) {
	if r := accountMeasurement(c); r != nil {
		r.outcome.Success = true
	}
}
func recordAccountUpstream(c *gin.Context, status int) {
	if r := accountMeasurement(c); r != nil {
		r.outcome.UpstreamStatus = status
	}
}

// recordAccountFirstByte marks the first content delta written to the client.
func recordAccountFirstByte(c *gin.Context) {
	if r := accountMeasurement(c); r != nil {
		r.firstByte.Do(func() { r.outcome.FirstByte = time.Since(r.started) })
	}
}
func recordAccountEmpty(c *gin.Context) {
	if r := accountMeasurement(c); r != nil {
		r.outcome.Empty = true
	}
}
