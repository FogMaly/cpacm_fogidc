package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/claude/openai/responses"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/openai/openai/responses"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestShouldEnableCodexRequestCompression_FromFeatures(t *testing.T) {
	auth := &cliproxyauth.Auth{}

	if !shouldEnableCodexRequestCompression([]byte(`{"features":{"enable_request_compression":true}}`), auth) {
		t.Fatal("expected compression to be enabled from features.enable_request_compression=true")
	}
	if shouldEnableCodexRequestCompression([]byte(`{"features":{"enable_request_compression":false}}`), auth) {
		t.Fatal("expected compression to be disabled from features.enable_request_compression=false")
	}
}

func TestShouldEnableCodexRequestCompression_FromAuthAttributes(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"enable_request_compression": "true",
		},
	}
	if !shouldEnableCodexRequestCompression([]byte(`{"input":"hello"}`), auth) {
		t.Fatal("expected compression to be enabled from auth attributes")
	}
}

func TestApplyCodexRequestCompression(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request error: %v", err)
	}

	if err := applyCodexRequestCompression(req, body, true); err != nil {
		t.Fatalf("applyCodexRequestCompression error: %v", err)
	}

	if got := req.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want %q", got, "gzip")
	}
	if req.ContentLength <= 0 {
		t.Fatalf("ContentLength = %d, want > 0", req.ContentLength)
	}

	compressed, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read compressed body error: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("new gzip reader error: %v", err)
	}
	defer reader.Close()

	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read decoded body error: %v", err)
	}
	if !bytes.Equal(decoded, body) {
		t.Fatalf("decoded body = %s, want %s", decoded, body)
	}
}

func TestCodexShouldUseChatCompletionsBridge_DisablesAIXJByBaseURL(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{
		CodexKey: []config.CodexKey{{
			APIKey:  "sk-aixj",
			Name:    "aixj",
			BaseURL: "https://aixj.vip/v1",
		}},
	})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-aixj",
			"base_url": "https://aixj.vip/v1",
		},
	}

	if executor.codexShouldUseChatCompletionsBridge(auth, "https://aixj.vip/v1") {
		t.Fatal("expected aixj base_url to avoid chat completions bridge")
	}
}

func TestCodexShouldUseChatCompletionsBridge_DoesNotMatchByNameOnly(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{
		CodexKey: []config.CodexKey{{
			APIKey:  "sk-non-aixj",
			Name:    "aixj",
			Prefix:  "aixj",
			BaseURL: "https://example.com/v1",
		}},
	})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-non-aixj",
			"base_url": "https://example.com/v1",
		},
	}

	if !executor.codexShouldUseChatCompletionsBridge(auth, "https://example.com/v1") {
		t.Fatal("expected API-key provider base_url to use chat completions bridge")
	}
}

func TestCodexShouldUseChatCompletionsBridge_DisablesForChatGPTBackend(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}

	if executor.codexShouldUseChatCompletionsBridge(auth, "https://chatgpt.com/backend-api/codex") {
		t.Fatal("expected chatgpt backend codex endpoint to avoid chat completions bridge")
	}
}

func TestCodexShouldUseChatCompletionsBridgeForRequest_PrefersNativeResponsesWhenExplicitlySupported(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-test",
			"base_url":            "https://aixj.vip/v1",
			"supported_protocols": "responses, chat/completions",
		},
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	if executor.codexShouldUseChatCompletionsBridgeForRequest(auth, "https://aixj.vip/v1", opts) {
		t.Fatal("expected explicit responses support to bypass chat completions bridge")
	}
}

func TestCodexShouldUseChatCompletionsBridgeForRequest_PrefersNativeResponsesForCodexByDefault(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://aixj.vip/v1",
		},
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	if executor.codexShouldUseChatCompletionsBridgeForRequest(auth, "https://aixj.vip/v1", opts) {
		t.Fatal("expected codex openai-response request to prefer native responses by default")
	}
	if !executor.codexShouldRelayOpenAIResponses(auth, "https://aixj.vip/v1", opts) {
		t.Fatal("expected codex openai-response request to relay native responses by default")
	}
}

func TestCodexShouldUseChatCompletionsBridgeForRequest_PrefersNativeResponsesFromMetadataProtocols(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://example.com/v1",
		},
		Metadata: map[string]any{
			"supported_protocols": "responses; chat/completions",
		},
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	if executor.codexShouldUseChatCompletionsBridgeForRequest(auth, "https://example.com/v1", opts) {
		t.Fatal("expected metadata responses support to bypass chat completions bridge")
	}
	if !executor.codexShouldRelayOpenAIResponses(auth, "https://example.com/v1", opts) {
		t.Fatal("expected metadata responses support to enable native relay")
	}
}

func TestCodexShouldUseChatCompletionsBridgeForRequest_KeepsBridgeForChatOnlyCodex(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-test",
			"base_url":            "https://example.com/v1",
			"supported_protocols": "chat/completions",
		},
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	if !executor.codexShouldUseChatCompletionsBridgeForRequest(auth, "https://example.com/v1", opts) {
		t.Fatal("expected chat-only codex auth to keep chat completions bridge")
	}
}

func TestCodexShouldUseChatCompletionsBridgeForRequest_DisablesAIXJChatBridge(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-test",
			"base_url":            "https://aixj.vip/v1",
			"supported_protocols": "chat/completions",
		},
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")}

	if executor.codexShouldUseChatCompletionsBridgeForRequest(auth, "https://aixj.vip/v1", opts) {
		t.Fatal("expected aixj to avoid chat completions bridge")
	}
}

