package auth

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	routingEngineV1     = "v1"
	routingEngineV2     = "v2"
	routingEngineShadow = "shadow"

	routingEventBufferSize = 128

	legacyRouteObserverMetadataKey = "cliproxy.route_observer"
)

type RoutingPlan struct {
	Engine               string
	RequestedModel       string
	RequestedProviders   []string
	ProviderOrder        []string
	ModelCandidates      []string
	AllowProjectSwitch   bool
	AllowPreviewFallback bool
	StrictPublicRouting  bool
}

type RoutingAttemptContext struct {
	RequestID                       string
	RequestedModel                  string
	TriedRouteKeys                  map[string]struct{}
	TriedProviders                  map[string]struct{}
	TriedAuths                      map[string]struct{}
	BlockedAuths                    map[string]string
	BlockedProviderModels           map[string]string
	CurrentProvider                 string
	CurrentProviderIndex            int
	CurrentFallbackDepth            int
	PrimaryRouteChosen              bool
	AdvancePrimaryProvider          bool
	LastFailureReason               string
	EffectiveProviderOffsetSnapshot map[string]int
	FallbackTrace                   []FallbackEvent
}

type FallbackEvent struct {
	Stage  string `json:"stage"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type CandidateRoute struct {
	Provider string
	AuthID   string
	Model    string
	Reason   string
}

type RoutingEvent struct {
	OccurredAt             string         `json:"occurred_at,omitempty"`
	RequestID              string         `json:"request_id,omitempty"`
	EngineVersion          string         `json:"engine_version,omitempty"`
	RequestedModel         string         `json:"requested_model,omitempty"`
	RequestedProviders     []string       `json:"requested_providers,omitempty"`
	ChosenProvider         string         `json:"chosen_provider,omitempty"`
	ChosenAuth             string         `json:"chosen_auth,omitempty"`
	ChosenModel            string         `json:"chosen_model,omitempty"`
	ChosenPrefix           string         `json:"chosen_prefix,omitempty"`
	ChosenLabel            string         `json:"chosen_label,omitempty"`
	ChosenBaseURL          string         `json:"chosen_base_url,omitempty"`
	FallbackStage          string         `json:"fallback_stage,omitempty"`
	FallbackReason         string         `json:"fallback_reason,omitempty"`
	ErrorMessage           string         `json:"error_message,omitempty"`
	SelectorStrategy       string         `json:"selector_strategy,omitempty"`
	ProviderOffsetSnapshot map[string]int `json:"provider_offset_snapshot,omitempty"`
	ResultStatus           string         `json:"result_status,omitempty"`
	StateTransition        string         `json:"state_transition,omitempty"`
	CompareProvider        string         `json:"compare_provider,omitempty"`
	CompareAuth            string         `json:"compare_auth,omitempty"`
	CompareModel           string         `json:"compare_model,omitempty"`
	CompareDiff            string         `json:"compare_diff,omitempty"`
}

func routingEventFromSelection(
	attempt *RoutingAttemptContext,
	plan RoutingPlan,
	route *CandidateRoute,
	auth *Auth,
	manager *Manager,
	status string,
	transition string,
	reason string,
	err error,
) RoutingEvent {
	event := RoutingEvent{
		OccurredAt:             time.Now().UTC().Format(time.RFC3339Nano),
		RequestID:              attempt.RequestID,
		EngineVersion:          routingEngineV2,
		RequestedModel:         plan.RequestedModel,
		RequestedProviders:     plan.RequestedProviders,
		FallbackStage:          routeFallbackStage(attempt),
		FallbackReason:         reason,
		SelectorStrategy:       manager.currentRoutingEngineSelectorLabel(),
		ProviderOffsetSnapshot: attempt.EffectiveProviderOffsetSnapshot,
		ResultStatus:           status,
		StateTransition:        transition,
	}
	if err != nil {
		event.ErrorMessage = strings.TrimSpace(err.Error())
	}
	if route != nil {
		event.ChosenProvider = route.Provider
		event.ChosenAuth = route.AuthID
		event.ChosenModel = route.Model
	}
	if auth != nil {
		event.ChosenPrefix = strings.TrimSpace(auth.Prefix)
		event.ChosenLabel = strings.TrimSpace(auth.Label)
		if auth.Attributes != nil {
			event.ChosenBaseURL = strings.TrimSpace(auth.Attributes["base_url"])
		}
	}
	return event
}

type routingFailurePolicy struct {
	Reason             string
	Fatal              bool
	BlockAuth          bool
	BlockProviderModel bool
	StateLevel         string
	StateTransition    string
}

type RoutingErrorAction struct {
	Name               string `json:"name"`
	HTTPStatus         int    `json:"http_status"`
	Reason             string `json:"reason"`
	StateLevel         string `json:"state_level"`
	StateTransition    string `json:"state_transition"`
	Fatal              bool   `json:"fatal"`
	BlockAuth          bool   `json:"block_auth"`
	BlockProviderModel bool   `json:"block_provider_model"`
	Description        string `json:"description,omitempty"`
}

func normalizeRoutingEngine(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case routingEngineV2:
		return routingEngineV2
	case routingEngineShadow:
		return routingEngineShadow
	default:
		return routingEngineV1
	}
}

func (m *Manager) routingEngine() string {
	if m == nil {
		return routingEngineV1
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return routingEngineV1
	}
	return normalizeRoutingEngine(cfg.Routing.Engine)
}

func (m *Manager) routingStrategy() string {
	if m == nil {
		return "round-robin"
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return "round-robin"
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy)) {
	case "fill-first", "fillfirst", "ff":
		return "fill-first"
	case "health-aware", "health", "weighted":
		return "health-aware"
	default:
		return "round-robin"
	}
}

func (m *Manager) buildRoutingPlan(req cliproxyexecutor.Request, providers []string, opts cliproxyexecutor.Options, engine string) RoutingPlan {
	strict := strictPublicModelRoutingEnabled(opts.Metadata)
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	allowPreviewFallback := cfg.QuotaExceeded.SwitchPreviewModel && !strict
	plan := RoutingPlan{
		Engine:               normalizeRoutingEngine(engine),
		RequestedModel:       strings.TrimSpace(req.Model),
		RequestedProviders:   append([]string(nil), providers...),
		ProviderOrder:        append([]string(nil), providers...),
		ModelCandidates:      buildRoutingModelCandidates(strings.TrimSpace(req.Model), providers, allowPreviewFallback),
		AllowProjectSwitch:   cfg.QuotaExceeded.SwitchProject,
		AllowPreviewFallback: allowPreviewFallback,
		StrictPublicRouting:  strict,
	}
	if len(plan.ModelCandidates) == 0 && plan.RequestedModel != "" {
		plan.ModelCandidates = []string{plan.RequestedModel}
	}
	return plan
}

func buildRoutingModelCandidates(requestedModel string, providers []string, allowPreviewFallback bool) []string {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil
	}
	seen := map[string]struct{}{strings.ToLower(requestedModel): {}}
	out := []string{requestedModel}
	if !allowPreviewFallback {
		return out
	}
	for _, provider := range providers {
		for _, candidate := range fallbackModelsForProvider(provider, requestedModel) {
			key := strings.ToLower(strings.TrimSpace(candidate))
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, candidate)
		}
	}
	return out
}

func fallbackModelsForProvider(provider, requestedModel string) []string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "claude":
		if resolved := resolveModelFromCompatibilityMap(requestedModel, map[string]string{
			"claude-opus-4-6":            "claude-sonnet-4-6",
			"claude-opus-4-6-thinking":   "claude-sonnet-4-6",
			"claude-sonnet-4-5":          "claude-sonnet-4-5-20250929",
			"claude-sonnet-4-5-thinking": "claude-sonnet-4-5-20250929",
			"claude-haiku-4-5":           "claude-haiku-4-5-20251001",
		}); resolved != "" && !strings.EqualFold(resolved, requestedModel) {
			return []string{resolved}
		}
	case "codex":
		if resolved := resolveModelFromCompatibilityMap(requestedModel, map[string]string{
			"codex-mini-latest":  "gpt-5.3-codex",
			"gpt-5-codex":        "gpt-5.3-codex",
			"gpt-5.1-codex":      "gpt-5.3-codex",
			"gpt-5.1-codex-max":  "gpt-5.3-codex",
			"gpt-5.1-codex-mini": "gpt-5.3-codex",
			"gpt-5.2-codex":      "gpt-5.3-codex",
		}); resolved != "" && !strings.EqualFold(resolved, requestedModel) {
			return []string{resolved}
		}
	}
	return nil
}

func newRoutingAttemptContext(ctx context.Context, plan RoutingPlan, snapshot map[string]int, advancePrimary bool) *RoutingAttemptContext {
	attempt := &RoutingAttemptContext{
		TriedRouteKeys:                  make(map[string]struct{}),
		TriedProviders:                  make(map[string]struct{}),
		TriedAuths:                      make(map[string]struct{}),
		BlockedAuths:                    make(map[string]string),
		BlockedProviderModels:           make(map[string]string),
		AdvancePrimaryProvider:          advancePrimary,
		EffectiveProviderOffsetSnapshot: make(map[string]int, len(snapshot)),
	}
	for key, value := range snapshot {
		attempt.EffectiveProviderOffsetSnapshot[key] = value
	}
	if ctx != nil {
		attempt.RequestID = logging.GetRequestID(ctx)
	}
	attempt.RequestedModel = strings.TrimSpace(plan.RequestedModel)
	return attempt
}

func routeKey(provider, authID, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "|" + strings.TrimSpace(authID) + "|" + strings.TrimSpace(model)
}

func providerModelKey(provider, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "|" + canonicalModelKey(model)
}

func (a *RoutingAttemptContext) currentModel(plan RoutingPlan) string {
	if a == nil || len(plan.ModelCandidates) == 0 {
		return ""
	}
	idx := a.CurrentFallbackDepth
	if idx < 0 {
		idx = 0
	}
	if idx >= len(plan.ModelCandidates) {
		idx = len(plan.ModelCandidates) - 1
	}
	return plan.ModelCandidates[idx]
}

func (a *RoutingAttemptContext) isAuthBlocked(authID string) bool {
	if a == nil {
		return false
	}
	_, ok := a.BlockedAuths[authID]
	return ok
}

func (a *RoutingAttemptContext) isProviderBlockedForModel(provider, model string) bool {
	if a == nil {
		return false
	}
	_, ok := a.BlockedProviderModels[providerModelKey(provider, model)]
	return ok
}

func (a *RoutingAttemptContext) markRouteTried(route CandidateRoute) {
	if a == nil {
		return
	}
	if key := routeKey(route.Provider, route.AuthID, route.Model); key != "" {
		a.TriedRouteKeys[key] = struct{}{}
	}
	if strings.TrimSpace(route.Reason) != "" && !strings.EqualFold(strings.TrimSpace(route.Reason), strings.TrimSpace(route.Model)) {
		a.TriedRouteKeys[routeKey(route.Provider, route.AuthID, route.Reason)] = struct{}{}
	}
	if route.Provider != "" {
		a.TriedProviders[route.Provider] = struct{}{}
	}
	if route.AuthID != "" {
		a.TriedAuths[route.AuthID] = struct{}{}
	}
}

func (a *RoutingAttemptContext) hasTriedRoute(provider, authID, model string) bool {
	if a == nil {
		return false
	}
	_, ok := a.TriedRouteKeys[routeKey(provider, authID, model)]
	return ok
}

func (a *RoutingAttemptContext) blockAuth(authID, reason string) {
	if a == nil || strings.TrimSpace(authID) == "" {
		return
	}
	a.BlockedAuths[authID] = strings.TrimSpace(reason)
}

func (a *RoutingAttemptContext) blockProviderModel(provider, model, reason string) {
	if a == nil {
		return
	}
	key := providerModelKey(provider, model)
	if key == "" {
		return
	}
	a.BlockedProviderModels[key] = strings.TrimSpace(reason)
}

func (a *RoutingAttemptContext) noteFallback(stage, from, to, reason string) {
	if a == nil {
		return
	}
	a.FallbackTrace = append(a.FallbackTrace, FallbackEvent{
		Stage:  strings.TrimSpace(stage),
		From:   strings.TrimSpace(from),
		To:     strings.TrimSpace(to),
		Reason: strings.TrimSpace(reason),
	})
}

func rotateProviders(providers []string, offset int) []string {
	if len(providers) == 0 {
		return nil
	}
	if offset < 0 {
		offset = 0
	}
	offset = offset % len(providers)
	if offset == 0 {
		return append([]string(nil), providers...)
	}
	out := make([]string, 0, len(providers))
	out = append(out, providers[offset:]...)
	out = append(out, providers[:offset]...)
	return out
}

func (m *Manager) providerOffsetSnapshot(model string) map[string]int {
	out := make(map[string]int)
	if m == nil {
		return out
	}
	key := canonicalModelKey(model)
	if key == "" {
		return out
	}
	m.mu.RLock()
	out[key] = m.providerOffsets[key]
	m.mu.RUnlock()
	return out
}

func (m *Manager) advancePrimaryProviderOffset(model string, providers []string) {
	if m == nil {
		return
	}
	key := canonicalModelKey(model)
	if key == "" || len(providers) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.providerOffsets[key]
	next := current + 1
	if next >= len(providers) {
		next = 0
	}
	m.providerOffsets[key] = next
}

func (m *Manager) pickNextProvider(plan RoutingPlan, attempt *RoutingAttemptContext) (string, error) {
	if attempt == nil {
		return "", &Error{Code: "routing_attempt_missing", Message: "routing attempt context is nil"}
	}
	currentModel := attempt.currentModel(plan)
	if currentModel == "" {
		return "", &Error{Code: "routing_model_missing", Message: "routing model is empty"}
	}
	ordered := append([]string(nil), plan.ProviderOrder...)
	if key := canonicalModelKey(plan.RequestedModel); key != "" {
		ordered = rotateProviders(ordered, attempt.EffectiveProviderOffsetSnapshot[key])
	}
	weighted := m.orderProvidersByRuntimeScore(ordered)
	if attempt.CurrentProvider != "" && !attempt.isProviderBlockedForModel(attempt.CurrentProvider, currentModel) {
		return attempt.CurrentProvider, nil
	}
	for i := 0; i < len(weighted); i++ {
		provider := weighted[i]
		if attempt.isProviderBlockedForModel(provider, currentModel) {
			continue
		}
		attempt.CurrentProvider = provider
		attempt.CurrentProviderIndex = i
		if !attempt.PrimaryRouteChosen {
			attempt.PrimaryRouteChosen = true
			if attempt.AdvancePrimaryProvider {
				m.advancePrimaryProviderOffset(plan.RequestedModel, plan.ProviderOrder)
			}
		}
		return provider, nil
	}
	attempt.CurrentProvider = ""
	return "", &Error{Code: "provider_not_found", Message: "no provider available for current routing model", HTTPStatus: http.StatusServiceUnavailable}
}

func (m *Manager) orderProvidersByRuntimeScore(providers []string) []string {
	if len(providers) <= 1 {
		return append([]string(nil), providers...)
	}
	now := time.Now().UTC()
	ordered := append([]string(nil), providers...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left := m.providerSelectionScore(ordered[i], now)
		right := m.providerSelectionScore(ordered[j], now)
		if math.Abs(left-right) < 0.0001 {
			return false
		}
		return left > right
	})
	return ordered
}

func (m *Manager) collectAuthCandidates(provider, model string, attempt *RoutingAttemptContext) ([]*Auth, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	requestedPrefix := ""
	if attempt != nil {
		requestedPrefix = requestedAuthPrefixLock(map[string]any{
			cliproxyexecutor.RequestedModelMetadataKey: attempt.RequestedModel,
		}, model)
	}
	modelKey := strings.TrimSpace(model)
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	m.mu.RLock()
	registryRef := registry.GetGlobalRegistry()
	candidates := make([]*Auth, 0, len(m.auths))
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		providerKey := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if providerKey != provider {
			continue
		}
		if !authMatchesRequestedPrefix(candidate, requestedPrefix) {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if attempt != nil {
			if attempt.isAuthBlocked(candidate.ID) {
				continue
			}
			if attempt.hasTriedRoute(providerKey, candidate.ID, model) {
				continue
			}
		}
		if modelKey != "" && !m.authSupportsRequestedModel(candidate, modelKey, registryRef) {
			continue
		}
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	if len(candidates) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable}
	}
	for i := range candidates {
		candidates[i].Runtime = m.buildAuthSelectionRuntime(context.Background(), candidates[i], model)
	}
	return candidates, nil
}

func (m *Manager) pickNextAuthWithinProvider(ctx context.Context, provider string, auths []*Auth, model string, opts cliproxyexecutor.Options) (*Auth, ProviderExecutor, error) {
	auths = filterCandidatesByProtocol(provider, opts, auths)
	if len(auths) == 0 {
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable}
	}
	m.mu.RLock()
	selector := m.selector
	executor := m.executors[strings.ToLower(strings.TrimSpace(provider))]
	m.mu.RUnlock()
	if stickySelected, ok := m.pickCandidateByAffinity(affinityScopeForProvider(provider), model, opts, auths); ok && stickySelected != nil {
		if executor == nil {
			return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		authCopy := stickySelected.Clone()
		if !stickySelected.indexAssigned {
			m.mu.Lock()
			if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
				current.EnsureIndex()
				authCopy = current.Clone()
			}
			m.mu.Unlock()
		}
		return authCopy, executor, nil
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	selected, err := selector.Pick(ctx, provider, model, opts, auths)
	if err != nil {
		return nil, nil, err
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth", HTTPStatus: http.StatusServiceUnavailable}
	}
	if executor == nil {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
}

func (m *Manager) resolveRouteModelForAuth(plan RoutingPlan, auth *Auth, routeModel string) (string, bool) {
	if auth == nil {
		return "", false
	}
	routeModel = normalizeRequestedModelForAuth(auth, routeModel)
	resolved := rewriteModelForAuth(routeModel, auth)
	resolved = m.applyOAuthModelAlias(auth, resolved)
	resolved = m.applyAPIKeyModelAlias(auth, resolved)
	if plan.StrictPublicRouting && !sameStrictPublicModel(plan.RequestedModel, routeModel) {
		return "", false
	}
	return resolved, true
}

func (m *Manager) pickNextRouteFromPlan(ctx context.Context, plan RoutingPlan, attempt *RoutingAttemptContext, opts cliproxyexecutor.Options) (*CandidateRoute, ProviderExecutor, *Auth, error) {
	if attempt == nil {
		return nil, nil, nil, &Error{Code: "routing_attempt_missing", Message: "routing attempt context is nil"}
	}
	for {
		currentModel := attempt.currentModel(plan)
		if currentModel == "" {
			return nil, nil, nil, &Error{Code: "routing_model_missing", Message: "routing model is empty"}
		}
		provider, err := m.pickNextProvider(plan, attempt)
		if err != nil {
			if canAdvanceFallbackModel(plan, attempt) {
				from := currentModel
				attempt.CurrentFallbackDepth++
				attempt.CurrentProvider = ""
				attempt.CurrentProviderIndex = 0
				to := attempt.currentModel(plan)
				attempt.noteFallback("model", from, to, attempt.LastFailureReason)
				continue
			}
			return nil, nil, nil, err
		}
		candidates, err := m.collectAuthCandidates(provider, currentModel, attempt)
		if err != nil {
			attempt.blockProviderModel(provider, currentModel, "no_auth")
			attempt.CurrentProvider = ""
			attempt.CurrentProviderIndex++
			continue
		}
		auth, executor, err := m.pickNextAuthWithinProvider(ctx, provider, candidates, currentModel, opts)
		if err != nil {
			attempt.blockProviderModel(provider, currentModel, "selector_error")
			attempt.CurrentProvider = ""
			attempt.CurrentProviderIndex++
			continue
		}
		resolvedModel, ok := m.resolveRouteModelForAuth(plan, auth, currentModel)
		if !ok {
			attempt.markRouteTried(CandidateRoute{Provider: provider, AuthID: auth.ID, Model: currentModel})
			continue
		}
		if attempt.hasTriedRoute(provider, auth.ID, resolvedModel) {
			attempt.markRouteTried(CandidateRoute{Provider: provider, AuthID: auth.ID, Model: resolvedModel})
			continue
		}
		return &CandidateRoute{
			Provider: provider,
			AuthID:   auth.ID,
			Model:    resolvedModel,
			Reason:   currentModel,
		}, executor, auth, nil
	}
}

func canAdvanceFallbackModel(plan RoutingPlan, attempt *RoutingAttemptContext) bool {
	if attempt == nil || !plan.AllowPreviewFallback {
		return false
	}
	if attempt.CurrentFallbackDepth+1 >= len(plan.ModelCandidates) {
		return false
	}
	switch attempt.LastFailureReason {
	case "", "quota", "model_not_found", "transient", "provider_exhausted":
		return true
	default:
		return false
	}
}

func classifyRoutingFailure(err error, plan RoutingPlan) routingFailurePolicy {
	policy := routingFailurePolicy{
		Reason:          "request_failed",
		StateLevel:      "route",
		StateTransition: "route.failed",
	}
	statusCode := statusCodeFromError(err)
	switch statusCode {
	case http.StatusBadRequest:
		if isRequestInvalidError(err) {
			policy.Reason = "invalid_request"
			policy.Fatal = true
			policy.StateLevel = "request"
			policy.StateTransition = "request.invalid"
			return policy
		}
		if isProtocolMismatchBadRequest(err) {
			policy.Reason = "protocol_mismatch"
			policy.BlockProviderModel = true
			policy.StateLevel = "provider_model"
			policy.StateTransition = "provider_model.protocol_mismatch"
			return policy
		}
		policy.Reason = "bad_request"
		policy.StateLevel = "request"
		policy.StateTransition = "request.bad"
	case http.StatusUnauthorized:
		policy.Reason = "unauthorized"
		policy.BlockAuth = true
		policy.StateLevel = "auth"
		policy.StateTransition = "auth.unauthorized"
	case http.StatusPaymentRequired, http.StatusForbidden:
		if isDailyQuotaExceededError(&Error{HTTPStatus: statusCode, Message: err.Error()}) {
			policy.Reason = "daily_quota"
			policy.StateLevel = "model_on_auth"
			policy.StateTransition = "model_on_auth.daily_quota"
			if plan.AllowProjectSwitch {
				policy.BlockAuth = true
				policy.StateLevel = "auth"
				policy.StateTransition = "auth.daily_quota"
			} else {
				policy.BlockProviderModel = true
				policy.StateLevel = "provider_model"
				policy.StateTransition = "provider_model.daily_quota"
			}
			return policy
		}
		policy.Reason = "forbidden"
		policy.BlockAuth = true
		policy.StateLevel = "auth"
		policy.StateTransition = "auth.forbidden"
	case http.StatusNotFound:
		policy.Reason = "model_not_found"
		policy.StateLevel = "model_on_auth"
		policy.StateTransition = "model_on_auth.not_found"
		if !plan.AllowProjectSwitch {
			policy.BlockProviderModel = true
			policy.StateLevel = "provider_model"
			policy.StateTransition = "provider_model.not_found"
		}
	case http.StatusTooManyRequests:
		policy.Reason = "quota"
		policy.StateLevel = "model_on_auth"
		policy.StateTransition = "model_on_auth.quota"
		if !plan.AllowProjectSwitch {
			policy.BlockProviderModel = true
			policy.StateLevel = "provider_model"
			policy.StateTransition = "provider_model.quota"
		}
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		policy.Reason = "transient"
		policy.StateLevel = "route"
		policy.StateTransition = "route.transient"
	default:
		policy.Reason = "request_failed"
		policy.StateLevel = "route"
		policy.StateTransition = "route.failed"
	}
	return policy
}

func (m *Manager) applyRoutingFailure(plan RoutingPlan, attempt *RoutingAttemptContext, route CandidateRoute, err error) bool {
	attempt.markRouteTried(route)
	policy := classifyRoutingFailure(err, plan)
	attempt.LastFailureReason = policy.Reason
	attempt.noteFallback("error", route.Provider+"/"+route.AuthID+"/"+route.Model, "", policy.Reason)
	if policy.BlockAuth {
		attempt.blockAuth(route.AuthID, policy.Reason)
	}
	if policy.BlockProviderModel {
		attempt.blockProviderModel(route.Provider, route.Reason, policy.Reason)
		attempt.CurrentProvider = ""
		attempt.CurrentProviderIndex++
	}
	if policy.Fatal {
		return false
	}
	return true
}

func (m *Manager) executeLayeredOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	plan := m.buildRoutingPlan(req, providers, opts, routingEngineV2)
	attempt := newRoutingAttemptContext(ctx, plan, m.providerOffsetSnapshot(plan.RequestedModel), true)
	var lastErr error
	for {
		route, executor, auth, err := m.pickNextRouteFromPlan(ctx, plan, attempt, opts)
		if err != nil {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, err
		}
		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, route.Provider, route.Model)
		m.recordRoutingEvent(routingEventFromSelection(attempt, plan, route, auth, m, "selected", "route_selected", attempt.LastFailureReason, nil))

		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx, signalCollector := WithResultSignalCollector(execCtx)
		execReq := req
		execReq.Model = route.Model
		var resp cliproxyexecutor.Response
		startedAt := time.Now()
		errExec := m.withProviderSlot(execCtx, route.Provider, func() error {
			finishLoad := m.beginRuntimeLoad(auth, route.Provider)
			var err error
			resp, err = executor.Execute(execCtx, auth, execReq, opts)
			var resultErr *Error
			if err != nil {
				resultErr = &Error{Message: err.Error()}
				var se cliproxyexecutor.StatusError
				if errors.As(err, &se) && se != nil {
					resultErr.HTTPStatus = se.StatusCode()
				}
			}
			finishLoad(err == nil, time.Since(startedAt), resultErr)
			return err
		})
		result := Result{AuthID: auth.ID, Provider: route.Provider, Model: route.Model, Success: errExec == nil, Duration: time.Since(startedAt)}
		applyResultSignals(&result, signalCollector)
		if errExec != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return cliproxyexecutor.Response{}, errCtx
			}
			result.Error = &Error{Message: errExec.Error()}
			var se cliproxyexecutor.StatusError
			if errors.As(errExec, &se) && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			if ra := retryAfterFromError(errExec); ra != nil {
				result.RetryAfter = ra
			}
			m.MarkResult(execCtx, result)
			m.recordRoutingEvent(routingEventFromSelection(attempt, plan, route, auth, m, "failed", "route_failed", classifyRoutingFailure(errExec, plan).Reason, errExec))
			if isRequestInvalidError(errExec) {
				return cliproxyexecutor.Response{}, errExec
			}
			m.clearRequestAffinity(affinityScopeForProvider(route.Provider), route.Reason, opts, auth.ID)
			if !m.applyRoutingFailure(plan, attempt, *route, errExec) {
				return cliproxyexecutor.Response{}, errExec
			}
			lastErr = errExec
			continue
		}
		m.MarkResult(execCtx, result)
		m.recordRoutingEvent(routingEventFromSelection(attempt, plan, route, auth, m, "success", "route_succeeded", attempt.LastFailureReason, nil))
		m.rememberRequestAffinity(affinityScopeForProvider(route.Provider), route.Reason, opts, auth)
		return resp, nil
	}
}

func (m *Manager) executeCountLayeredOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	plan := m.buildRoutingPlan(req, providers, opts, routingEngineV2)
	attempt := newRoutingAttemptContext(ctx, plan, m.providerOffsetSnapshot(plan.RequestedModel), true)
	var lastErr error
	for {
		route, executor, auth, err := m.pickNextRouteFromPlan(ctx, plan, attempt, opts)
		if err != nil {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, err
		}

		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx, signalCollector := WithResultSignalCollector(execCtx)
		execReq := req
		execReq.Model = route.Model
		var resp cliproxyexecutor.Response
		startedAt := time.Now()
		errExec := m.withProviderSlot(execCtx, route.Provider, func() error {
			finishLoad := m.beginRuntimeLoad(auth, route.Provider)
			var err error
			resp, err = executor.CountTokens(execCtx, auth, execReq, opts)
			var resultErr *Error
			if err != nil {
				resultErr = &Error{Message: err.Error()}
				var se cliproxyexecutor.StatusError
				if errors.As(err, &se) && se != nil {
					resultErr.HTTPStatus = se.StatusCode()
				}
			}
			finishLoad(err == nil, time.Since(startedAt), resultErr)
			return err
		})
		result := Result{AuthID: auth.ID, Provider: route.Provider, Model: route.Model, Success: errExec == nil, Duration: time.Since(startedAt)}
		applyResultSignals(&result, signalCollector)
		if errExec != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return cliproxyexecutor.Response{}, errCtx
			}
			result.Error = &Error{Message: errExec.Error()}
			var se cliproxyexecutor.StatusError
			if errors.As(errExec, &se) && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			if ra := retryAfterFromError(errExec); ra != nil {
				result.RetryAfter = ra
			}
			m.MarkResult(execCtx, result)
			if isRequestInvalidError(errExec) {
				return cliproxyexecutor.Response{}, errExec
			}
			m.clearRequestAffinity(affinityScopeForProvider(route.Provider), route.Reason, opts, auth.ID)
			if !m.applyRoutingFailure(plan, attempt, *route, errExec) {
				return cliproxyexecutor.Response{}, errExec
			}
			lastErr = errExec
			continue
		}
		m.MarkResult(execCtx, result)
		m.rememberRequestAffinity(affinityScopeForProvider(route.Provider), route.Reason, opts, auth)
		return resp, nil
	}
}

func (m *Manager) executeStreamLayeredOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	plan := m.buildRoutingPlan(req, providers, opts, routingEngineV2)
	attempt := newRoutingAttemptContext(ctx, plan, m.providerOffsetSnapshot(plan.RequestedModel), true)
	var lastErr error
	for {
		route, executor, auth, err := m.pickNextRouteFromPlan(ctx, plan, attempt, opts)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}

		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx, signalCollector := WithResultSignalCollector(execCtx)
		execReq := req
		execReq.Model = route.Model
		m.recordRoutingEvent(routingEventFromSelection(attempt, plan, route, auth, m, "selected", "route_selected", attempt.LastFailureReason, nil))
		startedAt := time.Now()
		releaseProviderSlot, errAcquireProviderSlot := m.acquireProviderSlot(execCtx, route.Provider)
		if errAcquireProviderSlot != nil {
			return nil, errAcquireProviderSlot
		}
		finishLoad := m.beginRuntimeLoad(auth, route.Provider)
		chunks, errStream := executor.ExecuteStream(execCtx, auth, execReq, opts)
		if errStream != nil {
			releaseProviderSlot()
			rerr := &Error{Message: errStream.Error()}
			var se cliproxyexecutor.StatusError
			if errors.As(errStream, &se) && se != nil {
				rerr.HTTPStatus = se.StatusCode()
			}
			finishLoad(false, time.Since(startedAt), rerr)
			if errCtx := execCtx.Err(); errCtx != nil {
				return nil, errCtx
			}
			result := Result{AuthID: auth.ID, Provider: route.Provider, Model: route.Model, Success: false, Error: rerr, Duration: time.Since(startedAt)}
			result.RetryAfter = retryAfterFromError(errStream)
			m.MarkResult(execCtx, result)
			m.recordRoutingEvent(routingEventFromSelection(attempt, plan, route, auth, m, "failed", "route_failed", classifyRoutingFailure(errStream, plan).Reason, errStream))
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			m.clearRequestAffinity(affinityScopeForProvider(route.Provider), route.Reason, opts, auth.ID)
			if !m.applyRoutingFailure(plan, attempt, *route, errStream) {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}
		out := make(chan cliproxyexecutor.StreamChunk)
		go func(streamCtx context.Context, streamAuth *Auth, streamRoute CandidateRoute, streamChunks <-chan cliproxyexecutor.StreamChunk, signalCollector *ResultSignalCollector, release func(), finish func(bool, time.Duration, *Error)) {
			defer close(out)
			defer release()
			var failed bool
			forward := true
			for chunk := range streamChunks {
				if chunk.Err != nil && !failed {
					failed = true
					rerr := &Error{Message: chunk.Err.Error()}
					var se cliproxyexecutor.StatusError
					if errors.As(chunk.Err, &se) && se != nil {
						rerr.HTTPStatus = se.StatusCode()
					}
					result := Result{
						AuthID:     streamAuth.ID,
						Provider:   streamRoute.Provider,
						Model:      streamRoute.Model,
						Success:    false,
						Error:      rerr,
						RetryAfter: retryAfterFromError(chunk.Err),
						Duration:   time.Since(startedAt),
					}
					applyResultSignals(&result, signalCollector)
					finish(false, time.Since(startedAt), rerr)
					m.MarkResult(streamCtx, result)
					m.recordRoutingEvent(routingEventFromSelection(attempt, plan, &streamRoute, streamAuth, m, "failed", "route_failed", classifyRoutingFailure(chunk.Err, plan).Reason, chunk.Err))
					m.clearRequestAffinity(affinityScopeForProvider(streamRoute.Provider), streamRoute.Reason, opts, streamAuth.ID)
				}
				if !forward {
					continue
				}
				select {
				case <-streamCtx.Done():
					forward = false
				case out <- chunk:
				}
			}
			if !failed {
				finish(true, time.Since(startedAt), nil)
				result := Result{AuthID: streamAuth.ID, Provider: streamRoute.Provider, Model: streamRoute.Model, Success: true, Duration: time.Since(startedAt)}
				applyResultSignals(&result, signalCollector)
				m.MarkResult(streamCtx, result)
				m.recordRoutingEvent(routingEventFromSelection(attempt, plan, &streamRoute, streamAuth, m, "success", "route_succeeded", attempt.LastFailureReason, nil))
				m.rememberRequestAffinity(affinityScopeForProvider(streamRoute.Provider), streamRoute.Reason, opts, streamAuth)
			}
		}(execCtx, auth, *route, chunks, signalCollector, releaseProviderSlot, finishLoad)
		return out, nil
	}
}

func (m *Manager) previewLayeredPrimaryRoute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*CandidateRoute, error) {
	plan := m.buildRoutingPlan(req, providers, opts, routingEngineV2)
	attempt := newRoutingAttemptContext(ctx, plan, m.providerOffsetSnapshot(plan.RequestedModel), false)
	route, _, _, err := m.pickNextRouteFromPlan(ctx, plan, attempt, opts)
	return route, err
}

func (m *Manager) currentRoutingEngineSelectorLabel() string {
	return m.routingStrategy()
}

func (m *Manager) configureProviderConcurrency(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	limits := map[string]int{}
	if cfg != nil {
		for provider, limit := range cfg.Routing.ProviderConcurrency {
			key := strings.ToLower(strings.TrimSpace(provider))
			if key == "" || limit <= 0 {
				continue
			}
			limits[key] = limit
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providerConcurrencyLimits = limits
	m.providerSemaphores = make(map[string]chan struct{}, len(limits))
	for provider, limit := range limits {
		m.providerSemaphores[provider] = make(chan struct{}, limit)
	}
}

func (m *Manager) providerLimit(provider string) int {
	if m == nil {
		return 0
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.providerConcurrencyLimits[provider]
}

func (m *Manager) withProviderSlot(ctx context.Context, provider string, fn func() error) error {
	if m == nil {
		return fn()
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	m.mu.RLock()
	ch := m.providerSemaphores[provider]
	m.mu.RUnlock()
	if ch == nil {
		return fn()
	}
	select {
	case ch <- struct{}{}:
		defer func() { <-ch }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return fn()
}

func (m *Manager) acquireProviderSlot(ctx context.Context, provider string) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	m.mu.RLock()
	ch := m.providerSemaphores[provider]
	m.mu.RUnlock()
	if ch == nil {
		return func() {}, nil
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func routeFallbackStage(attempt *RoutingAttemptContext) string {
	if attempt == nil {
		return "primary"
	}
	if attempt.CurrentFallbackDepth > 0 || len(attempt.FallbackTrace) > 0 {
		return "fallback"
	}
	return "primary"
}

func RoutingErrorActionCatalog() []RoutingErrorAction {
	return []RoutingErrorAction{
		{
			Name:            "invalid_request",
			HTTPStatus:      http.StatusBadRequest,
			Reason:          "invalid_request",
			StateLevel:      "request",
			StateTransition: "request.invalid",
			Fatal:           true,
			Description:     "The request payload is malformed and routing should stop immediately.",
		},
		{
			Name:               "protocol_mismatch",
			HTTPStatus:         http.StatusBadRequest,
			Reason:             "protocol_mismatch",
			StateLevel:         "provider_model",
			StateTransition:    "provider_model.protocol_mismatch",
			BlockProviderModel: true,
			Description:        "Provider/model protocol mismatch should skip the incompatible channel and continue routing.",
		},
		{
			Name:            "unauthorized",
			HTTPStatus:      http.StatusUnauthorized,
			Reason:          "unauthorized",
			StateLevel:      "auth",
			StateTransition: "auth.unauthorized",
			BlockAuth:       true,
			Description:     "Auth credential is invalid for this request and should not be retried in the same attempt chain.",
		},
		{
			Name:            "forbidden",
			HTTPStatus:      http.StatusForbidden,
			Reason:          "forbidden",
			StateLevel:      "auth",
			StateTransition: "auth.forbidden",
			BlockAuth:       true,
			Description:     "Permission failure is treated as auth-scoped unless it matches a daily quota signal.",
		},
		{
			Name:               "daily_quota",
			HTTPStatus:         http.StatusPaymentRequired,
			Reason:             "daily_quota",
			StateLevel:         "provider_model",
			StateTransition:    "provider_model.daily_quota",
			BlockProviderModel: true,
			Description:        "Daily quota exhaustion blocks the provider/model route unless project switching is enabled.",
		},
		{
			Name:               "quota",
			HTTPStatus:         http.StatusTooManyRequests,
			Reason:             "quota",
			StateLevel:         "provider_model",
			StateTransition:    "provider_model.quota",
			BlockProviderModel: true,
			Description:        "429 quota signals move routing to the next project/provider when local project switching is disabled.",
		},
		{
			Name:            "model_not_found",
			HTTPStatus:      http.StatusNotFound,
			Reason:          "model_not_found",
			StateLevel:      "model_on_auth",
			StateTransition: "model_on_auth.not_found",
			Description:     "Model not found is scoped to the current auth/model unless routing upgrades it to provider/model blocking.",
		},
		{
			Name:            "transient",
			HTTPStatus:      http.StatusServiceUnavailable,
			Reason:          "transient",
			StateLevel:      "route",
			StateTransition: "route.transient",
			Description:     "Transient upstream failures allow routing to continue without mutating auth ownership beyond cooldown state.",
		},
	}
}

func (m *Manager) recordRoutingEvent(event RoutingEvent) {
	if m == nil {
		return
	}
	if strings.TrimSpace(event.OccurredAt) == "" {
		event.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	event.RequestedProviders = append([]string(nil), event.RequestedProviders...)
	if len(event.ProviderOffsetSnapshot) > 0 {
		copied := make(map[string]int, len(event.ProviderOffsetSnapshot))
		for key, value := range event.ProviderOffsetSnapshot {
			copied[key] = value
		}
		event.ProviderOffsetSnapshot = copied
	}
	m.routingMu.Lock()
	sinks := append([]func(RoutingEvent){}, m.routingSinks...)
	if len(m.routingEvents) >= routingEventBufferSize {
		copy(m.routingEvents, m.routingEvents[1:])
		m.routingEvents[len(m.routingEvents)-1] = event
	} else {
		m.routingEvents = append(m.routingEvents, event)
	}
	m.routingMu.Unlock()
	for _, sink := range sinks {
		if sink == nil {
			continue
		}
		sink(event)
	}
	entry := logEntryWithRequestID(context.Background())
	fields := log.Fields{
		"engine_version":           event.EngineVersion,
		"requested_model":          event.RequestedModel,
		"requested_providers":      event.RequestedProviders,
		"chosen_provider":          event.ChosenProvider,
		"chosen_auth":              event.ChosenAuth,
		"chosen_model":             event.ChosenModel,
		"fallback_stage":           event.FallbackStage,
		"fallback_reason":          event.FallbackReason,
		"selector_strategy":        event.SelectorStrategy,
		"provider_offset_snapshot": event.ProviderOffsetSnapshot,
		"result_status":            event.ResultStatus,
		"state_transition":         event.StateTransition,
		"compare_provider":         event.CompareProvider,
		"compare_auth":             event.CompareAuth,
		"compare_model":            event.CompareModel,
		"compare_diff":             event.CompareDiff,
	}
	if event.RequestID != "" {
		fields["request_id"] = event.RequestID
	}
	entry = entry.WithFields(fields)
	if log.IsLevelEnabled(log.DebugLevel) {
		entry.Debug("routing_event")
	}
}

func (m *Manager) RecentRoutingEvents(limit int) []RoutingEvent {
	if m == nil || limit == 0 {
		return nil
	}
	m.routingMu.Lock()
	defer m.routingMu.Unlock()
	if limit < 0 || limit > len(m.routingEvents) {
		limit = len(m.routingEvents)
	}
	start := len(m.routingEvents) - limit
	if start < 0 {
		start = 0
	}
	out := make([]RoutingEvent, 0, limit)
	for _, event := range m.routingEvents[start:] {
		out = append(out, event)
	}
	return out
}

func routeObserverFromMetadata(metadata map[string]any) func(CandidateRoute) {
	if len(metadata) == 0 {
		return nil
	}
	raw := metadata[legacyRouteObserverMetadataKey]
	if raw == nil {
		return nil
	}
	fn, _ := raw.(func(CandidateRoute))
	return fn
}

func cloneOptionsWithRouteObserver(opts cliproxyexecutor.Options, observer func(CandidateRoute)) cliproxyexecutor.Options {
	cloned := opts
	if len(opts.Metadata) == 0 {
		cloned.Metadata = map[string]any{legacyRouteObserverMetadataKey: observer}
		return cloned
	}
	cloned.Metadata = make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		cloned.Metadata[key] = value
	}
	cloned.Metadata[legacyRouteObserverMetadataKey] = observer
	return cloned
}

func routeDiffSummary(actual, compare CandidateRoute) string {
	diffs := make([]string, 0, 3)
	if !strings.EqualFold(strings.TrimSpace(actual.Provider), strings.TrimSpace(compare.Provider)) {
		diffs = append(diffs, "provider")
	}
	if strings.TrimSpace(actual.AuthID) != strings.TrimSpace(compare.AuthID) {
		diffs = append(diffs, "auth")
	}
	if !strings.EqualFold(strings.TrimSpace(actual.Model), strings.TrimSpace(compare.Model)) {
		diffs = append(diffs, "model")
	}
	if len(diffs) == 0 {
		return "match"
	}
	sort.Strings(diffs)
	return strings.Join(diffs, ",")
}

func (m *Manager) emitShadowComparison(ctx context.Context, req cliproxyexecutor.Request, providers []string, actual CandidateRoute, compare *CandidateRoute) {
	if compare == nil {
		return
	}
	m.recordRoutingEvent(RoutingEvent{
		RequestID:              logging.GetRequestID(ctx),
		EngineVersion:          routingEngineShadow,
		RequestedModel:         strings.TrimSpace(req.Model),
		RequestedProviders:     append([]string(nil), providers...),
		ChosenProvider:         actual.Provider,
		ChosenAuth:             actual.AuthID,
		ChosenModel:            actual.Model,
		SelectorStrategy:       m.currentRoutingEngineSelectorLabel(),
		ResultStatus:           "shadow_compare",
		StateTransition:        "v1_vs_v2_primary_route",
		CompareProvider:        compare.Provider,
		CompareAuth:            compare.AuthID,
		CompareModel:           compare.Model,
		CompareDiff:            routeDiffSummary(actual, *compare),
		ProviderOffsetSnapshot: m.providerOffsetSnapshot(req.Model),
	})
}
