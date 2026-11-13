package kiro

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestGrokNativeReasoning(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max", "none"} {
		t.Run(effort, func(t *testing.T) {
			c := mustConvert(t, `{"model":"grok-4.7","thinking":{"type":"enabled"},"output_config":{"effort":"`+effort+`"},"messages":[{"role":"user","content":"hello"}]}`)
			want := effort
			if effort == "max" {
				want = "xhigh"
			}
			if effort == "none" {
				want = "high"
			}
			f := c.Payload.AdditionalModelRequestFields
			if f == nil || f.Reasoning == nil || f.Reasoning.Effort != want || f.OutputConfig != nil {
				t.Fatalf("wrong Grok wire fields: %+v", f)
			}
		})
	}
}

func TestNativeReasoningDoesNotDuplicatePrompt(t *testing.T) {
	c := mustConvert(t, `{"model":"claude-opus-4.7","system":"Keep this instruction.","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hello"}]}`)
	if !c.Thinking || c.Payload.AdditionalModelRequestFields == nil {
		t.Fatal("native reasoning disabled by removing the prefix")
	}
	if got := c.Payload.ConversationState.History[0].UserInputMessage.Content; got != "Keep this instruction." {
		t.Fatalf("duplicated prompt controls: %q", got)
	}
}

func TestHistoryReasoningWireContract(t *testing.T) {
	cases := []struct{ name, blocks, extra, want string }{
		{"signed", `{"type":"thinking","thinking":"private","signature":" signed "}`, "", `{"reasoningText":{"text":"private","signature":" signed "}}`},
		{"redacted", `{"type":"redacted_thinking","data":"c2VhbGVk"}`, "", `{"redactedContent":"c2VhbGVk"}`},
		{"unsigned", `{"type":"thinking","thinking":"private"}`, "", ""},
		{"empty signature", `{"type":"thinking","thinking":"private","signature":" "}`, "", ""},
		{"placeholder", `{"type":"thinking","thinking":"private","signature":"kiro-rs-thinking-signature"}`, "", ""},
		{"empty thinking", `{"type":"thinking","thinking":"","signature":"sig"}`, "", ""},
		{"invalid redacted", `{"type":"redacted_thinking","data":"not base64"}`, "", ""},
		{"empty redacted", `{"type":"redacted_thinking","data":""}`, "", ""},
		{"newline redacted", `{"type":"redacted_thinking","data":"c2Vh\nbGVk"}`, "", ""},
		{"multiple", `{"type":"thinking","thinking":"private","signature":"s1"},{"type":"thinking","thinking":"other","signature":"s2"}`, "", ""},
		{"merged assistant", `{"type":"thinking","thinking":"private","signature":"s1"}`, `,{"role":"assistant","content":[{"type":"thinking","thinking":"other","signature":"s2"}]}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustConvert(t, `{"model":"claude-opus-4.7","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[`+tc.blocks+`,{"type":"text","text":"visible"}]}`+tc.extra+`,{"role":"user","content":"continue"}]}`)
			h := c.Payload.ConversationState.History
			a := h[len(h)-1].AssistantResponseMessage
			if a == nil || a.Content != "visible" {
				t.Fatalf("reasoning leaked into history text: %+v", a)
			}
			raw, _ := json.Marshal(a)
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if got := string(wire["reasoningContent"]); got != tc.want {
				t.Fatalf("reasoning=%s, want %s", got, tc.want)
			}
		})
	}
}

const signedHistoryRequest = `{"model":"claude-opus-4.7","thinking":{"type":"enabled"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"signature"},{"type":"text","text":"answer"}]},{"role":"user","content":"continue"}],"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`

