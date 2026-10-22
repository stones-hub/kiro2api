package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Admin-side view and editing of the live model catalog. Changes are validated
// as a whole catalog, persisted by the caller, and only then swapped in, so a
// failed save never changes what /v1 serves.

var ErrCatalogConflict = errors.New("模型列表已被更新，请刷新页面后再操作")

type CatalogModelView struct {
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	Description    string               `json:"description,omitempty"`
	InputTypes     []string             `json:"input_types,omitempty"`
	RateMultiplier float64              `json:"rate_multiplier,omitempty"`
	MaxInput       int                  `json:"max_input_tokens"`
	MaxOutput      int                  `json:"max_output_tokens"`
	Thinking       ThinkingCapabilities `json:"thinking"`
	// Request names that resolve to this model, besides the ID and "-thinking" variants.
	Aliases []string `json:"aliases,omitempty"`
}

type CatalogView struct {
	Revision  int                `json:"revision"`
	Source    string             `json:"source"`
	UpdatedAt *time.Time         `json:"updated_at,omitempty"`
	Models    []CatalogModelView `json:"models"`
}

func modelView(m UpstreamModel) CatalogModelView {
	return CatalogModelView{ID: m.ID, Name: m.Name, Description: m.Description, InputTypes: m.InputTypes, RateMultiplier: m.RateMultiplier, MaxInput: m.TokenLimits.Input, MaxOutput: m.TokenLimits.Output, Thinking: m.Thinking}
}

// PrepareModelView validates one upstream entry and describes it like a catalog model.
func PrepareModelView(raw json.RawMessage) (CatalogModelView, error) {
	m, err := prepareModel(raw)
	if err != nil {
		return CatalogModelView{}, err
	}
	return modelView(m), nil
}

func CatalogSnapshot() CatalogView {
	c := activeModels.Load()
	v := CatalogView{Revision: c.revision, Source: c.source, Models: []CatalogModelView{}}
	if !c.updatedAt.IsZero() {
		t := c.updatedAt
		v.UpdatedAt = &t
	}
	aliases := map[string][]string{}
	for name, m := range c.entries {
		if name != m.ID && !strings.HasSuffix(name, "-thinking") {
			aliases[m.ID] = append(aliases[m.ID], name)
		}
	}
	for _, id := range c.order {
		mv := modelView(c.entries[id])
		mv.Aliases = aliases[id]
		sort.Strings(mv.Aliases)
		v.Models = append(v.Models, mv)
	}
	return v
}

// SameModel reports whether an upstream entry matches the catalog model on the
// fields that change how requests are served.
func SameModel(a, b CatalogModelView) bool {
	return a.Name == b.Name && a.MaxInput == b.MaxInput && a.MaxOutput == b.MaxOutput &&
		a.RateMultiplier == b.RateMultiplier && a.Thinking.Mode == b.Thinking.Mode &&
		strings.Join(a.Thinking.Efforts, ",") == strings.Join(b.Thinking.Efforts, ",")
}

// UpdateCatalog adds or replaces the given upstream entries and removes IDs,
// then persists the new document with save before activating it. revision must
// match CatalogSnapshot().Revision.
func UpdateCatalog(revision int, upsert []json.RawMessage, remove []string, save func([]byte) error) error {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	cur := activeModels.Load()
	if revision != cur.revision {
		return ErrCatalogConflict
	}
	if len(upsert) == 0 && len(remove) == 0 {
		return errors.New("没有要修改的模型")
	}
	raw := make(map[string]json.RawMessage, len(cur.raw)+len(upsert))
	for id, r := range cur.raw {
		raw[id] = r
	}
	order := append([]string{}, cur.order...)
	for _, id := range remove {
		if _, ok := raw[id]; !ok {
			return fmt.Errorf("模型不存在: %s", id)
		}
		delete(raw, id)
	}
	for _, r := range upsert {
		m, err := prepareModel(r)
		if err != nil {
			return err
		}
		if _, ok := raw[m.ID]; !ok {
			order = append(order, m.ID)
		}
		raw[m.ID] = r
	}
	doc := struct {
		CheckedAt string            `json:"checked_at"`
		Region    string            `json:"region"`
		Models    []json.RawMessage `json:"models"`
	}{CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Region: "us-east-1", Models: []json.RawMessage{}}
	for _, id := range order {
		if r, ok := raw[id]; ok {
			doc.Models = append(doc.Models, r)
		}
	}
	if len(doc.Models) == 0 {
		return errors.New("至少保留一个模型")
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	next, err := parseModelCatalog(data)
	if err != nil {
		return err
	}
	if err = save(append(data, '\n')); err != nil {
		return err
	}
	storeCatalog(next, "admin")
	return nil
}
