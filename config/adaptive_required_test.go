package config

import "testing"

func TestAdaptiveOnlyCatalogAcceptsOfficialSchema(t *testing.T) {
	d := []byte(`{"models":[{"modelId":"claude-opus-5.5","tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":128000},"additionalModelRequestFieldsSchema":{"properties":{"thinking":{"properties":{"type":{"enum":["adaptive"]}}},"output_config":{"properties":{"effort":{"enum":["low","medium","high","xhigh","max"],"default":"medium"}}},"max_tokens":{"type":"integer","minimum":1024,"maximum":128000}}}}]}`)
	catalog, err := parseModelCatalog(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude-opus-5.5", "claude-opus-5-5", "claude-opus-5-5-thinking"} {
		m, ok := catalog.entries[name]
		if !ok || m.ID != "claude-opus-5.5" || m.Thinking.Mode != "adaptive" || m.Thinking.CanDisable || m.Thinking.DefaultEffort != "medium" {
			t.Fatalf("invalid capability for %s: %+v", name, m.Thinking)
		}
	}
}
