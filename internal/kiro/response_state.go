package kiro

import "strings"

// responseState keeps completion and usage semantics identical for both paths.
type responseState struct {
	usage       *TokenUsage
	stopReason  string
	nativeStop  string
	contextFull bool
	streamErr   *StreamError
	// credits is the metered spend Amazon Q reports for this turn. It is the
	// only authoritative number the service sends: token counts are never
	// itemized, so usage is derived from this value.
	credits       float64
	creditsUnit   string
	sawCredits    bool
}

func (s *responseState) observe(ev Event) {
	switch {
	case ev.Metadata != nil:
		if ev.Metadata.TokenUsage != nil {
			s.usage = ev.Metadata.TokenUsage
		}
		if reason := strings.TrimSpace(ev.Metadata.StopReason); reason != "" {
			s.nativeStop = strings.ToUpper(reason)
			if s.nativeStop == "CONTENT_FILTERED" {
				s.stopReason = "refusal"
			}
		}
	case ev.Metering != nil:
		// Later metering frames supersede earlier ones for the same turn.
		if ev.Metering.Usage > 0 {
			s.credits = ev.Metering.Usage
			s.creditsUnit = strings.ToLower(strings.TrimSpace(ev.Metering.Unit))
			s.sawCredits = true
		}
	case ev.ContextUsage != nil:
		s.contextFull = s.contextFull || ev.ContextUsage.ContextUsagePercentage >= 100
	case ev.Err != nil:
		if ev.Err.Code == "ContentLengthExceededException" {
			if s.stopReason != "refusal" {
				s.stopReason = "max_tokens"
			}
		} else if s.streamErr == nil {
			s.streamErr = ev.Err
		}
	}
}

func (s *responseState) completionReason(hasTools bool) string {
	if s.stopReason != "" {
		return s.stopReason
	}
	switch s.nativeStop {
	case "MAX_TOKENS", "MODEL_CONTEXT_WINDOW_EXCEEDED", "STOP_SEQUENCE":
		return strings.ToLower(s.nativeStop)
	case "":
		if s.contextFull {
			return "model_context_window_exceeded"
		}
	}
	if hasTools {
		return "tool_use"
	}
	return "end_turn"
}

func toolStreamError(message string) *StreamError {
	// Never include raw arguments: they can contain client secrets or file data.
	return &StreamError{Kind: "error", Code: "InvalidToolUse", Message: message}
}
