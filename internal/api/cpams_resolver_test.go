package api

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
	log "github.com/sirupsen/logrus"
)

type rawLogMessageFormatter struct{}

func (rawLogMessageFormatter) Format(entry *log.Entry) ([]byte, error) {
	return append([]byte(entry.Message), '\n'), nil
}

func TestCPAMSResolver_MergeCodexChannelsPickBestModel(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 3, 5, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-03T05:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "models": [
        {"model": "sub2api/gpt-5.3-codex", "status": "green"},
        {"model": "sub2api/gpt-4.1", "status": "red"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "models": [
        {"model": "yunyi-codex/gpt-5.3-codex", "status": "green"},
        {"model": "yunyi-codex/gpt-4.1", "status": "green"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	got, err := resolver.Resolve("codex")
	if err != nil {
		t.Fatalf("Resolve(codex) error = %v", err)
	}
	if got != "sub2api/gpt-5.3-codex" {
		t.Fatalf("Resolve(codex) = %q, want %q", got, "sub2api/gpt-5.3-codex")
	}
}

func TestCPAMSResolver_FallbackToExistingTokenWhenSnapshotAllRed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 3, 5, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-03T05:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "models": [
        {"model": "sub2api/gpt-5.3-codex", "status": "red"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "models": [
        {"model": "yunyi-codex/gpt-5.3-codex", "status": "red"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath: snapshotPath,
		tokenTTL:     15 * time.Minute,
		channelTokens: map[string]map[string]time.Time{
			"codex": {
				"sub2api/gpt-5.3-codex": now.Add(10 * time.Minute),
			},
		},
		selected: make(map[string]cpamsSelection),
		now:      func() time.Time { return now },
	}

	got, err := resolver.Resolve("codex")
	if err != nil {
		t.Fatalf("Resolve(codex) error = %v", err)
	}
	if got != "sub2api/gpt-5.3-codex" {
		t.Fatalf("Resolve(codex) = %q, want %q", got, "sub2api/gpt-5.3-codex")
	}
}

func TestCPAMSResolver_TokenTTLRespected(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 3, 5, 0, 0, 0, time.UTC)
	now := base
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-03T05:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "models": [
        {"model": "sub2api/gpt-5.3-codex", "status": "green"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	first, err := resolver.Resolve("codex")
	if err != nil {
		t.Fatalf("first Resolve(codex) error = %v", err)
	}
	if first != "sub2api/gpt-5.3-codex" {
		t.Fatalf("first Resolve(codex) = %q, want %q", first, "sub2api/gpt-5.3-codex")
	}

	// Snapshot degrades to all-red, but token still valid so we should keep using the cached model.
	writeSnapshotAtPath(t, snapshotPath, `{
  "updated_at": "2026-03-03T05:05:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "models": [
        {"model": "sub2api/gpt-5.3-codex", "status": "red"}
      ]
    }
  ]
}`)

	now = base.Add(5 * time.Minute)
	second, err := resolver.Resolve("codex")
	if err != nil {
		t.Fatalf("second Resolve(codex) error = %v", err)
	}
	if second != "sub2api/gpt-5.3-codex" {
		t.Fatalf("second Resolve(codex) = %q, want %q", second, "sub2api/gpt-5.3-codex")
	}

	// Token expired and snapshot still unhealthy -> expect failure.
	now = base.Add(16 * time.Minute)
	if _, err := resolver.Resolve("codex"); err == nil {
		t.Fatalf("third Resolve(codex) error = nil, want non-nil after token expiry on unhealthy snapshot")
	}
}

func TestBuildCPAMSCandidates_PreferStableMergedModel(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 3, 5, 0, 0, 0, time.UTC)
	snapshot := &cpamsMultiSnapshot{
		UpdatedAt: "2026-03-03T05:00:00Z",
		Entries: []cpamsMultiEntry{
			{
				UpdatedAt:      "2026-03-03T05:00:01Z",
				ProviderPrefix: "sub2api",
				Summary:        cpamsProbeSummary{Total: 10, Green: 9, Yellow: 1, Red: 0},
				Models: []cpamsModelHealth{
					{
						Model:    "sub2api/gpt-5.3-codex",
						Status:   "green",
						Reason:   "ok",
						TestedAt: "2026-03-03T05:00:01Z",
					},
				},
			},
			{
				UpdatedAt:      "2026-03-03T05:00:01Z",
				ProviderPrefix: "yunyi-codex",
				Summary:        cpamsProbeSummary{Total: 10, Green: 0, Yellow: 1, Red: 9},
				Models: []cpamsModelHealth{
					{
						Model:    "yunyi-codex/gpt-5.3-codex",
						Status:   "yellow",
						Reason:   "rate_limited",
						TestedAt: "2026-03-03T05:00:01Z",
					},
					{
						Model:    "yunyi-codex/gpt-4.1",
						Status:   "green",
						Reason:   "timeout_or_network_error",
						TestedAt: "2026-03-03T05:00:01Z",
					},
				},
			},
		},
	}

	candidates := buildCPAMSCandidates("codex", snapshot, now)
	if len(candidates) == 0 {
		t.Fatalf("buildCPAMSCandidates(codex) returned no candidates")
	}
	if candidates[0].FullModel != "sub2api/gpt-5.3-codex" {
		t.Fatalf("top candidate = %q, want %q", candidates[0].FullModel, "sub2api/gpt-5.3-codex")
	}
}

func TestBuildCPAMSCandidates_IgnoresDisabledEntries(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 2, 21, 20, 0, 0, time.UTC)
	snapshot := &cpamsMultiSnapshot{
		UpdatedAt: "2026-04-02T21:20:23Z",
		Entries: []cpamsMultiEntry{
			{
				UpdatedAt:      "2026-04-02T21:20:07Z",
				ProviderPrefix: "whitedream-codex",
				Disabled:       true,
				Summary:        cpamsProbeSummary{Total: 0, Green: 0, Yellow: 0, Red: 0},
				Models: []cpamsModelHealth{
					{
						Model:    "whitedream-codex/gpt-5.4",
						Status:   "green",
						Reason:   "ok",
						TestedAt: "2026-04-02T21:20:07Z",
					},
				},
			},
			{
				UpdatedAt:      "2026-04-02T21:20:08Z",
				ProviderPrefix: "yunyi-codex",
				Summary:        cpamsProbeSummary{Total: 1, Green: 1, Yellow: 0, Red: 0},
				Models: []cpamsModelHealth{
					{
						Model:    "yunyi-codex/gpt-5.3-codex",
						Status:   "green",
						Reason:   "ok",
						TestedAt: "2026-04-02T21:20:08Z",
					},
				},
			},
		},
	}

	candidates := buildCPAMSCandidates("codex", snapshot, now)
	if len(candidates) != 1 {
		t.Fatalf("candidate len = %d, want 1", len(candidates))
	}
	if candidates[0].FullModel != "yunyi-codex/gpt-5.3-codex" {
		t.Fatalf("top candidate = %q, want yunyi-codex/gpt-5.3-codex", candidates[0].FullModel)
	}
}

func TestCPAMSResolver_SnapshotIncludesDecisionAndTokens(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 3, 5, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-03T05:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "models": [
        {"model": "sub2api/gpt-5.3-codex", "status": "green", "reason": "ok", "tested_at": "2026-03-03T05:00:01Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	selectedModel, err := resolver.Resolve("codex")
	if err != nil {
		t.Fatalf("Resolve(codex) error = %v", err)
	}

	states, err := resolver.Snapshot("codex")
	if err != nil {
		t.Fatalf("Snapshot(codex) error = %v", err)
	}

	state, ok := states["codex"]
	if !ok {
		t.Fatalf("Snapshot(codex) missing codex channel")
	}
	if state.Selection.Model != selectedModel {
		t.Fatalf("selection.model = %q, want %q", state.Selection.Model, selectedModel)
	}
	if state.Selection.Decision != "issue_new_token_for_healthy_candidate" {
		t.Fatalf("selection.decision = %q, want %q", state.Selection.Decision, "issue_new_token_for_healthy_candidate")
	}
	if len(state.Tokens) != 1 {
		t.Fatalf("len(tokens) = %d, want 1", len(state.Tokens))
	}
	if state.Tokens[0].Model != selectedModel {
		t.Fatalf("token.model = %q, want %q", state.Tokens[0].Model, selectedModel)
	}
	if state.Tokens[0].ExpiresInSec <= 0 {
		t.Fatalf("token.expires_in_sec = %d, want >0", state.Tokens[0].ExpiresInSec)
	}
}

func TestCPAMSResolver_RuntimeOutageSkipsCandidateUntilNextProbeRefresh(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T09:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "models": [
        {"model": "sub2api/gpt-5.3-codex", "status": "green", "tested_at": "2026-03-31T09:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "models": [
        {"model": "yunyi-codex/gpt-5.3-codex", "status": "green", "tested_at": "2026-03-31T09:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky:        make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	resolver.SetRuntimeOutageSource(func(context.Context) []health.RuntimeOutageSnapshotItem {
		return []health.RuntimeOutageSnapshotItem{
			{
				Prefix:   "sub2api",
				Provider: "codex",
				Model:    "gpt-5.3-codex",
				FailedAt: now,
				Reason:   "upstream_502",
			},
		}
	})

	first, err := resolver.ResolveRequested("codex", "gpt-5.3-codex", "conv-1", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested() with active outage error = %v", err)
	}
	if first.Model != "yunyi-codex/gpt-5.3-codex" {
		t.Fatalf("ResolveRequested() with active outage = %q, want %q", first.Model, "yunyi-codex/gpt-5.3-codex")
	}

	writeSnapshotAtPath(t, snapshotPath, `{
  "updated_at": "2026-03-31T11:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "models": [
        {"model": "sub2api/gpt-5.3-codex", "status": "green", "tested_at": "2026-03-31T11:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "models": [
        {"model": "yunyi-codex/gpt-5.3-codex", "status": "green", "tested_at": "2026-03-31T11:00:00Z"}
      ]
    }
  ]
}`)

	refreshedResolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky:        make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	refreshedResolver.SetRuntimeOutageSource(func(context.Context) []health.RuntimeOutageSnapshotItem {
		return []health.RuntimeOutageSnapshotItem{
			{
				Prefix:   "sub2api",
				Provider: "codex",
				Model:    "gpt-5.3-codex",
				FailedAt: now,
				Reason:   "upstream_502",
			},
		}
	})

	second, err := refreshedResolver.ResolveRequested("codex", "gpt-5.3-codex", "conv-2", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested() after probe refresh error = %v", err)
	}
	if second.Model != "sub2api/gpt-5.3-codex" {
		t.Fatalf("ResolveRequested() after probe refresh = %q, want %q", second.Model, "sub2api/gpt-5.3-codex")
	}
}

func TestCPAMSResolver_ClaudeBridgeCandidateBypassesRuntimeOutage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 21, 37, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T20:25:17Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "claude cheep",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {
          "model": "claude cheep/claude-opus-4-6",
          "upstream_model": "[0.05次]-claude-opus-4.6",
          "response_model": "gpt-5.4",
          "status": "green",
          "reason": "text_only_ok",
          "tested_at": "2026-04-10T20:25:17Z"
        }
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky:        make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	resolver.SetRuntimeOutageSource(func(context.Context) []health.RuntimeOutageSnapshotItem {
		return []health.RuntimeOutageSnapshotItem{
			{
				Prefix:   "claude cheep",
				Provider: "claude",
				Model:    "claude-opus-4-6",
				FailedAt: now,
				Reason:   "timeout awaiting response headers",
			},
		}
	})

	resolution, err := resolver.ResolveRequested("claude", "claude-opus-4-6", "", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(claude bridge outage bypass) error = %v", err)
	}
	if resolution.Model != "claude cheep/claude-opus-4-6" {
		t.Fatalf("ResolveRequested(claude bridge outage bypass) = %q, want %q", resolution.Model, "claude cheep/claude-opus-4-6")
	}
}

func TestCPAMSResolver_ClaudeBridgeCandidateFallbacksWhenHealthyPoolIsEmpty(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 21, 43, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T20:25:17Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "claude cheep",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {
          "model": "claude cheep/claude-opus-4-6",
          "upstream_model": "[0.05次]-claude-opus-4.6",
          "response_model": "gpt-5.4",
          "status": "green",
          "reason": "text_only_ok",
          "tested_at": "2026-04-10T20:25:17Z"
        }
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky:        make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("claude", "claude-opus-4-6", "", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(claude bridge fallback) error = %v", err)
	}
	if resolution.Model != "claude cheep/claude-opus-4-6" {
		t.Fatalf("ResolveRequested(claude bridge fallback) = %q, want %q", resolution.Model, "claude cheep/claude-opus-4-6")
	}
	if resolution.Decision != "issue_new_token_health_aware_same_model" && resolution.Decision != "single_candidate_fallback_same_model" {
		t.Fatalf("decision = %q, want issue_new_token_health_aware_same_model or single_candidate_fallback_same_model", resolution.Decision)
	}
}

func TestCPAMSResolver_SingleCandidateFallbackWhenHealthyPoolIsEmpty(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 21, 52, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T20:25:17Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "solo",
      "summary": {"total": 1, "green": 0, "yellow": 0, "red": 1},
      "models": [
        {"model": "solo/claude-opus-4-6", "status": "red", "reason": "timeout", "tested_at": "2026-04-10T20:25:17Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky:        make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("claude", "claude-opus-4-6", "", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(single candidate fallback) error = %v", err)
	}
	if resolution.Model != "solo/claude-opus-4-6" {
		t.Fatalf("ResolveRequested(single candidate fallback) = %q, want %q", resolution.Model, "solo/claude-opus-4-6")
	}
	if resolution.Decision != "single_candidate_fallback_same_model" {
		t.Fatalf("decision = %q, want single_candidate_fallback_same_model", resolution.Decision)
	}
}

func TestCPAMSHealthyCandidates_BinaryHealthPolicyKeepsOnlyGreen(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	settings := cpamsSoftRankSettings{
		Enabled:                   true,
		YellowScoreDelta:          10,
		YellowMaxShare:            1,
		StatefulAllowYellow:       false,
		MinGreenCountForExclusive: 2,
		YellowFreshnessWindow:     10 * time.Minute,
	}

	candidates := []cpamsCandidate{
		{
			FullModel:      "sub2api/gpt-5.4",
			StatusRank:     cpamsStatusGreen,
			UnifiedScore:   82,
			UnifiedStatus:  "healthy",
			LatestTestedAt: now,
		},
		{
			FullModel:      "yunyi-codex/gpt-5.4",
			StatusRank:     cpamsStatusYellow,
			UnifiedScore:   76,
			UnifiedStatus:  "degraded",
			LatestTestedAt: now.Add(-2 * time.Minute),
		},
	}

	healthy := cpamsHealthyCandidates(candidates, now, settings, false)
	if len(healthy) != 1 {
		t.Fatalf("len(healthy) = %d, want 1", len(healthy))
	}
	if healthy[0].FullModel != "sub2api/gpt-5.4" {
		t.Fatalf("healthy[0] = %q, want green candidate first", healthy[0].FullModel)
	}
}

func TestCPAMSHealthyCandidatesDetailed_BinaryHealthPolicyRejectsYellow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	settings := cpamsSoftRankSettings{
		Enabled:                   true,
		YellowScoreDelta:          10,
		YellowMaxShare:            1,
		StatefulAllowYellow:       false,
		MinGreenCountForExclusive: 2,
		YellowFreshnessWindow:     10 * time.Minute,
	}

	candidates := []cpamsCandidate{
		{
			FullModel:      "sub2api/gpt-5.4",
			StatusRank:     cpamsStatusGreen,
			UnifiedScore:   82,
			UnifiedStatus:  "healthy",
			LatestTestedAt: now,
		},
		{
			FullModel:      "yunyi-codex/gpt-5.4",
			StatusRank:     cpamsStatusYellow,
			UnifiedScore:   76,
			UnifiedStatus:  "degraded",
			LatestTestedAt: now.Add(-2 * time.Minute),
		},
		{
			FullModel:      "openai/gpt-5.4",
			StatusRank:     cpamsStatusYellow,
			UnifiedScore:   65,
			UnifiedStatus:  "degraded",
			LatestTestedAt: now.Add(-1 * time.Minute),
		},
		{
			FullModel:      "ee/gpt-5.4",
			StatusRank:     cpamsStatusYellow,
			UnifiedScore:   78,
			UnifiedStatus:  "degraded",
			LatestTestedAt: now.Add(-15 * time.Minute),
		},
	}

	healthy, decision := cpamsHealthyCandidatesDetailed(candidates, now, settings, false)
	if len(healthy) != 1 {
		t.Fatalf("len(healthy) = %d, want 1", len(healthy))
	}
	if decision.GreenCount != 1 || decision.YellowCount != 3 {
		t.Fatalf("unexpected pool counts: %+v", decision)
	}
	if decision.BestGreenScore != 82 {
		t.Fatalf("decision.BestGreenScore = %v, want 82", decision.BestGreenScore)
	}
	if got := decision.CounterfactualBestWithoutRestriction; got != "sub2api/gpt-5.4" {
		t.Fatalf("decision.CounterfactualBestWithoutRestriction = %q, want %q", got, "sub2api/gpt-5.4")
	}
	if got := len(decision.YellowCandidatesAdmitted); got != 0 {
		t.Fatalf("decision.YellowCandidatesAdmitted len = %d, want 0", got)
	}
	if got := decision.YellowRejectReason["yunyi-codex/gpt-5.4"]; got != "binary_health_policy" {
		t.Fatalf("yunyi reject reason = %q, want %q", got, "binary_health_policy")
	}
	if got := decision.YellowRejectReason["openai/gpt-5.4"]; got != "binary_health_policy" {
		t.Fatalf("openai reject reason = %q, want %q", got, "binary_health_policy")
	}
	if got := decision.YellowRejectReason["ee/gpt-5.4"]; got != "binary_health_policy" {
		t.Fatalf("ee reject reason = %q, want %q", got, "binary_health_policy")
	}
	if got := decision.GreenExclusiveReason; got != "green_only_policy" {
		t.Fatalf("decision.GreenExclusiveReason = %q, want %q", got, "green_only_policy")
	}
	if got := decision.YellowFreshnessSeconds["yunyi-codex/gpt-5.4"]; got != 120 {
		t.Fatalf("yellow freshness = %d, want 120", got)
	}
}

func TestCPAMSHealthyCandidates_RejectsStaleYellow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	settings := cpamsSoftRankSettings{
		Enabled:                   true,
		YellowScoreDelta:          10,
		YellowMaxShare:            1,
		StatefulAllowYellow:       false,
		MinGreenCountForExclusive: 2,
		YellowFreshnessWindow:     10 * time.Minute,
	}

	candidates := []cpamsCandidate{
		{
			FullModel:      "sub2api/gpt-5.4",
			StatusRank:     cpamsStatusGreen,
			UnifiedScore:   82,
			UnifiedStatus:  "healthy",
			LatestTestedAt: now,
		},
		{
			FullModel:      "yunyi-codex/gpt-5.4",
			StatusRank:     cpamsStatusYellow,
			UnifiedScore:   80,
			UnifiedStatus:  "degraded",
			LatestTestedAt: now.Add(-11 * time.Minute),
		},
	}

	healthy := cpamsHealthyCandidates(candidates, now, settings, false)
	if len(healthy) != 1 {
		t.Fatalf("len(healthy) = %d, want 1", len(healthy))
	}
	if healthy[0].StatusRank != cpamsStatusGreen {
		t.Fatalf("healthy[0].StatusRank = %d, want green only", healthy[0].StatusRank)
	}
}

func TestCPAMSHealthyCandidates_StatefulRequestsStayGreenOnlyByDefault(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	settings := cpamsSoftRankSettings{
		Enabled:                   true,
		YellowScoreDelta:          10,
		YellowMaxShare:            1,
		StatefulAllowYellow:       false,
		MinGreenCountForExclusive: 2,
		YellowFreshnessWindow:     10 * time.Minute,
	}

	candidates := []cpamsCandidate{
		{
			FullModel:      "sub2api/gpt-5.4",
			StatusRank:     cpamsStatusGreen,
			UnifiedScore:   82,
			UnifiedStatus:  "healthy",
			LatestTestedAt: now,
		},
		{
			FullModel:      "yunyi-codex/gpt-5.4",
			StatusRank:     cpamsStatusYellow,
			UnifiedScore:   81,
			UnifiedStatus:  "degraded",
			LatestTestedAt: now.Add(-2 * time.Minute),
		},
	}

	healthy := cpamsHealthyCandidates(candidates, now, settings, true)
	if len(healthy) != 1 {
		t.Fatalf("len(healthy) = %d, want 1", len(healthy))
	}
	if healthy[0].FullModel != "sub2api/gpt-5.4" {
		t.Fatalf("healthy[0] = %q, want green candidate", healthy[0].FullModel)
	}
}

func TestCPAMSHealthyCandidatesDetailed_StatefulRequestsRecordGreenExclusiveReason(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	settings := cpamsSoftRankSettings{
		Enabled:                   true,
		YellowScoreDelta:          10,
		YellowMaxShare:            1,
		StatefulAllowYellow:       false,
		MinGreenCountForExclusive: 2,
		YellowFreshnessWindow:     10 * time.Minute,
	}

	candidates := []cpamsCandidate{
		{
			FullModel:      "sub2api/gpt-5.4",
			StatusRank:     cpamsStatusGreen,
			UnifiedScore:   82,
			UnifiedStatus:  "healthy",
			LatestTestedAt: now,
		},
		{
			FullModel:      "yunyi-codex/gpt-5.4",
			StatusRank:     cpamsStatusYellow,
			UnifiedScore:   81,
			UnifiedStatus:  "degraded",
			LatestTestedAt: now.Add(-2 * time.Minute),
		},
	}

	healthy, decision := cpamsHealthyCandidatesDetailed(candidates, now, settings, true)
	if len(healthy) != 1 {
		t.Fatalf("len(healthy) = %d, want 1", len(healthy))
	}
	if got := decision.GreenExclusiveReason; got != "stateful_request" {
		t.Fatalf("decision.GreenExclusiveReason = %q, want %q", got, "stateful_request")
	}
	if got := decision.YellowRejectReason["yunyi-codex/gpt-5.4"]; got != "stateful_request" {
		t.Fatalf("yellow reject reason = %q, want %q", got, "stateful_request")
	}
}

func TestCPAMSHealthyCandidates_KeepsGreenExclusiveWhenGreenPoolIsLargeEnough(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	settings := cpamsSoftRankSettings{
		Enabled:                   true,
		YellowScoreDelta:          10,
		YellowMaxShare:            1,
		StatefulAllowYellow:       false,
		MinGreenCountForExclusive: 2,
		YellowFreshnessWindow:     10 * time.Minute,
	}

	candidates := []cpamsCandidate{
		{
			FullModel:      "sub2api/gpt-5.4",
			StatusRank:     cpamsStatusGreen,
			UnifiedScore:   84,
			UnifiedStatus:  "healthy",
			LatestTestedAt: now,
		},
		{
			FullModel:      "openai/gpt-5.4",
			StatusRank:     cpamsStatusGreen,
			UnifiedScore:   80,
			UnifiedStatus:  "healthy",
			LatestTestedAt: now,
		},
		{
			FullModel:      "yunyi-codex/gpt-5.4",
			StatusRank:     cpamsStatusYellow,
			UnifiedScore:   79,
			UnifiedStatus:  "degraded",
			LatestTestedAt: now.Add(-2 * time.Minute),
		},
	}

	healthy := cpamsHealthyCandidates(candidates, now, settings, false)
	if len(healthy) != 2 {
		t.Fatalf("len(healthy) = %d, want 2", len(healthy))
	}
	for _, candidate := range healthy {
		if candidate.StatusRank != cpamsStatusGreen {
			t.Fatalf("healthy contains non-green candidate: %+v", candidate)
		}
	}
}

func TestCPAMSResolver_ResolveRequested_EmitsDecisionLog(t *testing.T) {
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 0, "yellow": 1, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "yellow", "reason": "slow", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 0, "yellow": 1, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "yellow", "reason": "slow", "tested_at": "2026-03-31T11:58:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky:        make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	originalOut := log.StandardLogger().Out
	originalFormatter := log.StandardLogger().Formatter
	originalLevel := log.StandardLogger().Level
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFormatter(rawLogMessageFormatter{})
	log.SetLevel(log.InfoLevel)
	defer func() {
		log.SetOutput(originalOut)
		log.SetFormatter(originalFormatter)
		log.SetLevel(originalLevel)
	}()

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4", "conv-log", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested() error = %v", err)
	}
	if resolution.Model == "" {
		t.Fatal("ResolveRequested() returned empty model")
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		t.Fatalf("expected decision log line, got %q", buf.String())
	}

	var payload cpamsRouteDecisionLog
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &payload); err != nil {
		t.Fatalf("failed to parse decision log JSON: %v\nraw=%s", err, lines[len(lines)-1])
	}
	if payload.Event != "cpams_route_decision" {
		t.Fatalf("payload.Event = %q, want %q", payload.Event, "cpams_route_decision")
	}
	if payload.RequestChannel != "codex" {
		t.Fatalf("payload.RequestChannel = %q, want %q", payload.RequestChannel, "codex")
	}
	if payload.RequestedModel != "gpt-5.4" {
		t.Fatalf("payload.RequestedModel = %q, want %q", payload.RequestedModel, "gpt-5.4")
	}
	if payload.GreenCount != 0 || payload.YellowCount != 2 {
		t.Fatalf("unexpected pool counts in payload: %+v", payload)
	}
	if payload.FinalPoolSize != 2 {
		t.Fatalf("payload.FinalPoolSize = %d, want 2", payload.FinalPoolSize)
	}
	if len(payload.YellowCandidatesAdmitted) != 2 {
		t.Fatalf("payload.YellowCandidatesAdmitted = %#v, want both yellow candidates admitted", payload.YellowCandidatesAdmitted)
	}
	if got := payload.YellowAdmitReason["sub2api/gpt-5.4"]; got != "no_green_candidates" {
		t.Fatalf("sub2api admit reason = %q, want %q", got, "no_green_candidates")
	}
	if got := payload.YellowAdmitReason["yunyi-codex/gpt-5.4"]; got != "no_green_candidates" {
		t.Fatalf("yunyi admit reason = %q, want %q", got, "no_green_candidates")
	}
	if payload.SelectedCandidate == "" {
		t.Fatal("payload.SelectedCandidate is empty")
	}
	if payload.Decision == "" {
		t.Fatal("payload.Decision is empty")
	}
}

func TestCPAMSResolver_ResolveRequested_PreservesExactModel(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-06T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 2, "green": 2, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"},
        {"model": "sub2api/gpt-5.2", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "fill-first", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(codex, gpt-5.4-pro) error = %v", err)
	}
	if got := cpamsCanonicalRequestedModel(resolution.Model); got != "sub2api/gpt-5.4-pro" {
		t.Fatalf("resolved model = %q, want same exact model family %q", resolution.Model, "sub2api/gpt-5.4-pro")
	}
	if strings.Contains(resolution.Model, "gpt-5.2") {
		t.Fatalf("resolved model = %q, should not downgrade to gpt-5.2", resolution.Model)
	}
	if resolution.Decision != "issue_new_token_fill_first_same_model" {
		t.Fatalf("decision = %q, want %q", resolution.Decision, "issue_new_token_fill_first_same_model")
	}
}

func TestCPAMSResolver_ResolveRequested_RoundRobinSameModel(t *testing.T) {
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
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	first, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "round-robin", "", false)
	if err != nil {
		t.Fatalf("first ResolveRequested error = %v", err)
	}
	second, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "round-robin", "", false)
	if err != nil {
		t.Fatalf("second ResolveRequested error = %v", err)
	}
	if first.Model != second.Model {
		t.Fatalf("resolver should reuse healthy model without conversation key, got %q then %q", first.Model, second.Model)
	}
	if second.Decision != "reuse_valid_token_same_model" {
		t.Fatalf("decision = %q, want %q", second.Decision, "reuse_valid_token_same_model")
	}
}

