package auth

import (
	"math"
	"sort"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
)

const (
	defaultSchedulerSoftOverloadThreshold = 0.75
	defaultSchedulerHardOverloadThreshold = 0.90
	defaultSchedulerRecoverThreshold      = 0.68
	defaultSchedulerStickyBonus           = 0.12
	defaultSchedulerStickyTTLSeconds      = 900
	defaultSchedulerCooldownSeconds       = 60
	defaultSchedulerHalfOpenProbeRatio    = 0.10
	defaultSchedulerRPSWindowSeconds      = 10
	defaultSchedulerErrorWindowSeconds    = 30
	defaultSchedulerLatencyWindowSeconds  = 30
	defaultLoadWeightInflight             = 1.0
	defaultLoadWeightSticky               = 0.35
	defaultLoadWeightToken                = 0.25
	defaultAuthScoreWeightHealth          = 0.45
	defaultAuthScoreWeightBalance         = 0.20
	defaultAuthScoreWeightLatency         = 0.20
	defaultAuthScoreWeightIdle            = 0.15
	defaultProviderMaxCapacity            = 32
	targetProviderP95LatencyMS            = 1500.0
)

type runtimeLoadTracker struct {
	Inflight       int
	Starts         []time.Time
	Completions    []runtimeCompletion
	LastSelectedAt time.Time
	CooldownUntil  time.Time
}

type runtimeCompletion struct {
	At       time.Time
	Success  bool
	Duration time.Duration
}

type schedulerSettings struct {
	SoftOverloadThreshold float64
	HardOverloadThreshold float64
	RecoverThreshold      float64
	StickyBonus           float64
	StickyTTL             time.Duration
	Cooldown              time.Duration
	HalfOpenProbeRatio    float64
	RPSWindow             time.Duration
	ErrorWindow           time.Duration
	LatencyWindow         time.Duration
	LoadWeights           internalconfig.RoutingLoadWeights
	AuthScoreWeights      internalconfig.RoutingAuthScoreWeights
}

type runtimeLoadSnapshot struct {
	Inflight       int
	RecentRPS      float64
	ErrorRate      float64
	P95LatencyMS   float64
	LastSelectedAt time.Time
	CooldownUntil  time.Time
}

