package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndLoadSnapshotToFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "usage-statistics.json")
	now := time.Date(2026, 3, 16, 21, 40, 0, 0, time.UTC)
	input := StatisticsSnapshot{
		TotalRequests: 2,
		SuccessCount:  1,
		FailureCount:  1,
		TotalTokens:   42,
		APIs: map[string]APISnapshot{
			"codex": {
				TotalRequests: 2,
				TotalTokens:   42,
				Models: map[string]ModelSnapshot{
					"gpt-5.3-codex": {
						TotalRequests: 2,
						TotalTokens:   42,
						Details: []RequestDetail{
							{
								Timestamp: now,
								Source:    "unit-test",
								AuthIndex: "0",
								Failed:    false,
								Tokens: TokenStats{
									InputTokens:  20,
									OutputTokens: 22,
									TotalTokens:  42,
								},
							},
						},
					},
				},
			},
		},
		RequestsByDay:  map[string]int64{"2026-03-16": 2},
		RequestsByHour: map[string]int64{"21": 2},
		TokensByDay:    map[string]int64{"2026-03-16": 42},
		TokensByHour:   map[string]int64{"21": 42},
	}

	if err := SaveSnapshotToFile(path, input); err != nil {
		t.Fatalf("SaveSnapshotToFile() error = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat persisted file: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("persisted file is empty")
	}

	got, err := LoadSnapshotFromFile(path)
	if err != nil {
		t.Fatalf("LoadSnapshotFromFile() error = %v", err)
	}

	if got.TotalRequests != input.TotalRequests || got.SuccessCount != input.SuccessCount || got.FailureCount != input.FailureCount || got.TotalTokens != input.TotalTokens {
		t.Fatalf("loaded top-level counters mismatch: got %+v want %+v", got, input)
	}

	api, ok := got.APIs["codex"]
	if !ok {
		t.Fatalf("loaded APIs missing codex entry: %+v", got.APIs)
	}
	model, ok := api.Models["gpt-5.3-codex"]
	if !ok {
		t.Fatalf("loaded model snapshot missing gpt-5.3-codex: %+v", api.Models)
	}
	if len(model.Details) != 1 {
		t.Fatalf("loaded detail length = %d, want 1", len(model.Details))
	}
	if !model.Details[0].Timestamp.Equal(now) {
		t.Fatalf("loaded detail timestamp = %s, want %s", model.Details[0].Timestamp, now)
	}
}

func TestRequestStatisticsDirtyLifecycle(t *testing.T) {
	t.Parallel()

	stats := NewRequestStatistics()
	if stats.Dirty() {
		t.Fatal("new RequestStatistics should not be dirty")
	}

	stats.recordImported("codex", "gpt-5.3-codex", &apiStats{Models: map[string]*modelStats{}}, RequestDetail{
		Timestamp: time.Date(2026, 3, 16, 21, 50, 0, 0, time.UTC),
		Tokens: TokenStats{
			TotalTokens: 1,
		},
	})
	if !stats.Dirty() {
		t.Fatal("RequestStatistics should be dirty after import")
	}

	stats.MarkPersisted()
	if stats.Dirty() {
		t.Fatal("RequestStatistics should not be dirty after MarkPersisted")
	}
}
