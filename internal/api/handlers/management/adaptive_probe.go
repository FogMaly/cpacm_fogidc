package management

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
)

const (
	adaptiveProbeScanInterval  = 15 * time.Second
	adaptiveProbeRetryInterval = 30 * time.Second
	adaptiveProbeFastInterval  = 90 * time.Second
	adaptiveProbeTimeout       = 5 * time.Minute
)

type adaptiveProbeState struct {
	Prefix              string
	NextProbeAt         time.Time
	LastFailureAt       time.Time
	LastFailureStatus   int
	LastFailureModel    string
	LastFailureReason   string
	LastProbeAt         time.Time
	LastProbeUpdatedAt  time.Time
	LastProbeStatus     string
	LastProbeReason     string
	LastProbeSummary    adaptiveProbeSummary
	ConsecutiveFailures int
	Running             bool
}

type adaptiveProbeSummary struct {
	Total  int `json:"total"`
	Green  int `json:"green"`
	Yellow int `json:"yellow"`
	Red    int `json:"red"`
}

type adaptiveProbeSnapshotEntry struct {
	UpdatedAt      string                     `json:"updated_at"`
	ProviderPrefix string                     `json:"provider_prefix"`
	BaseURL        string                     `json:"base_url,omitempty"`
	SourceMode     string                     `json:"source_mode,omitempty"`
	IntervalSec    int                        `json:"interval_sec,omitempty"`
	Summary        adaptiveProbeSummary       `json:"summary"`
	Models         []map[string]any           `json:"models"`
	Task           map[string]any             `json:"task,omitempty"`
	Error          string                     `json:"error,omitempty"`
	Extra          map[string]json.RawMessage `json:"-"`
}

type adaptiveProbeSnapshotFile struct {
	UpdatedAt   string                       `json:"updated_at"`
	IntervalSec int                          `json:"interval_sec"`
	Entries     []adaptiveProbeSnapshotEntry `json:"entries"`
	Summary     adaptiveProbeSummary         `json:"summary"`
	ByModel     map[string]map[string]any    `json:"by_model,omitempty"`
}

func (h *Handler) startAdaptiveProbeWorker() {
	if h == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(adaptiveProbeScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-h.adaptiveProbeWake:
			}
			h.runAdaptiveProbeRound()
		}
	}()
}

func (h *Handler) NotifyAdaptiveProbeFailure(prefix, model string, status int, reason string) {
	if h == nil {
		return
	}
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return
	}
	now := time.Now().UTC()
	h.adaptiveProbeMu.Lock()
	state := h.adaptiveProbes[prefix]
	if state == nil {
		state = &adaptiveProbeState{Prefix: prefix}
		h.adaptiveProbes[prefix] = state
	}
	state.LastFailureAt = now
	state.LastFailureStatus = status
	state.LastFailureModel = strings.TrimSpace(model)
	state.LastFailureReason = strings.TrimSpace(reason)
	state.ConsecutiveFailures++
	if state.NextProbeAt.IsZero() || state.NextProbeAt.After(now) {
		state.NextProbeAt = now
	}
	h.adaptiveProbeMu.Unlock()
	select {
	case h.adaptiveProbeWake <- struct{}{}:
	default:
	}
}

func (h *Handler) runAdaptiveProbeRound() {
	if h == nil {
		return
	}
	for {
		prefix := h.nextAdaptiveProbePrefix()
		if prefix == "" {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), adaptiveProbeTimeout)
		entry, err := h.adaptiveProbeRunner(ctx, prefix)
		cancel()

		nextDelay := adaptiveProbeRetryInterval
		recovered := false
		if err == nil && entry != nil {
			if mergeErr := h.mergeAdaptiveProbeEntry(entry); mergeErr != nil {
				err = mergeErr
			} else {
				recovered = adaptiveProbeEntryRecovered(entry)
				if !recovered {
					nextDelay = adaptiveProbeFastInterval
				}
			}
		}
		h.finishAdaptiveProbe(prefix, entry, recovered, nextDelay)
	}
}

