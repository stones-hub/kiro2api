package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed models.default.json
var defaultModelsJSON []byte

// UpstreamModel uses the field names returned by Kiro's ListAvailableModels.
type UpstreamModel struct {
	ID             string               `json:"modelId"`
	Name           string               `json:"modelName"`
	Description    string               `json:"description,omitempty"`
	InputTypes     []string             `json:"supportedInputTypes,omitempty"`
	RateMultiplier float64              `json:"rateMultiplier,omitempty"`
	RequestSchema  json.RawMessage      `json:"additionalModelRequestFieldsSchema,omitempty"`
	Thinking       ThinkingCapabilities `json:"-"`
	TokenLimits    struct {
		Input  int `json:"maxInputTokens"`
		Output int `json:"maxOutputTokens"`
	} `json:"tokenLimits"`
}

type ModelEntry struct {
	ID string
	UpstreamModel
}

type modelCatalog struct {
	entries map[string]UpstreamModel
	// Canonical IDs in document order and their original JSON, so the admin
	// page can rewrite the catalog without losing upstream fields.
	order     []string
	raw       map[string]json.RawMessage
	revision  int
	source    string
	updatedAt time.Time
}

var activeModels atomic.Pointer[modelCatalog]

// catalogMu serializes catalog replacements; readers use activeModels only.
var catalogMu sync.Mutex

func storeCatalog(c *modelCatalog, source string) {
	c.source = source
	c.revision = 1
	if cur := activeModels.Load(); cur != nil {
		c.revision = cur.revision + 1
	}
	activeModels.Store(c)
}

var builtinModelSchemas = map[string]json.RawMessage{}
var claudeVersion = regexp.MustCompile(`^(claude-(?:opus|sonnet|haiku)-[0-9]+)\.([0-9]+)$`)
var modelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// These aliases name a specific version, never a newer model or Auto.
var datedModelAliases = map[string]string{
	"claude-sonnet-4-20250514":   "claude-sonnet-4",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4.5",
	"claude-opus-4-5-20251101":   "claude-opus-4.5",
}

func init() {
	var defaults struct {
		Models []UpstreamModel `json:"models"`
	}
	if err := json.Unmarshal(defaultModelsJSON, &defaults); err != nil {
		panic(err)
	}
	for _, model := range defaults.Models {
		builtinModelSchemas[model.ID] = model.RequestSchema
	}
	catalog, err := parseModelCatalog(defaultModelsJSON)
	if err != nil {
		panic(fmt.Sprintf("invalid built-in model catalog: %v", err))
	}
	storeCatalog(catalog, "builtin")
}

// LoadModelCatalog replaces the built-in snapshot with an account's verified
// model list. Invalid files fail startup; they never silently enable fallbacks.
func LoadModelCatalog(path string) error {
	return LoadModelCatalogFrom(path, "file")
}

// LoadModelCatalogFrom is LoadModelCatalog with the source label shown in the admin page.
func LoadModelCatalogFrom(path, source string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read model catalog: %w", err)
	}
	catalog, err := parseModelCatalog(data)
	if err != nil {
		return err
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	storeCatalog(catalog, source)
	return nil
}

// prepareModel validates one ListAvailableModels entry and derives its thinking controls.
func prepareModel(raw json.RawMessage) (UpstreamModel, error) {
	var model UpstreamModel
	if err := json.Unmarshal(raw, &model); err != nil {
		return model, fmt.Errorf("invalid model entry: %w", err)
	}
	if len(model.ID) > 100 || !modelIDPattern.MatchString(model.ID) || model.TokenLimits.Input <= 0 || model.TokenLimits.Output <= 0 {
		return model, fmt.Errorf("invalid model ID or token limits: %q", model.ID)
	}
	if model.Name == "" {
		model.Name = model.ID
	}
	schema := model.RequestSchema
	if len(schema) == 0 {
		schema = builtinModelSchemas[model.ID]
	}
	var err error
	model.Thinking, err = thinkingCapabilities(model, schema)
	if err != nil {
		return model, fmt.Errorf("invalid thinking schema for %s: %w", model.ID, err)
	}
	return model, nil
}

func parseModelCatalog(data []byte) (*modelCatalog, error) {
	var document struct {
		// A string, so an odd timestamp never blocks startup.
		CheckedAt string            `json:"checked_at"`
		Models    []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("invalid model catalog JSON: %w", err)
	}
	if len(document.Models) == 0 {
		return nil, fmt.Errorf("model catalog is empty")
	}
	catalog := &modelCatalog{entries: make(map[string]UpstreamModel), raw: make(map[string]json.RawMessage)}
	if t, err := time.Parse(time.RFC3339Nano, document.CheckedAt); err == nil {
		catalog.updatedAt = t.UTC()
	}
	for _, raw := range document.Models {
		model, err := prepareModel(raw)
		if err != nil {
			return nil, err
		}
		if _, exists := catalog.entries[model.ID]; exists {
			return nil, fmt.Errorf("duplicate model ID: %q", model.ID)
		}
		catalog.entries[model.ID] = model
		catalog.raw[model.ID] = raw
		catalog.order = append(catalog.order, model.ID)
	}
	addAlias := func(alias, canonical string) error {
		model, ok := catalog.entries[canonical]
		if !ok {
			return nil
		}
		if existing, exists := catalog.entries[alias]; exists && existing.ID != canonical {
			return fmt.Errorf("ambiguous model alias: %q", alias)
		}
		catalog.entries[alias] = model
		return nil
	}
	for _, id := range catalog.order {
		if parts := claudeVersion.FindStringSubmatch(id); parts != nil {
			if err := addAlias(parts[1]+"-"+parts[2], id); err != nil {
				return nil, err
			}
		}
	}
	for alias, canonical := range datedModelAliases {
		if err := addAlias(alias, canonical); err != nil {
			return nil, err
		}
	}
	// Every available model accepts the compatibility suffix. Models without
	// native controls use their advertised legacy/default mode instead.
	// The suffix is local; Kiro only receives the original canonical ID.
	var thinkingAliases []string
	for alias := range catalog.entries {
		if !strings.HasSuffix(alias, "-thinking") {
			thinkingAliases = append(thinkingAliases, alias)
		}
	}
	for _, alias := range thinkingAliases {
		if err := addAlias(alias+"-thinking", catalog.entries[alias].ID); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

func ResolveModel(name string) (UpstreamModel, bool) {
	model, ok := activeModels.Load().entries[name]
	return model, ok
}

func SupportedModels() []ModelEntry {
	catalog := activeModels.Load()
	models := make([]ModelEntry, 0, len(catalog.entries))
	for id, model := range catalog.entries {
		models = append(models, ModelEntry{ID: id, UpstreamModel: model})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func (m UpstreamModel) Owner() string {
	switch {
	case strings.HasPrefix(m.ID, "claude-"):
		return "anthropic"
	case strings.HasPrefix(m.ID, "gpt-"):
		return "openai"
	case m.ID == "auto":
		return "kiro"
	default:
		return strings.SplitN(m.ID, "-", 2)[0]
	}
}
