package auth

import (
	"sort"
	"strings"
	"time"
)

type RoutingStateOwnership struct {
	Level       string   `json:"level"`
	States      []string `json:"states"`
	OwnedBy     string   `json:"owned_by"`
	UpdatedBy   []string `json:"updated_by"`
	ConsumedBy  []string `json:"consumed_by"`
	Description string   `json:"description,omitempty"`
}

type RoutingProviderStatus struct {
	Provider          string `json:"provider"`
	TotalAuths        int    `json:"total_auths"`
	AvailableAuths    int    `json:"available_auths"`
	CooldownAuths     int    `json:"cooldown_auths"`
	DisabledAuths     int    `json:"disabled_auths"`
	UnauthorizedAuths int    `json:"unauthorized_auths"`
}

type RoutingModelStatus struct {
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	TotalAuths         int    `json:"total_auths"`
	AvailableAuths     int    `json:"available_auths"`
	CooldownAuths      int    `json:"cooldown_auths"`
	QuotaExceededAuths int    `json:"quota_exceeded_auths"`
	SuspendedAuths     int    `json:"suspended_auths"`
}

type RoutingSnapshot struct {
	GeneratedAt          string                  `json:"generated_at,omitempty"`
	Engine               string                  `json:"engine,omitempty"`
	Strategy             string                  `json:"strategy,omitempty"`
	ProviderOffsets      map[string]int          `json:"provider_offsets,omitempty"`
	ProviderConcurrency  map[string]int          `json:"provider_concurrency,omitempty"`
	Providers            []RoutingProviderStatus `json:"providers,omitempty"`
	Models               []RoutingModelStatus    `json:"models,omitempty"`
	RecentEvents         []RoutingEvent          `json:"recent_events,omitempty"`
	ErrorActions         []RoutingErrorAction    `json:"error_actions,omitempty"`
	StateOwnershipMatrix []RoutingStateOwnership `json:"state_ownership_matrix,omitempty"`
}

func DefaultRoutingStateOwnershipMatrix() []RoutingStateOwnership {
	return []RoutingStateOwnership{
		{
			Level:       "auth",
			States:      []string{"disabled", "unauthorized", "cooldown", "quota"},
			OwnedBy:     "Auth",
			UpdatedBy:   []string{"MarkResult", "applyAuthFailureState", "management update"},
			ConsumedBy:  []string{"pickNextAuthWithinProvider", "collectAuthCandidates", "closestCooldownWait"},
			Description: "Auth-wide states that affect every model under a credential or project.",
		},
		{
			Level:       "model_on_auth",
			States:      []string{"unavailable", "quota_exceeded", "suspended", "next_retry_after"},
			OwnedBy:     "Auth.ModelStates[model]",
			UpdatedBy:   []string{"MarkResult", "resetModelState"},
			ConsumedBy:  []string{"isAuthBlockedForModel", "collectAuthCandidates", "registry synchronization"},
			Description: "Per-model states scoped to a single auth entry.",
		},
		{
			Level:       "provider_model",
			States:      []string{"provider_offset", "blocked_in_attempt", "recent_failure_trace"},
			OwnedBy:     "RoutingAttemptContext / Manager.providerOffsets",
			UpdatedBy:   []string{"pickNextProvider", "applyRoutingFailure", "advancePrimaryProviderOffset"},
			ConsumedBy:  []string{"pickNextProvider", "pickNextRouteFromPlan", "RecentRoutingEvents"},
			Description: "Routing-layer states scoped to provider/model selection rather than a persisted auth record.",
		},
	}
}

func (m *Manager) RoutingSnapshot(eventLimit int) RoutingSnapshot {
	snapshot := RoutingSnapshot{
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
		Engine:               m.routingEngine(),
		Strategy:             m.routingStrategy(),
		ProviderOffsets:      make(map[string]int),
		ProviderConcurrency:  make(map[string]int),
		ErrorActions:         RoutingErrorActionCatalog(),
		StateOwnershipMatrix: DefaultRoutingStateOwnershipMatrix(),
	}
	if m == nil {
		return snapshot
	}

	type providerAgg struct {
		RoutingProviderStatus
	}
	type modelAgg struct {
		RoutingModelStatus
	}

	now := time.Now()
	providers := make(map[string]*providerAgg)
	models := make(map[string]*modelAgg)

	m.mu.RLock()
	for key, value := range m.providerOffsets {
		snapshot.ProviderOffsets[key] = value
	}
	for key, value := range m.providerConcurrencyLimits {
		snapshot.ProviderConcurrency[key] = value
	}
	auths := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		auths = append(auths, auth.Clone())
	}
	m.mu.RUnlock()

	for _, auth := range auths {
		if auth == nil {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider == "" {
			provider = "unknown"
		}
		if _, ok := providers[provider]; !ok {
			providers[provider] = &providerAgg{
				RoutingProviderStatus: RoutingProviderStatus{Provider: provider},
			}
		}
		agg := providers[provider]
		agg.TotalAuths++
		authHasModelCooldown := false
		authHasUnauthorized := auth.StatusMessage == "unauthorized"
		for model, state := range auth.ModelStates {
			if state == nil {
				continue
			}
			blocked, reason, _ := isAuthBlockedForModel(auth, model, now)
			if blocked && reason == blockReasonCooldown {
				authHasModelCooldown = true
			}
		}
		switch {
		case auth.Disabled || auth.Status == StatusDisabled:
			agg.DisabledAuths++
		case authHasUnauthorized:
			agg.UnauthorizedAuths++
		case authHasModelCooldown:
			agg.CooldownAuths++
		default:
			blocked, _, _ := isAuthBlockedForModel(auth, "", now)
			if !blocked {
				agg.AvailableAuths++
			}
		}

		for model, state := range auth.ModelStates {
			if state == nil {
				continue
			}
			key := provider + "|" + canonicalModelKey(model)
			if _, ok := models[key]; !ok {
				models[key] = &modelAgg{
					RoutingModelStatus: RoutingModelStatus{
						Provider: provider,
						Model:    canonicalModelKey(model),
					},
				}
			}
			modelAgg := models[key]
			modelAgg.TotalAuths++
			blocked, reason, _ := isAuthBlockedForModel(auth, model, now)
			if !blocked {
				modelAgg.AvailableAuths++
			}
			if blocked && reason == blockReasonCooldown {
				modelAgg.CooldownAuths++
			}
			if state.Quota.Exceeded {
				modelAgg.QuotaExceededAuths++
			}
			if state.Status == StatusDisabled {
				modelAgg.SuspendedAuths++
			}
		}
	}

	snapshot.Providers = make([]RoutingProviderStatus, 0, len(providers))
	for _, provider := range providers {
		snapshot.Providers = append(snapshot.Providers, provider.RoutingProviderStatus)
	}
	sort.Slice(snapshot.Providers, func(i, j int) bool {
		return snapshot.Providers[i].Provider < snapshot.Providers[j].Provider
	})

	snapshot.Models = make([]RoutingModelStatus, 0, len(models))
	for _, model := range models {
		snapshot.Models = append(snapshot.Models, model.RoutingModelStatus)
	}
	sort.Slice(snapshot.Models, func(i, j int) bool {
		if snapshot.Models[i].Provider == snapshot.Models[j].Provider {
			return snapshot.Models[i].Model < snapshot.Models[j].Model
		}
		return snapshot.Models[i].Provider < snapshot.Models[j].Provider
	})

	snapshot.RecentEvents = m.RecentRoutingEvents(eventLimit)
	return snapshot
}
