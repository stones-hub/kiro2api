package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"kiro2api/config"
	"kiro2api/types"
	"kiro2api/utils"
)

type accountProviderError struct {
	step   string
	status int
}

func (e *accountProviderError) Error() string {
	return fmt.Sprintf("%s失败（HTTP %d）", e.step, e.status)
}
func safeAccountError(err error) string {
	var upstream *accountProviderError
	if errors.As(err, &upstream) {
		return upstream.Error()
	}
	if utils.IsProxyError(err) {
		return utils.ProxyErrorMessage(err)
	}
	return "账号刷新失败，请稍后重试或检查凭据"
}

func fetchAccountJSON(ctx context.Context, method, endpoint string, payload any, access string, out any, step string) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	req.Header.Set("User-Agent", "aws-sdk-js/1.0.0 KiroIDE-0.2.13")
	// DoRequest honours the account proxy carried by ctx.
	resp, err := utils.DoRequest(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &accountProviderError{step: step, status: resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
}

func FetchManagedAccount(ctx context.Context, cfg AuthConfig, previous *types.TokenInfo) (types.TokenInfo, *CreditSnapshot, error) {
	token := types.TokenInfo{}
	if previous != nil && previous.AccessToken != "" && time.Until(previous.ExpiresAt) > time.Minute {
		token = *previous
	} else {
		endpoint := config.RefreshTokenURL
		body := map[string]string{"refreshToken": cfg.RefreshToken}
		if cfg.AuthType == AuthMethodIdC {
			endpoint = config.IdcRefreshTokenURL
			body["clientId"] = cfg.ClientID
			body["clientSecret"] = cfg.ClientSecret
			body["grantType"] = "refresh_token"
		}
		var response types.RefreshResponse
		if err := fetchAccountJSON(ctx, "POST", endpoint, body, "", &response, "凭据验证"); err != nil {
			return token, nil, err
		}
		if response.AccessToken == "" || response.ExpiresIn <= 0 {
			return token, nil, errors.New("invalid refresh response")
		}
		token.FromRefreshResponse(response, cfg.RefreshToken)
		if response.RefreshToken != "" {
			token.RefreshToken = response.RefreshToken
		}
	}
	q := url.Values{"origin": {"AI_EDITOR"}, "resourceType": {"AGENTIC_REQUEST"}, "isEmailRequired": {"true"}}
	if token.ProfileArn != "" {
		q.Set("profileArn", token.ProfileArn)
	}
	var response struct {
		UserInfo struct {
			Email string `json:"email"`
		} `json:"userInfo"`
		SubscriptionInfo struct {
			Title string `json:"subscriptionTitle"`
		} `json:"subscriptionInfo"`
		NextDateReset float64 `json:"nextDateReset"`
		Breakdown     []struct {
			ResourceType string  `json:"resourceType"`
			Used         float64 `json:"currentUsageWithPrecision"`
			Limit        float64 `json:"usageLimitWithPrecision"`
			Reset        float64 `json:"nextDateReset"`
			Trial        *struct {
				Status string  `json:"freeTrialStatus"`
				Used   float64 `json:"currentUsageWithPrecision"`
				Limit  float64 `json:"usageLimitWithPrecision"`
			} `json:"freeTrialInfo"`
			Bonuses []struct {
				Status string  `json:"status"`
				Used   float64 `json:"currentUsage"`
				Limit  float64 `json:"usageLimit"`
			} `json:"bonuses"`
		} `json:"usageBreakdownList"`
	}
	if err := fetchAccountJSON(ctx, "GET", "https://codewhisperer.us-east-1.amazonaws.com/getUsageLimits?"+q.Encode(), nil, token.AccessToken, &response, "用量查询"); err != nil {
		return token, nil, err
	}
	for _, b := range response.Breakdown {
		if b.ResourceType != "CREDIT" {
			continue
		}
		s := &CreditSnapshot{Email: response.UserInfo.Email, Plan: response.SubscriptionInfo.Title, Limit: b.Limit, Used: b.Used, CheckedAt: time.Now().UTC()}
		if b.Trial != nil && b.Trial.Status == "ACTIVE" {
			s.Limit += b.Trial.Limit
			s.Used += b.Trial.Used
		}
		for _, bonus := range b.Bonuses {
			if bonus.Status == "ACTIVE" {
				s.Limit += bonus.Limit
				s.Used += bonus.Used
			}
		}
		s.Remaining = max(0, s.Limit-s.Used)
		reset := response.NextDateReset
		if b.Reset > 0 {
			reset = b.Reset
		}
		if reset > 0 {
			at := time.Unix(int64(reset), 0).UTC()
			s.ResetAt = &at
		}
		return token, s, nil
	}
	return token, nil, errors.New("credit data missing")
}