func TestSignatureRecoveryPreservesRequestAndRetryBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, body, payload string
			second              int
			wantCalls           int
		}{
			{"recognized", `{"reason":"THINKING_SIGNATURE_INVALID"}`, signedHistoryRequest, 200, 2},
			{"long error", `{"details":"` + strings.Repeat("x", 700) + `","code":"THINKING_SIGNATURE_INVALID"}`, signedHistoryRequest, 200, 2},
			{"typed error", `{"__type":"THINKING_SIGNATURE_INVALID"}`, signedHistoryRequest, 200, 2},
			{"bare error", `THINKING_SIGNATURE_INVALID`, signedHistoryRequest, 200, 2},
			{"repeated signature", `{"reason":"THINKING_SIGNATURE_INVALID"}`, signedHistoryRequest, 400, 2},
			{"token failure after retry", `{"reason":"THINKING_SIGNATURE_INVALID"}`, signedHistoryRequest, 401, 2},
			{"no history", `{"reason":"THINKING_SIGNATURE_INVALID"}`, pingRequest, 200, 1},
			{"generic signature", `{"message":"Invalid signature while thinking"}`, signedHistoryRequest, 200, 1},
			{"unrelated", `{"reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`, signedHistoryRequest, 200, 1},
			{"lookalike", `{"reason":"NOT_THINKING_SIGNATURE_INVALID"}`, signedHistoryRequest, 200, 1},
		} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, tc.name), func(t *testing.T) {
				var requests []map[string]any
				record := func(r hostapi.HTTPRequest) int {
					if strings.Contains(r.URL, "refreshToken") {
						t.Fatal("signature recovery triggered credential refresh")
					}
					var body map[string]any
					if err := json.Unmarshal(r.Body, &body); err != nil {
						t.Fatal(err)
					}
					requests = append(requests, body)
					if len(requests) == 1 {
						return 400
					}
					return tc.second
				}
				wantOK := tc.wantCalls == 2 && tc.second == 200
				if stream {
					body := []byte(tc.body)
					s := stubStream(t, &streamStub{reads: []hostapi.StreamChunk{{Payload: body[:len(body)/2]}, {Payload: body[len(body)/2:], Done: true}}})
					if tc.second == 200 {
						s.reads = append(s.reads, hostapi.StreamChunk{Payload: eventFrame("assistantResponseEvent", `{"content":"OK"}`), Done: true})
					} else {
						s.reads = append(s.reads, hostapi.StreamChunk{Payload: body, Done: true})
					}
					hostHTTPDoStream = func(r hostapi.HTTPRequest) (*hostapi.HTTPStreamResponse, error) {
						return &hostapi.HTTPStreamResponse{StatusCode: record(r), StreamID: "up-1"}, nil
					}
					oHTTP := hostHTTPDo
					hostHTTPDo = func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
						t.Fatal("unexpected credential refresh")
						return nil, nil
					}
					t.Cleanup(func() { hostHTTPDo = oHTTP })
					env := streamCall(t, tc.payload)
					if env.OK != wantOK {
						t.Fatalf("result=%+v", env)
					}
					if env.OK {
						if msg := waitClosed(t, s); msg != "" {
							t.Fatal(msg)
						}
					}
					if len(s.ended) != tc.wantCalls {
						t.Fatalf("upstream streams leaked: %v", s.ended)
					}
				} else {
					stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
						status := record(r)
						body := []byte(tc.body)
						if status == 200 {
							body = eventFrame("assistantResponseEvent", `{"content":"OK"}`)
						}
						return &hostapi.HTTPResponse{StatusCode: status, Body: body}, nil
					})
					raw, _ := json.Marshal(execRequest(tc.payload))
					out, err := HandleMethod(pluginabi.MethodExecutorExecute, raw)
					if err != nil {
						t.Fatal(err)
					}
					if env := decodeEnvelope(t, out); env.OK != wantOK {
						t.Fatalf("result=%s", out)
					}
				}
				if len(requests) != tc.wantCalls {
					t.Fatalf("attempts=%d, want %d", len(requests), tc.wantCalls)
				}
				if len(requests) == 2 {
					expected := requests[0]
					for _, h := range expected["conversationState"].(map[string]any)["history"].([]any) {
						if a, ok := h.(map[string]any)["assistantResponseMessage"].(map[string]any); ok {
							delete(a, "reasoningContent")
						}
					}
					if !reflect.DeepEqual(expected, requests[1]) {
						t.Fatal("retry changed fields beyond assistant history reasoning")
					}
				}
			})
		}
	}
}

func TestGrokCataloguePreservesUpstreamContextLimit(t *testing.T) {
	infos := models.ToModelInfos([]models.UpstreamModel{{ModelID: "grok-4.7", TokenLimits: &models.TokenLimits{MaxInputTokens: 500000, MaxOutputTokens: 8192}}})
	if len(infos) != 1 || infos[0].ContextLength != 500000 || infos[0].InputTokenLimit != 500000 {
		t.Fatalf("lost live context limit: %+v", infos)
	}
}

func TestDisabledNativeReasoningNeverBecomesText(t *testing.T) {
	ev := Event{Reasoning: &ReasoningContentEvent{Text: "private-reasoning", Signature: "private-signature"}}
	var collector responseCollector
	collector.add(ev)
	if got := string(collector.message(convertedRequest{})); strings.Contains(got, "private-") {
		t.Fatalf("nonstream leak: %s", got)
	}
	s := newSSEStream(convertedRequest{})
	events := append(s.handle(ev), s.finish()...)
	raw, _ := json.Marshal(sseChunks(events))
	for _, e := range events {
		data, _ := json.Marshal(e.data)
		if strings.Contains(string(data), "private-") {
			t.Fatalf("stream leak: %s (%s)", data, raw)
		}
	}
}
