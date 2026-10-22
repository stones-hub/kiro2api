package parser

import (
	"kiro2api/internal/testutil"
	"testing"
)

func TestMetadataUsageAcrossBinaryFragmentsAndReset(t *testing.T) {
	// Fixture values follow the official SDK schema; these are not live hits.
	payload := `{"tokenUsage":{"uncachedInputTokens":23,"outputTokens":4,"totalTokens":8219,"cacheReadInputTokens":8192,"cacheWriteInputTokens":0}}`
	frame := testutil.EventFrame("metadataEvent", payload)
	p := NewCompliantEventStreamParser()
	for pass := 0; pass < 2; pass++ {
		for offset := 0; offset < len(frame); offset += 7 {
			end := offset + 7
			if end > len(frame) {
				end = len(frame)
			}
			events, err := p.ParseStream(frame[offset:end])
			if err != nil || len(events) != 0 {
				t.Fatalf("metadata must not become content: %v, %v", events, err)
			}
		}
	}
	u := p.GetTokenUsage()
	if !u.Valid() || *u.CacheReadInputTokens != 8192 || *u.UncachedInputTokens != 23 {
		t.Fatalf("metadata was lost or counted twice: %+v", u)
	}
	p.Reset()
	if p.GetTokenUsage() != nil {
		t.Fatal("usage leaked into a subsequent request")
	}
	result, err := p.ParseResponse(frame)
	if err != nil || !result.TokenUsage.Valid() {
		t.Fatalf("nonstream metadata lost: %v", err)
	}
}

func TestCreditsAndContextPercentageNeverBecomeCachedTokens(t *testing.T) {
	// These payloads reproduce the numeric fields seen in the live account probe.
	frames := append(testutil.EventFrame("contextUsageEvent", `{"contextUsagePercentage":1.6406999826431274}`),
		testutil.EventFrame("meteringEvent", `{"usage":0.0608830072305141,"unit":"credit"}`)...)
	p := NewCompliantEventStreamParser()
	result, err := p.ParseResponse(frames)
	if err != nil || result.TokenUsage != nil {
		t.Fatalf("invented token statistics: %v", err)
	}
	usage := result.TokenUsage.AnthropicUsage(100, 4)
	if _, ok := usage["cache_read_input_tokens"]; ok {
		t.Fatal("unknown cache usage was reported as a hit or zero")
	}
	if usage["input_tokens"] != 100 {
		t.Fatal("fallback input estimate lost")
	}
}

func TestIncompleteOrInvalidMetadataIsNotUsedForBilling(t *testing.T) {
	for _, payload := range []string{
		`{}`, `{"tokenUsage":null}`, `{"tokenUsage":{"cacheReadInputTokens":100}}`,
		`{"tokenUsage":{"uncachedInputTokens":2,"outputTokens":4,"totalTokens":6,"cacheReadInputTokens":-1}}`,
		`{"tokenUsage":{"uncachedInputTokens":2,"outputTokens":4,"totalTokens":6,"cacheReadInputTokens":1.5}}`,
		`{"tokenUsage":{"uncachedInputTokens":2,"outputTokens":4,"totalTokens":6,"cacheReadInputTokens":"100"}}`,
	} {
		p := NewCompliantEventStreamParser()
		_, _ = p.ParseResponse(testutil.EventFrame("metadataEvent", payload))
		if p.GetTokenUsage() != nil {
			t.Fatalf("invalid usage accepted: %s", payload)
		}
	}
}

func TestMeteringEventsAreSummedAsCredits(t *testing.T) {
	frames := append(testutil.EventFrame("meteringEvent", `{"usage":0.25,"unit":"credit"}`),
		testutil.EventFrame("meteringEvent", `{"usage":0.05,"unit":"credit"}`)...)
	frames = append(frames, testutil.EventFrame("meteringEvent", `{"usage":9,"unit":"other"}`)...)
	p := NewCompliantEventStreamParser()
	if _, err := p.ParseResponse(frames); err != nil {
		t.Fatal(err)
	}
	if c, ok := p.Credits(); !ok || c < 0.2999 || c > 0.3001 {
		t.Fatalf("credits %v %v", c, ok)
	}
	p.Reset()
	if _, ok := p.Credits(); ok {
		t.Fatal("reset kept credits")
	}
}
