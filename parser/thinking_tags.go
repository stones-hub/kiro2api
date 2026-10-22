package parser

import (
	"encoding/json"
	"strings"
)

// Only an opening tag at the start of a response is eligible. This avoids
// turning XML examples or quoted tags in an ordinary answer into reasoning.
// Buffer at most the tag prefix; forward reasoning incrementally as it arrives.
type thinkingTags struct {
	state            int // 0=detect opening, 1=thinking, 2=ordinary text
	buffer, closeTag string
}

func (p *thinkingTags) feed(text string) (thought, answer string) {
	if p.state == 2 {
		return "", text
	}
	p.buffer += text
	if p.state == 0 {
		trimmed := strings.TrimLeft(p.buffer, " \t\r\n")
		for _, tag := range []string{"<thinking>", "<think>"} {
			if strings.HasPrefix(trimmed, tag) {
				p.closeTag = "</" + tag[1:]
				p.buffer = trimmed[len(tag):]
				p.state = 1
				break
			}
		}
		if p.state == 0 {
			for _, tag := range []string{"<thinking>", "<think>"} {
				if len(p.buffer) <= 64 && strings.HasPrefix(tag, trimmed) {
					return "", ""
				}
			}
			answer, p.buffer, p.state = p.buffer, "", 2
			return "", answer
		}
	}
	if end := strings.Index(p.buffer, p.closeTag); end >= 0 {
		thought, answer = p.buffer[:end], p.buffer[end+len(p.closeTag):]
		p.buffer, p.state = "", 2
		return thought, answer
	}
	keep := 0
	for size := 1; size < len(p.closeTag) && size <= len(p.buffer); size++ {
		if strings.HasSuffix(p.buffer, p.closeTag[:size]) {
			keep = size
		}
	}
	thought = p.buffer[:len(p.buffer)-keep]
	p.buffer = p.buffer[len(p.buffer)-keep:]
	return thought, ""
}

func (cmp *CompliantMessageProcessor) translateWithThinkingTags(input []SSEEvent) []SSEEvent {
	if cmp.thinkingTags == nil {
		return cmp.reasoning.translate(input)
	}
	var output []SSEEvent
	for _, event := range input {
		data, _ := event.Data.(map[string]any)
		delta, _ := data["delta"].(map[string]any)
		block, _ := data["content_block"].(map[string]any)
		if event.Event == "content_block_start" && block["type"] == "text" {
			continue
		}
		if event.Event == "content_block_delta" && delta["type"] == "text_delta" {
			value, _ := delta["text"].(string)
			thought, answer := cmp.thinkingTags.feed(value)
			if thought != "" {
				cmp.reasoning.source = "kiro_tagged_text"
				payload, _ := json.Marshal(map[string]string{"text": thought})
				events, _ := cmp.reasoning.reasoning(payload)
				output = append(output, events...)
			}
			if answer != "" {
				output = append(output, cmp.reasoning.translate([]SSEEvent{contentEvent("content_block_delta", 0, "delta", map[string]any{"type": "text_delta", "text": answer})})...)
			}
			continue
		}
		output = append(output, cmp.reasoning.translate([]SSEEvent{event})...)
	}
	return output
}

func (cmp *CompliantMessageProcessor) finishThinking() []SSEEvent {
	var output []SSEEvent
	if p := cmp.thinkingTags; p != nil && p.buffer != "" {
		if p.state == 1 {
			cmp.reasoning.source = "kiro_tagged_text"
			payload, _ := json.Marshal(map[string]string{"text": p.buffer})
			output, _ = cmp.reasoning.reasoning(payload)
		} else {
			output = cmp.reasoning.translate([]SSEEvent{contentEvent("content_block_delta", 0, "delta", map[string]any{"type": "text_delta", "text": p.buffer})})
		}
		p.buffer = ""
	}
	return append(output, cmp.reasoning.finish()...)
}
