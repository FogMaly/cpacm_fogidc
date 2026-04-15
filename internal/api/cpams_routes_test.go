package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestCPAMSSyntheticConversationSeed_IgnoresHost(t *testing.T) {
	buildSeed := func(host string) string {
		req := httptest.NewRequest(http.MethodPost, "https://"+host+"/cpamc/claude/messages", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer test-proxy-key")
		req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
		req.Header.Set("X-Forwarded-For", "198.51.100.24")
		return cpamsSyntheticConversationSeed(req)
	}

	seedFromFogIDC := buildSeed("ysjf.fogidc.com")
	seedFromOtherHost := buildSeed("other.example.com")
	if strings.TrimSpace(seedFromFogIDC) == "" {
		t.Fatal("seedFromFogIDC = empty, want non-empty")
	}
	if seedFromFogIDC != seedFromOtherHost {
		t.Fatalf("cpams synthetic conversation seed changed with host: fogidc=%q other=%q", seedFromFogIDC, seedFromOtherHost)
	}
}

func TestCPAMSSyntheticConversationSeed_ChangesWithForwardedClientIP(t *testing.T) {
	buildSeed := func(clientIP string) string {
		req := httptest.NewRequest(http.MethodPost, "https://ysjf.fogidc.com/cpamc/claude/messages", nil)
		req.Host = "ysjf.fogidc.com"
		req.Header.Set("Authorization", "Bearer test-proxy-key")
		req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
		req.Header.Set("X-Forwarded-For", clientIP)
		return cpamsSyntheticConversationSeed(req)
	}

	seedA := buildSeed("198.51.100.24")
	seedB := buildSeed("198.51.100.77")
	if strings.TrimSpace(seedA) == "" || strings.TrimSpace(seedB) == "" {
		t.Fatalf("unexpected empty seeds: seedA=%q seedB=%q", seedA, seedB)
	}
	if seedA == seedB {
		t.Fatalf("cpams synthetic conversation seed did not change with forwarded client IP: %q", seedA)
	}
}

func TestParseCPAMSModelAlias(t *testing.T) {
	testCases := []struct {
		name               string
		model              string
		wantChannel        string
		wantRequestedModel string
		wantHasAlias       bool
	}{
		{
			name:               "codex direct alias",
			model:              "codex/gpt-5.2",
			wantChannel:        "codex",
			wantRequestedModel: "gpt-5.2",
			wantHasAlias:       true,
		},
		{
			name:               "claude direct alias",
			model:              "claude/claude-sonnet-4-6",
			wantChannel:        "claude",
			wantRequestedModel: "claude-sonnet-4-6",
			wantHasAlias:       true,
		},
		{
			name:               "cpamc codex alias",
			model:              "cpamc/codex/gpt-5.2",
			wantChannel:        "codex",
			wantRequestedModel: "gpt-5.2",
			wantHasAlias:       true,
		},
		{
			name:               "cpamc claude alias",
			model:              "cpamc/claude/claude-sonnet-4-6",
			wantChannel:        "claude",
			wantRequestedModel: "claude-sonnet-4-6",
			wantHasAlias:       true,
		},
		{
			name:               "alias preserves suffix",
			model:              "cpamc/codex/gpt-5.4-pro(high)",
			wantChannel:        "codex",
			wantRequestedModel: "gpt-5.4-pro(high)",
			wantHasAlias:       true,
		},
		{
			name:               "non alias model",
			model:              "gpt-5.2",
			wantChannel:        "",
			wantRequestedModel: "",
			wantHasAlias:       false,
		},
		{
			name:               "invalid cpamc channel",
			model:              "cpamc/unknown/gpt-5.2",
			wantChannel:        "",
			wantRequestedModel: "",
			wantHasAlias:       false,
		},
		{
			name:               "single segment channel",
			model:              "codex",
			wantChannel:        "",
			wantRequestedModel: "",
			wantHasAlias:       false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotChannel, gotRequestedModel, gotHasAlias := parseCPAMSModelAlias(tc.model)
			if gotChannel != tc.wantChannel || gotRequestedModel != tc.wantRequestedModel || gotHasAlias != tc.wantHasAlias {
				t.Fatalf(
					"parseCPAMSModelAlias(%q) = (%q, %q, %t), want (%q, %q, %t)",
					tc.model, gotChannel, gotRequestedModel, gotHasAlias, tc.wantChannel, tc.wantRequestedModel, tc.wantHasAlias,
				)
			}
		})
	}
}

