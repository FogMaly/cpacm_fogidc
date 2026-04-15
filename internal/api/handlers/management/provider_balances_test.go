package management

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestGetProviderBalances_OpenRouterAndUnsupported(t *testing.T) {
	gin.SetMode(gin.TestMode)

	openRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/credits" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openrouter-key" {
			t.Fatalf("authorization = %q, want Bearer openrouter-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"total_credits": 100.0,
				"total_usage":   24.5,
			},
		})
	}))
	t.Cleanup(openRouter.Close)

	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "gemini-key", Name: "Gemini Main"},
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "openrouter",
				BaseURL: openRouter.URL + "/api/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "openrouter-key"},
				},
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(payload.Items))
	}

	got := make(map[string]providerBalanceItem, len(payload.Items))
	for _, item := range payload.Items {
		got[item.Section+"#"+strconvItoa(item.Index)] = item
	}

	openrouterItem, ok := got["openai-compatibility#0"]
	if !ok {
		t.Fatalf("missing openai-compatibility#0 item")
	}
	if openrouterItem.Status != "ok" {
		t.Fatalf("openrouter status = %q, want ok", openrouterItem.Status)
	}
	if !strings.Contains(openrouterItem.Label, "75.50") {
		t.Fatalf("openrouter label = %q, want remaining amount", openrouterItem.Label)
	}

	geminiItem, ok := got["gemini-api-key#0"]
	if !ok {
		t.Fatalf("missing gemini-api-key#0 item")
	}
	if geminiItem.Status != "unsupported" {
		t.Fatalf("gemini status = %q, want unsupported", geminiItem.Status)
	}
	if !strings.Contains(geminiItem.Label, "未开放") {
		t.Fatalf("gemini label = %q, want unsupported copy", geminiItem.Label)
	}
}

func TestGetProviderBalances_AnthropicAdminCost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/organizations/cost_report" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant-admin-test" {
			t.Fatalf("x-api-key = %q, want sk-ant-admin-test", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"results": []map[string]any{
						{"amount": "1234", "currency": "USD"},
						{"amount": "66", "currency": "USD"},
					},
				},
			},
		})
	}))
	t.Cleanup(anthropic.Close)

	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "sk-ant-admin-test",
				Name:    "Anthropic Admin",
				BaseURL: anthropic.URL,
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}
	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.Kind != "usage_30d" {
		t.Fatalf("kind = %q, want usage_30d", item.Kind)
	}
	if !strings.Contains(item.Label, "13.00") {
		t.Fatalf("label = %q, want 13.00", item.Label)
	}
}

func TestGetProviderBalances_ClaudeObservedUsageFallbackUsesLocalUsageForUnsupportedChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.ConfigureRuntime(usage.RuntimeConfig{})

	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "sk-8AvZtrmRyoctoCQUNhJCzpzeDPQbrFG7QWDtTOt7g9902LET",
				Name:    "xn--ai-oz3gh81c.xn--253ax40a.cn",
				Prefix:  "鹤辞ai",
				BaseURL: "https://xn--ai-oz3gh81c.xn--253ax40a.cn",
			},
			{
				APIKey:  "sk-3MDR8CeS6r19qQgeWhwfi9hDeEz7f77IjXhPt2r82PijEq0y",
				Name:    "covs-reseller",
				Prefix:  "covs",
				BaseURL: "https://rsxermu666.cn",
				Headers: map[string]string{
					"x-api-key": "sk-3MDR8CeS6r19qQgeWhwfi9hDeEz7f77IjXhPt2r82PijEq0y",
				},
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	h.usageStats = usage.NewRequestStatistics()

	now := time.Now().UTC()
	h.usageStats.Record(context.Background(), coreusage.Record{
		Provider:    "claude",
		Model:       "claude-opus-4-6",
		APIKey:      "local-session",
		Source:      "sk-8AvZtrmRyoctoCQUNhJCzpzeDPQbrFG7QWDtTOt7g9902LET",
		RequestedAt: now.Add(-10 * time.Minute),
		Detail: coreusage.Detail{
			InputTokens:  1000000,
			OutputTokens: 1000000,
			TotalTokens:  2000000,
		},
	})
	h.usageStats.Record(context.Background(), coreusage.Record{
		Provider:    "claude",
		Model:       "claude-sonnet-4-6",
		APIKey:      "local-session",
		Source:      "sk-3MDR8CeS6r19qQgeWhwfi9hDeEz7f77IjXhPt2r82PijEq0y",
		RequestedAt: now.Add(-5 * time.Minute),
		Detail: coreusage.Detail{
			InputTokens:  1000000,
			OutputTokens: 1000000,
			TotalTokens:  2000000,
		},
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(payload.Items))
	}

	got := make(map[string]providerBalanceItem, len(payload.Items))
	for _, item := range payload.Items {
		got[item.Prefix] = item
	}

	for _, prefix := range []string{"鹤辞ai", "covs"} {
		item, ok := got[prefix]
		if !ok {
			t.Fatalf("missing item for prefix %q", prefix)
		}
		if item.Status != "ok" {
			t.Fatalf("%s status = %q, want ok", prefix, item.Status)
		}
		if item.Kind != "estimated_observed_usage" {
			t.Fatalf("%s kind = %q, want estimated_observed_usage", prefix, item.Kind)
		}
		if item.Currency != "" {
			t.Fatalf("%s currency = %q, want empty currency", prefix, item.Currency)
		}
		if !strings.Contains(item.Label, "今日已用 2000000 Tokens") {
			t.Fatalf("%s label = %q, want token observed usage label", prefix, item.Label)
		}
		if !strings.Contains(item.Detail, "Claude 非官方渠道不按内置美元单价估算") {
			t.Fatalf("%s detail = %q, want claude pricing guardrail", prefix, item.Detail)
		}
		if !strings.Contains(item.Detail, "中国时间 00:00 至今") {
			t.Fatalf("%s detail = %q, want china-day window", prefix, item.Detail)
		}
	}
}

