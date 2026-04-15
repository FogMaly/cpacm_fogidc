package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a channel of provider chunks.
	ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval  = 5 * time.Second
	refreshPendingBackoff = time.Minute
	refreshFailureBackoff = 5 * time.Minute
	quotaBackoffBase      = time.Second
	quotaBackoffMax       = 30 * time.Minute
	chinaUTCOffsetHours   = 8
	// Keep a short backoff for transport-level failures even when
	// disable-cooling is enabled, so routing can quickly switch away.
	transportTimeoutCooldown = 15 * time.Second
)

var quotaCooldownDisabled atomic.Bool

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	return quotaCooldownDisabled.Load()
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// Error describes the failure when Success is false.
	Error *Error
	// Duration captures end-to-end provider call latency when available.
	Duration time.Duration
	// RuntimeOutageStatus carries an optional synthetic health status for semantic upstream failures.
	RuntimeOutageStatus int
	// RuntimeOutageReason carries an optional synthetic health reason even when the request recovered.
	RuntimeOutageReason string
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store     Store
	executors map[string]ProviderExecutor
	selector  Selector
	hook      Hook
	mu        sync.RWMutex
	auths     map[string]*Auth
	// requestAffinity pins the same session/conversation key to the same auth until it fails or expires.
	requestAffinity requestAffinityStore
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets map[string]int
	routingMu       sync.Mutex
	routingEvents   []RoutingEvent
	routingSinks    []func(RoutingEvent)

	// Retry controls request retry behavior.
	requestRetry     atomic.Int32
	maxRetryInterval atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// providerConcurrencyLimits caps concurrent upstream requests per provider.
	providerConcurrencyLimits map[string]int
	providerSemaphores        map[string]chan struct{}
	authHealth                map[string]*AuthHealthMetrics
	runtimeOutages            map[string]map[string]runtimeOutageState
	providerRuntimeLoads      map[string]*runtimeLoadTracker
	authRuntimeLoads          map[string]*runtimeLoadTracker
	probeSnapshotPath         string
	balanceSource             func(context.Context) []health.ProviderBalanceSnapshotItem
	balanceCache              []health.ProviderBalanceSnapshotItem
	balanceCacheAt            time.Time
	balanceRefreshInFlight    bool
	probeCache                *health.ProbeSnapshot
	probeCacheAt              time.Time

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	// Auto refresh state
	refreshCancel context.CancelFunc
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:                     store,
		executors:                 make(map[string]ProviderExecutor),
		selector:                  selector,
		hook:                      hook,
		auths:                     make(map[string]*Auth),
		requestAffinity:           requestAffinityStore{items: make(map[string]requestAffinitySelection)},
		providerOffsets:           make(map[string]int),
		providerConcurrencyLimits: make(map[string]int),
		providerSemaphores:        make(map[string]chan struct{}),
		authHealth:                make(map[string]*AuthHealthMetrics),
		runtimeOutages:            make(map[string]map[string]runtimeOutageState),
		providerRuntimeLoads:      make(map[string]*runtimeLoadTracker),
		authRuntimeLoads:          make(map[string]*runtimeLoadTracker),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	return manager
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.mu.Lock()
	m.selector = selector
	m.mu.Unlock()
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// RegisterRoutingSink appends a callback that receives routing events as they
// are recorded. Nil callbacks are ignored.
func (m *Manager) RegisterRoutingSink(fn func(RoutingEvent)) {
	if m == nil || fn == nil {
		return
	}
	m.routingMu.Lock()
	m.routingSinks = append(m.routingSinks, fn)
	m.routingMu.Unlock()
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

func (m *Manager) SetUnifiedProbeSnapshotPath(path string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.probeSnapshotPath = strings.TrimSpace(path)
	m.probeCache = nil
	m.probeCacheAt = time.Time{}
	m.mu.Unlock()
}

func (m *Manager) SetUnifiedBalanceSource(source func(context.Context) []health.ProviderBalanceSnapshotItem) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.balanceSource = source
	m.balanceCache = nil
	m.balanceCacheAt = time.Time{}
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.runtimeConfig.Store(cfg)
	m.configureProviderConcurrency(cfg)
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
}

