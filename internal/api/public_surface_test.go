package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestCPAMCPublicModels_StripsPrefixesAndDedupes(t *testing.T) {
	server := newTestServer(t)
	reg := registry.GetGlobalRegistry()

	reg.RegisterClient("test-public-codex-models", "codex", []*registry.ModelInfo{
		{ID: "gpt-5.4"},
		{ID: "sub2api/gpt-5.4"},
		{ID: "openai/gpt-5.4"},
		{ID: "gpt-5.4-pro"},
	})
	reg.RegisterClient("test-public-claude-models", "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-6"},
		{ID: "yunyi-claude/claude-sonnet-4-6"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("test-public-codex-models")
		reg.UnregisterClient("test-public-claude-models")
	})

	req := httptest.NewRequest(http.MethodGet, "/cpamc/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()

	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /cpamc/models status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var payload struct {
		Data []cpamcPublicModel `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal /cpamc/models response: %v", err)
	}

	got := make(map[string]bool)
	for _, item := range payload.Data {
		got[item.Channel+":"+item.ID] = true
		if strings.Contains(item.ID, "/") {
			t.Fatalf("public model id should not contain provider prefix: %q", item.ID)
		}
	}

	for _, want := range []string{
		"codex:gpt-5.4",
		"codex:gpt-5.4-pro",
		"claude:claude-sonnet-4-6",
	} {
		if !got[want] {
			t.Fatalf("missing public model %s in %v", want, got)
		}
	}
}

func TestCPAMCPublicHealth_UsesProjectedStatuses(t *testing.T) {
	server := newTestServer(t)
	reg := registry.GetGlobalRegistry()

	reg.RegisterClient("test-public-health-codex", "codex", []*registry.ModelInfo{
		{ID: "gpt-5.4"},
		{ID: "gpt-5.4-pro"},
	})
	reg.RegisterClient("test-public-health-claude", "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-6"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("test-public-health-codex")
		reg.UnregisterClient("test-public-health-claude")
	})

	writeStaticSnapshot(t, server.configFilePath, "model-health-multi.json", `{
  "updated_at": "2026-03-11T12:00:00Z",
  "interval_sec": 86400,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 2, "green": 1, "yellow": 0, "red": 1},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-11T12:00:00Z"},
        {"model": "sub2api/gpt-5.4-pro", "status": "red", "reason": "timeout_or_network_error", "tested_at": "2026-03-11T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-claude",
      "summary": {"total": 1, "green": 0, "yellow": 1, "red": 0},
      "models": [
        {"model": "yunyi-claude/claude-sonnet-4-6", "status": "yellow", "reason": "rate_limited", "tested_at": "2026-03-11T12:00:00Z"}
      ]
    }
  ]
}`)

	req := httptest.NewRequest(http.MethodGet, "/cpamc/health", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()

	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /cpamc/health status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "provider_prefix") || strings.Contains(rr.Body.String(), "base_url") {
		t.Fatalf("public health response leaked upstream fields: %s", rr.Body.String())
	}

	var payload struct {
		Data []cpamcPublicHealth `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal /cpamc/health response: %v", err)
	}

	got := make(map[string]string)
	scores := make(map[string]float64)
	protocols := make(map[string][]string)
	candidateCounts := make(map[string]int)
	continuations := make(map[string]string)
	testedAt := make(map[string]string)
	for _, item := range payload.Data {
		got[item.Channel+":"+item.ID] = item.Status
		scores[item.Channel+":"+item.ID] = item.Score
		protocols[item.Channel+":"+item.ID] = item.Protocols
		candidateCounts[item.Channel+":"+item.ID] = item.CandidateCount
		continuations[item.Channel+":"+item.ID] = item.Continuation
		testedAt[item.Channel+":"+item.ID] = item.TestedAt
	}

	if got["codex:gpt-5.4"] != "available" {
		t.Fatalf("gpt-5.4 status = %q, want %q", got["codex:gpt-5.4"], "available")
	}
	if got["codex:gpt-5.4-pro"] != "unavailable" {
		t.Fatalf("gpt-5.4-pro status = %q, want %q", got["codex:gpt-5.4-pro"], "unavailable")
	}
	if got["claude:claude-sonnet-4-6"] != "degraded" {
		t.Fatalf("claude-sonnet-4-6 status = %q, want %q", got["claude:claude-sonnet-4-6"], "degraded")
	}
	if scores["codex:gpt-5.4"] <= 0 {
		t.Fatalf("gpt-5.4 score = %v, want > 0", scores["codex:gpt-5.4"])
	}
	if len(protocols["codex:gpt-5.4"]) == 0 {
		t.Fatal("expected codex protocols in public health response")
	}
	if candidateCounts["codex:gpt-5.4"] != 1 {
		t.Fatalf("gpt-5.4 candidate_count = %d, want 1", candidateCounts["codex:gpt-5.4"])
	}
	if continuations["codex:gpt-5.4"] != "supported" {
		t.Fatalf("gpt-5.4 continuation = %q, want %q", continuations["codex:gpt-5.4"], "supported")
	}
	if testedAt["claude:claude-sonnet-4-6"] != "2026-03-11T12:00:00Z" {
		t.Fatalf("claude-sonnet-4-6 tested_at = %q, want %q", testedAt["claude:claude-sonnet-4-6"], "2026-03-11T12:00:00Z")
	}
}

func TestRawModelHealthRequiresManagementKey(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "mgmt-key")
	server := newTestServer(t)

	writeStaticSnapshot(t, server.configFilePath, "model-health.json", `{"updated_at":"2026-03-11T12:00:00Z","entries":[]}`)

	req := httptest.NewRequest(http.MethodGet, "/model-health.json", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET /model-health.json without key status = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/model-health.json?management_key=mgmt-key", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /model-health.json with key status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

func TestProviderBalancesRouteRequiresManagementKeyAndReturnsPayload(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "mgmt-key")
	server := newTestServer(t)
	server.cfg.GeminiKey = []config.GeminiKey{
		{APIKey: "gemini-key", Name: "Gemini Main"},
	}

	req := httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v0/management/provider-balances without key status = %d, want 401", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances?management_key=mgmt-key", nil)
	req.RemoteAddr = "203.0.113.10:54321"
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v0/management/provider-balances with key status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var payload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal provider balances response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}
}

func TestV1ModelsRouteEnabledForAuthenticatedTraffic(t *testing.T) {
	server := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.RemoteAddr = "203.0.113.10:54321"
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("public GET /v1/models status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.RemoteAddr = "127.0.0.1:54321"
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("internal GET /v1/models status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/cpams/codex/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	rr = httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("POST /cpams/codex/responses status = %d, want 404", rr.Code)
	}
}

func writeStaticSnapshot(t *testing.T, configFilePath, name, body string) {
	t.Helper()
	staticDir := managementasset.StaticDir(configFilePath)
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		t.Fatalf("mkdir static dir: %v", err)
	}
	path := filepath.Join(staticDir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
