package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"kiro2api/converter"
	"kiro2api/types"
	"kiro2api/utils"
)

func TestOpenAISystemAndDeveloperRolesReachUpstream(t *testing.T) {
	var input types.OpenAIRequest
	if err := utils.SafeUnmarshal([]byte(`{"model":"claude-opus-4-8","reasoning_effort":"medium","messages":[{"role":"system","content":"First rule"},{"role":"developer","content":[{"type":"text","text":"Second rule","cache_control":{"type":"ephemeral","ttl":"5m"}}]},{"role":"user","content":"Synthetic input"}]}`), &input); err != nil {
		t.Fatal(err)
	}
	req := converter.ConvertOpenAIToAnthropic(input)
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || len(req.System) != 2 {
		t.Fatal("instruction messages not separated from conversation")
	}
	if req.System[1].CacheControl == nil || req.System[1].CacheControl.TTL != "5m" {
		t.Fatal("OpenAI instruction cache hint lost")
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if !validateThinkingRequest(c, &req) {
		t.Fatal("thinking controls rejected")
	}
	wire, err := buildCodeWhispererRequest(c, req, types.TokenInfo{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer wire.Body.Close()
	var payload types.CodeWhispererRequest
	if err := json.NewDecoder(wire.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	history := payload.ConversationState.History
	if len(history) == 0 || history[0].(map[string]any)["userInputMessage"].(map[string]any)["content"] != "First rule\nSecond rule" {
		t.Fatal("OpenAI system/developer text did not reach upstream")
	}
	if payload.ConversationState.CurrentMessage.UserInputMessage.Content != "Synthetic input" {
		t.Fatal("instruction conversion replaced user input")
	}
}

func TestSystemStringAndBlocksReachUpstreamAndCountEqually(t *testing.T) {
	const system = "Synthetic system rule.\nKeep the quoted word: \"绿色\"."
	var referenceHistory []any
	var referenceCount int
	for index, value := range []any{system, []map[string]any{{"type": "text", "text": system, "cache_control": map[string]string{"type": "ephemeral", "ttl": "5m"}}}} {
		body, err := json.Marshal(map[string]any{
			"model": "claude-opus-4-6", "system": value, "max_tokens": 65536,
			"thinking": map[string]string{"type": "adaptive"}, "output_config": map[string]string{"effort": "medium"},
			"messages": []map[string]string{{"role": "user", "content": "Synthetic message"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Exercise the same map -> Sonic marshal -> typed decode used by /messages.
		var raw map[string]any
		if err := utils.SafeUnmarshal(body, &raw); err != nil {
			t.Fatal(err)
		}
		normalized, err := utils.SafeMarshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		var req types.AnthropicRequest
		if err := utils.SafeUnmarshal(normalized, &req); err != nil {
			t.Fatal(err)
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		if !validateThinkingRequest(c, &req) {
			t.Fatal("valid thinking controls rejected")
		}
		wire, err := buildCodeWhispererRequest(c, req, types.TokenInfo{}, true)
		if err != nil {
			t.Fatal(err)
		}
		var payload types.CodeWhispererRequest
		if err := json.NewDecoder(wire.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		wire.Body.Close()
		history := payload.ConversationState.History
		if len(history) == 0 || history[0].(map[string]any)["userInputMessage"].(map[string]any)["content"] != system {
			t.Fatal("system content lost before upstream")
		}
		if payload.AdditionalModelRequestFields.OutputConfig.Effort != "medium" || payload.AdditionalModelRequestFields.MaxTokens != 64000 || payload.InferenceConfig.MaxTokens != 64000 {
			t.Fatal("system normalization changed thinking or limits")
		}
		if index == 0 {
			referenceHistory = history
		} else if !reflect.DeepEqual(history, referenceHistory) {
			t.Fatal("equivalent system forms produced different upstream history")
		}
		countResponse := httptest.NewRecorder()
		countContext, _ := gin.CreateTestContext(countResponse)
		countContext.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(body))
		countContext.Request.Header.Set("Content-Type", "application/json")
		handleCountTokens(countContext)
		if countResponse.Code != http.StatusOK {
			t.Fatal("count_tokens rejected a valid system")
		}
		var counted types.CountTokensResponse
		if err := json.Unmarshal(countResponse.Body.Bytes(), &counted); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			referenceCount = counted.InputTokens
		} else if counted.InputTokens != referenceCount {
			t.Fatal("equivalent system forms were counted differently")
		}
	}
}

func TestSystemStringAndBlocksShareCachePrefix(t *testing.T) {
	oldCache := localDiscountCache
	t.Cleanup(func() { localDiscountCache = oldCache })
	t.Setenv("KIRO_LOCAL_CACHE_DISCOUNT", "true")
	// This test isolates system normalization; long-context policy has separate coverage.
	t.Setenv("KIRO_LOCAL_CACHE_MAX_PERCENT", "100")
	t.Setenv("KIRO_LOCAL_CACHE_MIN_UNCACHED_TOKENS", "4")
	t.Setenv("KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT", "false")
	localDiscountCache = configuredDiscountCache()
	system := strings.Repeat("Synthetic stable input prefix for cache accounting. ", 400)
	for index, value := range []any{system, []map[string]any{{"type": "text", "text": system, "cache_control": map[string]string{"type": "ephemeral"}}}} {
		body, _ := json.Marshal(map[string]any{"model": "claude-opus-4-6", "system": value,
			"messages": []map[string]string{{"role": "user", "content": "Reply briefly"}}})
		var req types.AnthropicRequest
		if err := utils.SafeUnmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("X-API-Key", "synthetic-cache-scope")
		attempt := prepareCacheDiscount(c, req)
		if attempt == nil {
			t.Fatal("system request skipped cache accounting")
		}
		if index == 0 {
			if attempt.ReadTokens != 0 {
				t.Fatal("cold request reported cache read")
			}
			attempt.Commit()
		} else if attempt.ReadTokens < 1024 {
			t.Fatal("equivalent system representation lost cache discount")
		}
	}
}
