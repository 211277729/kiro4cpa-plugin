package kiro

import (
	"encoding/json"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageForEventType drives one non-stream call whose upstream emits a single
// usage frame under eventType and returns the usage object the plugin reports.
// payload is the frame body, so callers can exercise the flat and the
// frame-named (nested) shapes the same way upstream ships them.
func usageForEventType(t *testing.T, eventType, payload string) map[string]int {
	t.Helper()
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 200, Body: kiroBody(
			eventFrame("assistantResponseEvent", `{"content":"Hi"}`),
			eventFrame(eventType, payload),
		)}, nil
	})
	resp := callHandler[pluginapi.ExecutorResponse](t, pluginabi.MethodExecutorExecute,
		execRequest(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`))
	var msg struct {
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(resp.Payload, &msg); err != nil {
		t.Fatal(err)
	}
	return msg.Usage
}

const flatTokenUsage = `{"tokenUsage":{"uncachedInputTokens":11,"outputTokens":7,"cacheReadInputTokens":5,"cacheWriteInputTokens":3}}`

// TestMetadataEventNamesCarryUsage covers every frame name and payload shape the
// plugin must decode. Amazon Q ships the metadata either flat or wrapped in a
// frame-named object; a flat-only decode reported every cache count as zero
// because usage fell back to a local estimate.
func TestMetadataEventNamesCarryUsage(t *testing.T) {
	cases := []struct{ name, eventType, payload string }{
		{"metadataEvent flat", "metadataEvent", flatTokenUsage},
		{"messageMetadataEvent flat", "messageMetadataEvent", flatTokenUsage},
		{"messageMetadataEvent nested", "messageMetadataEvent",
			`{"messageMetadataEvent":` + flatTokenUsage + `}`},
		{"metadataEvent nested", "metadataEvent",
			`{"metadataEvent":` + flatTokenUsage + `}`},
	}
	for _, tc := range cases {
		usage := usageForEventType(t, tc.eventType, tc.payload)
		if usage["input_tokens"] != 11 || usage["output_tokens"] != 7 ||
			usage["cache_read_input_tokens"] != 5 || usage["cache_creation_input_tokens"] != 3 {
			t.Errorf("%s usage = %v, want real upstream counts", tc.name, usage)
		}
	}
}

// TestUnknownEventTypeKeepsStreamUsable guards the diagnostic path: an event
// type the plugin does not model must not fail or truncate the answer.
func TestUnknownEventTypeKeepsStreamUsable(t *testing.T) {
	usage := usageForEventType(t, "someFutureEventType", flatTokenUsage)
	if usage == nil {
		t.Fatal("usage missing when an unknown event type was present")
	}
}
