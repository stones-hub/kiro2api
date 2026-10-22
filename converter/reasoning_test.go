package converter

import (
	"errors"
	"testing"

	"kiro2api/types"
)

func TestSignedReasoningHistoryPreservesTextAndSignature(t *testing.T) {
	content := []any{map[string]any{"type": "thinking", "thinking": "Exact native text\n中文", "signature": "opaque-value"}, map[string]any{"type": "text", "text": "42"}}
	req := types.AnthropicRequest{Model: "claude-opus-4-8", Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "Problem"}, {Role: "assistant", Content: content}, {Role: "user", Content: "Follow-up"}}}
	cw, err := BuildCodeWhispererRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cw.ConversationState.History) != 2 {
		t.Fatal("history pairing changed")
	}
	assistant := cw.ConversationState.History[1].(types.HistoryAssistantMessage).AssistantResponseMessage
	if assistant.ReasoningContent == nil || assistant.ReasoningContent.ReasoningText.Text != "Exact native text\n中文" || assistant.ReasoningContent.ReasoningText.Signature != "opaque-value" || assistant.Content != "42" {
		t.Fatal("native history text or signature modified")
	}
}

func TestUnsignedCompatibilityReasoningDoesNotBecomeSignedNativeHistory(t *testing.T) {
	value, err := extractReasoningHistory([]any{map[string]any{"type": "thinking", "thinking": "Tagged text"}})
	if err != nil || value != nil {
		t.Fatal("unsigned reasoning promoted to native history")
	}
	value, err = extractReasoningHistory([]types.ContentBlock{{Type: "redacted_thinking", Data: "AQIDBA=="}})
	if err != nil || value.RedactedContent != "AQIDBA==" {
		t.Fatal("opaque redaction modified")
	}
	_, err = extractReasoningHistory([]types.ContentBlock{{Type: "thinking", Thinking: "one", Signature: "sig1"}, {Type: "thinking", Thinking: "two", Signature: "sig2"}})
	if !errors.Is(err, ErrReasoningHistory) {
		t.Fatal("independent signatures silently concatenated")
	}
}

func TestOpenAIReasoningHistoryRoundTrip(t *testing.T) {
	req := ConvertOpenAIToAnthropic(types.OpenAIRequest{Model: "claude-opus-4-8", Messages: []types.OpenAIMessage{{Role: "assistant", Content: "42", ReasoningContent: "Check", ReasoningSignature: "sig"}}})
	reasoning, err := extractReasoningHistory(req.Messages[0].Content)
	if err != nil || reasoning == nil || reasoning.ReasoningText.Text != "Check" || reasoning.ReasoningText.Signature != "sig" {
		t.Fatal("OpenAI signed reasoning round trip lost")
	}
}
