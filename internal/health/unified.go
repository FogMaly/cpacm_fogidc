package health

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"time"
)

type ProviderBalanceSnapshotItem struct {
	Section     string
	ProviderKey string
	DisplayName string
	Prefix      string
	BaseURL     string
	Status      string
	Kind        string
	Label       string
	Detail      string
	Currency    string
	Amount      float64
	Total       float64
	Used        float64
	UpdatedAt   time.Time
}

type ProbeSignal struct {
	Status    string
	TestedAt  time.Time
	UpdatedAt time.Time
	Reason    string
	Found     bool
	Stale     bool
}

type RuntimeSignal struct {
	SuccessCount      int
	FailureCount      int
	Quota429Count     int
	AvgLatencyMS      float64
	InflightRequests  int
	RecentRPS10       float64
	RecentErrorRate30 float64
	P95LatencyMS30    float64
	CooldownUntil     time.Time
	LastUpdatedAt     time.Time
	Found             bool
}

type RuntimeOutageSignal struct {
	Active   bool
	FailedAt time.Time
	Reason   string
	Found    bool
}

type BalanceSignal struct {
	Status    string
	Kind      string
	Label     string
	Detail    string
	Amount    float64
	Total     float64
	Used      float64
	UpdatedAt time.Time
	Found     bool
}

type LoadSignal struct {
	Active int
	Found  bool
}

type UnifiedSignals struct {
	Probe   ProbeSignal
	Runtime RuntimeSignal
	Outage  RuntimeOutageSignal
	Balance BalanceSignal
	Load    LoadSignal
}

type UnifiedScore struct {
	Score          float64
	Status         string
	ProbeScore     float64
	RuntimeScore   float64
	OutageScore    float64
	BalanceScore   float64
	LoadScore      float64
	Available      bool
	Reasons        []string
	SignalsPresent int
}

type RuntimeOutageSnapshotItem struct {
	Prefix   string
	BaseURL  string
	Provider string
	Label    string
	Model    string
	FailedAt time.Time
	Reason   string
}

type ProbeSnapshot struct {
	UpdatedAt   string       `json:"updated_at"`
	IntervalSec int          `json:"interval_sec"`
	Entries     []ProbeEntry `json:"entries"`
}

type ProbeEntry struct {
	UpdatedAt          string       `json:"updated_at"`
	ProviderPrefix     string       `json:"provider_prefix"`
	BaseURL            string       `json:"base_url"`
	SourceMode         string       `json:"source_mode,omitempty"`
	SupportedProtocols string       `json:"supported_protocols,omitempty"`
	Disabled           bool         `json:"disabled,omitempty"`
	Error              string       `json:"error,omitempty"`
	Models             []ProbeModel `json:"models"`
}

type ProbeModel struct {
	Model                       string `json:"model"`
	UpstreamModel               string `json:"upstream_model"`
	Status                      string `json:"status"`
	Reason                      string `json:"reason"`
	ResponsesContinuationStatus string `json:"responses_continuation_status,omitempty"`
	ResponsesContinuationReason string `json:"responses_continuation_reason,omitempty"`
	TestedAt                    string `json:"tested_at"`
	ResponseModel               string `json:"response_model,omitempty"`
	TextFallbackResponseModel   string `json:"text_fallback_response_model,omitempty"`
}

