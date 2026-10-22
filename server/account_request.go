package server

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/discount"
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

func acquireRequestAccount(c *gin.Context, service *auth.AuthService, model string) (*types.TokenWithUsage, func(), bool) {
	if pool := service.ManagedPool(); pool != nil {
		started := time.Now()
		// A prefix prepared by the route points at the account holding Kiro's
		// prompt cache for this conversation.
		var attempt *discount.Attempt
		if v, ok := c.Get("discount_attempt"); ok {
			attempt, _ = v.(*discount.Attempt)
		}
		lease, err := pool.AcquirePreferred(c.Request.Context(), attempt.Preferred())
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, auth.ErrPoolBusy) {
				status = http.StatusTooManyRequests
				c.Header("Retry-After", "2")
			}
			respondError(c, status, "%s", err.Error())
			return nil, func() {}, false
		}
		attempt.Bind(lease.AccountID())
		r := &accountRequest{lease: lease, outcome: auth.RequestOutcome{Model: model, Stream: c.GetBool("request_stream"), Affinity: attempt.Affinity()}, started: started}
		c.Set("account_request", r)
		// Upstream calls built from this context leave through the account's proxy.
		c.Request = c.Request.WithContext(utils.WithHTTPClient(c.Request.Context(), lease.Client))
		return lease.Token, func() {
			r.once.Do(func() {
				r.outcome.Status = c.Writer.Status()
				r.outcome.Canceled = c.Request.Context().Err() != nil
				r.outcome.Duration = time.Since(r.started)
				lease.Release(r.outcome)
			})
		}, true
	}
	token, err := service.GetTokenWithUsage()
	if err != nil {
		respondError(c, http.StatusServiceUnavailable, "%s", err.Error())
		return nil, func() {}, false
	}
	return token, func() {}, true
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
