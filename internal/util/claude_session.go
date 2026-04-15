package util

import "strings"

const claudeAccountSessionMarker = "account__session_"

// ExtractClaudeAccountSessionID parses Claude CLI style metadata.user_id values:
// user_<hash>_account__session_<uuid>
// and returns the trailing session id when present.
func ExtractClaudeAccountSessionID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	lower := strings.ToLower(raw)
	idx := strings.LastIndex(lower, claudeAccountSessionMarker)
	if idx < 0 {
		return ""
	}

	session := strings.TrimSpace(raw[idx+len(claudeAccountSessionMarker):])
	if session == "" {
		return ""
	}

	end := len(session)
	for i, r := range session {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		end = i
		break
	}
	session = strings.TrimSpace(session[:end])
	if session == "" {
		return ""
	}
	return strings.ToLower(session)
}
