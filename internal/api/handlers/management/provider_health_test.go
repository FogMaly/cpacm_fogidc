package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type providerHealthTestExecutor struct{}

func (e *providerHealthTestExecutor) Identifier() string { return "codex" }

func (e *providerHealthTestExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *providerHealthTestExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	return nil, errors.New("not implemented")
}

func (e *providerHealthTestExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *providerHealthTestExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *providerHealthTestExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestGetProviderHealth_ClassifiesCompatProvidersAndIncludesUnifiedSignals(t *testing.T) {
	gin.SetMode(gin.TestMode)

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	gmncode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-gmn-openai" {
			t.Fatalf("authorization = %q, want Bearer sk-gmn-openai", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"isValid":   true,
			"remaining": 100000000.0,
			"quota": map[string]any{
				"limit":     100000000.0,
				"remaining": 100000000.0,
				"used":      0.0,
				"unit":      "USD",
			},
			"usage": map[string]any{
				"today": map[string]any{
					"actual_cost": 1.25,
					"cost":        1.25,
					"requests":    12,
				},
			},
		})
	}))
	t.Cleanup(gmncode.Close)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filepath.Join(staticDir, "model-health-multi.json"), []byte(`{
  "updated_at": "2026-03-28T12:00:00Z",
  "interval_sec": 300,
  "entries": [
    {
      "updated_at": "2026-03-28T12:00:00Z",
      "provider_prefix": "",
      "base_url": "`+gmncode.URL+`/v1",
      "models": [
        {
          "model": "gpt-5.3-codex",
          "status": "green",
          "reason": "ok",
          "tested_at": "2026-03-28T12:00:00Z"
        }
      ]
    }
  ]
}`), 0o600); err != nil {
		t.Fatalf("write probe snapshot: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	authManager := cliproxyauth.NewManager(nil, nil, nil)
	authManager.SetConfig(&config.Config{})
	authManager.RegisterExecutor(&providerHealthTestExecutor{})
	if _, err := authManager.Register(context.Background(), &cliproxyauth.Auth{
		ID:       "gmn-runtime",
		Provider: "codex",
		Label:    "openai",
		Attributes: map[string]string{
			"api_key":  "sk-gmn-openai",
			"base_url": gmncode.URL + "/v1",
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient("gmn-runtime", "codex", []*registry.ModelInfo{
		{ID: "gpt-5.3-codex"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient("gmn-runtime")
	})
	if _, err := authManager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model: "gpt-5.3-codex",
	}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("execute auth for runtime health: %v", err)
	}

	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "gmncode",
				BaseURL: gmncode.URL + "/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "sk-gmn-openai"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "gpt-5.3-codex", Alias: "gpt-5-codex"},
				},
			},
			{
				Name:    "nvidia",
				BaseURL: "https://integrate.api.nvidia.com/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "sk-nvidia"},
				},
			},
		},
	}

	h := NewHandler(cfg, configPath, authManager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-health", nil)

	h.GetProviderHealth(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload providerHealthPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(payload.Items))
	}

	var gmnItem *providerHealthItem
	var nvidiaItem *providerHealthItem
	for i := range payload.Items {
		item := &payload.Items[i]
		switch {
		case item.BaseURL == gmncode.URL+"/v1":
			gmnItem = item
		case item.BaseURL == "https://integrate.api.nvidia.com/v1":
			nvidiaItem = item
		}
	}

	if gmnItem == nil {
		t.Fatalf("missing gmncode item in payload: %+v", payload.Items)
	}
	if gmnItem.Category != "codex" {
		t.Fatalf("gmncode category = %q, want codex", gmnItem.Category)
	}
	if gmnItem.Balance == nil || gmnItem.Balance.Status != "ok" {
		t.Fatalf("gmncode balance = %#v, want ok balance item", gmnItem.Balance)
	}
	if gmnItem.Total != 1 || gmnItem.Healthy != 1 {
		t.Fatalf("gmncode counts = total:%d healthy:%d, want total:1 healthy:1", gmnItem.Total, gmnItem.Healthy)
	}
	if gmnItem.OverallStatus != "healthy" {
		t.Fatalf("gmncode overall_status = %q, want healthy", gmnItem.OverallStatus)
	}
	if gmnItem.Score <= 0 {
		t.Fatalf("gmncode score = %v, want > 0", gmnItem.Score)
	}

	if nvidiaItem == nil {
		t.Fatalf("missing nvidia item in payload: %+v", payload.Items)
	}
	if nvidiaItem.Category != "other" {
		t.Fatalf("nvidia category = %q, want other", nvidiaItem.Category)
	}
}

