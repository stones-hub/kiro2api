package converter

import (
	"regexp"
	"testing"

	"kiro2api/types"
)

var kiroToolIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func TestKiroToolUseIDKeepsValidIDs(t *testing.T) {
	for _, id := range []string{"toolu_01A09q90qw90lq917835lq9", "tooluse_abc-DEF_123", ""} {
		if got := kiroToolUseID(id); got != id {
			t.Fatalf("valid id %q rewritten to %q", id, got)
		}
	}
}

func TestKiroToolUseIDRewritesInvalidIDsConsistently(t *testing.T) {
	// IDs produced by other providers, e.g. "functions.Read:0" or "call:1".
	a, b := kiroToolUseID("functions.Read:0"), kiroToolUseID("functions.Read:0")
	if a != b || !kiroToolIDPattern.MatchString(a) {
		t.Fatalf("rewrite must be deterministic and valid: %q %q", a, b)
	}
	if kiroToolUseID("functions.Read:0") == kiroToolUseID("functions.Read.0") {
		t.Fatal("distinct ids collided after rewrite")
	}
	long := kiroToolUseID(string(make([]byte, 500)))
	if !kiroToolIDPattern.MatchString(long) || len(long) > 80 {
		t.Fatalf("long id not bounded: %d", len(long))
	}
}

func TestToolUseAndResultStayPairedAfterRewrite(t *testing.T) {
	id := "functions.Bash:3"
	name := "Bash"
	input := any(map[string]any{"command": "ls"})
	uses := extractToolUsesFromMessage([]types.ContentBlock{{Type: "tool_use", ID: &id, Name: &name, Input: &input}})
	results := extractToolResultsFromMessage([]any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"}})
	if len(uses) != 1 || len(results) != 1 || uses[0].ToolUseId != results[0].ToolUseId {
		t.Fatalf("tool pair broken: %+v %+v", uses, results)
	}
	if !kiroToolIDPattern.MatchString(uses[0].ToolUseId) {
		t.Fatalf("rewritten id still invalid: %q", uses[0].ToolUseId)
	}
}
