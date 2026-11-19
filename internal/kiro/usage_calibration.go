package kiro

import (
	"fmt"
	"time"
)

// logUsageCalibration writes one line per request describing what the service
// charged and what the plugin reported.
//
// The service never itemizes tokens, so the credit -> token conversion rests on
// per-model rates measured by hand. When a rate drifts, the reported usage
// drifts with it and the only symptom downstream is a bill that does not match
// the credit spend. One line per request is cheap enough to leave on for a day,
// and it carries exactly the numbers needed to re-derive the rates:
//
//	estimated tokens the caller sent, credits actually charged, and the split
//	the plugin reported.
//
// Set KIRO_USAGE_LOG to a file path to enable. Off by default.
func logUsageCalibration(model string, estimatedInput, estimatedOutput int, credits float64, sawCredits bool, result UsageResult) {
	path, enabled := usageLogFilePath()
	if !enabled {
		return
	}
	platformOverhead := ""
	if sawCredits && credits > 0 && estimatedInput > 0 {
		// The service bills its own agent prompt on top of the caller's tokens.
		// Printing the implied ratio makes a rate change visible immediately.
		platformOverhead = fmt.Sprintf(" credit_per_input_token=%.3e", credits/float64(estimatedInput+estimatedOutput))
	}
	writeLogLine(path, fmt.Sprintf(
		"%s model=%s in=%d out=%d credits=%.8f saw_credits=%t reported_in=%d reported_out=%d cache_read=%d cache_create=%d source=%s%s\n",
		time.Now().Format(time.RFC3339), model, estimatedInput, estimatedOutput, credits, sawCredits,
		result.InputTokens, result.OutputTokens, result.CacheReadInputTokens,
		result.CacheCreationInputTokens, result.Source, platformOverhead,
	))
}
