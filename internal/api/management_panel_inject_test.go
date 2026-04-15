package api

import (
	"bytes"
	"testing"
)

func TestInjectManagementEnhancementSnippets_AddsModelHealthScript(t *testing.T) {
	raw := []byte("<html><body><div id=\"root\"></div></body></html>")

	got := injectManagementEnhancementSnippets(raw)

	if !bytes.Contains(got, []byte(`id="cpapi-model-health-live"`)) {
		t.Fatalf("model health injection marker missing: %s", string(got))
	}
}

func TestInjectManagementEnhancementSnippets_IsIdempotent(t *testing.T) {
	raw := []byte("<html><body><div id=\"root\"></div></body></html>")

	once := injectManagementEnhancementSnippets(raw)
	twice := injectManagementEnhancementSnippets(once)

	if !bytes.Equal(once, twice) {
		t.Fatalf("injection should be idempotent")
	}
}

func TestInjectManagementEnhancementSnippets_ReplacesLegacyVersionedScript(t *testing.T) {
	raw := []byte("<html><body><script id=\"cpapi-model-health-live-v3\">window.legacy=1;</script><div id=\"root\"></div></body></html>")

	got := injectManagementEnhancementSnippets(raw)

	if bytes.Contains(got, []byte(`cpapi-model-health-live-v3`)) {
		t.Fatalf("legacy injection marker should be removed: %s", string(got))
	}
	if bytes.Count(got, []byte(`id="cpapi-model-health-live"`)) != 1 {
		t.Fatalf("expected a single stable injection marker: %s", string(got))
	}
}

func TestInjectManagementEnhancementSnippets_SkipsWhenNativePanelMarkerExists(t *testing.T) {
	raw := []byte("<html><body><script id=\"cpapi-model-health-live-v3\">window.legacy=1;</script><div id=\"root\" data-cpapi-native-ai-providers=\"1\"></div></body></html>")

	got := injectManagementEnhancementSnippets(raw)

	if bytes.Contains(got, []byte(`id="cpapi-model-health-live"`)) {
		t.Fatalf("native panel should skip legacy enhancement injection: %s", string(got))
	}
	if bytes.Contains(got, []byte(`cpapi-model-health-live-v3`)) {
		t.Fatalf("legacy injection marker should be removed even for native panel: %s", string(got))
	}
}