func TestNormalizeCPAMSRequestedModel(t *testing.T) {
	testCases := []struct {
		name      string
		channel   string
		model     string
		strict    bool
		wantModel string
		wantErr   bool
	}{
		{
			name:      "plain model stays plain",
			channel:   "codex",
			model:     "gpt-5.4-pro",
			strict:    false,
			wantModel: "gpt-5.4-pro",
		},
		{
			name:      "cpamc alias strips prefix",
			channel:   "codex",
			model:     "cpamc/codex/gpt-5.4-pro(high)",
			strict:    false,
			wantModel: "gpt-5.4-pro(high)",
		},
		{
			name:      "provider prefix strips upstream prefix",
			channel:   "codex",
			model:     "sub2api/gpt-5.4-pro",
			strict:    false,
			wantModel: "gpt-5.4-pro",
		},
		{
			name:    "alias channel mismatch returns error",
			channel: "codex",
			model:   "cpamc/claude/claude-sonnet-4-6",
			strict:  false,
			wantErr: true,
		},
		{
			name:    "strict public rejects cpamc alias",
			channel: "codex",
			model:   "cpamc/codex/gpt-5.4-pro",
			strict:  true,
			wantErr: true,
		},
		{
			name:    "strict public rejects provider prefix",
			channel: "codex",
			model:   "sub2api/gpt-5.4-pro",
			strict:  true,
			wantErr: true,
		},
		{
			name:    "strict public requires model",
			channel: "codex",
			model:   "",
			strict:  true,
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeCPAMSRequestedModel(tc.channel, tc.model, tc.strict)
			if (err != nil) != tc.wantErr {
				t.Fatalf("normalizeCPAMSRequestedModel(%q, %q, strict=%t) error = %v, wantErr %t", tc.channel, tc.model, tc.strict, err, tc.wantErr)
			}
			if err == nil && got != tc.wantModel {
				t.Fatalf("normalizeCPAMSRequestedModel(%q, %q, strict=%t) = %q, want %q", tc.channel, tc.model, tc.strict, got, tc.wantModel)
			}
		})
	}
}

func TestCPAMSHasExplicitThinkingConfig(t *testing.T) {
	testCases := []struct {
		name  string
		model string
		raw   string
		want  bool
	}{
		{
			name:  "model suffix counts as explicit",
			model: "gpt-5.4-pro(high)",
			raw:   `{}`,
			want:  true,
		},
		{
			name:  "openai chat reasoning effort",
			model: "gpt-5.4-pro",
			raw:   `{"reasoning_effort":"high"}`,
			want:  true,
		},
		{
			name:  "openai responses reasoning effort",
			model: "gpt-5.4-pro",
			raw:   `{"reasoning":{"effort":"low"}}`,
			want:  true,
		},
		{
			name:  "claude thinking type",
			model: "claude-sonnet-4-6",
			raw:   `{"thinking":{"type":"enabled"}}`,
			want:  true,
		},
		{
			name:  "claude thinking budget",
			model: "claude-sonnet-4-6",
			raw:   `{"thinking":{"budget_tokens":2048}}`,
			want:  true,
		},
		{
			name:  "gemini thinking level",
			model: "gemini-2.5-pro",
			raw:   `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh"}}}`,
			want:  true,
		},
		{
			name:  "missing thinking config",
			model: "gpt-5.4-pro",
			raw:   `{"input":"hi"}`,
			want:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := cpamsHasExplicitThinkingConfig(tc.model, []byte(tc.raw))
			if got != tc.want {
				t.Fatalf("cpamsHasExplicitThinkingConfig(%q, %s) = %t, want %t", tc.model, tc.raw, got, tc.want)
			}
		})
	}
}

func TestCPAMSEnsureThinkingSuffix(t *testing.T) {
	testCases := []struct {
		name   string
		model  string
		suffix string
		want   string
	}{
		{
			name:   "plain model gets suffix",
			model:  "gpt-5.4-pro",
			suffix: string(thinking.LevelXHigh),
			want:   "gpt-5.4-pro(xhigh)",
		},
		{
			name:   "existing suffix stays unchanged",
			model:  "gpt-5.4-pro(high)",
			suffix: string(thinking.LevelXHigh),
			want:   "gpt-5.4-pro(high)",
		},
		{
			name:   "blank model stays blank",
			model:  "",
			suffix: string(thinking.LevelXHigh),
			want:   "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := cpamsEnsureThinkingSuffix(tc.model, tc.suffix)
			if got != tc.want {
				t.Fatalf("cpamsEnsureThinkingSuffix(%q, %q) = %q, want %q", tc.model, tc.suffix, got, tc.want)
			}
		})
	}
}