func (h *Handler) nextAdaptiveProbePrefix() string {
	if h == nil {
		return ""
	}
	now := time.Now().UTC()
	h.adaptiveProbeMu.Lock()
	defer h.adaptiveProbeMu.Unlock()

	var (
		selected string
		nextAt   time.Time
	)
	for prefix, state := range h.adaptiveProbes {
		if state == nil {
			delete(h.adaptiveProbes, prefix)
			continue
		}
		if state.Running || state.NextProbeAt.After(now) {
			continue
		}
		if selected == "" || state.NextProbeAt.Before(nextAt) {
			selected = prefix
			nextAt = state.NextProbeAt
		}
	}
	if selected == "" {
		return ""
	}
	h.adaptiveProbes[selected].Running = true
	return selected
}

func (h *Handler) finishAdaptiveProbe(prefix string, entry *adaptiveProbeSnapshotEntry, recovered bool, nextDelay time.Duration) {
	if h == nil {
		return
	}
	now := time.Now().UTC()
	h.adaptiveProbeMu.Lock()
	defer h.adaptiveProbeMu.Unlock()

	state := h.adaptiveProbes[prefix]
	if state == nil {
		return
	}
	if entry != nil {
		state.LastProbeAt = now
		if updatedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(entry.UpdatedAt)); err == nil {
			state.LastProbeUpdatedAt = updatedAt.UTC()
		} else {
			state.LastProbeUpdatedAt = time.Time{}
		}
		state.LastProbeSummary = adaptiveProbeEntrySummary(*entry)
		state.LastProbeStatus = adaptiveProbeOverallStatus(state.LastProbeSummary)
		state.LastProbeReason = adaptiveProbePrimaryReason(entry)
	}
	if recovered {
		delete(h.adaptiveProbes, prefix)
		return
	}
	state.Running = false
	state.NextProbeAt = now.Add(nextDelay)
}

func (h *Handler) runAdaptiveProbeScript(ctx context.Context, prefix string) (*adaptiveProbeSnapshotEntry, error) {
	if h == nil {
		return nil, fmt.Errorf("handler is nil")
	}
	scriptsDir, err := h.probeScriptsDir()
	if err != nil {
		return nil, err
	}
	envPath, err := h.probeEnvFilePath()
	if err != nil {
		return nil, err
	}

	managementURL := "http://127.0.0.1:34050"
	if value, ok, errRead := readEnvValue(envPath, "MANAGEMENT_URL"); errRead == nil && ok && strings.TrimSpace(value) != "" {
		managementURL = strings.TrimSpace(value)
	}
	managementKey := ""
	if value, ok, errRead := readEnvValue(envPath, "MANAGEMENT_KEY"); errRead == nil && ok {
		managementKey = strings.TrimSpace(value)
	}
	if managementKey == "" {
		managementKey = strings.TrimSpace(h.localPassword)
	}
	if managementKey == "" {
		managementKey = strings.TrimSpace(h.envSecret)
	}

	tmpFile, err := os.CreateTemp("", "adaptive-probe-*.json")
	if err != nil {
		return nil, err
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()
	defer os.Remove(tmpPath)

	cmd := exec.CommandContext(ctx, "bash", filepath.Join(scriptsDir, "hourly-model-health-monitor.sh"), "--once")
	cmd.Dir = filepath.Dir(scriptsDir)
	cmd.Env = append(os.Environ(),
		"MODEL_HEALTH_SKIP_CONFIG_SOURCE=true",
		"MANAGEMENT_URL="+managementURL,
		"MANAGEMENT_KEY="+managementKey,
		"CONFIG_YAML_PATH="+h.configFilePath,
		"PROVIDER_PREFIX="+prefix,
		"OUT_JSON="+tmpPath,
		"OUT_AUTH_JSON=",
		"OPENCLAW_SYNC_ENABLED=false",
	)

	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("adaptive probe timed out for %s", prefix)
	}
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("adaptive probe failed for %s: %s", prefix, msg)
	}

	raw, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, err
	}
	var entry adaptiveProbeSnapshotEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, err
	}
	if strings.TrimSpace(entry.ProviderPrefix) == "" {
		entry.ProviderPrefix = prefix
	}
	entry.ProviderPrefix = strings.ToLower(strings.TrimSpace(entry.ProviderPrefix))
	entry.Summary = adaptiveProbeEntrySummary(entry)
	return &entry, nil
}