func TestGetProviderBalances_CodexObservedUsageFallbackUsesUSDForOpenAIModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.ConfigureRuntime(usage.RuntimeConfig{})

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-openai-fallback-test",
				Name:    "generic-openai",
				Prefix:  "generic-openai",
				BaseURL: "https://example.com/v1",
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	h.usageStats = usage.NewRequestStatistics()

	now := time.Now().UTC()
	h.usageStats.Record(context.Background(), coreusage.Record{
		Provider:    "codex",
		Model:       "gpt-5.3-codex",
		APIKey:      "local-session",
		Source:      "sk-openai-fallback-test",
		RequestedAt: now.Add(-3 * time.Minute),
		Detail: coreusage.Detail{
			InputTokens:  1000000,
			OutputTokens: 1000000,
			TotalTokens:  2000000,
		},
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.Kind != "estimated_observed_usage" {
		t.Fatalf("kind = %q, want estimated_observed_usage", item.Kind)
	}
	if item.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", item.Currency)
	}
	if math.Abs(item.Amount-15.75) > 0.000001 {
		t.Fatalf("amount = %v, want 15.75", item.Amount)
	}
	if !strings.Contains(item.Label, "今日已用 $15.75") {
		t.Fatalf("label = %q, want usd label", item.Label)
	}
	if !strings.Contains(item.Detail, "非准确估计值") {
		t.Fatalf("detail = %q, want estimate marker", item.Detail)
	}
}

func TestGetProviderBalances_YunyiQuotaForCodexAndClaude(t *testing.T) {
	gin.SetMode(gin.TestMode)

	yunyi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/api/v1/me" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer yunyi-key-codex" && auth != "Bearer yunyi-key-claude" {
			t.Fatalf("authorization = %q, want yunyi bearer token", auth)
		}
		if got := r.Header.Get("User-Agent"); got != "cc-switch/1.0" {
			t.Fatalf("user-agent = %q, want cc-switch/1.0", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "active",
			"quota": map[string]any{
				"daily_quota":     3072,
				"daily_spent":     512,
				"next_reset_at":   time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339),
				"daily_remaining": 2560,
			},
		})
	}))
	t.Cleanup(yunyi.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "yunyi-key-codex",
				Name:    "yunyi-codex",
				BaseURL: yunyi.URL + "/codex/v1",
			},
		},
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "yunyi-key-claude",
				Name:    "yunyi-claude",
				BaseURL: yunyi.URL + "/claude",
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 2 {
		t.Fatalf("items len = %d, want 2", len(payload.Items))
	}

	got := make(map[string]providerBalanceItem, len(payload.Items))
	for _, item := range payload.Items {
		got[item.Section+"#"+strconvItoa(item.Index)] = item
	}

	for _, key := range []string{"codex-api-key#0", "claude-api-key#0"} {
		item, ok := got[key]
		if !ok {
			t.Fatalf("missing %s item", key)
		}
		if item.Status != "ok" {
			t.Fatalf("%s status = %q, want ok", key, item.Status)
		}
		if item.Kind != "remaining_balance" {
			t.Fatalf("%s kind = %q, want remaining_balance", key, item.Kind)
		}
		if item.Currency != "USD" {
			t.Fatalf("%s currency = %q, want USD", key, item.Currency)
		}
		if math.Abs(item.Amount-25.6) > 0.000001 {
			t.Fatalf("%s amount = %v, want 25.6", key, item.Amount)
		}
		if math.Abs(item.Total-30.72) > 0.000001 {
			t.Fatalf("%s total = %v, want 30.72", key, item.Total)
		}
		if math.Abs(item.Used-5.12) > 0.000001 {
			t.Fatalf("%s used = %v, want 5.12", key, item.Used)
		}
		if !strings.Contains(item.Detail, "已用 $5.12") {
			t.Fatalf("%s detail = %q, want usage summary", key, item.Detail)
		}
		if !strings.Contains(item.Detail, "后重置") {
			t.Fatalf("%s detail = %q, want reset countdown", key, item.Detail)
		}
	}
}

