package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToClaude_StringInput(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4",
		"input": "Reply with ok",
		"max_output_tokens": 16
	}`)

	output := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "model").String(); got != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want %q", got, "claude-sonnet-4-6")
	}
	if got := gjson.Get(outputStr, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want %q", got, "user")
	}
	if got := gjson.Get(outputStr, "messages.0.content").String(); got != "Reply with ok" {
		t.Fatalf("messages.0.content = %q, want %q", got, "Reply with ok")
	}
	if got := gjson.Get(outputStr, "max_tokens").Int(); got != 16 {
		t.Fatalf("max_tokens = %d, want %d", got, 16)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_SanitizesToolSchema(t *testing.T) {
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
					}
				}
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "tools.0.input_schema.properties.items.items.type").String(); got != "object" {
		t.Fatalf("items.items.type = %q, want object", got)
	}
	if !gjson.Get(outputStr, "tools.0.input_schema.properties.items.items.properties").IsObject() {
		t.Fatal("items.items.properties missing after sanitize")
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DropsEmptyAssistantTextMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep this"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"and this"}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	outputStr := string(output)

	if got := len(gjson.Get(outputStr, "messages").Array()); got != 2 {
		t.Fatalf("messages length = %d, want 2", got)
	}
	if got := gjson.Get(outputStr, "messages.0.content").String(); got != "keep this" {
		t.Fatalf("messages.0.content = %q, want %q", got, "keep this")
	}
	if got := gjson.Get(outputStr, "messages.1.content").String(); got != "and this" {
		t.Fatalf("messages.1.content = %q, want %q", got, "and this")
	}
}

func TestConvertOpenAIResponsesRequestToClaude_MapsCustomToolsAndCalls(t *testing.T) {
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

	output := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", inputJSON, true)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "tools.0.name").String(); got != "apply_patch" {
		t.Fatalf("tools.0.name = %q, want apply_patch", got)
	}
	if got := gjson.Get(outputStr, "tools.0.input_schema.properties.input.type").String(); got != "string" {
		t.Fatalf("custom tool input schema type = %q, want string", got)
	}
	if got := gjson.Get(outputStr, "messages.0.content.0.type").String(); got != "tool_use" {
		t.Fatalf("messages.0.content.0.type = %q, want tool_use", got)
	}
	if got := gjson.Get(outputStr, "messages.0.content.0.input.input").String(); got != "*** Begin Patch" {
		t.Fatalf("tool_use input.input = %q, want custom tool input", got)
	}
	if got := gjson.Get(outputStr, "messages.1.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages.1.content.0.type = %q, want tool_result", got)
	}
	if got := gjson.Get(outputStr, "messages.1.content.0.content").String(); got != "OK" {
		t.Fatalf("tool_result content = %q, want OK", got)
	}
	if got := gjson.Get(outputStr, "tool_choice.name").String(); got != "apply_patch" {
		t.Fatalf("tool_choice.name = %q, want apply_patch", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_PreservesIncomingMetadataUserID(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"metadata":{"user_id":"user_04408135829d8e3beb46401215e3163fab6c5f9c857b575056e37708c9e3f91b_account__session_b77671ff-818b-4676-9aee-b6d6466cbd6c"},
		"input":"ok"
	}`)

	output := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	if got := gjson.GetBytes(output, "metadata.user_id").String(); got != "user_04408135829d8e3beb46401215e3163fab6c5f9c857b575056e37708c9e3f91b_account__session_b77671ff-818b-4676-9aee-b6d6466cbd6c" {
		t.Fatalf("metadata.user_id = %q, want preserved incoming user_id", got)
	}
}

func TestConvertOpenAIResponsesRequestToClaude_DerivesMetadataUserIDFromPreviousResponseID(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"previous_response_id":"resp_123",
		"input":"ok"
	}`)

	first := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	second := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-6", inputJSON, false)
	if got := gjson.GetBytes(first, "metadata.user_id").String(); got == "" {
		t.Fatal("metadata.user_id empty, want derived id")
	} else if got != gjson.GetBytes(second, "metadata.user_id").String() {
		t.Fatalf("metadata.user_id not stable: %q != %q", got, gjson.GetBytes(second, "metadata.user_id").String())
	}
}
