package converter

import (
	"encoding/json"
	"kiro2api/types"
	"testing"
)

func TestModelAliasIsPreservedAcrossCurrentMessageAndHistory(t *testing.T) {
	request := types.AnthropicRequest{
		Model: "claude-sonnet-4-6", MaxTokens: 16,
		Messages: []types.AnthropicRequestMessage{
			{Role: "user", Content: "previous question"},
			{Role: "assistant", Content: "previous answer"},
			{Role: "user", Content: "current question"},
		},
	}
	upstream, err := BuildCodeWhispererRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := upstream.ConversationState.CurrentMessage.UserInputMessage.ModelId; got != "claude-sonnet-4.6" {
		t.Fatalf("current message model = %q", got)
	}
	data, err := json.Marshal(upstream.ConversationState.History)
	if err != nil {
		t.Fatal(err)
	}
	var history []struct {
		User *struct {
			Model string `json:"modelId"`
		} `json:"userInputMessage"`
	}
	if err := json.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, event := range history {
		if event.User != nil {
			users++
			if event.User.Model != "claude-sonnet-4.6" {
				t.Fatalf("history model = %q", event.User.Model)
			}
		}
	}
	if users == 0 {
		t.Fatal("test request produced no historical user message")
	}
}

func TestUnavailableHaikuCannotBecomeAuto(t *testing.T) {
	_, err := BuildCodeWhispererRequest(types.AnthropicRequest{
		Model: "claude-3-5-haiku-20241022", MaxTokens: 16,
		Messages: []types.AnthropicRequestMessage{{Role: "user", Content: "hello"}},
	}, nil)
	if _, ok := err.(*types.ModelNotFoundErrorType); !ok {
		t.Fatalf("expected model_not_found, got %v", err)
	}
}
