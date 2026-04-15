package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestGetLoadBalancerRuntime_IncludesProvidersPrefixesAuthsAndAdaptiveProbes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	authManager := cliproxyauth.NewManager(nil, nil, nil)
	authManager.SetConfig(&config.Config{})
	authManager.RegisterExecutor(&providerHealthTestExecutor{})

	auth := &cliproxyauth.Auth{
		ID:       "lb-auth-1",
		Provider: "codex",
		Prefix:   "yunyi-codex",
		Label:    "yunyi-main",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://yunyi.example/v1",
		},
	}
	if _, err := authManager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{
		{ID: "gpt-5.3-codex"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	if _, err := authManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: "gpt-5.3-codex",
	}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("execute auth: %v", err)
	}

	h := NewHandler(&config.Config{}, configPath, authManager)
	h.NotifyAdaptiveProbeFailure("yunyi-codex", "yunyi-codex/gpt-5.3-codex", http.StatusServiceUnavailable, "cpams_invalidate_status_503")

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/load-balancer-runtime?event_limit=10", nil)

	h.GetLoadBalancerRuntime(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload loadBalancerRuntimePayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.GeneratedAt == "" {
		t.Fatalf("generated_at is empty")
	}
	if len(payload.Providers) != 1 {
		t.Fatalf("providers len = %d, want 1", len(payload.Providers))
	}
	if payload.Providers[0].Provider != "codex" {
		t.Fatalf("provider = %q, want codex", payload.Providers[0].Provider)
	}
	if payload.Providers[0].TotalAuths != 1 {
		t.Fatalf("provider total_auths = %d, want 1", payload.Providers[0].TotalAuths)
	}

	if len(payload.Prefixes) != 1 {
		t.Fatalf("prefixes len = %d, want 1", len(payload.Prefixes))
	}
	prefix := payload.Prefixes[0]
	if prefix.Prefix != "yunyi-codex" {
		t.Fatalf("prefix = %q, want yunyi-codex", prefix.Prefix)
	}
	if prefix.AuthCount != 1 {
		t.Fatalf("prefix auth_count = %d, want 1", prefix.AuthCount)
	}
	if prefix.SuccessCount != 1 {
		t.Fatalf("prefix success_count = %d, want 1", prefix.SuccessCount)
	}
	if prefix.AdaptiveProbe == nil || prefix.AdaptiveProbe.Prefix != "yunyi-codex" {
		t.Fatalf("prefix adaptive_probe = %#v, want yunyi-codex", prefix.AdaptiveProbe)
	}

	if len(payload.Auths) != 1 {
		t.Fatalf("auths len = %d, want 1", len(payload.Auths))
	}
	if payload.Auths[0].AuthID != auth.ID {
		t.Fatalf("auth id = %q, want %q", payload.Auths[0].AuthID, auth.ID)
	}
	if payload.Auths[0].Prefix != "yunyi-codex" {
		t.Fatalf("auth prefix = %q, want yunyi-codex", payload.Auths[0].Prefix)
	}

	if len(payload.AdaptiveProbes) != 1 {
		t.Fatalf("adaptive probes len = %d, want 1", len(payload.AdaptiveProbes))
	}
	if payload.AdaptiveProbes[0].LastFailureStatus != http.StatusServiceUnavailable {
		t.Fatalf("adaptive probe failure status = %d, want 503", payload.AdaptiveProbes[0].LastFailureStatus)
	}

	if payload.RoutingSnapshot.GeneratedAt == "" {
		t.Fatalf("routing snapshot generated_at is empty")
	}
	if len(payload.RoutingSnapshot.Providers) == 0 {
		t.Fatalf("routing snapshot providers is empty")
	}
	if len(payload.RecentEvents) != len(payload.RoutingSnapshot.RecentEvents) {
		t.Fatalf("recent events len = %d, routing snapshot recent events len = %d, want equal", len(payload.RecentEvents), len(payload.RoutingSnapshot.RecentEvents))
	}
}