func TestGetProviderBalances_Sub2APIUsageForCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sub2api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sub2api-key" {
			t.Fatalf("authorization = %q, want Bearer sub2api-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"isValid": true,
			"mode":    "quota_limited",
			"status":  "active",
			"unit":    "USD",
			"quota": map[string]any{
				"limit":     91.0,
				"remaining": 91.0,
				"used":      0.0,
				"unit":      "USD",
			},
			"remaining": 91.0,
		})
	}))
	t.Cleanup(sub2api.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sub2api-key",
				Name:    "sub2api-openai",
				BaseURL: sub2api.URL,
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.Kind != "remaining_balance" {
		t.Fatalf("kind = %q, want remaining_balance", item.Kind)
	}
	if item.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", item.Currency)
	}
	if math.Abs(item.Amount-91.0) > 0.000001 {
		t.Fatalf("amount = %v, want 91", item.Amount)
	}
	if math.Abs(item.Total-91.0) > 0.000001 {
		t.Fatalf("total = %v, want 91", item.Total)
	}
	if math.Abs(item.Used-0.0) > 0.000001 {
		t.Fatalf("used = %v, want 0", item.Used)
	}
	if !strings.Contains(item.Detail, "总额 $91.00") {
		t.Fatalf("detail = %q, want total summary", item.Detail)
	}
}

func TestGetProviderBalances_SoapAPIUsageForCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	soapapi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/public/key-usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer soapapi-key" {
			t.Fatalf("authorization = %q, want Bearer soapapi-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"isValid": true,
			"mode":    "quota_limited",
			"status":  "active",
			"rate_limits": []map[string]any{
				{
					"limit":     100.0,
					"remaining": 55.47,
					"used":      44.53,
					"window":    "1d",
				},
			},
			"usage": map[string]any{
				"today": map[string]any{
					"actual_cost": 44.53,
				},
			},
		})
	}))
	t.Cleanup(soapapi.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "soapapi-key",
				Name:    "soapapi",
				BaseURL: soapapi.URL,
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.Kind != "remaining_balance" {
		t.Fatalf("kind = %q, want remaining_balance", item.Kind)
	}
	if math.Abs(item.Amount-55.47) > 0.000001 {
		t.Fatalf("amount = %v, want 55.47", item.Amount)
	}
	if math.Abs(item.Used-44.53) > 0.000001 {
		t.Fatalf("used = %v, want 44.53", item.Used)
	}
	if math.Abs(item.Total-100.0) > 0.000001 {
		t.Fatalf("total = %v, want 100", item.Total)
	}
	if !strings.Contains(item.Label, "剩余额度 $55.47") {
		t.Fatalf("label = %q, want remaining quota label", item.Label)
	}
}