func TestCPAMSResolver_ResolveRequested_ReusesLoadedCandidateUntilItFails(t *testing.T) {
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
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath: snapshotPath,
		tokenTTL:     15 * time.Minute,
		channelTokens: map[string]map[string]time.Time{
			"codex": {
				"sub2api/gpt-5.4-pro": now.Add(10 * time.Minute),
			},
		},
		selected: make(map[string]cpamsSelection),
		sticky: map[string]cpamsSelection{
			"codex|gpt-5.4-pro|conv-a": {
				Model:          "sub2api/gpt-5.4-pro",
				RequestedModel: "gpt-5.4-pro",
				ExpiresAt:      now.Add(10 * time.Minute),
			},
		},
		now: func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "round-robin", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(codex, gpt-5.4-pro) error = %v", err)
	}
	if resolution.Model != "sub2api/gpt-5.4-pro" {
		t.Fatalf("resolved model = %q, want %q", resolution.Model, "sub2api/gpt-5.4-pro")
	}
	if resolution.Decision != "reuse_valid_token_same_model" {
		t.Fatalf("decision = %q, want %q", resolution.Decision, "reuse_valid_token_same_model")
	}
}

func TestCPAMSResolver_ResolveRequested_BalancesAwayFromLoadedCandidateWhenNoTokenExists(t *testing.T) {
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
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky: map[string]cpamsSelection{
			"codex|gpt-5.4-pro|conv-a": {
				Model:          "sub2api/gpt-5.4-pro",
				RequestedModel: "gpt-5.4-pro",
				ExpiresAt:      now.Add(10 * time.Minute),
			},
		},
		now: func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "round-robin", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(codex, gpt-5.4-pro) error = %v", err)
	}
	if resolution.Model != "yunyi-codex/gpt-5.4-pro" {
		t.Fatalf("resolved model = %q, want %q", resolution.Model, "yunyi-codex/gpt-5.4-pro")
	}
	if resolution.Decision != "issue_new_token_balanced_same_model" {
		t.Fatalf("decision = %q, want %q", resolution.Decision, "issue_new_token_balanced_same_model")
	}
}

