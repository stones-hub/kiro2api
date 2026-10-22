package parser

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"kiro2api/internal/testutil"
)

func reasoningFixture() []byte {
	var data []byte
	for _, event := range [][2]string{
		{"reasoningContentEvent", `{"text":"Check the "}`},
		{"reasoningContentEvent", `{"text":"constraints."}`},
		{"reasoningContentEvent", `{"signature":"opaque-signature"}`},
		{"assistantResponseEvent", `{"content":"42"}`},
		{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":10,"outputTokens":8,"totalTokens":18}}`},
	} {
		data = append(data, testutil.EventFrame(event[0], event[1])...)
	}
	return data
}

func assertBlockLifecycle(t *testing.T, events []SSEEvent) {
	t.Helper()
	started, closed := map[int]bool{}, map[int]bool{}
	for _, event := range events {
		data, _ := event.Data.(map[string]any)
		index, _ := data["index"].(int)
		switch event.Event {
		case "content_block_start":
			if started[index] {
				t.Fatalf("duplicate block index %d", index)
			}
			started[index] = true
		case "content_block_delta":
			if !started[index] || closed[index] {
				t.Fatalf("delta outside active block %d", index)
			}
		case "content_block_stop":
			if !started[index] || closed[index] {
				t.Fatalf("invalid stop %d", index)
			}
			closed[index] = true
		}
	}
	if !reflect.DeepEqual(started, closed) {
		t.Fatal("unclosed content block")
	}
}

func TestNativeThinkingAcrossEveryFrameBoundary(t *testing.T) {
	fixture := reasoningFixture()
	for _, size := range []int{1, 7, 63, len(fixture)} {
		p := NewCompliantEventStreamParser()
		var events []SSEEvent
		for offset := 0; offset < len(fixture); offset += size {
			end := min(len(fixture), offset+size)
			part, err := p.ParseStream(fixture[offset:end])
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, part...)
		}
		events = append(events, p.Finish()...)
		assertBlockLifecycle(t, events)
		result := &ParseResult{Events: events}
		blocks := result.GetThinkingBlocks()
		if len(blocks) != 1 || blocks[0]["thinking"] != "Check the constraints." || blocks[0]["signature"] != "opaque-signature" || blocks[0]["thinking_source"] != "kiro_native" {
			t.Fatalf("lost reasoning: %v", blocks)
		}
		if result.GetCompletionText() != "42" || !p.GetTokenUsage().Valid() {
			t.Fatal("answer or trailing metadata lost")
		}
		if len(p.Finish()) != 0 {
			t.Fatal("Finish is not idempotent")
		}
		p.Reset()
		plain, err := p.ParseResponse(testutil.EventFrame("assistantResponseEvent", `{"content":"plain"}`))
		if err != nil || len(plain.GetThinkingBlocks()) != 0 || plain.GetCompletionText() != "plain" {
			t.Fatal("thinking state leaked across reset")
		}
	}
}

func TestInterleavedNativeThinkingAndTools(t *testing.T) {
	var fixture []byte
	for _, event := range [][2]string{
		{"reasoningContentEvent", `{"text":"First check.","signature":"sig1"}`},
		{"assistantResponseEvent", `{"content":"First answer."}`},
		{"reasoningContentEvent", `{"text":"Second check.","signature":"sig2"}`},
		{"toolUseEvent", `{"toolUseId":"tool-1","name":"lookup","input":{"q":"x"},"stop":true}`},
	} {
		fixture = append(fixture, testutil.EventFrame(event[0], event[1])...)
	}
	p := NewCompliantEventStreamParser()
	result, err := p.ParseResponse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	assertBlockLifecycle(t, result.Events)
	if len(result.GetThinkingBlocks()) != 2 {
		t.Fatal("interleaved thinking lost")
	}
	blocks := result.GetNativeContentBlocks()
	foundTool := false
	for _, block := range blocks {
		if block["type"] == "tool_use" && block["id"] == "tool-1" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatal("tool lost when thinking changed indices")
	}
}

func TestRedactedReasoningRemainsOpaque(t *testing.T) {
	p := NewCompliantEventStreamParser()
	fixture := testutil.EventFrame("reasoningContentEvent", `{"redactedContent":"AQIDBA=="}`)
	fixture = append(fixture, testutil.EventFrame("assistantResponseEvent", `{"content":"Done"}`)...)
	result, err := p.ParseResponse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	assertBlockLifecycle(t, result.Events)
	blocks := result.GetThinkingBlocks()
	if len(blocks) != 1 || blocks[0]["type"] != "redacted_thinking" || blocks[0]["data"] != "AQIDBA==" || result.GetCompletionText() != "Done" {
		t.Fatal("redacted reasoning was altered or exposed as text")
	}
}

func TestThinkingTagSplitBoundariesAndLiteralText(t *testing.T) {
	for _, input := range []string{" \n<thinking>Check 中文.</thinking>42", "<think>Check 中文.</think>42"} {
		for size := 1; size < 20; size++ {
			p := NewCompliantEventStreamParser()
			p.SetThinkingTags(true)
			var events []SSEEvent
			// Split only ASCII in real JSON events; split multibyte frame bytes separately above.
			runes := []rune(input)
			for offset := 0; offset < len(runes); offset += size {
				payload, _ := json.Marshal(map[string]string{"content": string(runes[offset:min(len(runes), offset+size)])})
				part, err := p.ParseStream(testutil.EventFrame("assistantResponseEvent", string(payload)))
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, part...)
			}
			events = append(events, p.Finish()...)
			assertBlockLifecycle(t, events)
			result := &ParseResult{Events: events}
			blocks := result.GetThinkingBlocks()
			if len(blocks) != 1 || blocks[0]["thinking"] != "Check 中文." || blocks[0]["thinking_source"] != "kiro_tagged_text" || blocks[0]["signature"] != nil || result.GetCompletionText() != "42" {
				t.Fatalf("tagged thought incorrectly mapped: %v", blocks)
			}
		}
	}
	for _, enabled := range []bool{false, true} {
		for _, input := range []string{"An XML example: <thinking>x</thinking>", "<thinkable>42", strings.Repeat(" ", 70) + "42"} {
			p := NewCompliantEventStreamParser()
			p.SetThinkingTags(enabled)
			payload, _ := json.Marshal(map[string]string{"content": input})
			result, err := p.ParseResponse(testutil.EventFrame("assistantResponseEvent", string(payload)))
			if err != nil || len(result.GetThinkingBlocks()) != 0 || result.GetCompletionText() != input {
				t.Fatal("ordinary text reclassified as thinking")
			}
		}
	}
}