func TestCPAMSProtocolHint(t *testing.T) {
	testCases := []struct {
		path string
		want string
	}{
		{path: "/v1/responses", want: "responses"},
		{path: "/v1/responses/compact", want: "responses/compact"},
		{path: "/v1/chat/completions", want: "chat/completions"},
		{path: "/v1/messages", want: "messages"},
	}

	for _, tc := range testCases {
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req
		if got := cpamsProtocolHint(c); got != tc.want {
			t.Fatalf("cpamsProtocolHint(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestCPAMSRequiresStatefulCodexResponses(t *testing.T) {
	if cpamsRequiresStatefulCodexResponses("codex", "responses", []byte(`{"prompt_cache_key":"conv-1"}`)) {
		t.Fatalf("prompt_cache_key alone should not require stateful responses routing")
	}
	if cpamsRequiresStatefulCodexResponses("codex", "responses", []byte(`{"parallel_tool_calls":true,"tool_choice":"auto","tools":[]}`)) {
		t.Fatalf("openclaw-style first response request should not require stateful responses routing")
	}
	if !cpamsRequiresStatefulCodexResponses("codex", "responses", []byte(`{"previous_response_id":"resp_123"}`)) {
		t.Fatalf("previous_response_id should require stateful responses routing")
	}
	if cpamsRequiresStatefulCodexResponses("codex", "responses", []byte(`{"tools":[{"type":"function","name":"x"}],"tool_choice":"auto","parallel_tool_calls":true}`)) {
		t.Fatalf("tool metadata alone should not require stateful responses routing before a previous_response_id exists")
	}
	if cpamsRequiresStatefulCodexResponses("codex", "chat/completions", []byte(`{"prompt_cache_key":"conv-1"}`)) {
		t.Fatalf("chat/completions should not require stateful responses routing")
	}
	if cpamsRequiresStatefulCodexResponses("claude", "responses", []byte(`{"prompt_cache_key":"conv-1"}`)) {
		t.Fatalf("non-codex channel should not require stateful responses routing")
	}
}

func TestApplyCPAMSResolution_PublicRouteKeepsSafeHeaders(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/cpamc/codex/responses", nil)
	ctx.Set("cpamc_public_strict", true)

	server := &Server{}
	server.applyCPAMSResolution(ctx, "codex", cpamsResolution{
		Model:          "sub2api/gpt-5.4(xhigh)",
		RequestedModel: "gpt-5.4(xhigh)",
		Decision:       "issue_new_token_round_robin_same_model",
	})

	if got := recorder.Header().Get("X-CPAMS-Channel"); got != "codex" {
		t.Fatalf("X-CPAMS-Channel = %q, want %q on public route", got, "codex")
	}
	if got := recorder.Header().Get("X-CPAMS-Requested-Model"); got != "gpt-5.4(xhigh)" {
		t.Fatalf("X-CPAMS-Requested-Model = %q, want %q on public route", got, "gpt-5.4(xhigh)")
	}
	if got := recorder.Header().Get("X-CPAMS-Decision"); got != "issue_new_token_round_robin_same_model" {
		t.Fatalf("X-CPAMS-Decision = %q, want %q on public route", got, "issue_new_token_round_robin_same_model")
	}
	if got := recorder.Header().Get("X-CPAMS-Model"); got != "" {
		t.Fatalf("X-CPAMS-Model = %q, want empty on public route", got)
	}
	if got := ctx.GetString("cpams_model"); got != "sub2api/gpt-5.4(xhigh)" {
		t.Fatalf("cpams_model context = %q, want %q", got, "sub2api/gpt-5.4(xhigh)")
	}
}

func TestCPAMSShouldSuppressTransientInvalidation(t *testing.T) {
	if !cpamsShouldSuppressTransientInvalidation(cpamsResolution{
		Model:          "claude cheep/claude-opus-4-6(xhigh)",
		CandidateCount: 1,
	}) {
		t.Fatal("expected single bridge candidate transient invalidation to be suppressed")
	}
	if cpamsShouldSuppressTransientInvalidation(cpamsResolution{
		Model:          "covs/claude-opus-4-6(xhigh)",
		CandidateCount: 1,
	}) {
		t.Fatal("did not expect native candidate transient invalidation suppression")
	}
	if cpamsShouldSuppressTransientInvalidation(cpamsResolution{
		Model:          "claude cheep/claude-opus-4-6(xhigh)",
		CandidateCount: 2,
	}) {
		t.Fatal("did not expect suppression when alternatives exist")
	}
}

func TestMaybeInvalidateCPAMSResolution_DoesNotCooldownSingleBridgeCandidate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 21, 0, 0, 0, time.UTC)
	resolver := &cpamsResolver{
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		failed:        make(map[string]cpamsFailureBlock),
		now:           func() time.Time { return now },
	}
	server := &Server{cpamsResolver: resolver}

	server.maybeInvalidateCPAMSResolution("claude", "claude-opus-4-6", cpamsResolution{
		Model:           "claude cheep/claude-opus-4-6(xhigh)",
		RequestedModel:  "claude-opus-4-6(xhigh)",
		CandidateCount:  1,
		SnapshotUpdated: now.Add(-time.Minute),
	}, "", http.StatusInternalServerError)

	if len(resolver.failed) != 0 {
		t.Fatalf("failed blocks = %d, want 0", len(resolver.failed))
	}
}

func TestMaybeInvalidateCPAMSResolution_CooldownsSingleNativeCandidate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 21, 0, 0, 0, time.UTC)
	resolver := &cpamsResolver{
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		failed:        make(map[string]cpamsFailureBlock),
		now:           func() time.Time { return now },
	}
	server := &Server{cpamsResolver: resolver}

	server.maybeInvalidateCPAMSResolution("claude", "claude-opus-4-6", cpamsResolution{
		Model:           "covs/claude-opus-4-6(xhigh)",
		RequestedModel:  "claude-opus-4-6(xhigh)",
		CandidateCount:  1,
		SnapshotUpdated: now.Add(-time.Minute),
	}, "", http.StatusInternalServerError)

	if len(resolver.failed) != 1 {
		t.Fatalf("failed blocks = %d, want 1", len(resolver.failed))
	}
}

