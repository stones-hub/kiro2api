package server

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"kiro2api/config"
	"kiro2api/converter"
	"kiro2api/parser"
	"kiro2api/types"
)

// Use account schemas for native controls and explicit compatibility modes
// for other models. Caller effort always takes priority over local defaults.
func prepareThinkingRequest(req *types.AnthropicRequest) error {
	req.KiroModelFields = nil
	req.KiroThinking = nil
	req.ExtractThinkingTags = false
	suffix := strings.HasSuffix(req.Model, "-thinking")
	effort := req.ReasoningEffort
	if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
		if effort != "" && effort != req.OutputConfig.Effort {
			return fmt.Errorf("output_config.effort and reasoning_effort must agree")
		}
		effort = req.OutputConfig.Effort
	}
	if !suffix && req.Thinking == nil && effort == "" {
		return nil // Preserve the upstream defaults for ordinary model names.
	}
	model, supported := config.ResolveModel(req.Model)
	if !supported {
		return fmt.Errorf("unknown model %q", req.Model)
	}
	cap := model.Thinking
	disabled := effort == "none"
	display := "summarized"
	budget := 0
	if req.Thinking != nil {
		switch req.Thinking.Type {
		case "adaptive", "enabled":
			if disabled {
				return fmt.Errorf("enabled thinking conflicts with reasoning_effort=none")
			}
		case "disabled":
			disabled = true // An explicit off switch takes priority over the suffix.
		default:
			return fmt.Errorf("thinking.type must be adaptive, enabled or disabled")
		}
		budget = req.Thinking.BudgetTokens
		if budget < 0 {
			return fmt.Errorf("thinking.budget_tokens must not be negative")
		}
		if req.Thinking.Display != "" {
			display = req.Thinking.Display
		}
	}
	if display != "summarized" && display != "omitted" {
		return fmt.Errorf("thinking.display must be summarized or omitted")
	}
	if effort == "" {
		if budget > 0 {
			effort = effortForBudget(budget)
		} else if suffix {
			effort = "max"
		} else if cap.DefaultEffort != "" {
			effort = cap.DefaultEffort
		} else {
			effort = "high"
		}
	}
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max", "none":
	default:
		return fmt.Errorf("unsupported reasoning effort; use none, minimal, low, medium, high, xhigh or max")
	}
	status := &types.KiroThinkingStatus{Mode: cap.Mode, RequestedEffort: effort}
	req.KiroThinking = status
	// Minimal and xhigh have no exact equivalent in some account schemas.
	// Report the actual value in headers instead of claiming exact preservation.
	if effort == "minimal" {
		effort = "low"
	}
	if effort == "xhigh" && len(cap.Efforts) > 0 && !slices.Contains(cap.Efforts, effort) {
		effort = "max"
	}
	maxTokens := req.MaxTokens
	if maxTokens < 0 {
		return fmt.Errorf("max_tokens must not be negative")
	}
	if maxTokens == 0 {
		maxTokens = 16384
	}
	maxTokens = max(cap.MinTokens, min(maxTokens, cap.MaxTokens))
	status.MaxTokensAdjusted = req.MaxTokens != maxTokens
	// Both upstream max-token locations must use this same effective limit.
	req.MaxTokens = maxTokens
	if cap.Mode == "upstream_default" {
		return nil // No undocumented controls; native events still pass through.
	}
	if cap.Mode == "legacy_hint" {
		if disabled || display == "omitted" {
			status.Mode = "upstream_default"
			return nil
		}
		// Exact legacy budgets are hints. Explicit effort overrides a budget.
		if budget == 0 || req.ReasoningEffort != "" || (req.OutputConfig != nil && req.OutputConfig.Effort != "") {
			budget = budgetForEffort(effort)
		}
		budget = min(budget, max(1, maxTokens-1))
		status.BudgetTokens = budget
		hint := fmt.Sprintf("<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>%d</max_thinking_length>", budget)
		req.System = append([]types.AnthropicSystemMessage{{Type: "text", Text: hint}}, req.System...)
		req.ExtractThinkingTags = true
		return nil
	}
	if !disabled && !slices.Contains(cap.Efforts, effort) {
		return fmt.Errorf("reasoning effort %q is not supported by this model", effort)
	}
	if cap.Mode == "reasoning" {
		if disabled {
			effort = "none"
			status.Mode = "disabled"
		}
		if !slices.Contains(cap.Efforts, effort) {
			return fmt.Errorf("this model has no reasoning disable control")
		}
		status.EffectiveEffort = effort
		req.KiroModelFields = &types.KiroModelRequestFields{Reasoning: &types.OutputConfig{Effort: effort}}
		return nil
	}
	// Adaptive-only models (e.g. Opus 5.5) cannot turn thinking off. Use the closest
	// equivalent instead of failing: the least reasoning, hidden from the client.
	if disabled && !cap.CanDisable {
		if !slices.Contains(cap.Efforts, "low") {
			return fmt.Errorf("this model requires adaptive thinking and has no low effort")
		}
		disabled = false
		effort = "low"
		display = "omitted"
		status.DisableFallback = true
	}
	fields := &types.KiroModelRequestFields{MaxTokens: maxTokens, Thinking: &types.KiroThinkingConfig{Type: "disabled"}}
	if !disabled {
		fields.Thinking.Type = "adaptive"
		fields.Thinking.Display = display
		fields.OutputConfig = &types.OutputConfig{Effort: effort}
		status.EffectiveEffort = effort
	} else {
		status.Mode = "disabled"
	}
	req.KiroModelFields = fields
	return nil
}

// Legacy budgets cannot be enforced by adaptive Kiro models. These documented
// ranges are a compatibility translation, not equivalent token guarantees.
func effortForBudget(budget int) string {
	switch {
	case budget <= 2048:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 16384:
		return "high"
	default:
		return "max"
	}
}

func budgetForEffort(effort string) int {
	switch effort {
	case "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 8192
	case "xhigh":
		return 16384
	default:
		return 32768
	}
}

func validateThinkingRequest(c *gin.Context, req *types.AnthropicRequest) bool {
	if err := prepareThinkingRequest(req); err != nil {
		respondError(c, 400, "%s", err.Error())
		return false
	}
	if err := converter.ValidateReasoningHistory(req.Messages); err != nil {
		respondError(c, 400, "%s", err.Error())
		return false
	}
	if status := req.KiroThinking; status != nil {
		c.Header("X-Kiro-Thinking-Mode", status.Mode)
		c.Header("X-Kiro-Thinking-Requested-Effort", status.RequestedEffort)
		if status.EffectiveEffort != "" {
			c.Header("X-Kiro-Thinking-Effort", status.EffectiveEffort)
		}
		if status.BudgetTokens > 0 {
			c.Header("X-Kiro-Thinking-Hint-Budget", strconv.Itoa(status.BudgetTokens))
		}
		c.Header("X-Kiro-Effective-Max-Tokens", strconv.Itoa(req.MaxTokens))
		if status.DisableFallback {
			c.Header("X-Kiro-Thinking-Disable-Fallback", "adaptive-low-omitted")
		}
		if status.MaxTokensAdjusted {
			c.Header("X-Kiro-Max-Tokens-Adjusted", "true")
		}
	}
	return true
}

func responseParser(req types.AnthropicRequest) *parser.CompliantEventStreamParser {
	p := parser.NewCompliantEventStreamParser()
	p.SetThinkingTags(req.ExtractThinkingTags)
	return p
}
