package util

import (
	"net/http"
	"strings"
)

// ApplyCustomHeadersFromAttrs applies user-defined headers stored in the provided attributes map.
// Custom headers override built-in defaults when conflicts occur.
func ApplyCustomHeadersFromAttrs(r *http.Request, attrs map[string]string) {
	if r == nil {
		return
	}
	applyCustomHeaders(r, extractCustomHeaders(attrs))
}

// ApplyReservedProviderHeadersFromAttrs applies provider-auth headers that are intentionally
// excluded from the generic custom-header path.
func ApplyReservedProviderHeadersFromAttrs(r *http.Request, attrs map[string]string) {
	if r == nil {
		return
	}
	applyCustomHeaders(r, extractReservedProviderHeaders(attrs))
}

// HeaderValueFromAttrsCI returns the first matching configured header value from attrs.
func HeaderValueFromAttrsCI(attrs map[string]string, names ...string) string {
	if len(attrs) == 0 || len(names) == 0 {
		return ""
	}
	targets := make(map[string]struct{}, len(names))
	for _, name := range names {
		normalized := normalizeHeaderLookupName(name)
		if normalized == "" {
			continue
		}
		targets[normalized] = struct{}{}
	}
	if len(targets) == 0 {
		return ""
	}
	for key, value := range attrs {
		if !strings.HasPrefix(key, "header:") {
			continue
		}
		if _, ok := targets[normalizeHeaderLookupName(strings.TrimPrefix(key, "header:"))]; !ok {
			continue
		}
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func extractCustomHeaders(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	headers := make(map[string]string)
	for k, v := range attrs {
		if !strings.HasPrefix(k, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(k, "header:"))
		if name == "" {
			continue
		}
		if isReservedInternalHeader(name) {
			continue
		}
		val := strings.TrimSpace(v)
		if val == "" {
			continue
		}
		headers[name] = val
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func extractReservedProviderHeaders(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	headers := make(map[string]string)
	for k, v := range attrs {
		if !strings.HasPrefix(k, "header:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(k, "header:"))
		if name == "" || !isReservedInternalHeader(name) {
			continue
		}
		val := strings.TrimSpace(v)
		if val == "" {
			continue
		}
		headers[name] = val
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func isReservedInternalHeader(name string) bool {
	switch normalizeHeaderLookupName(name) {
	case "x-newapi-username",
		"x-new-api-username",
		"x-newapi-password",
		"x-new-api-password",
		"x-newapi-token-name",
		"x-new-api-token-name",
		"x-newapi-access-token",
		"x-new-api-access-token",
		"x-newapi-user-id",
		"x-new-api-user-id":
		return true
	default:
		return false
	}
}

func normalizeHeaderLookupName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func applyCustomHeaders(r *http.Request, headers map[string]string) {
	if r == nil || len(headers) == 0 {
		return
	}
	for k, v := range headers {
		if k == "" || v == "" {
			continue
		}
		r.Header.Set(k, v)
	}
}
