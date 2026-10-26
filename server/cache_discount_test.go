package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/discount"
	"kiro2api/internal/testutil"
	"kiro2api/types"
	"kiro2api/utils"
)

type brokenCacheTestBody struct{ sent bool }

func (b *brokenCacheTestBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, testutil.EventFrame("assistantResponseEvent", `{"content":"partial"}`)), nil
	}
	return 0, errors.New("synthetic broken connection")
}
func (b *brokenCacheTestBody) Close() error { return nil }

type cacheTestTransport func(*http.Request) (*http.Response, error)

func (f cacheTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func testAccountingHandler(t *testing.T, protocol string, stream bool, request types.AnthropicRequest) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	c.Request.Header.Set("X-API-Key", "local-test-key")
	request.Stream = stream
	request.IncludeUsage = true
	if protocol == "anthropic" {
		if stream {
			handleStreamRequest(c, request, &types.TokenWithUsage{})
		} else {
			handleNonStreamRequest(c, request, types.TokenInfo{})
		}
	} else {
		if stream {
			handleOpenAIStreamRequest(c, request, types.TokenInfo{})
		} else {
			handleOpenAINonStreamRequest(c, request, types.TokenInfo{})
		}
	}
	if recorder.Code != 200 {
		t.Fatalf("handler status %d", recorder.Code)
	}
	if !stream {
		var data map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data["usage"].(map[string]any)
	}
	var finalUsage map[string]any
	terminal := false
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if line == "[DONE]" {
			terminal = true
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(line), &data); err != nil {
			t.Fatal(err)
		}
		if data["type"] == "message_stop" {
			terminal = true
		}
		if usage, ok := data["usage"].(map[string]any); ok {
			finalUsage = usage
		}
	}
	if !terminal || finalUsage == nil {
		t.Fatalf("terminal usage missing: %s", recorder.Body.String())
	}
	return finalUsage
}

func TestLocalDiscountFlowsThroughBothProtocolsAndStreamModes(t *testing.T) {
	oldClient, oldCache := utils.SharedHTTPClient, localDiscountCache
	t.Cleanup(func() { utils.SharedHTTPClient = oldClient; localDiscountCache = oldCache })
	fixture := testutil.EventFrame("assistantResponseEvent", `{"content":"OK"}`)
	fixture = append(fixture, testutil.EventFrame("meteringEvent", `{"usage":0.1,"unit":"credit"}`)...)
	utils.SharedHTTPClient = &http.Client{Transport: cacheTestTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(fixture))}, nil
	})}
	for _, protocol := range []string{"anthropic", "openai"} {
		for _, stream := range []bool{false, true} {
			var err error
			localDiscountCache, err = discount.New(5*time.Minute, 100, 1024)
			if err != nil {
				t.Fatal(err)
			}
			req := types.AnthropicRequest{Model: "claude-opus-4-8", MaxTokens: 16, Messages: []types.AnthropicRequestMessage{
				{Role: "user", Content: strings.Repeat("The notebook is on a wooden shelf. ", 250)},
			}}
			first := testAccountingHandler(t, protocol, stream, req)
			second := testAccountingHandler(t, protocol, stream, req)
			if protocol == "anthropic" {
				if first["cache_read_input_tokens"] != float64(0) || second["cache_read_input_tokens"].(float64) <= 0 {
					t.Fatalf("%s stream=%v cache not passed: %v %v", protocol, stream, first, second)
				}
				if second["input_tokens"].(float64)+second["cache_read_input_tokens"].(float64) != first["input_tokens"] {
					t.Fatal("input counted twice")
				}
				if first["cache_creation_input_tokens"] != float64(0) {
					t.Fatal("first request charged cache creation")
				}
			} else {
				if first["prompt_tokens"] != second["prompt_tokens"] {
					t.Fatal("OpenAI total prompt changed")
				}
				if second["prompt_tokens_details"].(map[string]any)["cached_tokens"].(float64) <= 0 {
					t.Fatal("OpenAI cached tokens missing")
				}
			}
			if second["cache_usage_source"] != "local_estimate" {
				t.Fatal("local accounting not identified")
			}
		}
	}
}

func TestBrokenAnthropicStreamDoesNotWarmDiscount(t *testing.T) {
	oldClient, oldCache := utils.SharedHTTPClient, localDiscountCache
	t.Cleanup(func() { utils.SharedHTTPClient = oldClient; localDiscountCache = oldCache })
	var err error
	localDiscountCache, err = discount.New(5*time.Minute, 100, 1024)
	if err != nil {
		t.Fatal(err)
	}
	utils.SharedHTTPClient = &http.Client{Transport: cacheTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &brokenCacheTestBody{}}, nil
	})}
	req := types.AnthropicRequest{Model: "claude-opus-4-8", MaxTokens: 16, Stream: true, Messages: []types.AnthropicRequestMessage{
		{Role: "user", Content: strings.Repeat("Synthetic long input for failed stream. ", 250)},
	}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("X-API-Key", "local-test-key")
	handleStreamRequest(c, req, &types.TokenWithUsage{})
	if prepareCacheDiscount(c, req).ReadTokens != 0 {
		t.Fatal("failed stream warmed discount state")
	}
}