func (m *Manager) lookupAPIKeyUpstreamModel(authID, requestedModel string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if table == nil {
		return ""
	}
	byAlias := table[authID]
	if len(byAlias) == 0 {
		return ""
	}
	key := strings.ToLower(thinking.ParseSuffix(requestedModel).ModelName)
	if key == "" {
		key = strings.ToLower(requestedModel)
	}
	resolved := strings.TrimSpace(byAlias[key])
	if resolved == "" {
		return ""
	}
	// Preserve thinking suffix from the client's requested model unless config already has one.
	requestResult := thinking.ParseSuffix(requestedModel)
	if thinking.ParseSuffix(resolved).HasSuffix {
		return resolved
	}
	if requestResult.HasSuffix && requestResult.RawSuffix != "" {
		return resolved + "(" + requestResult.RawSuffix + ")"
	}
	return resolved

}

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}

		byAlias := make(map[string]string)
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "gemini":
			if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "claude":
			if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "codex":
			if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "vertex":
			if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		default:
			// OpenAI-compat uses config selection from auth.Attributes.
			providerKey := ""
			compatName := ""
			if auth.Attributes != nil {
				providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			}
			if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
				if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
					compileAPIKeyModelAliasForModels(byAlias, entry.Models)
				}
			}
		}

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	m.apiKeyModelAlias.Store(out)
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.executors[executor.Identifier()] = executor
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	auth.EnsureIndex()
	m.mu.Lock()
	m.auths[auth.ID] = auth.Clone()
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	_ = m.persist(ctx, auth)
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	m.mu.Lock()
	if existing, ok := m.auths[auth.ID]; ok && existing != nil && !auth.indexAssigned && auth.Index == "" {
		auth.Index = existing.Index
		auth.indexAssigned = existing.indexAssigned
	}
	auth.EnsureIndex()
	m.auths[auth.ID] = auth.Clone()
	m.mu.Unlock()
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	_ = m.persist(ctx, auth)
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store == nil {
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		auth.EnsureIndex()
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxWait := m.retrySettings()
	engine := m.routingEngine()
	var shadowRoute *CandidateRoute
	if engine == routingEngineShadow {
		planned, err := m.previewLayeredPrimaryRoute(ctx, normalized, req, opts)
		if err == nil {
			shadowRoute = planned
		}
		var actualRoute CandidateRoute
		opts = cloneOptionsWithRouteObserver(opts, func(route CandidateRoute) {
			if actualRoute.Provider != "" {
				return
			}
			actualRoute = route
			m.emitShadowComparison(ctx, req, normalized, actualRoute, shadowRoute)
		})
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		var (
			resp    cliproxyexecutor.Response
			errExec error
		)
		switch engine {
		case routingEngineV2:
			resp, errExec = m.executeLayeredOnce(ctx, normalized, req, opts)
		default:
			resp, errExec = m.executeMixedOnce(ctx, normalized, req, opts)
		}
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable}
}

