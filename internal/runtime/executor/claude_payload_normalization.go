package executor

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeClaudeMessagesPayload(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	normalized := body
	changed := false

	if system := gjson.GetBytes(normalized, "system"); system.Exists() {
		if raw, keep, ok := normalizeClaudeStringOrBlocks(system); ok {
			changed = true
			if keep {
				normalized, _ = sjson.SetRawBytes(normalized, "system", []byte(raw))
			} else {
				normalized, _ = sjson.DeleteBytes(normalized, "system")
			}
		}
	}

	if messages := gjson.GetBytes(normalized, "messages"); messages.Exists() && messages.IsArray() {
		out := "[]"
		messages.ForEach(func(_, message gjson.Result) bool {
			raw, keep, ok := normalizeClaudeMessage(message)
			if !keep || ok {
				changed = true
			}
			if keep {
				out, _ = sjson.SetRaw(out, "-1", raw)
			}
			return true
		})
		if changed {
			normalized, _ = sjson.SetRawBytes(normalized, "messages", []byte(out))
		}
	}

	if !changed {
		return body
	}
	return normalized
}

func normalizeClaudeMessage(message gjson.Result) (string, bool, bool) {
	content := message.Get("content")
	if !content.Exists() {
		return message.Raw, true, false
	}

	raw, keep, changed := normalizeClaudeStringOrBlocks(content)
	if !keep {
		return "", false, true
	}
	if !changed {
		return message.Raw, true, false
	}

	updated, _ := sjson.SetRaw(message.Raw, "content", raw)
	return updated, true, true
}

func normalizeClaudeStringOrBlocks(content gjson.Result) (string, bool, bool) {
	switch {
	case content.Type == gjson.String:
		if strings.TrimSpace(content.String()) == "" {
			return "", false, true
		}
		return content.Raw, true, false
	case content.IsArray():
		parts := content.Array()
		out := "[]"
		changed := false
		kept := 0
		for i := range parts {
			partRaw, keep, partChanged := normalizeClaudeContentBlock(parts[i])
			if partChanged || !keep {
				changed = true
			}
			if keep {
				out, _ = sjson.SetRaw(out, "-1", partRaw)
				kept++
			}
		}
		if kept == 0 {
			return "", false, true
		}
		if !changed {
			return content.Raw, true, false
		}
		return out, true, true
	default:
		return content.Raw, true, false
	}
}

func normalizeClaudeContentBlock(part gjson.Result) (string, bool, bool) {
	if part.Get("type").String() == "text" && strings.TrimSpace(part.Get("text").String()) == "" {
		return "", false, true
	}
	return part.Raw, true, false
}
