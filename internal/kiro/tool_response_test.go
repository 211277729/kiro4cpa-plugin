package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func toolFrames(events ...ToolUseEvent) []byte {
	var body []byte
	for _, ev := range events {
		raw, _ := json.Marshal(ev)
		body = append(body, eventFrame("toolUseEvent", string(raw))...)
	}
	return body
}

func TestToolResponseRejectsInvalidCalls(t *testing.T) {
	cases := []struct {
		name   string
		events []ToolUseEvent
	}{
		{"invalid JSON", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: `{"path":`, Stop: true}}},
		{"missing stop with valid JSON", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: `{"path":"/a"}`}}},
		{"array", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: `[]`, Stop: true}}},
		{"null", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: `null`, Stop: true}}},
		{"missing name", []ToolUseEvent{{ToolUseID: "a", Input: `{}`, Stop: true}}},
		{"missing ID", []ToolUseEvent{{Name: "Read", Input: `{}`, Stop: true}}},
		{"changed name", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: `{`}, {ToolUseID: "a", Name: "Write", Input: `}`, Stop: true}}},
		{"input after stop", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: `{}`, Stop: true}, {ToolUseID: "a", Input: ` `}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := toolFrames(tc.events...)
			t.Run("nonstream", func(t *testing.T) {
				stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
					return &hostapi.HTTPResponse{StatusCode: 200, Body: body}, nil
				})
				raw, _ := json.Marshal(execRequest(toolRequest))
				out, err := HandleMethod(pluginabi.MethodExecutorExecute, raw)
				if err != nil {
					t.Fatal(err)
				}
				env := decodeEnvelope(t, out)
				if env.OK || env.Error.HTTPStatus != 502 {
					t.Fatalf("invalid tool call returned %s", out)
				}
			})
			t.Run("stream", func(t *testing.T) {
				s := stubStream(t, &streamStub{status: 200, reads: []hostapi.StreamChunk{{Payload: body, Done: true}}})
				streamCall(t, toolRequest)
				if msg := waitClosed(t, s); msg == "" || strings.Contains(strings.Join(s.emitted, ""), "message_stop") {
					t.Fatalf("invalid tool call completed normally: %q", msg)
				}
				if len(s.ended) != 1 {
					t.Fatalf("upstream not closed: %v", s.ended)
				}
			})
		})
	}
}

func TestToolResponseEmptyAndLateName(t *testing.T) {
	cases := []struct {
		name   string
		events []ToolUseEvent
		want   string
	}{
		{"no arguments", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Stop: true}}, `{}`},
		{"empty no stop", []ToolUseEvent{{ToolUseID: "a", Name: "Read"}}, `{}`},
		{"whitespace no stop", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: " \n"}}, `{}`},
		{"late name", []ToolUseEvent{{ToolUseID: "a", Input: `{"path":`}, {ToolUseID: "a", Name: "Read", Input: `"/a"}`, Stop: true}}, `{"path":"/a"}`},
		{"empty final fragment", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: `{"path":"/a"}`}, {ToolUseID: "a", Stop: true}}, `{"path":"/a"}`},
		{"duplicate empty stop", []ToolUseEvent{{ToolUseID: "a", Name: "Read", Input: `{}`, Stop: true}, {ToolUseID: "a", Stop: true}}, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conv := convertedRequest{ToolNames: map[string]string{"Read": "original-name"}}
			s := newSSEStream(conv)
			var c responseCollector
			var out []sseEvent
			for _, ev := range tc.events {
				e := Event{ToolUse: &ev}
				c.add(e)
				out = append(out, s.handle(e)...)
			}
			out = append(out, s.finish()...)
			if s.streamErr != nil || c.validate() != nil {
				t.Fatalf("stream=%v collector=%v", s.streamErr, c.validate())
			}
			starts, stops := 0, 0
			input := ""
			for _, ev := range out {
				switch ev.name {
				case "content_block_start":
					starts++
					if ev.data["content_block"].(map[string]any)["name"] != "original-name" {
						t.Fatal("lost late/original tool name")
					}
				case "content_block_delta":
					input += ev.data["delta"].(map[string]any)["partial_json"].(string)
				case "content_block_stop":
					stops++
				}
			}
			if starts != 1 || stops != 1 || strings.TrimSpace(input) != tc.want {
				t.Fatalf("starts=%d stops=%d input=%q", starts, stops, input)
			}
			var message struct {
				Content []struct {
					Name  string
					Input json.RawMessage
				}
			}
			if err := json.Unmarshal(c.message(conv), &message); err != nil {
				t.Fatal(err)
			}
			if len(message.Content) != 1 || message.Content[0].Name != "original-name" || string(message.Content[0].Input) != tc.want {
				t.Fatalf("message %s", c.message(conv))
			}
		})
	}
}

