package responses

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// TestConvertSystemRoleToDeveloper_BasicConversion tests the basic system -> developer role conversion
func TestConvertSystemRoleToDeveloper_BasicConversion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are a pirate."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Say hello."}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that system role was converted to developer
	firstItemRole := gjson.Get(outputStr, "input.0.role")
	if firstItemRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstItemRole.String())
	}

	// Check that user role remains unchanged
	secondItemRole := gjson.Get(outputStr, "input.1.role")
	if secondItemRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", secondItemRole.String())
	}

	// Check content is preserved
	firstItemContent := gjson.Get(outputStr, "input.0.content.0.text")
	if firstItemContent.String() != "You are a pirate." {
		t.Errorf("Expected content 'You are a pirate.', got '%s'", firstItemContent.String())
	}
}

// TestConvertSystemRoleToDeveloper_MultipleSystemMessages tests conversion with multiple system messages
func TestConvertSystemRoleToDeveloper_MultipleSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "Be concise."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that both system roles were converted
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "developer" {
		t.Errorf("Expected second role 'developer', got '%s'", secondRole.String())
	}

	// Check that user role is unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "user" {
		t.Errorf("Expected third role 'user', got '%s'", thirdRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_NoSystemMessages tests that requests without system messages are unchanged
func TestConvertSystemRoleToDeveloper_NoSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi there!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that user and assistant roles are unchanged
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "assistant" {
		t.Errorf("Expected role 'assistant', got '%s'", secondRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_EmptyInput tests that empty input arrays are handled correctly
func TestConvertSystemRoleToDeveloper_EmptyInput(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": []
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that input is still an empty array
	inputArray := gjson.Get(outputStr, "input")
	if !inputArray.IsArray() {
		t.Error("Input should still be an array")
	}
	if len(inputArray.Array()) != 0 {
		t.Errorf("Expected empty array, got %d items", len(inputArray.Array()))
	}
}

// TestConvertSystemRoleToDeveloper_NoInputField tests that requests without input field are unchanged
func TestConvertSystemRoleToDeveloper_NoInputField(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that other fields are still set correctly
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true by conversion")
	}

	store := gjson.Get(outputStr, "store")
	if !store.Bool() {
		t.Error("Store should default to true for response continuation")
	}
}

// TestConvertOpenAIResponsesRequestToCodex_OriginalIssue tests the exact issue reported by the user
func TestConvertOpenAIResponsesRequestToCodex_OriginalIssue(t *testing.T) {
	// This is the exact input that was failing with "System messages are not allowed"
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": "You are a pirate. Always respond in pirate speak."
			},
			{
				"type": "message",
				"role": "user",
				"content": "Say hello."
			}
		],
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify system role was converted to developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstRole.String())
	}

	// Verify stream was set to true (as required by Codex)
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true")
	}

	// Verify other required fields for Codex
	store := gjson.Get(outputStr, "store")
	if !store.Bool() {
		t.Error("Store should default to true")
	}

	parallelCalls := gjson.Get(outputStr, "parallel_tool_calls")
	if !parallelCalls.Bool() {
		t.Error("parallel_tool_calls should be true")
	}

	include := gjson.Get(outputStr, "include")
	if !include.IsArray() || len(include.Array()) != 1 {
		t.Error("include should be an array with one element")
	} else if include.Array()[0].String() != "reasoning.encrypted_content" {
		t.Errorf("Expected include[0] to be 'reasoning.encrypted_content', got '%s'", include.Array()[0].String())
	}
}

func TestConvertOpenAIResponsesRequestToCodex_PreservesCustomToolCallItems(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"custom_tool_call","call_id":"call_apply","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"call_apply","output":"OK"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "input.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("input.0.type = %q, want custom_tool_call", got)
	}
	if got := gjson.Get(outputStr, "input.0.input").String(); got != "*** Begin Patch" {
		t.Fatalf("input.0.input = %q, want custom tool input", got)
	}
	if got := gjson.Get(outputStr, "input.1.type").String(); got != "custom_tool_call_output" {
		t.Fatalf("input.1.type = %q, want custom_tool_call_output", got)
	}
}

