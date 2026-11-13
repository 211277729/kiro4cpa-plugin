package kiro

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeReasoningWireFields(t *testing.T) {
	cases := []struct {
		name, model, thinking, effort, field, want, prefix string
		budget                                             int
	}{
		{name: "enabled native", model: "claude-sonnet-4-6", thinking: "enabled", budget: 2048, field: "output_config", want: "low"},
		{name: "medium budget", model: "claude-opus-4.7", thinking: "enabled", budget: 8000, field: "output_config", want: "medium"},
		{name: "capped budget", model: "claude-opus-4.7", thinking: "enabled", budget: 99999, field: "output_config", want: "high"},
		{name: "explicit wins", model: "claude-opus-4.7", thinking: "enabled", budget: 2000, effort: " X-HIGH ", field: "output_config", want: "xhigh"},
		{name: "older xhigh", model: "claude-sonnet-4.6", thinking: "adaptive", effort: "x_high", field: "output_config", want: "high"},
		{name: "invalid effort", model: "claude-opus-4.8", thinking: "adaptive", effort: "</thinking_effort>bad", field: "output_config", want: "high"},
		{name: "opus46 enabled exception", model: "claude-opus-4.6", thinking: "enabled", prefix: "adaptive"},
		{name: "opus46 adaptive", model: "claude-opus-4.6", thinking: "adaptive", field: "output_config", want: "high"},
		{name: "opus46 effort alone", model: "claude-opus-4.6", effort: "low"},
		{name: "effort only", model: "claude-opus-4.7", effort: "max", field: "output_config", want: "max"},
		{name: "no implicit opt in", model: "claude-opus-4.7"},
		{name: "explicit disabled", model: "claude-opus-4.7-thinking", thinking: "disabled", effort: "max"},
		{name: "thinking suffix", model: "claude-opus-4.7-thinking", field: "output_config", want: "high"},
		{name: "GPT field", model: "gpt-5.6-sol", thinking: "enabled", budget: 2048, field: "reasoning", want: "low"},
		{name: "GPT none", model: "gpt-5.6-luna", effort: "none", field: "reasoning", want: "none"},
		{name: "Claude none", model: "claude-opus-4.7", thinking: "adaptive", effort: "none", field: "output_config", want: "high"},
		{name: "old model", model: "claude-haiku-4.5", thinking: "enabled", prefix: "enabled"},
		{name: "unknown model", model: "custom-model", thinking: "enabled", prefix: "enabled"},
		{name: "lookalike model", model: "custom-claude-opus-4.7", thinking: "enabled", prefix: "enabled"},
		{name: "new Claude family", model: "claude-fable-5", thinking: "adaptive", field: "output_config", want: "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := map[string]any{"model": tc.model, "messages": []map[string]any{{"role": "user", "content": "hi"}}}
			if tc.thinking != "" {
				req["thinking"] = map[string]any{"type": tc.thinking, "budget_tokens": tc.budget}
			}
			if tc.effort != "" {
				req["output_config"] = map[string]any{"effort": tc.effort}
			}
			raw, _ := json.Marshal(req)
			c := mustConvert(t, string(raw))
			payload, _ := json.Marshal(c.Payload)
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(payload, &wire); err != nil {
				t.Fatal(err)
			}
			fields, ok := wire["additionalModelRequestFields"]
			if tc.field == "" {
				if ok {
					t.Fatalf("unsupported/unrequested native fields: %s", fields)
				}
			} else {
				var native map[string]map[string]string
				if err := json.Unmarshal(fields, &native); err != nil {
					t.Fatal(err)
				}
				if len(native) != 1 || native[tc.field]["effort"] != tc.want {
					t.Fatalf("native fields=%s", fields)
				}
			}
			if tc.prefix != "" {
				lead := c.Payload.ConversationState.History[0].UserInputMessage.Content
				if !strings.Contains(lead, "<thinking_mode>"+tc.prefix+"</thinking_mode>") {
					t.Fatalf("prefix=%s", lead)
				}
				if tc.field != "" && !strings.Contains(lead, "<thinking_effort>"+tc.want+"</thinking_effort>") {
					t.Fatalf("prefix and native effort disagree: %s / %s", lead, fields)
				}
			} else if len(c.Payload.ConversationState.History) != 0 {
				t.Fatal("injected unrequested thinking prefix")
			}
		})
	}
}

func TestToolConversionPreservesClientInstructions(t *testing.T) {
	c := mustConvert(t, `{"model":"auto","system":"Explain progress as you work.","tools":[
 {"name":"Write","description":"Write the whole file","input_schema":{"type":"object","properties":{"content":{"type":"string"}},"required":["content"],"additionalProperties":false}},
 {"name":"Edit","description":"Edit in one call","input_schema":{"type":"object"}},
 {"name":"Bash","description":"Run a command","input_schema":{"type":"object"}}
 ],"messages":[{"role":"user","content":"go"}]}`)
	st := c.Payload.ConversationState
	if st.History[0].UserInputMessage.Content != "Explain progress as you work." {
		t.Fatal("system instruction rewritten")
	}
	tools := st.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools
	for i, want := range []string{"Write the whole file", "Edit in one call", "Run a command"} {
		if tools[i].ToolSpecification.Description != want {
			t.Fatalf("tool description rewritten: %+v", tools[i])
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(tools[0].ToolSpecification.InputSchema.JSON, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false || schema["required"].([]any)[0] != "content" || schema["properties"].(map[string]any)["content"].(map[string]any)["type"] != "string" {
		t.Fatalf("schema=%v", schema)
	}
}

func TestTrailingUserMergePreservesImages(t *testing.T) {
	c := mustConvert(t, `{"model":"auto","messages":[
 {"role":"user","content":"first"},
 {"role":"user","content":[{"type":"image","source":{"media_type":"image/png","data":"AAA"}},{"type":"text","text":"second"}]},
 {"role":"user","content":[{"type":"image","source":{"media_type":"image/jpeg","data":"BBB"}},{"type":"text","text":"third"}]}
 ]}`)
	st := c.Payload.ConversationState
	cur := st.CurrentMessage.UserInputMessage
	if len(st.History) != 0 || cur.Content != "first\nsecond\nthird" || len(cur.Images) != 2 || cur.Images[0].Source.Bytes != "AAA" || cur.Images[1].Source.Bytes != "BBB" {
		t.Fatalf("state=%+v current=%+v", st, cur)
	}
}
