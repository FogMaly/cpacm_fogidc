package watcher

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

var ignoredWatcherJSONBaseNames = map[string]struct{}{
	"model-health.json":       {},
	"model-health-multi.json": {},
}

func isIgnoredWatcherJSONPath(path string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(path)))
	_, ignored := ignoredWatcherJSONBaseNames[base]
	return ignored
}

func isRecognizedAuthJSONData(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return strings.TrimSpace(probe.Type) != ""
}
