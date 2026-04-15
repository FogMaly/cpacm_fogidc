package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	log "github.com/sirupsen/logrus"
)

const (
	cpamsDefaultTokenTTL = 15 * time.Minute
	// Keep some stickiness when the pool is fully green, but force fresh routing
	// once the current pool has degraded and the cached token is no longer the top pick.
	cpamsHealthAwareReuseScoreSlack = 40
)

const (
	cpamsStatusUnknown = 0
	cpamsStatusRed     = 1
	cpamsStatusYellow  = 2
	cpamsStatusGreen   = 3
)

type cpamsResolver struct {
	snapshotPath string
	tokenTTL     time.Duration

	mu             sync.Mutex
	channelTokens  map[string]map[string]time.Time
	sticky         map[string]cpamsSelection
	selected       map[string]cpamsSelection
	failed         map[string]cpamsFailureBlock
	cursors        map[string]int
	now            func() time.Time
	balanceSource  func(context.Context) []health.ProviderBalanceSnapshotItem
	outageSource   func(context.Context) []health.RuntimeOutageSnapshotItem
	balanceCache   []health.ProviderBalanceSnapshotItem
	balanceCacheAt time.Time
}

type cpamsSelection struct {
	Model           string    `json:"model"`
	RequestedModel  string    `json:"requested_model,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	SnapshotUpdated time.Time `json:"snapshot_updated"`
	Decision        string    `json:"decision,omitempty"`
	CandidateCount  int       `json:"candidate_count,omitempty"`
	ChosenAt        time.Time `json:"chosen_at,omitempty"`
}

type cpamsFailureBlock struct {
	RequestedModel  string    `json:"requested_model,omitempty"`
	SnapshotUpdated time.Time `json:"snapshot_updated"`
	FailedAt        time.Time `json:"failed_at"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
}

type cpamsMultiSnapshot struct {
	UpdatedAt   string            `json:"updated_at"`
	IntervalSec int               `json:"interval_sec"`
	Entries     []cpamsMultiEntry `json:"entries"`
}

type cpamsMultiEntry struct {
	UpdatedAt      string             `json:"updated_at"`
	ProviderPrefix string             `json:"provider_prefix"`
	Disabled       bool               `json:"disabled,omitempty"`
	Summary        cpamsProbeSummary  `json:"summary"`
	Models         []cpamsModelHealth `json:"models"`
}

type cpamsProbeSummary struct {
	Total  int `json:"total"`
	Green  int `json:"green"`
	Yellow int `json:"yellow"`
	Red    int `json:"red"`
}

type cpamsModelHealth struct {
	Model                       string `json:"model"`
	UpstreamModel               string `json:"upstream_model"`
	ResponseModel               string `json:"response_model,omitempty"`
	TextFallbackResponseModel   string `json:"text_fallback_response_model,omitempty"`
	Status                      string `json:"status"`
	Reason                      string `json:"reason"`
	ResponsesContinuationStatus string `json:"responses_continuation_status,omitempty"`
	ResponsesContinuationReason string `json:"responses_continuation_reason,omitempty"`
	HTTP                        int    `json:"http"`
	TestedAt                    string `json:"tested_at"`
}

type cpamsAggregate struct {
	Upstream           string
	Score              int
	Observations       int
	Healthy            int
	Greens             int
	Yellows            int
	Reds               int
	NetworkFailures    int
	LatestTestedAt     time.Time
	BestFullModel      string
	BestStatusRank     int
	BestPrefixPriority int
}

type cpamsCandidate struct {
	FullModel                   string
	Upstream                    string
	Reason                      string
	ResponsesContinuationStatus string
	ResponsesContinuationReason string
	StatusRank                  int
	Score                       int
	UnifiedScore                float64
	UnifiedStatus               string
	PrefixPriority              int
	Observations                int
	Healthy                     int
	Greens                      int
	Yellows                     int
	Reds                        int
	NetworkFailures             int
	LatestTestedAt              time.Time
}

type cpamsSoftRankSettings struct {
	Enabled                   bool
	YellowScoreDelta          float64
	YellowMaxShare            float64
	StatefulAllowYellow       bool
	MinGreenCountForExclusive int
	YellowFreshnessWindow     time.Duration
}

type cpamsSoftRankDecision struct {
	SoftRankEnabled                      bool               `json:"soft_rank_enabled"`
	YellowScoreDeltaThreshold            float64            `json:"yellow_score_delta_threshold"`
	YellowMaxShare                       float64            `json:"yellow_max_share"`
	StatefulAllowYellow                  bool               `json:"stateful_allow_yellow"`
	MinGreenCountForExclusive            int                `json:"min_green_count_for_exclusive"`
	YellowFreshnessWindowSeconds         int64              `json:"yellow_freshness_window_seconds"`
	GreenCount                           int                `json:"green_count"`
	YellowCount                          int                `json:"yellow_count"`
	BestGreenScore                       float64            `json:"best_green_score"`
	YellowScoreDelta                     map[string]float64 `json:"yellow_score_delta,omitempty"`
	YellowFreshnessSeconds               map[string]int64   `json:"yellow_freshness_seconds,omitempty"`
	YellowCandidatesAdmitted             []string           `json:"yellow_candidates_admitted,omitempty"`
	YellowCandidatesRejected             []string           `json:"yellow_candidates_rejected,omitempty"`
	YellowAdmitReason                    map[string]string  `json:"yellow_admit_reason,omitempty"`
	YellowRejectReason                   map[string]string  `json:"yellow_reject_reason,omitempty"`
	GreenExclusiveReason                 string             `json:"green_exclusive_reason,omitempty"`
	FinalPoolSize                        int                `json:"final_pool_size"`
	CounterfactualBestWithoutRestriction string             `json:"counterfactual_best_without_restriction,omitempty"`
	CounterfactualBestRank               string             `json:"counterfactual_best_without_restriction_rank,omitempty"`
	CounterfactualBestScore              float64            `json:"counterfactual_best_without_restriction_score,omitempty"`
}

type cpamsChannelState struct {
	Selection cpamsSelection   `json:"selection"`
	Tokens    []cpamsTokenInfo `json:"tokens"`
}

type cpamsTokenInfo struct {
	Model        string    `json:"model"`
	ExpiresAt    time.Time `json:"expires_at"`
	ExpiresInSec int64     `json:"expires_in_sec"`
}

type cpamsResolution struct {
	Model           string
	RequestedModel  string
	Decision        string
	CandidateCount  int
	SnapshotUpdated time.Time
}

type cpamsRouteDecisionLog struct {
	Event             string  `json:"event"`
	RequestChannel    string  `json:"request_channel"`
	RequestedModel    string  `json:"requested_model,omitempty"`
	RoutingStrategy   string  `json:"routing_strategy,omitempty"`
	Stateful          bool    `json:"stateful"`
	CandidateCount    int     `json:"candidate_count"`
	Decision          string  `json:"decision,omitempty"`
	SelectedCandidate string  `json:"selected_candidate,omitempty"`
	SelectedRank      string  `json:"selected_rank,omitempty"`
	SelectedScore     float64 `json:"selected_score"`
	cpamsSoftRankDecision
}

func newCPAMSResolver(configFilePath string) *cpamsResolver {
	staticDir := managementasset.StaticDir(configFilePath)
	snapshotPath := filepath.Join(staticDir, "model-health-multi.json")

	tokenTTL := cpamsDefaultTokenTTL
	if raw := strings.TrimSpace(os.Getenv("CPAMS_TOKEN_TTL_SECONDS")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			tokenTTL = time.Duration(secs) * time.Second
		}
	}

	return &cpamsResolver{
		snapshotPath:  snapshotPath,
		tokenTTL:      tokenTTL,
		channelTokens: make(map[string]map[string]time.Time),
		sticky:        make(map[string]cpamsSelection),
		selected:      make(map[string]cpamsSelection),
		failed:        make(map[string]cpamsFailureBlock),
		cursors:       make(map[string]int),
		now:           time.Now,
	}
}

func (r *cpamsResolver) SetBalanceSource(source func(context.Context) []health.ProviderBalanceSnapshotItem) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.balanceSource = source
	r.balanceCache = nil
	r.balanceCacheAt = time.Time{}
	r.mu.Unlock()
}

func (r *cpamsResolver) SetRuntimeOutageSource(source func(context.Context) []health.RuntimeOutageSnapshotItem) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.outageSource = source
	r.mu.Unlock()
}

