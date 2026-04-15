package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestManagerExecute_CodexResponsesPreferNativePool(t *testing.T) {
	mgr := newRoutingTestManager(t, nil)
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "a-bridge",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-bridge",
			"base_url": "https://gmncode.cn/v1",
		},
	}, "gpt-5.4")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "z-native|gpt-5.4" {
		t.Fatalf("codex calls = %v, want [z-native|gpt-5.4]", calls)
	}
}

func TestManagerExecute_CodexChatCompletionsPreferBridgePool(t *testing.T) {
	mgr := newRoutingTestManager(t, nil)
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "a-bridge",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-bridge",
			"base_url": "https://gmncode.cn/v1",
		},
	}, "gpt-5.4")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "a-bridge|gpt-5.4" {
		t.Fatalf("codex calls = %v, want [a-bridge|gpt-5.4]", calls)
	}
}

func TestManagerExecute_CodexResponsesAllowsBridgeOnlyPoolWhenNoNativeExists(t *testing.T) {
	mgr := newRoutingTestManager(t, nil)
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "a-bridge",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-bridge",
			"base_url": "https://gmncode.cn/v1",
		},
	}, "gpt-5.4")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "a-bridge|gpt-5.4" {
		t.Fatalf("codex calls = %v, want [a-bridge|gpt-5.4]", calls)
	}
}

func TestManagerExecute_CodexResponsesAllowsClaudeMessagesPoolWhenNoNativeExists(t *testing.T) {
	mgr := newRoutingTestManager(t, nil)
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "a-messages",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":             "sk-bridge",
			"base_url":            "https://nowcoding.ai/v1",
			"supported_protocols": "claude-messages",
		},
	}, "gpt-5.4")

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "a-messages|gpt-5.4" {
		t.Fatalf("codex calls = %v, want [a-messages|gpt-5.4]", calls)
	}
}

func TestCodexAuthSupportsNativeResponses_CodexPathInference(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-codex-path",
			"base_url": "https://cdn1.yunyi.cfd/codex/v1",
		},
	}
	if !codexAuthSupportsNativeResponses(auth) {
		t.Fatal("codexAuthSupportsNativeResponses() = false, want true for /codex/ base URL")
	}
}

func TestCodexExplicitProtocolsOverrideInference(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":             "sk-native-via-protocols",
			"base_url":            "https://example.com/v1",
			"supported_protocols": "responses, chat/completions",
		},
	}
	if !codexAuthSupportsNativeResponses(auth) {
		t.Fatal("codexAuthSupportsNativeResponses() = false, want true from explicit supported_protocols")
	}
	if !codexAuthSupportsProtocol(auth, "chat/completions") {
		t.Fatal("codexAuthSupportsProtocol(chat/completions) = false, want true")
	}
}

func TestCodexExplicitProtocolsAllowChatCompletionsBridgeThroughResponses(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":             "sk-responses-only",
			"base_url":            "https://example.com/v1",
			"supported_protocols": "responses",
		},
	}
	if !codexAuthSupportsNativeResponses(auth) {
		t.Fatal("codexAuthSupportsNativeResponses() = false, want true for responses-only upstream")
	}
	if !codexAuthSupportsProtocol(auth, "chat/completions") {
		t.Fatal("codexAuthSupportsProtocol(chat/completions) = false, want true when responses bridge is available")
	}
}

func TestCodexExplicitProtocolsAllowResponsesBridgeThroughChatCompletions(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":             "sk-chat-only",
			"base_url":            "https://example.com/v1",
			"supported_protocols": "chat/completions",
		},
	}
	if !codexAuthSupportsProtocol(auth, "responses") {
		t.Fatal("codexAuthSupportsProtocol(responses) = false, want true when chat/completions bridge is available")
	}
}

func TestCodexExplicitProtocolsAllowResponsesBridgeThroughClaudeMessages(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":             "sk-claude-messages",
			"base_url":            "https://nowcoding.ai/v1",
			"supported_protocols": "claude-messages",
		},
	}
	if codexAuthSupportsNativeResponses(auth) {
		t.Fatal("codexAuthSupportsNativeResponses() = true, want false for claude-messages only")
	}
	if !codexAuthSupportsProtocol(auth, "responses") {
		t.Fatal("codexAuthSupportsProtocol(responses) = false, want true when claude-messages bridge is available")
	}
	if !codexAuthSupportsProtocol(auth, "chat/completions") {
		t.Fatal("codexAuthSupportsProtocol(chat/completions) = false, want true when claude-messages bridge is available")
	}
}

