package covsactivation

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestServiceActivateAndAuthenticate(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Host: "127.0.0.1",
		Port: 34050,
		COVSActivation: config.COVSActivationConfig{
			Enable:                      true,
			PublicBaseURL:               "http://127.0.0.1:34050",
			StateFile:                   filepath.Join(dir, "covs-state.json"),
			DefaultClaudeProviderPrefix: "covs",
			Cards: []config.COVSActivationCard{
				{Code: "BTZO-4P80-ZGF3-DMOK", Type: "month", Product: "claude", DurationDays: 30},
			},
		},
	}
	svc := NewService(cfg, filepath.Join(dir, "config.yaml"))

	resp, status, err := svc.Activate(ActivationRequest{
		CardCode: "BTZO-4P80-ZGF3-DMOK",
		DeviceID: "device-1",
		DeviceInfo: map[string]any{
			"platform": "linux",
		},
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("Activate status=%d err=%v", status, err)
	}
	if resp.Product != "claude" {
		t.Fatalf("product = %q, want claude", resp.Product)
	}
	if resp.AnthropicBaseURL != "http://127.0.0.1:34050" {
		t.Fatalf("anthropicBaseUrl = %q", resp.AnthropicBaseURL)
	}

	auth, ok := svc.AuthenticateToken(resp.Token)
	if !ok {
		t.Fatal("AuthenticateToken returned false")
	}
	if auth.ModelPrefix != "covs" {
		t.Fatalf("model prefix = %q, want covs", auth.ModelPrefix)
	}

	_, status, err = svc.Activate(ActivationRequest{
		CardCode: "BTZO-4P80-ZGF3-DMOK",
		DeviceID: "device-2",
	})
	if err == nil || status != http.StatusBadRequest {
		t.Fatalf("expected device mismatch, status=%d err=%v", status, err)
	}

	if _, err := os.Stat(cfg.COVSActivation.StateFile); err != nil {
		t.Fatalf("state file stat: %v", err)
	}
}

func TestAccessProviderAuthenticatesXAPIKey(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		COVSActivation: config.COVSActivationConfig{
			Enable:                      true,
			StateFile:                   filepath.Join(dir, "covs-state.json"),
			DefaultClaudeProviderPrefix: "covs",
			Cards: []config.COVSActivationCard{
				{Code: "TEST-CARD", Product: "claude"},
			},
		},
	}
	svc := NewService(cfg, filepath.Join(dir, "config.yaml"))
	resp, _, err := svc.Activate(ActivationRequest{CardCode: "TEST-CARD", DeviceID: "dev-1"})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	provider := &accessProvider{service: svc}
	req, err := http.NewRequest(http.MethodPost, "http://example.com/v1/messages", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Api-Key", resp.Token)

	result, authErr := provider.Authenticate(req.Context(), req)
	if authErr != nil {
		t.Fatalf("Authenticate error: %v", authErr)
	}
	if result == nil || result.Principal != resp.Token {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Metadata["model_prefix"] != "covs" {
		t.Fatalf("model_prefix = %q, want covs", result.Metadata["model_prefix"])
	}
}