func TestCPAMSResolver_ResolveRequested_HealthAwareStrategyUsesBestCandidate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-06T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 10, "green": 10, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 10, "green": 2, "yellow": 8, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "yellow", "reason": "rate_limited", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(codex, gpt-5.4-pro) error = %v", err)
	}
	if resolution.Model != "sub2api/gpt-5.4-pro" {
		t.Fatalf("resolved model = %q, want %q", resolution.Model, "sub2api/gpt-5.4-pro")
	}
	if resolution.Decision != "issue_new_token_health_aware_same_model" {
		t.Fatalf("decision = %q, want %q", resolution.Decision, "issue_new_token_health_aware_same_model")
	}
}

func TestCPAMSResolver_ResolveRequested_HealthAwareUsesBalanceSignal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-06T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 10, "green": 10, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 10, "green": 10, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}
	resolver.SetBalanceSource(func(_ context.Context) []health.ProviderBalanceSnapshotItem {
		return []health.ProviderBalanceSnapshotItem{
			{Prefix: "sub2api", ProviderKey: "codex", Status: "warning", Label: "余额紧张", UpdatedAt: now},
			{Prefix: "yunyi-codex", ProviderKey: "codex", Status: "ok", Label: "余额充足", UpdatedAt: now},
		}
	})

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(codex, gpt-5.4-pro) error = %v", err)
	}
	if resolution.Model != "yunyi-codex/gpt-5.4-pro" {
		t.Fatalf("resolved model = %q, want %q", resolution.Model, "yunyi-codex/gpt-5.4-pro")
	}
}

