package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotWithPersistedDetails_MergesDiskRecordsWithoutChangingTotals(t *testing.T) {
	dir := t.TempDir()
	recordTime := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	body := `{"timestamp":"2026-04-02T10:00:00Z","api_key":"codex","model":"gpt-5.4","source":"unit","auth_index":"0","failed":false,"tokens":{"total_tokens":42}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-04-02.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ConfigureRuntime(RuntimeConfig{
		Enabled:             true,
		DetailsStateDir:     dir,
		DetailsMemoryWindow: time.Hour,
		DetailsRetention:    7 * 24 * time.Hour,
	})
	defer ConfigureRuntime(RuntimeConfig{})

	base := StatisticsSnapshot{
		TotalRequests: 10,
		SuccessCount:  9,
		FailureCount:  1,
		TotalTokens:   123,
		APIs: map[string]APISnapshot{
			"codex": {
				TotalRequests: 10,
				TotalTokens:   123,
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						TotalRequests: 10,
						TotalTokens:   123,
					},
				},
			},
		},
	}

	merged, status := SnapshotWithPersistedDetails(base, recordTime.Add(time.Hour), 24*time.Hour)
	if status.LoadedRecords != 1 {
		t.Fatalf("status.LoadedRecords = %d, want 1", status.LoadedRecords)
	}
	if status.MergedRecords != 1 {
		t.Fatalf("status.MergedRecords = %d, want 1", status.MergedRecords)
	}
	if merged.TotalRequests != 10 {
		t.Fatalf("merged.TotalRequests = %d, want 10", merged.TotalRequests)
	}
	details := merged.APIs["codex"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
	if !details[0].Timestamp.Equal(recordTime) {
		t.Fatalf("detail timestamp = %s, want %s", details[0].Timestamp, recordTime)
	}
}

func TestSnapshotWithPersistedDetails_DedupesExistingMemoryDetail(t *testing.T) {
	dir := t.TempDir()
	recordTime := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	body := `{"timestamp":"2026-04-02T10:00:00Z","api_key":"codex","model":"gpt-5.4","source":"unit","auth_index":"0","failed":false,"tokens":{"total_tokens":42}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "2026-04-02.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ConfigureRuntime(RuntimeConfig{
		Enabled:             true,
		DetailsStateDir:     dir,
		DetailsMemoryWindow: time.Hour,
		DetailsRetention:    7 * 24 * time.Hour,
	})
	defer ConfigureRuntime(RuntimeConfig{})

	base := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"codex": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: recordTime,
							Source:    "unit",
							AuthIndex: "0",
							Failed:    false,
							Tokens: TokenStats{
								TotalTokens: 42,
							},
						}},
					},
				},
			},
		},
	}

	merged, status := SnapshotWithPersistedDetails(base, recordTime.Add(time.Hour), 24*time.Hour)
	if status.MergedRecords != 0 {
		t.Fatalf("status.MergedRecords = %d, want 0", status.MergedRecords)
	}
	details := merged.APIs["codex"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("len(details) = %d, want 1", len(details))
	}
}
