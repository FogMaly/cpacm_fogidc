package usage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestRequestStatisticsRecord_PrunesDetailsByWindow(t *testing.T) {
	SetDetailsMemoryWindow(time.Hour)
	defer SetDetailsMemoryWindow(defaultDetailsMemoryWindow)
	stats := NewRequestStatistics()
	oldTime := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(2 * time.Hour)

	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "codex",
		Model:       "gpt-5.4",
		RequestedAt: oldTime,
		Detail: coreusage.Detail{
			TotalTokens: 1,
		},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "codex",
		Model:       "gpt-5.4",
		RequestedAt: newTime,
		Detail: coreusage.Detail{
			TotalTokens: 2,
		},
	})

	snapshot := stats.Snapshot()
	model := snapshot.APIs["codex"].Models["gpt-5.4"]
	if len(model.Details) != 1 {
		t.Fatalf("len(model.Details) = %d, want 1", len(model.Details))
	}
	if !model.Details[0].Timestamp.Equal(newTime) {
		t.Fatalf("detail timestamp = %s, want %s", model.Details[0].Timestamp, newTime)
	}
	if snapshot.TotalRequests != 2 {
		t.Fatalf("TotalRequests = %d, want 2", snapshot.TotalRequests)
	}
}

func TestRequestStatisticsPruneExpiredDetails_PreservesAggregates(t *testing.T) {
	SetDetailsMemoryWindow(time.Hour)
	defer SetDetailsMemoryWindow(defaultDetailsMemoryWindow)

	stats := NewRequestStatistics()
	now := time.Date(2026, 4, 2, 19, 0, 0, 0, time.UTC)
	oldTime := now.Add(-48 * time.Hour)
	newTime := now.Add(-30 * time.Minute)

	result := stats.MergeSnapshot(StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"codex": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4-old": {
						Details: []RequestDetail{
							{
								Timestamp: oldTime,
								Tokens: TokenStats{
									TotalTokens: 11,
								},
							},
						},
					},
					"gpt-5.4": {
						Details: []RequestDetail{
							{
								Timestamp: newTime,
								Tokens: TokenStats{
									TotalTokens: 22,
								},
							},
						},
					},
				},
			},
		},
	})
	if result.Added != 2 {
		t.Fatalf("MergeSnapshot added = %d, want 2", result.Added)
	}

	removed := stats.PruneExpiredDetails(now)
	if removed != 1 {
		t.Fatalf("PruneExpiredDetails removed = %d, want 1", removed)
	}

	snapshot := stats.Snapshot()
	model := snapshot.APIs["codex"].Models["gpt-5.4"]
	oldModel := snapshot.APIs["codex"].Models["gpt-5.4-old"]
	if snapshot.TotalRequests != 2 {
		t.Fatalf("TotalRequests = %d, want 2", snapshot.TotalRequests)
	}
	if model.TotalRequests != 1 {
		t.Fatalf("Model TotalRequests = %d, want 1", model.TotalRequests)
	}
	if oldModel.TotalRequests != 1 {
		t.Fatalf("Old model TotalRequests = %d, want 1", oldModel.TotalRequests)
	}
	if len(model.Details) != 1 {
		t.Fatalf("len(model.Details) = %d, want 1", len(model.Details))
	}
	if len(oldModel.Details) != 0 {
		t.Fatalf("len(oldModel.Details) = %d, want 0", len(oldModel.Details))
	}
	if !model.Details[0].Timestamp.Equal(newTime) {
		t.Fatalf("detail timestamp = %s, want %s", model.Details[0].Timestamp, newTime)
	}
}

func TestLoggerPlugin_DiskFirstModeSkipsInMemoryDetailsForLiveTraffic(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 4, 2, 20, 0, 0, 0, time.UTC)
	stats := NewRequestStatistics()
	plugin := &LoggerPlugin{stats: stats}

	ConfigureRuntime(RuntimeConfig{
		Enabled:             true,
		DetailsStateDir:     dir,
		DetailsMemoryWindow: 15 * time.Minute,
		DetailsRetention:    7 * 24 * time.Hour,
	})
	defer ConfigureRuntime(RuntimeConfig{})

	plugin.HandleUsage(context.Background(), coreusage.Record{
		APIKey:      "codex",
		Model:       "gpt-5.4",
		RequestedAt: now,
		Source:      "unit",
		AuthIndex:   "0",
		Detail: coreusage.Detail{
			TotalTokens: 42,
		},
	})

	defaultRuntime.mu.RLock()
	sink := defaultRuntime.sink
	defaultRuntime.mu.RUnlock()
	if sink == nil {
		t.Fatal("expected runtime sink to be configured")
	}
	sink.Close()

	snapshot := stats.Snapshot()
	if got := len(snapshot.APIs["codex"].Models["gpt-5.4"].Details); got != 0 {
		t.Fatalf("in-memory details len = %d, want 0 in disk-first mode", got)
	}

	merged, status := SnapshotWithPersistedDetails(snapshot, now.Add(time.Hour), 24*time.Hour)
	if status.LoadedRecords != 1 {
		t.Fatalf("status.LoadedRecords = %d, want 1", status.LoadedRecords)
	}
	details := merged.APIs["codex"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("merged details len = %d, want 1", len(details))
	}
	if !details[0].Timestamp.Equal(now) {
		t.Fatalf("detail timestamp = %s, want %s", details[0].Timestamp, now)
	}

	data, err := os.ReadFile(filepath.Join(dir, "2026-04-02.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected persisted usage detail file to be non-empty")
	}
}
