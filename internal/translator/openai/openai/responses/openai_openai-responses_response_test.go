package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_SeparatesParallelToolCalls(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"shell_command","arguments":""}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"first\"}"}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"shell_command","arguments":""}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"command\":\"second\"}"}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}

	var param any
	doneArgs := map[string]string{}
	doneNames := map[string]string{}

	for _, chunk := range chunks {
		events := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-5.4", nil, nil, chunk, &param)
		for _, event := range events {
			dataIdx := strings.Index(event, "\ndata: ")
			if dataIdx == -1 {
				continue
			}
			payload := event[dataIdx+7:]
			switch gjson.Get(payload, "type").String() {
			case "response.function_call_arguments.done":
				doneArgs[gjson.Get(payload, "item_id").String()] = gjson.Get(payload, "arguments").String()
			case "response.output_item.done":
				item := gjson.Get(payload, "item")
				if item.Get("type").String() == "function_call" {
					doneNames[item.Get("call_id").String()] = item.Get("name").String()
				}
			}
		}
	}

	if got := len(doneArgs); got != 2 {
		t.Fatalf("function_call_arguments.done count = %d, want 2", got)
	}
	if got := doneArgs["fc_call_a"]; got != `{"command":"first"}` {
		t.Fatalf("call_a arguments = %q", got)
	}
	if got := doneArgs["fc_call_b"]; got != `{"command":"second"}` {
		t.Fatalf("call_b arguments = %q", got)
	}
	if got := doneNames["call_a"]; got != "shell_command" {
		t.Fatalf("call_a name = %q", got)
	}
	if got := doneNames["call_b"]; got != "shell_command" {
		t.Fatalf("call_b name = %q", got)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_MessageThenToolGetsUniqueOutputIndexes(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"先检查日志"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"shell_command","arguments":""}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_2","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}

	var param any
	msgDoneIndex := int64(-1)
	funcAddedIndex := int64(-1)
	funcDoneIndex := int64(-1)
	completedOutput0 := ""
	completedOutput1 := ""

	for _, chunk := range chunks {
		events := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-5.4", nil, nil, chunk, &param)
		for _, event := range events {
			dataIdx := strings.Index(event, "\ndata: ")
			if dataIdx == -1 {
				continue
			}
			payload := event[dataIdx+7:]
			switch gjson.Get(payload, "type").String() {
			case "response.output_item.done":
				item := gjson.Get(payload, "item")
				switch item.Get("type").String() {
				case "message":
					msgDoneIndex = gjson.Get(payload, "output_index").Int()
				case "function_call":
					funcDoneIndex = gjson.Get(payload, "output_index").Int()
				}
			case "response.output_item.added":
				item := gjson.Get(payload, "item")
				if item.Get("type").String() == "function_call" {
					funcAddedIndex = gjson.Get(payload, "output_index").Int()
				}
			case "response.completed":
				completedOutput0 = gjson.Get(payload, "response.output.0.type").String()
				completedOutput1 = gjson.Get(payload, "response.output.1.type").String()
			}
		}
	}

	if msgDoneIndex != 0 {
		t.Fatalf("message output_index = %d, want 0", msgDoneIndex)
	}
	if funcAddedIndex != 1 {
		t.Fatalf("function added output_index = %d, want 1", funcAddedIndex)
	}
	if funcDoneIndex != 1 {
		t.Fatalf("function done output_index = %d, want 1", funcDoneIndex)
	}
	if completedOutput0 != "message" || completedOutput1 != "function_call" {
		t.Fatalf("unexpected response.output ordering: got [%q, %q]", completedOutput0, completedOutput1)
	}
}

func TestConvertOpenAIChatCompletionsResponseToOpenAIResponses_UsesCustomToolCallType(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-5.4",
		"tools":[{"type":"custom","name":"apply_patch","description":"apply unified diff"}]
	}`)
	chunks := [][]byte{
		[]byte(`data: {"id":"chatcmpl_custom_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_apply","type":"function","function":{"name":"apply_patch","arguments":""}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_custom_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":\"*** Begin Patch\"}"}}]},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_custom_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}

	var param any
	var itemAdded, inputDone, itemDone, completed gjson.Result

	for _, chunk := range chunks {
		events := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-5.4", originalRequest, originalRequest, chunk, &param)
		for _, event := range events {
			dataIdx := strings.Index(event, "\ndata: ")
			if dataIdx == -1 {
				continue
			}
			payload := gjson.Parse(event[dataIdx+7:])
			switch payload.Get("type").String() {
			case "response.output_item.added":
				itemAdded = payload
			case "response.custom_tool_call_input.done":
				inputDone = payload
			case "response.output_item.done":
				itemDone = payload
			case "response.completed":
				completed = payload
			}
		}
	}

	if got := itemAdded.Get("item.type").String(); got != "custom_tool_call" {
		t.Fatalf("itemAdded.item.type = %q, want custom_tool_call", got)
	}
	if got := inputDone.Get("input").String(); got != "*** Begin Patch" {
		t.Fatalf("custom tool input done = %q, want custom tool input", got)
	}
	if got := itemDone.Get("item.type").String(); got != "custom_tool_call" {
		t.Fatalf("itemDone.item.type = %q, want custom_tool_call", got)
	}
	if got := itemDone.Get("item.input").String(); got != "*** Begin Patch" {
		t.Fatalf("itemDone.item.input = %q, want custom tool input", got)
	}
	if got := completed.Get("response.output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("completed response.output.0.type = %q, want custom_tool_call", got)
	}
}