func TestCodexShouldUseStreamForNonStreamingResponses(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-aixj",
				Name:    "aixj",
				BaseURL: "https://aixj.vip/v1",
			},
			{
				APIKey:  "sk-yunyi",
				Name:    "yunyi-codex",
				BaseURL: "https://cdn1.yunyi.cfd/codex/v1",
			},
			{
				APIKey:  "sk-soapapi",
				Name:    "soapapi",
				BaseURL: "https://api.soapapi.top/v1",
			},
		},
	})

	tests := []struct {
		name    string
		auth    *cliproxyauth.Auth
		baseURL string
		want    bool
	}{
		{
			name: "aixj",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"api_key":  "sk-aixj",
					"base_url": "https://aixj.vip/v1",
				},
			},
			baseURL: "https://aixj.vip/v1",
			want:    true,
		},
		{
			name: "yunyi-codex",
			auth: &cliproxyauth.Auth{
				Prefix: "yunyi-codex",
				Attributes: map[string]string{
					"api_key":  "sk-yunyi",
					"base_url": "https://cdn1.yunyi.cfd/codex/v1",
				},
			},
			baseURL: "https://cdn1.yunyi.cfd/codex/v1",
			want:    true,
		},
		{
			name: "soapapi",
			auth: &cliproxyauth.Auth{
				Provider: "soapapi",
				Attributes: map[string]string{
					"api_key":  "sk-soapapi",
					"base_url": "https://api.soapapi.top/v1",
				},
			},
			baseURL: "https://api.soapapi.top/v1",
			want:    true,
		},
		{
			name: "configured sse transport",
			auth: &cliproxyauth.Auth{
				Provider: "example",
				Attributes: map[string]string{
					"api_key":                       "sk-example-sse",
					"base_url":                      "https://example.com/v1",
					"responses_nonstream_transport": "sse",
				},
			},
			baseURL: "https://example.com/v1",
			want:    true,
		},
		{
			name: "configured json transport overrides legacy fallback",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"api_key":                       "sk-aixj",
					"base_url":                      "https://aixj.vip/v1",
					"responses_nonstream_transport": "json",
				},
			},
			baseURL: "https://aixj.vip/v1",
			want:    false,
		},
		{
			name: "generic provider",
			auth: &cliproxyauth.Auth{
				Provider: "example",
				Attributes: map[string]string{
					"api_key":  "sk-example",
					"base_url": "https://example.com/v1",
				},
			},
			baseURL: "https://example.com/v1",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := executor.codexShouldUseStreamForNonStreamingResponses(tt.auth, tt.baseURL); got != tt.want {
				t.Fatalf("codexShouldUseStreamForNonStreamingResponses() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexShouldUseClaudeMessagesBridgeForRequest_PrefersMessagesBridgeWhenExplicitlyConfigured(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "nowcoding",
		Attributes: map[string]string{
			"api_key":             "sk-test",
			"base_url":            "https://nowcoding.ai/v1",
			"supported_protocols": "claude-messages",
		},
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	if !executor.codexShouldUseClaudeMessagesBridge(auth, "https://nowcoding.ai/v1") {
		t.Fatal("expected nowcoding responses requests to use claude messages bridge")
	}
	if executor.codexShouldUseChatCompletionsBridgeForRequest(auth, "https://nowcoding.ai/v1", opts) {
		t.Fatal("expected claude messages bridge to bypass chat completions bridge")
	}
	if executor.codexShouldRelayOpenAIResponses(auth, "https://nowcoding.ai/v1", opts) {
		t.Fatal("expected claude messages bridge to bypass native responses relay")
	}
}

func TestCodexShouldUseClaudeMessagesBridgeForRequest_InfersNowcodingBridgeWithoutExplicitProtocols(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "nowcoding",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://nowcoding.ai/v1",
		},
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	if !executor.codexShouldUseClaudeMessagesBridge(auth, "https://nowcoding.ai/v1") {
		t.Fatal("expected inferred nowcoding bridge to use claude messages bridge")
	}
	if executor.codexShouldUseChatCompletionsBridgeForRequest(auth, "https://nowcoding.ai/v1", opts) {
		t.Fatal("expected inferred nowcoding bridge to bypass chat completions bridge")
	}
	if executor.codexShouldRelayOpenAIResponses(auth, "https://nowcoding.ai/v1", opts) {
		t.Fatal("expected inferred nowcoding bridge to bypass native responses relay")
	}
}

func TestCodexShouldUseProviderRetry_8200CodexByPrefix(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{
		CodexKey: []config.CodexKey{{
			APIKey:  "sk-8200",
			Name:    "test-codex-plan",
			Prefix:  "8200codex",
			BaseURL: "http://49.51.249.22/v1",
		}},
	})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Prefix:   "8200codex",
		Attributes: map[string]string{
			"api_key":  "sk-8200",
			"base_url": "http://49.51.249.22/v1",
			"prefix":   "8200codex",
		},
	}

	if !executor.codexShouldUseProviderRetry(auth, "http://49.51.249.22/v1") {
		t.Fatal("expected 8200codex prefix to enable provider retry")
	}
}

func TestCodexShouldUseProviderRetry_AIXJAndNowcoding(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	for _, tc := range []struct {
		name    string
		auth    *cliproxyauth.Auth
		baseURL string
	}{
		{
			name: "aixj",
			auth: &cliproxyauth.Auth{
				Prefix: "aixj",
				Attributes: map[string]string{
					"prefix":   "aixj",
					"base_url": "https://aixj.vip/v1",
				},
			},
			baseURL: "https://aixj.vip/v1",
		},
		{
			name: "nowcoding",
			auth: &cliproxyauth.Auth{
				Prefix: "nowcoding",
				Attributes: map[string]string{
					"prefix":   "nowcoding",
					"base_url": "https://nowcoding.ai/v1",
				},
			},
			baseURL: "https://nowcoding.ai/v1",
		},
	} {
		if !executor.codexShouldUseProviderRetry(tc.auth, tc.baseURL) {
			t.Fatalf("%s should enable provider retry", tc.name)
		}
	}
}

func TestCodexShouldUseProviderRetry_Gmncode(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Prefix:   "gmncode.cn",
		Label:    "gmncode.cn",
		Attributes: map[string]string{
			"api_key":  "sk-gmn",
			"base_url": "https://gmncode.cn/v1",
			"prefix":   "gmncode.cn",
			"name":     "gmncode.cn",
		},
	}

	if !executor.codexShouldUseProviderRetry(auth, "https://gmncode.cn/v1") {
		t.Fatal("expected gmncode.cn to enable provider retry")
	}
	if got := executor.codexRetryAttempts(auth, "https://gmncode.cn/v1"); got != 3 {
		t.Fatalf("codexRetryAttempts() = %d, want %d", got, 3)
	}
}

func TestCodexShouldRetryStatus_DoesNotRetryQuotaExceeded429(t *testing.T) {
	body := []byte(`{"code":"USAGE_LIMIT_EXCEEDED","message":"error: code=429 reason=\"DAILY_LIMIT_EXCEEDED\" message=\"daily usage limit exceeded\" metadata=map[]"}`)
	if codexShouldRetryStatus(http.StatusTooManyRequests, body) {
		t.Fatal("expected daily quota 429 to skip retry")
	}
	if !codexShouldRetryStatus(http.StatusBadGateway, nil) {
		t.Fatal("expected 502 to remain retryable")
	}
}

func TestCodexCreds_NormalizesRootBaseURL(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://gmncode.cn",
		},
	}

	apiKey, baseURL := codexCreds(auth)
	if apiKey != "sk-test" {
		t.Fatalf("apiKey = %q, want %q", apiKey, "sk-test")
	}
	if baseURL != "https://gmncode.cn/v1" {
		t.Fatalf("baseURL = %q, want %q", baseURL, "https://gmncode.cn/v1")
	}
}

func TestCodexNormalizeNativeResponsesPayloadSynthesizesStreamOutput(t *testing.T) {
	raw := []byte(strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","item_id":"msg_aixj","output_index":0,"content_index":0,"delta":"OK"}`,
		``,
		`event: response.output_text.done`,
		`data: {"type":"response.output_text.done","item_id":"msg_aixj","output_index":0,"content_index":0,"text":"OK"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_aixj","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_aixj","object":"response","created_at":1,"model":"gpt-5.3-codex","status":"completed","output":[],"output_text":null}}`,
		``,
	}, "\n"))

	payload, err := codexNormalizeNativeResponsesPayload(raw)
	if err != nil {
		t.Fatalf("codexNormalizeNativeResponsesPayload() error = %v", err)
	}
	if got := gjson.GetBytes(payload, "output.0.content.0.text").String(); got != "OK" {
		t.Fatalf("output text = %q, want OK; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "output_text").String(); got != "OK" {
		t.Fatalf("output_text = %q, want OK; payload=%s", got, payload)
	}
}

