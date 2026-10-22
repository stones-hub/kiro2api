package converter

import "testing"

func TestOpenAIUsageCountsCachedInputExactlyOnce(t *testing.T) {
	for _, counts := range [][3]int{{8192, 0, 8192}, {0, 8192, 8192}, {4096, 4096, 8192}, {0, 0, 0}} {
		r := ConvertAnthropicToOpenAI(map[string]any{"usage": map[string]any{
			"input_tokens": 23, "output_tokens": 4, "cache_read_input_tokens": counts[0], "cache_creation_input_tokens": counts[1],
		}}, "test-model", "test-id")
		if r.Usage.PromptTokens != counts[2]+23 || r.Usage.TotalTokens != counts[2]+27 || r.Usage.PromptTokensDetails == nil || r.Usage.PromptTokensDetails.CachedTokens != counts[0] {
			t.Fatalf("cache accounting duplicated or lost: %+v", r.Usage)
		}
	}
	r := ConvertAnthropicToOpenAI(map[string]any{"usage": map[string]any{"input_tokens": 23, "output_tokens": 4}}, "test", "id")
	if r.Usage.PromptTokensDetails != nil {
		t.Fatal("unknown cache count became a reported zero")
	}
}
