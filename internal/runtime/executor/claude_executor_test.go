package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestApplyClaudeToolPrefix(t *testing.T) {
	input := []byte(`{"tools":[{"name":"alpha"},{"name":"proxy_bravo"}],"tool_choice":{"type":"tool","name":"charlie"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"delta","id":"t1","input":{}}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_alpha" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_alpha")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_bravo" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_bravo")
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "proxy_charlie" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "proxy_charlie")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_delta" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_delta")
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinTools(t *testing.T) {
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"my_custom_tool","input_schema":{"type":"object"}}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("built-in tool name should not be prefixed: tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_my_custom_tool" {
		t.Fatalf("custom tool should be prefixed: tools.1.name = %q, want %q", got, "proxy_my_custom_tool")
	}
}

func TestStripClaudeToolPrefixFromResponse(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_use","name":"proxy_alpha","id":"t1","input":{}},{"type":"tool_use","name":"bravo","id":"t2","input":{}}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.name").String(); got != "alpha" {
		t.Fatalf("content.0.name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "bravo" {
		t.Fatalf("content.1.name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromStreamLine(t *testing.T) {
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"proxy_alpha","id":"t1"},"index":0}`)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.name").String(); got != "alpha" {
		t.Fatalf("content_block.name = %q, want %q", got, "alpha")
	}
}

func TestNormalizeClaudeMessagesPayload_DropsEmptyTextMessages(t *testing.T) {
	input := []byte(`{
		"system":[
			{"type":"text","text":""},
			{"type":"text","text":"Keep system"}
		],
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":""},
			{"role":"assistant","content":[
				{"type":"text","text":""},
				{"type":"tool_use","id":"tool_1","name":"shell_command","input":{"command":"pwd"}}
			]},
			{"role":"user","content":[
				{"type":"text","text":""},
				{"type":"text","text":"Reply with exactly OK."}
			]}
		]
	}`)

	out := normalizeClaudeMessagesPayload(input)

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 3 {
		t.Fatalf("messages length = %d, want 3", got)
	}
	if gjson.GetBytes(out, "messages.1.content").Type == gjson.String {
		t.Fatalf("messages.1.content should not be empty string: %s", out)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.type").String(); got != "tool_use" {
		t.Fatalf("messages.1.content.0.type = %q, want tool_use", got)
	}
	if got := gjson.GetBytes(out, "messages.2.content.0.text").String(); got != "Reply with exactly OK." {
		t.Fatalf("messages.2.content.0.text = %q, want Reply with exactly OK.", got)
	}
	if got := len(gjson.GetBytes(out, "system").Array()); got != 1 {
		t.Fatalf("system length = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "system.0.text").String(); got != "Keep system" {
		t.Fatalf("system.0.text = %q, want Keep system", got)
	}
}

func TestNormalizeClaudeFinalResponse_OpenAINonThinkingAlias(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6-thinking",
		"choices":[
			{
				"message":{
					"role":"assistant",
					"content":"pong",
					"reasoning_content":"internal"
				},
				"finish_reason":"stop"
			}
		]
	}`)

	out := normalizeClaudeFinalResponse(input, "whitedream-max/claude-opus-4-6", "openai")

	if got := gjson.GetBytes(out, "model").String(); got != "claude-opus-4-6" {
		t.Fatalf("model = %q, want %q", got, "claude-opus-4-6")
	}
	if gjson.GetBytes(out, "choices.0.message.reasoning_content").Exists() {
		t.Fatal("expected reasoning_content to be removed for non-thinking alias")
	}
}

func TestShouldUseClaudeNativePassthrough_YunyiRelayByDefault(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Prefix: "yunyi-claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": "https://cdn1.yunyi.cfd/claude",
		},
	}

	if !shouldUseClaudeNativePassthrough(auth, sdktranslator.FromString("claude"), sdktranslator.FromString("claude")) {
		t.Fatal("expected yunyi-claude native passthrough to be enabled by default")
	}
}

func TestNormalizeClaudeFinalResponse_ClaudeNonThinkingAlias(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6-thinking",
		"content":[
			{"type":"thinking","thinking":"internal"},
			{"type":"text","text":"pong"}
		],
		"stop_reason":"end_turn"
	}`)

	out := normalizeClaudeFinalResponse(input, "whitedream-max/claude-opus-4-6", "claude")

	if got := gjson.GetBytes(out, "model").String(); got != "claude-opus-4-6" {
		t.Fatalf("model = %q, want %q", got, "claude-opus-4-6")
	}
	if got := len(gjson.GetBytes(out, "content").Array()); got != 1 {
		t.Fatalf("content length = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "content.0.type").String(); got != "text" {
		t.Fatalf("content.0.type = %q, want text", got)
	}
}

func TestClaudeShouldRejectForeignAssistantIdentityResponse(t *testing.T) {
	response := []byte(`{
		"content":[
			{"type":"text","text":"你好！我是 Cursor，由 Anysphere 开发的 AI 助手。"}
		],
		"usage":{"output_tokens":21}
	}`)

	if !claudeShouldRejectForeignAssistantIdentityResponse(response) {
		t.Fatal("expected foreign assistant identity response to be rejected")
	}
}

func TestClaudeShouldRejectForeignAssistantIdentityResponse_WithMarkdownAndThinking(t *testing.T) {
	response := []byte(`{
		"content":[
			{"type":"thinking","thinking":"internal"},
			{"type":"text","text":"你好！我是 **Cursor**，由 **Anysphere** 开发的 AI 助手。"}
		],
		"usage":{"output_tokens":21}
	}`)

	if !claudeShouldRejectForeignAssistantIdentityResponse(response) {
		t.Fatal("expected markdown foreign assistant identity response to be rejected")
	}
}

func TestClaudeShouldRejectForeignAssistantIdentityResponse_OpenAIChatPayload(t *testing.T) {
	response := []byte(`{
		"choices":[
			{
				"message":{
					"role":"assistant",
					"content":"Hello! I'm **Cursor**, an AI assistant developed by **Anysphere**."
				},
				"finish_reason":"stop"
			}
		],
		"usage":{"completion_tokens":21}
	}`)

	if !claudeShouldRejectForeignAssistantIdentityResponse(response) {
		t.Fatal("expected OpenAI-style foreign assistant identity response to be rejected")
	}
}

