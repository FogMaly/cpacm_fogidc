package responses

import (
	"strings"

	"github.com/tidwall/gjson"
)

func responsesToolKindByName(originalRequestRawJSON []byte, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "function"
	}
	tools := gjson.GetBytes(originalRequestRawJSON, "tools")
	if !tools.IsArray() {
		return "function"
	}
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("name").String()) != name {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "custom") {
			return "custom"
		}
		return "function"
	}
	return "function"
}

func responsesCustomToolSchema() string {
	return `{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`
}

func responsesWrapCustomToolInput(input string) string {
	inputJSON := gjson.AppendJSONString(nil, input)
	return `{"input":` + string(inputJSON) + `}`
}

func responsesUnwrapCustomToolInput(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if gjson.Valid(trimmed) {
		parsed := gjson.Parse(trimmed)
		if parsed.IsObject() {
			if input := parsed.Get("input"); input.Exists() {
				if input.Type == gjson.String {
					return input.String()
				}
				return input.Raw
			}
		}
	}
	return trimmed
}

func responsesToolOutputToString(output gjson.Result) string {
	if !output.Exists() {
		return ""
	}
	switch {
	case output.Type == gjson.String:
		return output.String()
	case output.IsArray():
		var builder strings.Builder
		for _, part := range output.Array() {
			text := strings.TrimSpace(part.Get("text").String())
			if text == "" && part.Type == gjson.String {
				text = strings.TrimSpace(part.String())
			}
			if text == "" {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(text)
		}
		if builder.Len() > 0 {
			return builder.String()
		}
		return output.Raw
	default:
		return output.String()
	}
}
