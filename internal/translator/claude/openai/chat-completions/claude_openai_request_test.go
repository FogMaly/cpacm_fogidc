package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToClaude_MapsSystemMessagesToSystemField(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"system","content":"You are a precise Claude assistant."},
			{"role":"user","content":"继续"}
		]
	}`)

	out := ConvertOpenAIRequestToClaude("claude-opus-4-6", input, false)

	if got := len(gjson.GetBytes(out, "system").Array()); got != 1 {
		t.Fatalf("system length = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "system.0.text").String(); got != "You are a precise Claude assistant." {
		t.Fatalf("system.0.text = %q, want %q", got, "You are a precise Claude assistant.")
	}
	if got := len(gjson.GetBytes(out, "messages").Array()); got != 1 {
		t.Fatalf("messages length = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "继续" {
		t.Fatalf("messages.0.content.0.text = %q, want %q", got, "继续")
	}
}

func TestConvertOpenAIRequestToClaude_PreservesIncomingMetadataUserID(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6",
		"metadata":{"user_id":"user_04408135829d8e3beb46401215e3163fab6c5f9c857b575056e37708c9e3f91b_account__session_b77671ff-818b-4676-9aee-b6d6466cbd6c"},
		"messages":[{"role":"user","content":"hi"}]
	}`)

	out := ConvertOpenAIRequestToClaude("claude-opus-4-6", input, false)

	if got := gjson.GetBytes(out, "metadata.user_id").String(); got != "user_04408135829d8e3beb46401215e3163fab6c5f9c857b575056e37708c9e3f91b_account__session_b77671ff-818b-4676-9aee-b6d6466cbd6c" {
		t.Fatalf("metadata.user_id = %q, want preserved incoming user_id", got)
	}
}

func TestConvertOpenAIRequestToClaude_DerivesMetadataUserIDFromSessionID(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6",
		"session_id":"session-42",
		"messages":[{"role":"user","content":"hi"}]
	}`)

	first := ConvertOpenAIRequestToClaude("claude-opus-4-6", input, false)
	second := ConvertOpenAIRequestToClaude("claude-opus-4-6", input, false)

	if got := gjson.GetBytes(first, "metadata.user_id").String(); got == "" {
		t.Fatal("metadata.user_id empty, want stable derived id")
	} else if got != gjson.GetBytes(second, "metadata.user_id").String() {
		t.Fatalf("metadata.user_id not stable: %q != %q", got, gjson.GetBytes(second, "metadata.user_id").String())
	}
}

func TestConvertOpenAIRequestToClaude_GroupsConsecutiveToolResultsIntoSingleUserMessage(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{
				"role":"assistant",
				"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"TaskList","arguments":"{}"}},
					{"id":"call_2","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x\"}"}}
				]
			},
			{"role":"tool","tool_call_id":"call_1","content":"No tasks found"},
			{"role":"tool","tool_call_id":"call_2","content":"File does not exist"},
			{"role":"user","content":"继续"}
		]
	}`)

	out := ConvertOpenAIRequestToClaude("claude-opus-4-6", input, false)

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 3 {
		t.Fatalf("messages length = %d, want 3; out=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "assistant" {
		t.Fatalf("messages.0.role = %q, want assistant", got)
	}
	if got := gjson.GetBytes(out, "messages.1.role").String(); got != "user" {
		t.Fatalf("messages.1.role = %q, want user", got)
	}
	if got := len(gjson.GetBytes(out, "messages.1.content").Array()); got != 2 {
		t.Fatalf("messages.1.content length = %d, want 2; out=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages.1.content.0.type = %q, want tool_result", got)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.tool_use_id").String(); got != "call_1" {
		t.Fatalf("messages.1.content.0.tool_use_id = %q, want call_1", got)
	}
	if got := gjson.GetBytes(out, "messages.1.content.1.tool_use_id").String(); got != "call_2" {
		t.Fatalf("messages.1.content.1.tool_use_id = %q, want call_2", got)
	}
	if got := gjson.GetBytes(out, "messages.2.role").String(); got != "user" {
		t.Fatalf("messages.2.role = %q, want user", got)
	}
	if got := gjson.GetBytes(out, "messages.2.content.0.text").String(); got != "继续" {
		t.Fatalf("messages.2.content.0.text = %q, want 继续", got)
	}
}

func TestConvertOpenAIRequestToClaude_FlushesPendingToolResultsAtEnd(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{
				"role":"assistant",
				"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"TaskList","arguments":"{}"}}
				]
			},
			{"role":"tool","tool_call_id":"call_1","content":"No tasks found"}
		]
	}`)

	out := ConvertOpenAIRequestToClaude("claude-opus-4-6", input, false)

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 2 {
		t.Fatalf("messages length = %d, want 2; out=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages.1.content.0.type = %q, want tool_result", got)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.tool_use_id").String(); got != "call_1" {
		t.Fatalf("messages.1.content.0.tool_use_id = %q, want call_1", got)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.content").String(); got != "No tasks found" {
		t.Fatalf("messages.1.content.0.content = %q, want No tasks found", got)
	}
}

func TestConvertOpenAIRequestToClaude_PreservesNativeClaudeBridgePayload(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"继续当前任务"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_todos","name":"read_todos","input":{"scope":"active"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_todos","content":"{\"items\":[\"修复桥接\",\"验证长任务\"]}"},
				{"type":"text","text":"基于工具结果继续"}
			]}
		],
		"tools":[
			{"name":"read_todos","description":"Read todo items","input_schema":{"type":"object","properties":{"scope":{"type":"string"}}}}
		],
		"tool_choice":{"type":"tool","name":"read_todos"}
	}`)

	out := ConvertOpenAIRequestToClaude("claude-opus-4-6", input, false)

	if got := gjson.GetBytes(out, "messages.1.content.0.type").String(); got != "tool_use" {
		t.Fatalf("messages.1.content.0.type = %q, want tool_use; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.name").String(); got != "read_todos" {
		t.Fatalf("messages.1.content.0.name = %q, want read_todos; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.2.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages.2.content.0.type = %q, want tool_result; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.2.content.0.tool_use_id").String(); got != "call_todos" {
		t.Fatalf("messages.2.content.0.tool_use_id = %q, want call_todos; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.2.content.1.text").String(); got != "基于工具结果继续" {
		t.Fatalf("messages.2.content.1.text = %q, want 基于工具结果继续; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "read_todos" {
		t.Fatalf("tools.0.name = %q, want read_todos; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.0.input_schema.properties.scope.type").String(); got != "string" {
		t.Fatalf("tools.0.input_schema.properties.scope.type = %q, want string; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "tool" {
		t.Fatalf("tool_choice.type = %q, want tool; out=%s", got, out)
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "read_todos" {
		t.Fatalf("tool_choice.name = %q, want read_todos; out=%s", got, out)
	}
}
