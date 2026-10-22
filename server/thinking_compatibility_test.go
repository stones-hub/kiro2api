package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"kiro2api/config"
	"kiro2api/converter"
	"kiro2api/types"
)

func TestAllCatalogThinkingAliasesAndProtocolWire(t *testing.T) {
	for _, entry := range config.SupportedModels() {
		if !strings.HasSuffix(entry.ID, "-thinking") {
			continue
		}
		for _, protocol := range []string{"anthropic", "openai"} {
			t.Run(entry.ID+"/"+protocol, func(t *testing.T) {
				req := types.AnthropicRequest{Model: entry.ID, MaxTokens: 65536, ReasoningEffort: "low",
					System:   []types.AnthropicSystemMessage{{Type: "text", Text: "Original system"}},
					Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "Synthetic input"}}}
				if protocol == "openai" {
					maxTokens, completionTokens := 2048, 65536
					req = converter.ConvertOpenAIToAnthropic(types.OpenAIRequest{Model: entry.ID, MaxTokens: &maxTokens,
						MaxCompletionTokens: &completionTokens, ReasoningEffort: "low", Messages: []types.OpenAIMessage{
							{Role: "system", Content: "Original system"}, {Role: "user", Content: "Synthetic input"}}})
				}
				originalSystem, _ := json.Marshal(req.System)
				originalMessages, _ := json.Marshal(req.Messages)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest("POST", "/test", nil)
				if !validateThinkingRequest(c, &req) {
					t.Fatalf("valid model/effort rejected: %s", recorder.Body.String())
				}
				wire, err := buildCodeWhispererRequest(c, req, types.TokenInfo{}, true)
				if err != nil {
					t.Fatal(err)
				}
				defer wire.Body.Close()
				var body types.CodeWhispererRequest
				if err := json.NewDecoder(wire.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ConversationState.CurrentMessage.UserInputMessage.ModelId != entry.UpstreamModel.ID {
					t.Fatal("suffix changed model version")
				}
				if req.MaxTokens != min(65536, entry.Thinking.MaxTokens) || body.InferenceConfig == nil || body.InferenceConfig.MaxTokens != req.MaxTokens {
					t.Fatal("effective output cap was lost")
				}
				fields := body.AdditionalModelRequestFields
				switch entry.Thinking.Mode {
				case "adaptive":
					if fields == nil || fields.Thinking == nil || fields.Thinking.Type != "adaptive" || fields.OutputConfig == nil || fields.OutputConfig.Effort != "low" || fields.Reasoning != nil || fields.MaxTokens != req.MaxTokens {
						t.Fatal("Claude native controls not preserved")
					}
				case "reasoning":
					if fields == nil || fields.Reasoning == nil || fields.Reasoning.Effort != "low" || fields.Thinking != nil || fields.OutputConfig != nil || fields.MaxTokens != 0 {
						t.Fatal("GPT native reasoning received Claude-only fields")
					}
				default:
					if fields != nil || recorder.Header().Get("X-Kiro-Thinking-Effort") != "" {
						t.Fatal("unsupported native controls advertised/sent")
					}
				}
				if entry.Thinking.Mode == "legacy_hint" {
					if !req.ExtractThinkingTags || req.KiroThinking.BudgetTokens != 1024 || !strings.Contains(req.System[0].Text, "<max_thinking_length>1024</max_thinking_length>") {
						t.Fatal("legacy caller effort did not override suffix")
					}
				} else {
					currentSystem, _ := json.Marshal(req.System)
					currentMessages, _ := json.Marshal(req.Messages)
					if req.ExtractThinkingTags || string(currentSystem) != string(originalSystem) || string(currentMessages) != string(originalMessages) {
						t.Fatal("native/default mode changed a prompt")
					}
				}
			})
		}
	}
}

func TestNativeMaxTokensNormalizationBothWireLocations(t *testing.T) {
	for _, tc := range []struct {
		model           string
		requested, want int
	}{
		{"claude-opus-4-6", 16, 1024}, {"claude-opus-4-6", 0, 16384},
		{"claude-sonnet-4-6", 1024, 1024}, {"claude-sonnet-4-6", 64000, 64000},
		{"claude-opus-4-6", 65536, 64000}, {"claude-opus-4-8", 131072, 128000},
		{"claude-opus-5", 128000, 128000}, {"claude-sonnet-5", 131072, 64000},
	} {
		req := types.AnthropicRequest{Model: tc.model, MaxTokens: tc.requested, ReasoningEffort: "medium",
			Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "Synthetic input"}}}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/test", nil)
		if !validateThinkingRequest(c, &req) {
			t.Fatal("normalizable request rejected")
		}
		wire, err := buildCodeWhispererRequest(c, req, types.TokenInfo{}, true)
		if err != nil {
			t.Fatal(err)
		}
		var body types.CodeWhispererRequest
		if err := json.NewDecoder(wire.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		wire.Body.Close()
		if req.MaxTokens != tc.want || body.AdditionalModelRequestFields.MaxTokens != tc.want || body.InferenceConfig.MaxTokens != tc.want {
			t.Fatalf("inconsistent limits: %s %d", tc.model, tc.requested)
		}
		if req.KiroThinking.EffectiveEffort != "medium" {
			t.Fatal("length normalization changed caller effort")
		}
	}
}

