package kiro

import (
	"strings"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
)

// Capability rules follow kiro.rs v0.9.8. Unknown models keep the existing
// prompt-only behavior rather than receiving fields their schema may reject.
func usesGPTReasoning(model string) bool {
	switch strings.ToLower(model) {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return true
	}
	return false
}

func supportsNativeReasoning(model string) bool {
	model = strings.ToLower(model)
	if usesGPTReasoning(model) {
		return true
	}
	switch model {
	case "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8", "claude-sonnet-4.6":
		return true
	}
	for _, family := range []string{"claude-fable-5", "claude-mythos-5", "claude-sonnet-5", "claude-opus-5", "claude-haiku-5", "claude-5"} {
		if model == family || strings.HasPrefix(model, family+".") || strings.HasPrefix(model, family+"-") {
			return true
		}
	}
	return false
}

func reasoningEffort(req claudeRequest, model string) string {
	effort := ""
	if req.OutputConfig != nil {
		effort = strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort))
	}
	if effort == "" {
		budget := defaultThinkingBudget
		if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
			budget = min(req.Thinking.BudgetTokens, models.MaxThinkingBudget)
		}
		switch {
		case budget <= 4000:
			effort = "low"
		case budget <= 16000:
			effort = "medium"
		default:
			effort = "high"
		}
	}
	switch effort {
	case "x-high", "x_high":
		effort = "xhigh"
	case "none":
		if !usesGPTReasoning(model) {
			effort = "high"
		}
	case "low", "medium", "high", "xhigh", "max":
	default:
		effort = "high"
	}
	if effort == "xhigh" {
		switch strings.ToLower(model) {
		case "claude-opus-4.6", "claude-sonnet-4.6", "claude-opus-4.5", "claude-sonnet-4.5", "claude-haiku-4.5":
			effort = "high"
		}
	}
	return effort
}

func nativeReasoningFields(req claudeRequest, model string, suffixThinking bool) *additionalModelRequestFields {
	if !supportsNativeReasoning(model) || req.Thinking != nil && req.Thinking.Type == "disabled" {
		return nil
	}
	enabled := suffixThinking || req.Thinking != nil && (req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive")
	if !enabled && (req.OutputConfig == nil || strings.TrimSpace(req.OutputConfig.Effort) == "") {
		return nil
	}
	// Opus 4.6 accepts the native field only for explicitly adaptive requests.
	if strings.EqualFold(model, "claude-opus-4.6") && (req.Thinking == nil || req.Thinking.Type != "adaptive") {
		return nil
	}
	config := &reasoningConfig{Effort: reasoningEffort(req, model)}
	if usesGPTReasoning(model) {
		return &additionalModelRequestFields{Reasoning: config}
	}
	return &additionalModelRequestFields{OutputConfig: config}
}