func TestConvertOpenAIResponsesRequestToCodex_PreservesExplicitStoreFalse(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": "Say hello.",
		"store": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	store := gjson.Get(outputStr, "store")
	if store.Type != gjson.False {
		t.Errorf("Expected explicit store=false to be preserved, got %s", store.Raw)
	}
}

// TestConvertSystemRoleToDeveloper_AssistantRole tests that assistant role is preserved
func TestConvertSystemRoleToDeveloper_AssistantRole(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check system -> developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	// Check user unchanged
	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "user" {
		t.Errorf("Expected second role 'user', got '%s'", secondRole.String())
	}

	// Check assistant unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "assistant" {
		t.Errorf("Expected third role 'assistant', got '%s'", thirdRole.String())
	}
}

func TestUserFieldDeletion(t *testing.T) {
	inputJSON := []byte(`{  
		"model": "gpt-5.2",  
		"user": "test-user",  
		"input": [{"role": "user", "content": "Hello"}]  
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify user field is deleted
	userField := gjson.Get(outputStr, "user")
	if userField.Exists() {
		t.Errorf("user field should be deleted, but it was found with value: %s", userField.Raw)
	}
}

func TestServiceTierPreserved(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4",
		"service_tier": "fast",
		"input": [{"role": "user", "content": "Hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	serviceTier := gjson.Get(outputStr, "service_tier")
	if !serviceTier.Exists() {
		t.Fatal("service_tier should be preserved")
	}
	if serviceTier.String() != "fast" {
		t.Fatalf("service_tier = %q, want %q", serviceTier.String(), "fast")
	}
}

func TestFastModeSetsServiceTierWhenMissing(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4",
		"features": {"fast_mode": true},
		"input": [{"role": "user", "content": "Hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	serviceTier := gjson.Get(outputStr, "service_tier")
	if !serviceTier.Exists() {
		t.Fatal("service_tier should be set when features.fast_mode=true")
	}
	if serviceTier.String() != "fast" {
		t.Fatalf("service_tier = %q, want %q", serviceTier.String(), "fast")
	}
}

func TestFastModeDoesNotOverrideExplicitServiceTier(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4",
		"service_tier": "default",
		"features": {"fast_mode": true},
		"input": [{"role": "user", "content": "Hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	serviceTier := gjson.Get(outputStr, "service_tier")
	if !serviceTier.Exists() {
		t.Fatal("service_tier should exist")
	}
	if serviceTier.String() != "default" {
		t.Fatalf("service_tier = %q, want %q", serviceTier.String(), "default")
	}
}

func TestClientTuningFieldsPreserved(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4",
		"model_context_window": 1000000,
		"model_auto_compact_token_limit": 900000,
		"features": {
			"fast_mode": true,
			"enable_request_compression": true
		},
		"input": [{"role": "user", "content": "Hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "model_context_window").Int() != 1000000 {
		t.Fatalf("model_context_window should be preserved")
	}
	if gjson.Get(outputStr, "model_auto_compact_token_limit").Int() != 900000 {
		t.Fatalf("model_auto_compact_token_limit should be preserved")
	}
	if !gjson.Get(outputStr, "features.enable_request_compression").Bool() {
		t.Fatalf("features.enable_request_compression should be preserved")
	}
	if gjson.Get(outputStr, "service_tier").String() != "fast" {
		t.Fatalf("service_tier should default to fast when fast_mode=true")
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesMessageStringContent(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4",
		"input": [
			{"role": "system", "content": "You are concise."},
			{"role": "user", "content": "Hello"},
			{"role": "assistant", "content": "Hi"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "input.0.role").String(); got != "developer" {
		t.Fatalf("input.0.role = %q, want developer", got)
	}
	if got := gjson.Get(outputStr, "input.0.content.0.type").String(); got != "input_text" {
		t.Fatalf("input.0.content.0.type = %q, want input_text", got)
	}
	if got := gjson.Get(outputStr, "input.1.content.0.text").String(); got != "Hello" {
		t.Fatalf("input.1.content.0.text = %q, want Hello", got)
	}
	if got := gjson.Get(outputStr, "input.2.content.0.type").String(); got != "output_text" {
		t.Fatalf("input.2.content.0.type = %q, want output_text", got)
	}
}

func TestConvertOpenAIResponsesRequestToCodex_DropsEmptyMessageContentBlocks(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.4",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": ""}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "ok"}]},
			{"type": "function_call", "call_id": "call_123", "name": "ping", "arguments": "{}"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	items := gjson.Get(outputStr, "input").Array()
	if len(items) != 2 {
		t.Fatalf("len(input) = %d, want 2 after dropping empty message", len(items))
	}
	if got := gjson.Get(outputStr, "input.0.role").String(); got != "assistant" {
		t.Fatalf("input.0.role = %q, want assistant", got)
	}
	if got := gjson.Get(outputStr, "input.0.content.0.text").String(); got != "ok" {
		t.Fatalf("input.0.content.0.text = %q, want ok", got)
	}
	if got := gjson.Get(outputStr, "input.1.type").String(); got != "function_call" {
		t.Fatalf("input.1.type = %q, want function_call", got)
	}
}

func TestConvertOpenAIResponsesRequestToCodex_NormalizesMalformedFunctionArguments(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"function_call","call_id":"call_bad","name":"shell_command","arguments":"{\"command\":\"first\"}{\"command\":\"second\"}"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	gotArgs := gjson.Get(outputStr, "input.0.arguments").String()
	if !json.Valid([]byte(gotArgs)) {
		t.Fatalf("function call arguments should be valid JSON, got %q", gotArgs)
	}
	if got := gjson.Get(gotArgs, "_error").String(); got != "arguments were not valid JSON" {
		t.Fatalf("fallback _error = %q, want %q", got, "arguments were not valid JSON")
	}
	if got := gjson.Get(gotArgs, "_raw_arguments").String(); got != "{\"command\":\"first\"}{\"command\":\"second\"}" {
		t.Fatalf("fallback _raw_arguments = %q", got)
	}
}

func TestConvertOpenAIResponsesRequestToCodex_SkipsMalformedFunctionCallPairs(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"function_call","call_id":"call_bad","name":"shell_command","arguments":"{\"command\":\"first\"}{\"command\":\"second\"}"},
			{"type":"function_call_output","call_id":"call_bad","output":"failed to parse function arguments: trailing characters at line 1 column 10"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := len(gjson.Get(outputStr, "input").Array()); got != 1 {
		t.Fatalf("input length = %d, want 1", got)
	}
	if got := gjson.Get(outputStr, "input.0.role").String(); got != "user" {
		t.Fatalf("input.0.role = %q, want user", got)
	}
	if got := gjson.Get(outputStr, "input.0.content.0.text").String(); got != "continue" {
		t.Fatalf("input.0.content.0.text = %q, want continue", got)
	}
}

func TestConvertOpenAIResponsesRequestToCodex_TrimsProblematicAssistantTailAfterLastUser(t *testing.T) {
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

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := len(gjson.Get(outputStr, "input").Array()); got != 1 {
		t.Fatalf("input length = %d, want 1", got)
	}
	if got := gjson.Get(outputStr, "input.0.role").String(); got != "user" {
		t.Fatalf("input.0.role = %q, want user", got)
	}
	if got := gjson.Get(outputStr, "input.0.content.0.text").String(); got != "continue tracing" {
		t.Fatalf("input.0.content.0.text = %q, want continue tracing", got)
	}
}
