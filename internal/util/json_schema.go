package util

import (
	"encoding/json"
	"strings"
)

var defaultToolSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{},
	"required":   []string{},
}

// SanitizeToolSchemaJSON normalizes a tool JSON schema so downstream providers
// do not reject structurally invalid definitions such as `items: null`.
func SanitizeToolSchemaJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return `{"type":"object","properties":{},"required":[]}`
	}

	var schema any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		return `{"type":"object","properties":{},"required":[]}`
	}

	sanitized := sanitizeToolSchemaNode(schema)
	data, err := json.Marshal(sanitized)
	if err != nil {
		return `{"type":"object","properties":{},"required":[]}`
	}
	return string(data)
}

func sanitizeToolSchemaNode(node any) any {
	switch value := node.(type) {
	case map[string]any:
		if props, ok := value["properties"].(map[string]any); ok && props != nil {
			for key, child := range props {
				props[key] = sanitizeToolSchemaNode(child)
			}
			value["properties"] = props
		}

		for _, key := range []string{"items", "not", "if", "then", "else", "contains"} {
			if child, ok := value[key]; ok {
				value[key] = sanitizeToolSchemaNode(child)
			}
		}

		schemaType, _ := value["type"].(string)
		schemaType = strings.TrimSpace(strings.ToLower(schemaType))
		if schemaType == "" {
			if _, ok := value["properties"]; ok {
				schemaType = "object"
				value["type"] = "object"
			} else if _, ok := value["items"]; ok {
				schemaType = "array"
				value["type"] = "array"
			}
		}

		switch schemaType {
		case "object":
			props, ok := value["properties"].(map[string]any)
			if !ok || props == nil {
				props = map[string]any{}
				value["properties"] = props
			}
			if required, ok := value["required"]; ok {
				filtered := sanitizeRequiredSchemaFields(required, props)
				value["required"] = filtered
			} else {
				value["required"] = []string{}
			}
		case "array":
			items := sanitizeToolSchemaNode(value["items"])
			if items == nil {
				items = cloneDefaultToolSchema()
			}
			value["items"] = items
		}

		for _, key := range []string{"anyOf", "allOf", "oneOf"} {
			if options, ok := value[key].([]any); ok {
				filtered := make([]any, 0, len(options))
				for _, option := range options {
					sanitized := sanitizeToolSchemaNode(option)
					if sanitized != nil {
						filtered = append(filtered, sanitized)
					}
				}
				if len(filtered) == 0 {
					delete(value, key)
				} else {
					value[key] = filtered
				}
			}
		}

		if additional, ok := value["additionalProperties"]; ok {
			switch typed := additional.(type) {
			case map[string]any:
				value["additionalProperties"] = sanitizeToolSchemaNode(typed)
			case nil:
				delete(value, "additionalProperties")
			}
		}

		return value
	case []any:
		for i, child := range value {
			value[i] = sanitizeToolSchemaNode(child)
		}
		return value
	case nil:
		return nil
	default:
		return node
	}
}

func sanitizeRequiredSchemaFields(required any, props map[string]any) []string {
	items, ok := required.([]any)
	if !ok || len(items) == 0 {
		return []string{}
	}
	filtered := make([]string, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			continue
		}
		if _, exists := props[name]; exists {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

func cloneDefaultToolSchema() map[string]any {
	return map[string]any{
		"type":       defaultToolSchema["type"],
		"properties": map[string]any{},
	}
}