func (r *cpamsResolver) ResolveRequested(channelRaw, requestedModelRaw, conversationKey, strategyRaw, protocolHint string, requireStateful bool) (cpamsResolution, error) {
	channel, err := normalizeCPAMSChannel(channelRaw)
	if err != nil {
		return cpamsResolution{}, err
	}

	requestedModel := strings.TrimSpace(requestedModelRaw)
	if requestedModel == "" {
		model, err := r.Resolve(channel)
		if err != nil {
			return cpamsResolution{}, err
		}
		resolution := cpamsResolution{Model: model}
		if r != nil {
			r.mu.Lock()
			if sel, ok := r.selected[channel]; ok {
				resolution.Decision = strings.TrimSpace(sel.Decision)
				resolution.CandidateCount = sel.CandidateCount
				resolution.SnapshotUpdated = sel.SnapshotUpdated
			}
			r.mu.Unlock()
		}
		return resolution, nil
	}

	canonicalRequested := cpamsCanonicalRequestedModel(requestedModel)
	if canonicalRequested == "" {
		return cpamsResolution{}, fmt.Errorf("cpams: requested model is empty")
	}

	now := r.now().UTC()
	stickyKey := cpamsStickyKey(channel, canonicalRequested, conversationKey)
	var stickySelection cpamsSelection
	var hasStickySelection bool

	r.mu.Lock()
	if r.sticky == nil {
		r.sticky = make(map[string]cpamsSelection)
	}
	if stickyKey != "" {
		if sel, ok := r.sticky[stickyKey]; ok {
			if sel.Model != "" && now.Before(sel.ExpiresAt) {
				stickySelection = sel
				hasStickySelection = true
			} else {
				delete(r.sticky, stickyKey)
			}
		}
	}
	validTokens := r.collectValidTokensLocked(channel, now)
	r.mu.Unlock()

	candidates, snapshotUpdatedAt, err := r.loadCandidatesForRequestedModel(channel, canonicalRequested, now)
	if err != nil {
		if model, exp, ok := pickLatestTokenForRequestedModel(validTokens, canonicalRequested); ok {
			decision := "snapshot_error_fallback_existing_token_same_model"
			r.mu.Lock()
			r.ensureTokenLocked(channel, model, exp)
			r.setSelectionLocked(channel, model, canonicalRequested, exp, snapshotUpdatedAt, decision, 0, now)
			if stickyKey != "" {
				r.sticky[stickyKey] = cpamsSelection{Model: model, RequestedModel: canonicalRequested, ExpiresAt: exp, SnapshotUpdated: snapshotUpdatedAt, Decision: decision, CandidateCount: 0, ChosenAt: now}
			}
			r.mu.Unlock()
			r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, nil, cpamsSoftRankDecision{}, model, decision)
			return cpamsResolution{Model: cpamsApplyRequestedSuffix(model, requestedModel), RequestedModel: requestedModel, Decision: decision, SnapshotUpdated: snapshotUpdatedAt}, nil
		}
		r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, nil, cpamsSoftRankDecision{}, "", "snapshot_load_error")
		return cpamsResolution{}, err
	}
	r.applyUnifiedHealth(channel, canonicalRequested, snapshotUpdatedAt, now, candidates)
	protocolCandidates := append([]cpamsCandidate(nil), candidates...)
	candidates = cpamsFilterCandidatesForProtocol(channel, protocolHint, requireStateful, candidates)
	statefulCandidates := append([]cpamsCandidate(nil), candidates...)
	candidates = r.filterBlockedCandidates(channel, canonicalRequested, snapshotUpdatedAt, candidates)
	settings := cpamsSoftRankSettingsFromEnv()
	healthy, softDecision := cpamsHealthyCandidatesDetailed(candidates, now, settings, requireStateful)

	if requireStateful && len(candidates) == 0 {
		r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, "", "missing_stateful_candidate")
		return cpamsResolution{}, fmt.Errorf("cpams: no stateful %s candidate available for %s%s", strings.TrimSpace(protocolHint), canonicalRequested, cpamsStatefulCandidateDiagnostic(protocolCandidates, statefulCandidates))
	}
	if hasStickySelection && len(healthy) > 0 {
		if cpamsCandidatesContainModel(healthy, stickySelection.Model) {
			r.mu.Lock()
			r.ensureTokenLocked(channel, stickySelection.Model, stickySelection.ExpiresAt)
			decision := "reuse_sticky_selection"
			stickySelection.Decision = decision
			stickySelection.ChosenAt = now
			stickySelection.RequestedModel = canonicalRequested
			stickySelection.SnapshotUpdated = snapshotUpdatedAt
			stickySelection.CandidateCount = len(candidates)
			r.sticky[stickyKey] = stickySelection
			r.setSelectionLocked(channel, stickySelection.Model, canonicalRequested, stickySelection.ExpiresAt, snapshotUpdatedAt, decision, len(candidates), now)
			selectedModel := cpamsApplyRequestedSuffix(stickySelection.Model, requestedModel)
			r.mu.Unlock()
			r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, stickySelection.Model, decision)
			return cpamsResolution{
				Model:           selectedModel,
				RequestedModel:  requestedModel,
				Decision:        decision,
				CandidateCount:  len(candidates),
				SnapshotUpdated: snapshotUpdatedAt,
			}, nil
		}
		r.mu.Lock()
		delete(r.sticky, stickyKey)
		r.mu.Unlock()
	}
	if stickyKey == "" {
		if model, exp, ok := pickLatestHealthyTokenForRequestedModel(validTokens, canonicalRequested, healthy); ok &&
			cpamsShouldReuseHealthyToken(normalizeCPAMSRoutingStrategy(strategyRaw), healthy, model) {
			decision := "reuse_valid_token_same_model"
			expiresAt := now.Add(r.tokenTTL)
			if exp.After(expiresAt) {
				expiresAt = exp
			}
			r.mu.Lock()
			r.ensureTokenLocked(channel, model, expiresAt)
			r.setSelectionLocked(channel, model, canonicalRequested, expiresAt, snapshotUpdatedAt, decision, len(candidates), now)
			if stickyKey != "" {
				r.sticky[stickyKey] = cpamsSelection{
					Model:           model,
					RequestedModel:  canonicalRequested,
					ExpiresAt:       expiresAt,
					SnapshotUpdated: snapshotUpdatedAt,
					Decision:        decision,
					CandidateCount:  len(candidates),
					ChosenAt:        now,
				}
			}
			r.mu.Unlock()
			r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, model, decision)
			return cpamsResolution{
				Model:           cpamsApplyRequestedSuffix(model, requestedModel),
				RequestedModel:  requestedModel,
				Decision:        decision,
				CandidateCount:  len(candidates),
				SnapshotUpdated: snapshotUpdatedAt,
			}, nil
		}
	}
	if len(healthy) == 0 {
		if model, exp, ok := pickLatestTokenForRequestedModelInCandidates(validTokens, canonicalRequested, candidates); ok {
			decision := "all_unhealthy_fallback_existing_token_same_model"
			r.mu.Lock()
			r.ensureTokenLocked(channel, model, exp)
			r.setSelectionLocked(channel, model, canonicalRequested, exp, snapshotUpdatedAt, decision, len(candidates), now)
			if stickyKey != "" {
				r.sticky[stickyKey] = cpamsSelection{Model: model, RequestedModel: canonicalRequested, ExpiresAt: exp, SnapshotUpdated: snapshotUpdatedAt, Decision: decision, CandidateCount: len(candidates), ChosenAt: now}
			}
			r.mu.Unlock()
			r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, model, decision)
			return cpamsResolution{Model: cpamsApplyRequestedSuffix(model, requestedModel), RequestedModel: requestedModel, Decision: decision, CandidateCount: len(candidates), SnapshotUpdated: snapshotUpdatedAt}, nil
		}
		if len(candidates) == 0 {
			if model, exp, ok := pickLatestTokenForRequestedModel(validTokens, canonicalRequested); ok {
				decision := "all_unhealthy_fallback_existing_token_same_model"
				r.mu.Lock()
				r.ensureTokenLocked(channel, model, exp)
				r.setSelectionLocked(channel, model, canonicalRequested, exp, snapshotUpdatedAt, decision, len(candidates), now)
				if stickyKey != "" {
					r.sticky[stickyKey] = cpamsSelection{Model: model, RequestedModel: canonicalRequested, ExpiresAt: exp, SnapshotUpdated: snapshotUpdatedAt, Decision: decision, CandidateCount: len(candidates), ChosenAt: now}
				}
				r.mu.Unlock()
				r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, model, decision)
				return cpamsResolution{Model: cpamsApplyRequestedSuffix(model, requestedModel), RequestedModel: requestedModel, Decision: decision, CandidateCount: len(candidates), SnapshotUpdated: snapshotUpdatedAt}, nil
			}
		}
		if len(candidates) == 1 {
			solo := candidates[0]
			decision := "single_candidate_fallback_same_model"
			expiresAt := now.Add(r.tokenTTL)
			r.mu.Lock()
			r.ensureTokenLocked(channel, solo.FullModel, expiresAt)
			r.setSelectionLocked(channel, solo.FullModel, canonicalRequested, expiresAt, snapshotUpdatedAt, decision, len(candidates), now)
			if stickyKey != "" {
				r.sticky[stickyKey] = cpamsSelection{
					Model:           solo.FullModel,
					RequestedModel:  canonicalRequested,
					ExpiresAt:       expiresAt,
					SnapshotUpdated: snapshotUpdatedAt,
					Decision:        decision,
					CandidateCount:  len(candidates),
					ChosenAt:        now,
				}
			}
			r.mu.Unlock()
			r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, solo.FullModel, decision)
			return cpamsResolution{
				Model:           cpamsApplyRequestedSuffix(solo.FullModel, requestedModel),
				RequestedModel:  requestedModel,
				Decision:        decision,
				CandidateCount:  len(candidates),
				SnapshotUpdated: snapshotUpdatedAt,
			}, nil
		}
		if bridgeCandidate, ok := cpamsPickBridgeCandidate(candidates); ok {
			decision := "bridge_candidate_fallback_same_model"
			expiresAt := now.Add(r.tokenTTL)
			r.mu.Lock()
			r.ensureTokenLocked(channel, bridgeCandidate.FullModel, expiresAt)
			r.setSelectionLocked(channel, bridgeCandidate.FullModel, canonicalRequested, expiresAt, snapshotUpdatedAt, decision, len(candidates), now)
			if stickyKey != "" {
				r.sticky[stickyKey] = cpamsSelection{
					Model:           bridgeCandidate.FullModel,
					RequestedModel:  canonicalRequested,
					ExpiresAt:       expiresAt,
					SnapshotUpdated: snapshotUpdatedAt,
					Decision:        decision,
					CandidateCount:  len(candidates),
					ChosenAt:        now,
				}
			}
			r.mu.Unlock()
			r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, bridgeCandidate.FullModel, decision)
			return cpamsResolution{
				Model:           cpamsApplyRequestedSuffix(bridgeCandidate.FullModel, requestedModel),
				RequestedModel:  requestedModel,
				Decision:        decision,
				CandidateCount:  len(candidates),
				SnapshotUpdated: snapshotUpdatedAt,
			}, nil
		}
		r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, "", "no_healthy_candidate_no_valid_token")
		return cpamsResolution{}, fmt.Errorf("cpams: model %s has no healthy candidate and no valid token", canonicalRequested)
	}

	selectedCandidate, decision := r.pickCandidate(channel, canonicalRequested, normalizeCPAMSRoutingStrategy(strategyRaw), healthy)
	expiresAt := now.Add(r.tokenTTL)
	selection := cpamsSelection{
		Model:           selectedCandidate.FullModel,
		RequestedModel:  canonicalRequested,
		ExpiresAt:       expiresAt,
		SnapshotUpdated: snapshotUpdatedAt,
		Decision:        decision,
		CandidateCount:  len(candidates),
		ChosenAt:        now,
	}

	r.mu.Lock()
	r.ensureTokenLocked(channel, selectedCandidate.FullModel, expiresAt)
	r.setSelectionLocked(channel, selectedCandidate.FullModel, canonicalRequested, expiresAt, snapshotUpdatedAt, decision, len(candidates), now)
	if stickyKey != "" {
		r.sticky[stickyKey] = selection
	}
	r.mu.Unlock()
	r.logResolveRequestedDecision(channel, requestedModel, normalizeCPAMSRoutingStrategy(strategyRaw), requireStateful, candidates, softDecision, selectedCandidate.FullModel, decision)

	return cpamsResolution{
		Model:           cpamsApplyRequestedSuffix(selectedCandidate.FullModel, requestedModel),
		RequestedModel:  requestedModel,
		Decision:        decision,
		CandidateCount:  len(candidates),
		SnapshotUpdated: snapshotUpdatedAt,
	}, nil
}

