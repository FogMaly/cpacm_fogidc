package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
)

type runtimeOutageState struct {
	FailedAt time.Time
	Reason   string
}

func (m *Manager) updateRuntimeOutage(result Result) {
	if m == nil || result.AuthID == "" || !shouldRecordRuntimeOutage(result) {
		return
	}
	modelKey := runtimeOutageModelKey(result.Model)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.runtimeOutages == nil {
		m.runtimeOutages = make(map[string]map[string]runtimeOutageState)
	}
	byModel := m.runtimeOutages[result.AuthID]
	if byModel == nil {
		byModel = make(map[string]runtimeOutageState)
		m.runtimeOutages[result.AuthID] = byModel
	}
	state := runtimeOutageState{
		FailedAt: time.Now().UTC(),
		Reason:   runtimeOutageReason(result),
	}
	if existing, ok := byModel[modelKey]; ok && existing.FailedAt.After(state.FailedAt) {
		return
	}
	byModel[modelKey] = state
}

func (m *Manager) runtimeOutageSignalForAuth(auth *Auth, model string, probeUpdatedAt time.Time) health.RuntimeOutageSignal {
	if m == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return health.RuntimeOutageSignal{}
	}

	m.mu.RLock()
	byModel := m.runtimeOutages[auth.ID]
	if len(byModel) == 0 {
		m.mu.RUnlock()
		return health.RuntimeOutageSignal{}
	}
	modelState, modelOK := byModel[runtimeOutageModelKey(model)]
	authState, authOK := byModel[""]
	m.mu.RUnlock()

	best := health.RuntimeOutageSignal{}
	if authOK {
		best = buildRuntimeOutageSignal(authState, probeUpdatedAt)
	}
	if modelOK {
		candidate := buildRuntimeOutageSignal(modelState, probeUpdatedAt)
		if !best.Found || candidate.Active || candidate.FailedAt.After(best.FailedAt) {
			best = candidate
		}
	}
	return best
}

func buildRuntimeOutageSignal(state runtimeOutageState, probeUpdatedAt time.Time) health.RuntimeOutageSignal {
	if state.FailedAt.IsZero() && strings.TrimSpace(state.Reason) == "" {
		return health.RuntimeOutageSignal{}
	}
	return health.RuntimeOutageSignal{
		Active:   state.FailedAt.IsZero() || probeUpdatedAt.IsZero() || !probeUpdatedAt.After(state.FailedAt),
		FailedAt: state.FailedAt,
		Reason:   strings.TrimSpace(state.Reason),
		Found:    true,
	}
}

func (m *Manager) RuntimeOutageSnapshot() []health.RuntimeOutageSnapshotItem {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]health.RuntimeOutageSnapshotItem, 0, len(m.runtimeOutages))
	for authID, byModel := range m.runtimeOutages {
		if len(byModel) == 0 {
			continue
		}
		auth := m.auths[authID]
		if auth == nil {
			continue
		}
		baseURL := ""
		if auth.Attributes != nil {
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		}
		for modelKey, state := range byModel {
			if state.FailedAt.IsZero() && strings.TrimSpace(state.Reason) == "" {
				continue
			}
			out = append(out, health.RuntimeOutageSnapshotItem{
				Prefix:   strings.TrimSpace(auth.Prefix),
				BaseURL:  baseURL,
				Provider: strings.TrimSpace(auth.Provider),
				Label:    strings.TrimSpace(auth.Label),
				Model:    modelKey,
				FailedAt: state.FailedAt,
				Reason:   strings.TrimSpace(state.Reason),
			})
		}
	}
	return out
}

func runtimeOutageModelKey(model string) string {
	return health.CanonicalModel(model)
}

func shouldRecordRuntimeOutage(result Result) bool {
	if strings.TrimSpace(result.RuntimeOutageReason) != "" {
		return true
	}
	if result.Success {
		return false
	}
	if result.Error == nil {
		return true
	}
	statusCode := statusCodeFromResult(result.Error)
	switch statusCode {
	case 0:
		return !strings.Contains(strings.ToLower(strings.TrimSpace(result.Error.Message)), "invalid_request_error")
	case http.StatusBadRequest:
		return false
	case http.StatusTooManyRequests:
		return false
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusConflict, http.StatusLocked, http.StatusTooEarly,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return statusCode >= 500
	}
}

func runtimeOutageReason(result Result) string {
	if reason := strings.TrimSpace(result.RuntimeOutageReason); reason != "" {
		return reason
	}
	if result.Error == nil {
		return "request_failed"
	}
	statusCode := statusCodeFromResult(result.Error)
	switch statusCode {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired:
		return "payment_required"
	case http.StatusForbidden:
		if isDailyQuotaExceededError(result.Error) {
			return "daily_quota"
		}
		return "forbidden"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusRequestTimeout:
		return "timeout"
	case http.StatusConflict:
		return "conflict"
	case http.StatusLocked:
		return "locked"
	case http.StatusTooEarly:
		return "too_early"
	case http.StatusInternalServerError:
		return "upstream_500"
	case http.StatusBadGateway:
		return "upstream_502"
	case http.StatusServiceUnavailable:
		return "upstream_503"
	case http.StatusGatewayTimeout:
		return "upstream_504"
	case 0:
		msg := strings.TrimSpace(result.Error.Message)
		if msg == "" {
			return "transport_error"
		}
		return msg
	default:
		msg := strings.TrimSpace(result.Error.Message)
		if msg == "" {
			return "request_failed"
		}
		return msg
	}
}
