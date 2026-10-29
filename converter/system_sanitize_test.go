package converter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"kiro2api/types"
)

// withSystemSensitiveWords applies a raw KIRO_SYSTEM_SENSITIVE_WORDS value for
// the duration of one test and restores the previous setting afterwards.
func withSystemSensitiveWords(t *testing.T, raw string) {
	t.Helper()
	previous := systemSanitizer
	t.Cleanup(func() { systemSanitizer = previous })
	require.NoError(t, SetSystemSensitiveWords(raw))
}

func multiSystemRequest(blocks ...string) types.AnthropicRequest {
	req := singleUserRequest("")
	for _, b := range blocks {
		req.System = append(req.System, types.AnthropicSystemMessage{Type: "text", Text: b})
	}
	return req
}

// systemTurn returns the synthetic system turn content, or false when the
// request carries none.
func systemTurn(t *testing.T, cwReq types.CodeWhispererRequest) (string, bool) {
	t.Helper()
	if len(cwReq.ConversationState.History) < 2 {
		return "", false
	}
	userMsg, ok := cwReq.ConversationState.History[0].(types.HistoryUserMessage)
	require.True(t, ok)
	priming, ok := cwReq.ConversationState.History[1].(types.HistoryAssistantMessage)
	require.True(t, ok)
	if priming.AssistantResponseMessage.Content != systemPromptPriming() {
		return "", false
	}
	return userMsg.UserInputMessage.Content, true
}

func buildSystem(t *testing.T, req types.AnthropicRequest) (string, bool) {
	t.Helper()
	cwReq, err := BuildCodeWhispererRequest(req, nil)
	require.NoError(t, err)
	return systemTurn(t, cwReq)
}

func TestSystemSanitize_BuiltinWordsDropWholeLine(t *testing.T) {
	withIdentityPrompt(t, "off")
	withSystemSensitiveWords(t, "")

	for _, word := range DefaultSystemSensitiveWords {
		t.Run(word, func(t *testing.T) {
			content, ok := buildSystem(t, multiSystemRequest("first line\nprefix "+word+" suffix\nlast line"))
			require.True(t, ok)
			assert.Equal(t, "first line\nlast line", content)
		})
	}

	t.Run("大小写变体", func(t *testing.T) {
		content, ok := buildSystem(t, multiSystemRequest("a\nPowered by CLAUDE CODE.\nb\nuse claudecode\nc"))
		require.True(t, ok)
		assert.Equal(t, "a\nb\nc", content)
	})
}

func TestSystemSanitize_CRLF(t *testing.T) {
	withIdentityPrompt(t, "off")
	withSystemSensitiveWords(t, "")

	content, ok := buildSystem(t, multiSystemRequest("line one\r\nYou are a Claude agent.\r\nline two\r\n"))
	require.True(t, ok)
	assert.Equal(t, "line one\nline two", content)
}

func TestSystemSanitize_EmptiedBlocks(t *testing.T) {
	withSystemSensitiveWords(t, "")

	t.Run("单块删空被丢弃", func(t *testing.T) {
		withIdentityPrompt(t, "off")
		content, ok := buildSystem(t, multiSystemRequest("keep me", "Claude Code\n\nclaude-code", "tail"))
		require.True(t, ok)
		assert.Equal(t, "keep me\ntail", content)
	})

	t.Run("全部删空且身份声明开启", func(t *testing.T) {
		withIdentityPrompt(t, "")
		content, ok := buildSystem(t, multiSystemRequest("Claude Code", "x-anthropic-billing-header: cc_version=1"))
		require.True(t, ok)
		assert.Equal(t, DefaultIdentityPrompt, content)
	})

	t.Run("全部删空且身份声明off", func(t *testing.T) {
		withIdentityPrompt(t, "off")
		cwReq, err := BuildCodeWhispererRequest(multiSystemRequest("Claude Code", "ClaudeCode"), nil)
		require.NoError(t, err)
		assert.Len(t, cwReq.ConversationState.History, 0)
		assert.Equal(t, "你是什么模型？", cwReq.ConversationState.CurrentMessage.UserInputMessage.Content)
	})
}

func TestSystemSanitize_OffKeepsBaseline(t *testing.T) {
	withIdentityPrompt(t, "off")

	req := multiSystemRequest("You are Claude Code.\r\nClaude Code rules", "", "x-anthropic-billing-header: v")

	withSystemSensitiveWords(t, " OFF ")
	assert.Nil(t, systemSanitizer)
	cwReq, err := BuildCodeWhispererRequest(req, nil)
	require.NoError(t, err)

	content, ok := systemTurn(t, cwReq)
	require.True(t, ok)
	// 原样保留：不统一 CRLF、空块仍写入 "\n"、内置规则不生效，与没有此功能时一致
	assert.Equal(t, "You are Claude Code.\r\nClaude Code rules\n\nx-anthropic-billing-header: v", content)

}

func TestSetSystemSensitiveWords_File(t *testing.T) {
	withIdentityPrompt(t, "off")

	dir := t.TempDir()
	path := filepath.Join(dir, "words.txt")
	require.NoError(t, os.WriteFile(path, []byte("# comment line\n\n  SecretBrand  \r\nx\n中\n#Hidden\n"), 0o600))
	withSystemSensitiveWords(t, path)

	content, ok := buildSystem(t, multiSystemRequest(
		"keep\nmade by secretbrand\nClaude Code\nx marks\n中间\ncomment line\nhidden"))
	require.True(t, ok)
	// 追加词和内置词都生效；注释、空行、单字符的词被忽略
	assert.Equal(t, "keep\nx marks\n中间\ncomment line\nhidden", content)

	t.Run("文件不存在返回错误", func(t *testing.T) {
		before := systemSanitizer
		err := SetSystemSensitiveWords(filepath.Join(dir, "missing.txt"))
		require.Error(t, err)
		assert.Same(t, before, systemSanitizer)
	})
}