func TestThinkingCompatibilityDisableAndEffortTranslation(t *testing.T) {
	for _, model := range []string{"claude-opus-5-thinking", "gpt-5.6-sol-thinking", "claude-haiku-4.5-thinking", "minimax-m2.5-thinking"} {
		req := types.AnthropicRequest{Model: model, MaxTokens: 4096, Thinking: &types.ThinkingConfig{Type: "disabled"}}
		if err := prepareThinkingRequest(&req); err != nil {
			t.Fatal(err)
		}
		if req.ExtractThinkingTags || len(req.System) != 0 {
			t.Fatal("explicit off injected a hint")
		}
		if f := req.KiroModelFields; f != nil {
			if f.Thinking != nil && f.Thinking.Type != "disabled" {
				t.Fatal("Claude off lost")
			}
			if f.Reasoning != nil && f.Reasoning.Effort != "none" {
				t.Fatal("GPT off lost")
			}
		}
	}
	for _, tc := range []struct{ model, effort, want string }{
		{"claude-opus-4-6", "xhigh", "max"}, {"claude-opus-4-7", "xhigh", "xhigh"},
		{"claude-opus-5", "minimal", "low"}, {"gpt-5.6-luna", "xhigh", "xhigh"},
	} {
		req := types.AnthropicRequest{Model: tc.model, ReasoningEffort: tc.effort}
		if err := prepareThinkingRequest(&req); err != nil {
			t.Fatal(err)
		}
		if req.KiroThinking.RequestedEffort != tc.effort || req.KiroThinking.EffectiveEffort != tc.want {
			t.Fatal("effort translation not reported correctly")
		}
	}
}

// opus55Catalog loads the built-in catalog plus Opus 5.5's account schema, which
// only allows adaptive thinking, and restores the built-in catalog afterwards.
func opus55Catalog(t *testing.T) {
	t.Helper()
	builtin := filepath.Join("..", "config", "models.default.json")
	raw, err := os.ReadFile(builtin)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var opus any
	_ = json.Unmarshal([]byte(`{"modelId":"claude-opus-5.5","modelName":"Claude Opus 5.5","tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":128000},"additionalModelRequestFieldsSchema":{"type":"object","properties":{"thinking":{"type":"object","properties":{"type":{"type":"string","enum":["adaptive"]},"display":{"type":"string","enum":["summarized","omitted"]}},"required":["type"]},"output_config":{"type":"object","properties":{"effort":{"type":"string","enum":["low","medium","high","xhigh","max"],"default":"medium"}}},"max_tokens":{"type":"integer","minimum":1024,"maximum":128000}},"additionalProperties":false}}`), &opus)
	doc["models"] = append(doc["models"].([]any), opus)
	b, _ := json.Marshal(doc)
	file := filepath.Join(t.TempDir(), "models.json")
	if err = os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = config.LoadModelCatalog(file); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := config.LoadModelCatalog(builtin); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAdaptiveOnlyDisableFallsBackToLowOmitted(t *testing.T) {
	opus55Catalog(t)
	for _, req := range []types.AnthropicRequest{
		{Model: "claude-opus-5-5", MaxTokens: 4096, Thinking: &types.ThinkingConfig{Type: "disabled"}},
		{Model: "claude-opus-5.5", MaxTokens: 4096, ReasoningEffort: "none"},
	} {
		if err := prepareThinkingRequest(&req); err != nil {
			t.Fatalf("%s: disable request rejected: %v", req.Model, err)
		}
		f := req.KiroModelFields
		if f == nil || f.Thinking == nil || f.Thinking.Type != "adaptive" || f.Thinking.Display != "omitted" || f.OutputConfig == nil || f.OutputConfig.Effort != "low" {
			t.Fatalf("%s: unexpected fields %+v", req.Model, f)
		}
		if !req.KiroThinking.DisableFallback || req.KiroThinking.EffectiveEffort != "low" {
			t.Fatalf("%s: fallback not reported: %+v", req.Model, req.KiroThinking)
		}
	}
	// Models that can turn thinking off keep doing so.
	req := types.AnthropicRequest{Model: "claude-opus-5", MaxTokens: 4096, Thinking: &types.ThinkingConfig{Type: "disabled"}}
	if err := prepareThinkingRequest(&req); err != nil || req.KiroThinking.DisableFallback || req.KiroModelFields.Thinking.Type != "disabled" {
		t.Fatalf("disable-capable model changed: %v %+v", err, req.KiroModelFields)
	}
}
