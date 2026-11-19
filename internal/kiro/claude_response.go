package kiro

import (
	"encoding/json"
	"strings"
)

// responseCollector folds a complete Kiro event stream into one Claude message.
type responseCollector struct {
	responseState
	text      strings.Builder
	reasoning strings.Builder
	signature string
	tools     toolAccumulator
}

func (c *responseCollector) add(ev Event) {
	c.observe(ev)
	if c.streamErr != nil {
		return
	}
	switch {
	case ev.Assistant != nil:
		c.text.WriteString(ev.Assistant.Content)
	case ev.Reasoning != nil:
		c.reasoning.WriteString(ev.Reasoning.Text)
		if ev.Reasoning.Signature != "" {
			c.signature = ev.Reasoning.Signature
		}
	case ev.ToolUse != nil:
		_, c.streamErr = c.tools.add(*ev.ToolUse)
	}
}

func (c *responseCollector) validate() *StreamError {
	if c.streamErr != nil {
		return c.streamErr
	}
	if c.stopReason == "refusal" && len(c.tools.order) > 0 {
		return &StreamError{Kind: "error", Code: "ContentFiltered", Message: "upstream refused a response containing tool calls"}
	}
	return c.tools.finish()
}

func (c *responseCollector) message(conv convertedRequest) []byte {
	text := c.text.String()
	thinking := c.reasoning.String()
	if conv.Thinking && thinking == "" {
		thinking, text = extractThinking(text)
	}
	if !conv.Thinking {
		thinking = ""
	}
	content := []map[string]any{}
	if thinking != "" {
		block := map[string]any{"type": "thinking", "thinking": thinking}
		if c.signature != "" {
			block["signature"] = c.signature
		}
		content = append(content, block)
	}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, id := range c.tools.order {
		t := c.tools.tools[id]
		input := json.RawMessage(t.arguments())
		name := t.name
		if original, ok := conv.ToolNames[name]; ok {
			name = original
		}
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
	}
	stop := c.completionReason(len(c.tools.order) > 0)
	var output tokenEstimate
	output.add(c.text.String() + c.reasoning.String())
	for _, id := range c.tools.order {
		output.add(c.tools.tools[id].input.String())
	}
	out, _ := json.Marshal(map[string]any{
		"id": "msg_" + strings.ReplaceAll(newUUID(), "-", ""), "type": "message", "role": "assistant",
		"model": conv.ModelID, "content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": usageFieldsFromState(&c.responseState, conv.ModelID, conv.InputTokens, output.tokens()),
	})
	return out
}

// usageFieldsFromState reports usage for one turn, preferring a real upstream
// report and otherwise deriving tokens from the metered credit spend.
func usageFieldsFromState(state *responseState, model string, estimatedInput, estimatedOutput int) map[string]int64 {
	var upstream *TokenUsage
	var credits float64
	var sawCredits bool
	if state != nil {
		upstream = state.usage
		credits = state.credits
		sawCredits = state.sawCredits
	}
	result := newUsageAccounting().apply(UsageInput{
		Model:           model,
		EstimatedInput:  estimatedInput,
		EstimatedOutput: estimatedOutput,
		Credits:         credits,
		SawCredits:      sawCredits,
		Upstream:        upstream,
	})
	logUsageCalibration(model, estimatedInput, estimatedOutput, credits, sawCredits, result)
	fields := map[string]int64{
		"input_tokens":  result.InputTokens,
		"output_tokens": result.OutputTokens,
	}
	// Only send the cache fields when they carry a value: some clients treat a
	// present-but-zero field differently from an absent one.
	if result.CacheReadInputTokens > 0 {
		fields["cache_read_input_tokens"] = result.CacheReadInputTokens
	}
	if result.CacheCreationInputTokens > 0 {
		fields["cache_creation_input_tokens"] = result.CacheCreationInputTokens
	}
	return fields
}
