package util

import (
	"net"
	"net/http"
	"strings"
)

var forwardedIPHeaders = []string{
	"CF-Connecting-IP",
	"True-Client-IP",
	"X-Original-Forwarded-For",
	"X-Forwarded-For",
	"Forwarded",
	"X-Real-IP",
	"X-Client-IP",
	"X-Original-Client-IP",
}

// ResolveClientIP returns the best-effort real client IP for a proxied request.
// It prefers explicit forwarding headers and falls back to the TCP remote address.
func ResolveClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}

	for _, header := range forwardedIPHeaders {
		ip := resolveClientIPFromHeader(header, r.Header.Values(header))
		if ip != "" {
			return ip
		}
	}

	return normalizeSingleIPToken(r.RemoteAddr)
}

func resolveClientIPFromHeader(header string, values []string) string {
	if len(values) == 0 {
		return ""
	}

	candidates := make([]string, 0, len(values))
	for _, value := range values {
		candidates = append(candidates, extractHeaderIPCandidates(header, value)...)
	}

	return selectBestClientIP(header, candidates)
}

func extractHeaderIPCandidates(header, value string) []string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil
	}

	if strings.EqualFold(header, "Forwarded") {
		return extractForwardedHeaderIPs(raw)
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if ip := normalizeSingleIPToken(part); ip != "" {
			out = append(out, ip)
		}
	}
	return out
}

func extractForwardedHeaderIPs(raw string) []string {
	entries := strings.Split(raw, ",")
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		params := strings.Split(entry, ";")
		for _, param := range params {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "for") {
				continue
			}
			if ip := normalizeSingleIPToken(value); ip != "" {
				out = append(out, ip)
			}
		}
	}
	return out
}

// selectBestClientIP returns the best client IP from a list of candidates.
// For chain headers (X-Forwarded-For, X-Original-Forwarded-For), the real client
// is at the RIGHT (last position) since proxies append to the right.
// For single-value headers, the value IS the direct client.
func selectBestClientIP(header string, candidates []string) string {
	chainHeader := false
	switch strings.ToLower(header) {
	case "x-forwarded-for", "x-original-forwarded-for":
		chainHeader = true
	}

	if chainHeader {
		// For chain headers, scan from right to left.
		// X-Forwarded-For: <client>, <proxy1>, <proxy2> → last is most recent / closest to server
		// Actually, the convention is client-first: "client, proxy1, proxy2"
		// But real-world CDNs/proxies may prepend instead of append.
		// So we search for the FIRST public IP from the right (most recent).
		// But the safest heuristic: prefer the last valid IP in the chain,
		// because well-behaved proxies append. If a CDN prepends, the last
		// is still closer to the real client than the first (which would be the CDN).
		for i := len(candidates) - 1; i >= 0; i-- {
			ip := normalizeSingleIPToken(candidates[i])
			if ip == "" {
				continue
			}
			if parsed := net.ParseIP(ip); isPublicClientIP(parsed) {
				return ip
			}
		}
		// Fallback: left-to-right for non-public candidates
		for _, candidate := range candidates {
			ip := normalizeSingleIPToken(candidate)
			if ip != "" {
				return ip
			}
		}
		return ""
	}

	// For single-value headers (CF-Connecting-IP, X-Real-IP, Forwarded, etc.)
	// the first valid IP is the direct client.
	firstValid := ""
	for _, candidate := range candidates {
		ip := normalizeSingleIPToken(candidate)
		if ip == "" {
			continue
		}
		if firstValid == "" {
			firstValid = ip
		}
		if parsed := net.ParseIP(ip); isPublicClientIP(parsed) {
			return ip
		}
	}
	return firstValid
}

func normalizeSingleIPToken(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.Trim(value, "\"")
	if value == "" || strings.EqualFold(value, "unknown") {
		return ""
	}

	if strings.HasPrefix(value, "[") {
		if end := strings.Index(value, "]"); end > 1 {
			value = value[1:end]
		}
	}

	if parsed := net.ParseIP(value); parsed != nil {
		return parsed.String()
	}

	if host, _, err := net.SplitHostPort(value); err == nil {
		if parsed := net.ParseIP(strings.TrimSpace(host)); parsed != nil {
			return parsed.String()
		}
	}

	if idx := strings.LastIndex(value, ":"); idx > 0 && !strings.Contains(value[:idx], ":") {
		if parsed := net.ParseIP(strings.TrimSpace(value[:idx])); parsed != nil {
			return parsed.String()
		}
	}

	return ""
}

func isPublicClientIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	return true
}
