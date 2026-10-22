package parser

import (
	"encoding/json"
	"sort"
	"strings"
)

// Native reasoning shares the stream with text and tool calls. Keep their
// downstream block indices distinct, including when reasoning is interleaved.
// Streams without native reasoning retain their existing event representation.
type reasoningStream struct {
	source                  string
	active                  bool
	next                    int
	textIndex, thoughtIndex int
	tools                   map[int]int
}

func newReasoningStream() *reasoningStream {
	return &reasoningStream{source: "kiro_native", textIndex: -1, thoughtIndex: -1, tools: make(map[int]int)}
}

func contentEvent(kind string, index int, field string, value any) SSEEvent {
	data := map[string]any{"type": kind, "index": index}
	if field != "" {
		data[field] = value
	}
	return SSEEvent{Event: kind, Data: data}
}

func (s *reasoningStream) close(index *int) []SSEEvent {
	if *index < 0 {
		return nil
	}
	event := contentEvent("content_block_stop", *index, "", nil)
	*index = -1
	return []SSEEvent{event}
}

func (s *reasoningStream) reasoning(payload []byte) ([]SSEEvent, error) {
	var value struct {
		Text            string `json:"text"`
		Signature       string `json:"signature"`
		RedactedContent string `json:"redactedContent"`
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	if value.Text == "" && value.Signature == "" && value.RedactedContent == "" {
		return nil, nil
	}
	s.active = true
	events := s.close(&s.textIndex)
	if value.RedactedContent != "" {
		events = append(events, s.close(&s.thoughtIndex)...)
		index := s.next
		s.next++
		events = append(events,
			contentEvent("content_block_start", index, "content_block", map[string]any{"type": "redacted_thinking", "data": value.RedactedContent}),
			contentEvent("content_block_stop", index, "", nil))
	}
	if value.Text != "" || value.Signature != "" {
		if s.thoughtIndex < 0 {
			s.thoughtIndex = s.next
			s.next++
			events = append(events, contentEvent("content_block_start", s.thoughtIndex, "content_block", map[string]any{"type": "thinking", "thinking": "", "thinking_source": s.source}))
		}
		if value.Text != "" {
			events = append(events, contentEvent("content_block_delta", s.thoughtIndex, "delta", map[string]any{"type": "thinking_delta", "thinking": value.Text}))
		}
		if value.Signature != "" {
			events = append(events, contentEvent("content_block_delta", s.thoughtIndex, "delta", map[string]any{"type": "signature_delta", "signature": value.Signature}))
		}
	}
	return events, nil
}

func (s *reasoningStream) translate(input []SSEEvent) []SSEEvent {
	var output []SSEEvent
	for _, event := range input {
		data, ok := event.Data.(map[string]any)
		if !ok {
			output = append(output, event)
			continue
		}
		index, _ := data["index"].(int)
		block, _ := data["content_block"].(map[string]any)
		delta, _ := data["delta"].(map[string]any)
		if !s.active {
			if strings.HasPrefix(event.Event, "content_block_") && index >= s.next {
				s.next = index + 1
			}
			switch event.Event {
			case "content_block_start":
				if block["type"] == "tool_use" {
					s.tools[index] = index
				} else {
					s.textIndex = index
				}
			case "content_block_delta":
				if delta["type"] == "text_delta" {
					s.textIndex = index
				}
			case "content_block_stop":
				if index == s.textIndex {
					s.textIndex = -1
				}
				delete(s.tools, index)
			}
			output = append(output, event)
			continue
		}
		switch event.Event {
		case "content_block_start":
			output = append(output, s.close(&s.thoughtIndex)...)
			if block["type"] == "tool_use" {
				output = append(output, s.close(&s.textIndex)...)
				mapped := s.next
				s.next++
				s.tools[index] = mapped
				output = append(output, contentEvent(event.Event, mapped, "content_block", block))
			} // Text starts are generated at their first delta to avoid empty blocks.
		case "content_block_delta":
			output = append(output, s.close(&s.thoughtIndex)...)
			if delta["type"] == "text_delta" {
				if s.textIndex < 0 {
					s.textIndex = s.next
					s.next++
					output = append(output, contentEvent("content_block_start", s.textIndex, "content_block", map[string]any{"type": "text", "text": ""}))
				}
				output = append(output, contentEvent(event.Event, s.textIndex, "delta", delta))
			} else if mapped, exists := s.tools[index]; exists {
				output = append(output, contentEvent(event.Event, mapped, "delta", delta))
			}
		case "content_block_stop":
			if mapped, exists := s.tools[index]; exists {
				output = append(output, contentEvent(event.Event, mapped, "", nil))
				delete(s.tools, index)
			} else if index == 0 {
				output = append(output, s.close(&s.textIndex)...)
			}
		default:
			if event.Event == "message_delta" || event.Event == "message_stop" || event.Event == "error" || event.Event == "exception" {
				output = append(output, s.finish()...)
			}
			output = append(output, event)
		}
	}
	return output
}

func (s *reasoningStream) finish() []SSEEvent {
	if !s.active {
		return nil
	}
	events := s.close(&s.thoughtIndex)
	events = append(events, s.close(&s.textIndex)...)
	var indices []int
	for _, index := range s.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		events = append(events, contentEvent("content_block_stop", index, "", nil))
	}
	clear(s.tools)
	return events
}