func TestCPAMSResolver_ResolveRequested_AutoIncludesOpenAIPrefixForCodexModels(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 12, 20, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-12T20:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 0, "yellow": 0, "red": 1},
      "models": [
        {"model": "yunyi-codex/gpt-5.3-codex", "status": "red", "reason": "timeout_or_network_error", "tested_at": "2026-03-12T20:00:00Z"}
      ]
    },
    {
      "provider_prefix": "openai",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "openai/gpt-5.3-codex", "status": "green", "reason": "ok", "tested_at": "2026-03-12T20:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.3-codex", "", "round-robin", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested(codex, gpt-5.3-codex) error = %v", err)
	}
	if got := cpamsCanonicalRequestedModel(resolution.Model); got != "openai/gpt-5.3-codex" {
		t.Fatalf("resolved model = %q, want %q", resolution.Model, "openai/gpt-5.3-codex")
	}
}

func TestCPAMSResolver_ResolveRequested_StickyConversationReusesRoute(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	now := base
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
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	first, err := resolver.ResolveRequested("codex", "gpt-5.4-pro(high)", "prompt_cache_key:conv-1", "round-robin", "", false)
	if err != nil {
		t.Fatalf("first ResolveRequested error = %v", err)
	}
	now = base.Add(5 * time.Minute)
	second, err := resolver.ResolveRequested("codex", "gpt-5.4-pro(high)", "prompt_cache_key:conv-1", "round-robin", "", false)
	if err != nil {
		t.Fatalf("second ResolveRequested error = %v", err)
	}
	if first.Model != second.Model {
		t.Fatalf("sticky conversation should reuse route, got %q then %q", first.Model, second.Model)
	}
	if second.Decision != "reuse_sticky_selection" {
		t.Fatalf("decision = %q, want %q", second.Decision, "reuse_sticky_selection")
	}
	if !strings.HasSuffix(first.Model, "(high)") {
		t.Fatalf("resolved model = %q, want thinking suffix preserved", first.Model)
	}
}

