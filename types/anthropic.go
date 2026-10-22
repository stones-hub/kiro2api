package types

// AnthropicTool 表示 Anthropic API 的工具结构
type AnthropicTool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	CacheControl *CacheControl  `json:"cache_control,omitempty"`
	// 以下字段只出现在 Anthropic 服务端工具（如 web_search_20250305）上
	Type           string   `json:"type,omitempty"`
	MaxUses        int      `json:"max_uses,omitempty"`
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
}

// ToolChoice 表示工具选择策略
type ToolChoice struct {
	Type string `json:"type"`           // "auto", "any", "tool"
	Name string `json:"name,omitempty"` // 当type为"tool"时指定的工具名称
}

// AnthropicRequest 表示 Anthropic API 的请求结构
type AnthropicRequest struct {
	Thinking            *ThinkingConfig           `json:"thinking,omitempty"`
	OutputConfig        *OutputConfig             `json:"output_config,omitempty"`
	ReasoningEffort     string                    `json:"reasoning_effort,omitempty"`
	KiroModelFields     *KiroModelRequestFields   `json:"-"`
	KiroThinking        *KiroThinkingStatus       `json:"-"`
	ExtractThinkingTags bool                      `json:"-"`
	Model               string                    `json:"model"`
	MaxTokens           int                       `json:"max_tokens"`
	Messages            []AnthropicRequestMessage `json:"messages"`
	System              AnthropicSystemMessages   `json:"system,omitempty"`
	Tools               []AnthropicTool           `json:"tools,omitempty"`
	ToolChoice          any                       `json:"tool_choice,omitempty"` // 可以是string或ToolChoice对象
	Stream              bool                      `json:"stream"`
	Temperature         *float64                  `json:"temperature,omitempty"`
	Metadata            map[string]any            `json:"metadata,omitempty"`
	CacheControl        *CacheControl             `json:"cache_control,omitempty"`
	IncludeUsage        bool                      `json:"-"` // OpenAI stream_options.include_usage
}

type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type OutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// AnthropicStreamResponse 表示 Anthropic 流式响应的结构
type AnthropicStreamResponse struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentDelta struct {
		Text string `json:"text"`
		Type string `json:"type"`
	} `json:"delta,omitempty"`
	Content []struct {
		Text string `json:"text"`
		Type string `json:"type"`
	} `json:"content,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
}

// AnthropicRequestMessage 表示 Anthropic API 的消息结构
type AnthropicRequestMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // 可以是 string 或 []ContentBlock
}

type AnthropicSystemMessage struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"` // 可以是 string 或 []ContentBlock
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// CacheControl is a prompt-cache hint, not evidence of a cache hit.
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// ContentBlock 表示消息内容块的结构
type ContentBlock struct {
	Type         string        `json:"type"`
	Thinking     string        `json:"thinking,omitempty"`
	Signature    string        `json:"signature,omitempty"`
	Data         string        `json:"data,omitempty"` // Opaque redacted thinking, never decoded as text.
	Text         *string       `json:"text,omitempty"`
	ToolUseId    *string       `json:"tool_use_id,omitempty"`
	Content      any           `json:"content,omitempty"`  // tool_result的内容，可以是string、[]any或map[string]any
	Name         *string       `json:"name,omitempty"`     // tool_use的名称
	Input        *any          `json:"input,omitempty"`    // tool_use的输入参数
	ID           *string       `json:"id,omitempty"`       // tool_use的唯一标识符
	IsError      *bool         `json:"is_error,omitempty"` // tool_result是否表示错误
	Source       *ImageSource  `json:"source,omitempty"`   // 图片数据源
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ImageSource 表示图片数据源的结构
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/jpeg", "image/png", "image/gif", "image/webp"
	Data      string `json:"data"`       // base64编码的图片数据
}
