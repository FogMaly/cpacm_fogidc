package claude

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponseToClaudeNonStream_NormalizesModelAndStripsThinkingForNonThinkingAlias(t *testing.T) {
	raw := []byte(`{
		"id":"resp_1",
		"model":"claude-opus-4-6-thinking",
		"choices":[
			{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"pong",
					"reasoning_content":"internal"
				},
				"finish_reason":"stop"
			}
		],
		"usage":{"prompt_tokens":8,"completion_tokens":19}
	}`)

	out := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "claude-opus-4-6", nil, nil, raw, nil)
	root := gjson.Parse(out)

	if got := root.Get("model").String(); got != "claude-opus-4-6" {
		t.Fatalf("model = %q, want %q", got, "claude-opus-4-6")
	}
	if got := len(root.Get("content").Array()); got != 1 {
		t.Fatalf("content length = %d, want 1", got)
	}
	if got := root.Get("content.0.type").String(); got != "text" {
		t.Fatalf("content.0.type = %q, want %q", got, "text")
	}
}

func TestConvertOpenAIResponseToClaudeNonStream_PreservesThinkingForThinkingAlias(t *testing.T) {
	raw := []byte(`{
		"id":"resp_1",
		"model":"claude-opus-4-6-thinking",
		"choices":[
			{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"pong",
					"reasoning_content":"internal"
				},
				"finish_reason":"stop"
			}
		]
	}`)

	out := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "claude-opus-4-6-thinking", nil, nil, raw, nil)
	root := gjson.Parse(out)

	if got := root.Get("model").String(); got != "claude-opus-4-6-thinking" {
		t.Fatalf("model = %q, want %q", got, "claude-opus-4-6-thinking")
	}
	foundThinking := false
	for _, item := range root.Get("content").Array() {
		if item.Get("type").String() == "thinking" {
			foundThinking = true
			break
		}
	}
	if !foundThinking {
		t.Fatal("expected thinking block to be preserved for thinking alias")
	}
}

func TestConvertOpenAIResponseToClaudeNonStream_SupportsResponsesOutputText(t *testing.T) {
	raw := []byte(`{
		"id":"resp_1",
		"model":"gpt-5.4",
		"output":[
			{
				"type":"message",
				"role":"assistant",
				"content":[
					{"type":"output_text","text":"OK"}
				]
			}
		],
		"usage":{"input_tokens":8,"output_tokens":19}
	}`)

	out := ConvertOpenAIResponseToClaudeNonStream(context.Background(), "claude-opus-4-6", nil, nil, raw, nil)
	root := gjson.Parse(out)

	if got := root.Get("content.0.type").String(); got != "text" {
		t.Fatalf("content.0.type = %q, want %q", got, "text")
	}
	if got := root.Get("content.0.text").String(); got != "OK" {
		t.Fatalf("content.0.text = %q, want %q", got, "OK")
	}
}