func TestResolveCPAMSRequest_DefaultThinkingUsesXHigh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-06T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
	}

	raw := []byte(`{"model":"gpt-5.4-pro","input":"reply with exactly OK"}`)
	resolution, rewritten, err := server.resolveCPAMSRequest(nil, "codex", "gpt-5.4-pro", raw)
	if err != nil {
		t.Fatalf("resolveCPAMSRequest error = %v", err)
	}
	if !strings.HasSuffix(resolution.Model, "("+string(thinking.LevelXHigh)+")") {
		t.Fatalf("resolution.Model = %q, want default xhigh suffix", resolution.Model)
	}
	if resolution.RequestedModel != "gpt-5.4-pro(xhigh)" {
		t.Fatalf("resolution.RequestedModel = %q, want %q", resolution.RequestedModel, "gpt-5.4-pro(xhigh)")
	}
	if got := gjson.GetBytes(rewritten, "model").String(); got != "sub2api/gpt-5.4-pro(xhigh)" {
		t.Fatalf("rewritten model = %q, want %q", got, "sub2api/gpt-5.4-pro(xhigh)")
	}
}

func TestResolveCPAMSRequest_ExplicitThinkingConfigIsPreserved(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-06T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
	}

	raw := []byte(`{"model":"gpt-5.4-pro","reasoning_effort":"low","input":"reply with exactly OK"}`)
	resolution, rewritten, err := server.resolveCPAMSRequest(nil, "codex", "gpt-5.4-pro", raw)
	if err != nil {
		t.Fatalf("resolveCPAMSRequest error = %v", err)
	}
	if thinking.ParseSuffix(resolution.Model).HasSuffix {
		t.Fatalf("resolution.Model = %q, should keep explicit body thinking config without forcing suffix", resolution.Model)
	}
	if got := gjson.GetBytes(rewritten, "model").String(); got != "sub2api/gpt-5.4-pro" {
		t.Fatalf("rewritten model = %q, want %q", got, "sub2api/gpt-5.4-pro")
	}
	if got := gjson.GetBytes(rewritten, "reasoning_effort").String(); got != "low" {
		t.Fatalf("rewritten reasoning_effort = %q, want %q", got, "low")
	}
}

func TestExtractCPAMSConversationKey_IgnoresBroadUserAffinityFields(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
  "model": "gpt-5.4-pro",
  "user": "user-123",
  "metadata": {
    "user_id": "u-1",
    "session_id": "s-1"
  }
}`)

	if got := extractCPAMSConversationKey(nil, raw); got != "" {
		t.Fatalf("extractCPAMSConversationKey() = %q, want empty for broad user/session fields", got)
	}
}

func TestExtractCPAMSConversationKey_FallsBackToSyntheticSessionSeed(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	req := httptest.NewRequest(http.MethodPost, "/cpamc/claude/messages", bytes.NewBufferString(`{"model":"claude-opus-4-6"}`))
	req.Header.Set("Authorization", "Bearer test-proxy-key")
	req.Header.Set("User-Agent", "sticky-client/1.0")
	req.Header.Set("X-Forwarded-For", "198.51.100.24")
	ctx.Request = req

	got := extractCPAMSConversationKey(ctx, []byte(`{"model":"claude-opus-4-6"}`))
	if !strings.HasPrefix(got, "session_affinity_seed:fallback:") {
		t.Fatalf("extractCPAMSConversationKey() = %q, want session_affinity_seed:fallback:*", got)
	}
	if strings.Contains(got, "test-proxy-key") {
		t.Fatalf("extractCPAMSConversationKey() leaked raw auth token: %q", got)
	}
}

func TestExtractCPAMSConversationKey_UsesExplicitConversationFields(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
  "model": "claude-opus-4-6",
  "conversation_id": "conv-123"
}`)

	if got := extractCPAMSConversationKey(nil, raw); got != "conversation_id:conv-123" {
		t.Fatalf("extractCPAMSConversationKey() = %q, want %q", got, "conversation_id:conv-123")
	}
}