func TestOfficialMetadataStillWorksWithLocalDiscountDisabled(t *testing.T) {
	oldClient, oldCache := utils.SharedHTTPClient, localDiscountCache
	t.Cleanup(func() { utils.SharedHTTPClient = oldClient; localDiscountCache = oldCache })
	localDiscountCache = nil
	fixture := testutil.EventFrame("assistantResponseEvent", `{"content":"OK"}`)
	fixture = append(fixture, testutil.EventFrame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":23,"outputTokens":4,"totalTokens":8219,"cacheReadInputTokens":8192,"cacheWriteInputTokens":0}}`)...)
	utils.SharedHTTPClient = &http.Client{Transport: cacheTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(fixture))}, nil
	})}
	req := types.AnthropicRequest{Model: "claude-opus-4-8", MaxTokens: 16, Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "hello"}}}
	for _, stream := range []bool{false, true} {
		usage := testAccountingHandler(t, "anthropic", stream, req)
		if usage["input_tokens"] != float64(23) || usage["cache_read_input_tokens"] != float64(8192) {
			t.Fatalf("official metadata lost: %v", usage)
		}
	}
}

func TestDisabledLocalDiscountKeepsFullInputOnRepeat(t *testing.T) {
	oldClient, oldCache := utils.SharedHTTPClient, localDiscountCache
	t.Cleanup(func() { utils.SharedHTTPClient = oldClient; localDiscountCache = oldCache })
	t.Setenv("KIRO_LOCAL_CACHE_DISCOUNT", "false")
	// The prefix index still runs, because conversation affinity depends on it;
	// only the local estimate is withheld from the client.
	localDiscountCache = configuredDiscountCache()
	if localDiscountCache == nil {
		t.Fatal("prefix index missing, conversations cannot stay on their account")
	}
	fixture := testutil.EventFrame("assistantResponseEvent", `{"content":"OK"}`)
	fixture = append(fixture, testutil.EventFrame("meteringEvent", `{"usage":0.1,"unit":"credit"}`)...)
	utils.SharedHTTPClient = &http.Client{Transport: cacheTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(fixture))}, nil
	})}
	req := types.AnthropicRequest{Model: "claude-opus-4-6", MaxTokens: 1024,
		System:   types.AnthropicSystemMessages{{Type: "text", Text: strings.Repeat("Synthetic stable reference. ", 300)}},
		Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "Reply briefly"}},
	}
	fullInput := float64(utils.NewTokenEstimator().EstimateTokens(&types.CountTokensRequest{Model: req.Model, System: req.System, Messages: req.Messages}))
	if fullInput <= 1024 {
		t.Fatal("fixture is below the local cache minimum")
	}
	for _, protocol := range []string{"anthropic", "openai"} {
		for _, stream := range []bool{false, true} {
			for attempt := 0; attempt < 2; attempt++ {
				usage := testAccountingHandler(t, protocol, stream, req)
				field := "input_tokens"
				if protocol == "openai" {
					field = "prompt_tokens"
				}
				if usage[field] != fullInput {
					t.Fatalf("%s stream=%v repeat=%d full input not retained: %v", protocol, stream, attempt, usage)
				}
				for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
					if value, present := usage[key]; present && value != float64(0) {
						t.Fatal("disabled local cache reported a discount")
					}
				}
				if details, ok := usage["prompt_tokens_details"].(map[string]any); ok && details["cached_tokens"] != nil && details["cached_tokens"] != float64(0) {
					t.Fatal("disabled OpenAI local cache reported a discount")
				}
				if usage["cache_usage_source"] == "local_estimate" {
					t.Fatal("disabled local discount source reported")
				}
			}
		}
	}
}

func TestFixedDiscountFlowsThroughAllProtocolsAndInputSizes(t *testing.T) {
	oldClient, oldCache := utils.SharedHTTPClient, localDiscountCache
	t.Cleanup(func() { utils.SharedHTTPClient = oldClient; localDiscountCache = oldCache })
	t.Setenv("KIRO_LOCAL_CACHE_DISCOUNT", "true")
	t.Setenv("KIRO_LOCAL_CACHE_MAX_PERCENT", "40")
	t.Setenv("KIRO_LOCAL_CACHE_MIN_UNCACHED_TOKENS", "0")
	t.Setenv("KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT", "true")
	fixture := testutil.EventFrame("assistantResponseEvent", `{"content":"OK"}`)
	utils.SharedHTTPClient = &http.Client{Transport: cacheTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(fixture))}, nil
	})}
	for _, repetitions := range []int{1, 300, 55000} {
		req := types.AnthropicRequest{Model: "claude-opus-4-6", MaxTokens: 32,
			System:   types.AnthropicSystemMessages{{Type: "text", Text: strings.Repeat("Synthetic stable reference data. ", repetitions)}},
			Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "Reply OK"}},
		}
		full := utils.NewTokenEstimator().EstimateTokens(&types.CountTokensRequest{Model: req.Model, System: req.System, Messages: req.Messages})
		for _, protocol := range []string{"anthropic", "openai"} {
			for _, stream := range []bool{false, true} {
				localDiscountCache = configuredDiscountCache()
				first := testAccountingHandler(t, protocol, stream, req)
				second := testAccountingHandler(t, protocol, stream, req)
				if protocol == "anthropic" {
					if first["cache_read_input_tokens"] != float64(0) || first["input_tokens"] != float64(full) {
						t.Fatal("first call discounted")
					}
					if second["cache_read_input_tokens"] != float64(full*40/100) || second["input_tokens"] != float64(full-full*40/100) {
						t.Fatalf("wrong fixed Anthropic usage: %v", second)
					}
				} else {
					if first["prompt_tokens"] != float64(full) || second["prompt_tokens"] != float64(full) {
						t.Fatal("OpenAI input total changed")
					}
					if first["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(0) || second["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(full*40/100) {
						t.Fatal("wrong OpenAI cached count")
					}
				}
			}
		}
	}
}
