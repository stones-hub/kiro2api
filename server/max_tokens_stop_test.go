package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"kiro2api/internal/testutil"
)

const maxTokensText = "partial answer text"

var (
	maxTokensTextFrame     = testutil.EventFrame("assistantResponseEvent", `{"content":"`+maxTokensText+`"}`)
	maxTokensToolFrame     = testutil.EventFrame("toolUseEvent", `{"toolUseId":"tool-1","name":"lookup","input":{"q":"x"},"stop":true}`)
	lengthExceptionFrame   = exceptionFrame(`{"__type":"ContentLengthExceededException","message":"synthetic too long"}`)
	throttleExceptionFrame = exceptionFrame(`{"__type":"ThrottlingException","message":"synthetic throttle"}`)
)

func joinFrames(frames ...[]byte) []byte {
	var out []byte
	for _, f := range frames {
		out = append(out, f...)
	}
	return out
}

// stopReasons returns every non-null stop_reason (Anthropic) or finish_reason
// (OpenAI) the client received, in order.
func stopReasons(t *testing.T, body string, stream bool) []string {
	t.Helper()
	var docs []map[string]any
	if stream {
		for _, line := range strings.Split(body, "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok || data == "[DONE]" {
				continue
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(data), &doc); err != nil {
				t.Fatalf("bad SSE line %q: %v", line, err)
			}
			docs = append(docs, doc)
		}
	} else {
		var doc map[string]any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("bad JSON %q: %v", body, err)
		}
		docs = append(docs, doc)
	}
	var out []string
	for _, doc := range docs {
		if sr, ok := doc["stop_reason"].(string); ok {
			out = append(out, sr)
		}
		if delta, ok := doc["delta"].(map[string]any); ok {
			if sr, ok := delta["stop_reason"].(string); ok {
				out = append(out, sr)
			}
		}
		choices, _ := doc["choices"].([]any)
		for _, c := range choices {
			if fr, ok := c.(map[string]any)["finish_reason"].(string); ok {
				out = append(out, fr)
			}
		}
	}
	return out
}

func TestMaxTokensExceptionMapsToTruncationStop(t *testing.T) {
	cases := []struct {
		name                string
		upstream            []byte
		anthropic, openai   string
		anthropicStreamSkip bool // the Anthropic stream path is out of scope for this case
	}{
		{"text+max_tokens", joinFrames(maxTokensTextFrame, lengthExceptionFrame), "max_tokens", "length", false},
		{"text only", joinFrames(maxTokensTextFrame), "end_turn", "stop", false},
		{"tool only", joinFrames(maxTokensTextFrame, maxTokensToolFrame), "tool_use", "tool_calls", false},
		{"tool+max_tokens", joinFrames(maxTokensTextFrame, maxTokensToolFrame, lengthExceptionFrame), "max_tokens", "length", false},
		{"throttling", joinFrames(maxTokensTextFrame, throttleExceptionFrame), "end_turn", "stop", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := dumpRouter(t, 1)
			installDumpUpstream(t, func(int) (*http.Response, error) { return kiroReply(http.StatusOK, tc.upstream) })
			for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
				for _, stream := range []bool{true, false} {
					want := tc.anthropic
					if strings.Contains(path, "chat") {
						want = tc.openai
					} else if stream && tc.anthropicStreamSkip {
						continue
					}
					body := fmt.Sprintf(`{"model":"claude-opus-4-8","max_tokens":16,"stream":%v,"messages":[{"role":"user","content":"hello max tokens"}]}`, stream)
					w := postDump(r, path, body)
					if w.Code != http.StatusOK {
						t.Fatalf("%s stream=%v: %d %s", path, stream, w.Code, w.Body.String())
					}
					if !strings.Contains(w.Body.String(), maxTokensText) {
						t.Errorf("%s stream=%v: text missing from %s", path, stream, w.Body.String())
					}
					got := stopReasons(t, w.Body.String(), stream)
					if len(got) != 1 || got[0] != want {
						t.Errorf("%s stream=%v: stop reasons %v, want [%s]\n%s", path, stream, got, want, w.Body.String())
					}
				}
			}
		})
	}
}