// ExecuteCount performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxWait := m.retrySettings()
	engine := m.routingEngine()

	var lastErr error
	for attempt := 0; ; attempt++ {
		var (
			resp    cliproxyexecutor.Response
			errExec error
		)
		switch engine {
		case routingEngineV2:
			resp, errExec = m.executeCountLayeredOnce(ctx, normalized, req, opts)
		default:
			resp, errExec = m.executeCountMixedOnce(ctx, normalized, req, opts)
		}
		if errExec == nil {
			return resp, nil
		}
		lastErr = errExec
		wait, shouldRetry := m.shouldRetryAfterError(errExec, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return cliproxyexecutor.Response{}, errWait
		}
	}
	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, &Error{Code: "auth_not_found", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable}
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	_, maxWait := m.retrySettings()
	engine := m.routingEngine()

	var lastErr error
	for attempt := 0; ; attempt++ {
		var (
			chunks    <-chan cliproxyexecutor.StreamChunk
			errStream error
		)
		switch engine {
		case routingEngineV2:
			chunks, errStream = m.executeStreamLayeredOnce(ctx, normalized, req, opts)
		default:
			chunks, errStream = m.executeStreamMixedOnce(ctx, normalized, req, opts)
		}
		if errStream == nil {
			return chunks, nil
		}
		lastErr = errStream
		wait, shouldRetry := m.shouldRetryAfterError(errStream, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
			return nil, errWait
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &Error{Code: "auth_not_found", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable}
}

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	scope := affinityScopeForProviders(providers)
	routeModel := req.Model
	strictPublicRouting := strictPublicModelRoutingEnabled(opts.Metadata)
	strictRequestedModel := requestedModelFromMetadata(opts.Metadata, routeModel)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	var lastErr error
	for {
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx, signalCollector := WithResultSignalCollector(execCtx)
		execReq := req
		execReq.Model = rewriteModelForAuth(routeModel, auth)
		execReq.Model = m.applyOAuthModelAlias(auth, execReq.Model)
		execReq.Model = m.applyAPIKeyModelAlias(auth, execReq.Model)
		if strictPublicRouting && !sameStrictPublicModel(strictRequestedModel, routeModel) {
			execReq.Model = routeModel
		}
		if observer := routeObserverFromMetadata(opts.Metadata); observer != nil {
			observer(CandidateRoute{Provider: provider, AuthID: auth.ID, Model: execReq.Model, Reason: routeModel})
		}
		var resp cliproxyexecutor.Response
		startedAt := time.Now()
		errExec := m.withProviderSlot(execCtx, provider, func() error {
			finishLoad := m.beginRuntimeLoad(auth, provider)
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
		result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: errExec == nil, Duration: time.Since(startedAt)}
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
			m.clearRequestAffinity(scope, routeModel, opts, auth.ID)
			lastErr = errExec
			continue
		}
		m.MarkResult(execCtx, result)
		m.rememberRequestAffinity(scope, routeModel, opts, auth)
		return resp, nil
	}
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if len(providers) == 0 {
		return cliproxyexecutor.Response{}, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	scope := affinityScopeForProviders(providers)
	routeModel := req.Model
	strictPublicRouting := strictPublicModelRoutingEnabled(opts.Metadata)
	strictRequestedModel := requestedModelFromMetadata(opts.Metadata, routeModel)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	var lastErr error
	for {
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if lastErr != nil {
				return cliproxyexecutor.Response{}, lastErr
			}
			return cliproxyexecutor.Response{}, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx, signalCollector := WithResultSignalCollector(execCtx)
		execReq := req
		execReq.Model = rewriteModelForAuth(routeModel, auth)
		execReq.Model = m.applyOAuthModelAlias(auth, execReq.Model)
		execReq.Model = m.applyAPIKeyModelAlias(auth, execReq.Model)
		if strictPublicRouting && !sameStrictPublicModel(strictRequestedModel, routeModel) {
			execReq.Model = routeModel
		}
		if observer := routeObserverFromMetadata(opts.Metadata); observer != nil {
			observer(CandidateRoute{Provider: provider, AuthID: auth.ID, Model: execReq.Model, Reason: routeModel})
		}
		var resp cliproxyexecutor.Response
		startedAt := time.Now()
		errExec := m.withProviderSlot(execCtx, provider, func() error {
			finishLoad := m.beginRuntimeLoad(auth, provider)
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
		result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: errExec == nil, Duration: time.Since(startedAt)}
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
			m.clearRequestAffinity(scope, routeModel, opts, auth.ID)
			lastErr = errExec
			continue
		}
		m.MarkResult(execCtx, result)
		m.rememberRequestAffinity(scope, routeModel, opts, auth)
		return resp, nil
	}
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	if len(providers) == 0 {
		return nil, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	scope := affinityScopeForProviders(providers)
	routeModel := req.Model
	strictPublicRouting := strictPublicModelRoutingEnabled(opts.Metadata)
	strictRequestedModel := requestedModelFromMetadata(opts.Metadata, routeModel)
	opts = ensureRequestedModelMetadata(opts, routeModel)
	tried := make(map[string]struct{})
	var lastErr error
	for {
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, opts, tried)
		if errPick != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)

		tried[auth.ID] = struct{}{}
		execCtx := ctx
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
			execCtx = context.WithValue(execCtx, "cliproxy.roundtripper", rt)
		}
		execCtx, signalCollector := WithResultSignalCollector(execCtx)
		execReq := req
		execReq.Model = rewriteModelForAuth(routeModel, auth)
		execReq.Model = m.applyOAuthModelAlias(auth, execReq.Model)
		execReq.Model = m.applyAPIKeyModelAlias(auth, execReq.Model)
		if strictPublicRouting && !sameStrictPublicModel(strictRequestedModel, routeModel) {
			execReq.Model = routeModel
		}
		if observer := routeObserverFromMetadata(opts.Metadata); observer != nil {
			observer(CandidateRoute{Provider: provider, AuthID: auth.ID, Model: execReq.Model, Reason: routeModel})
		}
		releaseProviderSlot, errAcquireProviderSlot := m.acquireProviderSlot(execCtx, provider)
		if errAcquireProviderSlot != nil {
			return nil, errAcquireProviderSlot
		}
		startedAt := time.Now()
		finishLoad := m.beginRuntimeLoad(auth, provider)
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
			result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false, Error: rerr, Duration: time.Since(startedAt)}
			result.RetryAfter = retryAfterFromError(errStream)
			m.MarkResult(execCtx, result)
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			m.clearRequestAffinity(scope, routeModel, opts, auth.ID)
			lastErr = errStream
			continue
		}
		out := make(chan cliproxyexecutor.StreamChunk)
		go func(streamCtx context.Context, streamAuth *Auth, streamProvider string, streamChunks <-chan cliproxyexecutor.StreamChunk, signalCollector *ResultSignalCollector, release func(), finish func(bool, time.Duration, *Error)) {
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
					finish(false, time.Since(startedAt), rerr)
					result := Result{AuthID: streamAuth.ID, Provider: streamProvider, Model: routeModel, Success: false, Error: rerr, Duration: time.Since(startedAt)}
					applyResultSignals(&result, signalCollector)
					m.MarkResult(streamCtx, result)
					m.clearRequestAffinity(scope, routeModel, opts, streamAuth.ID)
				}
				if !forward {
					continue
				}
				if streamCtx == nil {
					out <- chunk
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
				result := Result{AuthID: streamAuth.ID, Provider: streamProvider, Model: routeModel, Success: true, Duration: time.Since(startedAt)}
				applyResultSignals(&result, signalCollector)
				m.MarkResult(streamCtx, result)
				m.rememberRequestAffinity(scope, routeModel, opts, streamAuth)
			}
		}(execCtx, auth.Clone(), provider, chunks, signalCollector, releaseProviderSlot, finishLoad)
		return out, nil
	}
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func requestedModelFromMetadata(meta map[string]any, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(meta) == 0 {
		return fallback
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		return strings.TrimSpace(v)
	case []byte:
		trimmed := strings.TrimSpace(string(v))
		if trimmed == "" {
			return fallback
		}
		return trimmed
	default:
		return fallback
	}
}

func requestedAuthPrefixLock(meta map[string]any, fallback string) string {
	requested := requestedModelFromMetadata(meta, fallback)
	prefix, upstream := splitRequestedAuthPrefix(requested)
	if prefix == "" || upstream == "" {
		return ""
	}
	return prefix
}

func splitRequestedAuthPrefix(model string) (prefix string, upstream string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", ""
	}
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	prefix = strings.ToLower(strings.TrimSpace(parts[0]))
	upstream = strings.TrimSpace(parts[1])
	if prefix == "" || upstream == "" {
		return "", ""
	}
	return prefix, upstream
}