func (r *cpamsResolver) Resolve(channelRaw string) (string, error) {
	channel, err := normalizeCPAMSChannel(channelRaw)
	if err != nil {
		return "", err
	}
	now := r.now().UTC()

	r.mu.Lock()
	if sel, ok := r.selected[channel]; ok && sel.Model != "" && now.Before(sel.ExpiresAt) {
		r.ensureTokenLocked(channel, sel.Model, sel.ExpiresAt)
		r.setSelectionLocked(channel, sel.Model, sel.RequestedModel, sel.ExpiresAt, sel.SnapshotUpdated, "reuse_cached_selection", sel.CandidateCount, now)
		model := sel.Model
		r.mu.Unlock()
		return model, nil
	}
	validTokens := r.collectValidTokensLocked(channel, now)
	r.mu.Unlock()

	candidates, snapshotUpdatedAt, err := r.loadCandidates(channel, now)
	if err != nil {
		if model, exp, ok := pickLatestToken(validTokens); ok {
			r.mu.Lock()
			r.ensureTokenLocked(channel, model, exp)
			r.setSelectionLocked(channel, model, "", exp, snapshotUpdatedAt, "snapshot_error_fallback_existing_token", 0, now)
			r.mu.Unlock()
			return model, nil
		}
		return "", err
	}
	r.applyUnifiedHealth(channel, "", snapshotUpdatedAt, now, candidates)

	if len(candidates) == 0 {
		if model, exp, ok := pickLatestToken(validTokens); ok {
			r.mu.Lock()
			r.ensureTokenLocked(channel, model, exp)
			r.setSelectionLocked(channel, model, "", exp, snapshotUpdatedAt, "no_candidate_fallback_existing_token", 0, now)
			r.mu.Unlock()
			return model, nil
		}
		return "", fmt.Errorf("cpams: no candidate model for channel %s", channel)
	}

	// Prefer candidates that already have a valid token.
	for _, candidate := range candidates {
		exp, ok := validTokens[candidate.FullModel]
		if !ok || !now.Before(exp) {
			continue
		}
		r.mu.Lock()
		r.ensureTokenLocked(channel, candidate.FullModel, exp)
		r.setSelectionLocked(channel, candidate.FullModel, "", exp, snapshotUpdatedAt, "reuse_valid_token_best_candidate", len(candidates), now)
		r.mu.Unlock()
		return candidate.FullModel, nil
	}

	healthy := cpamsHealthyCandidates(candidates, now, cpamsSoftRankSettingsFromEnv(), false)
	// Issue a fresh token only for healthy candidates.
	for _, candidate := range healthy {
		exp := now.Add(r.tokenTTL)
		r.mu.Lock()
		r.ensureTokenLocked(channel, candidate.FullModel, exp)
		r.setSelectionLocked(channel, candidate.FullModel, "", exp, snapshotUpdatedAt, "issue_new_token_for_healthy_candidate", len(healthy), now)
		r.mu.Unlock()
		return candidate.FullModel, nil
	}

	if model, exp, ok := pickLatestToken(validTokens); ok {
		r.mu.Lock()
		r.ensureTokenLocked(channel, model, exp)
		r.setSelectionLocked(channel, model, "", exp, snapshotUpdatedAt, "all_unhealthy_fallback_existing_token", len(candidates), now)
		r.mu.Unlock()
		return model, nil
	}

	return "", fmt.Errorf("cpams: channel %s has no healthy candidate and no valid token", channel)
}

func (r *cpamsResolver) setSelectionLocked(channel, model, requestedModel string, expiresAt, snapshotUpdated time.Time, decision string, candidateCount int, chosenAt time.Time) {
	if r.selected == nil {
		r.selected = make(map[string]cpamsSelection)
	}
	r.selected[channel] = cpamsSelection{
		Model:           model,
		RequestedModel:  strings.TrimSpace(requestedModel),
		ExpiresAt:       expiresAt,
		SnapshotUpdated: snapshotUpdated,
		Decision:        strings.TrimSpace(decision),
		CandidateCount:  candidateCount,
		ChosenAt:        chosenAt.UTC(),
	}
}

func (r *cpamsResolver) ensureTokenLocked(channel, model string, expiresAt time.Time) {
	if r.channelTokens == nil {
		r.channelTokens = make(map[string]map[string]time.Time)
	}
	byModel := r.channelTokens[channel]
	if byModel == nil {
		byModel = make(map[string]time.Time)
		r.channelTokens[channel] = byModel
	}
	byModel[model] = expiresAt
}

func (r *cpamsResolver) collectValidTokensLocked(channel string, now time.Time) map[string]time.Time {
	valid := make(map[string]time.Time)
	byModel := r.channelTokens[channel]
	if len(byModel) == 0 {
		delete(r.channelTokens, channel)
		return valid
	}
	for model, expiresAt := range byModel {
		if !now.Before(expiresAt) {
			delete(byModel, model)
			continue
		}
		valid[model] = expiresAt
	}
	if len(byModel) == 0 {
		delete(r.channelTokens, channel)
	}
	return valid
}

func pickLatestToken(tokens map[string]time.Time) (string, time.Time, bool) {
	var (
		model string
		exp   time.Time
	)
	for candidateModel, candidateExp := range tokens {
		if candidateModel == "" {
			continue
		}
		if model == "" || candidateExp.After(exp) {
			model = candidateModel
			exp = candidateExp
		}
	}
	if model == "" {
		return "", time.Time{}, false
	}
	return model, exp, true
}

func pickLatestTokenForRequestedModel(tokens map[string]time.Time, requestedModel string) (string, time.Time, bool) {
	requestedKey := strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(requestedModel)))
	if requestedKey == "" {
		return "", time.Time{}, false
	}
	filtered := make(map[string]time.Time)
	for model, exp := range tokens {
		_, upstream := splitCPAMSModel(model)
		if strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(upstream))) != requestedKey {
			continue
		}
		filtered[model] = exp
	}
	return pickLatestToken(filtered)
}

func pickLatestTokenForRequestedModelInCandidates(tokens map[string]time.Time, requestedModel string, candidates []cpamsCandidate) (string, time.Time, bool) {
	if len(tokens) == 0 || len(candidates) == 0 {
		return "", time.Time{}, false
	}
	allowed := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		model := strings.ToLower(strings.TrimSpace(candidate.FullModel))
		if model == "" {
			continue
		}
		allowed[model] = struct{}{}
	}
	filtered := make(map[string]time.Time)
	for model, exp := range tokens {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(model))]; !ok {
			continue
		}
		filtered[model] = exp
	}
	return pickLatestTokenForRequestedModel(filtered, requestedModel)
}

func pickLatestHealthyTokenForRequestedModel(tokens map[string]time.Time, requestedModel string, healthy []cpamsCandidate) (string, time.Time, bool) {
	if len(tokens) == 0 || len(healthy) == 0 {
		return "", time.Time{}, false
	}
	healthyModels := make(map[string]struct{}, len(healthy))
	for _, candidate := range healthy {
		model := strings.TrimSpace(candidate.FullModel)
		if model == "" {
			continue
		}
		healthyModels[strings.ToLower(model)] = struct{}{}
	}
	filtered := make(map[string]time.Time)
	for model, exp := range tokens {
		if _, ok := healthyModels[strings.ToLower(strings.TrimSpace(model))]; !ok {
			continue
		}
		filtered[model] = exp
	}
	return pickLatestTokenForRequestedModel(filtered, requestedModel)
}

func cpamsShouldReuseHealthyToken(strategy string, healthy []cpamsCandidate, model string) bool {
	current, ok := cpamsFindCandidateByModel(healthy, model)
	if !ok {
		return false
	}
	if strategy != "health-aware" {
		return true
	}
	best := healthy[0]
	if cpamsSameModel(best.FullModel, current.FullModel) {
		return true
	}
	if current.StatusRank < best.StatusRank {
		return false
	}
	// When every currently available candidate is already degraded/yellow, do not
	// keep reviving an older token unless it is still the best route.
	if best.StatusRank < cpamsStatusGreen {
		return false
	}
	return current.Score+cpamsHealthAwareReuseScoreSlack >= best.Score
}

func cpamsFindCandidateByModel(candidates []cpamsCandidate, model string) (cpamsCandidate, bool) {
	for _, candidate := range candidates {
		if cpamsSameModel(candidate.FullModel, model) {
			return candidate, true
		}
	}
	return cpamsCandidate{}, false
}

func cpamsCandidatesContainModel(candidates []cpamsCandidate, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.FullModel), model) {
			return true
		}
	}
	return false
}