func adaptiveProbeEntryRecovered(entry *adaptiveProbeSnapshotEntry) bool {
	if entry == nil {
		return false
	}
	summary := adaptiveProbeEntrySummary(*entry)
	return summary.Total > 0 && summary.Green > 0 && summary.Red == 0
}

func adaptiveProbeOverallStatus(summary adaptiveProbeSummary) string {
	switch {
	case summary.Red > 0:
		return "red"
	case summary.Yellow > 0:
		return "yellow"
	case summary.Green > 0:
		return "green"
	default:
		return "unknown"
	}
}

func adaptiveProbePrimaryReason(entry *adaptiveProbeSnapshotEntry) string {
	if entry == nil {
		return ""
	}
	for _, model := range entry.Models {
		reason := strings.TrimSpace(adaptiveProbeStringValue(model["reason"]))
		if reason != "" {
			return reason
		}
		errText := strings.TrimSpace(adaptiveProbeStringValue(model["error"]))
		if errText != "" {
			return errText
		}
	}
	return strings.TrimSpace(entry.Error)
}

func adaptiveProbeEntrySummary(entry adaptiveProbeSnapshotEntry) adaptiveProbeSummary {
	summary := adaptiveProbeSummary{}
	for _, model := range entry.Models {
		summary.Total++
		status := strings.ToLower(strings.TrimSpace(adaptiveProbeStringValue(model["status"])))
		switch status {
		case "green":
			summary.Green++
		case "yellow":
			summary.Yellow++
		case "red":
			summary.Red++
		}
	}
	return summary
}

type adaptiveProbeQueueItem struct {
	Prefix              string               `json:"prefix"`
	Running             bool                 `json:"running"`
	ConsecutiveFailures int                  `json:"consecutive_failures"`
	LastFailureAt       string               `json:"last_failure_at,omitempty"`
	LastFailureStatus   int                  `json:"last_failure_status,omitempty"`
	LastFailureModel    string               `json:"last_failure_model,omitempty"`
	LastFailureReason   string               `json:"last_failure_reason,omitempty"`
	LastProbeAt         string               `json:"last_probe_at,omitempty"`
	LastProbeUpdatedAt  string               `json:"last_probe_updated_at,omitempty"`
	LastProbeStatus     string               `json:"last_probe_status,omitempty"`
	LastProbeReason     string               `json:"last_probe_reason,omitempty"`
	LastProbeSummary    adaptiveProbeSummary `json:"last_probe_summary"`
	NextProbeAt         string               `json:"next_probe_at,omitempty"`
	ExitBlockedReason   string               `json:"exit_blocked_reason,omitempty"`
}