func TestCodexExecuteOpenAIResponsesRelay_AIXJUsesStreamForNonStream(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("Accept = %q, want text/event-stream", got)
		}
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body error: %v", err)
		}
		if !gjson.GetBytes(upstreamBody, "stream").Bool() {
			t.Fatalf("upstream stream = false, want true; body=%s", upstreamBody)
		}
		if got := gjson.GetBytes(upstreamBody, "model").String(); got != "gpt-5.3-codex" {
			t.Fatalf("upstream model = %q, want gpt-5.3-codex", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_aixj\",\"object\":\"response\",\"created_at\":1,\"model\":\"gpt-5.3-codex\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_aixj\",\"output_index\":0,\"content_index\":0,\"delta\":\"OK\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.done\",\"item_id\":\"msg_aixj\",\"output_index\":0,\"content_index\":0,\"text\":\"OK\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_aixj\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_aixj\",\"object\":\"response\",\"created_at\":1,\"model\":\"gpt-5.3-codex\",\"status\":\"completed\",\"output\":[],\"output_text\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "aixj",
		Attributes: map[string]string{
			"api_key":  "sk-aixj",
			"base_url": server.URL + "/v1",
			"prefix":   "aixj",
		},
	}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "aixj/gpt-5.3-codex",
		Payload: []byte(`{"model":"aixj/gpt-5.3-codex","input":"reply OK"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "OK" {
		t.Fatalf("output text = %q, want OK; payload=%s", got, resp.Payload)
	}
}

func TestCodexExecute_ClaudeMessagesBridgeReturnsResponsesPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-nowcoding" {
			t.Fatalf("Authorization = %q, want Bearer token", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body error: %v", err)
		}
		if got := gjson.GetBytes(body, "model").String(); got != "claude-sonnet-4-6" {
			t.Fatalf("upstream model = %q, want claude-sonnet-4-6", got)
		}
		if got := gjson.GetBytes(body, "messages").Raw; !strings.Contains(got, "reply with exactly OK") {
			t.Fatalf("messages = %q, want original prompt", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-nowcoding",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "claude-messages",
		},
	}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: []byte(`{"model":"gpt-5.4","input":"reply with exactly OK"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "output.0.type").String(); got != "message" {
		t.Fatalf("output.0.type = %q, want message", got)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "OK" {
		t.Fatalf("output text = %q, want OK", got)
	}
}

func TestCodexExecute_ClaudeMessagesBridgeDropsEmptyClaudeMessages(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body error: %v", err)
		}
		if got := len(gjson.GetBytes(upstreamBody, "messages").Array()); got != 2 {
			t.Fatalf("messages length = %d, want 2; body=%s", got, upstreamBody)
		}
		if strings.Contains(string(upstreamBody), `"content":""`) {
			t.Fatalf("upstream body still contains empty content: %s", upstreamBody)
		}
		if got := gjson.GetBytes(upstreamBody, "messages.0.content").String(); got != "hello" {
			t.Fatalf("messages.0.content = %q, want hello", got)
		}
		if got := gjson.GetBytes(upstreamBody, "messages.1.content").String(); got != "Reply with exactly OK." {
			t.Fatalf("messages.1.content = %q, want Reply with exactly OK.", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_drop_empty\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "nowcoding",
		Attributes: map[string]string{
			"api_key":             "sk-nowcoding",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "claude-messages",
		},
	}
	payload := []byte(`{
		"model":"nowcoding/gpt-5.4",
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":""},
			{"role":"user","content":"Reply with exactly OK."}
		],
		"max_tokens":16
	}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !bytes.Contains(resp.Payload, []byte(`OK`)) {
		t.Fatalf("response payload = %s, want OK", resp.Payload)
	}
}

func TestCodexExecute_ClaudeMessagesBridgeReplaysToolContinuation(t *testing.T) {
	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body error: %v", err)
		}
		requestBodies = append(requestBodies, string(body))

		w.Header().Set("Content-Type", "text/event-stream")
		switch len(requestBodies) {
		case 1:
			if !strings.Contains(string(body), "use shell_command to run pwd") {
				t.Fatalf("first request body = %s, want original tool prompt", body)
			}
			_, _ = io.WriteString(w, "event: message_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_tool_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_pwd_1\",\"name\":\"shell_command\",\"input\":{}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"pwd\\\"}\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		case 2:
			if !strings.Contains(string(body), `"type":"tool_use"`) {
				t.Fatalf("second request body = %s, want replayed assistant tool_use", body)
			}
			if !strings.Contains(string(body), `"type":"tool_result"`) {
				t.Fatalf("second request body = %s, want replayed user tool_result", body)
			}
			if !strings.Contains(string(body), `"command":"pwd"`) {
				t.Fatalf("second request body = %s, want original tool arguments replayed", body)
			}
			if !strings.Contains(string(body), `"content":"OK"`) {
				t.Fatalf("second request body = %s, want tool result content replayed", body)
			}
			_, _ = io.WriteString(w, "event: message_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_tool_2\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"DONE\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		default:
			t.Fatalf("unexpected request count = %d", len(requestBodies))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-nowcoding",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "claude-messages",
		},
	}

	firstPayload := []byte(`{"model":"gpt-5.4","tool_choice":"auto","tools":[{"name":"shell_command","description":"run a shell command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],"input":"use shell_command to run pwd"}`)
	firstResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: firstPayload,
	})
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}

	firstID := gjson.GetBytes(firstResp.Payload, "id").String()
	if firstID == "" {
		t.Fatalf("first response missing id: %s", firstResp.Payload)
	}
	callID := gjson.GetBytes(firstResp.Payload, "output.0.call_id").String()
	if callID == "" {
		t.Fatalf("first response missing function call id: %s", firstResp.Payload)
	}

	secondPayload := []byte(`{"model":"gpt-5.4","previous_response_id":"` + firstID + `","tool_choice":"auto","tools":[{"name":"shell_command","description":"run a shell command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],"input":[{"type":"function_call_output","call_id":"` + callID + `","output":"OK"}]}`)
	secondResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: secondPayload,
	})
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if got := gjson.GetBytes(secondResp.Payload, "output.0.content.0.text").String(); got != "DONE" {
		t.Fatalf("second output text = %q, want DONE", got)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("requestBodies = %d, want 2", len(requestBodies))
	}
}

func TestCodexExecute_ClaudeMessagesBridgeContinuationDoesNotReuseRequiredToolChoice(t *testing.T) {
	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body error: %v", err)
		}
		requestBodies = append(requestBodies, string(body))

		w.Header().Set("Content-Type", "text/event-stream")
		switch len(requestBodies) {
		case 1:
			if !strings.Contains(string(body), `"tool_choice":{"type":"any"}`) {
				t.Fatalf("first request body = %s, want required tool choice translated to any", body)
			}
			_, _ = io.WriteString(w, "event: message_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_tool_required_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_get_time\",\"name\":\"get_time\",\"input\":{}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		case 2:
			if strings.Contains(string(body), `"tool_choice"`) {
				t.Fatalf("second request body = %s, want inherited tool_choice removed", body)
			}
			if !strings.Contains(string(body), `"type":"tool_result"`) {
				t.Fatalf("second request body = %s, want replayed user tool_result", body)
			}
			_, _ = io.WriteString(w, "event: message_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_tool_required_2\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"DONE\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		default:
			t.Fatalf("unexpected request count = %d", len(requestBodies))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-nowcoding",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "claude-messages",
		},
	}

	firstPayload := []byte(`{"model":"gpt-5.4","tool_choice":"required","tools":[{"name":"get_time","description":"return the current time","parameters":{"type":"object","properties":{},"additionalProperties":false}}],"input":"What time is it?"}`)
	firstResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: firstPayload,
	})
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}

	firstID := gjson.GetBytes(firstResp.Payload, "id").String()
	if firstID == "" {
		t.Fatalf("first response missing id: %s", firstResp.Payload)
	}
	callID := gjson.GetBytes(firstResp.Payload, "output.0.call_id").String()
	if callID == "" {
		t.Fatalf("first response missing function call id: %s", firstResp.Payload)
	}

	secondPayload := []byte(`{"model":"gpt-5.4","previous_response_id":"` + firstID + `","input":[{"type":"function_call_output","call_id":"` + callID + `","output":"2026-04-02T17:40:00Z"}]}`)
	secondResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: secondPayload,
	})
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if got := gjson.GetBytes(secondResp.Payload, "output.0.content.0.text").String(); got != "DONE" {
		t.Fatalf("second output text = %q, want DONE", got)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("requestBodies = %d, want 2", len(requestBodies))
	}
}

