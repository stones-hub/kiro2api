package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"kiro2api/config"
	"kiro2api/converter"
	"kiro2api/internal/testutil"
	"kiro2api/types"
	"kiro2api/utils"
)

func TestNativeThinkingThroughBothProtocols(t *testing.T) {
	oldClient, oldCache := utils.SharedHTTPClient, localDiscountCache
	t.Cleanup(func() { utils.SharedHTTPClient = oldClient; localDiscountCache = oldCache })
	localDiscountCache = nil
	var fixture []byte
	for _, item := range [][2]string{{"reasoningContentEvent", `{"text":"Check the constraints."}`}, {"reasoningContentEvent", `{"signature":"opaque-test-signature"}`}, {"assistantResponseEvent", `{"content":"42"}`}} {
		fixture = append(fixture, testutil.EventFrame(item[0], item[1])...)
	}
	utils.SharedHTTPClient = &http.Client{Transport: cacheTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(fixture))}, nil
	})}
	for _, protocol := range []string{"anthropic", "openai"} {
		for _, stream := range []bool{false, true} {
			req := types.AnthropicRequest{Model: "claude-opus-4-8", MaxTokens: 2048, Stream: stream, IncludeUsage: true, Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "Synthetic problem"}}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/test", nil)
			if protocol == "anthropic" {
				if stream {
					handleStreamRequest(c, req, &types.TokenWithUsage{})
				} else {
					handleNonStreamRequest(c, req, types.TokenInfo{})
				}
			} else {
				if stream {
					handleOpenAIStreamRequest(c, req, types.TokenInfo{})
				} else {
					handleOpenAINonStreamRequest(c, req, types.TokenInfo{})
				}
			}
			if recorder.Code != 200 {
				t.Fatalf("status %d", recorder.Code)
			}
			thought, answer, signature := "", "", ""
			if stream {
				for _, line := range strings.Split(recorder.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if line == "[DONE]" {
						continue
					}
					var event map[string]any
					if err := json.Unmarshal([]byte(line), &event); err != nil {
						t.Fatal(err)
					}
					delta, _ := event["delta"].(map[string]any)
					if choices, ok := event["choices"].([]any); ok && len(choices) > 0 {
						delta, _ = choices[0].(map[string]any)["delta"].(map[string]any)
					}
					for key, target := range map[string]*string{"text": &answer, "content": &answer, "thinking": &thought, "reasoning_content": &thought, "signature": &signature, "reasoning_signature": &signature} {
						if value, ok := delta[key].(string); ok {
							*target += value
						}
					}
				}
			} else {
				var value map[string]any
				if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
					t.Fatal(err)
				}
				if protocol == "anthropic" {
					for _, v := range value["content"].([]any) {
						b := v.(map[string]any)
						if b["type"] == "thinking" {
							thought, _ = b["thinking"].(string)
							signature, _ = b["signature"].(string)
						} else if b["type"] == "text" {
							answer, _ = b["text"].(string)
						}
					}
				} else {
					msg := value["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
					thought, _ = msg["reasoning_content"].(string)
					signature, _ = msg["reasoning_signature"].(string)
					answer, _ = msg["content"].(string)
				}
			}
			if thought != "Check the constraints." || signature != "opaque-test-signature" || answer != "42" {
				t.Fatalf("%s stream=%v: reasoning or answer lost", protocol, stream)
			}
		}
	}
}

func TestNativeThinkingSuffixWirePayload(t *testing.T) {
	for _, model := range []string{"claude-opus-4-6-thinking", "claude-sonnet-4.6-thinking", "claude-opus-4-8-thinking"} {
		for _, protocol := range []string{"anthropic", "openai"} {
			req := types.AnthropicRequest{Model: model, MaxTokens: 4096,
				Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "hello"}}}
			if protocol == "openai" {
				maxTokens := 4096
				req = converter.ConvertOpenAIToAnthropic(types.OpenAIRequest{Model: model, MaxTokens: &maxTokens,
					Messages: []types.OpenAIMessage{{Role: "user", Content: "hello"}}})
			}
			if err := prepareThinkingRequest(&req); err != nil {
				t.Fatal(err)
			}
			if req.Model != model || len(req.System) != 0 || req.ExtractThinkingTags {
				t.Fatal("suffix changed client model or injected a prompt")
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/test", nil)
			wire, err := buildCodeWhispererRequest(c, req, types.TokenInfo{}, true)
			if err != nil {
				t.Fatal(err)
			}
			var payload types.CodeWhispererRequest
			if err := json.NewDecoder(wire.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			wire.Body.Close()
			canonical, _ := config.ResolveModel(model)
			f := payload.AdditionalModelRequestFields
			if payload.ConversationState.CurrentMessage.UserInputMessage.ModelId != canonical.ID ||
				f == nil || f.Thinking.Type != "adaptive" || f.Thinking.Display != "summarized" ||
				f.OutputConfig.Effort != "max" || f.MaxTokens != 4096 {
				t.Fatal("native force-thinking wire controls missing")
			}
		}
	}
	for _, model := range []string{"claude-opus-4-6-thinking-thinking", "unknown-thinking"} {
		if _, ok := config.ResolveModel(model); ok {
			t.Fatalf("unexpected thinking alias %s", model)
		}
	}
}

