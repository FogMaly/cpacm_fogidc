package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ConvertOpenAIResponsesRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	inputResult := gjson.GetBytes(rawJSON, "input")
	if inputResult.Type == gjson.String {
		input, _ := sjson.Set(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`, "0.content.0.text", inputResult.String())
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "input", []byte(input))
	}

	rawJSON, _ = sjson.SetBytes(rawJSON, "stream", true)
	if !gjson.GetBytes(rawJSON, "store").Exists() {
		rawJSON, _ = sjson.SetBytes(rawJSON, "store", true)
	}
	rawJSON, _ = sjson.SetBytes(rawJSON, "parallel_tool_calls", true)
	rawJSON, _ = sjson.SetBytes(rawJSON, "include", []string{"reasoning.encrypted_content"})
	// Codex Responses rejects token limit fields, so strip them out before forwarding.
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "max_output_tokens")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "max_completion_tokens")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "temperature")
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "top_p")
	if !gjson.GetBytes(rawJSON, "service_tier").Exists() {
		if gjson.GetBytes(rawJSON, "features.fast_mode").Bool() || gjson.GetBytes(rawJSON, "fast_mode").Bool() {
			rawJSON, _ = sjson.SetBytes(rawJSON, "service_tier", "fast")
		}
	}

	// Delete the user field as it is not supported by the Codex upstream.
	rawJSON, _ = sjson.DeleteBytes(rawJSON, "user")

	// Normalize input messages to Codex-compatible content arrays and roles.
	rawJSON = normalizeCodexResponsesInput(rawJSON)

	return rawJSON
}

// normalizeCodexResponsesInput rewrites OpenAI Responses input items into the stricter
// Codex-compatible shape:
//   - system -> developer
//   - string content -> [{type: input_text/output_text, text: "..."}]
//   - text-like content arrays are normalized and empty text blocks are removed
//   - empty message items are dropped entirely
func normalizeCodexResponsesInput(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}

	out := `[]`
	inputArray := trimCodexResponsesAssistantOnlyTailAfterLastUser(inputResult.Array())
	for i := 0; i < len(inputArray); i++ {
		item := inputArray[i]
		itemType := strings.TrimSpace(item.Get("type").String())
		role := normalizeCodexResponsesRole(item.Get("role").String())
		if itemType == "" && role != "" {
			itemType = "message"
		}

		switch itemType {
		case "message", "":
			msg := normalizeCodexResponsesMessage(item, role)
			if msg == "" {
				continue
			}
			out, _ = sjson.SetRaw(out, "-1", msg)
		case "function_call":
			if codexResponsesFunctionArgumentsMalformed(item) && codexResponsesNextItemIsMalformedFunctionCallOutput(inputArray, i, item.Get("call_id").String()) {
				i++
				continue
			}
			normalized := normalizeCodexResponsesFunctionCall(item)
			if normalized == "" {
				continue
			}
			out, _ = sjson.SetRaw(out, "-1", normalized)
		case "function_call_output":
			out, _ = sjson.SetRaw(out, "-1", item.Raw)
		case "custom_tool_call", "custom_tool_call_output":
			out, _ = sjson.SetRaw(out, "-1", item.Raw)
		default:
			out, _ = sjson.SetRaw(out, "-1", item.Raw)
		}
	}
	result, _ := sjson.SetRawBytes(rawJSON, "input", []byte(out))
	return result
}

func normalizeCodexResponsesRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system":
		return "developer"
	case "developer", "user", "assistant":
		return strings.ToLower(strings.TrimSpace(role))
	default:
		return strings.TrimSpace(role)
	}
}

func normalizeCodexResponsesMessage(item gjson.Result, role string) string {
	if role == "" {
		role = "user"
	}
	msg := `{"type":"message","role":"","content":[]}`
	msg, _ = sjson.Set(msg, "role", role)

	content := item.Get("content")
	if content.Type == gjson.String {
		text := strings.TrimSpace(content.String())
		if text == "" {
			return ""
		}
		part := normalizeCodexResponsesTextPart(role, "", text)
		if part == "" {
			return ""
		}
		msg, _ = sjson.SetRaw(msg, "content.-1", part)
		return msg
	}
	if !content.Exists() || !content.IsArray() {
		return ""
	}

	added := 0
	content.ForEach(func(_, part gjson.Result) bool {
		if normalized := normalizeCodexResponsesContentPart(role, part); normalized != "" {
			msg, _ = sjson.SetRaw(msg, fmt.Sprintf("content.%d", added), normalized)
			added++
		}
		return true
	})
	if added == 0 {
		return ""
	}
	return msg
}

func normalizeCodexResponsesContentPart(role string, part gjson.Result) string {
	partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
	switch partType {
	case "", "text", "input_text", "output_text":
		return normalizeCodexResponsesTextPart(role, partType, part.Get("text").String())
	case "input_image":
		imageURL := strings.TrimSpace(part.Get("image_url").String())
		if imageURL == "" {
			imageURL = strings.TrimSpace(part.Get("image_url.url").String())
		}
		if imageURL == "" {
			return ""
		}
		out := `{"type":"input_image","image_url":""}`
		out, _ = sjson.Set(out, "image_url", imageURL)
		return out
	default:
		// Keep other blocks only when they carry meaningful text.
		return normalizeCodexResponsesTextPart(role, partType, part.Get("text").String())
	}
}

func normalizeCodexResponsesTextPart(role, partType, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if partType == "" || partType == "text" || partType == "input_text" || partType == "output_text" {
		if strings.EqualFold(role, "assistant") {
			partType = "output_text"
		} else {
			partType = "input_text"
		}
	}
	out := `{"type":"","text":""}`
	out, _ = sjson.Set(out, "type", partType)
	out, _ = sjson.Set(out, "text", text)
	return out
}

func normalizeCodexResponsesFunctionCall(item gjson.Result) string {
	callID := strings.TrimSpace(item.Get("call_id").String())
	name := strings.TrimSpace(item.Get("name").String())
	args := normalizeCodexResponsesFunctionArguments(item.Get("arguments").String())

	out := `{"type":"function_call","call_id":"","name":"","arguments":"{}"}`
	if callID != "" {
		out, _ = sjson.Set(out, "call_id", callID)
	}
	if name != "" {
		out, _ = sjson.Set(out, "name", name)
	}
	out, _ = sjson.Set(out, "arguments", args)
	return out
}

func normalizeCodexResponsesFunctionArguments(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	if gjson.Valid(trimmed) {
		parsed := gjson.Parse(trimmed)
		if parsed.IsObject() || parsed.IsArray() {
			return trimmed
		}
	}
	fallback, err := json.Marshal(map[string]string{
		"_raw_arguments": trimmed,
		"_error":         "arguments were not valid JSON",
	})
	if err != nil {
		return `{"_error":"arguments were not valid JSON"}`
	}
	return string(fallback)
}

func codexResponsesFunctionArgumentsMalformed(item gjson.Result) bool {
	arguments := item.Get("arguments")
	if !arguments.Exists() {
		return false
	}
	trimmed := strings.TrimSpace(arguments.String())
	if trimmed == "" {
		return false
	}
	if !gjson.Valid(trimmed) {
		return true
	}
	parsed := gjson.Parse(trimmed)
	return !parsed.IsObject() && !parsed.IsArray()
}

func codexResponsesNextItemIsMalformedFunctionCallOutput(items []gjson.Result, idx int, callID string) bool {
	if idx+1 >= len(items) || strings.TrimSpace(callID) == "" {
		return false
	}
	next := items[idx+1]
	if strings.TrimSpace(next.Get("type").String()) != "function_call_output" {
		return false
	}
	if next.Get("call_id").String() != callID {
		return false
	}
	output := strings.TrimSpace(next.Get("output").String())
	return strings.Contains(strings.ToLower(output), "failed to parse function arguments")
}

func trimCodexResponsesAssistantOnlyTailAfterLastUser(items []gjson.Result) []gjson.Result {
	lastUserIdx := -1
	for i, item := range items {
		itemType := strings.TrimSpace(item.Get("type").String())
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		if itemType == "" && role != "" {
			itemType = "message"
		}
		if itemType == "message" && (role == "user" || role == "developer" || role == "system") {
			lastUserIdx = i
		}
	}
	if lastUserIdx < 0 || lastUserIdx >= len(items)-1 {
		return items
	}
	assistantMessages := 0
	sawProblematicTail := false
	for i := lastUserIdx + 1; i < len(items); i++ {
		item := items[i]
		itemType := strings.TrimSpace(item.Get("type").String())
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		if itemType == "" && role != "" {
			itemType = "message"
		}
		switch itemType {
		case "reasoning":
			sawProblematicTail = true
			continue
		case "message":
			if role == "assistant" {
				assistantMessages++
				continue
			}
			return items
		case "function_call":
			if codexResponsesFunctionArgumentsMalformed(item) && codexResponsesNextItemIsMalformedFunctionCallOutput(items, i, item.Get("call_id").String()) {
				sawProblematicTail = true
				i++
				continue
			}
			return items
		case "custom_tool_call", "custom_tool_call_output":
			return items
		case "function_call_output":
			prevIdx := i - 1
			if prevIdx >= 0 && codexResponsesFunctionArgumentsMalformed(items[prevIdx]) && codexResponsesNextItemIsMalformedFunctionCallOutput(items, prevIdx, items[prevIdx].Get("call_id").String()) {
				sawProblematicTail = true
				continue
			}
			return items
		default:
			return items
		}
	}
	if !sawProblematicTail && assistantMessages <= 1 {
		return items
	}
	return items[:lastUserIdx+1]
}