func TestCPAMSResolver_ResolveRequested_UnhealthyStickyIsDroppedImmediately(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	now := base
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-06T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 0, "yellow": 0, "red": 1},
      "models": [
        {"model": "sub2api/gpt-5.4-pro", "status": "red", "reason": "timeout_or_network_error", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky: map[string]cpamsSelection{
			"codex|gpt-5.4-pro|prompt_cache_key:conv-1": {
				Model:          "sub2api/gpt-5.4-pro",
				RequestedModel: "gpt-5.4-pro",
				ExpiresAt:      now.Add(10 * time.Minute),
			},
		},
		now: func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "prompt_cache_key:conv-1", "round-robin", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested error = %v", err)
	}
	if resolution.Model != "yunyi-codex/gpt-5.4-pro" {
		t.Fatalf("resolved model = %q, want %q", resolution.Model, "yunyi-codex/gpt-5.4-pro")
	}
	if resolution.Decision == "reuse_sticky_selection" {
		t.Fatalf("decision = %q, want sticky to be dropped when probe marks it unhealthy", resolution.Decision)
	}
}

func TestCPAMSResolver_ResolveRequested_ReusesHealthyTokenWithoutConversationKey(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	now := base
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
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	first, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "health-aware", "", false)
	if err != nil {
		t.Fatalf("first ResolveRequested error = %v", err)
	}
	now = base.Add(5 * time.Minute)
	second, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "health-aware", "", false)
	if err != nil {
		t.Fatalf("second ResolveRequested error = %v", err)
	}
	if first.Model != second.Model {
		t.Fatalf("resolver should keep same upstream while healthy, got %q then %q", first.Model, second.Model)
	}
	if second.Decision != "reuse_valid_token_same_model" {
		t.Fatalf("decision = %q, want %q", second.Decision, "reuse_valid_token_same_model")
	}
}

