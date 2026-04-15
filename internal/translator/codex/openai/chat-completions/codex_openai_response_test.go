package chat_completions

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAI_IgnoresReasoningSummaryStreamEvents(t *testing.T) {
	var param any
	out := ConvertCodexResponseToOpenAI(
		context.Background(),
		"gpt-5.4",
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		nil,
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"planning...\"}"),
		&param,
	)
	if len(out) != 0 {
		t.Fatalf("expected reasoning summary stream event to be ignored, got %d chunks", len(out))
	}
}

func TestConvertCodexResponseToOpenAINonStream_SkipsReasoningContent(t *testing.T) {
	raw := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_123",
			"model":"gpt-5.4",
			"created_at":1710000000,
			"status":"completed",
			"output":[
				{
					"type":"reasoning",
					"summary":[{"type":"summary_text","text":"first think, then think again"}]
				},
				{
					"type":"message",
					"role":"assistant",
					"content":[{"type":"output_text","text":"final answer"}]
				}
			]
		}
	}`)

	out := ConvertCodexResponseToOpenAINonStream(context.Background(), "gpt-5.4", nil, nil, raw, nil)
	if out == "" {
		t.Fatalf("expected non-empty response")
	}
	if got := gjson.Get(out, "choices.0.message.content").String(); got != "final answer" {
		t.Fatalf("message.content = %q, want %q", got, "final answer")
	}
	if gjson.Get(out, "choices.0.message.reasoning_content").Exists() {
		t.Fatalf("reasoning_content should be omitted from chat compatibility response")
	}
}
