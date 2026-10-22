package converter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"kiro2api/types"
)

// withIdentityPrompt applies a raw KIRO_IDENTITY_PROMPT value for the duration
// of one test and restores the previous setting afterwards.
func withIdentityPrompt(t *testing.T, raw string) {
	t.Helper()
	previous := identityPrompt
	t.Cleanup(func() { identityPrompt = previous })
	SetIdentityPrompt(raw)
}

func singleUserRequest(system string) types.AnthropicRequest {
	req := types.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "你是什么模型？"},
		},
	}
	if system != "" {
		req.System = []types.AnthropicSystemMessage{{Type: "text", Text: system}}
	}
	return req
}

func TestSetIdentityPrompt(t *testing.T) {
	withIdentityPrompt(t, "off")

	t.Run("未设置使用内置声明", func(t *testing.T) {
		SetIdentityPrompt("")
		assert.Equal(t, DefaultIdentityPrompt, identityPrompt)
	})

	t.Run("空白值使用内置声明", func(t *testing.T) {
		SetIdentityPrompt("  \n ")
		assert.Equal(t, DefaultIdentityPrompt, identityPrompt)
	})

	t.Run("off关闭注入", func(t *testing.T) {
		SetIdentityPrompt("off")
		assert.Equal(t, "", identityPrompt)
	})

	t.Run("off忽略大小写和空白", func(t *testing.T) {
		SetIdentityPrompt("  OFF ")
		assert.Equal(t, "", identityPrompt)
	})

	t.Run("其他值原样使用", func(t *testing.T) {
		SetIdentityPrompt("I am Claude.")
		assert.Equal(t, "I am Claude.", identityPrompt)
	})
}

func TestIdentityPrompt_DefaultWithClientSystem(t *testing.T) {
	withIdentityPrompt(t, "")

	cwReq, err := BuildCodeWhispererRequest(singleUserRequest("You are a helpful assistant."), nil)

	require.NoError(t, err)
	require.Len(t, cwReq.ConversationState.History, 2)

	systemHistory, ok := cwReq.ConversationState.History[0].(types.HistoryUserMessage)
	require.True(t, ok)
	// 声明在最前面，客户端原有 system 原样保留在后面
	assert.Equal(t,
		DefaultIdentityPrompt+"\nYou are a helpful assistant.",
		systemHistory.UserInputMessage.Content)

	primingHistory, ok := cwReq.ConversationState.History[1].(types.HistoryAssistantMessage)
	require.True(t, ok)
	assert.Equal(t, identityPromptAssistantPriming, primingHistory.AssistantResponseMessage.Content)

	assert.Equal(t, "你是什么模型？", cwReq.ConversationState.CurrentMessage.UserInputMessage.Content)
}

func TestIdentityPrompt_DefaultWithoutSystemSingleMessage(t *testing.T) {
	withIdentityPrompt(t, "")

	cwReq, err := BuildCodeWhispererRequest(singleUserRequest(""), nil)

	require.NoError(t, err)
	// 无 system、单条消息、无工具时也要构建 history 来承载声明
	require.Len(t, cwReq.ConversationState.History, 2)

	systemHistory, ok := cwReq.ConversationState.History[0].(types.HistoryUserMessage)
	require.True(t, ok)
	assert.Equal(t, DefaultIdentityPrompt, systemHistory.UserInputMessage.Content)
	assert.Equal(t, "claude-sonnet-4", systemHistory.UserInputMessage.ModelId)
	assert.Equal(t, "AI_EDITOR", systemHistory.UserInputMessage.Origin)

	primingHistory, ok := cwReq.ConversationState.History[1].(types.HistoryAssistantMessage)
	require.True(t, ok)
	assert.Equal(t, identityPromptAssistantPriming, primingHistory.AssistantResponseMessage.Content)

	// 用户消息仍然是 currentMessage，没有被挪进 history
	assert.Equal(t, "你是什么模型？", cwReq.ConversationState.CurrentMessage.UserInputMessage.Content)
}