func (m *Manager) schedulerSettings() schedulerSettings {
	out := schedulerSettings{
		SoftOverloadThreshold: defaultSchedulerSoftOverloadThreshold,
		HardOverloadThreshold: defaultSchedulerHardOverloadThreshold,
		RecoverThreshold:      defaultSchedulerRecoverThreshold,
		StickyBonus:           defaultSchedulerStickyBonus,
		StickyTTL:             defaultRequestAffinityTTL,
		Cooldown:              time.Duration(defaultSchedulerCooldownSeconds) * time.Second,
		HalfOpenProbeRatio:    defaultSchedulerHalfOpenProbeRatio,
		RPSWindow:             time.Duration(defaultSchedulerRPSWindowSeconds) * time.Second,
		ErrorWindow:           time.Duration(defaultSchedulerErrorWindowSeconds) * time.Second,
		LatencyWindow:         time.Duration(defaultSchedulerLatencyWindowSeconds) * time.Second,
		LoadWeights: internalconfig.RoutingLoadWeights{
			Inflight:       defaultLoadWeightInflight,
			StickySessions: defaultLoadWeightSticky,
			TokenSlots:     defaultLoadWeightToken,
		},
		AuthScoreWeights: internalconfig.RoutingAuthScoreWeights{
			Health:  defaultAuthScoreWeightHealth,
			Balance: defaultAuthScoreWeightBalance,
			Latency: defaultAuthScoreWeightLatency,
			Idle:    defaultAuthScoreWeightIdle,
		},
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return out
	}
	scheduler := cfg.Routing.Scheduler
	if scheduler.SoftOverloadThreshold > 0 {
		out.SoftOverloadThreshold = scheduler.SoftOverloadThreshold
	}
	if scheduler.HardOverloadThreshold > 0 {
		out.HardOverloadThreshold = scheduler.HardOverloadThreshold
	}
	if scheduler.RecoverThreshold > 0 {
		out.RecoverThreshold = scheduler.RecoverThreshold
	}
	if scheduler.StickyBonus > 0 {
		out.StickyBonus = scheduler.StickyBonus
	}
	if scheduler.StickyTTLSeconds > 0 {
		out.StickyTTL = time.Duration(scheduler.StickyTTLSeconds) * time.Second
	}
	if scheduler.CooldownSeconds > 0 {
		out.Cooldown = time.Duration(scheduler.CooldownSeconds) * time.Second
	}
	if scheduler.HalfOpenProbeRatio > 0 {
		out.HalfOpenProbeRatio = scheduler.HalfOpenProbeRatio
	}
	if scheduler.Metrics.RPSWindowSeconds > 0 {
		out.RPSWindow = time.Duration(scheduler.Metrics.RPSWindowSeconds) * time.Second
	}
	if scheduler.Metrics.ErrorWindowSeconds > 0 {
		out.ErrorWindow = time.Duration(scheduler.Metrics.ErrorWindowSeconds) * time.Second
	}
	if scheduler.Metrics.LatencyWindowSeconds > 0 {
		out.LatencyWindow = time.Duration(scheduler.Metrics.LatencyWindowSeconds) * time.Second
	}
	if scheduler.Weights.Inflight > 0 {
		out.LoadWeights.Inflight = scheduler.Weights.Inflight
	}
	if scheduler.Weights.StickySessions > 0 {
		out.LoadWeights.StickySessions = scheduler.Weights.StickySessions
	}
	if scheduler.Weights.TokenSlots > 0 {
		out.LoadWeights.TokenSlots = scheduler.Weights.TokenSlots
	}
	if scheduler.AuthScoreWeights.Health > 0 {
		out.AuthScoreWeights.Health = scheduler.AuthScoreWeights.Health
	}
	if scheduler.AuthScoreWeights.Balance > 0 {
		out.AuthScoreWeights.Balance = scheduler.AuthScoreWeights.Balance
	}
	if scheduler.AuthScoreWeights.Latency > 0 {
		out.AuthScoreWeights.Latency = scheduler.AuthScoreWeights.Latency
	}
	if scheduler.AuthScoreWeights.Idle > 0 {
		out.AuthScoreWeights.Idle = scheduler.AuthScoreWeights.Idle
	}
	return out
}

func (m *Manager) providerRoutingProfile(provider string) internalconfig.RoutingProvider {
	provider = strings.ToLower(strings.TrimSpace(provider))
	profile := internalconfig.RoutingProvider{
		CapacityWeight: 1.0,
		MaxCapacity:    defaultProviderMaxCapacity,
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return profile
	}
	if limit := cfg.Routing.ProviderConcurrency[provider]; limit > 0 {
		profile.MaxCapacity = limit
	}
	if entry, ok := cfg.Routing.Scheduler.Providers[provider]; ok {
		if entry.CapacityWeight > 0 {
			profile.CapacityWeight = entry.CapacityWeight
		}
		if entry.MaxCapacity > 0 {
			profile.MaxCapacity = entry.MaxCapacity
		}
	}
	return profile
}

func (m *Manager) authCapacity(auth *Auth) int {
	if auth == nil {
		return 1
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	profile := m.providerRoutingProfile(provider)
	count := 0
	m.mu.RLock()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(candidate.Provider), provider) {
			count++
		}
	}
	m.mu.RUnlock()
	if count <= 0 {
		count = 1
	}
	capacity := profile.MaxCapacity / count
	if capacity <= 0 {
		capacity = 1
	}
	return capacity
}

