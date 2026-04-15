package management

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type loadBalancerRuntimePayload struct {
	GeneratedAt     string                     `json:"generated_at"`
	Providers       []loadBalancerProviderItem `json:"providers,omitempty"`
	Prefixes        []loadBalancerPrefixItem   `json:"prefixes"`
	Auths           []loadBalancerAuthItem     `json:"auths,omitempty"`
	AdaptiveProbes  []adaptiveProbeQueueItem   `json:"adaptive_probes,omitempty"`
	RecentEvents    []coreauth.RoutingEvent    `json:"recent_events,omitempty"`
	RoutingSnapshot coreauth.RoutingSnapshot   `json:"routing_snapshot"`
}

type loadBalancerProviderItem struct {
	Provider          string  `json:"provider"`
	TotalAuths        int     `json:"total_auths"`
	AvailableAuths    int     `json:"available_auths"`
	CooldownAuths     int     `json:"cooldown_auths"`
	DisabledAuths     int     `json:"disabled_auths"`
	UnauthorizedAuths int     `json:"unauthorized_auths"`
	InflightRequests  int     `json:"inflight_requests"`
	RecentRPS10       float64 `json:"recent_rps_10"`
	RecentErrorRate30 float64 `json:"recent_error_rate_30"`
	P95LatencyMS30    float64 `json:"p95_latency_ms_30"`
	LastSelectedAt    string  `json:"last_selected_at,omitempty"`
	CooldownUntil     string  `json:"cooldown_until,omitempty"`
}

type loadBalancerPrefixItem struct {
	Prefix            string                  `json:"prefix"`
	ProviderKinds     []string                `json:"provider_kinds,omitempty"`
	AuthCount         int                     `json:"auth_count"`
	DisabledAuths     int                     `json:"disabled_auths"`
	InflightRequests  int                     `json:"inflight_requests"`
	RecentRPS10       float64                 `json:"recent_rps_10"`
	RecentErrorRate30 float64                 `json:"recent_error_rate_30"`
	P95LatencyMS30    float64                 `json:"p95_latency_ms_30"`
	SuccessCount      int                     `json:"success_count"`
	FailureCount      int                     `json:"failure_count"`
	Quota429Count     int                     `json:"quota_429_count"`
	AvgLatencyMS      float64                 `json:"avg_latency_ms"`
	LastSelectedAt    string                  `json:"last_selected_at,omitempty"`
	LastUpdatedAt     string                  `json:"last_updated_at,omitempty"`
	CooldownUntil     string                  `json:"cooldown_until,omitempty"`
	AdaptiveProbe     *adaptiveProbeQueueItem `json:"adaptive_probe,omitempty"`
}

type loadBalancerAuthItem struct {
	AuthID            string  `json:"auth_id"`
	Provider          string  `json:"provider"`
	Prefix            string  `json:"prefix"`
	Label             string  `json:"label,omitempty"`
	BaseURL           string  `json:"base_url,omitempty"`
	Disabled          bool    `json:"disabled"`
	InflightRequests  int     `json:"inflight_requests"`
	RecentRPS10       float64 `json:"recent_rps_10"`
	RecentErrorRate30 float64 `json:"recent_error_rate_30"`
	P95LatencyMS30    float64 `json:"p95_latency_ms_30"`
	SuccessCount      int     `json:"success_count"`
	FailureCount      int     `json:"failure_count"`
	Quota429Count     int     `json:"quota_429_count"`
	AvgLatencyMS      float64 `json:"avg_latency_ms"`
	LastSelectedAt    string  `json:"last_selected_at,omitempty"`
	LastUpdatedAt     string  `json:"last_updated_at,omitempty"`
	CooldownUntil     string  `json:"cooldown_until,omitempty"`
}

func (h *Handler) GetLoadBalancerRuntime(c *gin.Context) {
	eventLimit := 50
	if raw := strings.TrimSpace(c.Query("event_limit")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value >= 0 && value <= 200 {
			eventLimit = value
		}
	}
	payload := h.collectLoadBalancerRuntime(c.Request.Context(), eventLimit)
	c.JSON(http.StatusOK, payload)
}