func TestGetProviderBalances_AIXJDailyUsageForCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aixj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-model-key" {
			t.Fatalf("authorization = %q, want Bearer sk-model-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"isValid":   true,
			"planName":  "Codex100刀订阅专用",
			"remaining": 87.66,
			"subscription": map[string]any{
				"daily_limit_usd": 100.0,
				"daily_usage_usd": 12.34,
			},
			"usage": map[string]any{
				"today": map[string]any{
					"actual_cost": 12.34,
					"requests":    18,
				},
			},
		})
	}))
	t.Cleanup(aixj.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-model-key",
				Name:    "aixj",
				BaseURL: aixj.URL + "/v1",
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.Kind != "remaining_balance" {
		t.Fatalf("kind = %q, want remaining_balance", item.Kind)
	}
	if item.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", item.Currency)
	}
	if math.Abs(item.Amount-87.66) > 0.000001 {
		t.Fatalf("amount = %v, want 87.66", item.Amount)
	}
	if math.Abs(item.Total-100.0) > 0.000001 {
		t.Fatalf("total = %v, want 100", item.Total)
	}
	if math.Abs(item.Used-12.34) > 0.000001 {
		t.Fatalf("used = %v, want 12.34", item.Used)
	}
	if !strings.Contains(item.Detail, "今日已用 $12.34 / 日总 $100.00") {
		t.Fatalf("detail = %q, want daily usage summary", item.Detail)
	}
	if !strings.Contains(item.Detail, "后重置") {
		t.Fatalf("detail = %q, want reset countdown", item.Detail)
	}
}

func TestGetProviderBalances_AIXJFallsBackToSubscriptionDailyUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aixj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"remaining": 99.5,
			"subscription": map[string]any{
				"daily_limit_usd": 100.0,
				"daily_usage_usd": 0.5,
			},
		})
	}))
	t.Cleanup(aixj.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-model-key",
				Name:    "aixj",
				BaseURL: aixj.URL + "/v1",
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if math.Abs(item.Amount-99.5) > 0.000001 {
		t.Fatalf("amount = %v, want 99.5", item.Amount)
	}
	if math.Abs(item.Used-0.5) > 0.000001 {
		t.Fatalf("used = %v, want 0.5", item.Used)
	}
	if !strings.Contains(item.Detail, "今日已用 $0.50 / 日总 $100.00") {
		t.Fatalf("detail = %q, want daily usage summary", item.Detail)
	}
	if !strings.Contains(item.Detail, "后重置") {
		t.Fatalf("detail = %q, want reset countdown", item.Detail)
	}
}

func TestGetProviderBalances_AIXJUsesAPIRemainingWhenQuotaExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aixj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"remaining": 0.0,
			"subscription": map[string]any{
				"daily_limit_usd": 100.0,
				"daily_usage_usd": 12.34,
			},
			"usage": map[string]any{
				"today": map[string]any{
					"actual_cost": 12.34,
				},
			},
		})
	}))
	t.Cleanup(aixj.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-model-key",
				Name:    "aixj",
				BaseURL: aixj.URL + "/v1",
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if math.Abs(item.Amount-0.0) > 0.000001 {
		t.Fatalf("amount = %v, want 0", item.Amount)
	}
	if math.Abs(item.Used-12.34) > 0.000001 {
		t.Fatalf("used = %v, want 12.34", item.Used)
	}
	if !strings.Contains(item.Label, "当日剩余额度 $0.00") {
		t.Fatalf("label = %q, want remaining zero label", item.Label)
	}
}

func TestGetProviderBalances_AIXJ429QuotaExceededBecomesWarningBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aixj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    "USAGE_LIMIT_EXCEEDED",
			"message": `error: code=429 reason="DAILY_LIMIT_EXCEEDED" message="daily usage limit exceeded" metadata=map[]`,
		})
	}))
	t.Cleanup(aixj.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-model-key",
				Name:    "aixj",
				BaseURL: aixj.URL + "/v1",
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "warning" {
		t.Fatalf("status = %q, want warning", item.Status)
	}
	if item.Kind != "remaining_balance" {
		t.Fatalf("kind = %q, want remaining_balance", item.Kind)
	}
	if math.Abs(item.Amount-0.0) > 0.000001 {
		t.Fatalf("amount = %v, want 0", item.Amount)
	}
	if !strings.Contains(item.Label, "当日剩余额度 $0.00") {
		t.Fatalf("label = %q, want zero remaining label", item.Label)
	}
	if !strings.Contains(item.Detail, "DAILY_LIMIT_EXCEEDED") {
		t.Fatalf("detail = %q, want quota exceeded detail", item.Detail)
	}
}