func TestToolInputStreamsBeforeStop(t *testing.T) {
	s := newSSEStream(convertedRequest{})
	out := s.handle(Event{ToolUse: &ToolUseEvent{ToolUseID: "a", Name: "Read", Input: `{"path":`}})
	if eventNames(out) != "content_block_start:tool_use content_block_delta:input_json_delta" {
		t.Fatalf("first fragment buffered until stop: %s", eventNames(out))
	}
	out = s.handle(Event{ToolUse: &ToolUseEvent{ToolUseID: "b", Name: "Read", Input: `{"path":`}})
	if strings.Contains(eventNames(out), "content_block_stop") {
		t.Fatal("starting parallel tool closed incomplete first call")
	}
}

func TestRefusalDuringToolOutputFails(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "incomplete", true: "complete"}[stopped], func(t *testing.T) {
			input := `{"path":`
			if stopped {
				input = `{"path":"/a"}`
			}
			body := append(toolFrames(ToolUseEvent{ToolUseID: "a", Name: "Read", Input: input, Stop: stopped}), eventFrame("metadataEvent", `{"stopReason":"CONTENT_FILTERED","stopDetails":{"refusal":{"category":"test","explanation":"refused"}}}`)...)
			t.Run("stream", func(t *testing.T) {
				s := stubStream(t, &streamStub{status: 200, reads: []hostapi.StreamChunk{{Payload: body, Done: true}}})
				streamCall(t, toolRequest)
				msg := waitClosed(t, s)
				all := strings.Join(s.emitted, "")
				if msg == "" || strings.Contains(all, "message_stop") || !stopped && strings.Contains(all, "content_block_stop") {
					t.Fatalf("close=%q emitted=%s", msg, all)
				}
			})
			t.Run("nonstream", func(t *testing.T) {
				stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
					return &hostapi.HTTPResponse{StatusCode: 200, Body: body}, nil
				})
				raw, _ := json.Marshal(execRequest(toolRequest))
				out, _ := HandleMethod(pluginabi.MethodExecutorExecute, raw)
				env := decodeEnvelope(t, out)
				if env.OK || env.Error.Retryable {
					t.Fatalf("refused tool response returned/retried: %s", out)
				}
			})
		})
	}
}

func TestMetadataStopWithoutUsagePreservesUsage(t *testing.T) {
	body := kiroBody(eventFrame("assistantResponseEvent", `{"content":"partial"}`), eventFrame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":42,"outputTokens":7}}`), eventFrame("metadataEvent", `{"stopReason":"MAX_TOKENS"}`))
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 200, Body: body}, nil
	})
	resp := callHandler[pluginapi.ExecutorResponse](t, pluginabi.MethodExecutorExecute, execRequest(pingRequest))
	var msg struct {
		StopReason string `json:"stop_reason"`
		Usage      map[string]int
	}
	if err := json.Unmarshal(resp.Payload, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.StopReason != "max_tokens" || msg.Usage["input_tokens"] != 42 || msg.Usage["output_tokens"] != 7 {
		t.Fatalf("message=%s", resp.Payload)
	}
}

func TestOutputEstimateIndependentOfFragments(t *testing.T) {
	for _, tool := range []bool{false, true} {
		outputs := []int64{}
		for _, size := range []int{1, 3, 1000} {
			s := newSSEStream(convertedRequest{})
			input := `{"path":"abcdefghijklmnopqrstuvwxyz"}`
			for i := 0; i < len(input); i += size {
				part := input[i:min(i+size, len(input))]
				if tool {
					s.handle(Event{ToolUse: &ToolUseEvent{ToolUseID: "a", Name: "Read", Input: part}})
				} else {
					s.handle(Event{Assistant: &AssistantResponseEvent{Content: part}})
				}
			}
			if tool {
				s.handle(Event{ToolUse: &ToolUseEvent{ToolUseID: "a", Stop: true}})
			}
			out := s.finish()
			outputs = append(outputs, out[len(out)-2].data["usage"].(map[string]int64)["output_tokens"])
		}
		if outputs[0] != outputs[1] || outputs[1] != outputs[2] {
			t.Fatalf("tool=%v estimates=%v", tool, outputs)
		}
	}
}

