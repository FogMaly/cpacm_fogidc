package auth

import (
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const defaultRequestAffinityTTL = 45 * time.Minute

type requestAffinitySelection struct {
	AuthID    string
	ExpiresAt time.Time
}

type requestAffinityStore struct {
	mu    sync.Mutex
	items map[string]requestAffinitySelection
}

func affinityScopeForProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func affinityScopeForProviders(providers []string) string {
	if len(providers) == 0 {
		return ""
	}
	uniq := make(map[string]struct{}, len(providers))
	order := make([]string, 0, len(providers))
	for _, provider := range providers {
		key := affinityScopeForProvider(provider)
		if key == "" {
			continue
		}
		if _, exists := uniq[key]; exists {
			continue
		}
		uniq[key] = struct{}{}
		order = append(order, key)
	}
	if len(order) == 0 {
		return ""
	}
	sort.Strings(order)
	return "mixed:" + strings.Join(order, ",")
}

func requestAffinityProtocol(opts cliproxyexecutor.Options) string {
	protocol := codexRequestProtocol(opts)
	if protocol != "" {
		return protocol
	}
	source := strings.ToLower(strings.TrimSpace(string(opts.SourceFormat)))
	if source == "" {
		return "unknown"
	}
	return source
}

func extractRequestAffinitySeed(opts cliproxyexecutor.Options) (string, string) {
	if len(opts.OriginalRequest) > 0 {
		for _, path := range []string{
			"prompt_cache_key",
			"previous_response_id",
			"conversation_id",
			"session_id",
			"metadata.conversation_id",
			"metadata.session_id",
		} {
			value := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, path).String())
			if value != "" {
				return path, value
			}
		}
		for _, path := range []string{"metadata.user_id", "metadata.userID"} {
			if sessionID := util.ExtractClaudeAccountSessionID(gjson.GetBytes(opts.OriginalRequest, path).String()); sessionID != "" {
				return path + ".account__session", sessionID
			}
		}
	}
	if opts.Headers != nil {
		for _, name := range []string{
			"Conversation_id",
			"Session_id",
			"Conversation-Id",
			"Session-Id",
		} {
			value := strings.TrimSpace(opts.Headers.Get(name))
			if value != "" {
				return "header:" + strings.ToLower(name), value
			}
		}
	}
	if opts.Metadata != nil {
		if value, ok := opts.Metadata[cliproxyexecutor.SessionAffinitySeedMetadataKey]; ok {
			if seed := strings.TrimSpace(stringValueAny(value)); seed != "" {
				return "metadata:" + cliproxyexecutor.SessionAffinitySeedMetadataKey, seed
			}
		}
	}
	return "", ""
}

func stringValueAny(v any) string {
	switch typed := v.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	case interface{ String() string }:
		return typed.String()
	default:
		return ""
	}
}

func buildRequestAffinityKey(scope, model string, opts cliproxyexecutor.Options) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return ""
	}
	seedSource, seedValue := extractRequestAffinitySeed(opts)
	if seedValue == "" {
		return ""
	}
	modelKey := strings.ToLower(strings.TrimSpace(canonicalModelKey(model)))
	if modelKey == "" {
		modelKey = strings.ToLower(strings.TrimSpace(model))
	}
	protocol := requestAffinityProtocol(opts)
	return strings.Join([]string{
		"request-affinity",
		scope,
		modelKey,
		protocol,
		seedSource,
		stableAuthIndex(seedValue),
	}, "|")
}

func (m *Manager) pickCandidateByAffinity(scope, model string, opts cliproxyexecutor.Options, candidates []*Auth) (*Auth, bool) {
	if m == nil || len(candidates) == 0 {
		return nil, false
	}
	if responseID := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String()); responseID != "" {
		now := time.Now()
		if selection, ok := responseAffinityGet(responseID, now); ok {
			for _, candidate := range candidates {
				if candidate == nil || candidate.ID != selection.AuthID {
					continue
				}
				if blocked, _, _ := isAuthBlockedForModel(candidate, model, now); blocked {
					responseAffinityDelete(responseID, candidate.ID)
					return nil, false
				}
				if m.shouldMigrateStickyAffinity(candidate, candidates, now) {
					responseAffinityDelete(responseID, candidate.ID)
					return nil, false
				}
				return candidate, true
			}
			responseAffinityDelete(responseID, selection.AuthID)
		}
	}
	key := buildRequestAffinityKey(scope, model, opts)
	if key == "" {
		return nil, false
	}
	now := time.Now()
	selection, ok := m.requestAffinityGet(key, now)
	if !ok {
		return nil, false
	}
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID != selection.AuthID {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(candidate, model, now); blocked {
			m.requestAffinityDelete(key, candidate.ID)
			return nil, false
		}
		if m.shouldMigrateStickyAffinity(candidate, candidates, now) {
			m.requestAffinityDelete(key, candidate.ID)
			return nil, false
		}
		return candidate, true
	}
	m.requestAffinityDelete(key, selection.AuthID)
	return nil, false
}

