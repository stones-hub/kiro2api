package types_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"kiro2api/types"
	"kiro2api/utils"
)

func TestSystemUnionAcrossMessagesAndCountTokensDecoders(t *testing.T) {
	for decoder, unmarshal := range map[string]func([]byte, any) error{"standard": json.Unmarshal, "production_sonic": utils.SafeUnmarshal} {
		for _, tc := range []struct {
			name, field string
			want        []types.AnthropicSystemMessage
		}{
			{"string", `,"system":"  System: \"测试\"\nline 2\\path  "`, []types.AnthropicSystemMessage{{Type: "text", Text: "  System: \"测试\"\nline 2\\path  "}}},
			{"blocks", `,"system":[{"type":"text","text":"first"},{"type":"text","text":"second","cache_control":{"type":"ephemeral","ttl":"5m"}}]`, []types.AnthropicSystemMessage{{Type: "text", Text: "first"}, {Type: "text", Text: "second", CacheControl: &types.CacheControl{Type: "ephemeral", TTL: "5m"}}}},
			{"empty_string", `,"system":""`, nil},
			{"null", `,"system":null`, nil},
			{"omitted", "", nil},
		} {
			t.Run(decoder+"/"+tc.name, func(t *testing.T) {
				body := []byte(`{"model":"claude-opus-4-6","max_tokens":4096,"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},"messages":[{"role":"user","content":"hello"}]` + tc.field + `}`)
				var messages types.AnthropicRequest
				if err := unmarshal(body, &messages); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual([]types.AnthropicSystemMessage(messages.System), tc.want) {
					t.Fatal("generation system content or cache hints changed")
				}
				if messages.OutputConfig.Effort != "medium" || messages.MaxTokens != 4096 {
					t.Fatal("unrelated request controls changed")
				}
				var count types.CountTokensRequest
				if err := unmarshal(body, &count); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual([]types.AnthropicSystemMessage(count.System), tc.want) {
					t.Fatal("count_tokens system differs from generation")
				}
				encoded, err := utils.SafeMarshal(messages)
				if err != nil {
					t.Fatal(err)
				}
				var roundtrip types.AnthropicRequest
				if err := unmarshal(encoded, &roundtrip); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(roundtrip.System, messages.System) {
					t.Fatal("normalized system changed during serialization")
				}
			})
		}
	}
}

func TestInvalidSystemTypesReturnSafeFieldError(t *testing.T) {
	for _, value := range []string{`true`, `123`, `{"private-marker":"text"}`, `["private-marker"]`, `[{"type":"text","text":{"private-marker":true}}]`} {
		for _, unmarshal := range []func([]byte, any) error{json.Unmarshal, utils.SafeUnmarshal} {
			var req types.AnthropicRequest
			err := unmarshal([]byte(`{"system":`+value+`}`), &req)
			if err == nil || !strings.Contains(err.Error(), "system must be a string or an array of text blocks") {
				t.Fatalf("unexpected system error type: %T", err)
			}
			if strings.Contains(err.Error(), "private-marker") {
				t.Fatal("system error exposed request contents")
			}
		}
	}
}