func TestCPAMSResolver_ResolveRequested_SkipsGlobalTokenReuseWhenConversationKeyPresent(t *testing.T) {
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
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath: snapshotPath,
		tokenTTL:     15 * time.Minute,
		channelTokens: map[string]map[string]time.Time{
			"codex": {
				"sub2api/gpt-5.4-pro": now.Add(10 * time.Minute),
			},
		},
		selected: make(map[string]cpamsSelection),
		sticky:   make(map[string]cpamsSelection),
		now:      func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "session_affinity_seed:fallback:user-a", "health-aware", "", false)
	if err != nil {
		t.Fatalf("ResolveRequested error = %v", err)
	}
	if resolution.Decision == "reuse_valid_token_same_model" {
		t.Fatalf("decision = %q, want fresh sticky selection for conversation-scoped routing", resolution.Decision)
	}
}

func TestCPAMSResolver_ResolveRequested_SwitchesWhenReusedTokenBecomesUnhealthy(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)
	now := base
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
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	first, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "fill-first", "", false)
	if err != nil {
		t.Fatalf("first ResolveRequested error = %v", err)
	}
	if first.Model != "sub2api/gpt-5.4-pro" {
		t.Fatalf("first model = %q, want %q", first.Model, "sub2api/gpt-5.4-pro")
	}

	writeSnapshotAtPath(t, snapshotPath, `{
  "updated_at": "2026-03-06T12:05:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 0, "yellow": 0, "red": 1},
      "models": [
        {"model": "sub2api/gpt-5.4-pro", "status": "red", "reason": "timeout_or_network_error", "tested_at": "2026-03-06T12:05:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4-pro", "status": "green", "reason": "ok", "tested_at": "2026-03-06T12:05:00Z"}
      ]
    }
  ]
}`)

	now = base.Add(5 * time.Minute)
	second, err := resolver.ResolveRequested("codex", "gpt-5.4-pro", "", "fill-first", "", false)
	if err != nil {
		t.Fatalf("second ResolveRequested error = %v", err)
	}
	if second.Model != "yunyi-codex/gpt-5.4-pro" {
		t.Fatalf("second model = %q, want %q", second.Model, "yunyi-codex/gpt-5.4-pro")
	}
	if second.Decision == "reuse_valid_token_same_model" {
		t.Fatalf("decision = %q, want resolver to switch away from unhealthy token", second.Decision)
	}
}

func TestCPAMSResolver_ResolveRequested_HealthAwareDoesNotReuseNonTopYellowToken(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 0, "yellow": 1, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "yellow", "reason": "responses_bootstrap_failed", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 0, "yellow": 1, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "yellow", "reason": "responses_bootstrap_failed", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath: snapshotPath,
		tokenTTL:     15 * time.Minute,
		channelTokens: map[string]map[string]time.Time{
			"codex": {
				"yunyi-codex/gpt-5.4": now.Add(10 * time.Minute),
			},
		},
		selected: make(map[string]cpamsSelection),
		now:      func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4", "", "health-aware", "responses", false)
	if err != nil {
		t.Fatalf("ResolveRequested() error = %v", err)
	}
	if resolution.Model != "sub2api/gpt-5.4" {
		t.Fatalf("resolution model = %q, want %q", resolution.Model, "sub2api/gpt-5.4")
	}
	if resolution.Decision != "issue_new_token_health_aware_same_model" {
		t.Fatalf("decision = %q, want %q", resolution.Decision, "issue_new_token_health_aware_same_model")
	}
}

func TestCPAMSPrefixPriority_ClaudeProvidersArePeerTier(t *testing.T) {
	t.Parallel()

	pairs := [][2]string{
		{"yunyi-claude", "鹤辞ai"},
		{"yunyi-claude", "covs"},
		{"cc", "鹤辞ai"},
	}
	for _, pair := range pairs {
		left := cpamsPrefixPriority("claude", pair[0])
		right := cpamsPrefixPriority("claude", pair[1])
		if left != right {
			t.Fatalf("cpamsPrefixPriority(claude, %q)=%d, cpamsPrefixPriority(claude, %q)=%d, want peer tier", pair[0], left, pair[1], right)
		}
	}
}

func TestCPAMSResolver_InvalidateModelClearsCachedState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	resolver := &cpamsResolver{
		tokenTTL: 15 * time.Minute,
		channelTokens: map[string]map[string]time.Time{
			"codex": {
				"yunyi-codex/gpt-5.4": now.Add(10 * time.Minute),
			},
		},
		sticky: map[string]cpamsSelection{
			"codex|gpt-5.4|prompt_cache_key:conv-1": {
				Model:          "yunyi-codex/gpt-5.4",
				RequestedModel: "gpt-5.4",
				ExpiresAt:      now.Add(10 * time.Minute),
			},
		},
		selected: map[string]cpamsSelection{
			"codex": {
				Model:          "yunyi-codex/gpt-5.4",
				RequestedModel: "gpt-5.4",
				ExpiresAt:      now.Add(10 * time.Minute),
			},
		},
		now: func() time.Time { return now },
	}

	resolver.InvalidateModel("codex", "gpt-5.4", "yunyi-codex/gpt-5.4(xhigh)", "prompt_cache_key:conv-1", now)

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
	if len(resolver.sticky) != 0 {
		t.Fatalf("sticky entries = %d, want 0", len(resolver.sticky))
	}
	if len(resolver.failed) != 1 {
		t.Fatalf("failure blocks = %d, want 1", len(resolver.failed))
	}
}

