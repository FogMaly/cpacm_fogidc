package auth

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
)

type AuthHealthMetrics struct {
	SuccessCount      int       `json:"success_count"`
	FailureCount      int       `json:"failure_count"`
	Quota429Count     int       `json:"quota_429_count"`
	AvgLatencyMS      float64   `json:"avg_latency_ms"`
	InflightRequests  int       `json:"inflight_requests"`
	RecentRPS10       float64   `json:"recent_rps_10"`
	RecentErrorRate30 float64   `json:"recent_error_rate_30"`
	P95LatencyMS30    float64   `json:"p95_latency_ms_30"`
	CooldownUntil     time.Time `json:"cooldown_until"`
	LastSelectedAt    time.Time `json:"last_selected_at"`
	LastUpdatedAt     time.Time `json:"last_updated_at"`
}

type AuthSelectionRuntime struct {
	Metrics AuthHealthMetrics
	Unified health.UnifiedScore
}

type AuthRuntimeSnapshot struct {
	ID       string
	Provider string
	Prefix   string
	Label    string
	BaseURL  string
	Disabled bool
	Metrics  AuthHealthMetrics
}

func (m *Manager) updateAuthHealth(result Result) {
	if m == nil || result.AuthID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.authHealth[result.AuthID]
	if entry == nil {
		entry = &AuthHealthMetrics{}
		m.authHealth[result.AuthID] = entry
	}
	if result.Success {
		entry.SuccessCount++
	} else {
		entry.FailureCount++
		if result.Error != nil && result.Error.HTTPStatus == 429 {
			entry.Quota429Count++
		}
	}
	if result.Duration > 0 {
		ms := float64(result.Duration.Milliseconds())
		if ms <= 0 {
			ms = float64(result.Duration) / float64(time.Millisecond)
		}
		if entry.AvgLatencyMS <= 0 {
			entry.AvgLatencyMS = ms
		} else {
			entry.AvgLatencyMS = entry.AvgLatencyMS*0.8 + ms*0.2
		}
	}
	entry.LastUpdatedAt = time.Now().UTC()
}

func (m *Manager) authHealthSnapshot(authID string) AuthHealthMetrics {
	if m == nil || strings.TrimSpace(authID) == "" {
		return AuthHealthMetrics{}
	}
	m.mu.RLock()
	entry := m.authHealth[authID]
	var metrics AuthHealthMetrics
	if entry != nil {
		metrics = *entry
	}
	settings := m.schedulerSettings()
	now := time.Now().UTC()
	load := snapshotRuntimeLoadTracker(m.authRuntimeLoads[strings.TrimSpace(authID)], now, settings)
	m.mu.RUnlock()
	metrics.InflightRequests = load.Inflight
	metrics.RecentRPS10 = load.RecentRPS
	metrics.RecentErrorRate30 = load.ErrorRate
	metrics.P95LatencyMS30 = load.P95LatencyMS
	metrics.CooldownUntil = load.CooldownUntil
	metrics.LastSelectedAt = load.LastSelectedAt
	return metrics
}

func healthScore(metrics AuthHealthMetrics) float64 {
	total := metrics.SuccessCount + metrics.FailureCount
	successRate := 1.0
	if total > 0 {
		successRate = float64(metrics.SuccessCount) / float64(total)
	}
	latencyPenalty := 0.0
	if metrics.AvgLatencyMS > 0 {
		latencyPenalty = math.Min(metrics.AvgLatencyMS/1000.0, 10)
	}
	score := successRate*100 - float64(metrics.Quota429Count*10) - latencyPenalty
	score -= math.Min(float64(metrics.InflightRequests)*5, 20)
	score -= math.Min(metrics.RecentErrorRate30*100*0.5, 35)
	if metrics.P95LatencyMS30 > 0 {
		score -= math.Min(metrics.P95LatencyMS30/300.0, 25)
	}
	if !metrics.CooldownUntil.IsZero() && metrics.CooldownUntil.After(time.Now().UTC()) {
		score *= 0.25
	}
	return score
}

func authSelectionRuntime(runtime any) AuthSelectionRuntime {
	switch metrics := runtime.(type) {
	case AuthSelectionRuntime:
		return metrics
	case *AuthSelectionRuntime:
		if metrics != nil {
			return *metrics
		}
	case AuthHealthMetrics:
		return AuthSelectionRuntime{Metrics: metrics}
	case *AuthHealthMetrics:
		if metrics != nil {
			return AuthSelectionRuntime{Metrics: *metrics}
		}
	}
	return AuthSelectionRuntime{}
}