func (m *Manager) rememberRequestAffinity(scope, model string, opts cliproxyexecutor.Options, auth *Auth) {
	if m == nil || auth == nil {
		return
	}
	key := buildRequestAffinityKey(scope, model, opts)
	if key == "" {
		return
	}
	settings := m.schedulerSettings()
	ttl := settings.StickyTTL
	if ttl <= 0 {
		ttl = defaultRequestAffinityTTL
	}
	m.requestAffinitySet(key, auth.ID, time.Now().Add(ttl).UTC())
}

func (m *Manager) clearRequestAffinity(scope, model string, opts cliproxyexecutor.Options, authID string) {
	if m == nil {
		return
	}
	key := buildRequestAffinityKey(scope, model, opts)
	if key == "" {
		return
	}
	m.requestAffinityDelete(key, authID)
}

func (m *Manager) requestAffinityGet(key string, now time.Time) (requestAffinitySelection, bool) {
	if m == nil || strings.TrimSpace(key) == "" {
		return requestAffinitySelection{}, false
	}
	m.requestAffinity.mu.Lock()
	defer m.requestAffinity.mu.Unlock()
	if m.requestAffinity.items == nil {
		return requestAffinitySelection{}, false
	}
	selection, ok := m.requestAffinity.items[key]
	if !ok {
		return requestAffinitySelection{}, false
	}
	if !selection.ExpiresAt.IsZero() && !selection.ExpiresAt.After(now) {
		delete(m.requestAffinity.items, key)
		return requestAffinitySelection{}, false
	}
	return selection, true
}

func (m *Manager) requestAffinitySet(key, authID string, expiresAt time.Time) {
	if m == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(authID) == "" {
		return
	}
	m.requestAffinity.mu.Lock()
	defer m.requestAffinity.mu.Unlock()
	if m.requestAffinity.items == nil {
		m.requestAffinity.items = make(map[string]requestAffinitySelection)
	}
	if len(m.requestAffinity.items) >= 8192 {
		now := time.Now()
		for existingKey, selection := range m.requestAffinity.items {
			if !selection.ExpiresAt.IsZero() && !selection.ExpiresAt.After(now) {
				delete(m.requestAffinity.items, existingKey)
			}
		}
	}
	m.requestAffinity.items[key] = requestAffinitySelection{
		AuthID:    authID,
		ExpiresAt: expiresAt,
	}
}

func (m *Manager) requestAffinityDelete(key, authID string) {
	if m == nil || strings.TrimSpace(key) == "" {
		return
	}
	m.requestAffinity.mu.Lock()
	defer m.requestAffinity.mu.Unlock()
	if m.requestAffinity.items == nil {
		return
	}
	selection, ok := m.requestAffinity.items[key]
	if !ok {
		return
	}
	if authID != "" && selection.AuthID != authID {
		return
	}
	delete(m.requestAffinity.items, key)
}

func (m *Manager) shouldMigrateStickyAffinity(target *Auth, candidates []*Auth, now time.Time) bool {
	if m == nil || target == nil || len(candidates) <= 1 {
		return false
	}
	settings := m.schedulerSettings()
	ratio := m.authLoadRatio(target, now)
	switch {
	case ratio < settings.SoftOverloadThreshold:
		return false
	case ratio >= settings.HardOverloadThreshold:
		return true
	}
	bestAltRatio := ratio
	foundAlt := false
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == target.ID {
			continue
		}
		altRatio := m.authLoadRatio(candidate, now)
		if !foundAlt || altRatio < bestAltRatio {
			bestAltRatio = altRatio
			foundAlt = true
		}
	}
	if !foundAlt || bestAltRatio >= ratio {
		return false
	}
	prob := stickyMigrationProbability(ratio, settings)
	return prob > 0 && rand.Float64() < prob
}

func stickyMigrationProbability(ratio float64, settings schedulerSettings) float64 {
	switch {
	case ratio < settings.SoftOverloadThreshold:
		return 0
	case ratio < 0.85:
		return 0.15
	case ratio < settings.HardOverloadThreshold:
		return 0.35
	default:
		return 0.70
	}
}
