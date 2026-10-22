package converter

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"kiro2api/types"
	"kiro2api/utils"
)

// 1x1 PNG
const testPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"

func testImageBlock() map[string]any {
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": "image/png",
			"data":       testPNGBase64,
		},
	}
}

func readToolUse() map[string]any {
	return map[string]any{"type": "tool_use", "id": "toolu_01Abc", "name": "Read", "input": map[string]any{"file_path": "/tmp/a.png"}}
}

func readToolResultWithImage() map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": "toolu_01Abc", "content": []any{testImageBlock()}}
}

func historyUser(t *testing.T, item any) types.HistoryUserMessage {
	t.Helper()
	msg, ok := item.(types.HistoryUserMessage)
	require.True(t, ok, "expected user history message, got %T", item)
	return msg
}

func TestBuildCodeWhispererRequest_DirectImage(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: []any{testImageBlock(), map[string]any{"type": "text", "text": "color?"}}},
		},
	}, nil)
	require.NoError(t, err)

	current := cwReq.ConversationState.CurrentMessage.UserInputMessage
	assert.Equal(t, "color?", current.Content)
	require.Len(t, current.Images, 1)
	assert.Equal(t, "png", current.Images[0].Format)
	assert.Equal(t, testPNGBase64, current.Images[0].Source.Bytes)
}

func TestBuildCodeWhispererRequest_ImageOnlyCurrentMessage(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{{Role: "user", Content: []any{testImageBlock()}}},
	}, nil)
	require.NoError(t, err)

	current := cwReq.ConversationState.CurrentMessage.UserInputMessage
	assert.Equal(t, imageOnlyUserContent, current.Content)
	assert.Len(t, current.Images, 1)
}

// Kiro 拒绝空 content 的历史 user 消息（Improperly formed request），且图片不能丢
func TestBuildCodeWhispererRequest_ImageOnlyHistoryMessage(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: []any{testImageBlock()}},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "Got it."}}},
			{Role: "user", Content: "color?"},
		},
	}, nil)
	require.NoError(t, err)

	require.Len(t, cwReq.ConversationState.History, 2)
	user := historyUser(t, cwReq.ConversationState.History[0])
	assert.Equal(t, imageOnlyUserContent, user.UserInputMessage.Content)
	assert.Len(t, user.UserInputMessage.Images, 1)
}

// Claude Code 的 Read 工具读取图片时，图片位于 tool_result.content 中
func TestBuildCodeWhispererRequest_ToolResultImage(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "Read /tmp/a.png"},
			{Role: "assistant", Content: []any{readToolUse()}},
			{Role: "user", Content: []any{readToolResultWithImage()}},
		},
	}, nil)
	require.NoError(t, err)

	current := cwReq.ConversationState.CurrentMessage.UserInputMessage
	require.Len(t, current.Images, 1)
	assert.Equal(t, testPNGBase64, current.Images[0].Source.Bytes)

	toolResults := current.UserInputMessageContext.ToolResults
	require.Len(t, toolResults, 1)
	assert.Equal(t, []map[string]any{{"text": utils.ToolResultImagePlaceholder}}, toolResults[0].Content)
}

func TestBuildCodeWhispererRequest_ToolResultImageInHistory(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "Read /tmp/a.png"},
			{Role: "assistant", Content: []any{readToolUse()}},
			{Role: "user", Content: []any{readToolResultWithImage()}},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "I've read the file."}}},
			{Role: "user", Content: "color?"},
		},
	}, nil)
	require.NoError(t, err)

	require.Len(t, cwReq.ConversationState.History, 4)
	user := historyUser(t, cwReq.ConversationState.History[2])
	assert.Len(t, user.UserInputMessage.Images, 1)
	require.Len(t, user.UserInputMessage.UserInputMessageContext.ToolResults, 1)
	assert.Equal(t, []map[string]any{{"text": utils.ToolResultImagePlaceholder}},
		user.UserInputMessage.UserInputMessageContext.ToolResults[0].Content)
}

// tool_result 中的无效图片会一直留在会话历史里，只能跳过，不能让请求失败
func TestBuildCodeWhispererRequest_InvalidToolResultImageSkipped(t *testing.T) {
	badImage := map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "base64", "media_type": "image/png", "data": "not base64!"},
	}
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "Read /tmp/a.png"},
			{Role: "assistant", Content: []any{readToolUse()}},
			{Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_01Abc", "content": []any{badImage}}}},
		},
	}, nil)
	require.NoError(t, err)

	current := cwReq.ConversationState.CurrentMessage.UserInputMessage
	assert.Empty(t, current.Images)
	assert.Len(t, current.UserInputMessageContext.ToolResults, 1)
}