func TestClaudeShouldRejectInterruptedAgentScaffoldResponse(t *testing.T) {
	requestPayload := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"继续"}]}`)
	response := []byte(`{
		"content":[
			{"type":"text","text":"Searched for 1 pattern, read 2 files (ctrl+o to expand)\n\nInterrupted · What should Claude do"}
		],
		"usage":{"output_tokens":14}
	}`)

	if !claudeShouldRejectInterruptedAgentScaffoldResponse(requestPayload, response) {
		t.Fatal("expected interrupted agent scaffold response to be rejected")
	}
}

func TestClaudeShouldNotRejectForeignAssistantIdentityResponse_WhenUserMentionsCursor(t *testing.T) {
	response := []byte(`{
		"content":[
			{"type":"text","text":"Cursor 是 Anysphere 开发的编辑器，不是 Claude。"}
		],
		"usage":{"output_tokens":21}
	}`)

	if claudeShouldRejectForeignAssistantIdentityResponse(response) {
		t.Fatal("did not expect explanatory Cursor mention to be rejected")
	}
}

func TestClaudePrepareRequest_UsesConfiguredXAPIKeyHeader(t *testing.T) {
	exec := NewClaudeExecutor(nil)
	req, err := http.NewRequest(http.MethodPost, "https://rsxermu666.cn/v1/messages", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":          "sk-covs-test",
			"header:x-api-key": "sk-covs-test",
		},
	}

	if err := exec.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest: %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-covs-test" {
		t.Fatalf("x-api-key = %q, want %q", got, "sk-covs-test")
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
}

func TestClaudePrepareRequest_DefaultsToBearerForCustomBaseWithoutXAPIKeyHeader(t *testing.T) {
	exec := NewClaudeExecutor(nil)
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/messages", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key": "sk-custom-test",
		},
	}

	if err := exec.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-custom-test" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer sk-custom-test")
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key = %q, want empty", got)
	}
}

func TestClaudeExecute_NormalizesNativePayloadForThirdPartyRelay(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_123",
			"type":"message",
			"role":"assistant",
			"model":"claude-sonnet-4-6",
			"content":[{"type":"text","text":"OK"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":1}
		}`)
	}))
	defer server.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":[{"type":"text","text":"keep system"}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hello"}]}
		],
		"max_tokens":32
	}`)

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gjson.GetBytes(capturedBody, "system").Exists() {
		t.Fatalf("expected relay payload to inline system blocks, got %s", capturedBody)
	}
	if got := gjson.GetBytes(capturedBody, "messages.0.content.0.text").String(); !strings.Contains(got, "keep system") {
		t.Fatalf("messages.0.content.0.text = %q, want injected system text", got)
	}
	if got := gjson.GetBytes(capturedBody, "messages.0.content.1.text").String(); got != "hello" {
		t.Fatalf("messages.0.content.1.text = %q, want hello", got)
	}
	if !strings.Contains(string(capturedBody), claudeThirdPartyRelaySystemPrefix) {
		t.Fatalf("captured body should inject relay system prefix: %s", capturedBody)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "OK" {
		t.Fatalf("content.0.text = %q, want OK", got)
	}
}

func TestApplyClaudeHeaders_OfficialAnthropicUsesClaudeCodeProfile(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key": "sk-ant-test",
		},
	}

	applyClaudeHeaders(req, auth, "sk-ant-test", true, []string{"files-api-2025-04-14"}, false)

	if got := req.Header.Get("x-api-key"); got != "sk-ant-test" {
		t.Fatalf("x-api-key = %q, want %q", got, "sk-ant-test")
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
	if got := req.Header.Get("Anthropic-Beta"); !strings.Contains(got, "claude-code-20250219") || !strings.Contains(got, "files-api-2025-04-14") {
		t.Fatalf("Anthropic-Beta = %q, want official Claude Code betas plus extra beta", got)
	}
	if got := req.Header.Get("X-Stainless-Helper-Method"); got != "stream" {
		t.Fatalf("X-Stainless-Helper-Method = %q, want %q", got, "stream")
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want %q", got, "text/event-stream")
	}
}

func TestApplyClaudeHeaders_ThirdPartyRelayUsesMinimalProfile(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://cdn1.yunyi.cfd/claude/v1/messages", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key": "sk-relay-test",
		},
	}

	applyClaudeHeaders(req, auth, "sk-relay-test", true, []string{"files-api-2025-04-14"}, false)

	if got := req.Header.Get("Authorization"); got != "Bearer sk-relay-test" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer sk-relay-test")
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key = %q, want empty", got)
	}
	if got := req.Header.Get("Anthropic-Beta"); got != "" {
		t.Fatalf("Anthropic-Beta = %q, want empty", got)
	}
	if got := req.Header.Get("X-Stainless-Helper-Method"); got != "" {
		t.Fatalf("X-Stainless-Helper-Method = %q, want empty", got)
	}
	if got := req.Header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q, want %q", got, "2023-06-01")
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want %q", got, "text/event-stream")
	}
}

func TestApplyClaudeHeaders_AppliesReservedProviderHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://llm.whitedream.top/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":                  "sk-relay-test",
			"header:X-NewAPI-Username": "test-user",
			"header:X-NewAPI-Password": "test-password",
		},
	}

	applyClaudeHeaders(req, auth, "sk-relay-test", false, nil, false)

	if got := req.Header.Get("X-NewAPI-Username"); got != "test-user" {
		t.Fatalf("X-NewAPI-Username = %q, want test-user", got)
	}
	if got := req.Header.Get("X-NewAPI-Password"); got != "test-password" {
		t.Fatalf("X-NewAPI-Password = %q, want test-password", got)
	}
}

func TestShouldUseClaudeOpenAICompatProfile(t *testing.T) {
	if !shouldUseClaudeOpenAICompatProfile(&cliproxyauth.Auth{
		Attributes: map[string]string{
			"header:X-NewAPI-Username": "test-user",
		},
	}) {
		t.Fatal("expected X-NewAPI header to trigger OpenAI compat profile")
	}

	if !shouldUseClaudeOpenAICompatProfile(&cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": "https://llm.whitedream.top",
		},
	}) {
		t.Fatal("expected llm.whitedream.top to trigger OpenAI compat profile")
	}

	if shouldUseClaudeOpenAICompatProfile(&cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": "https://api.anthropic.com",
		},
	}) {
		t.Fatal("did not expect official Claude base URL to trigger OpenAI compat profile")
	}
}

func TestClaudeExecute_DelegatesNewAPICompatRequestsToOpenAIChatCompletions(t *testing.T) {
	t.Parallel()

	var path string
	var authz string
	var username string
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authz = r.Header.Get("Authorization")
		username = r.Header.Get("X-NewAPI-Username")
		requestBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl_123",
			"object":"chat.completion",
			"created":1710000000,
			"model":"claude-sonnet-4-6",
			"choices":[{"index":0,"message":{"role":"assistant","content":"SMOKE_OK"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}
		}`)
	}))
	defer server.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":                  "sk-test",
			"base_url":                 server.URL,
			"header:X-NewAPI-Username": "test-user",
		},
	}
	payload := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"reply with SMOKE_OK"}]}],"max_tokens":32}`)

	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if path != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", path)
	}
	if authz != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want Bearer sk-test", authz)
	}
	if username != "test-user" {
		t.Fatalf("X-NewAPI-Username = %q, want test-user", username)
	}
	if got := gjson.GetBytes(requestBody, "messages.0.role").String(); got != "user" {
		t.Fatalf("translated request role = %q, want user", got)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "SMOKE_OK" {
		t.Fatalf("content.0.text = %q, want SMOKE_OK", got)
	}
}

func TestMinimizeClaudeThirdPartyRelayPayload_StripsBridgeFields(t *testing.T) {
	input := []byte(`{
		"system":[
			{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"Keep this system prompt","cache_control":{"type":"ephemeral"}}
		],
		"metadata":{
			"user_id":"user_deadbeef",
			"trace_id":"trace-123"
		},
		"tools":[
			{"name":"shell","cache_control":{"type":"ephemeral"}}
		],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}
			]}
		]
	}`)

	out := minimizeClaudeThirdPartyRelayPayload(input)

	if gjson.GetBytes(out, "system").Exists() {
		t.Fatalf("expected system to be removed, got %s", out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); !strings.Contains(got, "Keep this system prompt") {
		t.Fatalf("messages.0.content.0.text = %q, want injected system text", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.text").String(); got != "hello" {
		t.Fatalf("messages.0.content.1.text = %q, want %q", got, "hello")
	}
	if gjson.GetBytes(out, "metadata.user_id").Exists() {
		t.Fatal("expected metadata.user_id to be removed")
	}
	if got := gjson.GetBytes(out, "metadata.trace_id").String(); got != "trace-123" {
		t.Fatalf("metadata.trace_id = %q, want %q", got, "trace-123")
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatal("expected tool cache_control to be removed")
	}
	if gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() {
		t.Fatal("expected message cache_control to be removed")
	}
}

func TestMinimizeClaudeThirdPartyRelayPayload_PullsSystemMessagesOutOfClaudeNativeMessages(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"system","content":"Return JSON only."},
			{"role":"user","content":"hello"}
		]
	}`)

	out := minimizeClaudeThirdPartyRelayPayload(input)

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 1 {
		t.Fatalf("messages length = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); !strings.Contains(got, "Return JSON only.") || !strings.Contains(got, "hello") {
		t.Fatalf("messages.0.content = %q, want combined system text and user content", got)
	}
}

func TestMinimizeClaudeThirdPartyRelayPayload_PrependsSystemWhenConversationStartsWithAssistant(t *testing.T) {
	input := []byte(`{
		"system":"Return JSON only.",
		"messages":[
			{"role":"assistant","content":"Ready."},
			{"role":"user","content":"Go"}
		]
	}`)

	out := minimizeClaudeThirdPartyRelayPayload(input)

	if got := len(gjson.GetBytes(out, "messages").Array()); got != 3 {
		t.Fatalf("messages length = %d, want 3", got)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); !strings.Contains(got, "Return JSON only.") {
		t.Fatalf("messages.0.content.0.text = %q, want injected system text", got)
	}
}

