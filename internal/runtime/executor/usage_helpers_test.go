package executor

import "testing"

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":9}}}`)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestParseClaudeUsageIncludesCachedTokensInTotal(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":7}}`)
	detail := parseClaudeUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.TotalTokens != 37 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 37)
	}
}

func TestParseClaudeUsageFallsBackToCacheCreationTokens(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":5}}`)
	detail := parseClaudeUsage(data)
	if detail.CachedTokens != 5 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 5)
	}
	if detail.TotalTokens != 35 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 35)
	}
}

func TestParseClaudeUsageAggregatesCacheReadAndCreationTokens(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":7,"cache_creation_input_tokens":5}}`)
	detail := parseClaudeUsage(data)
	if detail.CachedTokens != 12 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 12)
	}
	if detail.TotalTokens != 42 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 42)
	}
}

func TestParseClaudeStreamUsageIncludesCachedTokensInTotal(t *testing.T) {
	line := []byte("data: {\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"cache_read_input_tokens\":7}}\n\n")
	detail, ok := parseClaudeStreamUsage(line)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.TotalTokens != 37 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 37)
	}
}

func TestParseClaudeStreamUsageAggregatesCacheReadAndCreationTokens(t *testing.T) {
	line := []byte("data: {\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":5}}\n\n")
	detail, ok := parseClaudeStreamUsage(line)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if detail.CachedTokens != 12 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 12)
	}
	if detail.TotalTokens != 42 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 42)
	}
}
