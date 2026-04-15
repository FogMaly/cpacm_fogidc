package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeResponseToOpenAI_UsesSequentialToolCallIndexesAndRequestedModel(t *testing.T) {
	originalRequest := []byte(`{"model":"claude-opus-4-6","stream":true}`)
	var param any

	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-6(xhigh)"}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"checking"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"TaskList","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":2}`),
		[]byte(`data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call_2","name":"Bash","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":3}`),
	}

	var toolDeltas []gjson.Result
	for _, chunk := range chunks {
		out := ConvertClaudeResponseToOpenAI(context.Background(), "claude-opus-4-6(xhigh)", originalRequest, nil, chunk, &param)
		for _, item := range out {
			root := gjson.Parse(item)
			if root.Get("choices.0.delta.tool_calls").Exists() {
				toolDeltas = append(toolDeltas, root.Get("choices.0.delta.tool_calls.0"))
			}
			if root.Get("model").Exists() && root.Get("model").String() != "claude-opus-4-6" {
				t.Fatalf("stream model = %q, want %q", root.Get("model").String(), "claude-opus-4-6")
			}
		}
	}

	if len(toolDeltas) != 2 {
		t.Fatalf("tool delta count = %d, want 2", len(toolDeltas))
	}
	if got := toolDeltas[0].Get("index").Int(); got != 0 {
		t.Fatalf("first tool index = %d, want 0", got)
	}
	if got := toolDeltas[1].Get("index").Int(); got != 1 {
		t.Fatalf("second tool index = %d, want 1", got)
	}
	if got := toolDeltas[1].Get("function.arguments").String(); got != `{"command":"pwd"}` {
		t.Fatalf("second tool args = %q", got)
	}
}

func TestConvertClaudeResponseToOpenAINonStream_UsesReasoningContentAndRequestedModel(t *testing.T) {
	raw := []byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-6(xhigh)"}} 
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first "}}
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"second"}}
data: {"type":"content_block_stop","index":0}
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"TaskList","input":{}}}
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}
data: {"type":"content_block_stop","index":1}
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":5}}
data: {"type":"message_stop"}`)

	originalRequest := []byte(`{"model":"claude-opus-4-6"}`)
	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "claude-opus-4-6(xhigh)", originalRequest, nil, raw, nil)
	root := gjson.Parse(out)

	if got := root.Get("model").String(); got != "claude-opus-4-6" {
		t.Fatalf("model = %q, want %q", got, "claude-opus-4-6")
	}
	if got := root.Get("choices.0.message.reasoning_content").String(); got != "first second" {
		t.Fatalf("reasoning_content = %q, want %q", got, "first second")
	}
	if root.Get("choices.0.message.reasoning").Exists() {
		t.Fatalf("unexpected legacy reasoning field: %s", root.Get("choices.0.message.reasoning").Raw)
	}
	if got := root.Get("choices.0.message.tool_calls.0.index").Int(); got != 0 {
		t.Fatalf("tool index = %d, want 0", got)
	}
}

func TestConvertClaudeResponseToOpenAI_UsesMessageStartUsageWhenMessageDeltaOnlyHasOutputTokens(t *testing.T) {
	originalRequest := []byte(`{"model":"claude-opus-4-6","stream":true}`)
	var param any

	var last gjson.Result
	for _, chunk := range [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-6(xhigh)","usage":{"input_tokens":111,"cache_read_input_tokens":7,"cache_creation_input_tokens":3,"output_tokens":0}}}`),
		[]byte(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`),
	} {
		out := ConvertClaudeResponseToOpenAI(context.Background(), "claude-opus-4-6(xhigh)", originalRequest, nil, chunk, &param)
		if len(out) > 0 {
			last = gjson.Parse(out[len(out)-1])
		}
	}

	if got := last.Get("usage.prompt_tokens").Int(); got != 114 {
		t.Fatalf("usage.prompt_tokens = %d, want 114", got)
	}
	if got := last.Get("usage.completion_tokens").Int(); got != 9 {
		t.Fatalf("usage.completion_tokens = %d, want 9", got)
	}
	if got := last.Get("usage.total_tokens").Int(); got != 123 {
		t.Fatalf("usage.total_tokens = %d, want 123", got)
	}
	if got := last.Get("usage.prompt_tokens_details.cached_tokens").Int(); got != 7 {
		t.Fatalf("usage.prompt_tokens_details.cached_tokens = %d, want 7", got)
	}
}

func TestConvertClaudeResponseToOpenAINonStream_UsesMessageStartUsageWithoutMessageDeltaUsage(t *testing.T) {
	raw := []byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-6(xhigh)","usage":{"input_tokens":88,"cache_read_input_tokens":5,"cache_creation_input_tokens":2,"output_tokens":0}}}
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}
data: {"type":"content_block_stop","index":0}
data: {"type":"message_stop"}`)

	originalRequest := []byte(`{"model":"claude-opus-4-6"}`)
	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "claude-opus-4-6(xhigh)", originalRequest, nil, raw, nil)
	root := gjson.Parse(out)

	if got := root.Get("usage.prompt_tokens").Int(); got != 90 {
		t.Fatalf("usage.prompt_tokens = %d, want 90", got)
	}
	if got := root.Get("usage.completion_tokens").Int(); got != 0 {
		t.Fatalf("usage.completion_tokens = %d, want 0", got)
	}
	if got := root.Get("usage.total_tokens").Int(); got != 90 {
		t.Fatalf("usage.total_tokens = %d, want 90", got)
	}
	if got := root.Get("usage.prompt_tokens_details.cached_tokens").Int(); got != 5 {
		t.Fatalf("usage.prompt_tokens_details.cached_tokens = %d, want 5", got)
	}
}

func TestConvertClaudeResponseToOpenAI_EmitsTerminalChunkWhenUpstreamSkipsMessageDelta(t *testing.T) {
	originalRequest := []byte(`{"model":"claude-opus-4-6","stream":true}`)
	var param any

	var chunks []string
	for _, chunk := range [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-6(xhigh)","usage":{"input_tokens":18,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"Bash","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":2}`),
		[]byte(`data: {"type":"message_stop"}`),
	} {
		chunks = append(chunks, ConvertClaudeResponseToOpenAI(context.Background(), "claude-opus-4-6(xhigh)", originalRequest, nil, chunk, &param)...)
	}

	if len(chunks) == 0 {
		t.Fatal("no stream chunks emitted")
	}
	last := gjson.Parse(chunks[len(chunks)-1])
	if got := last.Get("choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want %q", got, "tool_calls")
	}
	if got := last.Get("usage.prompt_tokens").Int(); got != 18 {
		t.Fatalf("usage.prompt_tokens = %d, want 18", got)
	}
}
