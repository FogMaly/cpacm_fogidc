package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorCompactPassthrough(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"gpt-5.1-codex-max","input":[{"role":"user","content":"hi"}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.1-codex-max",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses/compact" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/responses/compact")
	}
	if !gjson.GetBytes(gotBody, "input").Exists() {
		t.Fatalf("expected input in body")
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Fatalf("unexpected messages in body")
	}
	if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorCompactExtractsJSONFromSSE(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"resp_sse\",\"object\":\"response.compaction\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"gpt-5.1-codex-max","input":[{"role":"user","content":"hi"}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.1-codex-max",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/v1/responses/compact" {
		t.Fatalf("path = %q, want %q", gotPath, "/v1/responses/compact")
	}
	if string(resp.Payload) != `{"id":"resp_sse","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorResolveCredentials_NormalizesRootBaseURL(t *testing.T) {
	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	baseURL, apiKey := executor.resolveCredentials(&cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": "https://gmncode.cn",
		"api_key":  "test",
	}})

	if baseURL != "https://gmncode.cn/v1" {
		t.Fatalf("baseURL = %q, want %q", baseURL, "https://gmncode.cn/v1")
	}
	if apiKey != "test" {
		t.Fatalf("apiKey = %q, want %q", apiKey, "test")
	}
}

func TestOpenAICompatExecutorExecute_RetriesTransientTransportError(t *testing.T) {
	attempts := 0
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, timeoutErr(`Post "https://llm.whitedream.top/v1/chat/completions": net/http: timeout awaiting response headers`)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`,
			)),
		}, nil
	}))

	executor := NewOpenAICompatExecutor("claude", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": "https://llm.whitedream.top/v1",
		"api_key":  "test",
	}}
	resp, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"Reply with exactly OK"}],"stream":false}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !strings.Contains(string(resp.Payload), `"content":"OK"`) {
		t.Fatalf("payload = %s, want translated OK response", string(resp.Payload))
	}
}

func TestOpenAICompatExecutorExecuteStream_FallbacksClaudeBridgeAfterRetryableStatus(t *testing.T) {
	var requests [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, append([]byte(nil), body...))
		if len(requests) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = io.WriteString(w, `{"error":{"message":"openai_error","type":"bad_response_status_code"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_fallback","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("claude", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"claude-opus-4-6","stream":true,"max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"Reply with exactly OK"}]}]}`)

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		Stream:          true,
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error = %v", err)
	}

	var joined bytes.Buffer
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
		joined.WriteByte('\n')
	}

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if !gjson.GetBytes(requests[0], "stream").Bool() {
		t.Fatalf("first request body = %s, want stream=true", requests[0])
	}
	if gjson.GetBytes(requests[1], "stream").Bool() {
		t.Fatalf("second request body = %s, want stream=false fallback", requests[1])
	}
	output := joined.String()
	if !strings.Contains(output, `"type":"message_start"`) {
		t.Fatalf("stream output = %q, want synthesized message_start", output)
	}
	if !strings.Contains(output, `"type":"message_stop"`) {
		t.Fatalf("stream output = %q, want synthesized message_stop", output)
	}
	if !strings.Contains(output, `OK`) {
		t.Fatalf("stream output = %q, want fallback text", output)
	}
}

func TestOpenAICompatExecutorExecuteStream_FallbacksClaudeBridgeAfterEmptyStream(t *testing.T) {
	var requests [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, append([]byte(nil), body...))
		if len(requests) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_empty_fallback","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("claude", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"claude-opus-4-6","stream":true,"max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"Reply with exactly Recovered"}]}]}`)

	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		Stream:          true,
		OriginalRequest: payload,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error = %v", err)
	}

	var joined bytes.Buffer
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("stream chunk err = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
		joined.WriteByte('\n')
	}

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if !gjson.GetBytes(requests[0], "stream").Bool() {
		t.Fatalf("first request body = %s, want stream=true", requests[0])
	}
	if gjson.GetBytes(requests[1], "stream").Bool() {
		t.Fatalf("second request body = %s, want stream=false fallback", requests[1])
	}
	output := joined.String()
	if !strings.Contains(output, `"type":"message_start"`) {
		t.Fatalf("stream output = %q, want synthesized message_start", output)
	}
	if !strings.Contains(output, `"type":"message_stop"`) {
		t.Fatalf("stream output = %q, want synthesized message_stop", output)
	}
	if !strings.Contains(output, `Recovered`) {
		t.Fatalf("stream output = %q, want fallback text", output)
	}
}