func TestParseToolResultContent_ImageNotSerialized(t *testing.T) {
	text := utils.ParseToolResultContent([]any{
		map[string]any{"type": "text", "text": "file info"},
		testImageBlock(),
	})
	assert.NotContains(t, text, testPNGBase64)
	assert.Contains(t, text, utils.ToolResultImagePlaceholder)
}

func testPDFDocumentBlock(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../utils/testdata/macos-cjk.pdf")
	require.NoError(t, err)
	return map[string]any{
		"type":   "document",
		"source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": base64.StdEncoding.EncodeToString(raw)},
	}
}

// Claude Code 读 PDF：tool_result 只有一行提示，document 块与其并列。之前 document 被丢弃。
func TestBuildCodeWhispererRequest_ReadPDFSiblingDocument(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "read test.pdf"},
			{Role: "assistant", Content: []any{readToolUse()}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_01Abc", "content": "PDF file read: test.pdf (16.7KB)"},
				testPDFDocumentBlock(t),
			}},
		},
	}, nil)
	require.NoError(t, err)

	current := cwReq.ConversationState.CurrentMessage.UserInputMessage
	assert.Contains(t, current.Content, "Second line: 42 apples")
	assert.Contains(t, current.Content, "测试：随机答案")
	require.Len(t, current.UserInputMessageContext.ToolResults, 1)
}

// document 放在 tool_result 内部时，Kiro 的 toolResults 也要拿到文本而不是原始块
func TestBuildCodeWhispererRequest_ReadPDFInsideToolResult(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "read test.pdf"},
			{Role: "assistant", Content: []any{readToolUse()}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_01Abc", "content": []any{testPDFDocumentBlock(t)}},
			}},
		},
	}, nil)
	require.NoError(t, err)

	results := cwReq.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults
	require.Len(t, results, 1)
	require.Len(t, results[0].Content, 1)
	text, _ := results[0].Content[0]["text"].(string)
	assert.Contains(t, text, "Second line: 42 apples")
	_, hasSource := results[0].Content[0]["source"]
	assert.False(t, hasSource)
}

// 粘贴进对话框的 PDF 在历史消息里也要保留内容
func TestBuildCodeWhispererRequest_PastedPDFInHistory(t *testing.T) {
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: []any{testPDFDocumentBlock(t), map[string]any{"type": "text", "text": "summarize"}}},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "Done."}}},
			{Role: "user", Content: "what number?"},
		},
	}, nil)
	require.NoError(t, err)

	var found bool
	for _, item := range cwReq.ConversationState.History {
		if msg, ok := item.(types.HistoryUserMessage); ok && strings.Contains(msg.UserInputMessage.Content, "7391") {
			found = true
		}
	}
	assert.True(t, found, "history should contain the PDF text")
}

// 声明 jpeg 实为 png：按实际格式发送，而不是让请求失败
func TestBuildCodeWhispererRequest_ImageMediaTypeCorrected(t *testing.T) {
	block := testImageBlock()
	block["source"].(map[string]any)["media_type"] = "image/jpeg"
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{{Role: "user", Content: []any{block, map[string]any{"type": "text", "text": "color?"}}}},
	}, nil)
	require.NoError(t, err)
	current := cwReq.ConversationState.CurrentMessage.UserInputMessage
	require.Len(t, current.Images, 1)
	assert.Equal(t, "png", current.Images[0].Format)
}

// 无效图片在历史里只跳过图片本身，同一消息的文字必须保留（之前整条消息内容被丢弃）
func TestBuildCodeWhispererRequest_InvalidImageKeepsHistoryText(t *testing.T) {
	bad := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "!!not-base64!!"}}
	cwReq, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: []any{bad, map[string]any{"type": "text", "text": "remember 4821"}}},
			{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "ok"}}},
			{Role: "user", Content: []any{bad, map[string]any{"type": "text", "text": "number?"}}},
		},
	}, nil)
	require.NoError(t, err)
	first := historyUser(t, cwReq.ConversationState.History[0])
	assert.Contains(t, first.UserInputMessage.Content, "remember 4821")
	assert.Contains(t, first.UserInputMessage.Content, "Image omitted by proxy")
	current := cwReq.ConversationState.CurrentMessage.UserInputMessage
	assert.Contains(t, current.Content, "number?")
	assert.Empty(t, current.Images)
}