func authMatchesRequestedPrefix(auth *Auth, requestedPrefix string) bool {
	if auth == nil {
		return false
	}
	requestedPrefix = strings.ToLower(strings.TrimSpace(requestedPrefix))
	if requestedPrefix == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(auth.Prefix), requestedPrefix)
}

func strictPublicModelRoutingEnabled(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.StrictPublicModelRoutingMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	default:
		return false
	}
}

func canonicalStrictPublicModelKey(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if idx := strings.Index(model, "/"); idx >= 0 {
		model = strings.TrimSpace(model[idx+1:])
	}
	parsed := thinking.ParseSuffix(model)
	return strings.ToLower(strings.TrimSpace(parsed.ModelName))
}

func sameStrictPublicModel(requestedModel, resolvedModel string) bool {
	requestedKey := canonicalStrictPublicModelKey(requestedModel)
	resolvedKey := canonicalStrictPublicModelKey(resolvedModel)
	if requestedKey == "" || resolvedKey == "" {
		return false
	}
	if requestedKey == resolvedKey {
		return true
	}
	if !strings.HasPrefix(resolvedKey, requestedKey+"-") {
		return false
	}
	suffix := strings.TrimPrefix(resolvedKey, requestedKey+"-")
	return isStrictPublicVersionSuffix(suffix)
}

func isStrictPublicVersionSuffix(suffix string) bool {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" {
		return false
	}
	hasDigit := false
	for _, r := range suffix {
		if r >= '0' && r <= '9' {
			hasDigit = true
			continue
		}
		if r == '-' {
			continue
		}
		return false
	}
	return hasDigit
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

func normalizeRequestedModelForAuth(auth *Auth, model string) string {
	model = strings.TrimSpace(model)
	if auth == nil || model == "" {
		return model
	}
	if idx := strings.Index(model, "/"); idx >= 0 && idx < len(model)-1 {
		prefix := strings.Trim(strings.TrimSpace(auth.Prefix), "/")
		requestPrefix := strings.Trim(strings.TrimSpace(model[:idx]), "/")
		upstream := strings.TrimSpace(model[idx+1:])
		if upstream != "" && (prefix == "" || !strings.EqualFold(requestPrefix, prefix)) {
			return upstream
		}
	}
	return model
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return resolved
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return upstreamModel
	}
	return requestedModel
}

func (m *Manager) authSupportsRequestedModel(auth *Auth, model string, registryRef interface {
	ClientSupportsModel(clientID, modelID string) bool
}) bool {
	if auth == nil {
		return false
	}
	model = normalizeRequestedModelForAuth(auth, model)
	if model == "" {
		return !authExcludesAllModels(auth)
	}
	if authExcludesModel(auth, model) {
		return false
	}
	supportsRegisteredModel := func(candidate string) bool {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || registryRef == nil {
			return false
		}
		if registryRef.ClientSupportsModel(auth.ID, candidate) {
			return true
		}
		prefix := strings.Trim(strings.TrimSpace(auth.Prefix), "/")
		if prefix == "" || strings.Contains(candidate, "/") {
			return false
		}
		return registryRef.ClientSupportsModel(auth.ID, prefix+"/"+candidate)
	}
	if supportsRegisteredModel(model) {
		return true
	}
	resolved := strings.TrimSpace(m.applyAPIKeyModelAlias(auth, model))
	if resolved == "" {
		return false
	}
	if authExcludesModel(auth, model, resolved) {
		return false
	}
	if supportsRegisteredModel(resolved) {
		return true
	}
	return !strings.EqualFold(resolved, model)
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	if upstream := resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models)); upstream != "" {
		return upstream
	}
	return resolveClaudeAPIKeyCompatibilityModel(auth, entry, requestedModel)
}

func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	if upstream := resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models)); upstream != "" {
		return upstream
	}
	return resolveCodexAPIKeyCompatibilityModel(auth, entry, requestedModel)
}

func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveClaudeAPIKeyCompatibilityModel(auth *Auth, entry *internalconfig.ClaudeKey, requestedModel string) string {
	if !isClaudeCompatibilityChannel(auth, entry) {
		return ""
	}
	return resolveModelFromCompatibilityMap(requestedModel, map[string]string{
		"claude-opus-4-6":            "claude-sonnet-4-6",
		"claude-opus-4-6-thinking":   "claude-sonnet-4-6",
		"claude-sonnet-4-5":          "claude-sonnet-4-5-20250929",
		"claude-sonnet-4-5-thinking": "claude-sonnet-4-5-20250929",
		"claude-haiku-4-5":           "claude-haiku-4-5-20251001",
	})
}

func resolveCodexAPIKeyCompatibilityModel(auth *Auth, entry *internalconfig.CodexKey, requestedModel string) string {
	if !isCodexCompatibilityChannel(auth, entry) {
		return ""
	}
	return resolveModelFromCompatibilityMap(requestedModel, map[string]string{
		"codex-mini-latest":  "gpt-5.3-codex",
		"gpt-5-codex":        "gpt-5.3-codex",
		"gpt-5.1-codex":      "gpt-5.3-codex",
		"gpt-5.1-codex-max":  "gpt-5.3-codex",
		"gpt-5.1-codex-mini": "gpt-5.3-codex",
		"gpt-5.2-codex":      "gpt-5.3-codex",
	})
}

