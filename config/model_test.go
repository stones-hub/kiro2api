package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCurrentModelsAndVersionAliases(t *testing.T) {
	for input, want := range map[string]string{
		"claude-opus-5":              "claude-opus-5",
		"claude-sonnet-5":            "claude-sonnet-5",
		"claude-opus-4-8":            "claude-opus-4.8",
		"claude-sonnet-4-6":          "claude-sonnet-4.6",
		"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
		"claude-sonnet-4-20250514":   "claude-sonnet-4",
		"claude-haiku-4-5-20251001":  "claude-haiku-4.5",
		"gpt-5.6-sol":                "gpt-5.6-sol",
		"auto":                       "auto",
	} {
		t.Run(input, func(t *testing.T) {
			model, ok := ResolveModel(input)
			if !ok || model.ID != want {
				t.Fatalf("ResolveModel(%q) = %q, %v; want %q", input, model.ID, ok, want)
			}
		})
	}
}

func TestUnavailableModelsNeverFallBackToAuto(t *testing.T) {
	for _, name := range []string{"", "claude-3-7-sonnet-20250219", "claude-3-5-haiku-20241022", "claude-opus-99", "unknown-model"} {
		if model, ok := ResolveModel(name); ok {
			t.Fatalf("unavailable model %q unexpectedly maps to %q", name, model.ID)
		}
	}
}

func TestCatalogLimitsAndOwners(t *testing.T) {
	model, ok := ResolveModel("claude-sonnet-4-6")
	if !ok || model.TokenLimits.Input != 1000000 || model.TokenLimits.Output != 64000 || model.Owner() != "anthropic" {
		t.Fatalf("incorrect Sonnet 4.6 metadata: %+v", model)
	}
	gpt, _ := ResolveModel("gpt-5.6-sol")
	if gpt.Owner() != "openai" {
		t.Fatalf("incorrect GPT owner: %q", gpt.Owner())
	}
}

func TestCatalogOnlyEnablesAliasesForPresentModels(t *testing.T) {
	catalog, err := parseModelCatalog([]byte(`{"models":[{"modelId":"claude-haiku-4.5","tokenLimits":{"maxInputTokens":200000,"maxOutputTokens":64000}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.entries["claude-haiku-4-5-20251001"]; !ok {
		t.Fatal("missing available Haiku alias")
	}
	if _, ok := catalog.entries["claude-sonnet-4-5-20250929"]; ok {
		t.Fatal("alias enabled an unavailable Sonnet model")
	}
}

func TestInvalidCatalogPreservesCurrentCatalog(t *testing.T) {
	before := activeModels.Load()
	for _, data := range []string{
		`{broken`, `{"models":[]}`,
		`{"models":[{"modelId":"bad id","tokenLimits":{"maxInputTokens":1,"maxOutputTokens":1}}]}`,
		`{"models":[{"modelId":"auto"}]}`,
		`{"models":[{"modelId":"auto","tokenLimits":{"maxInputTokens":1,"maxOutputTokens":1}},{"modelId":"auto","tokenLimits":{"maxInputTokens":1,"maxOutputTokens":1}}]}`,
		`{"models":[{"modelId":"claude-sonnet-4.6","tokenLimits":{"maxInputTokens":1,"maxOutputTokens":1}},{"modelId":"claude-sonnet-4-6","tokenLimits":{"maxInputTokens":1,"maxOutputTokens":1}}]}`,
	} {
		file := filepath.Join(t.TempDir(), "models.json")
		if err := os.WriteFile(file, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := LoadModelCatalog(file); err == nil {
			t.Fatalf("accepted invalid catalog: %s", data)
		}
		if activeModels.Load() != before {
			t.Fatal("invalid file replaced the working catalog")
		}
	}
}
