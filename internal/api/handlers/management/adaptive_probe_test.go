package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestMergeAdaptiveProbeEntryFileReplacesProviderAndRebuildsSummary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "model-health-multi.json")
	if err := os.WriteFile(path, []byte(`{
  "updated_at": "2026-03-31T12:00:00Z",
  "interval_sec": 3600,
  "entries": [
    {
      "updated_at": "2026-03-31T12:00:00Z",
      "provider_prefix": "sub2api",
      "base_url": "https://sub2api.example/v1",
      "source_mode": "config_yaml",
      "summary": {"total": 1, "green": 1, "yellow": 0, "red": 0},
      "models": [
        {"model": "sub2api/gpt-5.4", "status": "green", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    },
    {
      "updated_at": "2026-03-31T12:00:00Z",
      "provider_prefix": "yunyi-codex",
      "base_url": "https://yunyi.example/v1",
      "source_mode": "config_yaml",
      "summary": {"total": 1, "green": 0, "yellow": 1, "red": 0},
      "models": [
        {"model": "yunyi-codex/gpt-5.4", "status": "yellow", "tested_at": "2026-03-31T12:00:00Z"}
      ]
    }
  ]
}`), 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	entry := adaptiveProbeSnapshotEntry{
		UpdatedAt:      "2026-03-31T12:05:00Z",
		ProviderPrefix: "yunyi-codex",
		BaseURL:        "https://yunyi.example/v1",
		SourceMode:     "config_yaml",
		Models: []map[string]any{
			{
				"model":     "yunyi-codex/gpt-5.4",
				"status":    "green",
				"tested_at": "2026-03-31T12:05:00Z",
			},
		},
	}

	if err := mergeAdaptiveProbeEntryFile(path, entry); err != nil {
		t.Fatalf("mergeAdaptiveProbeEntryFile() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read merged snapshot: %v", err)
	}
	var snapshot adaptiveProbeSnapshotFile
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("unmarshal merged snapshot: %v", err)
	}
	if snapshot.UpdatedAt != "2026-03-31T12:05:00Z" {
		t.Fatalf("snapshot.UpdatedAt = %q, want %q", snapshot.UpdatedAt, "2026-03-31T12:05:00Z")
	}
	if len(snapshot.Entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(snapshot.Entries))
	}
	if snapshot.Summary.Total != 2 || snapshot.Summary.Green != 2 || snapshot.Summary.Yellow != 0 || snapshot.Summary.Red != 0 {
		t.Fatalf("summary = %+v, want total=2 green=2 yellow=0 red=0", snapshot.Summary)
	}
	model, ok := snapshot.ByModel["yunyi-codex/gpt-5.4"]
	if !ok {
		t.Fatalf("by_model missing yunyi-codex/gpt-5.4")
	}
	if got := adaptiveProbeStringValue(model["status"]); got != "green" {
		t.Fatalf("yunyi by_model status = %q, want %q", got, "green")
	}
}

func TestAdaptiveProbeRoundRemovesRecoveredPrefix(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("debug: true\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	h := &Handler{
		cfg:               &config.Config{},
		configFilePath:    configPath,
		adaptiveProbes:    make(map[string]*adaptiveProbeState),
		adaptiveProbeWake: make(chan struct{}, 1),
	}
	h.adaptiveProbeRunner = func(_ context.Context, prefix string) (*adaptiveProbeSnapshotEntry, error) {
		return &adaptiveProbeSnapshotEntry{
			UpdatedAt:      time.Date(2026, 3, 31, 12, 5, 0, 0, time.UTC).Format(time.RFC3339),
			ProviderPrefix: prefix,
			BaseURL:        "https://sub2api.example/v1",
			SourceMode:     "config_yaml",
			Models: []map[string]any{
				{
					"model":     prefix + "/gpt-5.4",
					"status":    "green",
					"tested_at": time.Date(2026, 3, 31, 12, 5, 0, 0, time.UTC).Format(time.RFC3339),
				},
			},
		}, nil
	}

	h.NotifyAdaptiveProbeFailure("sub2api", "sub2api/gpt-5.4", 503, "probe_seed_failure")
	h.runAdaptiveProbeRound()

	if len(h.adaptiveProbes) != 0 {
		t.Fatalf("adaptive probe queue size = %d, want 0", len(h.adaptiveProbes))
	}

	snapshotPath := filepath.Join(root, "static", "model-health-multi.json")
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read merged snapshot: %v", err)
	}
	var snapshot adaptiveProbeSnapshotFile
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("unmarshal merged snapshot: %v", err)
	}
	if snapshot.Summary.Green != 1 || snapshot.Summary.Total != 1 {
		t.Fatalf("summary = %+v, want total=1 green=1", snapshot.Summary)
	}
}