func (r *cpamsResolver) loadCandidates(channel string, now time.Time) ([]cpamsCandidate, time.Time, error) {
	snapshot, snapshotUpdatedAt, err := r.loadSnapshot(now)
	if err != nil {
		return nil, time.Time{}, err
	}
	candidates := buildCPAMSCandidates(channel, &snapshot, now)
	return candidates, snapshotUpdatedAt, nil
}

func (r *cpamsResolver) loadCandidatesForRequestedModel(channel, requestedModel string, now time.Time) ([]cpamsCandidate, time.Time, error) {
	snapshot, snapshotUpdatedAt, err := r.loadSnapshot(now)
	if err != nil {
		return nil, time.Time{}, err
	}

	candidates := buildCPAMSRequestedCandidates(channel, requestedModel, &snapshot, now)
	return candidates, snapshotUpdatedAt, nil
}

func (r *cpamsResolver) loadSnapshot(now time.Time) (cpamsMultiSnapshot, time.Time, error) {
	return r.loadSnapshotWithStaleness(now, false)
}

func (r *cpamsResolver) loadSnapshotAllowStale(now time.Time) (cpamsMultiSnapshot, time.Time, error) {
	return r.loadSnapshotWithStaleness(now, true)
}

func (r *cpamsResolver) loadSnapshotWithStaleness(now time.Time, allowStale bool) (cpamsMultiSnapshot, time.Time, error) {
	if strings.TrimSpace(r.snapshotPath) == "" {
		return cpamsMultiSnapshot{}, time.Time{}, fmt.Errorf("cpams: model-health snapshot path is empty")
	}

	raw, err := os.ReadFile(r.snapshotPath)
	if err != nil {
		return cpamsMultiSnapshot{}, time.Time{}, fmt.Errorf("cpams: failed to read snapshot: %w", err)
	}

	var snapshot cpamsMultiSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return cpamsMultiSnapshot{}, time.Time{}, fmt.Errorf("cpams: failed to parse snapshot: %w", err)
	}

	snapshotUpdatedAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(snapshot.UpdatedAt))
	if !allowStale && !snapshotUpdatedAt.IsZero() && snapshot.IntervalSec > 0 {
		maxAge := time.Duration(snapshot.IntervalSec*2) * time.Second
		if now.Sub(snapshotUpdatedAt) > maxAge {
			return cpamsMultiSnapshot{}, snapshotUpdatedAt, fmt.Errorf("cpams: snapshot is stale (updated_at=%s, max_age=%s)", snapshotUpdatedAt.Format(time.RFC3339), maxAge.String())
		}
	}

	return snapshot, snapshotUpdatedAt, nil
}

func (r *cpamsResolver) cachedBalanceSnapshot() []health.ProviderBalanceSnapshotItem {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	source := r.balanceSource
	cached := append([]health.ProviderBalanceSnapshotItem(nil), r.balanceCache...)
	cachedAt := r.balanceCacheAt
	r.mu.Unlock()
	if len(cached) > 0 && time.Since(cachedAt) < 2*time.Minute {
		return cached
	}
	if source == nil {
		return nil
	}
	items := source(context.Background())
	r.mu.Lock()
	r.balanceCache = append(r.balanceCache[:0], items...)
	r.balanceCacheAt = time.Now()
	copied := append([]health.ProviderBalanceSnapshotItem(nil), r.balanceCache...)
	r.mu.Unlock()
	return copied
}

func (r *cpamsResolver) runtimeOutageSnapshot() []health.RuntimeOutageSnapshotItem {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	source := r.outageSource
	r.mu.Unlock()
	if source == nil {
		return nil
	}
	return source(context.Background())
}

func (r *cpamsResolver) applyUnifiedHealth(channel, requestedModel string, snapshotUpdatedAt, now time.Time, candidates []cpamsCandidate) {
	if len(candidates) == 0 {
		return
	}
	loads := r.activeLoadsByModel(channel, strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(requestedModel))), now)
	balances := r.cachedBalanceSnapshot()
	outages := r.runtimeOutageSnapshot()
	for i := range candidates {
		candidate := &candidates[i]
		prefix, upstream := splitCPAMSModel(candidate.FullModel)
		signals := health.UnifiedSignals{
			Probe: health.ProbeSignal{
				Status:    cpamsProbeStatusString(candidate.StatusRank),
				TestedAt:  candidate.LatestTestedAt,
				UpdatedAt: snapshotUpdatedAt,
				Found:     true,
			},
			Load: health.LoadSignal{
				Active: loads[candidate.FullModel],
				Found:  true,
			},
		}
		if !cpamsIsBridgeCandidate(*candidate) {
			signals.Outage = health.SelectRuntimeOutageSignal(outages, prefix, "", channel, prefix, upstream, snapshotUpdatedAt)
		}
		signals.Balance = health.SelectBalanceSignal(balances, prefix, "", channel, prefix)
		score := health.Score(signals, now)
		candidate.UnifiedScore = score.Score
		candidate.UnifiedStatus = score.Status
		if strings.EqualFold(score.Status, "unavailable") {
			candidate.StatusRank = cpamsStatusRed
		} else if strings.EqualFold(score.Status, "degraded") && candidate.StatusRank > cpamsStatusYellow {
			candidate.StatusRank = cpamsStatusYellow
		}
		candidate.Score += int(score.Score * 10)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			if candidates[i].UnifiedScore == candidates[j].UnifiedScore {
				if candidates[i].StatusRank == candidates[j].StatusRank {
					if candidates[i].PrefixPriority == candidates[j].PrefixPriority {
						return candidates[i].FullModel < candidates[j].FullModel
					}
					return candidates[i].PrefixPriority < candidates[j].PrefixPriority
				}
				return candidates[i].StatusRank > candidates[j].StatusRank
			}
			return candidates[i].UnifiedScore > candidates[j].UnifiedScore
		}
		return candidates[i].Score > candidates[j].Score
	})
}

func buildCPAMSCandidates(channel string, snapshot *cpamsMultiSnapshot, now time.Time) []cpamsCandidate {
	if snapshot == nil || len(snapshot.Entries) == 0 {
		return nil
	}

	aggregates := make(map[string]*cpamsAggregate)
	for _, entry := range snapshot.Entries {
		if entry.Disabled {
			continue
		}
		entryPrefix := strings.ToLower(strings.TrimSpace(entry.ProviderPrefix))
		entryStability := cpamsEntryStabilityScore(entry)
		for _, model := range entry.Models {
			full := strings.TrimSpace(model.Model)
			if full == "" {
				continue
			}

			prefixFromModel, upstream := splitCPAMSModel(full)
			effectivePrefix := prefixFromModel
			if effectivePrefix == "" {
				effectivePrefix = entryPrefix
			}
			if effectivePrefix == "" || upstream == "" {
				continue
			}
			if !cpamsChannelMatchesModel(channel, effectivePrefix, upstream) {
				continue
			}

			statusRank := cpamsAdjustedStatusRank(channel, upstream, model)
			reason := cpamsEffectiveReason(channel, upstream, model)
			fullModel := full
			if prefixFromModel == "" {
				fullModel = effectivePrefix + "/" + upstream
			}

			key := strings.ToLower(strings.TrimSpace(upstream))
			aggregate := aggregates[key]
			if aggregate == nil {
				aggregate = &cpamsAggregate{
					Upstream:           upstream,
					BestPrefixPriority: 1 << 20,
				}
				aggregates[key] = aggregate
			}
			aggregate.Observations++

			switch statusRank {
			case cpamsStatusGreen:
				aggregate.Greens++
			case cpamsStatusYellow:
				aggregate.Yellows++
			case cpamsStatusRed:
				aggregate.Reds++
			}
			if statusRank >= cpamsStatusYellow {
				aggregate.Healthy++
			}
			if cpamsIsNetworkFailureReason(reason) {
				aggregate.NetworkFailures++
			}
			aggregate.Score += cpamsStatusScore(statusRank) + entryStability + cpamsReasonScore(reason)
			aggregate.LatestTestedAt = maxCPAMSTime(aggregate.LatestTestedAt, cpamsResolveTestedAt(snapshot, entry, model))

			prefixPriority := cpamsPrefixPriority(channel, effectivePrefix)
			if aggregate.BestFullModel == "" ||
				statusRank > aggregate.BestStatusRank ||
				(statusRank == aggregate.BestStatusRank && prefixPriority < aggregate.BestPrefixPriority) ||
				(statusRank == aggregate.BestStatusRank && prefixPriority == aggregate.BestPrefixPriority && fullModel < aggregate.BestFullModel) {
				aggregate.BestFullModel = fullModel
				aggregate.BestStatusRank = statusRank
				aggregate.BestPrefixPriority = prefixPriority
			}
		}
	}

	candidates := make([]cpamsCandidate, 0, len(aggregates))
	for _, aggregate := range aggregates {
		if aggregate == nil || aggregate.BestFullModel == "" {
			continue
		}
		availability := 0
		if aggregate.Observations > 0 {
			availability = (aggregate.Healthy * 100) / aggregate.Observations
		}
		score := aggregate.Score +
			(aggregate.Greens * 220) +
			(aggregate.Yellows * 70) -
			(aggregate.Reds * 90) +
			(availability * 5) -
			(aggregate.NetworkFailures * 25) +
			(aggregate.BestStatusRank * 20) -
			(aggregate.BestPrefixPriority * 6) +
			cpamsFreshnessScore(now, aggregate.LatestTestedAt)
		candidates = append(candidates, cpamsCandidate{
			FullModel:      aggregate.BestFullModel,
			Upstream:       aggregate.Upstream,
			StatusRank:     aggregate.BestStatusRank,
			Score:          score,
			PrefixPriority: aggregate.BestPrefixPriority,
			Observations:   aggregate.Observations,
			Healthy:        aggregate.Healthy,
			Greens:         aggregate.Greens,
			Yellows:        aggregate.Yellows,
			Reds:           aggregate.Reds,
			LatestTestedAt: aggregate.LatestTestedAt,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			if candidates[i].StatusRank == candidates[j].StatusRank {
				if candidates[i].PrefixPriority == candidates[j].PrefixPriority {
					return candidates[i].FullModel < candidates[j].FullModel
				}
				return candidates[i].PrefixPriority < candidates[j].PrefixPriority
			}
			return candidates[i].StatusRank > candidates[j].StatusRank
		}
		return candidates[i].Score > candidates[j].Score
	})

	return candidates
}

