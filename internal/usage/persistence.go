package usage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const snapshotFileVersion = 1

type persistedSnapshot struct {
	Version int                `json:"version"`
	SavedAt time.Time          `json:"saved_at"`
	Usage   StatisticsSnapshot `json:"usage"`
}

// LoadSnapshotFromFile loads a persisted statistics snapshot from disk.
// It accepts both the wrapped persistence format and the raw snapshot shape
// used by the management export endpoint for forward compatibility.
func LoadSnapshotFromFile(path string) (StatisticsSnapshot, error) {
	var snapshot StatisticsSnapshot
	cleanPath := strings.TrimSpace(path)
	if cleanPath == "" {
		return snapshot, errors.New("usage snapshot path is empty")
	}

	raw, err := os.ReadFile(cleanPath)
	if err != nil {
		return snapshot, err
	}

	var wrapped persistedSnapshot
	if err := json.Unmarshal(raw, &wrapped); err == nil && hasSnapshotData(wrapped.Usage) {
		return wrapped.Usage, nil
	}

	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return StatisticsSnapshot{}, err
	}
	return snapshot, nil
}

// SaveSnapshotToFile persists the provided statistics snapshot atomically.
func SaveSnapshotToFile(path string, snapshot StatisticsSnapshot) error {
	cleanPath := strings.TrimSpace(path)
	if cleanPath == "" {
		return errors.New("usage snapshot path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0o755); err != nil {
		return err
	}

	payload := persistedSnapshot{
		Version: snapshotFileVersion,
		SavedAt: time.Now().UTC(),
		Usage:   snapshot,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	tmpPath := cleanPath + ".tmp"
	if err := os.WriteFile(tmpPath, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, cleanPath)
}

func hasSnapshotData(snapshot StatisticsSnapshot) bool {
	if snapshot.TotalRequests > 0 || snapshot.SuccessCount > 0 || snapshot.FailureCount > 0 || snapshot.TotalTokens > 0 {
		return true
	}
	if len(snapshot.APIs) > 0 || len(snapshot.RequestsByDay) > 0 || len(snapshot.RequestsByHour) > 0 || len(snapshot.TokensByDay) > 0 || len(snapshot.TokensByHour) > 0 {
		return true
	}
	return false
}
