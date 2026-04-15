package executor

import (
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestNormalizeOpenAIResponsesReplayRequest_DropsMissingTextBlocks(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"assistant","content":[{"type":"output_text"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"READY"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":""}]},
			{"type":"function_call","call_id":"call_1","name":"shell_command","arguments":"{}"}
		]
	}`)

	normalized := normalizeOpenAIResponsesReplayRequest(raw)

	if got := len(gjson.GetBytes(normalized, "input").Array()); got != 2 {
		t.Fatalf("input length = %d, want 2", got)
	}
	if got := gjson.GetBytes(normalized, "input.0.role").String(); got != "assistant" {
		t.Fatalf("input.0.role = %q, want assistant", got)
	}
	if got := gjson.GetBytes(normalized, "input.0.content.0.text").String(); got != "READY" {
		t.Fatalf("input.0.content.0.text = %q, want READY", got)
	}
	if got := gjson.GetBytes(normalized, "input.1.type").String(); got != "function_call" {
		t.Fatalf("input.1.type = %q, want function_call", got)
	}
}

func TestMergeOpenAIResponsesReplayTemplate_SanitizesStoredAssistantMessage(t *testing.T) {
	previousTemplate := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Remember the codeword."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"READY"}]}
		]
	}`)
	currentRequest := []byte(`{
		"model":"gpt-5.4",
		"previous_response_id":"resp_prev",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"What was it?"}]}
		]
	}`)

	merged, ok := mergeOpenAIResponsesReplayTemplate(previousTemplate, currentRequest)
	if !ok {
		t.Fatal("mergeOpenAIResponsesReplayTemplate returned ok=false")
	}

	if got := len(gjson.GetBytes(merged, "input").Array()); got != 3 {
		t.Fatalf("input length = %d, want 3", got)
	}
	if got := gjson.GetBytes(merged, "input.1.content.0.text").String(); got != "READY" {
		t.Fatalf("input.1.content.0.text = %q, want READY", got)
	}
	if got := gjson.GetBytes(merged, "input.2.content.0.text").String(); got != "What was it?" {
		t.Fatalf("input.2.content.0.text = %q, want %q", got, "What was it?")
	}
}

func TestMergeOpenAIResponsesReplayTemplate_DropsInheritedToolChoiceForToolOutputs(t *testing.T) {
	previousTemplate := []byte(`{
		"model":"gpt-5.4",
		"tool_choice":"required",
		"tools":[{"type":"function","name":"get_time","parameters":{"type":"object","properties":{},"additionalProperties":false}}],
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"What time is it?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_time","arguments":"{}"}
		]
	}`)
	currentRequest := []byte(`{
		"model":"gpt-5.4",
		"previous_response_id":"resp_prev",
		"input":[
			{"type":"function_call_output","call_id":"call_1","output":"2026-04-02T17:40:00Z"}
		]
	}`)

	merged, ok := mergeOpenAIResponsesReplayTemplate(previousTemplate, currentRequest)
	if !ok {
		t.Fatal("mergeOpenAIResponsesReplayTemplate returned ok=false")
	}

	if gjson.GetBytes(merged, "tool_choice").Exists() {
		t.Fatalf("tool_choice = %s, want deleted for function_call_output continuation", gjson.GetBytes(merged, "tool_choice").Raw)
	}
	if got := gjson.GetBytes(merged, "tools.0.name").String(); got != "get_time" {
		t.Fatalf("tools.0.name = %q, want get_time", got)
	}
	if got := len(gjson.GetBytes(merged, "input").Array()); got != 3 {
		t.Fatalf("input length = %d, want 3", got)
	}
	if got := gjson.GetBytes(merged, "input.2.type").String(); got != "function_call_output" {
		t.Fatalf("input.2.type = %q, want function_call_output", got)
	}
}