func TestCodexExecute_ClaudeMessagesBridgeRetriesWithoutSpecificToolChoice(t *testing.T) {
	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body error: %v", err)
		}
		requestBodies = append(requestBodies, string(body))

		w.Header().Set("Content-Type", "text/event-stream")
		if len(requestBodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unknown parameter: 'tool_choice.function'."},"type":"error"}`)
			return
		}
		if strings.Contains(string(body), `"tool_choice"`) {
			t.Fatalf("retry request body = %s, want tool_choice removed", body)
		}
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_retry\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_apply\",\"name\":\"apply_patch\",\"input\":{}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"input\\\":\\\"*** Begin Patch\\\\n*** End Patch\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "nowcoding",
		Attributes: map[string]string{
			"api_key":             "sk-nowcoding",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "claude-messages",
		},
	}
	payload := []byte(`{
		"model":"nowcoding/gpt-5.4",
		"stream":true,
		"tool_choice":{"type":"custom","name":"apply_patch"},
		"tools":[{"type":"custom","name":"apply_patch","description":"apply patch"}],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Call apply_patch exactly once. Put this exact input: *** Begin Patch\n*** End Patch"}]}]
	}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("request count = %d, want 2", len(requestBodies))
	}
	if !strings.Contains(requestBodies[0], `"tool_choice":{"name":"apply_patch","type":"tool"}`) {
		t.Fatalf("first request body = %s, want forced tool_choice", requestBodies[0])
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("response output type = %q, want custom_tool_call", got)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.name").String(); got != "apply_patch" {
		t.Fatalf("response output name = %q, want apply_patch", got)
	}
}

func TestCodexExecuteStream_ClaudeMessagesBridgeRetriesWithoutSpecificToolChoice(t *testing.T) {
	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body error: %v", err)
		}
		requestBodies = append(requestBodies, string(body))

		w.Header().Set("Content-Type", "text/event-stream")
		if len(requestBodies) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unknown parameter: 'tool_choice.function'."},"type":"error"}`)
			return
		}
		if strings.Contains(string(body), `"tool_choice"`) {
			t.Fatalf("retry request body = %s, want tool_choice removed", body)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_retry_stream\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: content_block_start\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_apply_stream\",\"name\":\"apply_patch\",\"input\":{}}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: content_block_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"input\\\":\\\"*** Begin Patch\\\\n*** End Patch\\\"}\"}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: content_block_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: message_delta\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: message_stop\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "nowcoding",
		Attributes: map[string]string{
			"api_key":             "sk-nowcoding",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "claude-messages",
		},
	}
	payload := []byte(`{
		"model":"nowcoding/gpt-5.4",
		"stream":true,
		"tool_choice":{"type":"custom","name":"apply_patch"},
		"tools":[{"type":"custom","name":"apply_patch","description":"apply patch"}],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Call apply_patch exactly once. Put this exact input: *** Begin Patch\n*** End Patch"}]}]
	}`)

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("request count = %d, want 2", len(requestBodies))
	}
	first, ok := <-stream
	if !ok {
		t.Fatal("expected first stream chunk")
	}
	if first.Err != nil {
		t.Fatalf("first chunk err = %v", first.Err)
	}
	if !strings.Contains(string(first.Payload), `"type":"response.created"`) {
		t.Fatalf("first chunk = %s, want response.created", first.Payload)
	}
	foundToolEvent := false
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), `"type":"response.output_item.added"`) {
			foundToolEvent = true
			break
		}
	}
	if !foundToolEvent {
		t.Fatal("expected translated tool call event after retry")
	}
}

func TestValidateCodexChatCompletionPayload_AllowsTerminalStopWithoutContent(t *testing.T) {
	payload := []byte(`{
		"id":"chatcmpl_empty",
		"object":"chat.completion",
		"created":1775049346,
		"model":"gpt-5.4",
		"choices":[
			{
				"index":0,
				"message":{"role":"assistant","content":""},
				"finish_reason":"stop"
			}
		],
		"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}
	}`)

	if err := validateCodexChatCompletionPayload(payload); err != nil {
		t.Fatalf("validateCodexChatCompletionPayload() error = %v", err)
	}
}

func TestCodexExecuteStream_ChatCompletionsBridgeAllowsTerminalStopWithoutContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"resp_empty_stream\",\"object\":\"chat.completion.chunk\",\"created\":1775049346,\"model\":\"gpt-5.4\",\"choices\":[{\"delta\":{\"content\":\"\",\"role\":\"assistant\"},\"logprobs\":null,\"finish_reason\":null,\"index\":0}],\"usage\":null}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"resp_empty_stream\",\"object\":\"chat.completion.chunk\",\"created\":1775049346,\"model\":\"gpt-5.4\",\"choices\":[{\"delta\":{},\"logprobs\":null,\"finish_reason\":\"stop\",\"index\":0}],\"usage\":null}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"resp_empty_stream\",\"object\":\"chat.completion.chunk\",\"created\":1775049346,\"model\":\"gpt-5.4\",\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "nowcoding",
		Attributes: map[string]string{
			"api_key":             "sk-nowcoding",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "chat/completions",
		},
	}
	payload := []byte(`{
		"model":"nowcoding/gpt-5.4",
		"stream":true,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"reply with nothing"}]}]
	}`)

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var sawCreated bool
	var sawCompleted bool
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		text := string(chunk.Payload)
		if strings.Contains(text, `"type":"response.created"`) {
			sawCreated = true
		}
		if strings.Contains(text, `"type":"response.completed"`) {
			sawCompleted = true
		}
	}

	if !sawCreated {
		t.Fatal("expected response.created event")
	}
	if !sawCompleted {
		t.Fatal("expected response.completed event")
	}
}

