package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMapUpstreamError(t *testing.T) {
	orphan := `{"message":"messages.104.content.0: unexpected ` + "`tool_use_id`" + ` found in ` + "`tool_result`" + ` blocks: toolu_x."}`
	for _, tt := range []struct {
		status int
		body   string
		want   int
		code   string
		retry  bool
	}{
		{400, orphan, 400, "invalid_request", false},
		{400, `{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`, 400, "context_length_exceeded", false},
		{402, `{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`, 429, "upstream_quota", true},
		{403, `{}`, 503, "upstream_account_auth", true},
		{429, `{}`, 429, "upstream_rate_limited", true},
		{404, `not json`, 400, "invalid_request", false},
		{500, `{"message":"boom"}`, 502, "upstream_error", false},
	} {
		got := MapUpstreamError(tt.status, []byte(tt.body))
		if got.Status != tt.want || got.Code != tt.code || (got.RetryAfter != "") != tt.retry {
			t.Errorf("%d %s => %+v", tt.status, tt.body, got)
		}
	}
	if m := MapUpstreamError(400, []byte(orphan)).Message; !strings.Contains(m, "tool_use_id") {
		t.Fatalf("upstream reason lost: %s", m)
	}
	if m := MapUpstreamError(400, []byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)).Message; !strings.HasPrefix(m, "prompt is too long") {
		t.Fatalf("compaction phrase missing: %s", m)
	}
	if m := MapUpstreamError(400, []byte(strings.Repeat("界", 2000))).Message; len(m) > maxUpstreamMessage+40 || !json.Valid([]byte(`"`+m+`"`)) {
		t.Fatal("long message not truncated cleanly")
	}
}

func TestUpstreamErrorReachesClientWithRealStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	resp := &http.Response{StatusCode: 400, Body: http.NoBody}
	resp.Body = ioNopCloser(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)
	fail := inspectUpstreamResponse(c, resp)
	if fail == nil {
		t.Fatal("error not handled")
	}
	if fail.retry {
		t.Fatal("a request Kiro rejected was sent to another account")
	}
	fail.write(c)
	var body struct {
		Type  string
		Error struct{ Type, Code, Message string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 400 || body.Type != "error" || body.Error.Type != "invalid_request_error" || body.Error.Code != "context_length_exceeded" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestBuildAndSendErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	handleRequestBuildError(c, &conversionError{errors.New("消息列表为空")})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request_error") {
		t.Fatalf("conversion error: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
	handleRequestSendError(c, context.Canceled)
	if w.Body.Len() != 0 {
		t.Fatalf("wrote to a gone client: %s", w.Body.String())
	}
}

type stringBody struct{ *strings.Reader }

func (stringBody) Close() error { return nil }

func ioNopCloser(s string) stringBody { return stringBody{strings.NewReader(s)} }