func TestBuildOpenAIResponsesReplayTemplate_PreservesCustomToolCalls(t *testing.T) {
	requestRaw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"edit file"}]}
		]
	}`)
	responseRaw := []byte(`{
		"output":[
			{"id":"ctc_call_apply","type":"custom_tool_call","status":"completed","call_id":"call_apply","name":"apply_patch","input":"*** Begin Patch"}
		]
	}`)

	template := buildOpenAIResponsesReplayTemplate(requestRaw, responseRaw)

	if got := len(gjson.GetBytes(template, "input").Array()); got != 2 {
		t.Fatalf("input length = %d, want 2", got)
	}
	if got := gjson.GetBytes(template, "input.1.type").String(); got != "custom_tool_call" {
		t.Fatalf("input.1.type = %q, want custom_tool_call", got)
	}
	if got := gjson.GetBytes(template, "input.1.input").String(); got != "*** Begin Patch" {
		t.Fatalf("input.1.input = %q, want custom tool input", got)
	}
}

func TestBuildOpenAIResponsesReplayTemplate_StripsResponseMetadataFromMessages(t *testing.T) {
	requestRaw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Remember the codeword."}]}
		]
	}`)
	responseRaw := []byte(`{
		"output":[
			{"id":"msg_1","type":"message","status":"completed","phase":"final_answer","role":"assistant","content":[{"type":"output_text","text":"READY"}]}
		]
	}`)

	template := buildOpenAIResponsesReplayTemplate(requestRaw, responseRaw)

	if got := gjson.GetBytes(template, "input.1.type").String(); got != "message" {
		t.Fatalf("input.1.type = %q, want message", got)
	}
	if got := gjson.GetBytes(template, "input.1.role").String(); got != "assistant" {
		t.Fatalf("input.1.role = %q, want assistant", got)
	}
	if gjson.GetBytes(template, "input.1.id").Exists() {
		t.Fatalf("input.1.id should be stripped, got %s", gjson.GetBytes(template, "input.1.id").Raw)
	}
	if gjson.GetBytes(template, "input.1.status").Exists() {
		t.Fatalf("input.1.status should be stripped, got %s", gjson.GetBytes(template, "input.1.status").Raw)
	}
	if gjson.GetBytes(template, "input.1.phase").Exists() {
		t.Fatalf("input.1.phase should be stripped, got %s", gjson.GetBytes(template, "input.1.phase").Raw)
	}
	if got := gjson.GetBytes(template, "input.1.content.0.text").String(); got != "READY" {
		t.Fatalf("input.1.content.0.text = %q, want READY", got)
	}
}

func TestSetCodexBridgeReplay_EvictsOldestWhenBoundsExceeded(t *testing.T) {
	resetCodexBridgeReplayStateForTest(t)
	codexBridgeReplayMaxEntries = 2
	codexBridgeReplayMaxTotalSize = 12
	codexBridgeReplayMaxEntrySize = 64

	expire := time.Now().Add(time.Hour)
	setCodexBridgeReplay([]string{"resp_1"}, []byte("AAAAAA"), expire)
	time.Sleep(2 * time.Millisecond)
	setCodexBridgeReplay([]string{"resp_2"}, []byte("BBBBBB"), expire)
	time.Sleep(2 * time.Millisecond)
	setCodexBridgeReplay([]string{"resp_3"}, []byte("CCCCCC"), expire)

	if _, ok := getCodexBridgeReplay("resp_1"); ok {
		t.Fatal("resp_1 should be evicted")
	}
	if got, ok := getCodexBridgeReplay("resp_2"); !ok || string(got) != "BBBBBB" {
		t.Fatalf("resp_2 = %q, ok=%v; want BBBBBB, true", string(got), ok)
	}
	if got, ok := getCodexBridgeReplay("resp_3"); !ok || string(got) != "CCCCCC" {
		t.Fatalf("resp_3 = %q, ok=%v; want CCCCCC, true", string(got), ok)
	}
	if got := len(codexBridgeReplayMap); got != 2 {
		t.Fatalf("replay map size = %d, want 2", got)
	}
	if codexBridgeReplayB > int64(codexBridgeReplayMaxTotalSize) {
		t.Fatalf("replay bytes = %d, want <= %d", codexBridgeReplayB, codexBridgeReplayMaxTotalSize)
	}
}

