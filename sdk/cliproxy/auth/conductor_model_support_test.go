package auth

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestAuthSupportsRequestedModel_PrefixedRegistrationAcceptsBareModel(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	auth := &Auth{
		ID:       "claude-prefixed-test",
		Provider: "claude",
		Prefix:   "covs",
	}
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: "covs/claude-sonnet-4-6"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	mgr := NewManager(nil, nil, nil)
	if !mgr.authSupportsRequestedModel(auth, "claude-sonnet-4-6", reg) {
		t.Fatal("expected bare claude model to be accepted via prefixed registration")
	}
}

func TestAuthSupportsRequestedModel_StripsForeignPrefixForSameProvider(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	auth := &Auth{
		ID:       "claude-foreign-prefix-test",
		Provider: "claude",
		Prefix:   "covs",
	}
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: "covs/claude-sonnet-4-6"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	mgr := NewManager(nil, nil, nil)
	if !mgr.authSupportsRequestedModel(auth, "yunyi-claude/claude-sonnet-4-6", reg) {
		t.Fatal("expected foreign provider prefix to be stripped for same-provider auth matching")
	}
}

func TestAuthSupportsRequestedModel_ExcludedModelsRejectForeignPrefixedMatch(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	auth := &Auth{
		ID:       "claude-excluded-test",
		Provider: "claude",
		Prefix:   "covs",
		Attributes: map[string]string{
			"excluded_models": "claude-sonnet-4-6",
		},
	}
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: "covs/claude-sonnet-4-6"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	mgr := NewManager(nil, nil, nil)
	if mgr.authSupportsRequestedModel(auth, "yunyi-claude/claude-sonnet-4-6", reg) {
		t.Fatal("expected excluded_models to reject foreign-prefixed claude fallback")
	}
}