func TestGetProviderHealth_DegradesResponseModelMismatchAndIncludesTraffic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filepath.Join(staticDir, "model-health-multi.json"), []byte(`{
  "updated_at": "2026-04-01T11:34:45Z",
  "interval_sec": 300,
  "entries": [
    {
      "updated_at": "2026-04-01T11:34:45Z",
      "provider_prefix": "鹤辞ai",
      "base_url": "https://example.com/claude",
      "models": [
        {
          "model": "鹤辞ai/claude-opus-4-6",
          "upstream_model": "claude-opus-4-6",
          "status": "green",
          "reason": "text_only_ok",
          "response_model": "claude-sonnet-4-6",
          "tested_at": "2026-04-01T11:34:45Z"
        }
      ]
    }
  ]
}`), 0o600); err != nil {
		t.Fatalf("write probe snapshot: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	stats := internalusage.NewRequestStatistics()
	stats.MergeSnapshot(internalusage.StatisticsSnapshot{
		TotalRequests: 1,
		SuccessCount:  1,
		TotalTokens:   42,
		APIs: map[string]internalusage.APISnapshot{
			"claude-test-key": {
				TotalRequests: 1,
				TotalTokens:   42,
				Models: map[string]internalusage.ModelSnapshot{
					"claude-opus-4-6": {
						TotalRequests: 1,
						TotalTokens:   42,
						Details: []internalusage.RequestDetail{
							{
								Timestamp: time.Date(2026, 3, 28, 18, 40, 0, 0, time.UTC),
								Source:    "claude-test-key",
								AuthIndex: "1",
								Tokens: internalusage.TokenStats{
									InputTokens:  21,
									OutputTokens: 21,
									TotalTokens:  42,
								},
								Failed: false,
							},
						},
					},
				},
			},
		},
	})

	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "claude-test-key",
				Name:    "鹤辞ai",
				Prefix:  "鹤辞ai",
				BaseURL: "https://example.com/claude",
				Models: []config.ClaudeModel{
					{Name: "claude-opus-4-6"},
				},
			},
		},
	}

	h := NewHandler(cfg, configPath, nil)
	h.SetUsageStatistics(stats)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-health", nil)

	h.GetProviderHealth(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload providerHealthPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.OverallStatus != "healthy" {
		t.Fatalf("overall_status = %q, want healthy", item.OverallStatus)
	}
	if item.Traffic.Requests != 1 || item.Traffic.Success != 1 || item.Traffic.Tokens != 42 {
		t.Fatalf("traffic = %#v, want requests=1 success=1 tokens=42", item.Traffic)
	}
	if item.Health.Status != "healthy" {
		t.Fatalf("health.status = %q, want healthy", item.Health.Status)
	}
	if len(item.Models) != 1 {
		t.Fatalf("models len = %d, want 1", len(item.Models))
	}
	model := item.Models[0]
	if !model.ResponseModelMismatch {
		t.Fatalf("response_model_mismatch = false, want true")
	}
	if model.ResponseModel != "claude-sonnet-4-6" {
		t.Fatalf("response_model = %q, want claude-sonnet-4-6", model.ResponseModel)
	}
	if model.Status != "healthy" {
		t.Fatalf("model.status = %q, want healthy", model.Status)
	}
	if model.Traffic.Requests != 1 || model.Traffic.Success != 1 || model.Traffic.Tokens != 42 {
		t.Fatalf("model traffic = %#v, want requests=1 success=1 tokens=42", model.Traffic)
	}
	if !strings.Contains(strings.Join(model.Reasons, " | "), "response model mismatch") {
		t.Fatalf("model reasons = %#v, want mismatch reason", model.Reasons)
	}
}