func TestExtractCPAMSConversationKey_UsesClaudeMetadataUserIDSession(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
  "model": "claude-opus-4-6",
  "metadata": {
    "user_id": "user_1640b1bedb606454bde7339145a4edb1af90468fd214340f5a5d0bb55444af86_account__session_5c259435-c2af-4b4a-9aa7-a406aa929f85"
  }
}`)

	if got := extractCPAMSConversationKey(nil, raw); got != "metadata.user_id.account__session:5c259435-c2af-4b4a-9aa7-a406aa929f85" {
		t.Fatalf("extractCPAMSConversationKey() = %q, want Claude metadata session key", got)
	}
}

func TestExtractCPAMSConversationKey_UsesSessionHeader(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	req := httptest.NewRequest(http.MethodPost, "/cpamc/claude/messages", bytes.NewBufferString(`{"model":"claude-opus-4-6"}`))
	req.Header.Set("Session-Id", "sess-42")
	ctx.Request = req

	if got := extractCPAMSConversationKey(ctx, []byte(`{"model":"claude-opus-4-6"}`)); got != "session-id:sess-42" {
		t.Fatalf("extractCPAMSConversationKey() = %q, want %q", got, "session-id:sess-42")
	}
}

func TestWrapCPAMSModelAliasRoute_RewritesStandardV1CodexAlias(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-06T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/responses", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		c.JSON(http.StatusOK, gin.H{
			"model":          gjson.GetBytes(raw, "model").String(),
			"cpams_model":    c.GetString("cpams_model"),
			"cpams_channel":  c.GetString("cpams_channel"),
			"cpams_decision": c.GetString("cpams_decision"),
		})
	}))

	body := `{"model":"codex/gpt-5.4-pro","input":"reply with exactly OK"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "sub2api/gpt-5.4-pro(xhigh)" {
		t.Fatalf("rewritten model = %q, want %q", got, "sub2api/gpt-5.4-pro(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "cpams_channel").String(); got != "codex" {
		t.Fatalf("cpams_channel = %q, want %q", got, "codex")
	}
	if got := recorder.Header().Get("X-CPAMS-Model"); got != "sub2api/gpt-5.4-pro(xhigh)" {
		t.Fatalf("X-CPAMS-Model = %q, want %q", got, "sub2api/gpt-5.4-pro(xhigh)")
	}
}

func TestWrapCPAMSModelAliasRoute_PassesThroughPlainModel(t *testing.T) {
	t.Parallel()

	server := &Server{}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/responses", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		c.JSON(http.StatusOK, gin.H{
			"model":       gjson.GetBytes(raw, "model").String(),
			"cpams_model": c.GetString("cpams_model"),
		})
	}))

	body := `{"model":"gpt-5.4","input":"reply with exactly OK"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "gpt-5.4" {
		t.Fatalf("model = %q, want %q", got, "gpt-5.4")
	}
	if got := gjson.Get(recorder.Body.String(), "cpams_model").String(); got != "" {
		t.Fatalf("cpams_model = %q, want empty for plain model passthrough", got)
	}
}

func TestWrapCPAMSModelAliasRoute_RejectsNativeClaudeIngressOnChatCompletions(t *testing.T) {
	t.Parallel()

	server := &Server{}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/chat/completions", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		t.Fatal("downstream should not run for native Claude ingress on /v1/chat/completions")
	}))

	body := `{"model":"claude/claude-opus-4-6","messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-App", "cli")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "error.code").String(); got != "cpamc_native_claude_requires_messages" {
		t.Fatalf("error.code = %q, want %q", got, "cpamc_native_claude_requires_messages")
	}
	if got := recorder.Header().Get("X-CPAMS-Expected-Endpoint"); got != "/v1/messages" {
		t.Fatalf("X-CPAMS-Expected-Endpoint = %q, want %q", got, "/v1/messages")
	}
}

