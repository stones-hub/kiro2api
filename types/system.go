package types

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Anthropic accepts either a string or text blocks for system. Normalize at
// the shared JSON boundary so generation, counting and caching see the same
// representation, retaining block order, text and cache_control hints.
type AnthropicSystemMessages []AnthropicSystemMessage

func (s *AnthropicSystemMessages) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	invalid := errors.New("system must be a string or an array of text blocks")
	if bytes.Equal(data, []byte("null")) {
		*s = nil
		return nil
	}
	if len(data) == 0 {
		return invalid
	}
	switch data[0] {
	case '"':
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return invalid
		}
		if text == "" {
			*s = nil
		} else {
			*s = AnthropicSystemMessages{{Type: "text", Text: text}}
		}
	case '[':
		var blocks []AnthropicSystemMessage
		if err := json.Unmarshal(data, &blocks); err != nil {
			return invalid
		}
		*s = blocks
	default:
		return invalid
	}
	return nil
}
