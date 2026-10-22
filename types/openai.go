package types

// OpenAI兼容的数据结构
type OpenAIMessage struct {
	Role               string           `json:"role"`
	ReasoningContent   string           `json:"reasoning_content,omitempty"`
	ReasoningSignature string           `json:"reasoning_signature,omitempty"`
	ReasoningSource    string           `json:"reasoning_source,omitempty"`
	Content            any              `json:"content"` // 可以是 string 或 []ContentBlock
	ToolCalls          []OpenAIToolCall `json:"tool_calls,omitempty"`
}

type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

type OpenAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      *bool          `json:"strict,omitempty"`
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

// OpenAIToolChoice 表示OpenAI格式的工具选择策略
type OpenAIToolChoice struct {
	Type     string                    `json:"type"`               // "function"
	Function *OpenAIToolChoiceFunction `json:"function,omitempty"` // 当指定具体函数时
}

type OpenAIToolChoiceFunction struct {
	Name string `json:"name"`
}

type OpenAIRequest struct {
	ReasoningEffort     string               `json:"reasoning_effort,omitempty"`
	Thinking            *ThinkingConfig      `json:"thinking,omitempty"`
	OutputConfig        *OutputConfig        `json:"output_config,omitempty"`
	Model               string               `json:"model"`
	Messages            []OpenAIMessage      `json:"messages"`
	MaxTokens           *int                 `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                 `json:"max_completion_tokens,omitempty"`
	Temperature         *float64             `json:"temperature,omitempty"`
	Stream              *bool                `json:"stream,omitempty"`
	Tools               []OpenAITool         `json:"tools,omitempty"`
	ToolChoice          any                  `json:"tool_choice,omitempty"` // 可以是 "auto", "none", "required" 或 OpenAIToolChoice
	StreamOptions       *OpenAIStreamOptions `json:"stream_options,omitempty"`
}

type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   Usage          `json:"usage"`
}