func buildCPAMSRequestedCandidates(channel, requestedModel string, snapshot *cpamsMultiSnapshot, now time.Time) []cpamsCandidate {
	if snapshot == nil || len(snapshot.Entries) == 0 {
		return nil
	}

	requestedKey := strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(requestedModel)))
	if requestedKey == "" {
		return nil
	}

	aggregates := make(map[string]*cpamsCandidate)
	for _, entry := range snapshot.Entries {
		if entry.Disabled {
			continue
		}
		entryPrefix := strings.ToLower(strings.TrimSpace(entry.ProviderPrefix))
		entryStability := cpamsEntryStabilityScore(entry)
		for _, model := range entry.Models {
			full := strings.TrimSpace(model.Model)
			if full == "" {
				continue
			}

			prefixFromModel, upstream := splitCPAMSModel(full)
			effectivePrefix := prefixFromModel
			if effectivePrefix == "" {
				effectivePrefix = entryPrefix
			}
			if effectivePrefix == "" || upstream == "" {
				continue
			}
			if !cpamsChannelMatchesModel(channel, effectivePrefix, upstream) {
				continue
			}
			if strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(upstream))) != requestedKey {
				continue
			}

			statusRank := cpamsAdjustedStatusRank(channel, upstream, model)
			reason := cpamsEffectiveReason(channel, upstream, model)
			fullModel := full
			if prefixFromModel == "" {
				fullModel = effectivePrefix + "/" + upstream
			}
			prefixPriority := cpamsPrefixPriority(channel, effectivePrefix)
			testedAt := cpamsResolveTestedAt(snapshot, entry, model)

			candidate := aggregates[fullModel]
			if candidate == nil {
				candidate = &cpamsCandidate{
					FullModel:                   fullModel,
					Upstream:                    upstream,
					Reason:                      reason,
					ResponsesContinuationStatus: strings.TrimSpace(model.ResponsesContinuationStatus),
					ResponsesContinuationReason: strings.TrimSpace(model.ResponsesContinuationReason),
					StatusRank:                  statusRank,
					PrefixPriority:              prefixPriority,
					LatestTestedAt:              testedAt,
				}
				aggregates[fullModel] = candidate
			}

			candidate.Observations++
			switch statusRank {
			case cpamsStatusGreen:
				candidate.Greens++
			case cpamsStatusYellow:
				candidate.Yellows++
			case cpamsStatusRed:
				candidate.Reds++
			}
			if statusRank >= cpamsStatusYellow {
				candidate.Healthy++
			}
			if cpamsIsNetworkFailureReason(reason) {
				candidate.NetworkFailures++
			}
			candidate.Score += cpamsStatusScore(statusRank) + entryStability + cpamsReasonScore(reason)
			candidate.LatestTestedAt = maxCPAMSTime(candidate.LatestTestedAt, testedAt)
			if statusRank > candidate.StatusRank {
				candidate.StatusRank = statusRank
				candidate.Reason = reason
			} else if candidate.Reason == "" {
				candidate.Reason = reason
			}
			if candidate.ResponsesContinuationStatus == "" {
				candidate.ResponsesContinuationStatus = strings.TrimSpace(model.ResponsesContinuationStatus)
			}
			if candidate.ResponsesContinuationReason == "" {
				candidate.ResponsesContinuationReason = strings.TrimSpace(model.ResponsesContinuationReason)
			}
		}
	}

	candidates := make([]cpamsCandidate, 0, len(aggregates))
	for _, candidate := range aggregates {
		if candidate == nil || candidate.FullModel == "" {
			continue
		}
		availability := 0
		if candidate.Observations > 0 {
			availability = (candidate.Healthy * 100) / candidate.Observations
		}
		candidate.Score +=
			(candidate.Greens * 220) +
				(candidate.Yellows * 70) -
				(candidate.Reds * 90) +
				(availability * 5) -
				(candidate.NetworkFailures * 25) +
				(candidate.StatusRank * 20) -
				(candidate.PrefixPriority * 6) +
				cpamsFreshnessScore(now, candidate.LatestTestedAt)
		candidates = append(candidates, *candidate)
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score == candidates[j].Score {
			if candidates[i].StatusRank == candidates[j].StatusRank {
				if candidates[i].PrefixPriority == candidates[j].PrefixPriority {
					return candidates[i].FullModel < candidates[j].FullModel
				}
				return candidates[i].PrefixPriority < candidates[j].PrefixPriority
			}
			return candidates[i].StatusRank > candidates[j].StatusRank
		}
		return candidates[i].Score > candidates[j].Score
	})

	return candidates
}

func cpamsHealthyCandidates(candidates []cpamsCandidate, now time.Time, settings cpamsSoftRankSettings, requireStateful bool) []cpamsCandidate {
	healthy, _ := cpamsHealthyCandidatesDetailed(candidates, now, settings, requireStateful)
	return healthy
}

func cpamsHealthyCandidatesDetailed(candidates []cpamsCandidate, now time.Time, settings cpamsSoftRankSettings, requireStateful bool) ([]cpamsCandidate, cpamsSoftRankDecision) {
	decision := cpamsSoftRankDecision{
		SoftRankEnabled:              settings.Enabled,
		YellowScoreDeltaThreshold:    settings.YellowScoreDelta,
		YellowMaxShare:               settings.YellowMaxShare,
		StatefulAllowYellow:          settings.StatefulAllowYellow,
		MinGreenCountForExclusive:    settings.MinGreenCountForExclusive,
		YellowFreshnessWindowSeconds: int64(settings.YellowFreshnessWindow / time.Second),
		YellowScoreDelta:             make(map[string]float64),
		YellowFreshnessSeconds:       make(map[string]int64),
		YellowAdmitReason:            make(map[string]string),
		YellowRejectReason:           make(map[string]string),
	}
	base := make([]cpamsCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.UnifiedStatus), "unavailable") {
			continue
		}
		if candidate.StatusRank < cpamsStatusYellow {
			continue
		}
		base = append(base, candidate)
	}
	if len(base) == 0 {
		return nil, decision
	}
	decision.CounterfactualBestWithoutRestriction = base[0].FullModel
	decision.CounterfactualBestRank = cpamsProbeStatusString(base[0].StatusRank)
	decision.CounterfactualBestScore = base[0].UnifiedScore

	greens := make([]cpamsCandidate, 0, len(base))
	yellows := make([]cpamsCandidate, 0, len(base))
	for _, candidate := range base {
		switch candidate.StatusRank {
		case cpamsStatusGreen:
			greens = append(greens, candidate)
		case cpamsStatusYellow:
			yellows = append(yellows, candidate)
		}
	}
	decision.GreenCount = len(greens)
	decision.YellowCount = len(yellows)
	for _, candidate := range yellows {
		decision.YellowFreshnessSeconds[candidate.FullModel] = cpamsCandidateFreshnessSeconds(now, candidate)
	}
	if len(greens) == 0 {
		if requireStateful && !settings.StatefulAllowYellow {
			decision.GreenExclusiveReason = "stateful_request"
			for _, candidate := range yellows {
				decision.YellowCandidatesRejected = append(decision.YellowCandidatesRejected, candidate.FullModel)
				decision.YellowRejectReason[candidate.FullModel] = "stateful_request"
			}
			return nil, decision
		}

		out := make([]cpamsCandidate, 0, len(yellows))
		for _, candidate := range yellows {
			if !cpamsYellowCandidateFreshEnough(now, candidate, settings.YellowFreshnessWindow) {
				decision.YellowCandidatesRejected = append(decision.YellowCandidatesRejected, candidate.FullModel)
				decision.YellowRejectReason[candidate.FullModel] = "stale_probe"
				continue
			}
			out = append(out, candidate)
			decision.YellowCandidatesAdmitted = append(decision.YellowCandidatesAdmitted, candidate.FullModel)
			decision.YellowAdmitReason[candidate.FullModel] = "no_green_candidates"
		}
		decision.GreenExclusiveReason = "no_green_candidates"
		decision.FinalPoolSize = len(out)
		return out, decision
	}
	decision.GreenExclusiveReason = "green_only_policy"
	bestGreenScore := greens[0].UnifiedScore
	for _, candidate := range greens[1:] {
		if candidate.UnifiedScore > bestGreenScore {
			bestGreenScore = candidate.UnifiedScore
		}
	}
	decision.BestGreenScore = bestGreenScore
	out := append([]cpamsCandidate(nil), greens...)
	rejectReason := "binary_health_policy"
	if requireStateful && !settings.StatefulAllowYellow {
		rejectReason = "stateful_request"
		decision.GreenExclusiveReason = "stateful_request"
	}
	for _, candidate := range yellows {
		decision.YellowCandidatesRejected = append(decision.YellowCandidatesRejected, candidate.FullModel)
		decision.YellowRejectReason[candidate.FullModel] = rejectReason
	}
	decision.FinalPoolSize = len(out)
	return out, decision
}

func cpamsYellowCandidateFreshEnough(now time.Time, candidate cpamsCandidate, freshnessWindow time.Duration) bool {
	if cpamsIsBridgeCandidate(candidate) {
		return true
	}
	if freshnessWindow <= 0 {
		return true
	}
	freshnessSeconds := cpamsCandidateFreshnessSeconds(now, candidate)
	if freshnessSeconds < 0 {
		return false
	}
	return time.Duration(freshnessSeconds)*time.Second <= freshnessWindow
}