func TestReasoningDisabledNeverExposesReasoning(t *testing.T) {
	for _, tc := range []struct {
		name, text    string
		tool, refusal bool
		want          string
	}{
		{name: "reasoning only"},
		{name: "visible answer", text: "answer", want: "answer"},
		{name: "tool answer", tool: true},
		{name: "refused", refusal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs := []Event{{Reasoning: &ReasoningContentEvent{Text: "reasoning"}}}
			if tc.text != "" {
				evs = append(evs, Event{Assistant: &AssistantResponseEvent{Content: tc.text}})
			}
			if tc.tool {
				evs = append(evs, Event{ToolUse: &ToolUseEvent{ToolUseID: "a", Name: "Read", Input: `{}`, Stop: true}})
			}
			if tc.refusal {
				evs = append(evs, Event{Metadata: &MetadataEvent{StopReason: "CONTENT_FILTERED"}})
			}
			var c responseCollector
			s := newSSEStream(convertedRequest{})
			var events []sseEvent
			for _, ev := range evs {
				c.add(ev)
				events = append(events, s.handle(ev)...)
			}
			events = append(events, s.finish()...)
			text := ""
			for _, ev := range events {
				if ev.name == "content_block_delta" {
					d := ev.data["delta"].(map[string]any)
					if d["type"] == "text_delta" {
						text += d["text"].(string)
					}
					if d["type"] == "thinking_delta" {
						t.Fatal("thinking emitted while disabled")
					}
				}
			}
			if text != tc.want {
				t.Fatalf("stream text=%q want=%q", text, tc.want)
			}
			var msg struct{ Content []struct{ Type, Text string } }
			if err := json.Unmarshal(c.message(convertedRequest{}), &msg); err != nil {
				t.Fatal(err)
			}
			text = ""
			for _, b := range msg.Content {
				if b.Type == "thinking" {
					t.Fatal("thinking emitted while disabled")
				}
				text += b.Text
			}
			if text != tc.want {
				t.Fatalf("nonstream text=%q want=%q", text, tc.want)
			}
		})
	}
}

// CPA's Responses translator finalizes the previous function when another
// content block starts, so serialized blocks are required even for parallel input.
func TestInterleavedInputEmitsSerialBlocks(t *testing.T) {
	s := newSSEStream(convertedRequest{})
	out := s.start()
	events := []Event{
		{ToolUse: &ToolUseEvent{ToolUseID: "a", Name: "Read", Input: `{"path":`}},
		{ToolUse: &ToolUseEvent{ToolUseID: "b", Name: "Read", Input: `{"path":`}},
		{Assistant: &AssistantResponseEvent{Content: "after tools"}},
		{ToolUse: &ToolUseEvent{ToolUseID: "b", Input: `"/b"}`, Stop: true}},
		{ToolUse: &ToolUseEvent{ToolUseID: "a", Input: `"/a"}`, Stop: true}},
	}
	for _, ev := range events {
		out = append(out, s.handle(ev)...)
	}
	out = append(out, s.finish()...)
	open := -1
	inputs := map[int]string{}
	names := []string{}
	for _, ev := range out {
		switch ev.name {
		case "content_block_start":
			if open != -1 {
				t.Fatalf("overlapping block: %v", ev)
			}
			open = ev.data["index"].(int)
			b := ev.data["content_block"].(map[string]any)
			if b["type"] == "tool_use" {
				names = append(names, b["id"].(string))
			}
		case "content_block_delta":
			if ev.data["index"] != open {
				t.Fatalf("delta for closed block: %v", ev)
			}
			d := ev.data["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				inputs[open] += d["partial_json"].(string)
			}
		case "content_block_stop":
			if ev.data["index"] != open {
				t.Fatalf("stop for closed block: %v", ev)
			}
			if input, ok := inputs[open]; ok && !json.Valid([]byte(input)) {
				t.Fatalf("closed incomplete input: %q", input)
			}
			open = -1
		}
	}
	if open != -1 || strings.Join(names, ",") != "a,b" || inputs[0] != `{"path":"/a"}` || inputs[1] != `{"path":"/b"}` {
		t.Fatalf("open=%d names=%v inputs=%v", open, names, inputs)
	}
}