func TestCodexExecute_ChatCompletionsBridgeReplaysPreviousResponseID(t *testing.T) {
	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body error: %v", err)
		}
		requestBodies = append(requestBodies, string(body))

		w.Header().Set("Content-Type", "application/json")
		switch len(requestBodies) {
		case 1:
			if !strings.Contains(string(body), "Remember exactly this codeword: BANANA-731") {
				t.Fatalf("first request body = %s, want original prompt", body)
			}
			_, _ = io.WriteString(w, `{"id":"chatcmpl_bridge_1","object":"chat.completion","created":1,"model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"READY"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		case 2:
			if !strings.Contains(string(body), "Remember exactly this codeword: BANANA-731") {
				t.Fatalf("second request body = %s, want replayed first user turn", body)
			}
			if !strings.Contains(string(body), "\"text\":\"READY\"") {
				t.Fatalf("second request body = %s, want replayed assistant turn", body)
			}
			_, _ = io.WriteString(w, `{"id":"chatcmpl_bridge_2","object":"chat.completion","created":2,"model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"BANANA-731"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
		default:
			t.Fatalf("unexpected request count = %d", len(requestBodies))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-bridge",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "chat/completions",
		},
	}

	firstPayload := []byte(`{"model":"gpt-5.4","input":"Remember exactly this codeword: BANANA-731. Reply only READY."}`)
	firstResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: firstPayload,
	})
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}

	firstID := gjson.GetBytes(firstResp.Payload, "id").String()
	if firstID == "" {
		t.Fatalf("first response missing id: %s", firstResp.Payload)
	}
	if replay, ok := getCodexBridgeReplay(firstID); !ok || !strings.Contains(string(replay), "BANANA-731") {
		t.Fatalf("bridge replay for %q missing or unexpected: ok=%v payload=%s firstResp=%s", firstID, ok, replay, firstResp.Payload)
	}

	secondPayload := []byte(`{"model":"gpt-5.4","previous_response_id":"` + firstID + `","input":"What was the codeword I asked you to remember? Reply with the codeword only."}`)
	secondResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: secondPayload,
	})
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if got := gjson.GetBytes(secondResp.Payload, "output.0.content.0.text").String(); got != "BANANA-731" {
		t.Fatalf("second output text = %q, want BANANA-731", got)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("requestBodies = %d, want 2", len(requestBodies))
	}
}

func TestCodexExecute_ReturnsOnResponseCompletedWithoutWaitingForClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		flusher.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"reply with OK only"}],"max_tokens":8}`)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	resp, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "openai/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "OK" {
		t.Fatalf("response content = %q, want OK; payload=%s", got, resp.Payload)
	}
}

func TestValidateCodexResponsesPayload_RejectsEmptyCompleted(t *testing.T) {
	err := validateCodexResponsesPayload([]byte(`{"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`))
	if err == nil {
		t.Fatal("expected empty responses payload to be rejected")
	}
}

func TestValidateCodexChatCompletionPayload_RejectsEmptyPayload(t *testing.T) {
	err := validateCodexChatCompletionPayload([]byte(`{"id":"chatcmpl_empty","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":null}]}`))
	if err == nil {
		t.Fatal("expected empty chat completion payload to be rejected")
	}
}

func TestCodexExecute_ReturnsErrorOnEmptyCompleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"reply with OK only"}],"max_tokens":8}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openai/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err == nil {
		t.Fatal("expected Execute() to reject empty completed response")
	}
}

func TestCodexExecute_ReturnsErrorOnEmptyChatBridgeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_empty","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":null}]}`)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"reply with OK only"}],"max_tokens":8}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openai/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err == nil {
		t.Fatal("expected Execute() to reject empty chat bridge response")
	}
}

func TestCodexExecute_ReturnsErrorOnShortContinuationResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_short\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"\\u597d\\uff0c\\u5f00\\u59cb\\u5168\\u901f\\u5199\\u4ee3\\u7801\\u3002\\u5148\\u641e\\u5b9a\\u9879\\u76ee\\u9aa8\\u67b6\\u7684\\u6240\\u6709\\u6838\\u5fc3\\u6587\\u4ef6\\u3002\"}]}],\"usage\":{\"input_tokens\":100,\"output_tokens\":13,\"total_tokens\":113}}}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"openai/gpt-5.4","input":"\u7ee7\u7eed"}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openai/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err == nil {
		t.Fatal("expected Execute() to reject short continuation response")
	}
}

func TestCodexExecute_AllowsShortNonContinuationResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"openai/gpt-5.4","input":"reply with exactly OK"}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openai/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(string(resp.Payload), "OK") {
		t.Fatalf("response payload = %s, want OK", resp.Payload)
	}
}

func TestCodexExecute_ReturnsErrorOnShortContinuationChatBridgeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_short","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"I will start by setting up the core files."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":11,"total_tokens":21}}`)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"continue"}],"max_tokens":32}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openai/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err == nil {
		t.Fatal("expected Execute() to reject short continuation chat bridge response")
	}
}

