package types

// KiroTokenUsage is metadataEvent.tokenUsage from AWS's streaming SDK.
// Pointer fields preserve the distinction between unavailable counts and zero.
type KiroTokenUsage struct {
	UncachedInputTokens   *int `json:"uncachedInputTokens"`
	OutputTokens          *int `json:"outputTokens"`
	TotalTokens           *int `json:"totalTokens"`
	CacheReadInputTokens  *int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens *int `json:"cacheWriteInputTokens"`
}

func (u *KiroTokenUsage) Valid() bool {
	if u == nil || u.UncachedInputTokens == nil || u.OutputTokens == nil || u.TotalTokens == nil {
		return false
	}
	for _, count := range []*int{u.UncachedInputTokens, u.OutputTokens, u.TotalTokens, u.CacheReadInputTokens, u.CacheWriteInputTokens} {
		if count != nil && (*count < 0 || *count > 2147483647) {
			return false
		}
	}
	return true
}

// AnthropicUsage uses upstream accounting when supplied, otherwise the existing
// estimates. It never derives cached tokens from prompt lengths or credits.
func (u *KiroTokenUsage) AnthropicUsage(inputEstimate, outputEstimate int) map[string]any {
	usage := map[string]any{"input_tokens": inputEstimate, "output_tokens": outputEstimate}
	if !u.Valid() {
		return usage
	}
	usage["input_tokens"] = *u.UncachedInputTokens
	usage["output_tokens"] = *u.OutputTokens
	if u.CacheReadInputTokens != nil {
		usage["cache_read_input_tokens"] = *u.CacheReadInputTokens
	}
	if u.CacheWriteInputTokens != nil {
		usage["cache_creation_input_tokens"] = *u.CacheWriteInputTokens
	}
	return usage
}
