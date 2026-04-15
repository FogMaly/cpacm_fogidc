package auth

import (
	"strings"
	"sync"
	"time"
)

type responseAffinitySelection struct {
	AuthID    string
	ExpiresAt time.Time
}

var (
	responseAffinityMu    sync.Mutex
	responseAffinityItems = make(map[string]responseAffinitySelection)
)

func normalizeResponseAffinityKey(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ""
	}
	return strings.ToLower(responseID)
}

func responseAffinityGet(responseID string, now time.Time) (responseAffinitySelection, bool) {
	key := normalizeResponseAffinityKey(responseID)
	if key == "" {
		return responseAffinitySelection{}, false
	}
	responseAffinityMu.Lock()
	defer responseAffinityMu.Unlock()
	selection, ok := responseAffinityItems[key]
	if !ok {
		return responseAffinitySelection{}, false
	}
	if !selection.ExpiresAt.IsZero() && !selection.ExpiresAt.After(now) {
		delete(responseAffinityItems, key)
		return responseAffinitySelection{}, false
	}
	return selection, true
}

func responseAffinityDelete(responseID, authID string) {
	key := normalizeResponseAffinityKey(responseID)
	if key == "" {
		return
	}
	responseAffinityMu.Lock()
	defer responseAffinityMu.Unlock()
	selection, ok := responseAffinityItems[key]
	if !ok {
		return
	}
	if authID != "" && selection.AuthID != authID {
		return
	}
	delete(responseAffinityItems, key)
}

func rememberResponseAffinity(responseIDs []string, authID string, ttl time.Duration) {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(responseIDs) == 0 {
		return
	}
	if ttl <= 0 {
		ttl = defaultRequestAffinityTTL
	}
	expiresAt := time.Now().Add(ttl).UTC()
	responseAffinityMu.Lock()
	defer responseAffinityMu.Unlock()
	if len(responseAffinityItems) >= 8192 {
		now := time.Now()
		for key, selection := range responseAffinityItems {
			if !selection.ExpiresAt.IsZero() && !selection.ExpiresAt.After(now) {
				delete(responseAffinityItems, key)
			}
		}
	}
	for _, responseID := range responseIDs {
		key := normalizeResponseAffinityKey(responseID)
		if key == "" {
			continue
		}
		responseAffinityItems[key] = responseAffinitySelection{
			AuthID:    authID,
			ExpiresAt: expiresAt,
		}
	}
}

// RememberResponseAffinityIDs binds synthesized/native response IDs back to the
// auth that produced them so previous_response_id can keep using the same auth.
func RememberResponseAffinityIDs(responseIDs []string, authID string) {
	rememberResponseAffinity(responseIDs, authID, defaultRequestAffinityTTL)
}