func cpamsSoftRankSettingsFromEnv() cpamsSoftRankSettings {
	settings := cpamsSoftRankSettings{
		Enabled:                   true,
		YellowScoreDelta:          10,
		YellowMaxShare:            0.25,
		StatefulAllowYellow:       false,
		MinGreenCountForExclusive: 2,
		YellowFreshnessWindow:     10 * time.Minute,
	}
	if raw := strings.TrimSpace(os.Getenv("CPAMS_SOFT_RANK_ENABLED")); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			settings.Enabled = parsed
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CPAMS_YELLOW_SCORE_DELTA")); raw != "" {
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil && parsed >= 0 {
			settings.YellowScoreDelta = parsed
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CPAMS_YELLOW_MAX_SHARE")); raw != "" {
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil && parsed >= 0 {
			if parsed > 1 {
				parsed = 1
			}
			settings.YellowMaxShare = parsed
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CPAMS_STATEFUL_ALLOW_YELLOW")); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			settings.StatefulAllowYellow = parsed
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CPAMS_MIN_GREEN_COUNT_FOR_EXCLUSIVE")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			settings.MinGreenCountForExclusive = parsed
		}
	}
	if raw := strings.TrimSpace(os.Getenv("CPAMS_YELLOW_FRESHNESS_SECONDS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			settings.YellowFreshnessWindow = time.Duration(parsed) * time.Second
		}
	}
	return settings
}

func (r *cpamsResolver) pickCandidate(channel, requestedModel, strategy string, candidates []cpamsCandidate) (cpamsCandidate, string) {
	if len(candidates) == 0 {
		return cpamsCandidate{}, ""
	}
	if strategy == "fill-first" {
		return candidates[0], "issue_new_token_fill_first_same_model"
	}
	now := time.Now().UTC()
	if r != nil && r.now != nil {
		now = r.now().UTC()
	}
	if strategy == "health-aware" {
		return candidates[0], "issue_new_token_health_aware_same_model"
	}
	leastLoaded := r.leastLoadedCandidates(channel, requestedModel, now, candidates)
	if len(leastLoaded) == 0 {
		leastLoaded = candidates
	}
	key := channel + ":" + strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(requestedModel)))
	r.mu.Lock()
	if r.cursors == nil {
		r.cursors = make(map[string]int)
	}
	index := r.cursors[key]
	if index >= 2_147_483_640 {
		index = 0
	}
	r.cursors[key] = index + 1
	r.mu.Unlock()
	return leastLoaded[index%len(leastLoaded)], "issue_new_token_balanced_same_model"
}

func (r *cpamsResolver) leastLoadedCandidates(channel, requestedModel string, now time.Time, candidates []cpamsCandidate) []cpamsCandidate {
	if r == nil || len(candidates) == 0 {
		return candidates
	}
	requestedKey := strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(requestedModel)))
	if requestedKey == "" {
		return candidates
	}

	loads := r.activeLoadsByModel(channel, requestedKey, now)
	minLoad := -1
	filtered := make([]cpamsCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		load := loads[candidate.FullModel]
		if minLoad == -1 || load < minLoad {
			minLoad = load
			filtered = filtered[:0]
			filtered = append(filtered, candidate)
			continue
		}
		if load == minLoad {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func (r *cpamsResolver) activeLoadsByModel(channel, requestedKey string, now time.Time) map[string]int {
	loads := make(map[string]int)
	if r == nil {
		return loads
	}

	channel = strings.TrimSpace(channel)
	requestedKey = strings.TrimSpace(requestedKey)
	if channel == "" || requestedKey == "" {
		return loads
	}

	stickyPrefix := channel + "|" + requestedKey + "|"

	r.mu.Lock()
	defer r.mu.Unlock()

	for key, sel := range r.sticky {
		if !now.Before(sel.ExpiresAt) {
			delete(r.sticky, key)
			continue
		}
		if !strings.HasPrefix(key, stickyPrefix) {
			continue
		}
		if strings.TrimSpace(sel.Model) == "" {
			continue
		}
		loads[sel.Model]++
	}

	byModel := r.channelTokens[channel]
	for model, expiresAt := range byModel {
		if !now.Before(expiresAt) {
			delete(byModel, model)
			continue
		}
		if loads[model] == 0 {
			loads[model] = 1
		}
	}
	if len(byModel) == 0 {
		delete(r.channelTokens, channel)
	}

	return loads
}

func cpamsStickyKey(channel, requestedModel, conversationKey string) string {
	conversationKey = strings.TrimSpace(conversationKey)
	requestedKey := strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(requestedModel)))
	if conversationKey == "" || requestedKey == "" {
		return ""
	}
	return channel + "|" + requestedKey + "|" + conversationKey
}

func cpamsCanonicalRequestedModel(model string) string {
	result := thinking.ParseSuffix(strings.TrimSpace(model))
	return strings.TrimSpace(result.ModelName)
}

func cpamsApplyRequestedSuffix(selectedModel, requestedModel string) string {
	selectedModel = strings.TrimSpace(selectedModel)
	if selectedModel == "" {
		return ""
	}
	requested := thinking.ParseSuffix(strings.TrimSpace(requestedModel))
	if !requested.HasSuffix || strings.TrimSpace(requested.RawSuffix) == "" {
		return selectedModel
	}
	if thinking.ParseSuffix(selectedModel).HasSuffix {
		return selectedModel
	}
	return selectedModel + "(" + requested.RawSuffix + ")"
}

func normalizeCPAMSRoutingStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	case "health-aware", "health", "weighted":
		return "health-aware"
	default:
		return "round-robin"
	}
}

func cpamsStatusScore(rank int) int {
	switch rank {
	case cpamsStatusGreen:
		return 140
	case cpamsStatusYellow:
		return 35
	case cpamsStatusRed:
		return -65
	default:
		return -20
	}
}

func cpamsStatusRank(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "green":
		return cpamsStatusGreen
	case "yellow":
		return cpamsStatusYellow
	case "red":
		return cpamsStatusRed
	default:
		return cpamsStatusUnknown
	}
}

func cpamsProbeStatusString(rank int) string {
	switch rank {
	case cpamsStatusGreen:
		return "green"
	case cpamsStatusYellow:
		return "yellow"
	case cpamsStatusRed:
		return "red"
	default:
		return "unknown"
	}
}

func cpamsCandidateFreshnessSeconds(now time.Time, candidate cpamsCandidate) int64 {
	if candidate.LatestTestedAt.IsZero() {
		return -1
	}
	freshness := now.Sub(candidate.LatestTestedAt)
	if freshness < 0 {
		return 0
	}
	return int64(freshness / time.Second)
}

func (r *cpamsResolver) logResolveRequestedDecision(channel, requestedModel, strategy string, requireStateful bool, candidates []cpamsCandidate, softDecision cpamsSoftRankDecision, selectedModel, decision string) {
	selected, ok := cpamsFindCandidateByModel(candidates, selectedModel)
	selectedRank := ""
	selectedScore := 0.0
	if ok {
		selectedRank = cpamsProbeStatusString(selected.StatusRank)
		selectedScore = selected.UnifiedScore
	}

	payload := cpamsRouteDecisionLog{
		Event:                 "cpams_route_decision",
		RequestChannel:        strings.TrimSpace(channel),
		RequestedModel:        strings.TrimSpace(requestedModel),
		RoutingStrategy:       strings.TrimSpace(strategy),
		Stateful:              requireStateful,
		CandidateCount:        len(candidates),
		Decision:              strings.TrimSpace(decision),
		SelectedCandidate:     strings.TrimSpace(selectedModel),
		SelectedRank:          selectedRank,
		SelectedScore:         selectedScore,
		cpamsSoftRankDecision: softDecision,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		log.WithError(err).Warn("cpams: failed to marshal route decision log")
		return
	}
	log.Info(string(raw))
}

func splitCPAMSModel(full string) (prefix string, upstream string) {
	trimmed := strings.TrimSpace(full)
	if trimmed == "" {
		return "", ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return "", trimmed
	}
	return strings.ToLower(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])
}

func cpamsChannelMatchesPrefix(channel, prefix string) bool {
	return cpamsChannelMatchesModel(channel, prefix, "")
}

func cpamsChannelMatchesModel(channel, prefix, upstream string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	upstream = strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(upstream)))
	switch channel {
	case "codex":
		if prefix == "sub2api" || prefix == "ee" {
			return true
		}
		if strings.Contains(prefix, "codex") {
			return true
		}
		return cpamsModelBelongsToChannel(channel, upstream)
	case "claude":
		if prefix == "cc" {
			return true
		}
		if strings.Contains(prefix, "claude") {
			return true
		}
		return cpamsModelBelongsToChannel(channel, upstream)
	default:
		return false
	}
}

func cpamsPrefixPriority(channel, prefix string) int {
	return cpamsRoutePriority(channel, prefix, "")
}

func cpamsRoutePriority(channel, prefix, upstream string) int {
	channel = strings.ToLower(strings.TrimSpace(channel))
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	upstream = strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(upstream)))
	switch channel {
	case "codex":
		switch prefix {
		case "sub2api":
			return 0
		case "yunyi-codex":
			return 1
		case "ee":
			return 2
		default:
			if strings.Contains(prefix, "codex") {
				return 3
			}
			if cpamsModelBelongsToChannel(channel, upstream) {
				switch prefix {
				case "openai":
					return 4
				default:
					return 8
				}
			}
			return 100
		}
	case "claude":
		if cpamsModelBelongsToChannel(channel, upstream) {
			// Keep Claude providers on the same tier so health/load signals decide.
			return 0
		}
		return 100
	default:
		return 1000
	}
}