func TestGetProviderBalances_GMNCodeDailyUsageForCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	gmncode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-gmncode-key" {
			t.Fatalf("authorization = %q, want Bearer sk-gmncode-key", got)
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
					"actual_cost": 0.127228,
					"cost":        0.127228,
					"requests":    36,
				},
			},
		})
	}))
	t.Cleanup(gmncode.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-gmncode-key",
				Name:    "gmncode",
				BaseURL: gmncode.URL + "/v1",
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.DisplayName != "gmncode" {
		t.Fatalf("display_name = %q, want gmncode", item.DisplayName)
	}
	if item.Kind != "remaining_balance" {
		t.Fatalf("kind = %q, want remaining_balance", item.Kind)
	}
	if item.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", item.Currency)
	}
	if math.Abs(item.Amount-89.872772) > 0.000001 {
		t.Fatalf("amount = %v, want 89.872772", item.Amount)
	}
	if math.Abs(item.Total-90.0) > 0.000001 {
		t.Fatalf("total = %v, want 90", item.Total)
	}
	if math.Abs(item.Used-0.127228) > 0.000001 {
		t.Fatalf("used = %v, want 0.127228", item.Used)
	}
	if !strings.Contains(item.Label, "89.87") {
		t.Fatalf("label = %q, want fixed daily remaining", item.Label)
	}
	if !strings.Contains(item.Detail, "今日已用 $0.13 / 日总 $90.00") {
		t.Fatalf("detail = %q, want daily usage summary", item.Detail)
	}
	if !strings.Contains(item.Detail, "后重置") {
		t.Fatalf("detail = %q, want reset countdown", item.Detail)
	}
}

func TestGetProviderBalances_NewAPIQuotaForCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			if r.Method != http.MethodPost {
				t.Fatalf("login method = %s, want POST", r.Method)
			}
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode login payload: %v", err)
			}
			if payload["username"] != "test-user" || payload["password"] != "test-password" {
				t.Fatalf("unexpected login payload: %#v", payload)
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "session-ok", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id": 192,
				},
			})
		case "/api/status":
			if _, err := r.Cookie("session"); err != nil {
				t.Fatalf("status request missing session cookie: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_display_type": "USD",
					"quota_per_unit":     500000,
				},
			})
		case "/api/user/self":
			if _, err := r.Cookie("session"); err != nil {
				t.Fatalf("self request missing session cookie: %v", err)
			}
			if got := r.Header.Get("New-API-User"); got != "192" {
				t.Fatalf("New-API-User = %q, want 192", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota":      4100000000.0,
					"used_quota": 0.0,
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(newAPI.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-bhKbIBq3KOz0Kt0mxioRipe5z4PXWJn9yLITmjfzHa5Ez1LT",
				Name:    "test-codex-plan",
				BaseURL: newAPI.URL + "/v1",
				Headers: map[string]string{
					"X-NewAPI-Username": "test-user",
					"X-NewAPI-Password": "test-password",
				},
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.DisplayName != "test-codex-plan" {
		t.Fatalf("display_name = %q, want test-codex-plan", item.DisplayName)
	}
	if item.Kind != "remaining_balance" {
		t.Fatalf("kind = %q, want remaining_balance", item.Kind)
	}
	if item.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", item.Currency)
	}
	if math.Abs(item.Amount-8200.0) > 0.000001 {
		t.Fatalf("amount = %v, want 8200", item.Amount)
	}
	if math.Abs(item.Total-8200.0) > 0.000001 {
		t.Fatalf("total = %v, want 8200", item.Total)
	}
	if math.Abs(item.Used-0.0) > 0.000001 {
		t.Fatalf("used = %v, want 0", item.Used)
	}
	if !strings.Contains(item.Label, "钱包余额") {
		t.Fatalf("label = %q, want wallet balance label", item.Label)
	}
	if !strings.Contains(item.Detail, "来源: 账户钱包") {
		t.Fatalf("detail = %q, want wallet detail", item.Detail)
	}
}

func TestGetProviderBalances_NowCodingWithoutConsoleAuthShowsDedicatedMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-nowcoding-api-key",
				Name:    "nowcoding",
				Prefix:  "nowcoding",
				BaseURL: "https://nowcoding.ai/v1",
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "unsupported" {
		t.Fatalf("status = %q, want unsupported", item.Status)
	}
	if !strings.Contains(item.Label, "NowCoding") {
		t.Fatalf("label = %q, want nowcoding-specific copy", item.Label)
	}
	if !strings.Contains(item.Detail, "X-NewAPI-Username") {
		t.Fatalf("detail = %q, want credential hint", item.Detail)
	}
}