func TestMinimizeClaudeThirdPartyRelayPayload_ClampsLargePromptOnlyMaxTokens(t *testing.T) {
	largePrompt := strings.Repeat("memory ", 4000)
	input := []byte(`{"max_tokens":4096,"messages":[{"role":"user","content":"` + largePrompt + `"}]}`)

	out := minimizeClaudeThirdPartyRelayPayload(input)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != claudeThirdPartyRelayLargePromptMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, claudeThirdPartyRelayLargePromptMaxTokens)
	}
}

func TestMinimizeClaudeThirdPartyRelayPayload_ClampsLargeToolPayloadMaxTokens(t *testing.T) {
	largePrompt := strings.Repeat("tool ", 9000)
	input := []byte(`{"max_tokens":4096,"tools":[{"name":"classify_result","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"` + largePrompt + `"}]}`)

	out := minimizeClaudeThirdPartyRelayPayload(input)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != claudeThirdPartyRelayLargeToolPayloadMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, claudeThirdPartyRelayLargeToolPayloadMaxTokens)
	}
}

func TestMinimizeClaudeThirdPartyRelayPayload_DoesNotClampSmallToolPayload(t *testing.T) {
	input := []byte(`{"max_tokens":4096,"tools":[{"name":"classify_result","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"short"}]}`)

	out := minimizeClaudeThirdPartyRelayPayload(input)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", got)
	}
}

func TestNormalizeCOVSClaudeRequest_EnforcesMinimumMaxTokens(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	input := []byte(`{"model":"claude-sonnet-4-6","max_tokens":32}`)
	out := normalizeCOVSClaudeRequest(input, auth)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != covsClaudeMinMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, covsClaudeMinMaxTokens)
	}
}

func TestNormalizeCOVSClaudeRequest_PreservesHigherMaxTokens(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	input := []byte(`{"model":"claude-sonnet-4-6","max_tokens":128}`)
	out := normalizeCOVSClaudeRequest(input, auth)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 128 {
		t.Fatalf("max_tokens = %d, want 128", got)
	}
}

func TestNormalizeCOVSClaudeRequest_StripsClaudeCodeBootstrapPrompt(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	input := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":32,
		"messages":[
			{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; cc_entrypoint=cli; cch=00000; You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"hi"}
		]
	}`)
	out := normalizeCOVSClaudeRequest(input, auth)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != covsClaudeMinMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, covsClaudeMinMaxTokens)
	}
	if got := len(gjson.GetBytes(out, "messages").Array()); got != 1 {
		t.Fatalf("messages length = %d, want 1", got)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages.0.role = %q, want user", got)
	}
}

func TestClaudeExecute_COVSExternalCLICompatPreservesClaudeCodePrompt(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_123",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"ok"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":1}
			}`)),
			Request: req,
		}, nil
	}))
	ginCtx := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))}
	ginCtx.Request.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	ginCtx.Request.Header.Set("X-App", "cli")
	ctx = context.WithValue(ctx, "gin", ginCtx)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"帮我继续处理当前任务"}],
		"max_tokens":64
	}`)

	_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !bytes.Contains(capturedBody, []byte("You are Claude Code, Anthropic's official CLI for Claude.")) {
		t.Fatalf("captured upstream body did not preserve Claude Code prompt: %s", capturedBody)
	}
}

func TestClaudeExecute_ThirdPartyNativeClaudeIngressNormalizesForRelay(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(req.Body)
		if gjson.GetBytes(capturedBody, "system").Exists() || gjson.GetBytes(capturedBody, "metadata").Exists() || bytes.Contains(capturedBody, []byte(`"cache_control"`)) {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"400","message":"Param Incorrect"}}`)),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_123",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"ok"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":1}
			}`)),
			Request: req,
		}, nil
	}))
	ginCtx := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))}
	ginCtx.Request.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	ginCtx.Request.Header.Set("X-App", "cli")
	ctx = context.WithValue(ctx, "gin", ginCtx)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-third-party-test",
			"base_url": "https://relay.example.com",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"metadata":{"user_id":"real-user-id"},
		"betas":["tools-2025-04-04"],
		"messages":[
			{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; cc_entrypoint=cli; cch=00000; You are Claude Code, Anthropic's official CLI for Claude.\n# System\n- enormous bootstrap should not reach the relay."},
			{"role":"system","content":[{"type":"text","text":"<system-reminder>auto mode instructions</system-reminder>"}]},
			{"role":"user","content":[{"type":"text","text":"Read the repo and continue the task."}]}
		],
		"max_tokens":128
	}`)

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "ok" {
		t.Fatalf("response text = %q, want ok", got)
	}
	if gjson.GetBytes(capturedBody, "system").Exists() {
		t.Fatalf("captured upstream body unexpectedly preserved system blocks: %s", capturedBody)
	}
	if gjson.GetBytes(capturedBody, "metadata").Exists() {
		t.Fatalf("captured upstream body unexpectedly preserved metadata: %s", capturedBody)
	}
	if bytes.Contains(capturedBody, []byte(`"cache_control"`)) {
		t.Fatalf("captured upstream body unexpectedly preserved cache_control: %s", capturedBody)
	}
	if !bytes.Contains(capturedBody, []byte("Follow these instructions in addition to the conversation:")) {
		t.Fatalf("captured upstream body did not inline system prompt for relay: %s", capturedBody)
	}
	if !bytes.Contains(capturedBody, []byte("You are Claude Code, Anthropic's official CLI for Claude.")) {
		t.Fatalf("captured upstream body lost Claude Code prompt: %s", capturedBody)
	}
	if bytes.Contains(capturedBody, []byte("x-anthropic-billing-header:")) {
		t.Fatalf("captured upstream body unexpectedly preserved Claude bootstrap header: %s", capturedBody)
	}
	if bytes.Contains(capturedBody, []byte("auto mode instructions")) {
		t.Fatalf("captured upstream body unexpectedly preserved relay-incompatible system messages: %s", capturedBody)
	}
	if got := len(gjson.GetBytes(capturedBody, "messages").Array()); got != 1 {
		t.Fatalf("captured upstream messages length = %d, want 1", got)
	}
}

func TestClaudeExecute_COVSNonCLICompatDoesNotInjectClaudeCodePrompt(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_123",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"ok"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":1}
			}`)),
			Request: req,
		}, nil
	}))
	ginCtx := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))}
	ginCtx.Request.Header.Set("User-Agent", "curl/8.5.0")
	ctx = context.WithValue(ctx, "gin", ginCtx)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"帮我继续处理当前任务"}],
		"max_tokens":64
	}`)

	_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if bytes.Contains(capturedBody, []byte("You are Claude Code, Anthropic's official CLI for Claude.")) {
		t.Fatalf("captured upstream body unexpectedly injected Claude Code prompt for non-CLI client: %s", capturedBody)
	}
}