func (h *Handler) collectLoadBalancerRuntime(_ context.Context, eventLimit int) loadBalancerRuntimePayload {
	payload := loadBalancerRuntimePayload{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		AdaptiveProbes: h.adaptiveProbeQueueSnapshot(),
	}
	if h == nil || h.authManager == nil {
		return payload
	}

	loads := h.authManager.RuntimeLoadSnapshot()
	providerLoads := h.authManager.ProviderRuntimeLoadSnapshot()
	runtimes := h.authManager.RuntimeHealthSnapshot()
	routing := h.authManager.RoutingSnapshot(eventLimit)
	payload.RoutingSnapshot = routing
	payload.RecentEvents = routing.RecentEvents

	runtimeByAuth := make(map[string]coreauth.AuthRuntimeSnapshot, len(runtimes))
	for _, item := range runtimes {
		runtimeByAuth[strings.TrimSpace(item.ID)] = item
	}
	adaptiveByPrefix := make(map[string]*adaptiveProbeQueueItem, len(payload.AdaptiveProbes))
	for i := range payload.AdaptiveProbes {
		item := payload.AdaptiveProbes[i]
		adaptiveByPrefix[strings.ToLower(strings.TrimSpace(item.Prefix))] = &payload.AdaptiveProbes[i]
	}

	providerRouting := make(map[string]coreauth.RoutingProviderStatus, len(routing.Providers))
	providerLoadByKey := make(map[string]loadBalancerProviderItem, len(providerLoads))
	for _, item := range routing.Providers {
		providerRouting[strings.ToLower(strings.TrimSpace(item.Provider))] = item
	}
	for _, load := range providerLoads {
		key := strings.ToLower(strings.TrimSpace(load.Provider))
		status := providerRouting[key]
		providerLoadByKey[key] = loadBalancerProviderItem{
			Provider:          strings.TrimSpace(load.Provider),
			TotalAuths:        status.TotalAuths,
			AvailableAuths:    status.AvailableAuths,
			CooldownAuths:     status.CooldownAuths,
			DisabledAuths:     status.DisabledAuths,
			UnauthorizedAuths: status.UnauthorizedAuths,
			InflightRequests:  load.Inflight,
			RecentRPS10:       load.RecentRPS,
			RecentErrorRate30: load.ErrorRate,
			P95LatencyMS30:    load.P95LatencyMS,
			LastSelectedAt:    formatRuntimeTime(load.LastSelectedAt),
			CooldownUntil:     formatRuntimeTime(load.CooldownUntil),
		}
	}
	providerCapacity := len(providerLoads)
	if len(routing.Providers) > providerCapacity {
		providerCapacity = len(routing.Providers)
	}
	payload.Providers = make([]loadBalancerProviderItem, 0, providerCapacity)
	for key, status := range providerRouting {
		item, ok := providerLoadByKey[key]
		if !ok {
			item = loadBalancerProviderItem{Provider: strings.TrimSpace(status.Provider)}
		}
		item.TotalAuths = status.TotalAuths
		item.AvailableAuths = status.AvailableAuths
		item.CooldownAuths = status.CooldownAuths
		item.DisabledAuths = status.DisabledAuths
		item.UnauthorizedAuths = status.UnauthorizedAuths
		providerLoadByKey[key] = item
	}
	for _, item := range providerLoadByKey {
		payload.Providers = append(payload.Providers, item)
	}
	sort.Slice(payload.Providers, func(i, j int) bool {
		if payload.Providers[i].InflightRequests == payload.Providers[j].InflightRequests {
			return payload.Providers[i].Provider < payload.Providers[j].Provider
		}
		return payload.Providers[i].InflightRequests > payload.Providers[j].InflightRequests
	})

	prefixes := make(map[string]*loadBalancerPrefixItem)
	authItems := make([]loadBalancerAuthItem, 0, len(loads))
	prefixLatencyWeight := make(map[string]float64, len(loads))
	prefixErrorWeight := make(map[string]float64, len(loads))

	for _, load := range loads {
		prefix := strings.ToLower(strings.TrimSpace(load.Prefix))
		if prefix == "" {
			prefix = strings.ToLower(strings.TrimSpace(load.Provider))
		}
		agg := prefixes[prefix]
		if agg == nil {
			agg = &loadBalancerPrefixItem{Prefix: prefix}
			if adaptive, ok := adaptiveByPrefix[prefix]; ok {
				copyAdaptive := *adaptive
				agg.AdaptiveProbe = &copyAdaptive
			}
			prefixes[prefix] = agg
		}
		agg.AuthCount++
		if providerKind := strings.TrimSpace(load.Provider); providerKind != "" {
			agg.ProviderKinds = appendIfMissing(agg.ProviderKinds, providerKind)
		}
		agg.InflightRequests += load.Inflight
		agg.RecentRPS10 += load.RecentRPS
		if load.P95LatencyMS > agg.P95LatencyMS30 {
			agg.P95LatencyMS30 = load.P95LatencyMS
		}
		if lastSelected := load.LastSelectedAt.UTC(); lastSelected.After(parseRuntimeTime(agg.LastSelectedAt)) {
			agg.LastSelectedAt = formatRuntimeTime(lastSelected)
		}
		if cooldown := load.CooldownUntil.UTC(); cooldown.After(parseRuntimeTime(agg.CooldownUntil)) {
			agg.CooldownUntil = formatRuntimeTime(cooldown)
		}

		runtime := runtimeByAuth[strings.TrimSpace(load.AuthID)]
		agg.SuccessCount += runtime.Metrics.SuccessCount
		agg.FailureCount += runtime.Metrics.FailureCount
		agg.Quota429Count += runtime.Metrics.Quota429Count
		completionWeight := float64(runtime.Metrics.SuccessCount + runtime.Metrics.FailureCount)
		agg.AvgLatencyMS = weightedAverageLatency(agg.AvgLatencyMS, prefixLatencyWeight[prefix], runtime.Metrics.AvgLatencyMS, completionWeight)
		prefixLatencyWeight[prefix] += completionWeight
		errorWeight := load.RecentRPS
		if errorWeight <= 0 && load.Inflight > 0 {
			errorWeight = float64(load.Inflight)
		}
		agg.RecentErrorRate30 = weightedAverageLatency(agg.RecentErrorRate30, prefixErrorWeight[prefix], load.ErrorRate, errorWeight)
		prefixErrorWeight[prefix] += errorWeight
		if runtime.Disabled {
			agg.DisabledAuths++
		}
		if runtime.Metrics.LastUpdatedAt.After(parseRuntimeTime(agg.LastUpdatedAt)) {
			agg.LastUpdatedAt = formatRuntimeTime(runtime.Metrics.LastUpdatedAt)
		}

		authItem := loadBalancerAuthItem{
			AuthID:            strings.TrimSpace(load.AuthID),
			Provider:          strings.TrimSpace(load.Provider),
			Prefix:            prefix,
			Label:             strings.TrimSpace(load.Label),
			BaseURL:           strings.TrimSpace(load.BaseURL),
			Disabled:          runtime.Disabled,
			InflightRequests:  load.Inflight,
			RecentRPS10:       load.RecentRPS,
			RecentErrorRate30: load.ErrorRate,
			P95LatencyMS30:    load.P95LatencyMS,
			SuccessCount:      runtime.Metrics.SuccessCount,
			FailureCount:      runtime.Metrics.FailureCount,
			Quota429Count:     runtime.Metrics.Quota429Count,
			AvgLatencyMS:      runtime.Metrics.AvgLatencyMS,
			LastSelectedAt:    formatRuntimeTime(load.LastSelectedAt),
			LastUpdatedAt:     formatRuntimeTime(runtime.Metrics.LastUpdatedAt),
			CooldownUntil:     formatRuntimeTime(load.CooldownUntil),
		}
		authItems = append(authItems, authItem)
	}

	for _, agg := range prefixes {
		sort.Strings(agg.ProviderKinds)
		payload.Prefixes = append(payload.Prefixes, *agg)
	}

	sort.Slice(payload.Prefixes, func(i, j int) bool {
		if payload.Prefixes[i].InflightRequests == payload.Prefixes[j].InflightRequests {
			return payload.Prefixes[i].Prefix < payload.Prefixes[j].Prefix
		}
		return payload.Prefixes[i].InflightRequests > payload.Prefixes[j].InflightRequests
	})
	sort.Slice(authItems, func(i, j int) bool {
		if authItems[i].Prefix == authItems[j].Prefix {
			if authItems[i].InflightRequests == authItems[j].InflightRequests {
				return authItems[i].AuthID < authItems[j].AuthID
			}
			return authItems[i].InflightRequests > authItems[j].InflightRequests
		}
		return authItems[i].Prefix < authItems[j].Prefix
	})
	payload.Auths = authItems
	return payload
}

func appendIfMissing(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), value) {
			return items
		}
	}
	return append(items, value)
}

func formatRuntimeTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

func parseRuntimeTime(raw string) time.Time {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	return parsed.UTC()
}

func weightedAverageLatency(current float64, currentWeight float64, next float64, nextWeight float64) float64 {
	if nextWeight <= 0 {
		return current
	}
	if currentWeight <= 0 {
		return next
	}
	totalWeight := currentWeight + nextWeight
	if totalWeight <= 0 {
		return 0
	}
	return ((current * currentWeight) + (next * nextWeight)) / totalWeight
}
