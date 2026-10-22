package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// UpstreamError is a Kiro error response translated for the client. The status
// says whose problem it is: 4xx means the request itself must change, 429/503
// mean another attempt can succeed, 502 means Kiro failed.
type UpstreamError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter string
}

// CodeWhispererErrorBody AWS CodeWhisperer错误响应体
type CodeWhispererErrorBody struct {
	Message string `json:"message"`
	Reason  string `json:"reason"`
}

const maxUpstreamMessage = 1000

func upstreamMessage(body []byte) (string, string) {
	var parsed CodeWhispererErrorBody
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		return truncate(parsed.Message), parsed.Reason
	}
	return truncate(strings.TrimSpace(string(body))), parsed.Reason
}

func truncate(s string) string {
	if len(s) <= maxUpstreamMessage {
		return s
	}
	// Cut on a rune boundary so the JSON stays valid UTF-8.
	cut := maxUpstreamMessage
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// MapUpstreamError maps a non-200 Kiro response. Account-level failures (auth,
// quota, rate limit) are reported as retryable, because the pool has already
// paused that account and the next attempt goes to another one; passing 401 or
// 402 through would make gateways treat this bridge's own key as broken.
func MapUpstreamError(status int, body []byte) UpstreamError {
	msg, reason := upstreamMessage(body)
	switch {
	case status == http.StatusBadRequest && reason == "CONTENT_LENGTH_EXCEEDS_THRESHOLD":
		// "prompt is too long" is the phrase clients such as Claude Code look for to compact.
		return UpstreamError{Status: 400, Code: "context_length_exceeded", Message: "prompt is too long: 输入超出模型上下文上限（Kiro: " + msg + "），请压缩或缩短对话后重试"}
	case status == http.StatusBadRequest:
		return UpstreamError{Status: 400, Code: "invalid_request", Message: "Kiro 拒绝了请求: " + msg}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return UpstreamError{Status: 503, Code: "upstream_account_auth", Message: "上游账号认证失败，已暂停该账号，请重试", RetryAfter: "1"}
	case status == http.StatusPaymentRequired:
		return UpstreamError{Status: 429, Code: "upstream_quota", Message: "上游账号额度已用完，已暂停该账号，请重试", RetryAfter: "1"}
	case status == http.StatusTooManyRequests:
		return UpstreamError{Status: 429, Code: "upstream_rate_limited", Message: "Kiro 上游限流，请稍后重试", RetryAfter: "5"}
	case status == http.StatusRequestEntityTooLarge:
		return UpstreamError{Status: 413, Code: "request_too_large", Message: "请求体过大: " + msg}
	case status >= 400 && status < 500:
		return UpstreamError{Status: 400, Code: "invalid_request", Message: fmt.Sprintf("Kiro 拒绝了请求（HTTP %d）: %s", status, msg)}
	default:
		return UpstreamError{Status: 502, Code: "upstream_error", Message: fmt.Sprintf("Kiro 上游服务错误（HTTP %d），请稍后重试", status)}
	}
}

// anthropicErrorType names the Anthropic error type for an HTTP status.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}
