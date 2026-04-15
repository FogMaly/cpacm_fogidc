package api

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"regexp"
)

//go:embed model_health_inject.js
var modelHealthInjectJS []byte

var legacyManagementEnhancementPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)<script[^>]*id=["']cpapi-model-health-live(?:-v[0-9]+)?["'][^>]*>.*?</script>`),
}

var nativeManagementPanelMarkers = [][]byte{
	[]byte(`data-cpapi-native-ai-providers=`),
}

// injectManagementEnhancements reads the management HTML and injects runtime panel enhancements.
func injectManagementEnhancements(filePath string) ([]byte, error) {
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	initialModelHealth := readSiblingManagementSnapshot(filePath, "model-health.json")
	return injectManagementEnhancementSnippetsWithModelHealth(raw, initialModelHealth), nil
}

func injectManagementEnhancementSnippets(raw []byte) []byte {
	return injectManagementEnhancementSnippetsWithModelHealth(raw, nil)
}

func injectManagementEnhancementSnippetsWithModelHealth(raw, initialModelHealth []byte) []byte {
	cleaned := stripLegacyManagementEnhancements(raw)
	if hasNativeManagementPanelMarker(cleaned) {
		return cleaned
	}
	return ensureInjectedSnippet(cleaned, []byte(`id="cpapi-model-health-live"`), modelHealthInjectionScript(initialModelHealth))
}

func stripLegacyManagementEnhancements(raw []byte) []byte {
	cleaned := raw
	for _, pattern := range legacyManagementEnhancementPatterns {
		cleaned = pattern.ReplaceAll(cleaned, nil)
	}
	return cleaned
}

func hasNativeManagementPanelMarker(raw []byte) bool {
	lower := bytes.ToLower(raw)
	for _, marker := range nativeManagementPanelMarkers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func ensureInjectedSnippet(raw, marker, snippet []byte) []byte {
	if bytes.Contains(raw, marker) {
		return raw
	}
	lower := bytes.ToLower(raw)
	idx := bytes.LastIndex(lower, []byte("</body>"))
	if idx < 0 {
		return append(append(raw, '\n'), snippet...)
	}
	out := make([]byte, 0, len(raw)+len(snippet))
	out = append(out, raw[:idx]...)
	out = append(out, snippet...)
	out = append(out, raw[idx:]...)
	return out
}

func readSiblingManagementSnapshot(filePath, name string) []byte {
	dir := filepath.Dir(filePath)
	if dir == "" || dir == "." {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	return bytes.TrimSpace(raw)
}

func modelHealthInjectionScript(initialModelHealth []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString(`<script id="cpapi-model-health-live">`)
	if len(initialModelHealth) > 0 {
		buf.WriteString(`window.__CPAPI_INITIAL_MODEL_HEALTH__=`)
		buf.Write(bytes.ReplaceAll(initialModelHealth, []byte(`</script`), []byte(`<\/script`)))
		buf.WriteString(`;`)
	}
	buf.Write(modelHealthInjectJS)
	buf.WriteString(`</script>`)
	return buf.Bytes()
}
