package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestManagerExecute_OpenAICompatPrefixedThinkingAliasRoutesToConfiguredUpstream(t *testing.T) {
	mgr := newRoutingTestManager(t, &internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			ForceModelPrefix: true,
		},
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name:    "whitedream-max",
				Prefix:  "whitedream-max",
				BaseURL: "https://llm.whitedream.top",
				APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{
					{APIKey: "sk-test"},
				},
				Models: []internalconfig.OpenAICompatibilityModel{
					{Name: "[Max]claude_opus_4-6-thinking", Alias: "claude-opus-4-6-thinking"},
					{Name: "[Max]claude_opus_4-6", Alias: "claude-opus-4-6"},
				},
			},
		},
	})
	exec := &routingTestExecutor{id: "whitedream-max"}
	mgr.RegisterExecutor(exec)

	registerRoutingTestAuth(t, mgr, &Auth{
		ID:       "compat-1",
		Provider: "whitedream-max",
		Prefix:   "whitedream-max",
		Attributes: map[string]string{
			"api_key":      "sk-test",
			"base_url":     "https://llm.whitedream.top",
			"compat_name":  "whitedream-max",
			"provider_key": "whitedream-max",
			"auth_kind":    "apikey",
		},
	}, "whitedream-max/claude-opus-4-6-thinking", "whitedream-max/claude-opus-4-6")

	_, err := mgr.Execute(context.Background(), []string{"whitedream-max"}, cliproxyexecutor.Request{
		Model: "whitedream-max/claude-opus-4-6-thinking",
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if calls := exec.Calls(); len(calls) != 1 || calls[0] != "compat-1|[Max]claude_opus_4-6-thinking" {
		t.Fatalf("calls = %v, want [compat-1|[Max]claude_opus_4-6-thinking]", calls)
	}
}
