package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultPersistedDetailsWindow = 24 * time.Hour
	maxPersistedDetailLineBytes   = 1024 * 1024
)

type PersistedMergeStatus struct {
	Enabled        bool      `json:"enabled"`
	WindowHours    int64     `json:"window_hours"`
	Since          time.Time `json:"since,omitempty"`
	Until          time.Time `json:"until,omitempty"`
	LoadedRecords  int       `json:"loaded_records"`
	MergedRecords  int       `json:"merged_records"`
	FilesScanned   int       `json:"files_scanned"`
	Directory      string    `json:"directory,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	WindowFallback string    `json:"window_fallback,omitempty"`
}

func SnapshotWithPersistedDetails(base StatisticsSnapshot, now time.Time, window time.Duration) (StatisticsSnapshot, PersistedMergeStatus) {
	status := PersistedMergeStatus{}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	defaultRuntime.mu.RLock()
	cfg := defaultRuntime.cfg
	defaultRuntime.mu.RUnlock()

	status.Enabled = cfg.Enabled
	status.Directory = cfg.DetailsStateDir

	if !cfg.Enabled || strings.TrimSpace(cfg.DetailsStateDir) == "" {
		return base, status
	}

	window, fallback := normalizePersistedDetailsWindow(window, cfg.DetailsRetention)
	status.WindowHours = int64(window / time.Hour)
	status.Until = now
	status.Since = now.Add(-window)
	status.WindowFallback = fallback

	records, filesScanned, err := loadPersistedDetailRecords(cfg.DetailsStateDir, status.Since, status.Until)
	status.FilesScanned = filesScanned
	if err != nil {
		status.LastError = err.Error()
		return base, status
	}
	status.LoadedRecords = len(records)
	merged, mergedCount := mergePersistedDetails(base, records)
	status.MergedRecords = mergedCount
	return merged, status
}

func normalizePersistedDetailsWindow(window, retention time.Duration) (time.Duration, string) {
	retention = normalizeDetailsRetention(retention)
	if window <= 0 {
		window = defaultPersistedDetailsWindow
		if window > retention {
			return retention, "retention"
		}
		return window, "default"
	}
	if window > retention {
		return retention, "retention"
	}
	return window, ""
}

func loadPersistedDetailRecords(dir string, since, until time.Time) ([]persistedDetailRecord, int, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, 0, errors.New("persisted detail directory is empty")
	}
	if until.IsZero() {
		until = time.Now().UTC()
	}
	if since.IsZero() {
		since = until.Add(-defaultPersistedDetailsWindow)
	}
	if since.After(until) {
		since, until = until, since
	}

	startDay := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	endDay := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)

	var out []persistedDetailRecord
	filesScanned := 0
	for day := startDay; !day.After(endDay); day = day.Add(24 * time.Hour) {
		path := filepath.Join(dir, day.Format("2006-01-02")+".jsonl")
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return out, filesScanned, err
		}
		filesScanned++

		records, err := readPersistedDetailFile(file, since, until)
		_ = file.Close()
		if err != nil {
			return out, filesScanned, err
		}
		out = append(out, records...)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out, filesScanned, nil
}

func readPersistedDetailFile(reader io.Reader, since, until time.Time) ([]persistedDetailRecord, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxPersistedDetailLineBytes)

	var out []persistedDetailRecord
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record persistedDetailRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return out, err
		}
		record.Timestamp = record.Timestamp.UTC()
		if record.Timestamp.Before(since) || record.Timestamp.After(until) {
			continue
		}
		out = append(out, record)
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func mergePersistedDetails(base StatisticsSnapshot, records []persistedDetailRecord) (StatisticsSnapshot, int) {
	if len(records) == 0 {
		return base, 0
	}

	merged := cloneStatisticsSnapshot(base)
	seen := make(map[string]struct{})
	for apiName, apiSnapshot := range merged.APIs {
		for modelName, modelSnapshot := range apiSnapshot.Models {
			for _, detail := range modelSnapshot.Details {
				seen[dedupKey(apiName, modelName, detail)] = struct{}{}
			}
		}
	}

	mergedCount := 0
	for _, record := range records {
		apiName := strings.TrimSpace(record.APIKey)
		if apiName == "" {
			apiName = "unknown"
		}
		modelName := strings.TrimSpace(record.Model)
		if modelName == "" {
			modelName = "unknown"
		}
		detail := RequestDetail{
			Timestamp: record.Timestamp.UTC(),
			Source:    record.Source,
			AuthIndex: record.AuthIndex,
			Tokens:    normaliseTokenStats(record.Tokens),
			Failed:    record.Failed,
		}
		key := dedupKey(apiName, modelName, detail)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		apiSnapshot := merged.APIs[apiName]
		if apiSnapshot.Models == nil {
			apiSnapshot.Models = make(map[string]ModelSnapshot)
		}
		modelSnapshot := apiSnapshot.Models[modelName]
		modelSnapshot.Details = append(modelSnapshot.Details, detail)
		apiSnapshot.Models[modelName] = modelSnapshot
		merged.APIs[apiName] = apiSnapshot
		mergedCount++
	}

	for apiName, apiSnapshot := range merged.APIs {
		for modelName, modelSnapshot := range apiSnapshot.Models {
			sort.Slice(modelSnapshot.Details, func(i, j int) bool {
				return modelSnapshot.Details[i].Timestamp.Before(modelSnapshot.Details[j].Timestamp)
			})
			apiSnapshot.Models[modelName] = modelSnapshot
		}
		merged.APIs[apiName] = apiSnapshot
	}

	return merged, mergedCount
}

func cloneStatisticsSnapshot(snapshot StatisticsSnapshot) StatisticsSnapshot {
	out := StatisticsSnapshot{
		TotalRequests: snapshot.TotalRequests,
		SuccessCount:  snapshot.SuccessCount,
		FailureCount:  snapshot.FailureCount,
		TotalTokens:   snapshot.TotalTokens,
	}

	if snapshot.APIs != nil {
		out.APIs = make(map[string]APISnapshot, len(snapshot.APIs))
		for apiName, apiSnapshot := range snapshot.APIs {
			nextAPI := APISnapshot{
				TotalRequests: apiSnapshot.TotalRequests,
				TotalTokens:   apiSnapshot.TotalTokens,
			}
			if apiSnapshot.Models != nil {
				nextAPI.Models = make(map[string]ModelSnapshot, len(apiSnapshot.Models))
				for modelName, modelSnapshot := range apiSnapshot.Models {
					nextModel := ModelSnapshot{
						TotalRequests: modelSnapshot.TotalRequests,
						TotalTokens:   modelSnapshot.TotalTokens,
					}
					if len(modelSnapshot.Details) > 0 {
						nextModel.Details = make([]RequestDetail, len(modelSnapshot.Details))
						copy(nextModel.Details, modelSnapshot.Details)
					}
					nextAPI.Models[modelName] = nextModel
				}
			}
			out.APIs[apiName] = nextAPI
		}
	}

	if snapshot.RequestsByDay != nil {
		out.RequestsByDay = make(map[string]int64, len(snapshot.RequestsByDay))
		for k, v := range snapshot.RequestsByDay {
			out.RequestsByDay[k] = v
		}
	}
	if snapshot.RequestsByHour != nil {
		out.RequestsByHour = make(map[string]int64, len(snapshot.RequestsByHour))
		for k, v := range snapshot.RequestsByHour {
			out.RequestsByHour[k] = v
		}
	}
	if snapshot.TokensByDay != nil {
		out.TokensByDay = make(map[string]int64, len(snapshot.TokensByDay))
		for k, v := range snapshot.TokensByDay {
			out.TokensByDay[k] = v
		}
	}
	if snapshot.TokensByHour != nil {
		out.TokensByHour = make(map[string]int64, len(snapshot.TokensByHour))
		for k, v := range snapshot.TokensByHour {
			out.TokensByHour[k] = v
		}
	}
	return out
}