func TestWrapCPAMSModelAliasRoute_AllowsOpenAICompatClaudeAliasOnChatCompletions(t *testing.T) {
	t.Parallel()

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

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/chat/completions", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		c.JSON(http.StatusOK, gin.H{
			"model":         gjson.GetBytes(raw, "model").String(),
			"cpams_channel": c.GetString("cpams_channel"),
		})
	}))

	body := `{"model":"claude/claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "openai-client/1.0")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "covs/claude-opus-4-6(xhigh)" {
		t.Fatalf("rewritten model = %q, want %q", got, "covs/claude-opus-4-6(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "cpams_channel").String(); got != "claude" {
		t.Fatalf("cpams_channel = %q, want %q", got, "claude")
	}
}

func TestUnifiedChatCompletionsHandler_RoutesNativeClaudeAliasOnChatCompletions(t *testing.T) {
	t.Parallel()

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

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
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
				"model":         gjson.GetBytes(raw, "model").String(),
				"cpams_channel": c.GetString("cpams_channel"),
			})
		},
	))

	body := `{"model":"claude/claude-opus-4-6","messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-App", "cli")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "covs/claude-opus-4-6(xhigh)" {
		t.Fatalf("model = %q, want %q", got, "covs/claude-opus-4-6(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "cpams_channel").String(); got != "claude" {
		t.Fatalf("cpams_channel = %q, want %q", got, "claude")
	}
}

func TestUnifiedChatCompletionsHandler_RoutesNativeClaudePlainModelThroughCPAMS(t *testing.T) {
	t.Parallel()

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

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
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
				"model":         gjson.GetBytes(raw, "model").String(),
				"cpams_channel": c.GetString("cpams_channel"),
			})
		},
	))

	body := `{"model":"claude-opus-4-6","messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-App", "cli")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "covs/claude-opus-4-6(xhigh)" {
		t.Fatalf("model = %q, want %q", got, "covs/claude-opus-4-6(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "cpams_channel").String(); got != "claude" {
		t.Fatalf("cpams_channel = %q, want %q", got, "claude")
	}
}

func TestWrapCPAMSChatCompletionsRoute_RoutesNativeClaudeCompatBridge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "covs",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "covs/claude-opus-4-6", "status": "green", "reason": "ok", "tested_at": "2026-04-10T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/cpamc/:channel/chat/completions", server.wrapCPAMSChatCompletionsRoute(
		func(c *gin.Context) {
			t.Fatal("openai handler should not run for native Claude ingress")
		},
		func(c *gin.Context) {
			raw, err := c.GetRawData()
			if err != nil {
				t.Fatalf("GetRawData() error = %v", err)
			}
			c.JSON(http.StatusOK, gin.H{
				"model":         gjson.GetBytes(raw, "model").String(),
				"cpams_channel": c.GetString("cpams_channel"),
				"route":         c.GetString("cpams_route"),
			})
		},
	))

	body := `{"model":"claude-opus-4-6","messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.72.364; You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/cpamc/claude/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.72 (external, cli)")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-App", "cli")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "covs/claude-opus-4-6(xhigh)" {
		t.Fatalf("model = %q, want %q", got, "covs/claude-opus-4-6(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "cpams_channel").String(); got != "claude" {
		t.Fatalf("cpams_channel = %q, want %q", got, "claude")
	}
}

func TestWrapCPAMSChatCompletionsRoute_AllowsCodexCompatBridge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-04-10T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/cpamc/:channel/chat/completions", server.wrapCPAMSChatCompletionsRoute(
		func(c *gin.Context) {
			raw, err := c.GetRawData()
			if err != nil {
				t.Fatalf("GetRawData() error = %v", err)
			}
			c.JSON(http.StatusOK, gin.H{
				"model":         gjson.GetBytes(raw, "model").String(),
				"cpams_channel": c.GetString("cpams_channel"),
			})
		},
		nil,
	))

	body := `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/cpamc/codex/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "yunyi-codex/gpt-5.4(xhigh)" {
		t.Fatalf("rewritten model = %q, want %q", got, "yunyi-codex/gpt-5.4(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "cpams_channel").String(); got != "codex" {
		t.Fatalf("cpams_channel = %q, want %q", got, "codex")
	}
}

func TestWrapCPAMSModelAliasRoute_AllowsResponsesFirstTurnWithToolMetadata(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "green", "reason": "responses_continuation_failed", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "health-aware"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/responses", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		c.JSON(http.StatusOK, gin.H{
			"model":          gjson.GetBytes(raw, "model").String(),
			"tool_choice":    gjson.GetBytes(raw, "tool_choice").String(),
			"parallel_tools": gjson.GetBytes(raw, "parallel_tool_calls").Bool(),
			"cpams_model":    c.GetString("cpams_model"),
		})
	}))

	body := `{"model":"codex/gpt-5.4","previous_response_id":null,"parallel_tool_calls":true,"tool_choice":"auto","tools":[],"input":"reply with exactly OK"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "yunyi-codex/gpt-5.4(xhigh)" {
		t.Fatalf("rewritten model = %q, want %q", got, "yunyi-codex/gpt-5.4(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want %q", got, "auto")
	}
	if !gjson.Get(recorder.Body.String(), "parallel_tools").Bool() {
		t.Fatalf("parallel_tool_calls should be preserved")
	}
}