func TestSetCodexBridgeReplay_SkipsOversizedPayload(t *testing.T) {
	resetCodexBridgeReplayStateForTest(t)
	codexBridgeReplayMaxEntries = 8
	codexBridgeReplayMaxTotalSize = 1024
	codexBridgeReplayMaxEntrySize = 4

	setCodexBridgeReplay([]string{"resp_big"}, []byte("TOO-LARGE"), time.Now().Add(time.Hour))

	if _, ok := getCodexBridgeReplay("resp_big"); ok {
		t.Fatal("oversized payload should not be cached")
	}
	if got := len(codexBridgeReplayMap); got != 0 {
		t.Fatalf("replay map size = %d, want 0", got)
	}
	if codexBridgeReplayB != 0 {
		t.Fatalf("replay bytes = %d, want 0", codexBridgeReplayB)
	}
}

func TestGetCodexBridgeReplay_PrunesExpiredEntry(t *testing.T) {
	resetCodexBridgeReplayStateForTest(t)
	codexBridgeReplayMaxEntries = 8
	codexBridgeReplayMaxTotalSize = 1024
	codexBridgeReplayMaxEntrySize = 64

	setCodexBridgeReplay([]string{"resp_expired"}, []byte("alive"), time.Now().Add(-time.Second))

	if _, ok := getCodexBridgeReplay("resp_expired"); ok {
		t.Fatal("expired payload should not be returned")
	}
	if got := len(codexBridgeReplayMap); got != 0 {
		t.Fatalf("replay map size = %d, want 0", got)
	}
	if codexBridgeReplayB != 0 {
		t.Fatalf("replay bytes = %d, want 0", codexBridgeReplayB)
	}
}

func TestCodexCache_RequestRoutingUsesPromptCacheKeyWhenAvailable(t *testing.T) {
	cache := codexCache{
		ID:             "resp_prev",
		PromptCacheKey: "conv_upstream_1",
	}

	if got := cache.requestPromptCacheKey(); got != "conv_upstream_1" {
		t.Fatalf("requestPromptCacheKey() = %q, want %q", got, "conv_upstream_1")
	}
	if got := cache.requestConversationID(); got != "conv_upstream_1" {
		t.Fatalf("requestConversationID() = %q, want %q", got, "conv_upstream_1")
	}
	if got := cache.requestSessionID(); got != "conv_upstream_1" {
		t.Fatalf("requestSessionID() = %q, want %q", got, "conv_upstream_1")
	}
}

func TestSetCodexResponseCache_PreservesPromptCacheKey(t *testing.T) {
	resetCodexCacheStateForTest(t)

	cache := codexCache{
		ID:             "resp_seed",
		PromptCacheKey: "conv_upstream_2",
		Expire:         time.Now().Add(time.Hour),
	}
	setCodexResponseCache([]string{"resp_1"}, cache)

	got, ok := getCodexResponseCache("resp_1")
	if !ok {
		t.Fatal("expected response cache entry")
	}
	if got.PromptCacheKey != "conv_upstream_2" {
		t.Fatalf("PromptCacheKey = %q, want %q", got.PromptCacheKey, "conv_upstream_2")
	}
}

func resetCodexBridgeReplayStateForTest(t *testing.T) {
	t.Helper()
	codexCacheMu.Lock()
	oldMap := codexBridgeReplayMap
	oldBytes := codexBridgeReplayB
	oldMaxEntries := codexBridgeReplayMaxEntries
	oldMaxTotal := codexBridgeReplayMaxTotalSize
	oldMaxEntry := codexBridgeReplayMaxEntrySize

	codexBridgeReplayMap = make(map[string]codexBridgeReplayState)
	codexBridgeReplayB = 0
	codexCacheMu.Unlock()

	t.Cleanup(func() {
		codexCacheMu.Lock()
		codexBridgeReplayMap = oldMap
		codexBridgeReplayB = oldBytes
		codexBridgeReplayMaxEntries = oldMaxEntries
		codexBridgeReplayMaxTotalSize = oldMaxTotal
		codexBridgeReplayMaxEntrySize = oldMaxEntry
		codexCacheMu.Unlock()
	})
}

func resetCodexCacheStateForTest(t *testing.T) {
	t.Helper()
	codexCacheMu.Lock()
	oldMap := codexCacheMap
	codexCacheMap = make(map[string]codexCache)
	codexCacheMu.Unlock()

	t.Cleanup(func() {
		codexCacheMu.Lock()
		codexCacheMap = oldMap
		codexCacheMu.Unlock()
	})
}
