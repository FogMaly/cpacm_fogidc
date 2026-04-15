package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToCodex_DoesNotEnableReasoningSummaryByDefault(t *testing.T) {
	input := []byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"hello"}
		]
	}`)

	out := ConvertOpenAIRequestToCodex("gpt-5.4", input, false)

	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "medium" {
		t.Fatalf("reasoning.effort = %q, want %q", got, "medium")
	}
	if gjson.GetBytes(out, "reasoning.summary").Exists() {
		t.Fatalf("reasoning.summary should be omitted for chat compatibility")
	}
}

func TestConvertOpenAIRequestToCodex_DropsEmptyAssistantMessageButKeepsToolCalls(t *testing.T) {
	input := []byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"user","content":"hello"},
			{
				"role":"assistant",
				"content":"",
				"tool_calls":[
					{
						"id":"call_1",
						"type":"function",
						"function":{"name":"list_directory","arguments":"{}"}
					}
				]
			},
			{"role":"user","content":"reply with OK"}
		]
	}`)

	out := ConvertOpenAIRequestToCodex("gpt-5.4", input, false)

	items := gjson.GetBytes(out, "input").Array()
	if len(items) != 3 {
		t.Fatalf("len(input) = %d, want 3; input=%s", len(items), string(out))
	}
	if got := gjson.GetBytes(out, "input.0.role").String(); got != "user" {
		t.Fatalf("input.0.role = %q, want user", got)
	}
	if got := gjson.GetBytes(out, "input.1.type").String(); got != "function_call" {
		t.Fatalf("input.1.type = %q, want function_call", got)
	}
	if got := gjson.GetBytes(out, "input.1.call_id").String(); got != "call_1" {
		t.Fatalf("input.1.call_id = %q, want call_1", got)
	}
	if got := gjson.GetBytes(out, "input.2.role").String(); got != "user" {
		t.Fatalf("input.2.role = %q, want user", got)
	}
}