func TestGetProviderBalances_NowCodingQuotaViaConsoleLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	nowcoding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode login payload: %v", err)
			}
			if payload["username"] != "now-user" || payload["password"] != "now-pass" {
				t.Fatalf("unexpected login payload: %#v", payload)
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "nowcoding-session", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id": 9527,
				},
			})
		case "/api/status":
			if _, err := r.Cookie("session"); err != nil {
				t.Fatalf("status request missing session cookie: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_display_type": "USD",
					"quota_per_unit":     500000,
				},
			})
		case "/api/user/self":
			if _, err := r.Cookie("session"); err != nil {
				t.Fatalf("self request missing session cookie: %v", err)
			}
			if got := r.Header.Get("New-API-User"); got != "9527" {
				t.Fatalf("New-API-User = %q, want 9527", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota":         1250000000.0,
					"used_quota":    250000000.0,
					"request_count": 42,
				},
			})
		case "/api/user/topup/self":
			if _, err := r.Cookie("session"); err != nil {
				t.Fatalf("topup request missing session cookie: %v", err)
			}
			if got := r.Header.Get("New-API-User"); got != "9527" {
				t.Fatalf("topup New-API-User = %q, want 9527", got)
			}
			if got := r.URL.Query().Get("p"); got != "1" {
				t.Fatalf("topup page = %q, want 1", got)
			}
			if got := r.URL.Query().Get("page_size"); got != "100" {
				t.Fatalf("topup page_size = %q, want 100", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"total": 3,
					"items": []map[string]any{
						{
							"trade_no": "swx001",
							"money":    198.0,
							"status":   "success",
						},
						{
							"trade_no": "sub002",
							"money":    99.0,
							"status":   "success",
						},
						{
							"trade_no": "swx003",
							"money":    45.0,
							"status":   "success",
						},
					},
				},
			})
		case "/api/subscription/self":
			if _, err := r.Cookie("session"); err != nil {
				t.Fatalf("subscription request missing session cookie: %v", err)
			}
			if got := r.Header.Get("New-API-User"); got != "9527" {
				t.Fatalf("subscription New-API-User = %q, want 9527", got)
			}
			now := time.Now().UTC()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"billing_preference": "subscription_first",
					"subscriptions": []map[string]any{
						{
							"subscription": map[string]any{
								"amount_total":    250000000.0,
								"amount_used":     3317282.0,
								"status":          "active",
								"next_reset_time": now.Add(2 * time.Hour).Unix(),
								"end_time":        now.Add(30 * 24 * time.Hour).Unix(),
							},
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(nowcoding.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-nowcoding-api-key",
				Name:    "nowcoding",
				Prefix:  "nowcoding",
				BaseURL: nowcoding.URL + "/v1",
				Headers: map[string]string{
					"X-NewAPI-Username": "now-user",
					"X-NewAPI-Password": "now-pass",
				},
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.DisplayName != "nowcoding" {
		t.Fatalf("display_name = %q, want nowcoding", item.DisplayName)
	}
	if item.Kind != "remaining_balance" {
		t.Fatalf("kind = %q, want remaining_balance", item.Kind)
	}
	if math.Abs(item.Amount-493.365436) > 0.000001 {
		t.Fatalf("amount = %v, want subscription remaining quota", item.Amount)
	}
	if math.Abs(item.Used-6.634564) > 0.000001 {
		t.Fatalf("used = %v, want subscription used quota", item.Used)
	}
	if math.Abs(item.Total-500.0) > 0.000001 {
		t.Fatalf("total = %v, want subscription total quota", item.Total)
	}
	if !strings.Contains(item.Label, "当日订阅额度") {
		t.Fatalf("label = %q, want daily subscription label", item.Label)
	}
	if !strings.Contains(item.Detail, "今日已用 $6.63 / 日总 $500.00") {
		t.Fatalf("detail = %q, want subscription summary", item.Detail)
	}
	if !strings.Contains(item.Detail, "后重置") {
		t.Fatalf("detail = %q, want reset countdown", item.Detail)
	}
	if !strings.Contains(item.Detail, "充值账单 ¥243.00 / 2 笔") {
		t.Fatalf("detail = %q, want topup summary", item.Detail)
	}
	if !strings.Contains(item.Detail, "来源: NowCoding 订阅 /console/topup") {
		t.Fatalf("detail = %q, want subscription source", item.Detail)
	}
}

func TestGetProviderBalances_GenericNewAPIQuotaViaConsoleLogin_ShowsSubscriptionExpiry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "generic-session", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id": 2468,
				},
			})
		case "/api/status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_display_type": "USD",
					"quota_per_unit":     500000,
				},
			})
		case "/api/user/self":
			if got := r.Header.Get("New-API-User"); got != "2468" {
				t.Fatalf("New-API-User = %q, want 2468", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota":         900000000.0,
					"used_quota":    100000000.0,
					"request_count": 18,
				},
			})
		case "/api/user/topup/self":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"total": 1,
					"items": []map[string]any{
						{
							"trade_no": "swx001",
							"money":    88.0,
							"status":   "success",
						},
					},
				},
			})
		case "/api/subscription/self":
			now := time.Now().UTC()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"billing_preference": "subscription_first",
					"subscriptions": []map[string]any{
						{
							"subscription": map[string]any{
								"amount_total":    500000000.0,
								"amount_used":     50000000.0,
								"status":          "active",
								"next_reset_time": now.Add(3 * time.Hour).Unix(),
								"end_time":        now.Add(15 * 24 * time.Hour).Unix(),
							},
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(newAPI.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-generic-newapi-key",
				Name:    "test-codex-plan",
				Prefix:  "8200codex",
				BaseURL: newAPI.URL + "/v1",
				Headers: map[string]string{
					"X-NewAPI-Username": "generic-user",
					"X-NewAPI-Password": "generic-pass",
				},
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if !strings.Contains(item.Label, "当日订阅额度") {
		t.Fatalf("label = %q, want subscription label", item.Label)
	}
	if !strings.Contains(item.Detail, "今日已用 $100.00 / 日总 $1000.00") {
		t.Fatalf("detail = %q, want subscription summary", item.Detail)
	}
	if !strings.Contains(item.Detail, "订阅到期 ") {
		t.Fatalf("detail = %q, want subscription expiry", item.Detail)
	}
	if !strings.Contains(item.Detail, "来源: test-codex-plan 订阅 /console/topup") {
		t.Fatalf("detail = %q, want generic provider subscription source", item.Detail)
	}
}

func TestGetProviderBalances_NewAPISessionReuseAvoidsRepeatedLogin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var loginCount int
	newAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			loginCount++
			if loginCount > 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:  "session",
				Value: "cached-session",
				Path:  "/",
			})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id": 192,
				},
			})
		case "/api/status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_display_type": "USD",
					"quota_per_unit":     500000,
				},
			})
		case "/api/user/self":
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != "cached-session" {
				t.Fatalf("self request missing cached session cookie: %v", err)
			}
			if got := r.Header.Get("New-API-User"); got != "192" {
				t.Fatalf("New-API-User = %q, want 192", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota":      4100000000.0,
					"used_quota": 0.0,
				},
			})
		case "/api/user/topup/self":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"total": 0,
					"items": []map[string]any{},
				},
			})
		case "/api/subscription/self":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"billing_preference": "wallet_first",
					"subscriptions":      []map[string]any{},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(newAPI.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-bhKbIBq3KOz0Kt0mxioRipe5z4PXWJn9yLITmjfzHa5Ez1LT",
				Name:    "test-codex-plan",
				BaseURL: newAPI.URL + "/v1",
				Headers: map[string]string{
					"X-NewAPI-Username": "test-user",
					"X-NewAPI-Password": "test-password",
				},
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

		h.GetProviderBalances(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200: %s", i+1, rec.Code, rec.Body.String())
		}

		var payload struct {
			Items []providerBalanceItem `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("request %d unmarshal response: %v", i+1, err)
		}
		if len(payload.Items) != 1 {
			t.Fatalf("request %d items len = %d, want 1", i+1, len(payload.Items))
		}
		if payload.Items[0].Status != "ok" {
			t.Fatalf("request %d status = %q, want ok", i+1, payload.Items[0].Status)
		}
	}

	if loginCount != 1 {
		t.Fatalf("login count = %d, want 1", loginCount)
	}
}

func TestGetProviderBalances_NewAPITokenFallbackConvertsQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "session-ok", Path: "/"})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"id": 192,
				},
			})
		case "/api/status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"quota_display_type": "USD",
					"quota_per_unit":     500000,
				},
			})
		case "/api/user/self":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"success":false,"message":"boom"}`))
		case "/api/user/topup/self":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"total": 0,
					"items": []map[string]any{},
				},
			})
		case "/api/subscription/self":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"billing_preference": "wallet_first",
					"subscriptions":      []map[string]any{},
				},
			})
		case "/api/token/":
			if got := r.Header.Get("New-API-User"); got != "192" {
				t.Fatalf("New-API-User = %q, want 192", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data": map[string]any{
					"items": []map[string]any{
						{
							"name":            "test-codex-plan",
							"key":             "sk-bhKb**********z1LT",
							"remain_quota":    99999746735.0,
							"used_quota":      260822.0,
							"unlimited_quota": false,
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(newAPI.Close)

	cfg := &config.Config{
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-bhKbIBq3KOz0Kt0mxioRipe5z4PXWJn9yLITmjfzHa5Ez1LT",
				Name:    "test-codex-plan",
				BaseURL: newAPI.URL + "/v1",
				Headers: map[string]string{
					"X-NewAPI-Username":   "test-user",
					"X-NewAPI-Password":   "test-password",
					"X-NewAPI-Token-Name": "test-codex-plan",
				},
			},
		},
	}

	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/provider-balances", nil)

	h.GetProviderBalances(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Items []providerBalanceItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(payload.Items))
	}

	item := payload.Items[0]
	if item.Status != "ok" {
		t.Fatalf("status = %q, want ok", item.Status)
	}
	if item.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", item.Currency)
	}
	if math.Abs(item.Amount-199999.49347) > 0.00001 {
		t.Fatalf("amount = %v, want converted token quota", item.Amount)
	}
	if math.Abs(item.Used-0.521644) > 0.00001 {
		t.Fatalf("used = %v, want converted token usage", item.Used)
	}
	if !strings.Contains(item.Label, "199999.49") {
		t.Fatalf("label = %q, want converted token label", item.Label)
	}
	if !strings.Contains(item.Detail, "token 配额") {
		t.Fatalf("detail = %q, want token fallback detail", item.Detail)
	}
	if strings.Contains(item.Detail, "260822.00") {
		t.Fatalf("detail = %q, should not expose raw unconverted token used quota", item.Detail)
	}
}

