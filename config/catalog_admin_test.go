package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestUpdateCatalogAddsRemovesAndKeepsOnSaveFailure(t *testing.T) {
	before := activeModels.Load()
	t.Cleanup(func() { activeModels.Store(before) })
	rev := CatalogSnapshot().Revision
	sonnet := json.RawMessage(`{"modelId":"claude-sonnet-5.5","modelName":"Claude Sonnet 5.5","rateMultiplier":1.3,"tokenLimits":{"maxInputTokens":1000000,"maxOutputTokens":64000},"upstreamOnly":"kept"}`)
	if err := UpdateCatalog(rev, []json.RawMessage{sonnet}, nil, func([]byte) error { return errors.New("disk full") }); err == nil {
		t.Fatal("save failure ignored")
	}
	if _, ok := ResolveModel("claude-sonnet-5.5"); ok || CatalogSnapshot().Revision != rev {
		t.Fatal("failed save changed the live catalog")
	}
	var saved []byte
	if err := UpdateCatalog(rev, []json.RawMessage{sonnet}, []string{"glm-5"}, func(b []byte) error { saved = b; return nil }); err != nil {
		t.Fatal(err)
	}
	if m, ok := ResolveModel("claude-sonnet-5-5"); !ok || m.ID != "claude-sonnet-5.5" {
		t.Fatal("new model or its dash alias not served")
	}
	if _, ok := ResolveModel("claude-sonnet-5.5-thinking"); !ok {
		t.Fatal("thinking suffix missing for new model")
	}
	if _, ok := ResolveModel("glm-5"); ok {
		t.Fatal("removed model still served")
	}
	if err := UpdateCatalog(rev, nil, []string{"auto"}, func([]byte) error { return nil }); err != ErrCatalogConflict {
		t.Fatalf("stale revision: %v", err)
	}
	reparsed, err := parseModelCatalog(saved)
	if err != nil {
		t.Fatal(err)
	}
	var got, want bytes.Buffer
	_ = json.Compact(&got, reparsed.raw["claude-sonnet-5.5"])
	_ = json.Compact(&want, sonnet)
	if got.String() != want.String() {
		t.Fatalf("saved catalog lost upstream fields: %s", got.String())
	}
	snap := CatalogSnapshot()
	if snap.Source != "admin" || snap.Models[len(snap.Models)-1].ID != "claude-sonnet-5.5" {
		t.Fatalf("snapshot %s %v", snap.Source, snap.Models[len(snap.Models)-1].ID)
	}
	if err := UpdateCatalog(snap.Revision, []json.RawMessage{json.RawMessage(`{"modelId":"Bad ID","tokenLimits":{"maxInputTokens":1,"maxOutputTokens":1}}`)}, nil, func([]byte) error { return nil }); err == nil {
		t.Fatal("invalid model accepted")
	}
}
