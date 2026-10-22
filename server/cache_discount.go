package server

import (
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/config"
	"kiro2api/discount"
	"kiro2api/logger"
	"kiro2api/types"
)

var localDiscountCache *discount.Cache

func configuredDiscountCache() *discount.Cache {
	if os.Getenv("KIRO_LOCAL_CACHE_DISCOUNT") != "true" {
		return nil
	}
	policy := discount.Policy{MaxReadPercent: 40, MinUncachedTokens: 0, FixedPercentOnHit: true}
	if raw, present := os.LookupEnv("KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT"); present {
		if raw != "true" && raw != "false" {
			panic("invalid local cache configuration: KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT")
		}
		policy.FixedPercentOnHit = raw == "true"
	}
	for name, target := range map[string]*int{
		"KIRO_LOCAL_CACHE_MAX_PERCENT":         &policy.MaxReadPercent,
		"KIRO_LOCAL_CACHE_MIN_UNCACHED_TOKENS": &policy.MinUncachedTokens,
	} {
		if raw, present := os.LookupEnv(name); present {
			value, err := strconv.Atoi(raw)
			if err != nil {
				panic("invalid local cache configuration: " + name)
			}
			*target = value
		}
	}
	cache, err := discount.NewWithPolicy(5*time.Minute, 16384, 1, policy)
	if err != nil {
		panic("could not initialize local cache discount")
	}
	return cache
}

// prepareCacheDiscount returns the request's discount attempt. The route
// prepares it before choosing an account so the conversation's account can be
// preferred; handlers then reuse that same attempt.
func prepareCacheDiscount(c *gin.Context, req types.AnthropicRequest) *discount.Attempt {
	if v, ok := c.Get("discount_attempt"); ok {
		attempt, _ := v.(*discount.Attempt)
		return attempt
	}
	attempt := newCacheDiscountAttempt(c, req)
	c.Set("discount_attempt", attempt)
	return attempt
}

func newCacheDiscountAttempt(c *gin.Context, req types.AnthropicRequest) *discount.Attempt {
	if localDiscountCache == nil {
		return nil
	}
	model, ok := config.ResolveModel(req.Model)
	if !ok {
		return nil
	}
	// The bridge API key identifies the calling route. A forwarded user_id,
	// when available, further isolates end users without storing their identity.
	key := c.GetHeader("X-API-Key")
	if key == "" {
		key = c.GetHeader("Authorization")
	}
	userID, _ := req.Metadata["user_id"].(string)
	scope, _ := json.Marshal([]string{key, userID})
	attempt, err := localDiscountCache.Prepare(string(scope), &types.CountTokensRequest{
		Model: model.ID, System: req.System, Messages: req.Messages, Tools: filterSupportedTools(req.Tools),
	})
	if err != nil {
		logger.Warn("Local cache discount skipped: unsupported request representation")
		return nil
	}
	c.Header("X-Kiro-Cache-Accounting", "local-estimate")
	return attempt
}