func LoadProbeSnapshot(path string) (*ProbeSnapshot, error) {
	if strings.TrimSpace(path) == "" {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snapshot ProbeSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func FindProbeSignal(snapshot *ProbeSnapshot, prefix, baseURL, model string, now time.Time) ProbeSignal {
	if snapshot == nil {
		return ProbeSignal{}
	}
	model = canonicalModel(model)
	if model == "" {
		return ProbeSignal{}
	}
	prefix = normalizeKey(prefix)
	baseURL = normalizeKey(baseURL)
	snapshotUpdatedAt := parseTime(snapshot.UpdatedAt)
	stale := false
	if !snapshotUpdatedAt.IsZero() && snapshot.IntervalSec > 0 {
		maxAge := time.Duration(snapshot.IntervalSec*2) * time.Second
		stale = now.Sub(snapshotUpdatedAt) > maxAge
	}
	best := ProbeSignal{}
	bestRank := -1
	for _, entry := range snapshot.Entries {
		entryPrefix := normalizeKey(entry.ProviderPrefix)
		entryBaseURL := normalizeKey(entry.BaseURL)
		if prefix != "" && entryPrefix != "" && prefix != entryPrefix {
			continue
		}
		if prefix == "" && baseURL != "" && entryBaseURL != "" && baseURL != entryBaseURL {
			continue
		}
		for _, probeModel := range entry.Models {
			if canonicalModel(probeModel.UpstreamModel) != model && canonicalModel(probeModel.Model) != model {
				continue
			}
			rank := probeStatusRank(probeModel.Status)
			if rank < bestRank {
				continue
			}
			candidate := ProbeSignal{
				Status:    strings.ToLower(strings.TrimSpace(probeModel.Status)),
				TestedAt:  parseTime(probeModel.TestedAt),
				UpdatedAt: snapshotUpdatedAt,
				Reason:    strings.TrimSpace(probeModel.Reason),
				Found:     true,
				Stale:     stale,
			}
			if rank == bestRank && !candidate.TestedAt.After(best.TestedAt) {
				continue
			}
			best = candidate
			bestRank = rank
		}
	}
	return best
}

func SelectBalanceSignal(items []ProviderBalanceSnapshotItem, prefix, baseURL, provider, label string) BalanceSignal {
	prefix = normalizeKey(prefix)
	baseURL = normalizeKey(baseURL)
	provider = normalizeKey(provider)
	label = normalizeKey(label)
	best := BalanceSignal{}
	bestScore := -1
	bestRank := -1
	for _, item := range items {
		score := balanceIdentityMatchScore(
			prefix,
			baseURL,
			provider,
			label,
			item.Prefix,
			item.BaseURL,
			item.ProviderKey,
			item.DisplayName,
		)
		if score <= 0 {
			continue
		}
		rank := balanceStatusRank(item.Status)
		if score < bestScore {
			continue
		}
		candidate := BalanceSignal{
			Status:    strings.ToLower(strings.TrimSpace(item.Status)),
			Kind:      strings.TrimSpace(item.Kind),
			Label:     strings.TrimSpace(item.Label),
			Detail:    strings.TrimSpace(item.Detail),
			Amount:    item.Amount,
			Total:     item.Total,
			Used:      item.Used,
			UpdatedAt: item.UpdatedAt,
			Found:     true,
		}
		if score == bestScore && rank < bestRank {
			continue
		}
		if score == bestScore && rank == bestRank && !candidate.UpdatedAt.After(best.UpdatedAt) {
			continue
		}
		best = candidate
		bestScore = score
		bestRank = rank
	}
	return best
}

func SelectRuntimeOutageSignal(items []RuntimeOutageSnapshotItem, prefix, baseURL, provider, label, model string, probeUpdatedAt time.Time) RuntimeOutageSignal {
	prefix = normalizeKey(prefix)
	baseURL = normalizeKey(baseURL)
	provider = normalizeKey(provider)
	label = normalizeKey(label)
	model = canonicalModel(model)

	best := RuntimeOutageSignal{}
	bestScore := -1
	for _, item := range items {
		score := runtimeOutageIdentityMatchScore(
			prefix,
			baseURL,
			provider,
			label,
			model,
			item.Prefix,
			item.BaseURL,
			item.Provider,
			item.Label,
			item.Model,
		)
		if score <= 0 {
			continue
		}
		candidate := RuntimeOutageSignal{
			Active:   item.FailedAt.IsZero() || probeUpdatedAt.IsZero() || !probeUpdatedAt.After(item.FailedAt),
			FailedAt: item.FailedAt,
			Reason:   strings.TrimSpace(item.Reason),
			Found:    true,
		}
		if score < bestScore {
			continue
		}
		if score == bestScore {
			if candidate.Active != best.Active {
				if !candidate.Active {
					continue
				}
			} else if !candidate.FailedAt.After(best.FailedAt) {
				continue
			}
		}
		best = candidate
		bestScore = score
	}
	return best
}

func balanceIdentityMatchScore(targetPrefix, targetBaseURL, targetProvider, targetLabel, itemPrefix, itemBaseURL, itemProvider, itemLabel string) int {
	targetPrefix = normalizeKey(targetPrefix)
	targetBaseURL = normalizeKey(targetBaseURL)
	targetProvider = normalizeKey(targetProvider)
	targetLabel = normalizeKey(targetLabel)
	itemPrefix = normalizeKey(itemPrefix)
	itemBaseURL = normalizeKey(itemBaseURL)
	itemProvider = normalizeKey(itemProvider)
	itemLabel = normalizeKey(itemLabel)

	score := 0
	if targetBaseURL != "" && itemBaseURL != "" && targetBaseURL == itemBaseURL {
		score += 8
	}
	if targetLabel != "" && itemLabel != "" && targetLabel == itemLabel {
		score += 4
	}
	if targetProvider != "" && itemProvider != "" && targetProvider == itemProvider {
		score += 3
	}
	if targetPrefix != "" && itemPrefix != "" && targetPrefix == itemPrefix {
		score += 2
	}
	if targetPrefix != "" && targetBaseURL != "" && itemPrefix != "" && itemBaseURL != "" &&
		targetPrefix == itemPrefix && targetBaseURL == itemBaseURL {
		score += 4
	}
	return score
}

func runtimeOutageIdentityMatchScore(targetPrefix, targetBaseURL, targetProvider, targetLabel, targetModel, itemPrefix, itemBaseURL, itemProvider, itemLabel, itemModel string) int {
	targetPrefix = normalizeKey(targetPrefix)
	targetBaseURL = normalizeKey(targetBaseURL)
	targetProvider = normalizeKey(targetProvider)
	targetLabel = normalizeKey(targetLabel)
	targetModel = canonicalModel(targetModel)
	itemPrefix = normalizeKey(itemPrefix)
	itemBaseURL = normalizeKey(itemBaseURL)
	itemProvider = normalizeKey(itemProvider)
	itemLabel = normalizeKey(itemLabel)
	itemModel = canonicalModel(itemModel)

	score := 0
	if targetPrefix != "" && itemPrefix != "" && targetPrefix != itemPrefix {
		return 0
	}
	if targetBaseURL != "" && itemBaseURL != "" && targetBaseURL != itemBaseURL {
		return 0
	}
	if targetProvider != "" && itemProvider != "" && targetProvider != itemProvider {
		return 0
	}
	if targetLabel != "" && itemLabel != "" && targetLabel != itemLabel {
		return 0
	}
	switch {
	case targetModel != "" && itemModel != "" && targetModel == itemModel:
		score += 10
	case targetModel != "" && itemModel == "":
		score += 2
	case targetModel != "" && itemModel != "":
		return 0
	}
	if targetBaseURL != "" && itemBaseURL != "" && targetBaseURL == itemBaseURL {
		score += 8
	}
	if targetLabel != "" && itemLabel != "" && targetLabel == itemLabel {
		score += 4
	}
	if targetProvider != "" && itemProvider != "" && targetProvider == itemProvider {
		score += 3
	}
	if targetPrefix != "" && itemPrefix != "" && targetPrefix == itemPrefix {
		score += 2
	}
	if targetPrefix != "" && targetBaseURL != "" && itemPrefix != "" && itemBaseURL != "" &&
		targetPrefix == itemPrefix && targetBaseURL == itemBaseURL {
		score += 4
	}
	return score
}

func Score(signals UnifiedSignals, now time.Time) UnifiedScore {
	out := UnifiedScore{
		Available: true,
		Status:    "unknown",
	}
	type component struct {
		score  float64
		weight float64
		name   string
	}
	parts := make([]component, 0, 4)

	if signals.Probe.Found {
		score := scoreProbe(signals.Probe, now)
		out.ProbeScore = score
		parts = append(parts, component{score: score, weight: 0.45, name: "probe"})
		out.Reasons = appendReason(out.Reasons, probeReason(signals.Probe))
		out.SignalsPresent++
	}
	if signals.Runtime.Found {
		score := scoreRuntime(signals.Runtime)
		out.RuntimeScore = score
		parts = append(parts, component{score: score, weight: 0.35, name: "runtime"})
		out.Reasons = appendReason(out.Reasons, runtimeReason(signals.Runtime))
		out.SignalsPresent++
	}
	if signals.Outage.Found && signals.Outage.Active {
		score := scoreRuntimeOutage(signals.Outage)
		out.OutageScore = score
		parts = append(parts, component{score: score, weight: 0.60, name: "outage"})
		out.Reasons = appendReason(out.Reasons, runtimeOutageReason(signals.Outage))
		out.SignalsPresent++
	}
	if signals.Balance.Found {
		score := scoreBalance(signals.Balance)
		out.BalanceScore = score
		parts = append(parts, component{score: score, weight: 0.15, name: "balance"})
		out.Reasons = appendReason(out.Reasons, balanceReason(signals.Balance))
		out.SignalsPresent++
	}
	if signals.Load.Found {
		score := scoreLoad(signals.Load)
		out.LoadScore = score
		parts = append(parts, component{score: score, weight: 0.05, name: "load"})
		out.Reasons = appendReason(out.Reasons, loadReason(signals.Load))
		out.SignalsPresent++
	}

	totalWeight := 0.0
	totalScore := 0.0
	for _, part := range parts {
		totalWeight += part.weight
		totalScore += part.score * part.weight
	}
	if totalWeight > 0 {
		out.Score = totalScore / totalWeight
	} else {
		out.Score = 50
	}

	switch {
	case signals.Outage.Found && signals.Outage.Active:
		out.Available = false
		out.Status = "unavailable"
		out.Score = math.Min(out.Score, 5)
	case signals.Probe.Found && probeStatusRank(signals.Probe.Status) <= 1 && !signals.Runtime.Found:
		out.Available = false
		out.Status = "unavailable"
		out.Score = math.Min(out.Score, 15)
	case out.SignalsPresent == 0:
		out.Status = "unknown"
	case out.Score >= 75:
		out.Status = "healthy"
	case out.Score >= 35:
		out.Status = "degraded"
	default:
		out.Status = "unavailable"
		if signals.Runtime.Found || signals.Probe.Found {
			out.Available = false
		}
	}
	return out
}

func scoreProbe(signal ProbeSignal, now time.Time) float64 {
	score := 50.0
	switch probeStatusRank(signal.Status) {
	case 3:
		score = 100
	case 2:
		score = 68
	case 1:
		score = 8
	default:
		score = 45
	}
	if signal.Stale {
		score *= 0.55
	}
	if !signal.TestedAt.IsZero() {
		ageMin := now.Sub(signal.TestedAt).Minutes()
		switch {
		case ageMin > 180:
			score -= 20
		case ageMin > 60:
			score -= 10
		case ageMin > 15:
			score -= 4
		}
	}
	return clampScore(score)
}

func scoreRuntime(signal RuntimeSignal) float64 {
	total := signal.SuccessCount + signal.FailureCount
	successRate := 1.0
	if total > 0 {
		successRate = float64(signal.SuccessCount) / float64(total)
	}
	score := successRate * 100
	score -= float64(signal.Quota429Count * 8)
	if signal.AvgLatencyMS > 0 {
		score -= math.Min(signal.AvgLatencyMS/300.0, 25)
	}
	if signal.InflightRequests > 0 {
		score -= math.Min(float64(signal.InflightRequests)*5, 24)
	}
	if signal.RecentErrorRate30 > 0 {
		score -= math.Min(signal.RecentErrorRate30*100*0.6, 35)
	}
	if signal.P95LatencyMS30 > 0 {
		score -= math.Min(signal.P95LatencyMS30/250.0, 28)
	}
	if !signal.CooldownUntil.IsZero() && signal.CooldownUntil.After(time.Now().UTC()) {
		score *= 0.25
	}
	return clampScore(score)
}

func scoreRuntimeOutage(signal RuntimeOutageSignal) float64 {
	if signal.Active {
		return 0
	}
	return 100
}

func scoreBalance(signal BalanceSignal) float64 {
	status := strings.ToLower(strings.TrimSpace(signal.Status))
	score := 50.0
	switch status {
	case "ok":
		score = 90
	case "partial":
		score = 75
	case "warning":
		score = 38
	case "error":
		score = 20
	case "unsupported":
		score = 100
	}
	if signal.Total > 0 {
		ratio := signal.Amount / signal.Total
		switch {
		case ratio <= 0.05:
			score = math.Min(score, 20)
		case ratio <= 0.15:
			score = math.Min(score, 35)
		case ratio <= 0.3:
			score = math.Min(score, 55)
		}
	}
	return clampScore(score)
}

func scoreLoad(signal LoadSignal) float64 {
	if signal.Active <= 0 {
		return 100
	}
	return clampScore(100 - float64(signal.Active*12))
}

func probeReason(signal ProbeSignal) string {
	if !signal.Found {
		return ""
	}
	status := strings.ToLower(strings.TrimSpace(signal.Status))
	if signal.Stale {
		return "probe snapshot stale"
	}
	if signal.Reason != "" {
		return "probe " + status + ": " + signal.Reason
	}
	return "probe " + status
}

func runtimeReason(signal RuntimeSignal) string {
	if !signal.Found {
		return ""
	}
	if !signal.CooldownUntil.IsZero() && signal.CooldownUntil.After(time.Now().UTC()) {
		return "runtime cooldown active"
	}
	if signal.Quota429Count > 0 {
		return "runtime quota pressure"
	}
	if signal.RecentErrorRate30 >= 0.2 {
		return "runtime short-window errors elevated"
	}
	if signal.FailureCount > signal.SuccessCount {
		return "runtime failures elevated"
	}
	if signal.P95LatencyMS30 > 1800 {
		return "runtime p95 latency elevated"
	}
	if signal.AvgLatencyMS > 1800 {
		return "runtime latency elevated"
	}
	if signal.InflightRequests >= 4 {
		return "runtime load elevated"
	}
	return ""
}

func runtimeOutageReason(signal RuntimeOutageSignal) string {
	if !signal.Found || !signal.Active {
		return ""
	}
	if signal.Reason != "" {
		return "runtime outage: " + signal.Reason
	}
	return "runtime outage"
}

func balanceReason(signal BalanceSignal) string {
	if !signal.Found {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(signal.Status)) {
	case "warning":
		if signal.Label != "" {
			return "balance warning: " + signal.Label
		}
		return "balance warning"
	case "error":
		if signal.Label != "" {
			return "balance error: " + signal.Label
		}
		return "balance error"
	default:
		return ""
	}
}

func loadReason(signal LoadSignal) string {
	if !signal.Found || signal.Active <= 0 {
		return ""
	}
	if signal.Active >= 3 {
		return "active load high"
	}
	return ""
}

func canonicalModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if idx := strings.Index(model, "("); idx > 0 {
		model = strings.TrimSpace(model[:idx])
	}
	if parts := strings.SplitN(model, "/", 2); len(parts) == 2 {
		model = strings.TrimSpace(parts[1])
	}
	return strings.ToLower(model)
}

// CanonicalModel returns a stable model key suitable for comparisons across
// requested/upstream/response model fields.
func CanonicalModel(model string) string {
	return canonicalModel(model)
}

func parseTime(raw string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	return parsed
}

func normalizeKey(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func probeStatusRank(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "green":
		return 3
	case "yellow":
		return 2
	case "red":
		return 1
	default:
		return 0
	}
}

func balanceStatusRank(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok":
		return 4
	case "partial":
		return 3
	case "warning":
		return 2
	case "unsupported":
		return 1
	case "error":
		return 0
	default:
		return 1
	}
}

func clampScore(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

func appendReason(dst []string, reason string) []string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return dst
	}
	for _, existing := range dst {
		if existing == reason {
			return dst
		}
	}
	return append(dst, reason)
}