func TestNativeThinkingExplicitControls(t *testing.T) {
	for _, tc := range []struct {
		name, body, mode, effort, display string
		fail                              bool
	}{
		{"ordinary", `{"model":"claude-opus-4-6","max_tokens":16}`, "", "", "", false},
		{"anthropic", `{"model":"claude-opus-4-6","max_tokens":4096,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, "adaptive", "max", "summarized", false},
		{"openai", `{"model":"claude-opus-4-6","max_tokens":4096,"reasoning_effort":"medium"}`, "adaptive", "medium", "summarized", false},
		{"override_suffix", `{"model":"claude-opus-4-6-thinking","max_tokens":4096,"reasoning_effort":"low"}`, "adaptive", "low", "summarized", false},
		{"disabled", `{"model":"claude-opus-4-6-thinking","max_tokens":4096,"thinking":{"type":"disabled"}}`, "disabled", "", "", false},
		{"none", `{"model":"claude-opus-4-8-thinking","max_tokens":4096,"reasoning_effort":"none"}`, "disabled", "", "", false},
		{"omitted", `{"model":"claude-opus-4-8","max_tokens":4096,"thinking":{"type":"adaptive","display":"omitted"}}`, "adaptive", "high", "omitted", false},
		{"xhigh48", `{"model":"claude-opus-4-8","max_tokens":4096,"reasoning_effort":"xhigh"}`, "adaptive", "xhigh", "summarized", false},
		{"xhigh46", `{"model":"claude-opus-4-6","max_tokens":4096,"reasoning_effort":"xhigh"}`, "adaptive", "max", "summarized", false},
		{"conflict", `{"model":"claude-opus-4-6","max_tokens":4096,"reasoning_effort":"low","output_config":{"effort":"max"}}`, "", "", "", true},
		{"budget", `{"model":"claude-opus-4-6","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":1024}}`, "adaptive", "low", "summarized", false},
		{"budget_effort_priority", `{"model":"claude-opus-4-6","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":32768},"output_config":{"effort":"low"}}`, "adaptive", "low", "summarized", false},
		{"unknown_effort", `{"model":"claude-opus-4-6","max_tokens":4096,"reasoning_effort":"invalid"}`, "", "", "", true},
		{"too_small", `{"model":"claude-opus-4-6-thinking","max_tokens":16}`, "adaptive", "max", "summarized", false},
		{"too_large", `{"model":"claude-opus-4-6-thinking","max_tokens":64001}`, "adaptive", "max", "summarized", false},
		{"negative_tokens", `{"model":"claude-opus-4-6-thinking","max_tokens":-1}`, "", "", "", true},
		{"negative_budget", `{"model":"claude-opus-5","thinking":{"type":"enabled","budget_tokens":-1}}`, "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req types.AnthropicRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatal(err)
			}
			req.System = []types.AnthropicSystemMessage{{Type: "text", Text: "Keep original system"}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if validateThinkingRequest(c, &req) == tc.fail {
				t.Fatalf("unexpected validation result for %s", tc.name)
			}
			if tc.fail {
				return
			}
			if req.ExtractThinkingTags || len(req.System) != 1 || req.System[0].Text != "Keep original system" {
				t.Fatal("native controls changed prompt")
			}
			if tc.mode == "" {
				if req.KiroModelFields != nil {
					t.Fatal("ordinary request default changed")
				}
				return
			}
			f := req.KiroModelFields
			if f == nil || f.Thinking.Type != tc.mode || f.Thinking.Display != tc.display {
				t.Fatal("incorrect native thinking control")
			}
			if tc.effort == "" {
				if f.OutputConfig != nil {
					t.Fatal("disabled thinking still sent effort")
				}
			} else if f.OutputConfig == nil || f.OutputConfig.Effort != tc.effort {
				t.Fatal("incorrect native effort")
			}
		})
	}
}