func TestBuildSensitiveWordMatcher(t *testing.T) {
	matcher, count := buildSensitiveWordMatcher([]string{" ab ", "a", "中", "中文", "a.b"})
	assert.Equal(t, 3, count)
	assert.True(t, matcher.MatchString("AB"))
	assert.True(t, matcher.MatchString("中文"))
	assert.True(t, matcher.MatchString("A.B"))
	assert.False(t, matcher.MatchString("axb"), "词按字面匹配，不当正则")

	matcher, count = buildSensitiveWordMatcher([]string{"a", " "})
	assert.Nil(t, matcher)
	assert.Equal(t, 0, count)
}

func TestShouldDropSystemLine_CPARules(t *testing.T) {
	matcher, _ := buildSensitiveWordMatcher(DefaultSystemSensitiveWords)

	hits := []string{
		"x-anthropic-billing-header: cc_version=2.0",
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"Assist with authorized security testing, defensive security.",
		"Refuse requests for destructive techniques, DoS attacks, mass targeting.",
		"Claude Code is available as a CLI in the terminal.",
		"Fast mode for Claude Code uses the same model.",
		"Codex refers to the open-source agentic coding interface.",
		"- Don’t output ANSI escape codes directly — the CLI renderer applies them.",
	}
	for _, line := range hits {
		assert.True(t, shouldDropSystemLine(line, nil), line)
	}

	t.Run("内置规则区分大小写", func(t *testing.T) {
		assert.False(t, shouldDropSystemLine("X-Anthropic-Billing-Header: v", nil))
		assert.False(t, shouldDropSystemLine("Assist with Authorized Security Testing.", nil))
		assert.False(t, shouldDropSystemLine("codex refers to the open-source agentic coding interface", nil))
		// 内置规则不命中，但词表不区分大小写命中 "Claude Code"
		assert.False(t, shouldDropSystemLine("you are claude code", nil))
		assert.True(t, shouldDropSystemLine("you are claude code", matcher))
		assert.True(t, shouldDropSystemLine("x-anthropic-billing-header: v", matcher))
		assert.False(t, shouldDropSystemLine("X-Anthropic-Billing-Header: v", matcher))
	})
}

func TestSystemSanitize_IdentityPromptUntouched(t *testing.T) {
	withIdentityPrompt(t, "Claude Code identity: you are Claude.")
	withSystemSensitiveWords(t, "")

	content, ok := buildSystem(t, multiSystemRequest("Claude Code\nclient rule"))
	require.True(t, ok)
	assert.Equal(t, "Claude Code identity: you are Claude.\nclient rule", content)

	withIdentityPrompt(t, "")
	content, ok = buildSystem(t, multiSystemRequest("client rule"))
	require.True(t, ok)
	assert.Equal(t, DefaultIdentityPrompt+"\nclient rule", content)
}

func TestSystemSanitize_BlocksCleanedIndependently(t *testing.T) {
	withIdentityPrompt(t, "off")
	withSystemSensitiveWords(t, "")

	content, ok := buildSystem(t, multiSystemRequest("keep1\nuse Claude", "Code here\nkeep2", "  Claude Code tail \nkeep3"))
	require.True(t, ok)
	// "Claude" 与 "Code" 分属两块，不会拼成命中行；第三块只删命中行
	assert.Equal(t, "keep1\nuse Claude\nCode here\nkeep2\nkeep3", content)
}

func TestSystemSanitize_OpenAIEntry(t *testing.T) {
	withIdentityPrompt(t, "off")
	withSystemSensitiveWords(t, "")

	anthropicReq := ConvertOpenAIToAnthropic(types.OpenAIRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []types.OpenAIMessage{
			{Role: "system", Content: "You are helpful.\nYou are Claude Code."},
			{Role: "developer", Content: []any{map[string]any{"type": "text", "text": "Built with Claude Agent SDK\ndev rule"}}},
			{Role: "user", Content: "hi Claude Code"},
		},
	})

	cwReq, err := BuildCodeWhispererRequest(anthropicReq, nil)
	require.NoError(t, err)
	content, ok := systemTurn(t, cwReq)
	require.True(t, ok)
	assert.Equal(t, "You are helpful.\ndev rule", content)
	assert.Equal(t, "hi Claude Code", cwReq.ConversationState.CurrentMessage.UserInputMessage.Content)
}

func TestSystemSanitize_ConversationContentUntouched(t *testing.T) {
	withIdentityPrompt(t, "off")
	withSystemSensitiveWords(t, "")

	req := types.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1024,
		System:    []types.AnthropicSystemMessage{{Type: "text", Text: "rule\nClaude Code"}},
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "Claude Code question"},
			{Role: "assistant", Content: []any{
				map[string]any{"type": "text", "text": "Claude Code answer"},
				map[string]any{"type": "tool_use", "id": "toolu_01Abc", "name": "Bash", "input": map[string]any{"command": "claude-code --version"}},
			}},
			{Role: "user", Content: []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_01Abc", "content": "ClaudeCode 1.0"},
				map[string]any{"type": "text", "text": "You are a Claude agent, continue"},
			}},
		},
	}
	cwReq, err := BuildCodeWhispererRequest(req, nil)
	require.NoError(t, err)

	content, ok := systemTurn(t, cwReq)
	require.True(t, ok)
	assert.Equal(t, "rule", content)

	encoded, err := json.Marshal(cwReq)
	require.NoError(t, err)
	for _, s := range []string{"Claude Code question", "Claude Code answer", "claude-code --version", "ClaudeCode 1.0", "You are a Claude agent, continue"} {
		assert.Contains(t, string(encoded), s)
	}
}