func TestMatchOfficialDailyUsageAdapter(t *testing.T) {
	tests := []struct {
		name string
		req  providerBalanceRequest
		want string
		ok   bool
	}{
		{
			name: "match aixj by name",
			req: providerBalanceRequest{
				DisplayName: "aixj",
			},
			want: "aixj",
			ok:   true,
		},
		{
			name: "match gmncode by host",
			req: providerBalanceRequest{
				DisplayName: "openai",
				BaseURL:     "https://gmncode.cn/v1",
			},
			want: "gmncode",
			ok:   true,
		},
		{
			name: "unknown provider",
			req: providerBalanceRequest{
				DisplayName: "openai",
				BaseURL:     "https://example.com/v1",
			},
			ok: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchOfficialDailyUsageAdapter(tc.req)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if got.DisplayName != tc.want {
				t.Fatalf("display_name = %q, want %q", got.DisplayName, tc.want)
			}
		})
	}
}

func TestResolveSub2APIUsageEndpoint_SoapAPIUsesPublicKeyUsage(t *testing.T) {
	got, err := resolveSub2APIUsageEndpointForRequest(providerBalanceRequest{
		DisplayName: "soapapi",
		BaseURL:     "https://example.com",
	})
	if err != nil {
		t.Fatalf("resolve endpoint: %v", err)
	}
	if got != "https://example.com/v1/public/key-usage" {
		t.Fatalf("endpoint = %q, want %q", got, "https://example.com/v1/public/key-usage")
	}
}

func TestResetCountdownAtChinaTime_UsesFixedAIXJResetTime(t *testing.T) {
	now := time.Date(2026, 3, 18, 5, 27, 9, 0, time.UTC)
	if got := resetCountdownAtChinaTime(now, 11, 45); got != "22h 18m" {
		t.Fatalf("countdown = %q, want %q", got, "22h 18m")
	}
}

func TestResetCountdownAtChinaTime_SupportsNextMidnight(t *testing.T) {
	now := time.Date(2026, 3, 18, 5, 35, 46, 0, time.UTC)
	if got := resetCountdownAtChinaTime(now, 24, 0); got != "10h 25m" {
		t.Fatalf("countdown = %q, want %q", got, "10h 25m")
	}
}

func strconvItoa(v int) string {
	return strconv.Itoa(v)
}
