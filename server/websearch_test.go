package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"kiro2api/types"
	"kiro2api/utils"
)

// Claude Code WebSearch 子请求的形状：只有一个原生 web_search 工具
const claudeCodeWebSearchBody = `{
	"model": "claude-sonnet-4-20250514", "max_tokens": 1024, "stream": %STREAM%,
	"system": [{"type": "text", "text": "You are an assistant for performing a web search tool use"}],
	"messages": [{"role": "user", "content": "Perform a web search for the query: ViewSpatial-Bench dataset"}],
	"tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 8, "blocked_domains": ["spam.example"]}]
}`

func decodeWebSearchRequest(t *testing.T, stream bool) types.AnthropicRequest {
	t.Helper()
	body := strings.Replace(claudeCodeWebSearchBody, "%STREAM%", map[bool]string{true: "true", false: "false"}[stream], 1)
	var req types.AnthropicRequest
	require.NoError(t, utils.SafeUnmarshal([]byte(body), &req))
	return req
}

func fakeKiroMCP(t *testing.T, payload string, status int) *[]map[string]any {
	t.Helper()
	var seen []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/mcp", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "arn:aws:test", r.Header.Get("x-amzn-kiro-profile-arn"))
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		seen = append(seen, body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)
	old := kiroMCPURL
	kiroMCPURL = server.URL + "/mcp"
	t.Cleanup(func() { kiroMCPURL = old })
	return &seen
}

func mcpPayload(t *testing.T, results string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "x",
		"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": results}}, "isError": false},
	})
	require.NoError(t, err)
	return string(payload)
}

const twoResults = `{"results":[
	{"title":"ViewSpatial-Bench","url":"https://huggingface.co/datasets/lidingm/ViewSpatial-Bench","snippet":"A benchmark for multi-view spatial localization.","publishedDate":1747267200000},
	{"title":"spam","url":"https://www.spam.example/x","snippet":"ads"}
]}`

func runWebSearch(t *testing.T, req types.AnthropicRequest) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handleWebSearchRequest(c, req, types.TokenInfo{AccessToken: "test-token", ProfileArn: "arn:aws:test"})
	return w
}

func TestIsPureWebSearchRequest(t *testing.T) {
	req := decodeWebSearchRequest(t, false)
	require.Len(t, req.Tools, 1)
	assert.Equal(t, "web_search_20250305", req.Tools[0].Type)
	assert.True(t, isPureWebSearchRequest(req))
	assert.Equal(t, "ViewSpatial-Bench dataset", extractWebSearchQuery(req))

	// 客户端自定义的同名工具（无 type）和混合工具都不走 MCP
	assert.False(t, isPureWebSearchRequest(types.AnthropicRequest{Tools: []types.AnthropicTool{{Name: "web_search", InputSchema: map[string]any{"type": "object"}}}}))
	req.Tools = append(req.Tools, types.AnthropicTool{Name: "Read"})
	assert.False(t, isPureWebSearchRequest(req))
}

func TestHandleWebSearchRequest_NonStream(t *testing.T) {
	seen := fakeKiroMCP(t, mcpPayload(t, twoResults), http.StatusOK)
	w := runWebSearch(t, decodeWebSearchRequest(t, false))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	require.Len(t, *seen, 1)
	call := (*seen)[0]
	assert.Equal(t, "tools/call", call["method"])
	params := call["params"].(map[string]any)
	assert.Equal(t, "web_search", params["name"])
	assert.Equal(t, "ViewSpatial-Bench dataset", params["arguments"].(map[string]any)["query"])

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	content := resp["content"].([]any)
	require.Len(t, content, 4)
	toolUse := content[1].(map[string]any)
	assert.Equal(t, "server_tool_use", toolUse["type"])
	result := content[2].(map[string]any)
	assert.Equal(t, "web_search_tool_result", result["type"])
	assert.Equal(t, toolUse["id"], result["tool_use_id"])
	links := result["content"].([]any)
	require.Len(t, links, 1, "blocked_domains 应过滤掉 spam.example")
	assert.Equal(t, "https://huggingface.co/datasets/lidingm/ViewSpatial-Bench", links[0].(map[string]any)["url"])
	summary := content[3].(map[string]any)["text"].(string)
	assert.Contains(t, summary, "multi-view spatial localization")
	assert.NotContains(t, summary, "spam.example")
	usage := resp["usage"].(map[string]any)
	assert.Equal(t, float64(1), usage["server_tool_use"].(map[string]any)["web_search_requests"])
}

func TestHandleWebSearchRequest_Stream(t *testing.T) {
	fakeKiroMCP(t, mcpPayload(t, twoResults), http.StatusOK)
	w := runWebSearch(t, decodeWebSearchRequest(t, true))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	for _, want := range []string{"event: message_start", `"type":"server_tool_use"`, `"type":"web_search_tool_result"`, "huggingface.co", "event: message_stop"} {
		assert.Contains(t, body, want)
	}
	assert.Equal(t, 4, strings.Count(body, "event: content_block_stop"))
}

// 搜索失败必须报错，不能返回空结果让模型去编
func TestHandleWebSearchRequest_FailuresAreErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		payload string
		status  int
	}{
		"http 403":      {`{"message":"denied"}`, http.StatusForbidden},
		"rpc error":     {`{"jsonrpc":"2.0","id":"x","error":{"code":-32600,"message":"bad"}}`, http.StatusOK},
		"payload error": {mcpPayload(t, `{"results":[],"error":"quota"}`), http.StatusOK},
		"not json":      {"<html>", http.StatusOK},
		"unparseable":   {mcpPayload(t, "not json"), http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			fakeKiroMCP(t, tc.payload, tc.status)
			w := runWebSearch(t, decodeWebSearchRequest(t, true))
			assert.Equal(t, http.StatusBadGateway, w.Code)
			assert.Contains(t, w.Body.String(), "Web search failed")
			assert.NotContains(t, w.Body.String(), "message_start")
		})
	}
}

func TestFilterWebSearchDomains_Allowed(t *testing.T) {
	results := []webSearchResult{{URL: "https://docs.python.org/3/"}, {URL: "https://python.org"}, {URL: "https://example.com"}}
	kept := filterWebSearchDomains(results, types.AnthropicTool{AllowedDomains: []string{"python.org"}})
	assert.Len(t, kept, 2)
}
