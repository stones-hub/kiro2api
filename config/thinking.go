package config

import (
	"encoding/json"
	"fmt"
	"slices"
)

// These capabilities describe request controls, not a promise of visible
// reasoning on every answer. Schemas are from ListAvailableModels.
type ThinkingCapabilities struct {
	CanDisable    bool     `json:"can_disable"`
	Mode          string   `json:"mode"`
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	MinTokens     int      `json:"min_tokens,omitempty"`
	MaxTokens     int      `json:"max_tokens,omitempty"`
}

type modelRequestSchema struct {
	Type       string                        `json:"type"`
	Properties map[string]modelRequestSchema `json:"properties"`
	Enum       []string                      `json:"enum"`
	Default    string                        `json:"default"`
	Minimum    int                           `json:"minimum"`
	Maximum    int                           `json:"maximum"`
}

func thinkingCapabilities(model UpstreamModel, data json.RawMessage) (ThinkingCapabilities, error) {
	cap := ThinkingCapabilities{Mode: "upstream_default", MaxTokens: model.TokenLimits.Output}
	var schema modelRequestSchema
	if len(data) != 0 {
		if err := json.Unmarshal(data, &schema); err != nil {
			return cap, err
		}
	}
	props := schema.Properties
	var effort modelRequestSchema
	if thinking, ok := props["thinking"]; ok && slices.Contains(thinking.Properties["type"].Enum, "adaptive") {
		cap.CanDisable = slices.Contains(thinking.Properties["type"].Enum, "disabled")
		cap.Mode = "adaptive"
		effort = props["output_config"].Properties["effort"]
		limit := props["max_tokens"]
		if limit.Type != "integer" || limit.Minimum <= 0 || limit.Maximum < limit.Minimum {
			return cap, fmt.Errorf("invalid output token range")
		}
		cap.MinTokens = limit.Minimum
		cap.MaxTokens = min(cap.MaxTokens, limit.Maximum)
		if cap.MaxTokens < cap.MinTokens {
			return cap, fmt.Errorf("model output cap is below thinking minimum")
		}
	} else if reasoning, ok := props["reasoning"]; ok {
		cap.Mode = "reasoning"
		effort = reasoning.Properties["effort"]
	} else {
		// Only these legacy Claude models have been probed with the Kiro hint
		// convention. Its output is labelled tagged text, never native reasoning.
		switch model.ID {
		case "claude-opus-4.5", "claude-sonnet-4.5", "claude-sonnet-4", "claude-haiku-4.5":
			cap.Mode = "legacy_hint"
		}
		return cap, nil
	}
	for _, value := range effort.Enum {
		if slices.Contains([]string{"none", "low", "medium", "high", "xhigh", "max"}, value) {
			cap.Efforts = append(cap.Efforts, value)
		}
	}
	if !slices.Contains(cap.Efforts, "high") {
		return cap, fmt.Errorf("unrecognized effort schema")
	}
	cap.DefaultEffort = effort.Default
	if !slices.Contains(cap.Efforts, cap.DefaultEffort) {
		cap.DefaultEffort = "high"
	}
	return cap, nil
}