func isClaudeCompatibilityChannel(auth *Auth, entry *internalconfig.ClaudeKey) bool {
	prefix := ""
	if auth != nil {
		prefix = strings.ToLower(strings.TrimSpace(auth.Prefix))
	}
	if prefix == "yunyi-claude" || prefix == "cc" {
		return true
	}
	baseURL := ""
	if entry != nil {
		baseURL = strings.ToLower(strings.TrimSpace(entry.BaseURL))
	}
	return strings.Contains(baseURL, "cdn1.yunyi.cfd/claude")
}

func isCodexCompatibilityChannel(auth *Auth, entry *internalconfig.CodexKey) bool {
	prefix := ""
	if auth != nil {
		prefix = strings.ToLower(strings.TrimSpace(auth.Prefix))
	}
	if prefix == "sub2api" || prefix == "yunyi-codex" || prefix == "8200codex" || prefix == "ee" {
		return true
	}
	baseURL := ""
	if entry != nil {
		baseURL = strings.ToLower(strings.TrimSpace(entry.BaseURL))
	}
	return strings.Contains(baseURL, "ai.last.ee") || strings.Contains(baseURL, "cdn1.yunyi.cfd/codex")
}

func resolveModelFromCompatibilityMap(requestedModel string, mapping map[string]string) string {
	if len(mapping) == 0 {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}

	requestResult := thinking.ParseSuffix(requestedModel)
	candidates := []string{requestResult.ModelName}
	if requestResult.ModelName != requestedModel {
		candidates = append(candidates, requestedModel)
	}

	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate))
		if key == "" {
			continue
		}
		resolved := strings.TrimSpace(mapping[key])
		if resolved == "" {
			continue
		}
		if thinking.ParseSuffix(resolved).HasSuffix {
			return resolved
		}
		if requestResult.HasSuffix && requestResult.RawSuffix != "" {
			return resolved + "(" + requestResult.RawSuffix + ")"
		}
		return resolved
	}

	return ""
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(cfg *internalconfig.Config, providerKey, compatName, authProvider string) *internalconfig.OpenAICompatibility {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

func (m *Manager) retrySettings() (int, time.Duration) {
	if m == nil {
		return 0, 0
	}
	return int(m.requestRetry.Load()), time.Duration(m.maxRetryInterval.Load())
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	if defaultRetry < 0 {
		defaultRetry = 0
	}
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) shouldRetryAfterError(err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	if status := statusCodeFromError(err); status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	wait, found := m.closestCooldownWait(providers, model, attempt)
	if !found || wait > maxWait {
		return 0, false
	}
	return wait, true
}

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()

		if result.Success {
			if result.Model != "" {
				state := ensureModelState(auth, result.Model)
				resetModelState(state, now)
				updateAggregatedAvailability(auth, now)
				if !hasModelError(auth, now) {
					auth.LastError = nil
					auth.StatusMessage = ""
					auth.Status = StatusActive
				}
				auth.UpdatedAt = now
				shouldResumeModel = true
				clearModelQuota = true
			} else {
				clearAuthStateOnSuccess(auth, now)
			}
		} else {
			if result.Model != "" {
				state := ensureModelState(auth, result.Model)
				state.Unavailable = true
				state.Status = StatusError
				state.UpdatedAt = now
				if result.Error != nil {
					state.LastError = cloneError(result.Error)
					state.StatusMessage = result.Error.Message
					auth.LastError = cloneError(result.Error)
					auth.StatusMessage = result.Error.Message
				}

				statusCode := statusCodeFromResult(result.Error)
				switch statusCode {
				case 401:
					next := now.Add(30 * time.Minute)
					state.NextRetryAfter = next
					suspendReason = "unauthorized"
					shouldSuspendModel = true
				case 402, 403:
					if isDailyQuotaExceededError(result.Error) {
						next := nextDailyQuotaResumeAtChinaMidnightUTC(now)
						state.NextRetryAfter = next
						state.Quota = QuotaState{
							Exceeded:      true,
							Reason:        "daily_quota",
							NextRecoverAt: next,
						}
						suspendReason = "quota"
						shouldSuspendModel = true
						setModelQuota = true
					} else {
						next := now.Add(30 * time.Minute)
						state.NextRetryAfter = next
						suspendReason = "payment_required"
						shouldSuspendModel = true
					}
				case 404:
					next := now.Add(12 * time.Hour)
					state.NextRetryAfter = next
					suspendReason = "not_found"
					shouldSuspendModel = true
				case 429:
					var next time.Time
					backoffLevel := state.Quota.BackoffLevel
					if result.RetryAfter != nil {
						next = now.Add(*result.RetryAfter)
					} else {
						cooldown, nextLevel := nextQuotaCooldown(backoffLevel, quotaCooldownDisabledForAuth(auth))
						if cooldown > 0 {
							next = now.Add(cooldown)
						}
						backoffLevel = nextLevel
					}
					state.NextRetryAfter = next
					state.Quota = QuotaState{
						Exceeded:      true,
						Reason:        "quota",
						NextRecoverAt: next,
						BackoffLevel:  backoffLevel,
					}
					suspendReason = "quota"
					shouldSuspendModel = true
					setModelQuota = true
				case 408, 500, 502, 503, 504:
					if quotaCooldownDisabledForAuth(auth) {
						if result.Error != nil && hasRetryableTransportMessage(result.Error.Message) {
							state.NextRetryAfter = now.Add(transportTimeoutCooldown)
						} else {
							state.NextRetryAfter = time.Time{}
						}
					} else {
						next := now.Add(1 * time.Minute)
						state.NextRetryAfter = next
					}
				default:
					state.NextRetryAfter = time.Time{}
				}

				auth.Status = StatusError
				auth.UpdatedAt = now
				updateAggregatedAvailability(auth, now)
			} else {
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
			}
		}

		_ = m.persist(ctx, auth)
	}
	m.mu.Unlock()

	if clearModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if setModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, result.Model)
	}
	if shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, result.Model)
	} else if shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, result.Model, suspendReason)
	}

	m.hook.OnResult(ctx, result)
	m.updateAuthHealth(result)
	m.updateRuntimeOutage(result)
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() || (!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		if status := sc.StatusCode(); status > 0 {
			if status == http.StatusBadRequest && hasModelNotSupportedMessage(err.Error()) {
				return http.StatusNotFound
			}
			return status
		}
	}
	return inferredHTTPStatusFromMessage(err.Error())
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	rap, ok := err.(retryAfterProvider)
	if !ok || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	val := *retryAfter
	return &val
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	if status := err.StatusCode(); status > 0 {
		if status == http.StatusBadRequest && hasModelNotSupportedMessage(err.Message) {
			return http.StatusNotFound
		}
		return status
	}
	return inferredHTTPStatusFromMessage(err.Message)
}

