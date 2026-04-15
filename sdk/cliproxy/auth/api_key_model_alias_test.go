package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestLookupAPIKeyUpstreamModel(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey:  "k",
				BaseURL: "https://example.com",
				Models: []internalconfig.GeminiModel{
					{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"},
					{Name: "gemini-2.5-flash(low)", Alias: "g25f"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "gemini", Attributes: map[string]string{"api_key": "k", "base_url": "https://example.com"}})

	tests := []struct {
		name   string
		authID string
		input  string
		want   string
	}{
		// Fast path + suffix preservation
		{"alias with suffix", "a1", "g25p(8192)", "gemini-2.5-pro-exp-03-25(8192)"},
		{"alias without suffix", "a1", "g25p", "gemini-2.5-pro-exp-03-25"},

		// Config suffix takes priority
		{"config suffix priority", "a1", "g25f(high)", "gemini-2.5-flash(low)"},
		{"config suffix no user suffix", "a1", "g25f", "gemini-2.5-flash(low)"},

		// Case insensitive
		{"uppercase alias", "a1", "G25P", "gemini-2.5-pro-exp-03-25"},
		{"mixed case with suffix", "a1", "G25p(4096)", "gemini-2.5-pro-exp-03-25(4096)"},

		// Direct name lookup
		{"upstream name direct", "a1", "gemini-2.5-pro-exp-03-25", "gemini-2.5-pro-exp-03-25"},
		{"upstream name with suffix", "a1", "gemini-2.5-pro-exp-03-25(8192)", "gemini-2.5-pro-exp-03-25(8192)"},

		// Cache miss scenarios
		{"non-existent auth", "non-existent", "g25p", ""},
		{"unknown alias", "a1", "unknown-alias", ""},
		{"empty auth ID", "", "g25p", ""},
		{"empty model", "a1", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input)
			if resolved != tt.want {
				t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
			}
		})
	}
}

func TestAPIKeyModelAlias_ConfigHotReload(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "k",
				Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"}},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "a1", Provider: "gemini", Attributes: map[string]string{"api_key": "k"}})

	// Initial alias
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "g25p"); resolved != "gemini-2.5-pro-exp-03-25" {
		t.Fatalf("before reload: got %q, want %q", resolved, "gemini-2.5-pro-exp-03-25")
	}

	// Hot reload with new alias
	mgr.SetConfig(&internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{
				APIKey: "k",
				Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-flash", Alias: "g25p"}},
			},
		},
	})

	// New alias should take effect
	if resolved := mgr.lookupAPIKeyUpstreamModel("a1", "g25p"); resolved != "gemini-2.5-flash" {
		t.Fatalf("after reload: got %q, want %q", resolved, "gemini-2.5-flash")
	}
}

