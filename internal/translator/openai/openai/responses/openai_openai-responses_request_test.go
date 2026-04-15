package responses

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_SanitizesToolSchema(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":"Reply with ok",
		"tools":[
			{
				"type":"function",
				"name":"spawn_agent",
				"description":"spawn",
				"parameters":{
					"type":"object",
					"properties":{
						"items":{"type":"array","items":null}
					},
					"required":["items","missing"]
				}
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "tools.0.function.parameters.properties.items.items.type").String(); got != "object" {
		t.Fatalf("items.items.type = %q, want object", got)
	}
	if !gjson.Get(outputStr, "tools.0.function.parameters.properties.items.items.properties").IsObject() {
		t.Fatal("items.items.properties missing after sanitize")
	}
	if got := len(gjson.Get(outputStr, "tools.0.function.parameters.required").Array()); got != 1 {
		t.Fatalf("required length = %d, want 1", got)
	}
	if gjson.Get(outputStr, "tools.0.function.parameters.properties.type").Exists() {
		t.Fatal("properties.type should not be synthesized from a property named items")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_NormalizesMalformedFunctionArguments(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"function_call","call_id":"call_bad","name":"shell_command","arguments":"{\"command\":\"first\"}{\"command\":\"second\"}"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	gotArgs := gjson.Get(outputStr, "messages.0.tool_calls.0.function.arguments").String()
	if !json.Valid([]byte(gotArgs)) {
		t.Fatalf("tool call arguments should be valid JSON, got %q", gotArgs)
	}
	if got := gjson.Get(gotArgs, "_error").String(); got != "arguments were not valid JSON" {
		t.Fatalf("fallback _error = %q, want %q", got, "arguments were not valid JSON")
	}
	if got := gjson.Get(gotArgs, "_raw_arguments").String(); got != "{\"command\":\"first\"}{\"command\":\"second\"}" {
		t.Fatalf("fallback _raw_arguments = %q", got)
	}
	if got := len(gjson.Get(outputStr, "messages").Array()); got != 1 {
		t.Fatalf("messages length = %d, want 1", got)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_SkipsMalformedFunctionCallPairs(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"function_call","call_id":"call_bad","name":"shell_command","arguments":"{\"command\":\"first\"}{\"command\":\"second\"}"},
			{"type":"function_call_output","call_id":"call_bad","output":"failed to parse function arguments: trailing characters at line 1 column 10"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := len(gjson.Get(outputStr, "messages").Array()); got != 1 {
		t.Fatalf("messages length = %d, want 1", got)
	}
	if gjson.Get(outputStr, "messages.0.tool_calls").Exists() {
		t.Fatal("malformed tool call pair should be skipped")
	}
	if got := gjson.Get(outputStr, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user", got)
	}
	if got := gjson.Get(outputStr, "messages.0.content.0.text").String(); got != "continue" {
		t.Fatalf("messages.0.content.0.text = %q, want continue", got)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_TrimsAssistantOnlyTailAfterLastUser(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue tracing"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Status update"}]},
			{"type":"function_call","call_id":"call_bad","name":"shell_command","arguments":"{\"command\":\"first\"}{\"command\":\"second\"}"},
			{"type":"function_call_output","call_id":"call_bad","output":"failed to parse function arguments: trailing characters at line 1 column 10"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":""}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Still working"}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := len(gjson.Get(outputStr, "messages").Array()); got != 1 {
		t.Fatalf("messages length = %d, want 1", got)
	}
	if got := gjson.Get(outputStr, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user", got)
	}
	if got := gjson.Get(outputStr, "messages.0.content.0.text").String(); got != "continue tracing" {
		t.Fatalf("messages.0.content.0.text = %q, want continue tracing", got)
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_DropsEmptyAssistantTextMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep this"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"and this"}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := len(gjson.Get(outputStr, "messages").Array()); got != 2 {
		t.Fatalf("messages length = %d, want 2", got)
	}
	if got := gjson.Get(outputStr, "messages.0.content.0.text").String(); got != "keep this" {
		t.Fatalf("messages.0.content.0.text = %q, want %q", got, "keep this")
	}
	if got := gjson.Get(outputStr, "messages.1.content.0.text").String(); got != "and this" {
		t.Fatalf("messages.1.content.0.text = %q, want %q", got, "and this")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MapsCustomToolsAndCalls(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"tools":[
			{"type":"custom","name":"apply_patch","description":"apply unified diff"}
		],
		"tool_choice":{"type":"custom","name":"apply_patch"},
		"input":[
			{"type":"custom_tool_call","call_id":"call_apply","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"call_apply","output":"OK"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "tools.0.function.name").String(); got != "apply_patch" {
		t.Fatalf("tools.0.function.name = %q, want apply_patch", got)
	}
	if got := gjson.Get(outputStr, "tools.0.function.parameters.properties.input.type").String(); got != "string" {
		t.Fatalf("custom tool input schema type = %q, want string", got)
	}
	if got := gjson.Get(outputStr, "messages.0.tool_calls.0.function.arguments").String(); got != "{\"input\":\"*** Begin Patch\"}" {
		t.Fatalf("custom tool call arguments = %q", got)
	}
	if got := gjson.Get(outputStr, "messages.1.role").String(); got != "tool" {
		t.Fatalf("messages.1.role = %q, want tool", got)
	}
	if got := gjson.Get(outputStr, "messages.1.content").String(); got != "OK" {
		t.Fatalf("messages.1.content = %q, want OK", got)
	}
	if got := gjson.Get(outputStr, "tool_choice.function.name").String(); got != "apply_patch" {
		t.Fatalf("tool_choice.function.name = %q, want apply_patch", got)
	}
}
