package kiro

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type AssistantResponseEvent struct {
	Content string `json:"content"`
}

// ToolUseEvent arrives in fragments: Input is a partial JSON string, and Stop
// marks the last fragment of a tool call.
type ToolUseEvent struct {
	Name      string `json:"name"`
	ToolUseID string `json:"toolUseId"`
	Input     string `json:"input"`
	Stop      bool   `json:"stop"`
}

type ReasoningContentEvent struct {
	Text            string `json:"text,omitempty"`
	Signature       string `json:"signature,omitempty"`
	RedactedContent string `json:"redactedContent,omitempty"`
}

type ContextUsageEvent struct {
	ContextUsagePercentage float64 `json:"contextUsagePercentage"`
}

type MeteringEvent struct {
	Unit       string  `json:"unit"`
	UnitPlural string  `json:"unitPlural"`
	Usage      float64 `json:"usage"`
}

type TokenUsage struct {
	UncachedInputTokens   int64 `json:"uncachedInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	CacheReadInputTokens  int64 `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
}

// hasCounts reports whether the object carried any real count. An all-zero
// tokenUsage is not worth treating as upstream data.
func (u TokenUsage) hasCounts() bool {
	return u.UncachedInputTokens > 0 || u.OutputTokens > 0 ||
		u.CacheReadInputTokens > 0 || u.CacheWriteInputTokens > 0
}

type MetadataEvent struct {
	TokenUsage *TokenUsage `json:"tokenUsage,omitempty"`
	StopReason string      `json:"stopReason,omitempty"`
}

// UnmarshalJSON accepts both payload shapes seen in the wild. Amazon Q wraps the
// metadata in a frame-named object on some deployments, so the tokenUsage object
// arrived nested one level deeper than the flat decode expected. A flat decode of
// a wrapped payload found no tokenUsage, left usage nil, and forced every usage
// report back to a local estimate -- which made cache reads read as zero even on
// a full cache hit.
func (m *MetadataEvent) UnmarshalJSON(data []byte) error {
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	inner := data
	for _, name := range []string{"messageMetadataEvent", "metadataEvent"} {
		if nested, ok := wrapper[name]; ok {
			inner = nested
			break
		}
	}
	type plain struct {
		TokenUsage *TokenUsage `json:"tokenUsage,omitempty"`
		StopReason string      `json:"stopReason,omitempty"`
	}
	var decoded plain
	if err := json.Unmarshal(inner, &decoded); err != nil {
		return err
	}
	m.TokenUsage = decoded.TokenUsage
	m.StopReason = decoded.StopReason
	return nil
}

// StreamError is an in-band error or exception frame.
type StreamError struct {
	Kind    string // "error" or "exception"
	Code    string
	Message string
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("kiro stream %s %s: %s", e.Kind, e.Code, e.Message)
}

// Event holds exactly one decoded payload; unknown event types leave every field nil.
type Event struct {
	Assistant    *AssistantResponseEvent
	ToolUse      *ToolUseEvent
	Reasoning    *ReasoningContentEvent
	ContextUsage *ContextUsageEvent
	Metering     *MeteringEvent
	Metadata     *MetadataEvent
	Err          *StreamError
}

func DecodeEvent(f Frame) (Event, error) {
	switch f.MessageType() {
	case "error":
		return Event{Err: &StreamError{Kind: "error", Code: f.header(":error-code"), Message: string(f.Payload)}}, nil
	case "exception":
		return Event{Err: &StreamError{Kind: "exception", Code: f.header(":exception-type"), Message: string(f.Payload)}}, nil
	}
	var ev Event
	var target any
	switch f.EventType() {
	case "assistantResponseEvent":
		ev.Assistant = &AssistantResponseEvent{}
		target = ev.Assistant
	case "toolUseEvent":
		ev.ToolUse = &ToolUseEvent{}
		target = ev.ToolUse
	case "reasoningContentEvent":
		ev.Reasoning = &ReasoningContentEvent{}
		target = ev.Reasoning
	case "contextUsageEvent":
		ev.ContextUsage = &ContextUsageEvent{}
		target = ev.ContextUsage
	case "meteringEvent":
		ev.Metering = &MeteringEvent{}
		target = ev.Metering
	case "metadataEvent", "messageMetadataEvent":
		// Amazon Q has been observed emitting the token usage frame under both
		// names with the same tokenUsage payload. Accepting only metadataEvent
		// silently dropped every real count and forced usage back to a local
		// estimate, so cache_read/cache_creation always reported zero.
		ev.Metadata = &MetadataEvent{}
		target = ev.Metadata
	default:
		// The frame name is not a reliable contract: upstream has used more than
		// one name for the same payload and adds names over time. Any unmodeled
		// frame that carries tokenUsage is still the usage frame, so decode it
		// rather than dropping the counts and falling back to an estimate.
		if usage, ok := tokenUsageFromUnknownFrame(f.Payload); ok {
			return Event{Metadata: &MetadataEvent{TokenUsage: usage}}, nil
		}
		debugUnknownEvent(f.EventType())
		return Event{}, nil
	}
	if err := json.Unmarshal(f.Payload, target); err != nil {
		return Event{}, fmt.Errorf("decode %s: %w", f.EventType(), err)
	}
	return ev, nil
}

