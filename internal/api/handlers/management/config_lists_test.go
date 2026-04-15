package management

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestPreserveUnnamedEntryNames_MatchesByIdentityAfterDelete(t *testing.T) {
	existing := []config.CodexKey{
		{APIKey: "codex-a", BaseURL: "https://a.example.com", Name: "Alpha"},
		{APIKey: "codex-b", BaseURL: "https://b.example.com", Name: "Beta"},
	}
	incoming := []config.CodexKey{
		{APIKey: "codex-b", BaseURL: "https://b.example.com"},
	}

	preserveUnnamedEntryNames(incoming, existing, func(entry *config.CodexKey) *string {
		return &entry.Name
	}, codexKeyIdentity)

	if got := incoming[0].Name; got != "Beta" {
		t.Fatalf("incoming[0].Name = %q, want %q", got, "Beta")
	}
}

func TestPreserveUnnamedEntryNames_FallsBackToSameIndexWhenLegacyUIDropsName(t *testing.T) {
	existing := []config.CodexKey{
		{APIKey: "codex-a", BaseURL: "https://a.example.com", Name: "Alpha"},
	}
	incoming := []config.CodexKey{
		{APIKey: "codex-renamed", BaseURL: "https://new.example.com"},
	}

	preserveUnnamedEntryNames(incoming, existing, func(entry *config.CodexKey) *string {
		return &entry.Name
	}, codexKeyIdentity)

	if got := incoming[0].Name; got != "Alpha" {
		t.Fatalf("incoming[0].Name = %q, want %q", got, "Alpha")
	}
}

func TestPreserveUnnamedEntryNames_DoesNotOverwriteProvidedName(t *testing.T) {
	existing := []config.CodexKey{
		{APIKey: "codex-a", BaseURL: "https://a.example.com", Name: "Alpha"},
	}
	incoming := []config.CodexKey{
		{APIKey: "codex-a", BaseURL: "https://a.example.com", Name: "Custom"},
	}

	preserveUnnamedEntryNames(incoming, existing, func(entry *config.CodexKey) *string {
		return &entry.Name
	}, codexKeyIdentity)

	if got := incoming[0].Name; got != "Custom" {
		t.Fatalf("incoming[0].Name = %q, want %q", got, "Custom")
	}
}