func TestCodexExecuteStream_ReturnsErrorOnEmptyCompletedBeforeForwarding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_empty\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_empty\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_empty\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"openai/gpt-5.4","messages":[{"role":"user","content":"reply with OK only"}],"max_tokens":8}`)

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "openai/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err == nil {
		t.Fatal("expected ExecuteStream() to reject empty completed stream")
	}
	if stream != nil {
		t.Fatal("expected stream to be nil on bootstrap validation failure")
	}
}

func TestCodexExecute_PreservesPreviousResponseIDForResponsesRequests(t *testing.T) {
	var capturedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		capturedBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"previous_response_id\":\"resp_prev\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"model":"gpt-5.4","input":"reply with OK only","previous_response_id":"resp_prev","stream":false}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(capturedBody, `"previous_response_id":"resp_prev"`) {
		t.Fatalf("captured request body = %s, want previous_response_id preserved", capturedBody)
	}
	if !bytes.Contains(resp.Payload, []byte(`"previous_response_id":"resp_prev"`)) {
		t.Fatalf("response payload = %s, want previous_response_id preserved", resp.Payload)
	}
}

func TestCodexExecute_RelaysPromptCacheKeyForNativeResponsesContinuation(t *testing.T) {
	resetCodexCacheStateForTest(t)

	var requestBodies []string
	var conversationHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		requestBodies = append(requestBodies, string(body))
		conversationHeaders = append(conversationHeaders, r.Header.Get("Conversation_id")+"|"+r.Header.Get("Session_id"))

		w.Header().Set("Content-Type", "application/json")
		switch len(requestBodies) {
		case 1:
			if strings.Contains(string(body), `"prompt_cache_key"`) {
				t.Fatalf("first request body = %s, want no prompt_cache_key yet", body)
			}
			_, _ = io.WriteString(w, `{"id":"resp_native_1","object":"response","status":"completed","prompt_cache_key":"conv_upstream_1","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"READY"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		case 2:
			if !strings.Contains(string(body), `"prompt_cache_key":"conv_upstream_1"`) {
				t.Fatalf("second request body = %s, want prompt_cache_key replayed", body)
			}
			if got := r.Header.Get("Conversation_id") + "|" + r.Header.Get("Session_id"); got != "conv_upstream_1|conv_upstream_1" {
				t.Fatalf("continuation headers = %q, want %q", got, "conv_upstream_1|conv_upstream_1")
			}
			_, _ = io.WriteString(w, `{"id":"resp_native_2","object":"response","status":"completed","prompt_cache_key":"conv_upstream_1","previous_response_id":"resp_native_1","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"BANANA-731"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
		default:
			t.Fatalf("unexpected request count = %d", len(requestBodies))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"base_url":            server.URL + "/v1",
			"supported_protocols": "responses",
		},
	}

	firstPayload := []byte(`{"model":"aixj/gpt-5.4","input":"Remember exactly this codeword: BANANA-731. Reply only READY."}`)
	firstResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "aixj/gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: firstPayload,
	})
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	firstID := gjson.GetBytes(firstResp.Payload, "id").String()
	if firstID != "resp_native_1" {
		t.Fatalf("first response id = %q, want %q", firstID, "resp_native_1")
	}

	secondPayload := []byte(`{"model":"aixj/gpt-5.4","previous_response_id":"resp_native_1","input":"What was the codeword I asked you to remember? Reply with the codeword only."}`)
	secondResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "aixj/gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: secondPayload,
	})
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if got := gjson.GetBytes(secondResp.Payload, "output.0.content.0.text").String(); got != "BANANA-731" {
		t.Fatalf("second output text = %q, want %q", got, "BANANA-731")
	}
	if len(requestBodies) != 2 {
		t.Fatalf("requestBodies = %d, want 2", len(requestBodies))
	}
	if got := conversationHeaders[1]; got != "conv_upstream_1|conv_upstream_1" {
		t.Fatalf("conversation headers[1] = %q, want %q", got, "conv_upstream_1|conv_upstream_1")
	}
}

func TestCodexExecute_ChatCompletionsRequestUsesNativeResponsesWhenExplicitlyConfigured(t *testing.T) {
	var requestPath string
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("io.ReadAll() error = %v", err)
		}
		requestBody = string(body)
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_native_chat_bridge","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi! How can I help?"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-responses-only",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "responses",
		},
	}
	payload := []byte(`{"model":"aixj/gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":false}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "aixj/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if requestPath != "/v1/responses" {
		t.Fatalf("requestPath = %q, want %q", requestPath, "/v1/responses")
	}
	if !strings.Contains(requestBody, `"type":"message"`) || !strings.Contains(requestBody, `"type":"input_text"`) {
		t.Fatalf("requestBody = %s, want native responses payload", requestBody)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "Hi! How can I help?" {
		t.Fatalf("choices.0.message.content = %q, want %q", got, "Hi! How can I help?")
	}
}

func TestCodexExecute_RetriesInvalidNativeResponsesRelay(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_retry_ok","object":"response","status":"completed","prompt_cache_key":"conv_retry_ok","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"READY"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "gmncode.cn",
		Label:  "gmncode.cn",
		Attributes: map[string]string{
			"api_key":             "sk-gmn",
			"base_url":            server.URL + "/v1",
			"prefix":              "gmncode.cn",
			"name":                "gmncode.cn",
			"supported_protocols": "responses",
		},
	}
	payload := []byte(`{"model":"gmncode.cn/gpt-5.4","input":"Reply only READY."}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gmncode.cn/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "READY" {
		t.Fatalf("output text = %q, want %q", got, "READY")
	}
	if requestCount != 2 {
		t.Fatalf("requestCount = %d, want %d", requestCount, 2)
	}
}

func TestCodexExecute_FallsBackToStreamWhenNativeResponsesPayloadIsEmpty(t *testing.T) {
	var requestCount int
	var accepts []string
	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		requestCount++
		accepts = append(accepts, r.Header.Get("Accept"))
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("io.ReadAll() error = %v", err)
		}
		requestBodies = append(requestBodies, string(body))

		switch requestCount {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_empty","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.output_text.delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_stream_ok\",\"output_index\":0,\"content_index\":0,\"delta\":\"READY\"}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_ok\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.4\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		default:
			t.Fatalf("unexpected requestCount = %d", requestCount)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-test",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "responses",
		},
	}
	payload := []byte(`{"model":"yunyi-codex/gpt-5.4","input":"Reply only READY."}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "yunyi-codex/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "READY" {
		t.Fatalf("output text = %q, want %q", got, "READY")
	}
	if requestCount != 2 {
		t.Fatalf("requestCount = %d, want %d", requestCount, 2)
	}
	if accepts[0] != "application/json" {
		t.Fatalf("first Accept = %q, want %q", accepts[0], "application/json")
	}
	if accepts[1] != "text/event-stream" {
		t.Fatalf("second Accept = %q, want %q", accepts[1], "text/event-stream")
	}
	if strings.Contains(requestBodies[0], `"stream":true`) {
		t.Fatalf("first request body = %s, want non-stream request", requestBodies[0])
	}
	if !strings.Contains(requestBodies[1], `"stream":true`) {
		t.Fatalf("second request body = %s, want stream retry", requestBodies[1])
	}
}

func TestCodexExecute_AdaptsThirdPartyNativeResponsesContinuationByReplayingContext(t *testing.T) {
	resetCodexCacheStateForTest(t)
	resetCodexBridgeReplayStateForTest(t)

	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		requestBodies = append(requestBodies, string(body))

		w.Header().Set("Content-Type", "application/json")
		switch len(requestBodies) {
		case 1:
			if !strings.Contains(string(body), "BANANA-731") {
				t.Fatalf("first request body = %s, want original prompt", body)
			}
			_, _ = io.WriteString(w, `{"id":"resp_native_replay_1","object":"response","status":"completed","prompt_cache_key":"conv_native_replay_1","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"READY"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		case 2:
			if strings.Contains(string(body), `"previous_response_id":"resp_native_replay_1"`) {
				t.Fatalf("second request body = %s, want previous_response_id removed after adaptation", body)
			}
			if !strings.Contains(string(body), "Remember exactly this codeword: BANANA-731") {
				t.Fatalf("second request body = %s, want replayed original user turn", body)
			}
			if !strings.Contains(string(body), `"text":"READY"`) {
				t.Fatalf("second request body = %s, want replayed assistant turn", body)
			}
			if !strings.Contains(string(body), "What was the codeword I asked you to remember?") {
				t.Fatalf("second request body = %s, want new continuation prompt", body)
			}
			_, _ = io.WriteString(w, `{"id":"resp_native_replay_2","object":"response","status":"completed","prompt_cache_key":"conv_native_replay_1","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"BANANA-731"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
		default:
			t.Fatalf("unexpected request count = %d", len(requestBodies))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-third-party",
			"base_url": server.URL + "/v1",
		},
	}

	firstPayload := []byte(`{"model":"soapapi/gpt-5.4","input":"Remember exactly this codeword: BANANA-731. Reply only READY."}`)
	firstResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "soapapi/gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: firstPayload,
	})
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	firstID := gjson.GetBytes(firstResp.Payload, "id").String()
	if firstID != "resp_native_replay_1" {
		t.Fatalf("first response id = %q, want %q", firstID, "resp_native_replay_1")
	}

	secondPayload := []byte(`{"model":"soapapi/gpt-5.4","previous_response_id":"resp_native_replay_1","input":"What was the codeword I asked you to remember? Reply with the codeword only."}`)
	secondResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "soapapi/gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: secondPayload,
	})
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if got := gjson.GetBytes(secondResp.Payload, "output.0.content.0.text").String(); got != "BANANA-731" {
		t.Fatalf("second output text = %q, want %q", got, "BANANA-731")
	}
	if len(requestBodies) != 2 {
		t.Fatalf("requestBodies = %d, want 2", len(requestBodies))
	}
}