func TestClaudeExecute_ThirdPartyRelayCLICompatTrimsUserBootstrapAndToolSchema(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_trimmed",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"ok"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":1}
			}`)),
			Request: req,
		}, nil
	}))

	ginCtx := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-6"}`))}
	ginCtx.Request.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	ginCtx.Request.Header.Set("X-App", "cli")
	ctx = context.WithValue(ctx, "gin", ginCtx)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-third-party-test",
			"base_url": "https://relay.example.com",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{
				"role":"user",
				"content":[
					{
						"type":"text",
						"text":"x-anthropic-billing-header: cc_version=2.1.72.364; cc_entrypoint=cli; cch=00000; You are Claude Code, Anthropic's official CLI for Claude.\n\n<system-reminder>browser tool metadata block</system-reminder>\n<system-reminder>task planner metadata block</system-reminder>\nRead the repo and continue the task."
					}
				]
			}
		],
		"tools":[
			{
				"name":"exec_command",
				"description":"Run a command with structured arguments for repository inspection and edits. This verbose description should be compacted before reaching the relay because it bloats the payload without changing execution semantics.",
				"input_schema":{
					"type":"object",
					"title":"ExecCommandInput",
					"description":"Large schema description that should not be forwarded to the relay.",
					"properties":{
						"cmd":{"type":"string","description":"Command to run"},
						"timeout":{"type":"number","default":120,"description":"Timeout in seconds"}
					},
					"required":["cmd"]
				}
			}
		]
	}`)

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "ok" {
		t.Fatalf("response text = %q, want ok", got)
	}
	if bytes.Contains(capturedBody, []byte("x-anthropic-billing-header:")) {
		t.Fatalf("captured upstream body unexpectedly preserved Claude bootstrap header: %s", capturedBody)
	}
	if bytes.Contains(capturedBody, []byte("<system-reminder>")) {
		t.Fatalf("captured upstream body unexpectedly preserved system reminder blocks: %s", capturedBody)
	}
	if !bytes.Contains(capturedBody, []byte("Read the repo and continue the task.")) {
		t.Fatalf("captured upstream body lost the actual task prompt: %s", capturedBody)
	}
	if gjson.GetBytes(capturedBody, "tools.0.input_schema.description").Exists() {
		t.Fatalf("captured upstream body unexpectedly preserved schema description: %s", capturedBody)
	}
	if gjson.GetBytes(capturedBody, "tools.0.input_schema.title").Exists() {
		t.Fatalf("captured upstream body unexpectedly preserved schema title: %s", capturedBody)
	}
	if gjson.GetBytes(capturedBody, "tools.0.input_schema.properties.timeout.default").Exists() {
		t.Fatalf("captured upstream body unexpectedly preserved schema default: %s", capturedBody)
	}
	if got := gjson.GetBytes(capturedBody, "tools.0.input_schema.properties.cmd.type").String(); got != "string" {
		t.Fatalf("captured schema cmd.type = %q, want string", got)
	}
}

func TestClaudeExecute_OpenAISourcePreservesNativeClaudeBridgeToolPayload(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_123",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"ok"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":1}
			}`)),
			Request: req,
		}, nil
	}))

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://api.anthropic.com",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
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
		"tool_choice":{"type":"tool","name":"read_todos"},
		"max_tokens":64
	}`)

	_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(capturedBody, "messages.1.content.0.type").String(); got != "tool_use" {
		t.Fatalf("captured messages.1.content.0.type = %q, want tool_use; body=%s", got, capturedBody)
	}
	if got := gjson.GetBytes(capturedBody, "messages.2.content.0.type").String(); got != "tool_result" {
		t.Fatalf("captured messages.2.content.0.type = %q, want tool_result; body=%s", got, capturedBody)
	}
	if got := gjson.GetBytes(capturedBody, "tools.0.name").String(); got != "read_todos" {
		t.Fatalf("captured tools.0.name = %q, want read_todos; body=%s", got, capturedBody)
	}
	if got := gjson.GetBytes(capturedBody, "tool_choice.name").String(); got != "read_todos" {
		t.Fatalf("captured tool_choice.name = %q, want read_todos; body=%s", got, capturedBody)
	}
}

func TestClaudeStreamNormalizer_SuppressesCOVSThinkingBlocks(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	normalizer := newClaudeStreamNormalizer("covs/claude-sonnet-4-6", auth)

	if out := normalizer.NormalizeEvent([][]byte{
		[]byte(`event: content_block_start`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
	}); len(out) != 0 {
		t.Fatal("expected thinking content_block_start to be suppressed")
	}
	if out := normalizer.NormalizeEvent([][]byte{
		[]byte(`event: content_block_delta`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hidden"}}`),
	}); len(out) != 0 {
		t.Fatal("expected thinking content_block_delta to be suppressed")
	}
	if out := normalizer.NormalizeEvent([][]byte{
		[]byte(`event: content_block_stop`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
	}); len(out) != 0 {
		t.Fatal("expected thinking content_block_stop to be suppressed")
	}

	out := normalizer.NormalizeEvent([][]byte{
		[]byte(`event: content_block_start`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
	})
	if len(out) != 2 {
		t.Fatalf("expected text content_block_start to be kept, got %d lines", len(out))
	}
	payload := bytes.TrimSpace(out[1][len("data: "):])
	if got := gjson.GetBytes(payload, "index").Int(); got != 0 {
		t.Fatalf("remapped index = %d, want 0", got)
	}
}

func TestNormalizeClaudeEvents_SuppressesCOVSThinkingForTranslatedStreams(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	normalizer := newClaudeStreamNormalizer("covs/claude-sonnet-4-6", auth)
	events := [][][]byte{
		{
			[]byte(`event: content_block_start`),
			[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		},
		{
			[]byte(`event: content_block_delta`),
			[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hidden"}}`),
		},
		{
			[]byte(`event: content_block_stop`),
			[]byte(`data: {"type":"content_block_stop","index":0}`),
		},
		{
			[]byte(`event: content_block_start`),
			[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		},
	}

	out := normalizeClaudeEvents(normalizer, events)
	if len(out) != 1 {
		t.Fatalf("normalized event count = %d, want 1", len(out))
	}
	payload := bytes.TrimSpace(out[0][1][len("data: "):])
	if got := gjson.GetBytes(payload, "index").Int(); got != 0 {
		t.Fatalf("remapped index = %d, want 0", got)
	}
}

func TestBuildCOVSClaudeRetryBody_UpgradesMaxTokensAndClearsStream(t *testing.T) {
	input := []byte(`{"model":"claude-sonnet-4-6","max_tokens":512,"stream":true}`)
	out := buildCOVSClaudeRetryBody(input)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != covsClaudeRetryMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, covsClaudeRetryMaxTokens)
	}
	if gjson.GetBytes(out, "stream").Exists() {
		t.Fatalf("stream should be removed: %s", out)
	}
}

func TestBuildCOVSClaudeTruncatedRetryBody_AddsTerseSystemPrompt(t *testing.T) {
	input := []byte(`{"model":"claude-sonnet-4-6","max_tokens":512,"system":"existing instruction"}`)
	out := buildCOVSClaudeTruncatedRetryBody(input)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != covsClaudeRetryMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, covsClaudeRetryMaxTokens)
	}
	if got := gjson.GetBytes(out, "system.0.text").String(); got != covsClaudeTerseRetrySystemPrompt {
		t.Fatalf("system.0.text = %q, want %q", got, covsClaudeTerseRetrySystemPrompt)
	}
	if got := gjson.GetBytes(out, "system.1.text").String(); got != "existing instruction" {
		t.Fatalf("system.1.text = %q, want %q", got, "existing instruction")
	}
}