func TestGetProviderHealth_MixedHealthyAndUnavailableProviderStaysHealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filepath.Join(staticDir, "model-health-multi.json"), []byte(`{
  "updated_at": "2026-04-01T15:00:00Z",
  "interval_sec": 300,
  "entries": [
    {
      "updated_at": "2026-04-01T15:00:00Z",
      "provider_prefix": "mixed-codex",
      "base_url": "https://mixed.example/v1",
      "models": [
        {
          "model": "mixed-codex/gpt-5.4",
          "upstream_model": "gpt-5.4",
          "status": "green",
          "reason": "text_only_ok",
          "tested_at": "2026-04-01T15:00:00Z"
        },
        {
          "model": "mixed-codex/gpt-5.3-codex",
          "upstream_model": "gpt-5.3-codex",
          "status": "red",
          "reason": "timeout_or_network_error",
          "tested_at": "2026-04-01T15:00:00Z"
        }
      ]
    }
  ]
}`), 0o600); err != nil {
		t.Fatalf("write probe snapshot: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:             "codex-key",
				Prefix:             "mixed-codex",
				BaseURL:            "https://mixed.example/v1",
				SupportedProtocols: "claude-messages",
				Models: []config.CodexModel{
					{Name: "gpt-5.4"},
					{Name: "gpt-5.3-codex"},
				},
			},
		},
	}

	h := NewHandler(cfg, configPath, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-health", nil)

	h.GetProviderHealth(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload providerHealthPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.OverallStatus != "healthy" {
		t.Fatalf("overall_status = %q, want healthy", item.OverallStatus)
	}
	if item.SupportedProtocols != "claude-messages" {
		t.Fatalf("supported_protocols = %q, want claude-messages", item.SupportedProtocols)
	}
	if item.Healthy != 1 {
		t.Fatalf("healthy = %d, want 1", item.Healthy)
	}
	if item.Unavailable != 1 {
		t.Fatalf("unavailable = %d, want 1", item.Unavailable)
	}
}

func TestGetProviderHealth_NowcodingCapabilityNoteIsOmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filepath.Join(staticDir, "model-health-multi.json"), []byte(`{
  "updated_at": "2026-04-01T22:07:38Z",
  "interval_sec": 300,
  "entries": [
    {
      "updated_at": "2026-04-01T22:07:38Z",
      "provider_prefix": "nowcoding",
      "base_url": "https://nowcoding.ai/v1",
      "supported_protocols": "claude-messages",
      "models": [
        {
          "model": "nowcoding/gpt-5.4",
          "upstream_model": "claude-sonnet-4-6",
          "status": "green",
          "reason": "text_only_ok",
          "response_model": "gpt-5.4",
          "tested_at": "2026-04-01T22:07:38Z"
        }
      ]
    }
  ]
}`), 0o600); err != nil {
		t.Fatalf("write probe snapshot: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:             "nowcoding-key",
				Name:               "nowcoding",
				Prefix:             "nowcoding",
				BaseURL:            "https://nowcoding.ai/v1",
				SupportedProtocols: "claude-messages",
				Models: []config.CodexModel{
					{Name: "claude-sonnet-4-6", Alias: "gpt-5.4"},
				},
			},
		},
	}

	h := NewHandler(cfg, configPath, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-health", nil)

	h.GetProviderHealth(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload providerHealthPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.DisplayName != "nowcoding · gpt-5.4 only" {
		t.Fatalf("display_name = %q, want %q", item.DisplayName, "nowcoding · gpt-5.4 only")
	}
	if item.CapabilityNote != "" {
		t.Fatalf("capability_note = %q, want empty", item.CapabilityNote)
	}
}

func TestAggregateRuntimeSignals_IgnoresEmptySnapshots(t *testing.T) {
	meta := providerHealthMeta{
		Prefix:      "soapapi",
		DisplayName: "soapapi",
		BaseURL:     "https://soapapi.top",
	}

	got := aggregateRuntimeSignals([]cliproxyauth.AuthRuntimeSnapshot{
		{
			ID:       "soapapi-1",
			Provider: "openai-compat",
			Prefix:   "soapapi",
			Label:    "soapapi",
			BaseURL:  "https://soapapi.top",
		},
	}, meta)

	if got.Found {
		t.Fatal("runtime signal should ignore empty snapshots")
	}
}

func TestNormalizeProviderModelDisplayStatus_IsBinary(t *testing.T) {
	cases := map[string]string{
		"healthy":     "healthy",
		"degraded":    "healthy",
		"unavailable": "unavailable",
		"unknown":     "unavailable",
	}

	for input, want := range cases {
		if got := normalizeProviderModelDisplayStatus(input); got != want {
			t.Fatalf("normalizeProviderModelDisplayStatus(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestProviderModelDisplayStatus_RedProbeOverridesRuntimeDerivedHealthy(t *testing.T) {
	if got := providerModelDisplayStatus("degraded", "red"); got != "unavailable" {
		t.Fatalf("providerModelDisplayStatus(%q, %q) = %q, want unavailable", "degraded", "red", got)
	}
	if got := providerModelDisplayStatus("healthy", "green"); got != "healthy" {
		t.Fatalf("providerModelDisplayStatus(%q, %q) = %q, want healthy", "healthy", "green", got)
	}
}

func TestBuildProviderHealthModels_RedProbeStaysUnavailableWithHealthyRuntime(t *testing.T) {
	now := time.Date(2026, 4, 10, 15, 56, 0, 0, time.UTC)

	models := buildProviderHealthModels(
		&health.ProbeEntry{
			UpdatedAt:      now.Format(time.RFC3339),
			ProviderPrefix: "鹤辞ai",
			BaseURL:        "https://example.com/claude",
			Models: []health.ProbeModel{
				{
					Model:         "鹤辞ai/claude-sonnet-4-6",
					UpstreamModel: "claude-sonnet-4-6",
					Status:        "red",
					Reason:        "Invalid API Key",
					TestedAt:      now.Format(time.RFC3339),
				},
			},
		},
		health.RuntimeSignal{
			SuccessCount: 12,
			Found:        true,
		},
		health.BalanceSignal{
			Status:    "ok",
			UpdatedAt: now,
			Found:     true,
		},
		nil,
		now,
	)

	if len(models) != 1 {
		t.Fatalf("models len = %d, want 1", len(models))
	}
	if models[0].Status != "unavailable" {
		t.Fatalf("model.status = %q, want unavailable", models[0].Status)
	}
	if models[0].Health.Status != "unavailable" {
		t.Fatalf("model.health.status = %q, want unavailable", models[0].Health.Status)
	}
	if models[0].Error != "Invalid API Key" {
		t.Fatalf("model.error = %q, want Invalid API Key", models[0].Error)
	}
}

func TestNormalizeProviderOverallDisplayStatus_IsBinary(t *testing.T) {
	cases := map[string]string{
		"healthy":     "healthy",
		"degraded":    "healthy",
		"unavailable": "unavailable",
		"unknown":     "unavailable",
	}

	for input, want := range cases {
		if got := normalizeProviderOverallDisplayStatus(input); got != want {
			t.Fatalf("normalizeProviderOverallDisplayStatus(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGetProviderHealth_PersistsSharedHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filepath.Join(staticDir, "model-health-multi.json"), []byte(`{
  "updated_at": "2026-03-28T12:00:00Z",
  "interval_sec": 300,
  "entries": [
    {
      "updated_at": "2026-03-28T12:00:00Z",
      "provider_prefix": "yunyi-codex",
      "base_url": "https://example.com/v1",
      "models": [
        {
          "model": "gpt-5.4",
          "status": "green",
          "reason": "ok",
          "tested_at": "2026-03-28T12:00:00Z"
        }
      ]
    }
  ]
}`), 0o600); err != nil {
		t.Fatalf("write probe snapshot: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "codex-key",
				Prefix:  "yunyi-codex",
				BaseURL: "https://example.com/v1",
				Models: []config.CodexModel{
					{Name: "gpt-5.4"},
				},
			},
		},
	}

	h := NewHandler(cfg, configPath, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-health", nil)

	h.GetProviderHealth(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload providerHealthPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	key := "yunyi-codex/gpt-5.4"
	history := payload.History[key]
	if len(history) != 1 {
		t.Fatalf("history len = %d, want 1", len(history))
	}
	if history[0].Status != "healthy" {
		t.Fatalf("history status = %q, want healthy", history[0].Status)
	}
	if history[0].At != "2026-03-28T12:00:00Z" {
		t.Fatalf("history at = %q, want tested_at", history[0].At)
	}

	raw, err := os.ReadFile(filepath.Join(staticDir, providerHealthHistoryFileName))
	if err != nil {
		t.Fatalf("read persisted history: %v", err)
	}
	if !strings.Contains(string(raw), key) {
		t.Fatalf("persisted history missing key %q: %s", key, string(raw))
	}
}

func TestGetProviderHealth_DisabledProviderIsShownAsUnknownNotHealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filepath.Join(staticDir, "model-health-multi.json"), []byte(`{
  "updated_at": "2026-04-02T21:20:07Z",
  "interval_sec": 3600,
  "entries": [
    {
      "updated_at": "2026-04-02T21:20:07Z",
      "provider_prefix": "whitedream-codex",
      "base_url": "https://llm.whitedream.top",
      "source_mode": "config_yaml",
      "disabled": true,
      "error": "provider_disabled",
      "models": []
    }
  ]
}`), 0o600); err != nil {
		t.Fatalf("write probe snapshot: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:         "sk-disabled",
				Name:           "whitedream-codex",
				Prefix:         "whitedream-codex",
				BaseURL:        "https://llm.whitedream.top",
				ExcludedModels: []string{"*"},
				Models: []config.CodexModel{
					{Name: "[不要钱]gpt-5.4", Alias: "gpt-5.4"},
				},
			},
		},
	}

	h := NewHandler(cfg, configPath, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-health", nil)

	h.GetProviderHealth(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload providerHealthPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}
	item := payload.Items[0]
	if !item.Disabled {
		t.Fatalf("disabled = false, want true")
	}
	if item.OverallStatus != "unknown" {
		t.Fatalf("overall_status = %q, want unknown", item.OverallStatus)
	}
	if item.Total != 0 || item.Healthy != 0 || item.Unavailable != 0 || item.Unknown != 0 {
		t.Fatalf("counts = total:%d healthy:%d unavailable:%d unknown:%d, want all zero", item.Total, item.Healthy, item.Unavailable, item.Unknown)
	}
	if len(item.Reasons) == 0 || item.Reasons[0] != "provider_disabled" {
		t.Fatalf("reasons = %#v, want provider_disabled", item.Reasons)
	}
}