func (m *Manager) beginRuntimeLoad(auth *Auth, provider string) func(bool, time.Duration, *Error) {
	if m == nil {
		return func(bool, time.Duration, *Error) {}
	}
	now := time.Now().UTC()
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}

	m.mu.Lock()
	if providerKey != "" {
		tracker := ensureRuntimeLoadTracker(m.providerRuntimeLoads, providerKey)
		tracker.Inflight++
		tracker.Starts = append(tracker.Starts, now)
		tracker.LastSelectedAt = now
	}
	if authID != "" {
		tracker := ensureRuntimeLoadTracker(m.authRuntimeLoads, authID)
		tracker.Inflight++
		tracker.Starts = append(tracker.Starts, now)
		tracker.LastSelectedAt = now
	}
	m.mu.Unlock()

	var once bool
	return func(success bool, duration time.Duration, resultErr *Error) {
		if once {
			return
		}
		once = true
		m.finishRuntimeLoad(auth, providerKey, success, duration, resultErr)
	}
}

func (m *Manager) finishRuntimeLoad(auth *Auth, provider string, success bool, duration time.Duration, resultErr *Error) {
	if m == nil {
		return
	}
	now := time.Now().UTC()
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	settings := m.schedulerSettings()

	m.mu.Lock()
	defer m.mu.Unlock()
	if provider != "" {
		tracker := ensureRuntimeLoadTracker(m.providerRuntimeLoads, provider)
		if tracker.Inflight > 0 {
			tracker.Inflight--
		}
		tracker.Completions = append(tracker.Completions, runtimeCompletion{At: now, Success: success, Duration: duration})
		if !success && shouldApplyRuntimeCooldown(resultErr) {
			tracker.CooldownUntil = now.Add(settings.Cooldown)
		}
		trimRuntimeLoadTrackerLocked(tracker, now, settings)
	}
	if authID != "" {
		tracker := ensureRuntimeLoadTracker(m.authRuntimeLoads, authID)
		if tracker.Inflight > 0 {
			tracker.Inflight--
		}
		tracker.Completions = append(tracker.Completions, runtimeCompletion{At: now, Success: success, Duration: duration})
		if !success && shouldApplyRuntimeCooldown(resultErr) {
			tracker.CooldownUntil = now.Add(settings.Cooldown)
		}
		trimRuntimeLoadTrackerLocked(tracker, now, settings)
	}
}

func ensureRuntimeLoadTracker(m map[string]*runtimeLoadTracker, key string) *runtimeLoadTracker {
	if m == nil {
		return &runtimeLoadTracker{}
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return &runtimeLoadTracker{}
	}
	tracker := m[key]
	if tracker == nil {
		tracker = &runtimeLoadTracker{}
		m[key] = tracker
	}
	return tracker
}

func trimRuntimeLoadTrackerLocked(tracker *runtimeLoadTracker, now time.Time, settings schedulerSettings) {
	if tracker == nil {
		return
	}
	if settings.RPSWindow <= 0 {
		settings.RPSWindow = time.Duration(defaultSchedulerRPSWindowSeconds) * time.Second
	}
	if settings.ErrorWindow <= 0 {
		settings.ErrorWindow = time.Duration(defaultSchedulerErrorWindowSeconds) * time.Second
	}
	if settings.LatencyWindow <= 0 {
		settings.LatencyWindow = time.Duration(defaultSchedulerLatencyWindowSeconds) * time.Second
	}
	trimStartsSince := now.Add(-settings.RPSWindow)
	startCut := 0
	for _, startedAt := range tracker.Starts {
		if startedAt.After(trimStartsSince) || startedAt.Equal(trimStartsSince) {
			break
		}
		startCut++
	}
	if startCut > 0 {
		tracker.Starts = append([]time.Time(nil), tracker.Starts[startCut:]...)
	}
	maxWindow := settings.ErrorWindow
	if settings.LatencyWindow > maxWindow {
		maxWindow = settings.LatencyWindow
	}
	trimCompletionsSince := now.Add(-maxWindow)
	doneCut := 0
	for _, item := range tracker.Completions {
		if item.At.After(trimCompletionsSince) || item.At.Equal(trimCompletionsSince) {
			break
		}
		doneCut++
	}
	if doneCut > 0 {
		tracker.Completions = append([]runtimeCompletion(nil), tracker.Completions[doneCut:]...)
	}
}

