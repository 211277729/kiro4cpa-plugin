package kiro

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// A merged assistant turn has one wire union. Counting all reasoning blocks
// prevents selectively replaying a signed fragment of an incomplete turn.
func selectHistoryReasoning(messages []claudeMessage) *historyReasoning {
	var candidate *claudeBlock
	for _, m := range messages {
		for _, b := range blocks(m.Content) {
			if b.Type != "thinking" && b.Type != "redacted_thinking" {
				continue
			}
			if candidate != nil {
				return nil
			}
			candidate = &b
		}
	}
	if candidate == nil {
		return nil
	}
	if candidate.Type == "thinking" {
		signature := strings.TrimSpace(candidate.Signature)
		if candidate.Thinking == "" || signature == "" || signature == "kiro-rs-thinking-signature" {
			return nil
		}
		return &historyReasoning{ReasoningText: &signedReasoning{Text: candidate.Thinking, Signature: candidate.Signature}}
	}
	if strings.ContainsAny(candidate.Data, "\r\n") {
		return nil
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(candidate.Data)
	if err != nil || len(decoded) == 0 {
		return nil
	}
	return &historyReasoning{RedactedContent: candidate.Data}
}

func isThinkingSignatureInvalid(body []byte) bool {
	const code = "THINKING_SIGNATURE_INVALID"
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return strings.TrimSpace(string(body)) == code
	}
	for _, key := range []string{"reason", "code", "__type"} {
		var value string
		if json.Unmarshal(fields[key], &value) == nil && value == code {
			return true
		}
	}
	return false
}

// Retry from the canonical payload, removing only assistant history reasoning.
// The caller shares its single retry with token recovery; this never penalizes
// credentials or retries a generic authentication-signature error.
func (c *convertedRequest) dropRejectedReasoning(err error) bool {
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != http.StatusBadRequest || !upstream.ThinkingSignatureInvalid {
		return false
	}
	changed := false
	for _, h := range c.Payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil && a.ReasoningContent != nil {
			a.ReasoningContent = nil
			changed = true
		}
	}
	return changed
}