func TestCodexExecute_AdaptsThirdPartyCodexPathToolContinuationByReplayingContext(t *testing.T) {
	resetCodexCacheStateForTest(t)
	resetCodexBridgeReplayStateForTest(t)

	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		requestBodies = append(requestBodies, string(body))

		w.Header().Set("Content-Type", "application/json")
		switch len(requestBodies) {
		case 1:
			if !strings.Contains(string(body), `"tool_choice":"required"`) {
				t.Fatalf("first request body = %s, want required tool_choice", body)
			}
			_, _ = io.WriteString(w, `{"id":"resp_native_tool_1","object":"response","status":"completed","prompt_cache_key":"conv_native_tool_1","model":"gpt-5.4","output":[{"type":"function_call","call_id":"call_tool_1","name":"get_token","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		case 2:
			if strings.Contains(string(body), `"previous_response_id":"resp_native_tool_1"`) {
				t.Fatalf("second request body = %s, want previous_response_id removed after adaptation", body)
			}
			if !strings.Contains(string(body), `"prompt_cache_key":"conv_native_tool_1"`) {
				t.Fatalf("second request body = %s, want prompt_cache_key replayed from cached response state", body)
			}
			if strings.Contains(string(body), `"tool_choice":"required"`) {
				t.Fatalf("second request body = %s, want inherited tool_choice removed for tool continuation", body)
			}
			if !strings.Contains(string(body), `"type":"function_call","call_id":"call_tool_1","name":"get_token","arguments":"{}"`) {
				t.Fatalf("second request body = %s, want replayed function_call context", body)
			}
			if !strings.Contains(string(body), `"type":"function_call_output","call_id":"call_tool_1","output":"BANANA-731"`) {
				t.Fatalf("second request body = %s, want function_call_output preserved", body)
			}
			_, _ = io.WriteString(w, `{"id":"resp_native_tool_2","object":"response","status":"completed","prompt_cache_key":"conv_native_tool_1","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"BANANA-731"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
		default:
			t.Fatalf("unexpected request count = %d", len(requestBodies))
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-third-party",
			"base_url": server.URL + "/codex/v1",
		},
	}

	firstPayload := []byte(`{"model":"yunyi-codex/gpt-5.4","tool_choice":"required","tools":[{"type":"function","name":"get_token","description":"Return the fixed token","parameters":{"type":"object","properties":{},"additionalProperties":false}}],"input":"Call get_token, then wait for the tool result."}`)
	firstResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "yunyi-codex/gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: firstPayload,
	})
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	firstID := gjson.GetBytes(firstResp.Payload, "id").String()
	if firstID != "resp_native_tool_1" {
		t.Fatalf("first response id = %q, want %q", firstID, "resp_native_tool_1")
	}

	secondPayload := []byte(`{"model":"yunyi-codex/gpt-5.4","previous_response_id":"resp_native_tool_1","input":[{"type":"function_call_output","call_id":"call_tool_1","output":"BANANA-731"}]}`)
	secondResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "yunyi-codex/gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: secondPayload,
	})
	if err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if got := gjson.GetBytes(secondResp.Payload, "output.0.content.0.text").String(); got != "BANANA-731" {
		t.Fatalf("second output text = %q, want %q", got, "BANANA-731")
	}
	if len(requestBodies) != 2 {
		t.Fatalf("requestBodies = %d, want 2", len(requestBodies))
	}
}

func TestCodexExecute_RelaysNativeOpenAIResponsesWithoutCodexRewrite(t *testing.T) {
	var capturedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/codex/v1",
		},
	}
	payload := []byte(`{"model":"yunyi-codex/gpt-5.4","input":"reply with OK","parallel_tool_calls":false,"metadata":{"trace":"1"}}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "yunyi-codex/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(capturedBody, `"include":["reasoning.encrypted_content"]`) {
		t.Fatalf("captured request body = %s, want no codex include rewrite", capturedBody)
	}
	if strings.Contains(capturedBody, `"store":true`) {
		t.Fatalf("captured request body = %s, want no codex store injection", capturedBody)
	}
	if strings.Contains(capturedBody, `"stream":true`) {
		t.Fatalf("captured request body = %s, want no forced streaming in relay mode", capturedBody)
	}
	if !strings.Contains(capturedBody, `"model":"gpt-5.4"`) {
		t.Fatalf("captured request body = %s, want base model relayed", capturedBody)
	}
	if !bytes.Equal(resp.Payload, []byte(`{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)) {
		t.Fatalf("response payload = %s, want raw upstream JSON", resp.Payload)
	}
}

func TestCodexExecute_RelaysNativeOpenAIResponsesNormalizesStringInput(t *testing.T) {
	var capturedBody string
	var hitPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/v1",
		},
	}
	payload := []byte(`{"model":"aixj/gpt-5.4","input":"reply with exactly OK","stream":false}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "aixj/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if hitPath != "/v1/responses" {
		t.Fatalf("hit path = %q, want %q", hitPath, "/v1/responses")
	}
	if strings.Contains(capturedBody, `"input":"reply with exactly OK"`) {
		t.Fatalf("captured request body = %s, want string input normalized", capturedBody)
	}
	if !strings.Contains(capturedBody, `"type":"message"`) || !strings.Contains(capturedBody, `"type":"input_text"`) {
		t.Fatalf("captured request body = %s, want message-array input", capturedBody)
	}
	if !bytes.Contains(resp.Payload, []byte(`"id":"resp_native"`)) {
		t.Fatalf("response payload = %s, want native upstream response", resp.Payload)
	}
}

func TestCodexExecute_RelaysNativeOpenAIResponsesSSEAsFinalJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_native\",\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_native\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5.4\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/codex/v1",
		},
	}
	payload := []byte(`{"model":"yunyi-codex/gpt-5.4","input":"reply with OK"}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "yunyi-codex/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := []byte(`{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	if !bytes.Equal(resp.Payload, want) {
		t.Fatalf("response payload = %s, want extracted response.completed payload", resp.Payload)
	}
}

