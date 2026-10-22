package converter

import (
	"encoding/json"
	"errors"
	"fmt"
	"kiro2api/types"
)

var ErrReasoningHistory = errors.New("unsupported reasoning history")

func ValidateReasoningHistory(messages []types.AnthropicRequestMessage) error {
	for _, message := range messages {
		if message.Role == "assistant" {
			if _, err := extractReasoningHistory(message.Content); err != nil {
				return err
			}
		}
	}
	return nil
}

// The official Kiro history schema has one reasoningContent union per assistant
// message. Preserve signed text or opaque redaction without modifying either.
func extractReasoningHistory(content any) (*types.KiroReasoningContent, error) {
	if _, isText := content.(string); isText || content == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	var blocks []types.ContentBlock
	if err := json.Unmarshal(encoded, &blocks); err != nil {
		return nil, err
	}
	var result *types.KiroReasoningContent
	for _, block := range blocks {
		var next *types.KiroReasoningContent
		switch block.Type {
		case "thinking":
			if block.Signature != "" {
				next = &types.KiroReasoningContent{ReasoningText: &types.KiroReasoningText{Text: block.Thinking, Signature: block.Signature}}
			}
		case "redacted_thinking":
			if block.Data != "" {
				next = &types.KiroReasoningContent{RedactedContent: block.Data}
			}
		}
		if next == nil {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("%w: Kiro accepts one signed thinking or redacted thinking block per assistant message", ErrReasoningHistory)
		}
		result = next
	}
	return result, nil
}