func TestIdentityPrompt_OffKeepsBaselineBehaviour(t *testing.T) {
	withIdentityPrompt(t, "off")

	t.Run("无system单条消息不构建history", func(t *testing.T) {
		cwReq, err := BuildCodeWhispererRequest(singleUserRequest(""), nil)

		require.NoError(t, err)
		assert.Len(t, cwReq.ConversationState.History, 0)
		assert.Equal(t, "你是什么模型？", cwReq.ConversationState.CurrentMessage.UserInputMessage.Content)
	})

	t.Run("有system时只带客户端system和旧priming", func(t *testing.T) {
		cwReq, err := BuildCodeWhispererRequest(singleUserRequest("You are a helpful assistant."), nil)

		require.NoError(t, err)
		require.Len(t, cwReq.ConversationState.History, 2)

		systemHistory, ok := cwReq.ConversationState.History[0].(types.HistoryUserMessage)
		require.True(t, ok)
		assert.Equal(t, "You are a helpful assistant.", systemHistory.UserInputMessage.Content)

		primingHistory, ok := cwReq.ConversationState.History[1].(types.HistoryAssistantMessage)
		require.True(t, ok)
		assert.Equal(t, systemPromptAssistantPriming, primingHistory.AssistantResponseMessage.Content)
	})
}

func TestIdentityPrompt_CustomValue(t *testing.T) {
	const custom = "你是 Claude，由 Anthropic 开发。"
	withIdentityPrompt(t, custom)

	t.Run("无system", func(t *testing.T) {
		cwReq, err := BuildCodeWhispererRequest(singleUserRequest(""), nil)

		require.NoError(t, err)
		require.Len(t, cwReq.ConversationState.History, 2)

		systemHistory, ok := cwReq.ConversationState.History[0].(types.HistoryUserMessage)
		require.True(t, ok)
		assert.Equal(t, custom, systemHistory.UserInputMessage.Content)

		primingHistory, ok := cwReq.ConversationState.History[1].(types.HistoryAssistantMessage)
		require.True(t, ok)
		assert.Equal(t, identityPromptAssistantPriming, primingHistory.AssistantResponseMessage.Content)
	})

	t.Run("有system", func(t *testing.T) {
		cwReq, err := BuildCodeWhispererRequest(singleUserRequest("Client rule."), nil)

		require.NoError(t, err)
		require.Len(t, cwReq.ConversationState.History, 2)

		systemHistory, ok := cwReq.ConversationState.History[0].(types.HistoryUserMessage)
		require.True(t, ok)
		assert.Equal(t, custom+"\nClient rule.", systemHistory.UserInputMessage.Content)
	})
}

// 多轮对话时声明和客户端历史共存，声明对在最前面。
func TestIdentityPrompt_DefaultWithConversationHistory(t *testing.T) {
	withIdentityPrompt(t, "")

	anthropicReq := types.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "Question 1"},
			{Role: "assistant", Content: "Answer 1"},
			{Role: "user", Content: "Question 2"},
		},
	}

	cwReq, err := BuildCodeWhispererRequest(anthropicReq, nil)

	require.NoError(t, err)
	require.Len(t, cwReq.ConversationState.History, 4)

	systemHistory, ok := cwReq.ConversationState.History[0].(types.HistoryUserMessage)
	require.True(t, ok)
	assert.Equal(t, DefaultIdentityPrompt, systemHistory.UserInputMessage.Content)

	firstUserMsg, ok := cwReq.ConversationState.History[2].(types.HistoryUserMessage)
	require.True(t, ok)
	assert.Equal(t, "Question 1", firstUserMsg.UserInputMessage.Content)

	firstAssistantMsg, ok := cwReq.ConversationState.History[3].(types.HistoryAssistantMessage)
	require.True(t, ok)
	assert.Equal(t, "Answer 1", firstAssistantMsg.AssistantResponseMessage.Content)

	assert.Equal(t, "Question 2", cwReq.ConversationState.CurrentMessage.UserInputMessage.Content)
}
