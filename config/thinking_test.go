package config

import "testing"

func TestAccountSchemaOverridesSnapshotAndPreservesCatalogMembership(t *testing.T) {
	data := []byte(`{"models":[{"modelId":"custom-model","tokenLimits":{"maxInputTokens":10000,"maxOutputTokens":4000},"additionalModelRequestFieldsSchema":{"properties":{"reasoning":{"properties":{"effort":{"enum":["none","low","medium","high","max"],"default":"medium"}}}}}},{"modelId":"claude-opus-4.6","tokenLimits":{"maxInputTokens":10000,"maxOutputTokens":64000},"additionalModelRequestFieldsSchema":null}]}`)
	catalog, err := parseModelCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	model := catalog.entries["custom-model-thinking"]
	if model.ID != "custom-model" || model.Thinking.Mode != "reasoning" || model.Thinking.DefaultEffort != "medium" {
		t.Fatal("new account schema was not used")
	}
	if catalog.entries["claude-opus-4.6"].Thinking.Mode != "upstream_default" {
		t.Fatal("explicit null schema did not override snapshot")
	}
	if _, ok := catalog.entries["claude-opus-5-thinking"]; ok {
		t.Fatal("capability snapshot enabled an unavailable model")
	}
}

func TestSnapshotCapabilitiesFillOlderAccountCatalog(t *testing.T) {
	catalog, err := parseModelCatalog([]byte(`{"models":[{"modelId":"claude-opus-5","tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":128000}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if catalog.entries["claude-opus-5-thinking"].Thinking.Mode != "adaptive" {
		t.Fatal("snapshot schema missing for legacy catalog")
	}
}
