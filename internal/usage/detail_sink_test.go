package usage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetailSinkClose_DrainsQueuedRecords(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sink, err := newDetailSink(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("newDetailSink() error = %v", err)
	}

	now := time.Date(2026, 4, 2, 20, 0, 0, 0, time.UTC)
	sink.Enqueue(persistedDetailRecord{
		Timestamp: now,
		APIKey:    "codex",
		Model:     "gpt-5.4",
		Tokens: TokenStats{
			TotalTokens: 42,
		},
	})

	sink.Close()

	data, err := os.ReadFile(filepath.Join(dir, "2026-04-02.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), `"api_key":"codex"`) {
		t.Fatalf("persisted file missing queued record: %s", string(data))
	}
}