func cpamsModelBelongsToChannel(channel, upstream string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	upstream = strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(upstream)))
	if upstream == "" {
		return false
	}
	switch channel {
	case "codex":
		return strings.Contains(upstream, "codex") ||
			strings.HasPrefix(upstream, "gpt-") ||
			strings.HasPrefix(upstream, "chatgpt-") ||
			strings.HasPrefix(upstream, "o1") ||
			strings.HasPrefix(upstream, "o3") ||
			strings.HasPrefix(upstream, "o4")
	case "claude":
		return strings.Contains(upstream, "claude")
	default:
		return false
	}
}

func cpamsEntryStabilityScore(entry cpamsMultiEntry) int {
	total := entry.Summary.Total
	green := entry.Summary.Green
	yellow := entry.Summary.Yellow
	red := entry.Summary.Red
	if total <= 0 {
		for _, model := range entry.Models {
			total++
			switch cpamsStatusRank(model.Status) {
			case cpamsStatusGreen:
				green++
			case cpamsStatusYellow:
				yellow++
			case cpamsStatusRed:
				red++
			}
		}
	}
	if total <= 0 {
		return 0
	}
	score := ((green * 180) + (yellow * 60) - (red * 140)) / total
	// Very small samples are noisy; shrink toward neutral.
	if total < 3 {
		score = (score * 70) / 100
	}
	return score
}

func cpamsAdjustedStatusRank(channel, upstream string, model cpamsModelHealth) int {
	statusRank := cpamsStatusRank(model.Status)
	if channel == "claude" && statusRank > cpamsStatusYellow && cpamsHasResponseModelMismatch(upstream, model) {
		return cpamsStatusYellow
	}
	return statusRank
}

func cpamsEffectiveReason(channel, upstream string, model cpamsModelHealth) string {
	reason := strings.TrimSpace(model.Reason)
	if channel == "claude" && cpamsHasResponseModelMismatch(upstream, model) {
		reason = cpamsAppendReason(reason, "response_model_mismatch")
	}
	return reason
}

func cpamsIsBridgeCandidate(candidate cpamsCandidate) bool {
	reason := strings.ToLower(strings.TrimSpace(candidate.Reason))
	if strings.Contains(reason, "response_model_mismatch") {
		return true
	}
	prefix, _ := splitCPAMSModel(candidate.FullModel)
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "claude cheep":
		return true
	default:
		return false
	}
}

func cpamsHasResponseModelMismatch(upstream string, model cpamsModelHealth) bool {
	requested := strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(upstream)))
	if requested == "" {
		requested = strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(strings.TrimSpace(firstNonEmpty(model.UpstreamModel, model.Model)))))
	}
	response := strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(strings.TrimSpace(firstNonEmpty(model.ResponseModel, model.TextFallbackResponseModel)))))
	return requested != "" && response != "" && requested != response
}

func cpamsAppendReason(parts ...string) string {
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{})
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key := strings.ToLower(part)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, part)
	}
	return strings.Join(out, "; ")
}

func cpamsReasonScore(reason string) int {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "" {
		return 0
	}
	switch {
	case strings.Contains(reason, "response_model_mismatch"):
		return -45
	case strings.Contains(reason, "ok"):
		return 10
	case strings.Contains(reason, "timeout"), strings.Contains(reason, "network"), strings.Contains(reason, "connection"):
		return -35
	case strings.Contains(reason, "auth"), strings.Contains(reason, "unauthorized"), strings.Contains(reason, "forbidden"):
		return -22
	case strings.Contains(reason, "quota"), strings.Contains(reason, "rate"):
		return -15
	case strings.Contains(reason, "error"), strings.Contains(reason, "invalid"):
		return -10
	default:
		return -3
	}
}

func firstNonEmpty(parts ...string) string {
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func cpamsFilterCandidatesForProtocol(channel, protocolHint string, requireStateful bool, candidates []cpamsCandidate) []cpamsCandidate {
	channel = strings.ToLower(strings.TrimSpace(channel))
	protocolHint = strings.ToLower(strings.TrimSpace(protocolHint))
	if len(candidates) == 0 {
		return nil
	}
	protocolBase, protocolTraits := cpamsSplitProtocolHint(protocolHint)
	if channel == "claude" && protocolTraits["claude-external-native"] {
		bridges := make([]cpamsCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			if cpamsIsBridgeCandidate(candidate) {
				bridges = append(bridges, candidate)
			}
		}
		if len(bridges) > 0 {
			candidates = bridges
		}
		filtered := make([]cpamsCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			prefix, _ := splitCPAMSModel(candidate.FullModel)
			if cpamsSupportsExternalNativeClaudeCLI(prefix, candidate.Reason) {
				filtered = append(filtered, candidate)
			}
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}
	if channel != "codex" {
		return candidates
	}
	switch protocolBase {
	case "responses", "responses/compact":
	default:
		return candidates
	}
	if !requireStateful {
		return candidates
	}

	filtered := make([]cpamsCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		prefix, _ := splitCPAMSModel(candidate.FullModel)
		if cpamsSupportsStatefulCodexResponses(prefix, candidate.Reason, candidate.ResponsesContinuationStatus, candidate.ResponsesContinuationReason) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func cpamsPickBridgeCandidate(candidates []cpamsCandidate) (cpamsCandidate, bool) {
	for _, candidate := range candidates {
		if cpamsIsBridgeCandidate(candidate) {
			return candidate, true
		}
	}
	return cpamsCandidate{}, false
}

func cpamsSplitProtocolHint(protocolHint string) (string, map[string]bool) {
	protocolHint = strings.ToLower(strings.TrimSpace(protocolHint))
	if protocolHint == "" {
		return "", nil
	}
	parts := strings.Split(protocolHint, "+")
	base := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		return base, nil
	}
	traits := make(map[string]bool, len(parts)-1)
	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		traits[part] = true
	}
	return base, traits
}

func cpamsSupportsExternalNativeClaudeCLI(prefix, reason string) bool {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.Contains(reason, "claude_external_cli_native_ok"),
		strings.Contains(reason, "claude_native_external_ok"),
		strings.Contains(reason, "external_native_claude_ok"),
		strings.Contains(reason, "native_claude_cli_ok"):
		return true
	}
	return cpamsKnownExternalNativeClaudePrefix(prefix)
}

func cpamsKnownExternalNativeClaudePrefix(prefix string) bool {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "covs":
		return true
	default:
		return false
	}
}

func cpamsSupportsStatefulCodexResponses(prefix, reason, continuationStatus, continuationReason string) bool {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	reason = strings.ToLower(strings.TrimSpace(reason))
	continuationStatus = strings.ToLower(strings.TrimSpace(continuationStatus))
	continuationReason = strings.ToLower(strings.TrimSpace(continuationReason))
	switch continuationStatus {
	case "failed":
		return false
	case "ok":
		return true
	}
	if cpamsReasonDisablesStatefulCodexResponses(continuationReason) {
		return false
	}
	if strings.Contains(continuationReason, "stateful_responses_ok") ||
		strings.Contains(continuationReason, "agent_responses_ok") ||
		strings.Contains(continuationReason, "responses_stateful_ok") {
		return true
	}
	if cpamsReasonDisablesStatefulCodexResponses(reason) {
		return false
	}
	if cpamsKnownStatefulCodexPrefix(prefix) {
		return true
	}
	return strings.Contains(reason, "stateful_responses_ok") ||
		strings.Contains(reason, "agent_responses_ok") ||
		strings.Contains(reason, "responses_stateful_ok") ||
		strings.Contains(reason, "text_only_ok")
}

func cpamsStatefulCandidateDiagnostic(protocolCandidates, statefulCandidates []cpamsCandidate) string {
	if len(protocolCandidates) == 0 {
		return ""
	}
	if len(statefulCandidates) == 0 {
		return "; available candidates are text-only or continuation-unverified: " + cpamsFormatStatefulCandidates(protocolCandidates)
	}
	return "; stateful candidates are temporarily blocked after recent failures: " + cpamsFormatStatefulCandidates(statefulCandidates)
}

func cpamsFormatStatefulCandidates(candidates []cpamsCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		model := strings.TrimSpace(candidate.FullModel)
		if model == "" {
			continue
		}
		reason := strings.TrimSpace(candidate.ResponsesContinuationReason)
		if reason == "" {
			switch strings.ToLower(strings.TrimSpace(candidate.ResponsesContinuationStatus)) {
			case "ok":
				reason = "stateful_responses_ok"
			case "failed":
				reason = "responses_continuation_failed"
			default:
				reason = strings.TrimSpace(candidate.Reason)
			}
		}
		if reason == "" {
			parts = append(parts, model)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s(%s)", model, reason))
	}
	return strings.Join(parts, ", ")
}

func cpamsIsNetworkFailureReason(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	return strings.Contains(reason, "timeout") ||
		strings.Contains(reason, "network") ||
		strings.Contains(reason, "connection")
}

func cpamsFreshnessScore(now, testedAt time.Time) int {
	if now.IsZero() || testedAt.IsZero() {
		return 0
	}
	age := now.Sub(testedAt)
	if age < 0 {
		age = -age
	}
	switch {
	case age <= 30*time.Minute:
		return 40
	case age <= 2*time.Hour:
		return 20
	case age <= 6*time.Hour:
		return 5
	case age <= 24*time.Hour:
		return -10
	default:
		return -30
	}
}

func cpamsResolveTestedAt(snapshot *cpamsMultiSnapshot, entry cpamsMultiEntry, model cpamsModelHealth) time.Time {
	if ts := cpamsParseTime(model.TestedAt); !ts.IsZero() {
		return ts
	}
	if ts := cpamsParseTime(entry.UpdatedAt); !ts.IsZero() {
		return ts
	}
	if snapshot != nil {
		if ts := cpamsParseTime(snapshot.UpdatedAt); !ts.IsZero() {
			return ts
		}
	}
	return time.Time{}
}

func cpamsParseTime(raw string) time.Time {
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}
	}
	return ts.UTC()
}

func maxCPAMSTime(left, right time.Time) time.Time {
	if left.IsZero() {
		return right
	}
	if right.IsZero() {
		return left
	}
	if right.After(left) {
		return right
	}
	return left
}