func TestCOVSClaudeShouldRetryEmptyResponse(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	if !covsClaudeShouldRetryEmptyResponse([]byte(`{"content":[{"type":"text","text":""}],"usage":{"output_tokens":512}}`), auth) {
		t.Fatal("expected empty visible content with output tokens to retry")
	}
	if covsClaudeShouldRetryEmptyResponse([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"output_tokens":512}}`), auth) {
		t.Fatal("did not expect visible text response to retry")
	}
	if covsClaudeShouldRetryEmptyResponse([]byte(`{"content":[{"type":"tool_use","id":"t1","name":"tool","input":{}}],"usage":{"output_tokens":512}}`), auth) {
		t.Fatal("did not expect tool_use response to retry")
	}
}

func TestCOVSClaudeShouldRetryEmptyResponse_MetadataOnly(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	raw := []byte(`{
		"id":"msg_empty",
		"type":"message",
		"role":"assistant",
		"model":"claude-opus-4-6",
		"content":[],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":7172,"output_tokens":0}
	}`)
	if !covsClaudeShouldRetryEmptyResponse(raw, auth) {
		t.Fatalf("expected metadata-only empty response to retry: %s", raw)
	}
}

func TestCOVSClaudeShouldRetryPseudoToolStubResponse(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	raw := []byte(`{
		"id":"msg_tool_stub",
		"type":"message",
		"role":"assistant",
		"model":"claude-opus-4-6",
		"content":[{"type":"text","text":"<claude:tool_call>\n\n</claude:tool_call>"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":12,"output_tokens":76}
	}`)
	if !covsClaudeShouldRetryPseudoToolStubResponse(raw, auth) {
		t.Fatalf("expected pseudo tool stub response to retry: %s", raw)
	}
	if !claudeShouldRejectPseudoToolStubResponse(raw) {
		t.Fatalf("expected pseudo tool stub response to be rejected: %s", raw)
	}
}

func TestCOVSClaudeShouldRetryPseudoToolStubResponse_WithVisiblePreamble(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	raw := []byte(`{
		"id":"msg_tool_stub_preamble",
		"type":"message",
		"role":"assistant",
		"model":"claude-opus-4-6",
		"content":[{"type":"text","text":"让我尝试搜索：\n\n<claude:tool_call>"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":12,"output_tokens":57}
	}`)
	if !covsClaudeShouldRetryPseudoToolStubResponse(raw, auth) {
		t.Fatalf("expected preamble pseudo tool stub response to retry: %s", raw)
	}
	if !claudeShouldRejectPseudoToolStubResponse(raw) {
		t.Fatalf("expected preamble pseudo tool stub response to be rejected: %s", raw)
	}
}

func TestCOVSClaudeShouldRetryPseudoToolStubStreamResponse_WithThinkingPreamble(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	raw := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-4-6","stop_reason":null,"usage":{"input_tokens":1243,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"thinking","thinking":""},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"问我为什么搜索被限制了。让我尝试使用WebSearch工具来搜索GitHub上的后台模板。"},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""},"index":1}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"\n\n让我尝试搜索：\n\n\n<claude:tool_call>"},"index":1}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":57}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))
	if !covsClaudeShouldRetryPseudoToolStubResponse(raw, auth) {
		t.Fatalf("expected stream pseudo tool stub response with thinking preamble to retry: %s", raw)
	}
	if !claudeShouldRejectPseudoToolStubResponse(raw) {
		t.Fatalf("expected stream pseudo tool stub response with thinking preamble to be rejected: %s", raw)
	}
}

func TestCompactCOVSClaudeEmptyRetryBody_StripsRelayBaggage(t *testing.T) {
	input := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"Follow these instructions in addition to the conversation:\n\nYou are Claude Code, Anthropic's official CLI for Claude."},
				{"type":"text","text":"<system-reminder>drop me</system-reminder>"},
				{"type":"text","text":"1"}
			]}
		],
		"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
		"tools":[{"name":"Bash","description":"very long tool description","input_schema":{"type":"object","description":"drop this","properties":{"command":{"type":"string","description":"drop this too"}}}}],
		"tool_choice":{"type":"auto"},
		"thinking":{"type":"adaptive"},
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"output_config":{"effort":"medium"},
		"metadata":{"user_id":"abc"},
		"stream":false
	}`)

	out := compactCOVSClaudeEmptyRetryBody(input)

	for _, path := range []string{"system", "tools", "tool_choice", "thinking", "context_management", "output_config", "metadata"} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed: %s", path, out)
		}
	}
	if got := len(gjson.GetBytes(out, "messages.0.content").Array()); got != 1 {
		t.Fatalf("content block count = %d, want 1: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); got != "1" {
		t.Fatalf("messages.0.content.0.text = %q, want %q", got, "1")
	}
}

func TestCOVSClaudeShouldRetryTruncatedResponse(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}

	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":512}`)
	if !covsClaudeShouldRetryTruncatedResponse([]byte(`{"content":[{"type":"text","text":"partial"}],"stop_reason":"max_tokens","usage":{"output_tokens":512}}`), body, auth) {
		t.Fatal("expected max_tokens truncation to retry")
	}
	if covsClaudeShouldRetryTruncatedResponse([]byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"output_tokens":32}}`), body, auth) {
		t.Fatal("did not expect end_turn response to retry")
	}
	if !covsClaudeShouldRetryTruncatedResponse([]byte(`{"content":[{"type":"text","text":"almost full"}],"stop_reason":"end_turn","usage":{"output_tokens":511}}`), body, auth) {
		t.Fatal("expected near-cap output_tokens response to retry even without max_tokens stop_reason")
	}
	if covsClaudeShouldRetryTruncatedResponse([]byte(`{"content":[{"type":"text","text":"partial"}],"stop_reason":"max_tokens","usage":{"output_tokens":4096}}`), []byte(`{"max_tokens":4096}`), auth) {
		t.Fatal("did not expect already-escalated max_tokens response to retry")
	}
}

func TestCOVSClaudeResponseRequestsToolContinuation_SSE(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"call_todos","name":"read_todos","input":{}},"index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	if !covsClaudeResponseRequestsToolContinuation(raw) {
		t.Fatal("expected SSE tool_use continuation to be detected")
	}
}

func TestCOVSClaudeResponseMissedToolResults_SSE(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"thinking","thinking":""},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"I haven't executed any tools yet - there are no tool results to reference."},"index":0}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	if !covsClaudeResponseMissedToolResults(raw) {
		t.Fatal("expected SSE missing-tool-results response to be detected")
	}
}

func TestSynthesizeClaudeStreamLinesFromResponse_Text(t *testing.T) {
	raw := []byte(`{
		"id":"msg_retry",
		"model":"claude-sonnet-4-6",
		"content":[{"type":"text","text":"console.log(\"ok\")"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"output_tokens":20}
	}`)

	lines := synthesizeClaudeStreamLinesFromResponse(raw, "covs/claude-sonnet-4-6")
	joined := string(bytes.Join(lines, []byte("\n")))

	if !strings.Contains(joined, `"type":"message_start"`) {
		t.Fatalf("missing message_start: %s", joined)
	}
	if !strings.Contains(joined, `"type":"text_delta","text":"console.log(\"ok\")"`) {
		t.Fatalf("missing text_delta payload: %s", joined)
	}
	if !strings.Contains(joined, `"type":"message_stop"`) {
		t.Fatalf("missing message_stop: %s", joined)
	}
}

func TestCOVSClaudeEmptyVisibleContentError(t *testing.T) {
	err := covsClaudeEmptyVisibleContentError()
	if err == nil {
		t.Fatal("expected error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status error, got %T", err)
	}
	if got := status.StatusCode(); got != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", got, http.StatusBadGateway)
	}
	if !strings.Contains(err.Error(), "empty visible content") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestReadBufferedClaudeStream_SuppressesTailErrorAfterMessageStop(t *testing.T) {
	reader := &tailErrorReadCloser{
		data: []byte(strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start"}`,
			"",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"},"index":0}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n")),
		err: errors.New("connection reset by peer"),
	}

	buffered, err := readBufferedClaudeStream(reader, context.Background(), nil, "", false)
	if err != nil {
		t.Fatalf("readBufferedClaudeStream() error = %v, want suppressed tail error", err)
	}
	if len(buffered.events) != 3 {
		t.Fatalf("buffered events = %d, want 3", len(buffered.events))
	}
	joined := string(bytes.Join(buffered.events[2], []byte("\n")))
	if !strings.Contains(joined, `"type":"message_stop"`) {
		t.Fatalf("last buffered event = %q, want message_stop", joined)
	}
}

func TestReadBufferedClaudeStream_ErrorsWhenMessageStopMissing(t *testing.T) {
	reader := &tailErrorReadCloser{
		data: []byte(strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start"}`,
			"",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"},"index":0}`,
			"",
		}, "\n")),
	}

	buffered, err := readBufferedClaudeStream(reader, context.Background(), nil, "", false)
	if err == nil {
		t.Fatal("expected missing message_stop to return error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("expected status error, got %T", err)
	}
	if got := status.StatusCode(); got != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want %d", got, http.StatusRequestTimeout)
	}
	if !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("error = %v, want message_stop detail", err)
	}
	if len(buffered.events) != 2 {
		t.Fatalf("buffered events = %d, want 2", len(buffered.events))
	}
}