func TestCPAMSResolver_InvalidateModelBlocksProviderUntilNextSnapshot(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	now := base
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
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
		failed:        make(map[string]cpamsFailureBlock),
		now:           func() time.Time { return now },
	}

	resolver.InvalidateModel("codex", "gpt-5.4", "sub2api/gpt-5.4", "", base)

	first, err := resolver.ResolveRequested("codex", "gpt-5.4", "", "health-aware", "responses", false)
	if err != nil {
		t.Fatalf("ResolveRequested() with active failure block error = %v", err)
	}
	if first.Model != "yunyi-codex/gpt-5.4" {
		t.Fatalf("model with active failure block = %q, want %q", first.Model, "yunyi-codex/gpt-5.4")
	}

	writeSnapshotAtPath(t, snapshotPath, `{
  "updated_at": "2026-03-31T12:05:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-31T12:05:00Z"}
      ]
    }
  ]
}`)
	now = base.Add(5 * time.Minute)

	second, err := resolver.ResolveRequested("codex", "gpt-5.4", "", "health-aware", "responses", false)
	if err != nil {
		t.Fatalf("ResolveRequested() after snapshot refresh error = %v", err)
	}
	if second.Model != "sub2api/gpt-5.4" {
		t.Fatalf("model after snapshot refresh = %q, want %q", second.Model, "sub2api/gpt-5.4")
	}
}

func TestCPAMSResolver_InvalidateModelRequiresCandidateRefresh(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	now := base
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
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
		failed:        make(map[string]cpamsFailureBlock),
		now:           func() time.Time { return now },
	}

	resolver.InvalidateModel("codex", "gpt-5.4", "sub2api/gpt-5.4", "", base)

	writeSnapshotAtPath(t, snapshotPath, `{
  "updated_at": "2026-03-31T12:05:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "yunyi-codex",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-31T12:05:00Z"}
      ]
    }
  ]
}`)
	now = base.Add(5 * time.Minute)

	first, err := resolver.ResolveRequested("codex", "gpt-5.4", "", "health-aware", "responses", false)
	if err != nil {
		t.Fatalf("ResolveRequested() with unrelated snapshot refresh error = %v", err)
	}
	if first.Model != "yunyi-codex/gpt-5.4" {
		t.Fatalf("model with stale failed candidate probe = %q, want %q", first.Model, "yunyi-codex/gpt-5.4")
	}

	writeSnapshotAtPath(t, snapshotPath, `{
  "updated_at": "2026-03-31T12:06:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-31T12:06:00Z"}
      ]
    }
  ]
}`)
	now = base.Add(6 * time.Minute)

	second, err := resolver.ResolveRequested("codex", "gpt-5.4", "", "health-aware", "responses", false)
	if err != nil {
		t.Fatalf("ResolveRequested() after candidate refresh error = %v", err)
	}
	if second.Model != "sub2api/gpt-5.4" {
		t.Fatalf("model after candidate refresh = %q, want %q", second.Model, "sub2api/gpt-5.4")
	}
}

func TestCPAMSResolver_CooldownModelTemporarilySkipsCandidate(t *testing.T) {
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
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
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
		failed:        make(map[string]cpamsFailureBlock),
		now:           func() time.Time { return now },
	}

	first, err := resolver.ResolveRequested("codex", "gpt-5.4", "", "health-aware", "responses", false)
	if err != nil {
		t.Fatalf("ResolveRequested() initial error = %v", err)
	}
	if first.Model != "sub2api/gpt-5.4" {
		t.Fatalf("initial model = %q, want %q", first.Model, "sub2api/gpt-5.4")
	}

	resolver.CooldownModel("codex", "gpt-5.4", first.Model, "", now, 5*time.Second)

	second, err := resolver.ResolveRequested("codex", "gpt-5.4", "", "health-aware", "responses", false)
	if err != nil {
		t.Fatalf("ResolveRequested() during cooldown error = %v", err)
	}
	if second.Model != "yunyi-codex/gpt-5.4" {
		t.Fatalf("model during cooldown = %q, want %q", second.Model, "yunyi-codex/gpt-5.4")
	}

	resolver.CooldownModel("codex", "gpt-5.4", second.Model, "", now, 10*time.Second)
	now = now.Add(6 * time.Second)

	third, err := resolver.ResolveRequested("codex", "gpt-5.4", "", "health-aware", "responses", false)
	if err != nil {
		t.Fatalf("ResolveRequested() after cooldown expiry error = %v", err)
	}
	if third.Model != "sub2api/gpt-5.4" {
		t.Fatalf("model after cooldown expiry = %q, want %q", third.Model, "sub2api/gpt-5.4")
	}
}

func TestCPAMSResolver_ResponsesRequireStatefulCandidate(t *testing.T) {
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
        {"model": "yunyi-codex/gpt-5.4", "status": "green", "reason": "text_only_ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
    {
      "provider_prefix": "sub2api",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "reason": "stateful_responses_ok", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4", "prompt_cache_key:conv-1", "health-aware", "responses", true)
	if err != nil {
		t.Fatalf("ResolveRequested(stateful responses) error = %v", err)
	}
	if resolution.Model != "sub2api/gpt-5.4" {
		t.Fatalf("ResolveRequested(stateful responses) = %q, want %q", resolution.Model, "sub2api/gpt-5.4")
	}
}

func TestCPAMSResolver_ResponsesAllowBridgeCapableCandidateWhenOnlyTextOnlyProviderExists(t *testing.T) {
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
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4", "prompt_cache_key:conv-1", "health-aware", "responses", true)
	if err != nil {
		t.Fatalf("ResolveRequested(stateful responses bridge fallback) error = %v", err)
	}
	if resolution.Model != "openai/gpt-5.4" {
		t.Fatalf("ResolveRequested(stateful responses bridge fallback) = %q, want %q", resolution.Model, "openai/gpt-5.4")
	}
}

func TestCPAMSResolver_ResponsesTreatsYunyiCodexAsStatefulCandidateWhenProbePasses(t *testing.T) {
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
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("codex", "gpt-5.4", "prompt_cache_key:conv-1", "health-aware", "responses", true)
	if err != nil {
		t.Fatalf("ResolveRequested(stateful responses via yunyi-codex) error = %v", err)
	}
	if resolution.Model != "yunyi-codex/gpt-5.4" {
		t.Fatalf("ResolveRequested(stateful responses via yunyi-codex) = %q, want %q", resolution.Model, "yunyi-codex/gpt-5.4")
	}
}

func TestCPAMSResolver_ClaudeExternalNativePrefersCompatibleCandidate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 18, 41, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T18:41:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "鹤辞ai",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "鹤辞ai/claude-opus-4-6", "status": "green", "reason": "text_only_ok", "tested_at": "2026-04-10T18:41:00Z"}
      ]
    },
    {
      "provider_prefix": "covs",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "covs/claude-opus-4-6", "status": "green", "reason": "text_only_ok", "tested_at": "2026-04-10T18:41:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("claude", "claude-opus-4-6", "", "health-aware", "chat/completions+claude-external-native", false)
	if err != nil {
		t.Fatalf("ResolveRequested(claude external native) error = %v", err)
	}
	if resolution.Model != "covs/claude-opus-4-6" {
		t.Fatalf("ResolveRequested(claude external native) = %q, want %q", resolution.Model, "covs/claude-opus-4-6")
	}
}