func snapshotRuntimeLoadTracker(tracker *runtimeLoadTracker, now time.Time, settings schedulerSettings) runtimeLoadSnapshot {
	if tracker == nil {
		return runtimeLoadSnapshot{}
	}
	copyTracker := *tracker
	trimRuntimeLoadTrackerLocked(&copyTracker, now, settings)

	rpsWindow := settings.RPSWindow.Seconds()
	if rpsWindow <= 0 {
		rpsWindow = defaultSchedulerRPSWindowSeconds
	}
	recentRPS := float64(len(copyTracker.Starts)) / rpsWindow

	errorSince := now.Add(-settings.ErrorWindow)
	totalCompletions := 0
	failures := 0
	latencySince := now.Add(-settings.LatencyWindow)
	latencies := make([]float64, 0, len(copyTracker.Completions))
	for _, item := range copyTracker.Completions {
		if item.At.After(errorSince) || item.At.Equal(errorSince) {
			totalCompletions++
			if !item.Success {
				failures++
			}
		}
		if item.At.After(latencySince) || item.At.Equal(latencySince) {
			latencies = append(latencies, float64(item.Duration)/float64(time.Millisecond))
		}
	}
	errorRate := 0.0
	if totalCompletions > 0 {
		errorRate = float64(failures) / float64(totalCompletions)
	}
	p95 := percentile(latencies, 0.95)
	return runtimeLoadSnapshot{
		Inflight:       copyTracker.Inflight,
		RecentRPS:      recentRPS,
		ErrorRate:      errorRate,
		P95LatencyMS:   p95,
		LastSelectedAt: copyTracker.LastSelectedAt,
		CooldownUntil:  copyTracker.CooldownUntil,
	}
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	if p <= 0 {
		return values[0]
	}
	if p >= 1 {
		return values[len(values)-1]
	}
	index := int(math.Ceil(float64(len(values))*p)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

func (m *Manager) authRuntimeLoadSnapshot(authID string) runtimeLoadSnapshot {
	if m == nil || strings.TrimSpace(authID) == "" {
		return runtimeLoadSnapshot{}
	}
	settings := m.schedulerSettings()
	now := time.Now().UTC()
	m.mu.RLock()
	tracker := m.authRuntimeLoads[strings.TrimSpace(authID)]
	snapshot := snapshotRuntimeLoadTracker(tracker, now, settings)
	m.mu.RUnlock()
	return snapshot
}

func (m *Manager) providerRuntimeLoadSnapshot(provider string) runtimeLoadSnapshot {
	if m == nil || strings.TrimSpace(provider) == "" {
		return runtimeLoadSnapshot{}
	}
	settings := m.schedulerSettings()
	now := time.Now().UTC()
	key := strings.ToLower(strings.TrimSpace(provider))
	m.mu.RLock()
	tracker := m.providerRuntimeLoads[key]
	snapshot := snapshotRuntimeLoadTracker(tracker, now, settings)
	m.mu.RUnlock()
	return snapshot
}

func (m *Manager) RuntimeLoadSnapshot() []health.RuntimeLoadSnapshotItem {
	if m == nil {
		return nil
	}
	settings := m.schedulerSettings()
	now := time.Now().UTC()
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]health.RuntimeLoadSnapshotItem, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		snapshot := snapshotRuntimeLoadTracker(m.authRuntimeLoads[strings.TrimSpace(auth.ID)], now, settings)
		baseURL := ""
		if auth.Attributes != nil {
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		}
		out = append(out, health.RuntimeLoadSnapshotItem{
			AuthID:         strings.TrimSpace(auth.ID),
			Provider:       strings.TrimSpace(auth.Provider),
			Prefix:         strings.TrimSpace(auth.Prefix),
			BaseURL:        baseURL,
			Label:          strings.TrimSpace(auth.Label),
			Inflight:       snapshot.Inflight,
			RecentRPS:      snapshot.RecentRPS,
			ErrorRate:      snapshot.ErrorRate,
			P95LatencyMS:   snapshot.P95LatencyMS,
			LastSelectedAt: snapshot.LastSelectedAt,
			CooldownUntil:  snapshot.CooldownUntil,
		})
	}
	return out
}