func TestClaudeExecuteStream_SuppressesTailErrorAfterMessageStop(t *testing.T) {
	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			Body:    &tailErrorReadCloser{data: streamBody, err: errors.New("unexpected EOF")},
			Request: req,
		}, nil
	}))

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://example.com",
		},
	}
	payload := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"reply with OK"}]}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var joined strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v, want suppressed tail error", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	output := joined.String()
	if !strings.Contains(output, `"type":"message_stop"`) {
		t.Fatalf("stream output = %q, want message_stop", output)
	}
	if strings.Contains(output, "event: error") {
		t.Fatalf("stream output = %q, unexpected terminal error event", output)
	}
}

func TestClaudeExecuteStream_ErrorsWhenMessageStopMissing(t *testing.T) {
	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			Body:    &tailErrorReadCloser{data: streamBody},
			Request: req,
		}, nil
	}))

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://example.com",
		},
	}
	payload := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"reply with OK"}]}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var sawErr bool
	for chunk := range stream {
		if chunk.Err == nil {
			continue
		}
		sawErr = true
		status, ok := chunk.Err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("expected status error, got %T", chunk.Err)
		}
		if got := status.StatusCode(); got != http.StatusRequestTimeout {
			t.Fatalf("status = %d, want %d", got, http.StatusRequestTimeout)
		}
		if !strings.Contains(chunk.Err.Error(), "message_stop") {
			t.Fatalf("error = %v, want message_stop detail", chunk.Err)
		}
	}
	if !sawErr {
		t.Fatal("expected incomplete Claude stream to emit error chunk")
	}
}

func TestClaudeExecuteStream_YunyiRelayAcceptsVisibleContentWithoutMessageStop(t *testing.T) {
	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			Body:    &tailErrorReadCloser{data: streamBody},
			Request: req,
		}, nil
	}))

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "yunyi-claude",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://cdn1.yunyi.cfd/claude",
		},
	}
	payload := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"reply with OK"}]}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var joined strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v, want tolerated yunyi relay tail", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	output := joined.String()
	if !strings.Contains(output, `"text":"OK"`) {
		t.Fatalf("stream output = %q, want visible content", output)
	}
	if strings.Contains(output, `"type":"message_stop"`) {
		t.Fatalf("stream output = %q, did not expect synthesized message_stop", output)
	}
}

func TestClaudeExecuteStream_COVSTranslatedStreamEmitsBeforeMessageStop(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	firstBatchWritten := make(chan struct{})
	releaseTail := make(chan struct{})

	go func() {
		defer pw.Close()
		firstBatch := strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start","message":{"id":"msg_covs_1","type":"message","role":"assistant","content":[],"model":"claude-opus-4-6","stop_reason":null,"usage":{"input_tokens":12,"output_tokens":0}}}`,
			"",
			"event: content_block_start",
			`data: {"type":"content_block_start","content_block":{"type":"text","text":""},"index":0}`,
			"",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Checking current project state.\n"},"index":0}`,
			"",
		}, "\n")
		_, _ = io.WriteString(pw, firstBatch)
		close(firstBatchWritten)
		<-releaseTail
		tail := strings.Join([]string{
			"event: content_block_stop",
			`data: {"type":"content_block_stop","index":0}`,
			"",
			"event: content_block_start",
			`data: {"type":"content_block_start","content_block":{"id":"call_1","input":{},"name":"Glob","type":"tool_use"},"index":1}`,
			"",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/opt/vpsAggregation\",\"pattern\":\"**/*.py\"}"},"index":1}`,
			"",
			"event: content_block_stop",
			`data: {"type":"content_block_stop","index":1}`,
			"",
			"event: message_delta",
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":32}}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n")
		_, _ = io.WriteString(pw, tail)
	}()

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
			Request:    req,
		}, nil
	}))

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","stream":true,"messages":[{"role":"user","content":"check the repo"}]}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	<-firstBatchWritten

	select {
	case chunk, ok := <-stream:
		if !ok {
			t.Fatal("stream closed before first translated chunk")
		}
		if chunk.Err != nil {
			t.Fatalf("first chunk err = %v", chunk.Err)
		}
		if !strings.Contains(string(chunk.Payload), `"delta":{"role":"assistant"}`) {
			t.Fatalf("first chunk = %s, want assistant role chunk before message_stop", chunk.Payload)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for first translated chunk before message_stop")
	}

	close(releaseTail)

	var joined strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	output := joined.String()
	if !strings.Contains(output, `"tool_calls"`) {
		t.Fatalf("stream output = %q, want translated tool_calls", output)
	}
	if !strings.Contains(output, `"finish_reason":"tool_calls"`) {
		t.Fatalf("stream output = %q, want finish_reason tool_calls", output)
	}
}

type tailErrorReadCloser struct {
	data []byte
	err  error
	done bool
}

func (r *tailErrorReadCloser) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

func (r *tailErrorReadCloser) Close() error { return nil }

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestClaudeExecute_RejectsShortContinuationResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_123",
			"type":"message",
			"role":"assistant",
			"model":"claude-sonnet-4-6",
			"content":[{"type":"text","text":"好的，开始全速写代码。先搞定项目骨架的所有核心文件。"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":20}
		}`)
	}))
	defer server.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"继续"}],"max_tokens":128}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	}

	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("Execute() error = nil, want short continuation rejection")
	}
	var se statusErr
	if !errors.As(err, &se) {
		t.Fatalf("Execute() error = %T, want statusErr", err)
	}
	if se.StatusCode() != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", se.StatusCode(), http.StatusBadGateway)
	}
	if !strings.Contains(se.Error(), "short continuation response") {
		t.Fatalf("error = %q, want short continuation marker", se.Error())
	}
}

func TestClaudeExecute_RejectsInterruptedAgentScaffoldResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_123",
			"type":"message",
			"role":"assistant",
			"model":"claude-sonnet-4-6",
			"content":[{"type":"text","text":"Searched for 1 pattern, read 2 files (ctrl+o to expand)\n\nInterrupted · What should Claude do"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":14}
		}`)
	}))
	defer server.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"继续"}],"max_tokens":128}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	}

	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err == nil {
		t.Fatal("Execute() error = nil, want interrupted scaffold rejection")
	}
	var se statusErr
	if !errors.As(err, &se) {
		t.Fatalf("Execute() error = %T, want statusErr", err)
	}
	if se.StatusCode() != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", se.StatusCode(), http.StatusBadGateway)
	}
	if !strings.Contains(se.Error(), "interrupted agent scaffold response") {
		t.Fatalf("error = %q, want interrupted scaffold marker", se.Error())
	}
}

func TestClaudeExecute_COVSPseudoToolStubFallbackRecovers(t *testing.T) {
	t.Parallel()

	var requests [][]byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		requests = append(requests, body)
		if len(requests) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{
					"id":"msg_bad",
					"type":"message",
					"role":"assistant",
					"model":"claude-opus-4-6",
					"content":[{"type":"thinking","thinking":"The user wants me to search GitHub."},{"type":"text","text":"<claude:tool_call>\n\n</claude:tool_call>"}],
					"stop_reason":"end_turn",
					"usage":{"input_tokens":12,"output_tokens":76}
				}`)),
				Request: req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_good",
				"type":"message",
				"role":"assistant",
				"model":"claude-opus-4-6",
				"content":[{"type":"text","text":"我已经根据现有页面风格整理好了节点入口页的设计方向。"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":14,"output_tokens":32}
			}`)),
			Request: req,
		}, nil
	}))
	ctx, collector := cliproxyauth.WithResultSignalCollector(ctx)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{
		"model":"claude-opus-4-6",
		"max_tokens":256,
		"tools":[{"name":"Bash","description":"Shell access","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
		"thinking":{"type":"enabled"},
		"messages":[{"role":"user","content":[{"type":"text","text":"去 GitHub 找一些节点管理页面设计参考。"}]}]
	}`)

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if strings.Contains(string(requests[1]), `"tools"`) {
		t.Fatalf("second request body = %s, did not expect tools in pseudo tool stub fallback request", requests[1])
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); !strings.Contains(got, "节点入口页") {
		t.Fatalf("response payload = %s, want final recovered text", resp.Payload)
	}
	if signal, ok := collector.RuntimeOutageSignal(); !ok {
		t.Fatal("expected runtime outage signal after pseudo tool fallback recovery")
	} else if signal.Reason != "covs_claude_pseudo_tool_stub" {
		t.Fatalf("runtime outage reason = %q, want %q", signal.Reason, "covs_claude_pseudo_tool_stub")
	}
}

