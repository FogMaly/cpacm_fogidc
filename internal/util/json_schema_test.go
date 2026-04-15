package util

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeToolSchemaJSON_FixesNullItems(t *testing.T) {
	raw := `{
		"type": "object",
		"properties": {
			"items": {
				"type": "array",
				"items": null
			}
		},
		"required": ["items", "missing"]
	}`

	got := SanitizeToolSchemaJSON(raw)
	if gjson.Get(got, "properties.items.items.type").String() != "object" {
		t.Fatalf("items.type = %q, want object", gjson.Get(got, "properties.items.items.type").String())
	}
	if !gjson.Get(got, "properties.items.items.properties").IsObject() {
		t.Fatal("items.properties missing after sanitize")
	}
	if gotRequired := len(gjson.Get(got, "required").Array()); gotRequired != 1 {
		t.Fatalf("required length = %d, want 1", gotRequired)
	}
	if gjson.Get(got, "required.0").String() != "items" {
		t.Fatalf("required.0 = %q, want items", gjson.Get(got, "required.0").String())
	}
}

func TestSanitizeToolSchemaJSON_DefaultsInvalidSchema(t *testing.T) {
	got := SanitizeToolSchemaJSON(`null`)
	if gjson.Get(got, "type").String() != "object" {
		t.Fatalf("type = %q, want object", gjson.Get(got, "type").String())
	}
	if !gjson.Get(got, "properties").IsObject() {
		t.Fatal("properties missing")
	}
	if !gjson.Get(got, "required").IsArray() {
		t.Fatal("required missing")
	}
}

func TestSanitizeToolSchemaJSON_AddsEmptyRequiredArray(t *testing.T) {
	got := SanitizeToolSchemaJSON(`{"type":"object","properties":{"message":{"type":"string"}}}`)
	if !gjson.Get(got, "required").IsArray() {
		t.Fatal("required missing")
	}
	if len(gjson.Get(got, "required").Array()) != 0 {
		t.Fatalf("required length = %d, want 0", len(gjson.Get(got, "required").Array()))
	}
}

func TestSanitizeToolSchemaJSON_NormalizesNullRequiredArray(t *testing.T) {
	got := SanitizeToolSchemaJSON(`{"type":"object","properties":{"plan":{"type":"array","items":null}},"required":null}`)
	if !gjson.Get(got, "required").IsArray() {
		t.Fatal("required should be an array")
	}
	if len(gjson.Get(got, "required").Array()) != 0 {
		t.Fatalf("required length = %d, want 0", len(gjson.Get(got, "required").Array()))
	}
}
