package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/config"
	"kiro2api/logger"
	"kiro2api/types"
	"kiro2api/utils"
)

// Anthropic 的 web_search 是服务端工具，Kiro 的 generateAssistantResponse 不执行它。
// 以前这里把工具静默过滤掉，Claude Code 的 WebSearch 子请求于是拿不到工具，
// 模型只能凭记忆编出“搜索结果”。现在纯 web_search 请求（Claude Code / Desktop
// 的 WebSearch 就是这种）改走 Kiro 的 MCP 接口真实搜索，再拼成 Anthropic 的
// server_tool_use + web_search_tool_result 响应。参考 Kiro-Go proxy/websearch.go。

const (
	webSearchToolName    = "web_search"
	webSearchQueryPrefix = "Perform a web search for the query:"
	webSearchTimeout     = 60 * time.Second
	webSearchSnippetMax  = 400
)

// kiroMCPURL 与 generateAssistantResponse 同一主机；测试可替换
var kiroMCPURL = strings.TrimSuffix(config.CodeWhispererURL, "/generateAssistantResponse") + "/mcp"

func isNativeWebSearchTool(tool types.AnthropicTool) bool {
	return tool.Name == webSearchToolName && strings.HasPrefix(strings.TrimSpace(tool.Type), "web_search_")
}

// isPureWebSearchRequest 判断请求是否只带一个原生 web_search 工具
func isPureWebSearchRequest(req types.AnthropicRequest) bool {
	return len(req.Tools) == 1 && isNativeWebSearchTool(req.Tools[0])
}

// extractWebSearchQuery 取最后一条 user 消息的文本，去掉 Claude Code 的固定前缀
func extractWebSearchQuery(req types.AnthropicRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if msg.Role != "user" {
			continue
		}
		var text string
		switch c := msg.Content.(type) {
		case string:
			text = c
		case []any:
			for j := len(c) - 1; j >= 0; j-- {
				if block, ok := c[j].(map[string]any); ok && block["type"] == "text" {
					if t, _ := block["text"].(string); strings.TrimSpace(t) != "" {
						text = t
						break
					}
				}
			}
		case []types.ContentBlock:
			for j := len(c) - 1; j >= 0; j-- {
				if c[j].Type == "text" && c[j].Text != nil && strings.TrimSpace(*c[j].Text) != "" {
					text = *c[j].Text
					break
				}
			}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(text, webSearchQueryPrefix))
	}
	return ""
}

type mcpResponse struct {
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Result *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
}

type webSearchResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet"`
	PublishedDate *int64 `json:"publishedDate"`
}

type webSearchResults struct {
	Results []webSearchResult `json:"results"`
	Error   string            `json:"error"`
}

// callKiroWebSearch 通过 Kiro MCP 的 tools/call 执行 web_search
func callKiroWebSearch(c *gin.Context, token types.TokenInfo, query string) (*webSearchResults, error) {
	body, err := json.Marshal(map[string]any{
		"id":      "web_search_" + randomHex(12),
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params":  map[string]any{"name": webSearchToolName, "arguments": map[string]any{"query": query}},
	})
	if err != nil {
		return nil, err
	}
	// 派生自请求上下文，保留其中的账号 HTTP 客户端（走账号代理）
	ctx, cancel := context.WithTimeout(c.Request.Context(), webSearchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroMCPURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.34 KiroIDE-0.11.107")
	req.Header.Set("user-agent", "aws-sdk-js/1.0.34 ua/2.1 api/codewhispererstreaming#1.0.34 m/E KiroIDE-0.11.107")
	if token.ProfileArn != "" {
		req.Header.Set("x-amzn-kiro-profile-arn", token.ProfileArn)
	}

	resp, err := utils.DoRequest(req)
	if err != nil {
		if utils.IsProxyError(err) {
			recordAccountProxyError(c)
			return nil, fmt.Errorf("账号代理连接失败")
		}
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, truncate(string(raw)))
	}

	var rpc mcpResponse
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return nil, fmt.Errorf("MCP 响应不是 JSON: %v", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	if rpc.Result == nil || rpc.Result.IsError || len(rpc.Result.Content) == 0 || rpc.Result.Content[0].Type != "text" {
		return nil, fmt.Errorf("MCP web_search 返回了无法识别的结果: %s", truncate(string(raw)))
	}
	var results webSearchResults
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &results); err != nil {
		return nil, fmt.Errorf("搜索结果无法解析: %v", err)
	}
	if strings.TrimSpace(results.Error) != "" {
		return nil, fmt.Errorf("搜索失败: %s", results.Error)
	}
	return &results, nil
}