func TestClaudeExecuteStream_COVSPseudoToolStubWithPreambleFallsBack(t *testing.T) {
	t.Parallel()

	var requests [][]byte
	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-4-6","stop_reason":null,"usage":{"input_tokens":1243,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"thinking","thinking":""},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"问我为什么搜索被限制了。让我尝试使用WebSearch工具来搜索GitHub上的后台模板。"},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""},"index":1}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"\n\n让我尝试搜索：\n\n\n<claude:tool_call>"},"index":1}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":57}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		requests = append(requests, body)
		if len(requests) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader(streamBody)),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_2",
				"type":"message",
				"role":"assistant",
				"model":"claude-opus-4-6",
				"content":[{"type":"text","text":"我先基于当前页面结构和现有上下文给你整理节点入口页设计方向。"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":12,"output_tokens":28}
			}`)),
			Request: req,
		}, nil
	}))
	ctx, collector := cliproxyauth.WithResultSignalCollector(ctx)

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{
		"model":"claude-opus-4-6",
		"stream":true,
		"max_tokens":256,
		"tools":[{"name":"WebSearch","description":"Search the web","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}],
		"messages":[{"role":"user","content":[{"type":"text","text":"去 GitHub 搜一些节点管理页面设计参考。"}]}]
	}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var joined strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if strings.Contains(string(requests[1]), `"tools"`) {
		t.Fatalf("second request body = %s, did not expect tools in pseudo tool stub fallback request", requests[1])
	}
	output := joined.String()
	if strings.Contains(output, "<claude:tool_call>") {
		t.Fatalf("stream output = %q, did not expect pseudo tool stub to reach client", output)
	}
	if !strings.Contains(output, "节点入口页设计方向") {
		t.Fatalf("stream output = %q, want synthesized fallback text", output)
	}
	if signal, ok := collector.RuntimeOutageSignal(); !ok {
		t.Fatal("expected runtime outage signal after stream pseudo tool fallback recovery")
	} else if signal.Reason != "covs_claude_pseudo_tool_stub" {
		t.Fatalf("runtime outage reason = %q, want %q", signal.Reason, "covs_claude_pseudo_tool_stub")
	}
}

func TestClaudeExecute_COVSToolContinuationPreservesHealthyToolUseResponse(t *testing.T) {
	t.Parallel()

	var requests [][]byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		requests = append(requests, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_1",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[
					{"type":"tool_use","id":"call_grep","name":"grep_todos","input":{"pattern":"TODO","path":"./src"}},
					{"type":"tool_use","id":"call_todowrite","name":"write_report_plan","input":{"items":["scan","report"]}}
				],
				"stop_reason":"tool_use",
				"usage":{"input_tokens":12,"output_tokens":32}
			}`)),
			Request: req,
		}, nil
	}))

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":128,
		"messages":[
			{"role":"user","content":[{"type":"text","text":"Based on the tool results, answer in plain Chinese what the current state is."}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_todos","name":"read_todos","input":{}},
				{"type":"tool_use","id":"call_memory","name":"read_memory","input":{}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_todos","content":"{\"items\":[\"修复 cpamc claude 桥接\",\"验证外部兼容入口\"],\"status\":\"in_progress\"}"},
				{"type":"tool_result","tool_use_id":"call_memory","content":"{\"last_note\":\"之前出现卡在第一步，需要确认是否是桥接层问题\"}"}
			]}
		],
		"tools":[
			{"name":"read_todos","description":"Read current todo items","input_schema":{"type":"object","properties":{},"additionalProperties":false}},
			{"name":"read_memory","description":"Read saved memory","input_schema":{"type":"object","properties":{},"additionalProperties":false}}
		],
		"stream":false
	}`)

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if !strings.Contains(string(requests[0]), `"type":"tool_result"`) {
		t.Fatalf("request body = %s, want tool_result continuation", requests[0])
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.type").String(); got != "tool_use" {
		t.Fatalf("response payload = %s, want first content block to stay tool_use", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "stop_reason").String(); got != "tool_use" {
		t.Fatalf("response payload stop_reason = %q, want tool_use; payload=%s", got, resp.Payload)
	}
}

func TestClaudeExecute_COVSOpenAICompatContinuationFallbackFlattensSSEToolResults(t *testing.T) {
	t.Parallel()

	var requests [][]byte
	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","stop_reason":null,"usage":{"input_tokens":1243,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"thinking","thinking":""},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"I haven't executed any tools yet - there are no tool results to reference."},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""},"index":1}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"您好！目前我没有已执行的工具结果可供查询。"},"index":1}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":298}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		requests = append(requests, body)
		if len(requests) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader(streamBody)),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_2",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"当前状态是：修复 cpamc claude 桥接和验证外部兼容入口都在进行中，之前的问题是卡在第一步。"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":12,"output_tokens":32}
			}`)),
			Request: req,
		}, nil
	}))

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"stream":false,
		"max_tokens":128,
		"tools":[
			{"type":"function","function":{"name":"read_todos","description":"Read current todo items","parameters":{"type":"object","properties":{},"additionalProperties":false}}},
			{"type":"function","function":{"name":"read_memory","description":"Read saved memory","parameters":{"type":"object","properties":{},"additionalProperties":false}}}
		],
		"messages":[
			{"role":"user","content":"Based on the tool results, answer in plain Chinese what the current state is."},
			{"role":"assistant","content":"","tool_calls":[
				{"id":"call_todos","type":"function","function":{"name":"read_todos","arguments":"{}"}},
				{"id":"call_memory","type":"function","function":{"name":"read_memory","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_todos","content":"{\"items\":[\"修复 cpamc claude 桥接\",\"验证外部兼容入口\"],\"status\":\"in_progress\"}"},
			{"role":"tool","tool_call_id":"call_memory","content":"{\"last_note\":\"之前出现卡在第一步，需要确认是否是桥接层问题\"}"}
		]
	}`)

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if !strings.Contains(string(requests[0]), `"stream":true`) {
		t.Fatalf("first request body = %s, want upstream SSE translation", requests[0])
	}
	if !strings.Contains(string(requests[0]), `"type":"tool_result"`) {
		t.Fatalf("first request body = %s, want translated tool_result continuation", requests[0])
	}
	if strings.Contains(string(requests[1]), `"type":"tool_result"`) {
		t.Fatalf("second request body = %s, did not expect raw tool_result blocks after SSE fallback flattening", requests[1])
	}
	if strings.Contains(string(requests[1]), `"tools"`) {
		t.Fatalf("second request body = %s, did not expect tools in covs fallback request", requests[1])
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); !strings.Contains(got, "修复 cpamc claude 桥接") {
		t.Fatalf("response payload = %s, want final recovered text", resp.Payload)
	}
}

func TestClaudeExecute_AllowsShortNonContinuationResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_123",
			"type":"message",
			"role":"assistant",
			"model":"claude-sonnet-4-6",
			"content":[{"type":"text","text":"OK"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":1}
		}`)
	}))
	defer server.Close()

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"reply with exactly OK"}],"max_tokens":32}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "OK" {
		t.Fatalf("content.0.text = %q, want %q", got, "OK")
	}
}