func (m *Manager) buildAuthSelectionRuntime(ctx context.Context, auth *Auth, model string) AuthSelectionRuntime {
	metrics := m.authHealthSnapshot(auth.ID)
	return AuthSelectionRuntime{
		Metrics: metrics,
		Unified: m.unifiedHealthForAuth(ctx, auth, model, metrics),
	}
}

func (m *Manager) unifiedHealthForAuth(ctx context.Context, auth *Auth, model string, metrics AuthHealthMetrics) health.UnifiedScore {
	signals := health.UnifiedSignals{}
	if auth != nil {
		now := time.Now().UTC()
		var probeUpdatedAt time.Time
		if metrics.LastUpdatedAt.IsZero() && (metrics.SuccessCount > 0 || metrics.FailureCount > 0 || metrics.Quota429Count > 0 || metrics.AvgLatencyMS > 0) {
			metrics.LastUpdatedAt = now
		}
		if metrics.SuccessCount > 0 || metrics.FailureCount > 0 || metrics.Quota429Count > 0 || metrics.AvgLatencyMS > 0 || !metrics.LastUpdatedAt.IsZero() {
			signals.Runtime = health.RuntimeSignal{
				SuccessCount:      metrics.SuccessCount,
				FailureCount:      metrics.FailureCount,
				Quota429Count:     metrics.Quota429Count,
				AvgLatencyMS:      metrics.AvgLatencyMS,
				InflightRequests:  metrics.InflightRequests,
				RecentRPS10:       metrics.RecentRPS10,
				RecentErrorRate30: metrics.RecentErrorRate30,
				P95LatencyMS30:    metrics.P95LatencyMS30,
				CooldownUntil:     metrics.CooldownUntil,
				LastUpdatedAt:     metrics.LastUpdatedAt,
				Found:             true,
			}
		}
		snapshot := m.cachedProbeSnapshot()
		if snapshot != nil {
			probeUpdatedAt, _ = time.Parse(time.RFC3339, strings.TrimSpace(snapshot.UpdatedAt))
			signals.Probe = health.FindProbeSignal(snapshot, auth.Prefix, auth.Attributes["base_url"], model, now)
		}
		signals.Outage = m.runtimeOutageSignalForAuth(auth, model, probeUpdatedAt)
		balances := m.cachedBalanceSnapshot(ctx)
		if len(balances) > 0 {
			signals.Balance = health.SelectBalanceSignal(balances, auth.Prefix, auth.Attributes["base_url"], auth.Provider, auth.Label)
		}
	}
	return health.Score(signals, time.Now().UTC())
}

func (m *Manager) cachedProbeSnapshot() *health.ProbeSnapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	path := m.probeSnapshotPath
	cached := m.probeCache
	cachedAt := m.probeCacheAt
	m.mu.RUnlock()
	if cached != nil && time.Since(cachedAt) < 15*time.Second {
		return cached
	}
	snapshot, err := health.LoadProbeSnapshot(path)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	m.probeCache = snapshot
	m.probeCacheAt = time.Now()
	m.mu.Unlock()
	return snapshot
}

func (m *Manager) cachedBalanceSnapshot(ctx context.Context) []health.ProviderBalanceSnapshotItem {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	source := m.balanceSource
	cached := append([]health.ProviderBalanceSnapshotItem(nil), m.balanceCache...)
	cachedAt := m.balanceCacheAt
	refreshing := m.balanceRefreshInFlight
	m.mu.RUnlock()
	if len(cached) > 0 && time.Since(cachedAt) < 2*time.Minute {
		return cached
	}
	if source == nil {
		return cached
	}
	if !refreshing {
		m.mu.Lock()
		if !m.balanceRefreshInFlight {
			m.balanceRefreshInFlight = true
			go m.refreshBalanceSnapshotAsync(source)
		}
		m.mu.Unlock()
	}
	return cached
}

func (m *Manager) refreshBalanceSnapshotAsync(source func(context.Context) []health.ProviderBalanceSnapshotItem) {
	if m == nil || source == nil {
		return
	}
	refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items := source(refreshCtx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(items) > 0 {
		m.balanceCache = append(m.balanceCache[:0], items...)
		m.balanceCacheAt = time.Now()
	}
	m.balanceRefreshInFlight = false
}

func (m *Manager) RuntimeHealthSnapshot() []AuthRuntimeSnapshot {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AuthRuntimeSnapshot, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		out = append(out, AuthRuntimeSnapshot{
			ID:       auth.ID,
			Provider: auth.Provider,
			Prefix:   auth.Prefix,
			Label:    auth.Label,
			BaseURL:  strings.TrimSpace(auth.Attributes["base_url"]),
			Disabled: auth.Disabled || auth.Status == StatusDisabled,
			Metrics:  m.authHealthSnapshot(auth.ID),
		})
	}
	return out
}