func (r *cpamsResolver) Snapshot(channelRaw string) (map[string]cpamsChannelState, error) {
	if r == nil {
		return map[string]cpamsChannelState{}, nil
	}

	filterChannel := ""
	if strings.TrimSpace(channelRaw) != "" {
		channel, err := normalizeCPAMSChannel(channelRaw)
		if err != nil {
			return nil, err
		}
		filterChannel = channel
	}

	now := r.now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	channelsSet := make(map[string]struct{})
	for channel := range r.channelTokens {
		channelsSet[channel] = struct{}{}
	}
	for channel := range r.selected {
		channelsSet[channel] = struct{}{}
	}
	if filterChannel != "" {
		channelsSet = map[string]struct{}{filterChannel: {}}
	}

	channels := make([]string, 0, len(channelsSet))
	for channel := range channelsSet {
		channels = append(channels, channel)
	}
	sort.Strings(channels)

	out := make(map[string]cpamsChannelState, len(channels))
	for _, channel := range channels {
		validTokens := r.collectValidTokensLocked(channel, now)
		selection := r.selected[channel]
		if selection.Model != "" && !now.Before(selection.ExpiresAt) {
			delete(r.selected, channel)
			selection = cpamsSelection{}
		}

		tokens := make([]cpamsTokenInfo, 0, len(validTokens))
		for model, expiresAt := range validTokens {
			expiresIn := expiresAt.Sub(now).Seconds()
			if expiresIn < 0 {
				expiresIn = 0
			}
			tokens = append(tokens, cpamsTokenInfo{
				Model:        model,
				ExpiresAt:    expiresAt,
				ExpiresInSec: int64(expiresIn),
			})
		}
		sort.Slice(tokens, func(i, j int) bool {
			if tokens[i].ExpiresAt.Equal(tokens[j].ExpiresAt) {
				return tokens[i].Model < tokens[j].Model
			}
			return tokens[i].ExpiresAt.After(tokens[j].ExpiresAt)
		})

		out[channel] = cpamsChannelState{
			Selection: selection,
			Tokens:    tokens,
		}
	}

	return out, nil
}

func (r *cpamsResolver) InvalidateModel(channelRaw, requestedModelRaw, selectedModelRaw, conversationKey string, snapshotUpdatedAt time.Time) {
	r.invalidateModel(channelRaw, requestedModelRaw, selectedModelRaw, conversationKey, snapshotUpdatedAt, true, 0)
}

func (r *cpamsResolver) EvictModel(channelRaw, requestedModelRaw, selectedModelRaw, conversationKey string, snapshotUpdatedAt time.Time) {
	r.invalidateModel(channelRaw, requestedModelRaw, selectedModelRaw, conversationKey, snapshotUpdatedAt, false, 0)
}

func (r *cpamsResolver) CooldownModel(channelRaw, requestedModelRaw, selectedModelRaw, conversationKey string, snapshotUpdatedAt time.Time, cooldown time.Duration) {
	if cooldown <= 0 {
		r.EvictModel(channelRaw, requestedModelRaw, selectedModelRaw, conversationKey, snapshotUpdatedAt)
		return
	}
	r.invalidateModel(channelRaw, requestedModelRaw, selectedModelRaw, conversationKey, snapshotUpdatedAt, true, cooldown)
}

func (r *cpamsResolver) invalidateModel(channelRaw, requestedModelRaw, selectedModelRaw, conversationKey string, snapshotUpdatedAt time.Time, blockCandidate bool, blockTTL time.Duration) {
	if r == nil {
		return
	}
	channel, err := normalizeCPAMSChannel(channelRaw)
	if err != nil {
		return
	}
	selectedModel := cpamsSelectionModelName(selectedModelRaw)
	if selectedModel == "" {
		return
	}
	requestedModel := cpamsCanonicalRequestedModel(requestedModelRaw)
	now := r.now().UTC()

	r.mu.Lock()
	defer r.mu.Unlock()

	if tokens := r.channelTokens[channel]; len(tokens) > 0 {
		delete(tokens, selectedModel)
		if len(tokens) == 0 {
			delete(r.channelTokens, channel)
		}
	}
	if sel, ok := r.selected[channel]; ok && cpamsSameModel(sel.Model, selectedModel) {
		delete(r.selected, channel)
	}

	stickyKey := cpamsStickyKey(channel, requestedModel, conversationKey)
	for key, sel := range r.sticky {
		if !now.Before(sel.ExpiresAt) {
			delete(r.sticky, key)
			continue
		}
		if stickyKey != "" && key == stickyKey {
			delete(r.sticky, key)
			continue
		}
		if !cpamsSameModel(sel.Model, selectedModel) {
			continue
		}
		if requestedModel != "" && !strings.EqualFold(strings.TrimSpace(cpamsCanonicalRequestedModel(sel.RequestedModel)), requestedModel) {
			continue
		}
		delete(r.sticky, key)
	}
	if blockCandidate && requestedModel != "" {
		if r.failed == nil {
			r.failed = make(map[string]cpamsFailureBlock)
		}
		block := cpamsFailureBlock{
			RequestedModel:  requestedModel,
			SnapshotUpdated: snapshotUpdatedAt,
			FailedAt:        now,
		}
		if blockTTL > 0 {
			block.ExpiresAt = now.Add(blockTTL)
		}
		r.failed[cpamsFailureKey(channel, requestedModel, selectedModel)] = block
	}
}

func (r *cpamsResolver) RememberResponseIDs(channelRaw, requestedModelRaw, selectedModelRaw string, responseIDs []string, snapshotUpdatedAt time.Time) {
	if r == nil || len(responseIDs) == 0 {
		return
	}
	channel, err := normalizeCPAMSChannel(channelRaw)
	if err != nil {
		return
	}
	selectedModel := cpamsSelectionModelName(selectedModelRaw)
	requestedModel := cpamsCanonicalRequestedModel(requestedModelRaw)
	if channel == "" || selectedModel == "" || requestedModel == "" {
		return
	}

	now := r.now().UTC()
	expiresAt := now.Add(r.tokenTTL)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.channelTokens == nil {
		r.channelTokens = make(map[string]map[string]time.Time)
	}
	if r.sticky == nil {
		r.sticky = make(map[string]cpamsSelection)
	}
	if tokens := r.channelTokens[channel]; tokens != nil {
		if exp, ok := tokens[selectedModel]; ok && exp.After(expiresAt) {
			expiresAt = exp
		}
	}
	r.ensureTokenLocked(channel, selectedModel, expiresAt)

	seen := make(map[string]struct{}, len(responseIDs))
	for _, responseID := range responseIDs {
		responseID = strings.TrimSpace(responseID)
		if responseID == "" {
			continue
		}
		if _, ok := seen[responseID]; ok {
			continue
		}
		seen[responseID] = struct{}{}

		stickyKey := cpamsStickyKey(channel, requestedModel, "previous_response_id:"+responseID)
		if stickyKey == "" {
			continue
		}
		r.sticky[stickyKey] = cpamsSelection{
			Model:           selectedModel,
			RequestedModel:  requestedModel,
			ExpiresAt:       expiresAt,
			SnapshotUpdated: snapshotUpdatedAt,
			Decision:        "bind_previous_response_id",
			ChosenAt:        now,
		}
	}
}

func (r *cpamsResolver) TokenTTL() time.Duration {
	if r == nil {
		return 0
	}
	return r.tokenTTL
}

func (r *cpamsResolver) SnapshotPath() string {
	if r == nil {
		return ""
	}
	return r.snapshotPath
}

func normalizeCPAMSChannel(channelRaw string) (string, error) {
	channel := strings.ToLower(strings.TrimSpace(channelRaw))
	switch channel {
	case "codex", "claude":
		return channel, nil
	default:
		return "", fmt.Errorf("cpams: unsupported channel %q", channelRaw)
	}
}

func cpamsSelectionModelName(model string) string {
	return strings.TrimSpace(thinking.ParseSuffix(strings.TrimSpace(model)).ModelName)
}

func cpamsSameModel(left, right string) bool {
	left = cpamsSelectionModelName(left)
	right = cpamsSelectionModelName(right)
	if left == "" || right == "" {
		return false
	}
	return strings.EqualFold(left, right)
}

func cpamsFailureKey(channel, requestedModel, selectedModel string) string {
	return strings.ToLower(strings.TrimSpace(channel)) + "|" +
		strings.ToLower(strings.TrimSpace(cpamsCanonicalRequestedModel(requestedModel))) + "|" +
		strings.ToLower(strings.TrimSpace(cpamsSelectionModelName(selectedModel)))
}

func (r *cpamsResolver) filterBlockedCandidates(channel, requestedModel string, snapshotUpdatedAt time.Time, candidates []cpamsCandidate) []cpamsCandidate {
	if r == nil || len(candidates) == 0 {
		return candidates
	}
	requestedModel = cpamsCanonicalRequestedModel(requestedModel)
	if requestedModel == "" {
		return candidates
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.failed) == 0 {
		return candidates
	}

	filtered := candidates[:0]
	now := r.now().UTC()
	for _, candidate := range candidates {
		key := cpamsFailureKey(channel, requestedModel, candidate.FullModel)
		block, ok := r.failed[key]
		if !ok {
			filtered = append(filtered, candidate)
			continue
		}
		if !block.ExpiresAt.IsZero() && !now.Before(block.ExpiresAt) {
			delete(r.failed, key)
			filtered = append(filtered, candidate)
			continue
		}
		releaseAt := block.SnapshotUpdated
		if block.FailedAt.After(releaseAt) {
			releaseAt = block.FailedAt
		}
		candidateUpdatedAt := candidate.LatestTestedAt
		if candidateUpdatedAt.IsZero() {
			candidateUpdatedAt = snapshotUpdatedAt
		}
		if candidateUpdatedAt.After(releaseAt) {
			delete(r.failed, key)
			filtered = append(filtered, candidate)
			continue
		}
	}
	return filtered
}
