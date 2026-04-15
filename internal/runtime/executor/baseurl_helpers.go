package executor

import (
	"net/url"
	"strings"
)

// normalizeOpenAICompatibleBaseURL makes common OpenAI-compatible root URLs usable
// by appending /v1 when the config points at a site root instead of an API root.
func normalizeOpenAICompatibleBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return strings.TrimSuffix(trimmed, "/")
	}

	normalized := strings.TrimSuffix(trimmed, "/")
	path := strings.TrimSpace(parsed.Path)
	switch strings.ToLower(path) {
	case "", "/":
		return normalized + "/v1"
	default:
		return normalized
	}
}

// buildClaudeMessagesEndpoint makes common Claude-compatible base URLs usable
// by producing a concrete /messages endpoint from either a site root, a /v1 root,
// or an already-complete /messages URL.
func buildClaudeMessagesEndpoint(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		normalized := strings.TrimSuffix(trimmed, "/")
		switch {
		case strings.HasSuffix(strings.ToLower(normalized), "/messages"):
			return normalized
		case strings.HasSuffix(strings.ToLower(normalized), "/v1"):
			return normalized + "/messages"
		default:
			return normalized + "/v1/messages"
		}
	}

	normalized := strings.TrimSuffix(trimmed, "/")
	path := strings.ToLower(strings.TrimSpace(parsed.Path))
	switch {
	case path == "" || path == "/":
		return normalized + "/v1/messages"
	case strings.HasSuffix(path, "/messages"):
		return normalized
	case strings.HasSuffix(path, "/v1"):
		return normalized + "/messages"
	default:
		return normalized + "/v1/messages"
	}
}

func buildClaudeMessagesRequestURL(raw string) string {
	endpoint := buildClaudeMessagesEndpoint(raw)
	if endpoint == "" {
		return ""
	}
	if shouldUseClaudeCodeCompatibilityProfile(raw) {
		return endpoint + "?beta=true"
	}
	return endpoint
}

func buildClaudeMessagesCountTokensEndpoint(raw string) string {
	endpoint := buildClaudeMessagesEndpoint(raw)
	if endpoint == "" {
		return ""
	}
	return strings.TrimSuffix(endpoint, "/") + "/count_tokens"
}

func buildClaudeMessagesCountTokensRequestURL(raw string) string {
	endpoint := buildClaudeMessagesCountTokensEndpoint(raw)
	if endpoint == "" {
		return ""
	}
	if shouldUseClaudeCodeCompatibilityProfile(raw) {
		return endpoint + "?beta=true"
	}
	return endpoint
}

func shouldUseClaudeCodeCompatibilityProfile(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Hostname(), "api.anthropic.com")
}

func shouldUseClaudeThirdPartyRelayProfile(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	return !shouldUseClaudeCodeCompatibilityProfile(raw)
}