func TestCPAMSPayloadHasSemanticToolUseMismatch(t *testing.T) {
	t.Parallel()

	if !cpamsPayloadHasSemanticToolUseMismatch([]byte(`{"error":{"message":"Invalid tool_use_id in tool_result. The referenced tool call does not exist in the current conversation."}}`)) {
		t.Fatal("expected semantic tool_use mismatch to be detected")
	}
	if cpamsPayloadHasSemanticToolUseMismatch([]byte(`{"id":"msg_123","type":"message"}`)) {
		t.Fatal("did not expect normal payload to be detected as semantic mismatch")
	}
}

func TestCPAMSRequestToolResultCount(t *testing.T) {
	t.Parallel()

	openAICompat := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_todos","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"ok"},
			{"role":"tool","tool_call_id":"call_2","content":"ok"}
		]
	}`)
	if got := cpamsRequestToolResultCount(openAICompat); got != 2 {
		t.Fatalf("cpamsRequestToolResultCount(openai compat) = %d, want 2", got)
	}

	nativeClaude := []byte(`{
		"messages":[
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":"ok"},
				{"type":"tool_result","tool_use_id":"call_2","content":"ok"}
			]}
		]
	}`)
	if got := cpamsRequestToolResultCount(nativeClaude); got != 2 {
		t.Fatalf("cpamsRequestToolResultCount(native claude) = %d, want 2", got)
	}
}

func TestCPAMSAnalyzeClaudeContinuationResponse(t *testing.T) {
	t.Parallel()

	native := cpamsAnalyzeClaudeContinuationResponse([]byte(`{
		"content":[
			{"type":"tool_use","id":"call_1","name":"read_todos","input":{}},
			{"type":"tool_use","id":"call_2","name":"read_memory","input":{}}
		],
		"stop_reason":"tool_use"
	}`))
	if native.StopReason != "tool_use" {
		t.Fatalf("native StopReason = %q, want tool_use", native.StopReason)
	}
	if native.ToolUseCount != 2 {
		t.Fatalf("native ToolUseCount = %d, want 2", native.ToolUseCount)
	}
	if native.ToolCallCount != 0 {
		t.Fatalf("native ToolCallCount = %d, want 0", native.ToolCallCount)
	}

	compat := cpamsAnalyzeClaudeContinuationResponse([]byte(`{
		"choices":[
			{
				"finish_reason":"tool_calls",
				"message":{
					"tool_calls":[
						{"id":"call_1","type":"function","function":{"name":"read_todos","arguments":"{}"}}
					]
				}
			}
		]
	}`))
	if compat.StopReason != "tool_calls" {
		t.Fatalf("compat StopReason = %q, want tool_calls", compat.StopReason)
	}
	if compat.ToolUseCount != 0 {
		t.Fatalf("compat ToolUseCount = %d, want 0", compat.ToolUseCount)
	}
	if compat.ToolCallCount != 1 {
		t.Fatalf("compat ToolCallCount = %d, want 1", compat.ToolCallCount)
	}
}

func TestWrapCPAMSModelAliasRoute_InvalidatesSemanticClaudeToolUseMismatch(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T00:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "covs",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "covs/claude-opus-4-6", "status": "green", "reason": "ok", "tested_at": "2026-04-10T00:00:00Z"}
      ]
    },
    {
      "provider_prefix": "hedaiai",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "hedaiai/claude-opus-4-6", "status": "green", "reason": "ok", "tested_at": "2026-04-10T00:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}},
		cpamsResolver: resolver,
	}

	var seen []string
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/chat/completions", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		model := gjson.GetBytes(raw, "model").String()
		seen = append(seen, model)
		if len(seen) == 1 {
			c.Data(http.StatusOK, "application/json", []byte(`{"error":{"message":"Invalid tool_use_id in tool_result. The referenced tool call does not exist in the current conversation.","type":"invalid_request_error"},"type":"error"}`))
			return
		}
		c.JSON(http.StatusOK, gin.H{"model": model})
	}))

	body := `{"model":"claude/claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	engine.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d body=%s", rec1.Code, http.StatusOK, rec1.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	engine.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d body=%s", rec2.Code, http.StatusOK, rec2.Body.String())
	}
	if len(seen) != 2 {
		t.Fatalf("seen models = %d, want 2", len(seen))
	}
	if seen[0] == seen[1] {
		t.Fatalf("expected second request to switch model after semantic mismatch, got %q twice", seen[0])
	}
}