// GetThinkingBlocks preserves native text, signature and redacted content in
// their stream order. No reasoning is synthesized from normal answer text.
func (pr *ParseResult) GetThinkingBlocks() []map[string]any {
	blocks := make(map[int]map[string]any)
	var order []int
	for _, event := range pr.Events {
		data, ok := event.Data.(map[string]any)
		if !ok {
			continue
		}
		index, _ := data["index"].(int)
		if event.Event == "content_block_start" {
			block, _ := data["content_block"].(map[string]any)
			if block["type"] == "thinking" || block["type"] == "redacted_thinking" {
				copy := make(map[string]any)
				for k, v := range block {
					copy[k] = v
				}
				blocks[index] = copy
				order = append(order, index)
			}
		} else if event.Event == "content_block_delta" && blocks[index] != nil {
			delta, _ := data["delta"].(map[string]any)
			for _, key := range []string{"thinking", "signature"} {
				if value, ok := delta[key].(string); ok {
					previous, _ := blocks[index][key].(string)
					blocks[index][key] = previous + value
				}
			}
		}
	}
	result := make([]map[string]any, 0, len(order))
	for _, index := range order {
		result = append(result, blocks[index])
	}
	return result
}

// GetNativeContentBlocks assembles ordered blocks for streams with reasoning.
// Existing non-reasoning response assembly remains unchanged.
func (pr *ParseResult) GetNativeContentBlocks() []map[string]any {
	blocks := make(map[int]map[string]any)
	for _, event := range pr.Events {
		data, ok := event.Data.(map[string]any)
		if !ok {
			continue
		}
		index, _ := data["index"].(int)
		if event.Event == "content_block_start" {
			original, _ := data["content_block"].(map[string]any)
			block := make(map[string]any)
			for k, v := range original {
				block[k] = v
			}
			// Text handlers may repeat their initial text in the first delta.
			if block["type"] == "text" {
				block["text"] = ""
			}
			blocks[index] = block
		} else if event.Event == "content_block_delta" {
			delta, _ := data["delta"].(map[string]any)
			if blocks[index] == nil && delta["type"] == "text_delta" {
				blocks[index] = map[string]any{"type": "text", "text": ""}
			}
			for _, key := range []string{"text", "thinking", "signature"} {
				if value, ok := delta[key].(string); ok && blocks[index] != nil {
					previous, _ := blocks[index][key].(string)
					blocks[index][key] = previous + value
				}
			}
		}
	}
	var indices []int
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	tools := pr.GetToolCalls()
	result := make([]map[string]any, 0, len(indices))
	for _, index := range indices {
		block := blocks[index]
		if block["type"] == "tool_use" {
			for _, tool := range tools {
				if tool.ID == block["id"] {
					block["input"] = tool.Arguments
					if tool.Arguments == nil {
						block["input"] = map[string]any{}
					}
					break
				}
			}
		}
		result = append(result, block)
	}
	return result
}
