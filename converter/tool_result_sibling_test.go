package converter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"kiro2api/types"
)

// Claude Code 把用户在模型工作时的插话、system-reminder 作为并列文本块放在
// tool_result 后面。之前带 tool_result 的消息 content 被整个清空，这些文本丢失。
func TestBuildCodeWhispererRequest_KeepsTextBesideToolResult(t *testing.T) {
	toolResultTurn := func(text string) []any {
		return []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_01Abc", "content": "file body"},
			map[string]any{"type": "text", "text": text},
		}
	}
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "read a.txt"},
			{Role: "assistant", Content: []any{readToolUse()}},
			{Role: "user", Content: toolResultTurn("stop, read b.txt instead")},
			{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "toolu_02Def", "name": "Read", "input": map[string]any{"file_path": "b.txt"}}}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_02Def", "content": "b body"},
				map[string]any{"type": "text", "text": "<system-reminder>queued note</system-reminder>"},
			}},
		},
	}, nil)
	require.NoError(t, err)

	current := cwReq.ConversationState.CurrentMessage.UserInputMessage
	assert.Equal(t, "<system-reminder>queued note</system-reminder>", current.Content)
	require.Len(t, current.UserInputMessageContext.ToolResults, 1)

	var historyHasInterjection bool
	for _, item := range cwReq.ConversationState.History {
		if msg, ok := item.(types.HistoryUserMessage); ok && len(msg.UserInputMessage.UserInputMessageContext.ToolResults) > 0 {
			assert.Equal(t, "stop, read b.txt instead", msg.UserInputMessage.Content)
			assert.NotContains(t, msg.UserInputMessage.Content, "file body") // 工具输出只在 toolResults 里
			historyHasInterjection = true
		}
	}
	assert.True(t, historyHasInterjection)
}

func TestBuildCodeWhispererRequest_ToolResultOnlyKeepsEmptyContent(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "read a.txt"},
			{Role: "assistant", Content: []any{readToolUse()}},
			{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_01Abc", "content": "file body"}}},
		},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "", cwReq.ConversationState.CurrentMessage.UserInputMessage.Content)
}