func (h *Handler) adaptiveProbeQueueSnapshot() []adaptiveProbeQueueItem {
	if h == nil {
		return nil
	}
	h.adaptiveProbeMu.Lock()
	defer h.adaptiveProbeMu.Unlock()

	out := make([]adaptiveProbeQueueItem, 0, len(h.adaptiveProbes))
	for _, state := range h.adaptiveProbes {
		if state == nil {
			continue
		}
		out = append(out, adaptiveProbeQueueItem{
			Prefix:              state.Prefix,
			Running:             state.Running,
			ConsecutiveFailures: state.ConsecutiveFailures,
			LastFailureAt:       formatAdaptiveProbeTime(state.LastFailureAt),
			LastFailureStatus:   state.LastFailureStatus,
			LastFailureModel:    state.LastFailureModel,
			LastFailureReason:   state.LastFailureReason,
			LastProbeAt:         formatAdaptiveProbeTime(state.LastProbeAt),
			LastProbeUpdatedAt:  formatAdaptiveProbeTime(state.LastProbeUpdatedAt),
			LastProbeStatus:     state.LastProbeStatus,
			LastProbeReason:     state.LastProbeReason,
			LastProbeSummary:    state.LastProbeSummary,
			NextProbeAt:         formatAdaptiveProbeTime(state.NextProbeAt),
			ExitBlockedReason:   adaptiveProbeBlockedReason(state),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out
}

func adaptiveProbeBlockedReason(state *adaptiveProbeState) string {
	if state == nil {
		return ""
	}
	if state.Running {
		return "probe_in_progress"
	}
	switch strings.ToLower(strings.TrimSpace(state.LastProbeStatus)) {
	case "red":
		if state.LastProbeReason != "" {
			return "last_probe_red: " + state.LastProbeReason
		}
		return "last_probe_red"
	case "yellow":
		if state.LastProbeReason != "" {
			return "last_probe_yellow: " + state.LastProbeReason
		}
		return "last_probe_yellow"
	case "green":
		return "awaiting_queue_cleanup"
	default:
		if !state.NextProbeAt.IsZero() {
			return "awaiting_next_probe"
		}
		return ""
	}
}

func formatAdaptiveProbeTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

func (h *Handler) mergeAdaptiveProbeEntry(entry *adaptiveProbeSnapshotEntry) error {
	if h == nil || entry == nil {
		return fmt.Errorf("adaptive probe entry is nil")
	}
	if strings.TrimSpace(h.configFilePath) == "" {
		return fmt.Errorf("config file path is empty")
	}
	paths := []string{
		filepath.Join(managementasset.StaticDir(h.configFilePath), "model-health-multi.json"),
		filepath.Join(filepath.Dir(h.configFilePath), "auths", "model-health-multi.json"),
	}
	for _, path := range paths {
		if err := mergeAdaptiveProbeEntryFile(path, *entry); err != nil {
			return err
		}
	}
	return nil
}

func mergeAdaptiveProbeEntryFile(path string, entry adaptiveProbeSnapshotEntry) error {
	snapshot, err := loadAdaptiveProbeSnapshotFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if snapshot.IntervalSec <= 0 {
		snapshot.IntervalSec = 3600
	}
	if entry.IntervalSec > 0 {
		snapshot.IntervalSec = entry.IntervalSec
	}
	entry.ProviderPrefix = strings.ToLower(strings.TrimSpace(entry.ProviderPrefix))
	entry.Summary = adaptiveProbeEntrySummary(entry)

	replaced := false
	key := providerHealthKey(entry.ProviderPrefix, entry.BaseURL)
	for i := range snapshot.Entries {
		current := &snapshot.Entries[i]
		if providerHealthKey(current.ProviderPrefix, current.BaseURL) == key {
			snapshot.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		for i := range snapshot.Entries {
			current := &snapshot.Entries[i]
			if entry.ProviderPrefix != "" && strings.EqualFold(strings.TrimSpace(current.ProviderPrefix), entry.ProviderPrefix) {
				snapshot.Entries[i] = entry
				replaced = true
				break
			}
		}
	}
	if !replaced {
		snapshot.Entries = append(snapshot.Entries, entry)
	}

	sort.Slice(snapshot.Entries, func(i, j int) bool {
		left := providerHealthKey(snapshot.Entries[i].ProviderPrefix, snapshot.Entries[i].BaseURL)
		right := providerHealthKey(snapshot.Entries[j].ProviderPrefix, snapshot.Entries[j].BaseURL)
		return left < right
	})

	snapshot.UpdatedAt = strings.TrimSpace(entry.UpdatedAt)
	if snapshot.UpdatedAt == "" {
		snapshot.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	snapshot.ByModel = make(map[string]map[string]any)
	snapshot.Summary = adaptiveProbeSummary{}
	for i := range snapshot.Entries {
		summary := adaptiveProbeEntrySummary(snapshot.Entries[i])
		snapshot.Entries[i].Summary = summary
		snapshot.Summary.Total += summary.Total
		snapshot.Summary.Green += summary.Green
		snapshot.Summary.Yellow += summary.Yellow
		snapshot.Summary.Red += summary.Red
		for _, model := range snapshot.Entries[i].Models {
			name := strings.TrimSpace(adaptiveProbeStringValue(model["model"]))
			if name == "" {
				continue
			}
			cloned := cloneAdaptiveProbeModel(model)
			snapshot.ByModel[name] = cloned
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func loadAdaptiveProbeSnapshotFile(path string) (adaptiveProbeSnapshotFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return adaptiveProbeSnapshotFile{}, err
	}
	var snapshot adaptiveProbeSnapshotFile
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return adaptiveProbeSnapshotFile{}, err
	}
	return snapshot, nil
}

func cloneAdaptiveProbeModel(model map[string]any) map[string]any {
	if len(model) == 0 {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(model))
	for key, value := range model {
		cloned[key] = value
	}
	return cloned
}

func adaptiveProbeStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return fmt.Sprintf("%v", value)
	}
}