func TestClaudeExecute_NativePassthroughPreservesHeadersAndBypassesGuards(t *testing.T) {
	t.Parallel()

	var capturedUserAgent string
	var capturedBeta string
	var capturedVersion string
	var capturedBody []byte

	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"betas":["tools-2025-04-04"],
		"messages":[{"role":"user","content":"继续"}],
		"max_tokens":128
	}`)

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedUserAgent = req.Header.Get("User-Agent")
		capturedBeta = req.Header.Get("Anthropic-Beta")
		capturedVersion = req.Header.Get("Anthropic-Version")
		capturedBody, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_123",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"看起来这是对话的开始，没有可以继续的上下文。"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":0}
			}`)),
			Request: req,
		}, nil
	}))
	ginCtx := &gin.Context{Request: httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))}
	ginCtx.Request.Header.Set("User-Agent", "native-client/1.0")
	ginCtx.Request.Header.Set("Anthropic-Version", "2023-06-01")
	ginCtx.Request.Header.Set("Anthropic-Beta", "request-beta-2025-01-01")
	ctx = context.WithValue(ctx, "gin", ginCtx)

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":            "sk-ant-test",
			"base_url":           "https://api.anthropic.com",
			"native_passthrough": "true",
		},
	}

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil in native passthrough mode", err)
	}

	if capturedUserAgent != "native-client/1.0" {
		t.Fatalf("User-Agent = %q, want native-client/1.0", capturedUserAgent)
	}
	if capturedVersion != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q, want 2023-06-01", capturedVersion)
	}
	if capturedBeta != "request-beta-2025-01-01" {
		t.Fatalf("Anthropic-Beta = %q, want request-beta-2025-01-01", capturedBeta)
	}
	if strings.Contains(capturedBeta, "claude-code-20250219") {
		t.Fatalf("Anthropic-Beta = %q, unexpected Claude Code compatibility beta", capturedBeta)
	}
	if !gjson.GetBytes(capturedBody, "betas").Exists() {
		t.Fatalf("captured body = %s, want betas preserved for passthrough", capturedBody)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got == "" {
		t.Fatalf("response payload = %s, want raw Claude response body", resp.Payload)
	}
}

func TestClaudeExecuteStream_NativePassthroughPreservesThinkingBlocks(t *testing.T) {
	t.Parallel()

	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"thinking","thinking":"secret"},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"secret"},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewReader(streamBody)),
			Request:    req,
		}, nil
	}))

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":            "sk-test",
			"base_url":           "https://rsxermu666.cn",
			"native_passthrough": "true",
		},
	}
	payload := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"reply with OK"}]}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var joined strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	output := joined.String()
	if !strings.Contains(output, `"type":"thinking"`) {
		t.Fatalf("stream output = %q, want raw thinking block preserved", output)
	}
	if !strings.Contains(output, `"type":"message_stop"`) {
		t.Fatalf("stream output = %q, want message_stop", output)
	}
}

func TestClaudeExecuteStream_ThirdPartyRelaySuppressesThinkingBlocks(t *testing.T) {
	t.Parallel()

	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"thinking","thinking":""},"index":0}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hidden"},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"text","text":""},"index":1}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"},"index":1}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewReader(streamBody)),
			Request:    req,
		}, nil
	}))

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://ysjf.fogidc.com",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","stream":true,"messages":[{"role":"user","content":"reply with OK"}]}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var joined strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	output := joined.String()
	if strings.Contains(output, `"type":"thinking"`) {
		t.Fatalf("stream output = %q, unexpected thinking block", output)
	}
	if strings.Contains(output, `"type":"thinking_delta"`) {
		t.Fatalf("stream output = %q, unexpected thinking delta", output)
	}
	if !strings.Contains(output, `"type":"text_delta","text":"ok"`) {
		t.Fatalf("stream output = %q, want text delta preserved", output)
	}
	if !strings.Contains(output, `"type":"content_block_stop","index":0`) {
		t.Fatalf("stream output = %q, want remapped text block stop", output)
	}
	if !strings.Contains(output, `"type":"message_stop"`) {
		t.Fatalf("stream output = %q, want message_stop", output)
	}
}

func TestClaudeExecute_NativePassthroughCOVSSanitizesBootstrapPrompt(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_123",
				"type":"message",
				"role":"assistant",
				"model":"claude-sonnet-4-6",
				"content":[{"type":"text","text":"ok"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":1}
			}`)),
			Request: req,
		}, nil
	}))

	exec := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":            "sk-covs-test",
			"base_url":           "https://rsxermu666.cn",
			"native_passthrough": "true",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"max_tokens":32,
		"messages":[
			{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; cc_entrypoint=cli; cch=00000; You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"hi"}
		]
	}`)

	resp, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(capturedBody, "max_tokens").Int(); got != covsClaudeMinMaxTokens {
		t.Fatalf("captured max_tokens = %d, want %d", got, covsClaudeMinMaxTokens)
	}
	if got := len(gjson.GetBytes(capturedBody, "messages").Array()); got != 1 {
		t.Fatalf("captured messages length = %d, want 1", got)
	}
	if got := gjson.GetBytes(capturedBody, "messages.0.role").String(); got != "user" {
		t.Fatalf("captured messages.0.role = %q, want user", got)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "ok" {
		t.Fatalf("response payload = %s, want assistant text ok", resp.Payload)
	}
}

func TestClaudeExecuteStream_COVSPreservesHealthyToolContinuation(t *testing.T) {
	t.Parallel()

	var requests [][]byte
	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"call_todos","name":"read_todos","input":{}},"index":0}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		requests = append(requests, body)
		if len(requests) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader(streamBody)),
				Request:    req,
			}, nil
		}
		t.Fatalf("unexpected request count = %d", len(requests))
		return nil, nil
	}))

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{
		"model":"claude-sonnet-4-6",
		"stream":true,
		"messages":[
			{"role":"user","content":[{"type":"text","text":"Based on the tool results, answer in plain Chinese what the current state is."}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_todos","name":"read_todos","input":{}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_todos","content":"{\"items\":[\"修复 cpamc claude 桥接\",\"验证外部兼容入口\"],\"status\":\"in_progress\"}"}
			]}
		],
		"tools":[
			{"name":"read_todos","description":"Read current todo items","input_schema":{"type":"object","properties":{},"additionalProperties":false}}
		]
	}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var joined strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	if !strings.Contains(string(requests[0]), `"type":"tool_result"`) {
		t.Fatalf("request body = %s, want tool_result continuation", requests[0])
	}
	output := joined.String()
	if !strings.Contains(output, `"type":"tool_use"`) {
		t.Fatalf("stream output = %q, want tool_use continuation", output)
	}
	if strings.Contains(output, "Completed tool outputs") {
		t.Fatalf("stream output = %q, did not expect fallback transcript", output)
	}
}

func TestClaudeExecuteStream_COVSEmptyLifecycleFallbackCompactsRetryBody(t *testing.T) {
	t.Parallel()

	var requests [][]byte
	streamBody := []byte(strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-4-6","stop_reason":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		requests = append(requests, body)
		if len(requests) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(bytes.NewReader(streamBody)),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"msg_2",
				"type":"message",
				"role":"assistant",
				"model":"claude-opus-4-6",
				"content":[{"type":"text","text":"99"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":12,"output_tokens":1}
			}`)),
			Request: req,
		}, nil
	}))

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-covs-test",
			"base_url": "https://rsxermu666.cn",
		},
	}
	payload := []byte(`{
		"model":"claude-opus-4-6",
		"stream":true,
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"Follow these instructions in addition to the conversation:\n\nYou are Claude Code, Anthropic's official CLI for Claude."},
				{"type":"text","text":"<system-reminder>drop me</system-reminder>"},
				{"type":"text","text":"1"}
			]}
		],
		"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],
		"tools":[{"name":"Bash","description":"Execute bash commands","input_schema":{"type":"object","description":"drop me","properties":{"command":{"type":"string","description":"drop me too"}}}}],
		"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
		"output_config":{"effort":"medium"},
		"thinking":{"type":"adaptive"}
	}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var joined strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
	}

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	second := requests[1]
	for _, path := range []string{"tools", "system", "context_management", "output_config", "thinking"} {
		if gjson.GetBytes(second, path).Exists() {
			t.Fatalf("retry body should remove %s: %s", path, second)
		}
	}
	if got := gjson.GetBytes(second, "messages.0.content.0.text").String(); got != "1" {
		t.Fatalf("retry body first text = %q, want %q: %s", got, "1", second)
	}
	output := joined.String()
	if !strings.Contains(output, `"type":"text_delta"`) || !strings.Contains(output, `"99"`) {
		t.Fatalf("stream output = %q, want synthesized fallback text", output)
	}
}
