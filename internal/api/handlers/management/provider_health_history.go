package management

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
)

const (
	providerHealthHistoryFileName    = "provider-health-history.json"
	providerHealthHistoryFileVersion = 1
	providerHealthHistoryMaxEntries  = 60
)

type providerHealthHistoryEntry struct {
	Status   string `json:"status"`
	At       string `json:"at"`
	TestedAt string `json:"tested_at,omitempty"`
}

type providerHealthHistory map[string][]providerHealthHistoryEntry

type persistedProviderHealthHistory struct {
	Version int                   `json:"version"`
	SavedAt time.Time             `json:"saved_at"`
	History providerHealthHistory `json:"history"`
}

func (h *Handler) updateProviderHealthHistory(items []providerHealthItem) providerHealthHistory {
	if h == nil {
		return nil
	}

	h.providerHealthMu.Lock()
	defer h.providerHealthMu.Unlock()

	history, _ := h.loadProviderHealthHistoryLocked()
	if history == nil {
		history = make(providerHealthHistory)
	}

	now := time.Now().UTC()
	changed := false
	for _, item := range items {
		for _, model := range item.Models {
			key := providerHealthHistoryKey(item.Prefix, firstNonEmpty(model.UpstreamModel, model.Model))
			if key == "" {
				continue
			}

			at := parseHealthTime(model.TestedAt)
			if at.IsZero() {
				at = parseHealthTime(item.UpdatedAt)
			}
			if at.IsZero() {
				at = now
			}

			entry := providerHealthHistoryEntry{
				Status:   strings.TrimSpace(model.Status),
				At:       formatHealthTime(at),
				TestedAt: strings.TrimSpace(model.TestedAt),
			}
			if entry.Status == "" || entry.At == "" {
				continue
			}

			next, didChange := appendProviderHealthHistoryEntry(history[key], entry)
			if didChange {
				history[key] = next
				changed = true
			}
		}
	}

	if changed {
		_ = h.saveProviderHealthHistoryLocked(history)
	}
	return cloneProviderHealthHistory(history)
}

func appendProviderHealthHistoryEntry(items []providerHealthHistoryEntry, entry providerHealthHistoryEntry) ([]providerHealthHistoryEntry, bool) {
	if len(items) > 0 {
		last := items[len(items)-1]
		if last.At == entry.At {
			if last.Status == entry.Status && strings.TrimSpace(last.TestedAt) == strings.TrimSpace(entry.TestedAt) {
				return items, false
			}
			next := append([]providerHealthHistoryEntry(nil), items...)
			next[len(next)-1] = entry
			return next, true
		}
	}

	next := append(append([]providerHealthHistoryEntry(nil), items...), entry)
	if len(next) > providerHealthHistoryMaxEntries {
		next = append([]providerHealthHistoryEntry(nil), next[len(next)-providerHealthHistoryMaxEntries:]...)
	}
	return next, true
}

func providerHealthHistoryKey(prefix, model string) string {
	prefix = strings.TrimSpace(prefix)
	model = strings.TrimSpace(model)
	if prefix == "" || model == "" {
		return ""
	}
	return prefix + "/" + model
}

func (h *Handler) providerHealthHistoryPath() string {
	staticDir := managementasset.StaticDir(h.configFilePath)
	if strings.TrimSpace(staticDir) == "" {
		return ""
	}
	return filepath.Join(staticDir, providerHealthHistoryFileName)
}

func (h *Handler) loadProviderHealthHistoryLocked() (providerHealthHistory, error) {
	if h.providerHealthLoaded {
		return cloneProviderHealthHistory(h.providerHealthHistory), nil
	}

	path := h.providerHealthHistoryPath()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("provider health history path is empty")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			h.providerHealthHistory = make(providerHealthHistory)
			h.providerHealthLoaded = true
			return make(providerHealthHistory), nil
		}
		return nil, err
	}

	var wrapped persistedProviderHealthHistory
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.History) > 0 {
		sanitized := sanitizeProviderHealthHistory(wrapped.History)
		h.providerHealthHistory = cloneProviderHealthHistory(sanitized)
		h.providerHealthLoaded = true
		return sanitized, nil
	}

	var direct providerHealthHistory
	if err := json.Unmarshal(raw, &direct); err != nil {
		return nil, err
	}
	sanitized := sanitizeProviderHealthHistory(direct)
	h.providerHealthHistory = cloneProviderHealthHistory(sanitized)
	h.providerHealthLoaded = true
	return sanitized, nil
}

func sanitizeProviderHealthHistory(history providerHealthHistory) providerHealthHistory {
	if len(history) == 0 {
		return make(providerHealthHistory)
	}
	out := make(providerHealthHistory, len(history))
	for key, entries := range history {
		cleanKey := strings.TrimSpace(key)
		if cleanKey == "" {
			continue
		}
		cleanEntries := make([]providerHealthHistoryEntry, 0, len(entries))
		for _, entry := range entries {
			status := strings.TrimSpace(entry.Status)
			at := strings.TrimSpace(entry.At)
			if status == "" || at == "" {
				continue
			}
			cleanEntries = append(cleanEntries, providerHealthHistoryEntry{
				Status:   status,
				At:       at,
				TestedAt: strings.TrimSpace(entry.TestedAt),
			})
		}
		if len(cleanEntries) > providerHealthHistoryMaxEntries {
			cleanEntries = cleanEntries[len(cleanEntries)-providerHealthHistoryMaxEntries:]
		}
		if len(cleanEntries) > 0 {
			out[cleanKey] = cleanEntries
		}
	}
	return out
}

func (h *Handler) saveProviderHealthHistoryLocked(history providerHealthHistory) error {
	path := h.providerHealthHistoryPath()
	if strings.TrimSpace(path) == "" {
		return errors.New("provider health history path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	payload := persistedProviderHealthHistory{
		Version: providerHealthHistoryFileVersion,
		SavedAt: time.Now().UTC(),
		History: history,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	h.providerHealthHistory = cloneProviderHealthHistory(history)
	h.providerHealthLoaded = true
	return nil
}

func cloneProviderHealthHistory(history providerHealthHistory) providerHealthHistory {
	if len(history) == 0 {
		return make(providerHealthHistory)
	}

	cloned := make(providerHealthHistory, len(history))
	for key, entries := range history {
		if len(entries) == 0 {
			cloned[key] = nil
			continue
		}
		nextEntries := make([]providerHealthHistoryEntry, len(entries))
		copy(nextEntries, entries)
		cloned[key] = nextEntries
	}
	return cloned
}