func TestCPAMSResolver_ClaudeExternalNativePrefersBridgeCandidateWhenAvailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 21, 56, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T20:25:17Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "claude cheep",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {
          "model": "claude cheep/claude-opus-4-6",
          "upstream_model": "[0.05次]-claude-opus-4.6",
          "response_model": "gpt-5.4",
          "status": "green",
          "reason": "text_only_ok",
          "tested_at": "2026-04-10T20:25:17Z"
        }
      ]
    },
    {
      "provider_prefix": "covs",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "covs/claude-opus-4-6", "status": "green", "reason": "text_only_ok", "tested_at": "2026-04-10T21:56:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		sticky:        make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("claude", "claude-opus-4-6", "", "health-aware", "chat/completions+claude-external-native", false)
	if err != nil {
		t.Fatalf("ResolveRequested(claude external native bridge) error = %v", err)
	}
	if resolution.Model != "claude cheep/claude-opus-4-6" {
		t.Fatalf("ResolveRequested(claude external native bridge) = %q, want %q", resolution.Model, "claude cheep/claude-opus-4-6")
	}
}

func TestCPAMSResolver_ClaudeExternalNativeDoesNotReviveStaleTokenOutsideCandidatePool(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 22, 34, 36, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T22:34:35Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "claude cheep",
      "summary": {"total": 1, "green": 0, "yellow": 0, "red": 1},
      "models": [
        {
          "model": "claude cheep/claude-opus-4-6",
          "upstream_model": "[0.05次]-claude-opus-4.6",
          "response_model": "gpt-5.4",
          "status": "red",
          "reason": "timeout awaiting response headers",
          "tested_at": "2026-04-10T22:34:35Z"
        }
      ]
    },
    {
      "provider_prefix": "covs",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "covs/claude-opus-4-6", "status": "green", "reason": "text_only_ok", "tested_at": "2026-04-10T22:34:35Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: map[string]map[string]time.Time{"claude": {"covs/claude-opus-4-6": now.Add(10 * time.Minute)}},
		selected:      make(map[string]cpamsSelection),
		sticky:        make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("claude", "claude-opus-4-6", "", "health-aware", "chat/completions+claude-external-native", false)
	if err != nil {
		t.Fatalf("ResolveRequested(claude external native stale token) error = %v", err)
	}
	if resolution.Model != "claude cheep/claude-opus-4-6" {
		t.Fatalf("ResolveRequested(claude external native stale token) = %q, want %q", resolution.Model, "claude cheep/claude-opus-4-6")
	}
}

func TestCPAMSResolver_ClaudeExternalNativeFallsBackWhenNoCompatibleCandidateExists(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 18, 41, 0, 0, time.UTC)
	snapshotPath := writeCPAMSSnapshot(t, `{
  "updated_at": "2026-04-10T18:41:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "provider_prefix": "鹤辞ai",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "鹤辞ai/claude-opus-4-6", "status": "green", "reason": "text_only_ok", "tested_at": "2026-04-10T18:41:00Z"}
      ]
    }
  ]
}`)

	resolver := &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      15 * time.Minute,
		channelTokens: make(map[string]map[string]time.Time),
		selected:      make(map[string]cpamsSelection),
		now:           func() time.Time { return now },
	}

	resolution, err := resolver.ResolveRequested("claude", "claude-opus-4-6", "", "health-aware", "chat/completions+claude-external-native", false)
	if err != nil {
		t.Fatalf("ResolveRequested(claude external native fallback) error = %v", err)
	}
	if resolution.Model != "鹤辞ai/claude-opus-4-6" {
		t.Fatalf("ResolveRequested(claude external native fallback) = %q, want %q", resolution.Model, "鹤辞ai/claude-opus-4-6")
	}
}

func TestBuildCPAMSRequestedCandidates_ClaudeResponseModelMismatchDowngradesToYellow(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 20, 25, 17, 0, time.UTC)
	snapshot := &cpamsMultiSnapshot{
		UpdatedAt: "2026-04-10T20:25:17Z",
		Entries: []cpamsMultiEntry{
			{
				UpdatedAt:      "2026-04-10T20:25:17Z",
				ProviderPrefix: "claude cheep",
				Summary:        cpamsProbeSummary{Total: 1, Green: 1, Yellow: 0, Red: 0},
				Models: []cpamsModelHealth{
					{
						Model:         "claude cheep/claude-opus-4-6",
						UpstreamModel: "[0.05次]-claude-opus-4.6",
						ResponseModel: "gpt-5.4",
						Status:        "green",
						Reason:        "text_only_ok",
						TestedAt:      "2026-04-10T20:25:17Z",
					},
				},
			},
		},
	}

	candidates := buildCPAMSRequestedCandidates("claude", "claude-opus-4-6", snapshot, now)
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	if candidates[0].StatusRank != cpamsStatusYellow {
		t.Fatalf("StatusRank = %d, want yellow(%d)", candidates[0].StatusRank, cpamsStatusYellow)
	}
	if !strings.Contains(candidates[0].Reason, "response_model_mismatch") {
		t.Fatalf("Reason = %q, want response_model_mismatch", candidates[0].Reason)
	}
}

func TestCPAMSYellowCandidateFreshEnough_BridgeCandidateBypassesFreshness(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 10, 21, 37, 0, 0, time.UTC)
	candidate := cpamsCandidate{
		FullModel:      "claude cheep/claude-opus-4-6",
		Reason:         "text_only_ok; response_model_mismatch",
		StatusRank:     cpamsStatusYellow,
		LatestTestedAt: now.Add(-30 * time.Minute),
	}
	if !cpamsYellowCandidateFreshEnough(now, candidate, 10*time.Minute) {
		t.Fatal("expected bridge candidate to bypass yellow freshness window")
	}
}

func TestCPAMSIsBridgeCandidate_KnownClaudeCheepPrefix(t *testing.T) {
	t.Parallel()

	candidate := cpamsCandidate{FullModel: "claude cheep/claude-opus-4-6", Reason: "text_only_ok"}
	if !cpamsIsBridgeCandidate(candidate) {
		t.Fatal("expected claude cheep prefix to be treated as bridge candidate")
	}
}

func writeCPAMSSnapshot(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "model-health-multi.json")
	writeSnapshotAtPath(t, path, body)
	return path
}

func writeSnapshotAtPath(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
