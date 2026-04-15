package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponseToOpenAINonStream_NormalizesModelAndStripsReasoningForNonThinkingAlias(t *testing.T) {
	raw := []byte(`{
		"id":"resp_1",
		"object":"chat.completion",
		"model":"claude-opus-4-6-thinking",
		"choices":[
			{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"pong",
					"reasoning_content":"internal",
					"reasoning":"internal2"
				},
				"finish_reason":"stop"
			}
		]
	}`)

	out := ConvertOpenAIResponseToOpenAINonStream(context.Background(), "claude-opus-4-6", nil, nil, raw, nil)
	root := gjson.Parse(out)

	if got := root.Get("model").String(); got != "claude-opus-4-6" {
		t.Fatalf("model = %q, want %q", got, "claude-opus-4-6")
	}
	if root.Get("choices.0.message.reasoning_content").Exists() {
		t.Fatal("reasoning_content should be stripped for non-thinking alias")
	}
	if root.Get("choices.0.message.reasoning").Exists() {
		t.Fatal("reasoning should be stripped for non-thinking alias")
	}
}

func TestConvertOpenAIResponseToOpenAI_StreamNormalizesModelAndStripsReasoningDelta(t *testing.T) {
	raw := []byte(`data: {"id":"resp_1","object":"chat.completion.chunk","model":"claude-opus-4-6-thinking","choices":[{"index":0,"delta":{"reasoning_content":"internal","content":"pong"}}]}`)

	out := ConvertOpenAIResponseToOpenAI(context.Background(), "claude-opus-4-6", nil, nil, raw, nil)
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	root := gjson.Parse(out[0])

	if got := root.Get("model").String(); got != "claude-opus-4-6" {
		t.Fatalf("model = %q, want %q", got, "claude-opus-4-6")
	}
	if root.Get("choices.0.delta.reasoning_content").Exists() {
		t.Fatal("reasoning_content delta should be stripped for non-thinking alias")
	}
	if got := root.Get("choices.0.delta.content").String(); got != "pong" {
		t.Fatalf("content = %q, want %q", got, "pong")
	}
}

func TestConvertOpenAIResponseToOpenAI_StreamDropsDoneEvent(t *testing.T) {
	out := ConvertOpenAIResponseToOpenAI(context.Background(), "gpt-5.4", nil, nil, []byte(`data: [DONE]`), nil)
	if len(out) != 0 {
		t.Fatalf("len(out) = %d, want 0", len(out))
	}
}
