package types

// Model 表示模型信息
type Model struct {
	ID               string   `json:"id"`
	Object           string   `json:"object"`
	Created          int64    `json:"created"`
	OwnedBy          string   `json:"owned_by"`
	DisplayName      string   `json:"display_name"`
	Type             string   `json:"type"`
	MaxTokens        int      `json:"max_tokens"`
	MaxOutputTokens  int      `json:"max_output_tokens,omitempty"`
	ThinkingMode     string   `json:"thinking_mode,omitempty"`
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
}

// ModelsResponse 表示模型列表响应
type ModelsResponse struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
