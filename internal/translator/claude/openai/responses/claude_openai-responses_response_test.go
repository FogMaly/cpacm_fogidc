package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeResponseToOpenAIResponses_TextDoneKeepsVisibleText(t *testing.T) {
	var param any
	var chunks []string
	for _, line := range [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"(1. Status Update)"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"shell_command","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`),
		[]byte(`data: {"type":"message_stop"}`),
	} {
		chunks = append(chunks, ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-sonnet-4-6", nil, nil, line, &param)...)
	}

	var outputTextDone, contentPartDone, outputItemDone, responseCompleted gjson.Result
	for _, chunk := range chunks {
		idx := strings.Index(chunk, "data:")
		if idx < 0 {
			continue
		}
		payload := strings.TrimSpace(chunk[idx+5:])
		event := gjson.Parse(payload)
		switch event.Get("type").String() {
		case "response.output_text.done":
			outputTextDone = event
		case "response.content_part.done":
			contentPartDone = event
		case "response.output_item.done":
			if event.Get("item.type").String() == "message" {
				outputItemDone = event
			}
		case "response.completed":
			responseCompleted = event
		}
	}

	want := "(1. Status Update)"
	if got := outputTextDone.Get("text").String(); got != want {
		t.Fatalf("output_text.done text = %q, want %q", got, want)
	}
	if got := contentPartDone.Get("part.text").String(); got != want {
		t.Fatalf("content_part.done part.text = %q, want %q", got, want)
	}
	if got := outputItemDone.Get("item.content.0.text").String(); got != want {
		t.Fatalf("output_item.done item.content.0.text = %q, want %q", got, want)
	}
	if got := responseCompleted.Get("response.output.0.content.0.text").String(); got != want {
		t.Fatalf("response.completed output text = %q, want %q", got, want)
	}
}

func TestConvertClaudeResponseToOpenAIResponses_UsesCustomToolCallType(t *testing.T) {
	originalRequest := []byte(`{
		"model":"gpt-5.4",
		"tools":[{"type":"custom","name":"apply_patch","description":"apply unified diff"}]
	}`)

	var param any
	var chunks []string
	for _, line := range [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_apply","name":"apply_patch","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"*** Begin Patch\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`),
		[]byte(`data: {"type":"message_stop"}`),
	} {
		chunks = append(chunks, ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-sonnet-4-6", originalRequest, originalRequest, line, &param)...)
	}

	var itemAdded, inputDone, itemDone, completed gjson.Result
	for _, chunk := range chunks {
		idx := strings.Index(chunk, "data:")
		if idx < 0 {
			continue
		}
		payload := strings.TrimSpace(chunk[idx+5:])
		event := gjson.Parse(payload)
		switch event.Get("type").String() {
		case "response.output_item.added":
			itemAdded = event
		case "response.custom_tool_call_input.done":
			inputDone = event
		case "response.output_item.done":
			itemDone = event
		case "response.completed":
			completed = event
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