func inferredHTTPStatusFromMessage(msg string) int {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return 0
	}
	switch {
	case hasModelNotSupportedMessage(msg):
		return http.StatusNotFound
	case hasTransportTimeoutMessage(msg):
		return http.StatusGatewayTimeout
	case hasRetryableTransportMessage(msg):
		return http.StatusBadGateway
	default:
		return 0
	}
}

func hasTransportTimeoutMessage(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "timeout awaiting response headers") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "client.timeout exceeded") ||
		strings.Contains(msg, "i/o timeout")
}

func hasModelNotSupportedMessage(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "model_not_supported") ||
		strings.Contains(msg, `"type":"model_not_supported"`) ||
		(strings.Contains(msg, "model '") && strings.Contains(msg, "is not supported"))
}

func hasRetryableTransportMessage(msg string) bool {
	msg = strings.ToLower(strings.TrimSpace(msg))
	if msg == "" {
		return false
	}
	if hasTransportTimeoutMessage(msg) {
		return true
	}
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "malformed http response") ||
		strings.Contains(msg, "http/1.x transport connection broken") ||
		// Treat Claude/Codex semantic bad upstream responses as short-cooldown failures too,
		// so disable-cooling does not keep routing users back onto the same broken path.
		strings.Contains(msg, "short continuation response") ||
		strings.Contains(msg, "interrupted agent scaffold response") ||
		strings.Contains(msg, "foreign assistant identity response") ||
		strings.Contains(msg, "pseudo tool stub response") ||
		strings.Contains(msg, "empty visible content after fallback retry") ||
		strings.Contains(msg, "stream disconnected before completion") ||
		strings.Contains(msg, "stream closed before message_stop") ||
		strings.Contains(msg, "stream closed before response.completed")
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried. Specifically, it checks for 400 Bad Request
// with "invalid_request_error" in the message, indicating the request itself is
// malformed and switching to a different auth will not help.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest {
		return false
	}
	if hasModelNotSupportedMessage(err.Error()) {
		return false
	}
	if isProtocolMismatchBadRequest(err) {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "invalid_request_error")
}

func isProtocolMismatchBadRequest(err error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromError(err) != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "unsupported content type") ||
		strings.Contains(msg, "unsupported media type")
}

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 402, 403:
		if isDailyQuotaExceededError(resultErr) {
			next := nextDailyQuotaResumeAtChinaMidnightUTC(now)
			auth.StatusMessage = "daily_quota_exhausted"
			auth.Quota.Exceeded = true
			auth.Quota.Reason = "daily_quota"
			auth.Quota.NextRecoverAt = next
			auth.NextRetryAfter = next
		} else {
			auth.StatusMessage = "payment_required"
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 404:
		auth.StatusMessage = "not_found"
		auth.NextRetryAfter = now.Add(12 * time.Hour)
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		var next time.Time
		if retryAfter != nil {
			next = now.Add(*retryAfter)
		} else {
			cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, quotaCooldownDisabledForAuth(auth))
			if cooldown > 0 {
				next = now.Add(cooldown)
			}
			auth.Quota.BackoffLevel = nextLevel
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if quotaCooldownDisabledForAuth(auth) {
			if resultErr != nil && hasRetryableTransportMessage(resultErr.Message) {
				auth.NextRetryAfter = now.Add(transportTimeoutCooldown)
			} else {
				auth.NextRetryAfter = time.Time{}
			}
		} else {
			auth.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := quotaBackoffBase * time.Duration(1<<prevLevel)
	if cooldown < quotaBackoffBase {
		cooldown = quotaBackoffBase
	}
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

func isDailyQuotaExceededError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(resultErr.Message))
	if msg == "" {
		return false
	}
	if strings.Contains(msg, "daily_limit_reached") ||
		strings.Contains(msg, "daily spending limit reached") {
		return true
	}
	if strings.Contains(msg, "quota will reset tomorrow") ||
		strings.Contains(msg, "daily quota") {
		return true
	}
	return false
}

