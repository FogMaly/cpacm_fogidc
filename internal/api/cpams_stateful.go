package api

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

func cpamsKnownStatefulCodexPrefix(prefix string) bool {
	switch strings.ToLower(strings.TrimSpace(prefix)) {
	case "sub2api", "ee":
		return true
	default:
		return false
	}
}

func cpamsReasonDisablesStatefulCodexResponses(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	return strings.Contains(reason, "responses_continuation_failed") ||
		strings.Contains(reason, "continuation_failed")
}

func cpamsCodexConfigSupportsResponses(protocols string) bool {
	for _, token := range strings.FieldsFunc(strings.ToLower(strings.TrimSpace(protocols)), func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == ' '
	}) {
		switch strings.TrimSpace(token) {
		case "responses", "/responses", "responses/compact", "/responses/compact":
			return true
		}
	}
	return false
}

func cpamsCodexConfigForcesBridge(entry config.CodexKey) bool {
	protocols := strings.ToLower(strings.TrimSpace(entry.SupportedProtocols))
	if protocols == "" {
		return false
	}
	return !cpamsCodexConfigSupportsResponses(protocols)
}

func logCPAMSCodexConfigWarnings(cfg *config.Config) {
	if cfg == nil {
		return
	}
	for _, entry := range cfg.CodexKey {
		if !cpamsCodexConfigSupportsResponses(entry.SupportedProtocols) {
			continue
		}
		if cpamsCodexConfigForcesBridge(entry) {
			continue
		}
		log.Infof("cpams: codex prefix=%q base_url=%q declares responses support; continuation routing now accepts bridge-capable codex providers unless probe explicitly marks continuation failed", strings.TrimSpace(entry.Prefix), strings.TrimSpace(entry.BaseURL))
	}
}