func (m *Manager) ProviderRuntimeLoadSnapshot() []health.ProviderRuntimeLoadSnapshotItem {
	if m == nil {
		return nil
	}
	settings := m.schedulerSettings()
	now := time.Now().UTC()
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]health.ProviderRuntimeLoadSnapshotItem, 0, len(m.providerRuntimeLoads))
	for provider, tracker := range m.providerRuntimeLoads {
		snapshot := snapshotRuntimeLoadTracker(tracker, now, settings)
		out = append(out, health.ProviderRuntimeLoadSnapshotItem{
			Provider:       provider,
			Inflight:       snapshot.Inflight,
			RecentRPS:      snapshot.RecentRPS,
			ErrorRate:      snapshot.ErrorRate,
			P95LatencyMS:   snapshot.P95LatencyMS,
			LastSelectedAt: snapshot.LastSelectedAt,
			CooldownUntil:  snapshot.CooldownUntil,
		})
	}
	return out
}

func shouldApplyRuntimeCooldown(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	status := statusCodeFromResult(resultErr)
	switch status {
	case 0, 408, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func clampUnit(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func (m *Manager) authLoadRatio(auth *Auth, now time.Time) float64 {
	if m == nil || auth == nil {
		return 0
	}
	snapshot := m.authRuntimeLoadSnapshot(strings.TrimSpace(auth.ID))
	capacity := m.authCapacity(auth)
	if capacity <= 0 {
		capacity = 1
	}
	return clampUnit(float64(snapshot.Inflight) / float64(capacity))
}

func (m *Manager) providerLoadRatio(provider string, now time.Time) float64 {
	if m == nil {
		return 0
	}
	snapshot := m.providerRuntimeLoadSnapshot(provider)
	profile := m.providerRoutingProfile(provider)
	maxCapacity := profile.MaxCapacity
	if maxCapacity <= 0 {
		maxCapacity = defaultProviderMaxCapacity
	}
	return clampUnit(float64(snapshot.Inflight) / float64(maxCapacity))
}

func (m *Manager) providerSelectionScore(provider string, now time.Time) float64 {
	if m == nil {
		return 0
	}
	profile := m.providerRoutingProfile(provider)
	snapshot := m.providerRuntimeLoadSnapshot(provider)
	healthFactor := clampUnit(1 - snapshot.ErrorRate)
	if healthFactor < 0.2 {
		healthFactor = 0.2
	}
	latencyFactor := 1.0
	if snapshot.P95LatencyMS > targetProviderP95LatencyMS {
		latencyFactor = targetProviderP95LatencyMS / snapshot.P95LatencyMS
		if latencyFactor < 0.4 {
			latencyFactor = 0.4
		}
	}
	loadFactor := 1 - m.providerLoadRatio(provider, now)
	if loadFactor < 0.2 {
		loadFactor = 0.2
	}
	score := profile.CapacityWeight * healthFactor * latencyFactor * loadFactor
	if !snapshot.CooldownUntil.IsZero() && snapshot.CooldownUntil.After(now) {
		score *= m.schedulerSettings().HalfOpenProbeRatio
	}
	return score
}