// loggedEventTypes remembers the unrecognized frame names already reported, so a
// long stream reports each name once instead of once per frame. Upstream adds
// event types over time; naming them makes a dropped usage frame diagnosable
// instead of invisible.
var (
	loggedEventTypesMu sync.Mutex
	loggedEventTypes   = make(map[string]struct{})
)

// traceFrame records every upstream frame this process receives. Upstream usage
// frames have arrived under names the plugin did not model, and the only way to
// settle which frame carries the counts is to look at the wire. Set
// KIRO_TRACE_FRAMES to a file path to enable; the payload is truncated so large
// answers stay readable.
//
// The host redirects the global standard logger without including plugin output,
// so tracing writes to its own file instead of log.Printf. This is a diagnostic
// switch only: it writes one line per frame, so leave it off on a busy gateway.
func traceFrame(f Frame) {
	path, enabled := traceFramesFilePath()
	if !enabled {
		return
	}
	payload := f.Payload
	if len(payload) > 320 {
		payload = payload[:320]
	}
	writeLogLine(path, fmt.Sprintf("%s type=%q message=%q payload=%s\n",
		time.Now().Format(time.RFC3339), f.EventType(), f.MessageType(), payload))
}

// writeLogLine appends one already-formatted line to a log file. Failures are
// silent: a diagnostic must never break a request.
func writeLogLine(path, line string) {
	traceMu.Lock()
	defer traceMu.Unlock()
	file, errOpen := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if errOpen != nil {
		return
	}
	_, _ = file.WriteString(line)
	_ = file.Close()
}

var (
	traceFramesOnce struct {
		sync.Once
		path    string
		enabled bool
	}
	usageLogOnce struct {
		sync.Once
		path    string
		enabled bool
	}
	traceMu sync.Mutex
)

// traceFramesPath resolves a diagnostic file from an environment variable. A
// value of "1" means the default name in the temp directory.
func traceFramesPath(envVar, defaultName string) (string, bool) {
	value := strings.TrimSpace(os.Getenv(envVar))
	if value == "" || value == "0" {
		return "", false
	}
	if value == "1" {
		value = filepath.Join(os.TempDir(), defaultName)
	}
	return value, true
}

func traceFramesFilePath() (string, bool) {
	traceFramesOnce.Do(func() {
		traceFramesOnce.path, traceFramesOnce.enabled = traceFramesPath("KIRO_TRACE_FRAMES", "kiro-frames.log")
	})
	return traceFramesOnce.path, traceFramesOnce.enabled
}

func usageLogFilePath() (string, bool) {
	usageLogOnce.Do(func() {
		usageLogOnce.path, usageLogOnce.enabled = traceFramesPath("KIRO_USAGE_LOG", "kiro-usage.log")
	})
	return usageLogOnce.path, usageLogOnce.enabled
}

// tokenUsageFromUnknownFrame searches an unmodeled frame for a tokenUsage object
// at any depth. Upstream wraps the payload differently across deployments, and
// the counts are the part that must not be lost: dropping them silently forces
// every usage report back to a local estimate.
func tokenUsageFromUnknownFrame(payload []byte) (*TokenUsage, bool) {
	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil, false
	}
	return findTokenUsage(doc, 0)
}

func findTokenUsage(node any, depth int) (*TokenUsage, bool) {
	if depth > 4 {
		return nil, false
	}
	object, ok := node.(map[string]any)
	if !ok {
		return nil, false
	}
	if raw, ok := object["tokenUsage"]; ok {
		encoded, errMarshal := json.Marshal(raw)
		if errMarshal == nil {
			var usage TokenUsage
			if errUnmarshal := json.Unmarshal(encoded, &usage); errUnmarshal == nil {
				if usage.hasCounts() {
					return &usage, true
				}
			}
		}
	}
	for _, value := range object {
		if usage, found := findTokenUsage(value, depth+1); found {
			return usage, true
		}
	}
	return nil, false
}

func debugUnknownEvent(eventType string) {
	if eventType == "" {
		return
	}
	loggedEventTypesMu.Lock()
	_, seen := loggedEventTypes[eventType]
	if !seen && len(loggedEventTypes) < 32 {
		loggedEventTypes[eventType] = struct{}{}
	}
	loggedEventTypesMu.Unlock()
	if !seen {
		log.Printf("kiro4cpa: ignoring unknown upstream event type %q", eventType)
	}
}