func TestAPIKeyModelAlias_MultipleProviders(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{{APIKey: "gemini-key", Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-pro", Alias: "gp"}}}},
		ClaudeKey: []internalconfig.ClaudeKey{{APIKey: "claude-key", Models: []internalconfig.ClaudeModel{{Name: "claude-sonnet-4", Alias: "cs4"}}}},
		CodexKey:  []internalconfig.CodexKey{{APIKey: "codex-key", Models: []internalconfig.CodexModel{{Name: "o3", Alias: "o"}}}},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	_, _ = mgr.Register(ctx, &Auth{ID: "gemini-auth", Provider: "gemini", Attributes: map[string]string{"api_key": "gemini-key"}})
	_, _ = mgr.Register(ctx, &Auth{ID: "claude-auth", Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}})
	_, _ = mgr.Register(ctx, &Auth{ID: "codex-auth", Provider: "codex", Attributes: map[string]string{"api_key": "codex-key"}})

	tests := []struct {
		authID, input, want string
	}{
		{"gemini-auth", "gp", "gemini-2.5-pro"},
		{"claude-auth", "cs4", "claude-sonnet-4"},
		{"codex-auth", "o", "o3"},
	}

	for _, tt := range tests {
		if resolved := mgr.lookupAPIKeyUpstreamModel(tt.authID, tt.input); resolved != tt.want {
			t.Errorf("lookupAPIKeyUpstreamModel(%q, %q) = %q, want %q", tt.authID, tt.input, resolved, tt.want)
		}
	}
}

func TestApplyAPIKeyModelAlias(t *testing.T) {
	cfg := &internalconfig.Config{
		GeminiKey: []internalconfig.GeminiKey{
			{APIKey: "k", Models: []internalconfig.GeminiModel{{Name: "gemini-2.5-pro-exp-03-25", Alias: "g25p"}}},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	apiKeyAuth := &Auth{ID: "a1", Provider: "gemini", Attributes: map[string]string{"api_key": "k"}}
	oauthAuth := &Auth{ID: "oauth-auth", Provider: "gemini", Attributes: map[string]string{"auth_kind": "oauth"}}
	_, _ = mgr.Register(ctx, apiKeyAuth)

	tests := []struct {
		name       string
		auth       *Auth
		inputModel string
		wantModel  string
	}{
		{
			name:       "api_key auth with alias",
			auth:       apiKeyAuth,
			inputModel: "g25p(8192)",
			wantModel:  "gemini-2.5-pro-exp-03-25(8192)",
		},
		{
			name:       "oauth auth passthrough",
			auth:       oauthAuth,
			inputModel: "some-model",
			wantModel:  "some-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolvedModel := mgr.applyAPIKeyModelAlias(tt.auth, tt.inputModel)

			if resolvedModel != tt.wantModel {
				t.Errorf("model = %q, want %q", resolvedModel, tt.wantModel)
			}
		})
	}
}

func TestApplyAPIKeyModelAlias_CodexCompatibilityFallback(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:  "codex-key",
				BaseURL: "https://ai.last.ee",
				Models: []internalconfig.CodexModel{
					{Name: "gpt-5.3-codex", Alias: ""},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Prefix:   "sub2api",
		Attributes: map[string]string{
			"api_key":  "codex-key",
			"base_url": "https://ai.last.ee",
		},
	}
	_, _ = mgr.Register(ctx, auth)

	resolved := mgr.applyAPIKeyModelAlias(auth, "gpt-5.1-codex")
	if resolved != "gpt-5.3-codex" {
		t.Fatalf("model = %q, want %q", resolved, "gpt-5.3-codex")
	}

	resolvedWithSuffix := mgr.applyAPIKeyModelAlias(auth, "gpt-5.2-codex(high)")
	if resolvedWithSuffix != "gpt-5.3-codex(high)" {
		t.Fatalf("model with suffix = %q, want %q", resolvedWithSuffix, "gpt-5.3-codex(high)")
	}
}

func TestApplyAPIKeyModelAlias_NowcodingMapsConfiguredGPT54Alias(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:  "nowcoding-key",
				Name:    "nowcoding",
				Prefix:  "nowcoding",
				BaseURL: "https://nowcoding.ai/v1",
				Models: []internalconfig.CodexModel{
					{Name: "claude-sonnet-4-6", Alias: "gpt-5.4"},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{
		ID:       "nowcoding-auth",
		Provider: "codex",
		Prefix:   "nowcoding",
		Label:    "nowcoding",
		Attributes: map[string]string{
			"api_key":  "nowcoding-key",
			"base_url": "https://nowcoding.ai/v1",
			"prefix":   "nowcoding",
			"name":     "nowcoding",
		},
	}
	_, _ = mgr.Register(ctx, auth)

	if resolved := mgr.applyAPIKeyModelAlias(auth, "gpt-5.4"); resolved != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want %q", resolved, "claude-sonnet-4-6")
	}
	if resolved := mgr.applyAPIKeyModelAlias(auth, "gpt-5.4(high)"); resolved != "claude-sonnet-4-6(high)" {
		t.Fatalf("model with suffix = %q, want %q", resolved, "claude-sonnet-4-6(high)")
	}
	if resolved := mgr.applyAPIKeyModelAlias(auth, "o4-mini"); resolved != "o4-mini" {
		t.Fatalf("unexpected fallback model = %q, want %q", resolved, "o4-mini")
	}
}

func TestApplyAPIKeyModelAlias_CodexCompatibilityFallback_8200Codex(t *testing.T) {
	cfg := &internalconfig.Config{
		CodexKey: []internalconfig.CodexKey{
			{
				APIKey:  "codex-8200-key",
				Prefix:  "8200codex",
				BaseURL: "http://49.51.249.22/v1",
				Models: []internalconfig.CodexModel{
					{Name: "gpt-5.3-codex", Alias: ""},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{
		ID:       "codex-8200-auth",
		Provider: "codex",
		Prefix:   "8200codex",
		Attributes: map[string]string{
			"api_key":  "codex-8200-key",
			"base_url": "http://49.51.249.22/v1",
		},
	}
	_, _ = mgr.Register(ctx, auth)

	resolved := mgr.applyAPIKeyModelAlias(auth, "gpt-5.1-codex")
	if resolved != "gpt-5.3-codex" {
		t.Fatalf("model = %q, want %q", resolved, "gpt-5.3-codex")
	}

	resolvedWithSuffix := mgr.applyAPIKeyModelAlias(auth, "gpt-5.2-codex(high)")
	if resolvedWithSuffix != "gpt-5.3-codex(high)" {
		t.Fatalf("model with suffix = %q, want %q", resolvedWithSuffix, "gpt-5.3-codex(high)")
	}
}

func TestApplyAPIKeyModelAlias_ClaudeCompatibilityFallback(t *testing.T) {
	cfg := &internalconfig.Config{
		ClaudeKey: []internalconfig.ClaudeKey{
			{
				APIKey:  "claude-key",
				BaseURL: "https://cdn1.yunyi.cfd/claude",
				Models: []internalconfig.ClaudeModel{
					{Name: "claude-sonnet-4-6", Alias: ""},
				},
			},
		},
	}

	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)

	ctx := context.Background()
	auth := &Auth{
		ID:       "claude-auth",
		Provider: "claude",
		Prefix:   "yunyi-claude",
		Attributes: map[string]string{
			"api_key":  "claude-key",
			"base_url": "https://cdn1.yunyi.cfd/claude",
		},
	}
	_, _ = mgr.Register(ctx, auth)

	resolved := mgr.applyAPIKeyModelAlias(auth, "claude-opus-4-6")
	if resolved != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want %q", resolved, "claude-sonnet-4-6")
	}

	resolvedThinking := mgr.applyAPIKeyModelAlias(auth, "claude-sonnet-4-5-thinking")
	if resolvedThinking != "claude-sonnet-4-5-20250929" {
		t.Fatalf("model = %q, want %q", resolvedThinking, "claude-sonnet-4-5-20250929")
	}
}