func TestWrapCPAMSModelAliasRoute_AllowsResponsesContinuationViaBridgeCapableProvider(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "openai",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "openai/gpt-5.4", "status": "green", "reason": "text_only_ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "health-aware"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/responses", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":          true,
			"model":       gjson.GetBytes(raw, "model").String(),
			"previous_id": gjson.GetBytes(raw, "previous_response_id").String(),
		})
	}))

	body := `{"model":"codex/gpt-5.4","previous_response_id":"resp_123","parallel_tool_calls":true,"tool_choice":"auto","tools":[],"input":"reply with exactly OK"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "openai/gpt-5.4(xhigh)" {
		t.Fatalf("rewritten model = %q, want %q", got, "openai/gpt-5.4(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "previous_id").String(); got != "resp_123" {
		t.Fatalf("previous_response_id = %q, want %q", got, "resp_123")
	}
}

func TestWrapCPAMSModelAliasRoute_AllowsResponsesContinuationForYunyiCodexWhenProbePasses(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "green", "reason": "text_only_ok", "responses_continuation_status": "ok", "responses_continuation_reason": "stateful_responses_ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "health-aware"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/responses", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		c.JSON(http.StatusOK, gin.H{
			"model":       gjson.GetBytes(raw, "model").String(),
			"previous_id": gjson.GetBytes(raw, "previous_response_id").String(),
			"cpams_model": c.GetString("cpams_model"),
		})
	}))

	body := `{"model":"codex/gpt-5.4","previous_response_id":"resp_123","input":"reply with exactly OK"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.Get(recorder.Body.String(), "model").String(); got != "yunyi-codex/gpt-5.4(xhigh)" {
		t.Fatalf("rewritten model = %q, want %q", got, "yunyi-codex/gpt-5.4(xhigh)")
	}
	if got := gjson.Get(recorder.Body.String(), "previous_id").String(); got != "resp_123" {
		t.Fatalf("previous_response_id = %q, want %q", got, "resp_123")
	}
}

func TestWrapCPAMSModelAliasRoute_InvalidatesFailedSelection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "health-aware"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/responses", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream_timeout"})
	}))

	body := `{"model":"codex/gpt-5.4","input":"reply with exactly OK"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
	}

	state, err := resolver.Snapshot("codex")
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if got := len(state["codex"].Tokens); got != 0 {
		t.Fatalf("token count = %d, want 0", got)
	}
	if got := state["codex"].Selection.Model; got != "" {
		t.Fatalf("selection model = %q, want empty", got)
	}
}

func TestWrapCPAMSModelAliasRoute_BindsPreviousResponseIDToOriginalSelection(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "stateful_responses_ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "green", "reason": "stateful_responses_ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	server := &Server{
		cfg:           &config.Config{Routing: config.RoutingConfig{Strategy: "round-robin"}},
		cpamsResolver: resolver,
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/responses", server.wrapCPAMSModelAliasRoute(func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			t.Fatalf("GetRawData() error = %v", err)
		}
		if strings.TrimSpace(gjson.GetBytes(raw, "previous_response_id").String()) == "" {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\",\"status\":\"in_progress\"}}\n\n")
			_, _ = c.Writer.WriteString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_123\",\"status\":\"completed\"}}\n\n")
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"model":          gjson.GetBytes(raw, "model").String(),
			"cpams_model":    c.GetString("cpams_model"),
			"cpams_decision": c.GetString("cpams_decision"),
		})
	}))

	firstBody := `{"model":"codex/gpt-5.4","parallel_tool_calls":true,"tool_choice":"auto","tools":[],"input":"step one"}`
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(firstBody))
	firstReq.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	engine.ServeHTTP(firstRecorder, firstReq)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first status = %d, want %d body=%s", firstRecorder.Code, http.StatusOK, firstRecorder.Body.String())
	}

	if _, ok := resolver.sticky["codex|gpt-5.4|previous_response_id:resp_123"]; !ok {
		t.Fatalf("expected sticky mapping for previous_response_id:resp_123, got %#v", resolver.sticky)
	}

	resolver.mu.Lock()
	resolver.ensureTokenLocked("codex", "yunyi-codex/gpt-5.4", now.Add(20*time.Minute))
	resolver.mu.Unlock()

	secondBody := `{"model":"codex/gpt-5.4","previous_response_id":"resp_123","parallel_tool_calls":true,"tool_choice":"auto","tools":[],"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(secondBody))
	secondReq.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	engine.ServeHTTP(secondRecorder, secondReq)

	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d body=%s", secondRecorder.Code, http.StatusOK, secondRecorder.Body.String())
	}
	if got := gjson.Get(secondRecorder.Body.String(), "cpams_model").String(); got != "sub2api/gpt-5.4(xhigh)" {
		t.Fatalf("cpams_model = %q, want %q", got, "sub2api/gpt-5.4(xhigh)")
	}
	if got := gjson.Get(secondRecorder.Body.String(), "cpams_decision").String(); got != "reuse_sticky_selection" {
		t.Fatalf("cpams_decision = %q, want %q", got, "reuse_sticky_selection")
	}
}