func TestCodexExecute_CompactUsesNativeResponsesWhenInferredByCodexPath(t *testing.T) {
	var hitPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		switch r.URL.Path {
		case "/codex/v1/responses/compact":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_compact","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		case "/codex/v1/chat/completions":
			http.Error(w, "unexpected chat bridge", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/codex/v1",
		},
	}
	payload := []byte(`{"model":"yunyi-codex/gpt-5.4","input":"reply with OK"}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "yunyi-codex/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Alt:             "responses/compact",
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if hitPath != "/codex/v1/responses/compact" {
		t.Fatalf("hit path = %q, want %q", hitPath, "/codex/v1/responses/compact")
	}
	want := []byte(`{"id":"resp_compact","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	if !bytes.Equal(resp.Payload, want) {
		t.Fatalf("response payload = %s, want compact native response", resp.Payload)
	}
}

func TestCodexExecute_CompactNormalizesStringInput(t *testing.T) {
	var capturedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/compact" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_compact","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/v1",
		},
	}
	payload := []byte(`{"model":"aixj/gpt-5.4","input":"reply with exactly OK"}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "aixj/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Alt:             "responses/compact",
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(capturedBody, `"input":"reply with exactly OK"`) {
		t.Fatalf("captured request body = %s, want string input normalized", capturedBody)
	}
	if !strings.Contains(capturedBody, `"type":"message"`) || !strings.Contains(capturedBody, `"type":"input_text"`) {
		t.Fatalf("captured request body = %s, want message-array input", capturedBody)
	}
	if !bytes.Contains(resp.Payload, []byte(`"id":"resp_compact"`)) {
		t.Fatalf("response payload = %s, want compact native upstream response", resp.Payload)
	}
}

func TestCodexExecute_RelaysNativeOpenAIResponsesMixedJSONAndSSETail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"pong"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		_, _ = io.WriteString(w, "event: error\n")
		_, _ = io.WriteString(w, "data: {\"error\":\"upstream_disconnect\",\"message\":\"Upstream connection closed unexpectedly\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n")
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/codex/v1",
		},
	}
	payload := []byte(`{"model":"yunyi-codex/gpt-5.4","input":"ping","max_output_tokens":16}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "yunyi-codex/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	want := []byte(`{"id":"resp_native","object":"response","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"pong"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	if !bytes.Equal(resp.Payload, want) {
		t.Fatalf("response payload = %s, want extracted leading JSON payload", resp.Payload)
	}
}

func TestCodexExecute_PrefersClaudeMessagesBridgeBeforeNativeRelay(t *testing.T) {
	var hitPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		switch r.URL.Path {
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"resp_bridge_2\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gpt-5.4\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n")
			_, _ = io.WriteString(w, "event: message_stop\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
		case "/v1/responses":
			http.Error(w, "unexpected native relay", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Prefix: "nowcoding",
		Attributes: map[string]string{
			"api_key":             "sk-test",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "claude-messages",
		},
	}
	payload := []byte(`{"model":"gpt-5.4","input":"reply with OK"}`)

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if hitPath != "/v1/messages" {
		t.Fatalf("hit path = %q, want %q", hitPath, "/v1/messages")
	}
	if !bytes.Contains(resp.Payload, []byte(`"OK"`)) {
		t.Fatalf("response payload = %s, want OK content", resp.Payload)
	}
}

func TestCodexExecute_BridgeReusesConversationHeadersAcrossPreviousResponseID(t *testing.T) {
	var conversations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		conversations = append(conversations, r.Header.Get("Conversation_id")+"|"+r.Header.Get("Session_id"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_123","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":             "sk-test",
			"base_url":            server.URL + "/v1",
			"supported_protocols": "chat/completions",
		},
	}
	firstPayload := []byte(`{"model":"gpt-5.4","input":"reply with OK"}`)
	firstResp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: firstPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: firstPayload,
	})
	if err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	firstID := gjson.GetBytes(firstResp.Payload, "id").String()
	if firstID == "" {
		t.Fatalf("first response missing id: %s", firstResp.Payload)
	}

	secondPayload := []byte(`{"model":"gpt-5.4","previous_response_id":"` + firstID + `","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: secondPayload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: secondPayload,
	}); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}

	if len(conversations) != 2 {
		t.Fatalf("conversation headers = %v, want 2 requests", conversations)
	}
	if conversations[0] == "|" || conversations[0] == "" {
		t.Fatalf("first conversation headers = %q, want non-empty Conversation_id/Session_id", conversations[0])
	}
	if conversations[0] != conversations[1] {
		t.Fatalf("conversation headers = %v, want same headers reused across continuation", conversations)
	}
}

func TestCodexExecuteStream_RelaysNativeResponsesImmediately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if !strings.Contains(string(body), `"model":"gpt-5.4"`) {
			t.Fatalf("request body = %s, want base model", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "event: response.created\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_native\",\"status\":\"in_progress\"}}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_native\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/codex/v1",
		},
	}
	payload := []byte(`{"model":"yunyi-codex/gpt-5.4","input":"reply with OK","stream":true}`)

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "yunyi-codex/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	first, ok := <-stream
	if !ok {
		t.Fatal("expected first relay chunk")
	}
	if first.Err != nil {
		t.Fatalf("first chunk err = %v", first.Err)
	}
	if got := string(first.Payload); !strings.Contains(got, "event: response.created") || !strings.Contains(got, `"type":"response.created"`) {
		t.Fatalf("first chunk = %q, want response.created event block", got)
	}
}

func TestCodexExecuteStream_RelaysNativeResponsesNormalizesStringInputAndReusesContinuationHeaders(t *testing.T) {
	var capturedBody string
	var conversationHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		capturedBody = string(body)
		conversationHeader = r.Header.Get("Conversation_id") + "|" + r.Header.Get("Session_id")
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flush")
		}
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_native\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL + "/v1",
		},
	}
	payload := []byte(`{"model":"aixj/gpt-5.4","previous_response_id":"resp_prev","input":"reply with exactly OK","stream":true}`)

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "aixj/gpt-5.4",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	first, ok := <-stream
	if !ok {
		t.Fatal("expected first relay chunk")
	}
	if first.Err != nil {
		t.Fatalf("first chunk err = %v", first.Err)
	}
	if strings.Contains(capturedBody, `"input":"reply with exactly OK"`) {
		t.Fatalf("captured request body = %s, want string input normalized", capturedBody)
	}
	if !strings.Contains(capturedBody, `"type":"message"`) || !strings.Contains(capturedBody, `"type":"input_text"`) {
		t.Fatalf("captured request body = %s, want message-array input", capturedBody)
	}
	if conversationHeader != "resp_prev|resp_prev" {
		t.Fatalf("conversation headers = %q, want %q", conversationHeader, "resp_prev|resp_prev")
	}
}