// filterWebSearchDomains 应用 allowed_domains / blocked_domains（Kiro 搜索本身不支持）
func filterWebSearchDomains(results []webSearchResult, tool types.AnthropicTool) []webSearchResult {
	if len(tool.AllowedDomains) == 0 && len(tool.BlockedDomains) == 0 {
		return results
	}
	matches := func(host string, domains []string) bool {
		for _, d := range domains {
			d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "*."))
			if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
				return true
			}
		}
		return false
	}
	var out []webSearchResult
	for _, r := range results {
		u, err := url.Parse(r.URL)
		if err != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if len(tool.AllowedDomains) > 0 && !matches(host, tool.AllowedDomains) {
			continue
		}
		if matches(host, tool.BlockedDomains) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func webSearchSummary(query string, results []webSearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Here are the web search results for %q:\n\n", query)
	if len(results) == 0 {
		b.WriteString("No results found.\n")
	}
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n", i+1, r.Title)
		if snippet := strings.TrimSpace(r.Snippet); snippet != "" {
			if runes := []rune(snippet); len(runes) > webSearchSnippetMax {
				snippet = string(runes[:webSearchSnippetMax]) + "..."
			}
			fmt.Fprintf(&b, "   %s\n", snippet)
		}
		fmt.Fprintf(&b, "   Source: %s\n\n", r.URL)
	}
	b.WriteString("These are raw search snippets; open a source URL to verify details before relying on them.")
	return b.String()
}

func webSearchResultBlocks(results []webSearchResult) []map[string]any {
	blocks := make([]map[string]any, 0, len(results))
	for _, r := range results {
		var pageAge any
		if r.PublishedDate != nil {
			pageAge = time.UnixMilli(*r.PublishedDate).UTC().Format("January 2, 2006")
		}
		blocks = append(blocks, map[string]any{
			"type":              "web_search_result",
			"title":             r.Title,
			"url":               r.URL,
			"encrypted_content": r.Snippet,
			"page_age":          pageAge,
		})
	}
	return blocks
}

// handleWebSearchRequest 执行纯 web_search 请求，按 Anthropic 格式返回流式或非流式响应。
// 搜索失败时返回错误，绝不返回空结果让模型去编。
func handleWebSearchRequest(c *gin.Context, req types.AnthropicRequest, token types.TokenInfo) {
	query := extractWebSearchQuery(req)
	if query == "" {
		respondError(c, http.StatusBadRequest, "%s", "无法从消息中提取搜索词")
		return
	}
	results, err := callKiroWebSearch(c, token, query)
	if err != nil {
		logger.Warn("WebSearch 失败", addReqFields(c, logger.Err(err))...)
		respondError(c, http.StatusBadGateway, "Web search failed: %v", err)
		return
	}
	filtered := filterWebSearchDomains(results.Results, req.Tools[0])
	recordAccountUpstream(c, http.StatusOK)

	summary := webSearchSummary(query, filtered)
	inputTokens := utils.NewTokenEstimator().EstimateTokens(&types.CountTokensRequest{Model: req.Model, System: req.System, Messages: req.Messages})
	outputTokens := max(1, len([]rune(summary))/4)
	toolUseID := "srvtoolu_" + randomHex(16)
	messageID := "msg_" + randomHex(12)
	usage := map[string]any{
		"input_tokens":                inputTokens,
		"output_tokens":               outputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"server_tool_use":             map[string]any{"web_search_requests": 1},
	}
	recordAccountUsage(c, usage)

	intro := fmt.Sprintf("I'll search for %q.", query)
	content := []map[string]any{
		{"type": "text", "text": intro},
		{"type": "server_tool_use", "id": toolUseID, "name": webSearchToolName, "input": map[string]any{"query": query}},
		{"type": "web_search_tool_result", "tool_use_id": toolUseID, "content": webSearchResultBlocks(filtered)},
		{"type": "text", "text": summary},
	}
	logger.Info("WebSearch 完成", addReqFields(c, logger.Int("results", len(results.Results)), logger.Int("kept", len(filtered)))...)

	if !req.Stream {
		c.JSON(http.StatusOK, map[string]any{
			"id": messageID, "type": "message", "role": "assistant", "model": req.Model,
			"content": content, "stop_reason": "end_turn", "stop_sequence": nil, "usage": usage,
		})
		recordAccountSuccess(c)
		return
	}

	if err := initializeSSEResponse(c); err != nil {
		respondError(c, http.StatusInternalServerError, "%s", "连接不支持SSE刷新")
		return
	}
	sender := &AnthropicStreamSender{}
	send := func(data map[string]any) bool { return sender.SendEvent(c, data) == nil }
	ok := send(map[string]any{"type": "message_start", "message": map[string]any{
		"id": messageID, "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	}})
	recordAccountFirstByte(c)
	for i, block := range content {
		if !ok {
			return
		}
		if block["type"] == "text" {
			ok = send(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}}) &&
				send(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": block["text"]}})
		} else {
			ok = send(map[string]any{"type": "content_block_start", "index": i, "content_block": block})
		}
		ok = ok && send(map[string]any{"type": "content_block_stop", "index": i})
	}
	ok = ok && send(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens, "server_tool_use": map[string]any{"web_search_requests": 1}}})
	ok = ok && send(map[string]any{"type": "message_stop"})
	if ok && c.Request.Context().Err() == nil {
		recordAccountSuccess(c)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