func nextDailyQuotaResumeAtChinaMidnightUTC(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	china := time.FixedZone("UTC+8", chinaUTCOffsetHours*3600)
	local := now.In(china)
	tomorrow := local.AddDate(0, 0, 1)
	resumeLocal := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, china)
	return resumeLocal.UTC()
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

func (m *Manager) pickNext(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, error) {
	scope := affinityScopeForProvider(provider)
	m.mu.RLock()
	selector := m.selector
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate.Provider != provider || candidate.Disabled {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && !m.authSupportsRequestedModel(candidate, modelKey, registryRef) {
			continue
		}
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	candidates = filterCandidatesByProtocol(provider, opts, candidates)
	if len(candidates) == 0 {
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable}
	}
	if stickySelected, ok := m.pickCandidateByAffinity(scope, model, opts, candidates); ok && stickySelected != nil {
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
	for i := range candidates {
		candidates[i].Runtime = m.buildAuthSelectionRuntime(ctx, candidates[i], model)
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	selected, errPick := selector.Pick(ctx, provider, model, opts, candidates)
	if errPick != nil {
		return nil, nil, errPick
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth", HTTPStatus: http.StatusServiceUnavailable}
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

func (m *Manager) pickNextMixed(ctx context.Context, providers []string, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, ProviderExecutor, string, error) {
	scope := affinityScopeForProviders(providers)
	requestedPrefix := requestedAuthPrefixLock(opts.Metadata, model)
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	selector := m.selector
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	// Always use base model name (without thinking suffix) for auth matching.
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if !authMatchesRequestedPrefix(candidate, requestedPrefix) {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if modelKey != "" && !m.authSupportsRequestedModel(candidate, modelKey, registryRef) {
			continue
		}
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	candidates = filterCandidatesByProtocol("mixed", opts, candidates)
	if len(candidates) == 0 {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable}
	}
	if stickySelected, ok := m.pickCandidateByAffinity(scope, model, opts, candidates); ok && stickySelected != nil {
		providerKey := strings.TrimSpace(strings.ToLower(stickySelected.Provider))
		m.mu.RLock()
		executor, okExecutor := m.executors[providerKey]
		m.mu.RUnlock()
		if !okExecutor {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
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
		return authCopy, executor, providerKey, nil
	}
	for i := range candidates {
		candidates[i].Runtime = m.buildAuthSelectionRuntime(ctx, candidates[i], model)
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	selected, errPick := selector.Pick(ctx, "mixed", model, opts, candidates)
	if errPick != nil {
		return nil, nil, "", errPick
	}
	if selected == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth", HTTPStatus: http.StatusServiceUnavailable}
	}
	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	m.mu.RLock()
	executor, okExecutor := m.executors[providerKey]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	m.mu.RUnlock()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
}

func filterCandidatesByProtocol(provider string, opts cliproxyexecutor.Options, candidates []*Auth) []*Auth {
	if len(candidates) == 0 {
		return candidates
	}

	requestProtocol := codexRequestProtocol(opts)
	if requestProtocol == "" {
		return candidates
	}

	filterForProvider := func(auths []*Auth, providerKey string) []*Auth {
		if providerKey != "codex" || len(auths) == 0 {
			return auths
		}

		responsesCapable := make([]*Auth, 0, len(auths))
		chatPreferred := make([]*Auth, 0, len(auths))
		chatFallback := make([]*Auth, 0, len(auths))
		for _, auth := range auths {
			if auth == nil {
				continue
			}
			if !codexAuthSupportsProtocol(auth, requestProtocol) {
				continue
			}
			if codexAuthSupportsNativeResponses(auth) {
				responsesCapable = append(responsesCapable, auth)
				chatFallback = append(chatFallback, auth)
				continue
			}
			chatPreferred = append(chatPreferred, auth)
		}

		switch requestProtocol {
		case "responses":
			if len(responsesCapable) > 0 {
				return responsesCapable
			}
			return chatPreferred
		case "chat/completions":
			if len(chatPreferred) > 0 {
				return chatPreferred
			}
			return chatFallback
		default:
			return auths
		}
	}

	if provider != "mixed" {
		return filterForProvider(candidates, provider)
	}

	byProvider := make(map[string][]*Auth)
	order := make([]string, 0, len(candidates))
	for _, auth := range candidates {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if providerKey == "" {
			continue
		}
		if _, ok := byProvider[providerKey]; !ok {
			order = append(order, providerKey)
		}
		byProvider[providerKey] = append(byProvider[providerKey], auth)
	}

	filtered := make([]*Auth, 0, len(candidates))
	for _, providerKey := range order {
		filtered = append(filtered, filterForProvider(byProvider[providerKey], providerKey)...)
	}
	return filtered
}

func codexRequestProtocol(opts cliproxyexecutor.Options) string {
	if strings.EqualFold(strings.TrimSpace(opts.Alt), "responses/compact") {
		return "responses"
	}
	if strings.EqualFold(strings.TrimSpace(string(opts.SourceFormat)), "openai-response") {
		return "responses"
	}
	return "chat/completions"
}

func codexAuthSupportsProtocol(auth *Auth, protocol string) bool {
	protocols, hasExplicit := codexExplicitProtocols(auth)
	if !hasExplicit {
		switch protocol {
		case "responses":
			// Leave responses eligibility open here; native-vs-bridge is resolved by
			// codexAuthSupportsNativeResponses and the final protocol split below.
			return true
		case "chat/completions":
			return true
		default:
			return true
		}
	}

	switch protocol {
	case "responses":
		return protocols["responses"] || protocols["chat/completions"] || protocols["claude-messages"]
	case "chat/completions":
		return protocols["responses"] || protocols["chat/completions"] || protocols["claude-messages"]
	default:
		return true
	}
}

func codexAuthSupportsNativeResponses(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if protocols, hasExplicit := codexExplicitProtocols(auth); hasExplicit {
		return protocols["responses"]
	}
	if auth.Attributes == nil {
		return true
	}
	apiKey := strings.TrimSpace(auth.Attributes["api_key"])
	if apiKey == "" {
		return true
	}
	baseURL := strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	return baseURL == "" ||
		strings.Contains(baseURL, "chatgpt.com/backend-api/codex") ||
		strings.Contains(baseURL, "/codex/") ||
		strings.HasSuffix(baseURL, "/codex")
}

func codexExplicitProtocols(auth *Auth) (map[string]bool, bool) {
	if auth == nil {
		return nil, false
	}

	rawValues := []string{}
	if auth.Attributes != nil {
		for _, key := range []string{"supported_protocols", "protocols"} {
			if raw := strings.TrimSpace(auth.Attributes[key]); raw != "" {
				rawValues = append(rawValues, raw)
			}
		}
	}
	if auth.Metadata != nil {
		for _, key := range []string{"supported_protocols", "protocols"} {
			if raw, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(raw) != "" {
				rawValues = append(rawValues, raw)
			}
		}
	}
	if len(rawValues) == 0 {
		return codexInferredProtocols(auth)
	}

	protocols := map[string]bool{}
	for _, raw := range rawValues {
		for _, token := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == '|' || r == ' '
		}) {
			switch strings.ToLower(strings.TrimSpace(token)) {
			case "responses", "/responses", "responses/compact", "/responses/compact":
				protocols["responses"] = true
			case "chat", "chat/completions", "/chat/completions", "chat-completions":
				protocols["chat/completions"] = true
			case "messages", "/messages", "claude-messages", "claude/messages", "anthropic/messages":
				protocols["claude-messages"] = true
			}
		}
	}
	if len(protocols) == 0 {
		return codexInferredProtocols(auth)
	}
	return protocols, true
}

func codexInferredProtocols(auth *Auth) (map[string]bool, bool) {
	if auth == nil {
		return nil, false
	}

	matchesNowcoding := func(value string) bool {
		normalized := strings.ToLower(strings.TrimSpace(value))
		return normalized == "nowcoding" || strings.Contains(normalized, "nowcoding.ai")
	}

	candidates := []string{auth.Provider, auth.Prefix, auth.Label}
	if auth.Attributes != nil {
		candidates = append(candidates,
			auth.Attributes["prefix"],
			auth.Attributes["name"],
			auth.Attributes["base_url"],
		)
	}
	if auth.Metadata != nil {
		for _, key := range []string{"prefix", "name", "base_url"} {
			if raw, ok := auth.Metadata[key].(string); ok {
				candidates = append(candidates, raw)
			}
		}
	}
	for _, candidate := range candidates {
		if matchesNowcoding(candidate) {
			return map[string]bool{"claude-messages": true}, true
		}
	}
	return nil, false
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 || interval > refreshCheckInterval {
		interval = refreshCheckInterval
	} else {
		interval = refreshCheckInterval
	}
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
	ctx, cancel := context.WithCancel(parent)
	m.refreshCancel = cancel
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		m.checkRefreshes(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.checkRefreshes(ctx)
			}
		}
	}()
}

// StopAutoRefresh cancels the background refresh loop, if running.
func (m *Manager) StopAutoRefresh() {
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
}

func (m *Manager) checkRefreshes(ctx context.Context) {
	// log.Debugf("checking refreshes")
	now := time.Now()
	snapshot := m.snapshotAuths()
	for _, a := range snapshot {
		typ, _ := a.AccountInfo()
		if typ != "api_key" {
			if !m.shouldRefresh(a, now) {
				continue
			}
			log.Debugf("checking refresh for %s, %s, %s", a.Provider, a.ID, typ)

			if exec := m.executorFor(a.Provider); exec == nil {
				continue
			}
			if !m.markRefreshPending(a.ID, now) {
				continue
			}
			go m.refreshAuth(ctx, a.ID)
		}
	}
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil || a.Disabled {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	if d := durationFromAttributes(a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval"); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[id]
	if !ok || auth == nil || auth.Disabled {
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	return true
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	if auth != nil {
		exec = m.executors[auth.Provider]
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return
	}
	cloned := auth.Clone()
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			current.LastError = &Error{Message: err.Error()}
			m.auths[id] = current
		}
		m.mu.Unlock()
		return
	}
	if updated == nil {
		updated = cloned
	}
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	_, _ = m.Update(ctx, updated)
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[provider]
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec = m.executors[executorKeyFromAuth(a)]
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(ctx context.Context, auth *Auth, method, targetURL string, body []byte, headers http.Header) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	return exec.HttpRequest(ctx, auth, req)
}