func TestCodexInferredProtocolsTreatNowcodingAsClaudeMessages(t *testing.T) {
	auth := &Auth{
		Provider: "codex",
		Prefix:   "nowcoding",
		Attributes: map[string]string{
			"api_key":  "sk-nowcoding",
			"base_url": "https://nowcoding.ai/v1",
		},
	}
	if codexAuthSupportsNativeResponses(auth) {
		t.Fatal("codexAuthSupportsNativeResponses() = true, want false for inferred nowcoding claude-messages bridge")
	}
	if !codexAuthSupportsProtocol(auth, "responses") {
		t.Fatal("codexAuthSupportsProtocol(responses) = false, want true for inferred nowcoding claude-messages bridge")
	}
	if !codexAuthSupportsProtocol(auth, "chat/completions") {
		t.Fatal("codexAuthSupportsProtocol(chat/completions) = false, want true for inferred nowcoding claude-messages bridge")
	}
}

func TestManagerExecute_CodexResponsesStickyAffinityKeepsSameAuth(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.SetConfig(&internalconfig.Config{})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native-2",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")

	optsConv1 := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: []byte(`{"prompt_cache_key":"conv-1"}`),
	}
	optsConv2 := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: []byte(`{"prompt_cache_key":"conv-2"}`),
	}

	for _, opts := range []cliproxyexecutor.Options{optsConv1, optsConv1, optsConv2} {
		if _, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, opts); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	}

	if calls := exec.Calls(); len(calls) != 3 || calls[0] != "z-native-1|gpt-5.4" || calls[1] != "z-native-1|gpt-5.4" || calls[2] != "z-native-2|gpt-5.4" {
		t.Fatalf("codex calls = %v, want [z-native-1|gpt-5.4 z-native-1|gpt-5.4 z-native-2|gpt-5.4]", calls)
	}
}

func TestManagerExecute_CodexResponsesStickyAffinitySwitchesAfterFailure(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.SetConfig(&internalconfig.Config{})
	var failFirst bool
	exec := &routingTestExecutor{
		id: "codex",
		execFunc: func(auth *Auth, req cliproxyexecutor.Request) (cliproxyexecutor.Response, error) {
			if auth.ID == "z-native-1" && failFirst {
				failFirst = false
				return cliproxyexecutor.Response{}, &Error{HTTPStatus: 502, Message: "upstream failed"}
			}
			return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
		},
	}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native-2",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")

	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: []byte(`{"prompt_cache_key":"conv-1"}`),
	}

	if _, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, opts); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	failFirst = true
	if _, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, opts); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if _, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, opts); err != nil {
		t.Fatalf("third Execute() error = %v", err)
	}

	if calls := exec.Calls(); len(calls) != 4 || calls[0] != "z-native-1|gpt-5.4" || calls[1] != "z-native-1|gpt-5.4" || calls[2] != "z-native-2|gpt-5.4" || calls[3] != "z-native-2|gpt-5.4" {
		t.Fatalf("codex calls = %v, want [z-native-1|gpt-5.4 z-native-1|gpt-5.4 z-native-2|gpt-5.4 z-native-2|gpt-5.4]", calls)
	}
}

func TestManagerExecute_CodexPreviousResponseIDAffinityKeepsSameAuth(t *testing.T) {
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.SetConfig(&internalconfig.Config{})
	exec := &routingTestExecutor{id: "codex"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native-1",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")
	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "z-native-2",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": "https://chatgpt.com/backend-api/codex",
		},
	}, "gpt-5.4")

	RememberResponseAffinityIDs([]string{"resp_affinity_123"}, "z-native-2")
	if _, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.4"}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: []byte(`{"previous_response_id":"resp_affinity_123"}`),
	}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "z-native-2|gpt-5.4" {
		t.Fatalf("codex calls = %v, want [z-native-2|gpt-5.4]", calls)
	}
}
