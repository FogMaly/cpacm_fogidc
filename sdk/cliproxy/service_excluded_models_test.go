package cliproxy

import (
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestRegisterModelsForAuth_UsesPreMergedExcludedModelsAttribute(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"gemini-cli": {"gemini-2.5-pro"},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-gemini-cli",
		Provider: "gemini-cli",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"auth_kind":       "oauth",
			"excluded_models": "gemini-2.5-flash",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	models := registry.GetAvailableModelsByProvider("gemini-cli")
	if len(models) == 0 {
		t.Fatal("expected gemini-cli models to be registered")
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if strings.EqualFold(modelID, "gemini-2.5-flash") {
			t.Fatalf("expected model %q to be excluded by auth attribute", modelID)
		}
	}

	seenGlobalExcluded := false
	for _, model := range models {
		if model == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(model.ID), "gemini-2.5-pro") {
			seenGlobalExcluded = true
			break
		}
	}
	if !seenGlobalExcluded {
		t.Fatal("expected global excluded model to be present when attribute override is set")
	}
}

func TestRegisterModelsForAuth_NowcodingRegistersConfiguredAliasAndUpstream(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
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
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-nowcoding",
		Provider: "codex",
		Prefix:   "nowcoding",
		Status:   coreauth.StatusActive,
		Label:    "nowcoding",
		Attributes: map[string]string{
			"api_key":  "nowcoding-key",
			"base_url": "https://nowcoding.ai/v1",
			"prefix":   "nowcoding",
			"name":     "nowcoding",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	for _, want := range []string{
		"gpt-5.4",
		"nowcoding/gpt-5.4",
		"claude-sonnet-4-6",
		"nowcoding/claude-sonnet-4-6",
	} {
		if !registry.ClientSupportsModel(auth.ID, want) {
			t.Fatalf("expected registered model %q for auth %q", want, auth.ID)
		}
	}
	for _, blocked := range []string{"gpt-5.4-pro", "nowcoding/gpt-5.4-pro", "o4-mini", "nowcoding/o4-mini"} {
		if registry.ClientSupportsModel(auth.ID, blocked) {
			t.Fatalf("did not expect registered model %q for auth %q", blocked, auth.ID)
		}
	}
}

func TestRegisterModelsForAuth_ForcePrefixedConfiguredModelsIgnoreWildcardExclude(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{
				ForceModelPrefix: true,
			},
			ClaudeKey: []internalconfig.ClaudeKey{
				{
					APIKey:  "claude-cheep-key",
					Name:    "claude cheep",
					Prefix:  "claude cheep",
					BaseURL: "https://llm.whitedream.top",
					Models: []internalconfig.ClaudeModel{
						{Name: "[超低价]claude-sonnet-4-6", Alias: "claude-sonnet-4-6"},
						{Name: "[限时]claude-opus-4-6-thinking", Alias: "claude-opus-4-6-thinking"},
					},
					ExcludedModels: []string{"*"},
				},
			},
		},
	}
	auth := &coreauth.Auth{
		ID:       "auth-claude-cheep",
		Provider: "claude",
		Prefix:   "claude cheep",
		Status:   coreauth.StatusActive,
		Label:    "claude cheep",
		Attributes: map[string]string{
			"api_key":  "claude-cheep-key",
			"base_url": "https://llm.whitedream.top",
			"prefix":   "claude cheep",
			"name":     "claude cheep",
		},
	}

	registry := GlobalModelRegistry()
	registry.UnregisterClient(auth.ID)
	t.Cleanup(func() {
		registry.UnregisterClient(auth.ID)
	})

	service.registerModelsForAuth(auth)

	for _, want := range []string{
		"claude cheep/claude-sonnet-4-6",
		"claude cheep/claude-opus-4-6-thinking",
	} {
		if !registry.ClientSupportsModel(auth.ID, want) {
			t.Fatalf("expected prefixed configured model %q for auth %q", want, auth.ID)
		}
	}
	for _, blocked := range []string{
		"claude-sonnet-4-6",
		"claude-opus-4-6-thinking",
	} {
		if registry.ClientSupportsModel(auth.ID, blocked) {
			t.Fatalf("did not expect bare model %q for auth %q when force prefix is enabled", blocked, auth.ID)
		}
	}
}
