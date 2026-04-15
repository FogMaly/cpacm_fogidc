package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gin "github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/covsactivation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

func newTestServer(t *testing.T, cfgs ...*proxyconfig.Config) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}
	if len(cfgs) > 0 && cfgs[0] != nil {
		cfg = cfgs[0]
		if cfg.AuthDir == "" {
			cfg.AuthDir = authDir
		}
		if cfg.Port == 0 {
			cfg.Port = 0
		}
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()

	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath)
}

func TestAmpProviderModelRoutes(t *testing.T) {
	testCases := []struct {
		name         string
		path         string
		wantStatus   int
		wantContains string
	}{
		{
			name:         "openai root models",
			path:         "/api/provider/openai/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "groq root models",
			path:         "/api/provider/groq/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "openai models",
			path:         "/api/provider/openai/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "anthropic models",
			path:         "/api/provider/anthropic/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"data"`,
		},
		{
			name:         "google models v1",
			path:         "/api/provider/google/v1/models",
			wantStatus:   http.StatusOK,
			wantContains: `"models"`,
		},
		{
			name:         "google models v1beta",
			path:         "/api/provider/google/v1beta/models",
			wantStatus:   http.StatusOK,
			wantContains: `"models"`,
		},
		{
			name:         "cpamc codex models",
			path:         "/cpamc/codex/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "cpamc claude models",
			path:         "/cpamc/claude/models",
			wantStatus:   http.StatusOK,
			wantContains: `"object":"list"`,
		},
		{
			name:         "cpamc codex health",
			path:         "/cpamc/codex/health",
			wantStatus:   http.StatusServiceUnavailable,
			wantContains: `"error":"public_health_unavailable"`,
		},
		{
			name:         "cpamc claude health",
			path:         "/cpamc/claude/health",
			wantStatus:   http.StatusServiceUnavailable,
			wantContains: `"error":"public_health_unavailable"`,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t)

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer test-key")

			rr := httptest.NewRecorder()
			server.engine.ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Fatalf("unexpected status code for %s: got %d want %d; body=%s", tc.path, rr.Code, tc.wantStatus, rr.Body.String())
			}
			if body := rr.Body.String(); !strings.Contains(body, tc.wantContains) {
				t.Fatalf("response body for %s missing %q: %s", tc.path, tc.wantContains, body)
			}
		})
	}
}

func TestCPAMCClaudeV1CompatibilityRoutesExist(t *testing.T) {
	server := newTestServer(t)

	testCases := []string{
		"/cpamc/claude/v1/messages",
		"/cpamc/claude/v1/messages/count_tokens",
	}

	for _, path := range testCases {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"claude-opus-4-6"}`))
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)

		if rr.Code == http.StatusNotFound {
			t.Fatalf("route %s returned 404, want registered compatibility route", path)
		}
	}
}

func TestUnifiedModelsHandler_ClaudeUserAgentIncludesPrefixedClaudeModels(t *testing.T) {
	server := newTestServer(t)
	reg := registry.GetGlobalRegistry()

	reg.RegisterClient("test-unified-models-claude-prefixed", "claude", []*registry.ModelInfo{
		{ID: "claude-opus-4-6"},
		{ID: "covs/claude-opus-4-6"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient("test-unified-models-claude-prefixed")
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"id":"covs/claude-opus-4-6"`) {
		t.Fatalf("GET /v1/models body missing prefixed claude model: %s", rr.Body.String())
	}
}

func TestUnifiedChatCompletionsHandler_RoutesNativeClaudeIngressToClaudeHandler(t *testing.T) {
	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-09T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "covs",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "covs/claude-opus-4-6", "status": "green", "reason": "ok", "tested_at": "2026-04-09T12:00:00Z"}
      ]
    }
  ]
}`)
	server := &Server{
		cfg: &proxyconfig.Config{Routing: proxyconfig.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: &cpamsResolver{
			snapshotPath:  snapshotPath,
			tokenTTL:      15 * time.Minute,
			channelTokens: make(map[string]map[string]time.Time),
			sticky:        make(map[string]cpamsSelection),
			selected:      make(map[string]cpamsSelection),
			now:           func() time.Time { return now },
		},
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/chat/completions", server.unifiedChatCompletionsHandler(
		func(c *gin.Context) {
			t.Fatal("openai handler should not run for native Claude ingress")
		},
		func(c *gin.Context) {
			raw, err := c.GetRawData()
			if err != nil {
				t.Fatalf("GetRawData() error = %v", err)
			}
			c.JSON(http.StatusOK, gin.H{
				"handler":       "claude",
				"model":         gjson.GetBytes(raw, "model").String(),
				"cpams_channel": c.GetString("cpams_channel"),
			})
		},
	))

	body := `{"model":"claude/claude-opus-4-6","max_tokens":16,"messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-App", "cli")

	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	if got := gjson.Get(rr.Body.String(), "handler").String(); got != "claude" {
		t.Fatalf("handler = %q, want claude", got)
	}
	if got := gjson.Get(rr.Body.String(), "model").String(); got != "covs/claude-opus-4-6(xhigh)" {
		t.Fatalf("model = %q, want covs/claude-opus-4-6(xhigh)", got)
	}
	if got := gjson.Get(rr.Body.String(), "cpams_channel").String(); got != "claude" {
		t.Fatalf("cpams_channel = %q, want claude", got)
	}
}

func TestUnifiedChatCompletionsHandler_RoutesOpenAICompatToOpenAIHandler(t *testing.T) {
	server := &Server{}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/chat/completions", server.unifiedChatCompletionsHandler(
		func(c *gin.Context) {
			raw, err := c.GetRawData()
			if err != nil {
				t.Fatalf("GetRawData() error = %v", err)
			}
			c.JSON(http.StatusOK, gin.H{
				"handler": "openai",
				"model":   gjson.GetBytes(raw, "model").String(),
			})
		},
		func(c *gin.Context) {
			t.Fatal("claude handler should not run for standard openai-compatible request")
		},
	))

	body := `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	if got := gjson.Get(rr.Body.String(), "handler").String(); got != "openai" {
		t.Fatalf("handler = %q, want openai", got)
	}
}

func TestCOVSActivationRequestMiddleware_RewritesNativeClaudeChatCompletionsPrefix(t *testing.T) {
	server := &Server{
		covsActivation: covsactivation.NewService(&proxyconfig.Config{
			COVSActivation: proxyconfig.COVSActivationConfig{Enable: true},
		}, filepath.Join(t.TempDir(), "config.yaml")),
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("accessProvider", covsactivation.AccessProviderName)
		c.Set("accessMetadata", map[string]string{
			"product":      "claude",
			"model_prefix": "covs",
		})
		c.Next()
	})
	engine.Use(server.covsActivationRequestMiddleware())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		c.JSON(http.StatusOK, payload)
	})

	body := `{"model":"claude-opus-4-6","max_tokens":16,"messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-App", "cli")

	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", rr.Code, rr.Body.String())
	}
	if got := gjson.Get(rr.Body.String(), "model").String(); got != "covs/claude-opus-4-6" {
		t.Fatalf("model = %q, want covs/claude-opus-4-6", got)
	}
}
