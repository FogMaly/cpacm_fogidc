package management

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type providerTrafficSummary struct {
	Requests   int64  `json:"requests"`
	Success    int64  `json:"success"`
	Failure    int64  `json:"failure"`
	Tokens     int64  `json:"tokens"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

type providerHealthSummary struct {
	Status                string   `json:"status"`
	UpdatedAt             string   `json:"updated_at,omitempty"`
	TestedAt              string   `json:"tested_at,omitempty"`
	IsStale               bool     `json:"is_stale"`
	Score                 float64  `json:"score"`
	Reason                string   `json:"reason,omitempty"`
	Reasons               []string `json:"reasons,omitempty"`
	RequestedModel        string   `json:"requested_model,omitempty"`
	ResponseModel         string   `json:"response_model,omitempty"`
	ResponseModelMismatch bool     `json:"response_model_mismatch,omitempty"`
}

type providerBalanceSummary struct {
	Status    string  `json:"status,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	Label     string  `json:"label,omitempty"`
	Detail    string  `json:"detail,omitempty"`
	Currency  string  `json:"currency,omitempty"`
	Amount    float64 `json:"amount,omitempty"`
	Total     float64 `json:"total,omitempty"`
	Used      float64 `json:"used,omitempty"`
	UpdatedAt string  `json:"updated_at,omitempty"`
}

type providerHealthModel struct {
	Model                 string                 `json:"model"`
	UpstreamModel         string                 `json:"upstream_model,omitempty"`
	RequestedModel        string                 `json:"requested_model,omitempty"`
	ResponseModel         string                 `json:"response_model,omitempty"`
	ResponseModelMismatch bool                   `json:"response_model_mismatch,omitempty"`
	Status                string                 `json:"status"`
	TestedAt              string                 `json:"tested_at,omitempty"`
	Error                 string                 `json:"error,omitempty"`
	Score                 float64                `json:"score"`
	Reasons               []string               `json:"reasons,omitempty"`
	Traffic               providerTrafficSummary `json:"traffic"`
	Health                providerHealthSummary  `json:"health"`
}

type providerHealthItem struct {
	Prefix             string                  `json:"prefix"`
	DisplayName        string                  `json:"display_name,omitempty"`
	CapabilityNote     string                  `json:"capability_note,omitempty"`
	BaseURL            string                  `json:"base_url,omitempty"`
	SupportedProtocols string                  `json:"supported_protocols,omitempty"`
	SourceMode         string                  `json:"source_mode,omitempty"`
	Disabled           bool                    `json:"disabled,omitempty"`
	UpdatedAt          string                  `json:"updated_at,omitempty"`
	IsStale            bool                    `json:"is_stale"`
	Total              int                     `json:"total"`
	Healthy            int                     `json:"healthy"`
	Degraded           int                     `json:"degraded"`
	Unavailable        int                     `json:"unavailable"`
	Unknown            int                     `json:"unknown"`
	OverallStatus      string                  `json:"overall_status"`
	Category           string                  `json:"category"`
	Score              float64                 `json:"score"`
	Reasons            []string                `json:"reasons,omitempty"`
	Models             []providerHealthModel   `json:"models"`
	Traffic            providerTrafficSummary  `json:"traffic"`
	Health             providerHealthSummary   `json:"health"`
	Balance            *providerBalanceItem    `json:"balance,omitempty"`
	BalanceSummary     *providerBalanceSummary `json:"balance_summary,omitempty"`
}

type providerHealthPayload struct {
	GeneratedAt string                `json:"generated_at"`
	Items       []providerHealthItem  `json:"items"`
	History     providerHealthHistory `json:"history,omitempty"`
}

type providerHealthMeta struct {
	Prefix             string
	DisplayName        string
	BaseURL            string
	SupportedProtocols string
	SourceMode         string
	Disabled           bool
	Category           string
	ProviderKey        string
	APIKeys            []string
	ModelNames         []string
}

func (h *Handler) GetProviderHealth(c *gin.Context) {
	items := h.collectProviderHealth(c.Request.Context())
	history := h.updateProviderHealthHistory(items)
	c.JSON(http.StatusOK, providerHealthPayload{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Items:       items,
		History:     history,
	})
}

func (h *Handler) collectProviderHealth(ctx context.Context) []providerHealthItem {
	metas := h.providerHealthMetas()

	balanceRequests, balanceItems := h.collectProviderBalanceResultsCached(ctx)
	balanceSnapshot := providerBalanceSnapshotFromResults(balanceRequests, balanceItems)
	usageSnapshot := usage.StatisticsSnapshot{}
	if h != nil && h.usageStats != nil {
		usageSnapshot = h.usageStats.Snapshot()
	}
	usageSnapshot, _ = usage.SnapshotWithPersistedDetails(usageSnapshot, time.Now().UTC(), 24*time.Hour)

	var runtimeSnapshot []coreauth.AuthRuntimeSnapshot
	if h != nil && h.authManager != nil {
		runtimeSnapshot = h.authManager.RuntimeHealthSnapshot()
	}

	snapshotPath := filepath.Join(managementasset.StaticDir(h.configFilePath), "model-health-multi.json")
	probeSnapshot, _ := health.LoadProbeSnapshot(snapshotPath)
	now := time.Now().UTC()

	out := make([]providerHealthItem, 0, len(metas)+len(probeSnapshotEntries(probeSnapshot)))
	matchedProbeKeys := make(map[string]struct{})

	for _, meta := range metas {
		entry := findProbeEntry(probeSnapshot, meta.Prefix, meta.BaseURL, providerHealthKey(meta.Prefix, meta.BaseURL))
		if entry != nil {
			matchedProbeKeys[providerHealthKey(entry.ProviderPrefix, entry.BaseURL)] = struct{}{}
		}
		meta = normalizeProviderHealthMeta(meta, entry)
		out = append(out, buildProviderHealthItem(meta, entry, runtimeSnapshot, balanceRequests, balanceItems, balanceSnapshot, usageSnapshot, now))
	}

	for _, entry := range probeSnapshotEntries(probeSnapshot) {
		key := providerHealthKey(entry.ProviderPrefix, entry.BaseURL)
		if _, ok := matchedProbeKeys[key]; ok {
			continue
		}
		meta := normalizeProviderHealthMeta(providerHealthMeta{}, &entry)
		out = append(out, buildProviderHealthItem(meta, &entry, runtimeSnapshot, balanceRequests, balanceItems, balanceSnapshot, usageSnapshot, now))
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Category == out[j].Category {
			if out[i].Score == out[j].Score {
				if out[i].Prefix == out[j].Prefix {
					return out[i].DisplayName < out[j].DisplayName
				}
				return out[i].Prefix < out[j].Prefix
			}
			return out[i].Score > out[j].Score
		}
		return providerCategoryOrder(out[i].Category) < providerCategoryOrder(out[j].Category)
	})
	return out
}

func buildProviderHealthItem(
	meta providerHealthMeta,
	entry *health.ProbeEntry,
	runtimeSnapshot []coreauth.AuthRuntimeSnapshot,
	balanceRequests []providerBalanceRequest,
	balanceItems []providerBalanceItem,
	balanceSnapshot []health.ProviderBalanceSnapshotItem,
	usageSnapshot usage.StatisticsSnapshot,
	now time.Time,
) providerHealthItem {
	disabled := meta.Disabled || (entry != nil && entry.Disabled)
	if disabled {
		updatedAt := time.Time{}
		if entry != nil {
			updatedAt = parseHealthTime(entry.UpdatedAt)
		}
		reason := "provider_disabled"
		if entry != nil && strings.TrimSpace(entry.Error) != "" {
			reason = strings.TrimSpace(entry.Error)
		}
		reasons := []string{reason}
		displayName := providerHealthDisplayName(meta)
		capabilityNote := providerCapabilityNote(meta)
		balanceItem := selectProviderBalanceItem(balanceRequests, balanceItems, meta)
		return providerHealthItem{
			Prefix:             meta.Prefix,
			DisplayName:        displayName,
			CapabilityNote:     capabilityNote,
			BaseURL:            meta.BaseURL,
			SupportedProtocols: meta.SupportedProtocols,
			SourceMode:         meta.SourceMode,
			Disabled:           true,
			UpdatedAt:          formatHealthTime(updatedAt),
			IsStale:            false,
			Total:              0,
			Healthy:            0,
			Degraded:           0,
			Unavailable:        0,
			Unknown:            0,
			OverallStatus:      "unknown",
			Category:           meta.Category,
			Score:              0,
			Reasons:            reasons,
			Models:             nil,
			Traffic:            providerTrafficSummary{},
			Health: providerHealthSummary{
				Status:    "unknown",
				UpdatedAt: formatHealthTime(updatedAt),
				IsStale:   false,
				Score:     0,
				Reason:    reason,
				Reasons:   reasons,
			},
			Balance:        balanceItem,
			BalanceSummary: buildProviderBalanceSummary(balanceItem),
		}
	}

	runtimeSignal := aggregateRuntimeSignals(runtimeSnapshot, meta)
	balanceSignal := health.SelectBalanceSignal(balanceSnapshot, meta.Prefix, meta.BaseURL, meta.ProviderKey, meta.DisplayName)
	balanceItem := selectProviderBalanceItem(balanceRequests, balanceItems, meta)
	if balanceItem != nil {
		balanceSignal = providerBalanceSignal(*balanceItem)
	}
	if strings.EqualFold(balanceSignal.Status, "error") {
		balanceSignal = health.BalanceSignal{}
	}

	providerTraffic, modelTraffic := buildProviderTraffic(meta, entry, usageSnapshot)
	models := buildProviderHealthModels(entry, runtimeSignal, balanceSignal, modelTraffic, now)
	counts := countProviderHealthStatuses(models)
	overallScore, overallStatus, reasons, updatedAt, isStale := summarizeProviderHealth(models, runtimeSignal, balanceSignal, entry, now)
	if len(models) == 0 {
		providerScore := health.Score(health.UnifiedSignals{
			Runtime: runtimeSignal,
			Balance: balanceSignal,
		}, now)
		overallScore = providerScore.Score
		overallStatus = normalizeProviderOverallDisplayStatus(providerScore.Status)
		reasons = providerScore.Reasons
		if balanceSignal.Found && updatedAt.IsZero() {
			updatedAt = balanceSignal.UpdatedAt
		}
	}
	overallStatus = normalizeProviderOverallDisplayStatus(overallStatus)
	healthSummary := providerHealthSummary{
		Status:    overallStatus,
		UpdatedAt: formatHealthTime(updatedAt),
		IsStale:   isStale,
		Score:     overallScore,
		Reason:    firstHealthReason(reasons),
		Reasons:   reasons,
	}

	displayName := providerHealthDisplayName(meta)
	capabilityNote := providerCapabilityNote(meta)

	return providerHealthItem{
		Prefix:             meta.Prefix,
		DisplayName:        displayName,
		CapabilityNote:     capabilityNote,
		BaseURL:            meta.BaseURL,
		SupportedProtocols: meta.SupportedProtocols,
		SourceMode:         meta.SourceMode,
		Disabled:           false,
		UpdatedAt:          formatHealthTime(updatedAt),
		IsStale:            isStale,
		Total:              len(models),
		Healthy:            counts["healthy"],
		Degraded:           counts["degraded"],
		Unavailable:        counts["unavailable"],
		Unknown:            counts["unknown"],
		OverallStatus:      overallStatus,
		Category:           meta.Category,
		Score:              overallScore,
		Reasons:            reasons,
		Models:             models,
		Traffic:            providerTraffic,
		Health:             healthSummary,
		Balance:            balanceItem,
		BalanceSummary:     buildProviderBalanceSummary(balanceItem),
	}
}

func normalizeProviderHealthMeta(meta providerHealthMeta, entry *health.ProbeEntry) providerHealthMeta {
	if meta.Prefix == "" && entry != nil {
		meta.Prefix = strings.TrimSpace(entry.ProviderPrefix)
	}
	if meta.BaseURL == "" && entry != nil {
		meta.BaseURL = strings.TrimSpace(entry.BaseURL)
	}
	if meta.SupportedProtocols == "" && entry != nil {
		meta.SupportedProtocols = strings.TrimSpace(entry.SupportedProtocols)
	}
	if meta.SourceMode == "" && entry != nil {
		meta.SourceMode = strings.TrimSpace(entry.SourceMode)
	}
	if entry != nil && entry.Disabled {
		meta.Disabled = true
	}
	if meta.DisplayName == "" {
		meta.DisplayName = firstNonEmpty(meta.Prefix, meta.BaseURL, meta.ProviderKey, "provider")
	}
	if meta.SourceMode == "" {
		meta.SourceMode = firstNonEmpty(meta.ProviderKey, "probe")
	}
	if meta.Category == "" {
		meta.Category = "other"
	}
	meta.APIKeys = trimNonEmptyUnique(meta.APIKeys)
	meta.ModelNames = trimNonEmptyUnique(meta.ModelNames)
	return meta
}

func providerHealthDisplayName(meta providerHealthMeta) string {
	displayName := strings.TrimSpace(meta.DisplayName)
	if !isNowcodingProviderMeta(meta) {
		return displayName
	}
	if displayName == "" {
		displayName = firstNonEmpty(meta.Prefix, meta.BaseURL, meta.ProviderKey, "provider")
	}
	if strings.Contains(strings.ToLower(displayName), "gpt-5.4 only") {
		return displayName
	}
	return displayName + " · gpt-5.4 only"
}

func providerCapabilityNote(meta providerHealthMeta) string {
	return ""
}

func isNowcodingProviderMeta(meta providerHealthMeta) bool {
	fingerprint := strings.ToLower(strings.Join([]string{
		strings.TrimSpace(meta.Prefix),
		strings.TrimSpace(meta.DisplayName),
		strings.TrimSpace(meta.BaseURL),
		strings.Join(meta.ModelNames, " "),
	}, " "))
	return strings.Contains(fingerprint, "nowcoding") || strings.Contains(fingerprint, "nowcoding.ai")
}

func selectProviderBalanceItem(
	requests []providerBalanceRequest,
	items []providerBalanceItem,
	meta providerHealthMeta,
) *providerBalanceItem {
	bestIndex := -1
	bestScore := -1
	bestStatusRank := -1
	bestUpdatedAt := time.Time{}

	for i := range items {
		score := providerBalanceMatchScore(meta, balanceRequestAt(requests, i), items[i])
		if score <= 0 {
			continue
		}

		statusRank := providerBalanceStatusRank(items[i].Status)
		updatedAt := parseBalanceUpdatedAt(items[i].UpdatedAt)

		if score < bestScore {
			continue
		}
		if score == bestScore && statusRank < bestStatusRank {
			continue
		}
		if score == bestScore && statusRank == bestStatusRank && !updatedAt.After(bestUpdatedAt) {
			continue
		}

		bestIndex = i
		bestScore = score
		bestStatusRank = statusRank
		bestUpdatedAt = updatedAt
	}

	if bestIndex < 0 {
		return nil
	}
	item := items[bestIndex]
	return &item
}

func balanceRequestAt(requests []providerBalanceRequest, index int) providerBalanceRequest {
	if index >= 0 && index < len(requests) {
		return requests[index]
	}
	return providerBalanceRequest{}
}

func providerBalanceMatchScore(meta providerHealthMeta, req providerBalanceRequest, item providerBalanceItem) int {
	metaPrefix := strings.ToLower(strings.TrimSpace(meta.Prefix))
	metaBaseURL := strings.ToLower(strings.TrimSpace(meta.BaseURL))
	metaDisplayName := strings.ToLower(strings.TrimSpace(meta.DisplayName))
	metaProviderKey := strings.ToLower(strings.TrimSpace(meta.ProviderKey))

	requestPrefix := strings.ToLower(strings.TrimSpace(req.Prefix))
	requestBaseURL := strings.ToLower(strings.TrimSpace(req.BaseURL))
	requestDisplayName := strings.ToLower(strings.TrimSpace(firstNonEmpty(req.DisplayName, item.DisplayName)))
	requestProviderKey := strings.ToLower(strings.TrimSpace(firstNonEmpty(req.ProviderKey, item.ProviderKey)))

	score := 0
	if metaBaseURL != "" && requestBaseURL != "" && metaBaseURL == requestBaseURL {
		score += 8
	}
	if metaDisplayName != "" && requestDisplayName != "" && metaDisplayName == requestDisplayName {
		score += 4
	}
	if metaProviderKey != "" && requestProviderKey != "" && metaProviderKey == requestProviderKey {
		score += 3
	}
	if metaPrefix != "" && requestPrefix != "" && metaPrefix == requestPrefix {
		score += 2
	}
	if metaPrefix != "" && metaBaseURL != "" && requestPrefix != "" && requestBaseURL != "" &&
		metaPrefix == requestPrefix && metaBaseURL == requestBaseURL {
		score += 4
	}
	return score
}

func providerBalanceStatusRank(status string) int {
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

func providerBalanceSignal(item providerBalanceItem) health.BalanceSignal {
	return health.BalanceSignal{
		Status:    strings.ToLower(strings.TrimSpace(item.Status)),
		Kind:      strings.TrimSpace(item.Kind),
		Label:     strings.TrimSpace(item.Label),
		Detail:    strings.TrimSpace(item.Detail),
		Amount:    item.Amount,
		Total:     item.Total,
		Used:      item.Used,
		UpdatedAt: parseBalanceUpdatedAt(item.UpdatedAt),
		Found:     true,
	}
}

func (h *Handler) providerHealthMetas() []providerHealthMeta {
	if h == nil || h.cfg == nil {
		return nil
	}
	out := make([]providerHealthMeta, 0)
	add := func(prefix, displayName, baseURL, supportedProtocols, sourceMode string, disabled bool, category, providerKey string, apiKeys []string, modelNames []string) {
		out = append(out, providerHealthMeta{
			Prefix:             strings.TrimSpace(prefix),
			DisplayName:        strings.TrimSpace(displayName),
			BaseURL:            strings.TrimSpace(baseURL),
			SupportedProtocols: strings.TrimSpace(supportedProtocols),
			SourceMode:         strings.TrimSpace(sourceMode),
			Disabled:           disabled,
			Category:           strings.TrimSpace(category),
			ProviderKey:        strings.TrimSpace(providerKey),
			APIKeys:            trimNonEmptyUnique(apiKeys),
			ModelNames:         trimNonEmptyUnique(modelNames),
		})
	}
	for i, entry := range h.cfg.CodexKey {
		add(entry.Prefix, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "codex", i), entry.BaseURL, entry.SupportedProtocols, "codex", providerConfigDisabled(entry.ExcludedModels), "codex", "codex", []string{entry.APIKey}, providerConfigModelNames(entry.Models))
	}
	for i, entry := range h.cfg.ClaudeKey {
		add(entry.Prefix, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "claude", i), entry.BaseURL, "", "claude", providerConfigDisabled(entry.ExcludedModels), "claude", "claude", []string{entry.APIKey}, providerConfigModelNames(entry.Models))
	}
	for i, entry := range h.cfg.GeminiKey {
		add(entry.Prefix, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "gemini", i), entry.BaseURL, "", "gemini", providerConfigDisabled(entry.ExcludedModels), "gemini", "gemini", []string{entry.APIKey}, providerConfigModelNames(entry.Models))
	}
	for i, entry := range h.cfg.VertexCompatAPIKey {
		add(entry.Prefix, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "vertex", i), entry.BaseURL, "", "vertex", false, "vertex", "vertex", []string{entry.APIKey}, providerConfigModelNames(entry.Models))
	}
	nvidiaEntries, compatEntries := splitOpenAICompatByNVIDIA(h.cfg.OpenAICompatibility)
	for i, entry := range compatEntries {
		add(entry.Prefix, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "compat", i), entry.BaseURL, "", "openai-compat", false, classifyCompatCategory(entry), compatProviderKey(entry), providerCompatAPIKeys(entry.APIKeyEntries), providerConfigModelNames(entry.Models))
	}
	for i, entry := range nvidiaEntries {
		add(entry.Prefix, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "nvidia", i), entry.BaseURL, "", "openai-compat", false, "other", compatProviderKey(entry), providerCompatAPIKeys(entry.APIKeyEntries), providerConfigModelNames(entry.Models))
	}
	return out
}

func providerConfigDisabled(excluded []string) bool {
	for _, item := range excluded {
		if strings.TrimSpace(item) == "*" {
			return true
		}
	}
	return false
}

func buildProviderHealthModels(entry *health.ProbeEntry, runtime health.RuntimeSignal, balance health.BalanceSignal, modelTraffic map[string]providerTrafficSummary, now time.Time) []providerHealthModel {
	if entry == nil {
		return nil
	}
	models := make([]providerHealthModel, 0, len(entry.Models))
	for _, model := range entry.Models {
		requestedModel := strings.TrimSpace(firstNonEmpty(model.Model, model.UpstreamModel))
		responseModel := strings.TrimSpace(firstNonEmpty(model.ResponseModel, model.TextFallbackResponseModel))
		probeStatus, responseModelMismatch, probeReason := normalizeProbeModelHealth(
			model.Status,
			model.Reason,
			requestedModel,
			responseModel,
		)
		probe := health.ProbeSignal{
			Status:    probeStatus,
			TestedAt:  parseHealthTime(model.TestedAt),
			UpdatedAt: parseHealthTime(entry.UpdatedAt),
			Reason:    probeReason,
			Found:     true,
		}
		if probe.UpdatedAt.IsZero() {
			probe.UpdatedAt = probe.TestedAt
		}
		score := health.Score(health.UnifiedSignals{
			Probe:   probe,
			Runtime: runtime,
			Balance: balance,
		}, now)
		displayStatus := providerModelDisplayStatus(score.Status, probeStatus)
		trafficSummary := lookupProviderModelTraffic(modelTraffic, requestedModel, model.Model)
		healthSummary := providerHealthSummary{
			Status:                displayStatus,
			UpdatedAt:             formatHealthTime(probe.UpdatedAt),
			TestedAt:              formatHealthTime(probe.TestedAt),
			IsStale:               !probe.UpdatedAt.IsZero() && now.Sub(probe.UpdatedAt) > 2*time.Hour,
			Score:                 score.Score,
			Reason:                strings.TrimSpace(probeReason),
			Reasons:               score.Reasons,
			RequestedModel:        requestedModel,
			ResponseModel:         responseModel,
			ResponseModelMismatch: responseModelMismatch,
		}
		models = append(models, providerHealthModel{
			Model:                 strings.TrimSpace(model.Model),
			UpstreamModel:         strings.TrimSpace(model.UpstreamModel),
			RequestedModel:        requestedModel,
			ResponseModel:         responseModel,
			ResponseModelMismatch: responseModelMismatch,
			Status:                displayStatus,
			TestedAt:              formatHealthTime(probe.TestedAt),
			Error:                 strings.TrimSpace(probeReason),
			Score:                 score.Score,
			Reasons:               score.Reasons,
			Traffic:               trafficSummary,
			Health:                healthSummary,
		})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Score == models[j].Score {
			return models[i].Model < models[j].Model
		}
		return models[i].Score > models[j].Score
	})
	return models
}

func normalizeProviderModelDisplayStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "healthy", "degraded":
		return "healthy"
	default:
		return "unavailable"
	}
}

func providerModelDisplayStatus(scoreStatus, probeStatus string) string {
	if isUnavailableProbeStatus(probeStatus) {
		return "unavailable"
	}
	return normalizeProviderModelDisplayStatus(scoreStatus)
}

func isUnavailableProbeStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "red", "error", "failed", "fail", "down", "unavailable":
		return true
	default:
		return false
	}
}

type providerTrafficAccumulator struct {
	Requests int64
	Success  int64
	Failure  int64
	Tokens   int64
	LastSeen time.Time
}

func (a *providerTrafficAccumulator) add(detail usage.RequestDetail) {
	a.Requests++
	if detail.Failed {
		a.Failure++
	} else {
		a.Success++
	}
	a.Tokens += detail.Tokens.TotalTokens
	if detail.Timestamp.After(a.LastSeen) {
		a.LastSeen = detail.Timestamp
	}
}

func (a providerTrafficAccumulator) summary() providerTrafficSummary {
	return providerTrafficSummary{
		Requests:   a.Requests,
		Success:    a.Success,
		Failure:    a.Failure,
		Tokens:     a.Tokens,
		LastSeenAt: formatHealthTime(a.LastSeen.UTC()),
	}
}

func buildProviderTraffic(meta providerHealthMeta, entry *health.ProbeEntry, snapshot usage.StatisticsSnapshot) (providerTrafficSummary, map[string]providerTrafficSummary) {
	if len(snapshot.APIs) == 0 {
		return providerTrafficSummary{}, map[string]providerTrafficSummary{}
	}

	providerAcc := providerTrafficAccumulator{}
	modelAcc := make(map[string]*providerTrafficAccumulator)
	for apiName, apiSnapshot := range snapshot.APIs {
		for modelName, modelSnapshot := range apiSnapshot.Models {
			for _, detail := range modelSnapshot.Details {
				if !providerUsageMatches(meta, apiName, detail) {
					continue
				}
				providerAcc.add(detail)
				key := normalizeProviderHealthModelKey(modelName)
				acc := modelAcc[key]
				if acc == nil {
					acc = &providerTrafficAccumulator{}
					modelAcc[key] = acc
				}
				acc.add(detail)
			}
		}
	}

	out := make(map[string]providerTrafficSummary, len(modelAcc)+len(meta.ModelNames)+len(entryModelNames(entry)))
	for _, modelName := range append(append([]string{}, meta.ModelNames...), entryModelNames(entry)...) {
		key := normalizeProviderHealthModelKey(modelName)
		if _, ok := out[key]; !ok {
			out[key] = providerTrafficSummary{}
		}
	}
	for key, acc := range modelAcc {
		out[key] = acc.summary()
	}
	return providerAcc.summary(), out
}

func providerUsageMatches(meta providerHealthMeta, apiName string, detail usage.RequestDetail) bool {
	apiKey := strings.ToLower(strings.TrimSpace(apiName))
	source := strings.ToLower(strings.TrimSpace(detail.Source))
	prefix := strings.ToLower(strings.TrimSpace(meta.Prefix))
	displayName := strings.ToLower(strings.TrimSpace(meta.DisplayName))

	if prefix != "" && (source == prefix || apiKey == prefix) {
		return true
	}
	if displayName != "" && (source == displayName || apiKey == displayName) {
		return true
	}

	for _, apiKeyCandidate := range meta.APIKeys {
		candidate := strings.ToLower(strings.TrimSpace(apiKeyCandidate))
		if candidate == "" {
			continue
		}
		if source == candidate || apiKey == candidate {
			return true
		}
	}
	return false
}

func providerConfigModelNames[T interface {
	GetName() string
	GetAlias() string
}](models []T) []string {
	out := make([]string, 0, len(models)*2)
	for _, model := range models {
		out = append(out, model.GetName(), model.GetAlias())
	}
	return trimNonEmptyUnique(out)
}

func providerCompatAPIKeys(entries []config.OpenAICompatibilityAPIKey) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.APIKey)
	}
	return trimNonEmptyUnique(out)
}

func entryModelNames(entry *health.ProbeEntry) []string {
	if entry == nil {
		return nil
	}
	out := make([]string, 0, len(entry.Models)*3)
	for _, model := range entry.Models {
		out = append(out, model.UpstreamModel, model.Model, model.ResponseModel, model.TextFallbackResponseModel)
	}
	return trimNonEmptyUnique(out)
}

func lookupProviderModelTraffic(modelTraffic map[string]providerTrafficSummary, names ...string) providerTrafficSummary {
	for _, name := range names {
		key := normalizeProviderHealthModelKey(name)
		if key == "" {
			continue
		}
		if summary, ok := modelTraffic[key]; ok {
			return summary
		}
	}
	return providerTrafficSummary{}
}

func normalizeProviderHealthModelKey(model string) string {
	return health.CanonicalModel(model)
}

func normalizeProbeModelHealth(status, reason, requestedModel, responseModel string) (string, bool, string) {
	normalizedStatus := strings.TrimSpace(status)
	mismatch := hasResponseModelMismatch(requestedModel, responseModel)
	if mismatch {
		reason = appendReasonText(reason, "response model mismatch")
	}
	return normalizedStatus, mismatch, strings.TrimSpace(reason)
}

func hasResponseModelMismatch(requestedModel, responseModel string) bool {
	requested := normalizeProviderHealthModelKey(requestedModel)
	response := normalizeProviderHealthModelKey(responseModel)
	return requested != "" && response != "" && requested != response
}

func appendReasonText(parts ...string) string {
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{})
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key := strings.ToLower(part)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, part)
	}
	return strings.Join(out, "; ")
}

func firstHealthReason(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return strings.TrimSpace(reasons[0])
}

func buildProviderBalanceSummary(item *providerBalanceItem) *providerBalanceSummary {
	if item == nil {
		return nil
	}
	return &providerBalanceSummary{
		Status:    strings.TrimSpace(item.Status),
		Kind:      strings.TrimSpace(item.Kind),
		Label:     strings.TrimSpace(item.Label),
		Detail:    strings.TrimSpace(item.Detail),
		Currency:  strings.TrimSpace(item.Currency),
		Amount:    item.Amount,
		Total:     item.Total,
		Used:      item.Used,
		UpdatedAt: strings.TrimSpace(item.UpdatedAt),
	}
}

func trimNonEmptyUnique(values []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func summarizeProviderHealth(models []providerHealthModel, runtime health.RuntimeSignal, balance health.BalanceSignal, entry *health.ProbeEntry, now time.Time) (float64, string, []string, time.Time, bool) {
	if len(models) == 0 {
		return 0, "unknown", nil, time.Time{}, false
	}
	totalScore := 0.0
	updatedAt := time.Time{}
	isStale := false
	reasonSet := make(map[string]struct{})
	counts := countProviderHealthStatuses(models)
	for _, model := range models {
		totalScore += model.Score
		testedAt := parseHealthTime(model.TestedAt)
		if testedAt.After(updatedAt) {
			updatedAt = testedAt
		}
		for _, reason := range model.Reasons {
			reason = strings.TrimSpace(reason)
			if reason == "" {
				continue
			}
			reasonSet[reason] = struct{}{}
		}
	}
	if entry != nil {
		entryUpdatedAt := parseHealthTime(entry.UpdatedAt)
		if entryUpdatedAt.After(updatedAt) {
			updatedAt = entryUpdatedAt
		}
		if !entryUpdatedAt.IsZero() && now.Sub(entryUpdatedAt) > 2*time.Hour {
			isStale = true
		}
	}
	providerScore := totalScore / float64(len(models))
	overallStatus := summarizeOverallProviderStatus(counts)
	reasons := make([]string, 0, len(reasonSet)+2)
	for reason := range reasonSet {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	if runtime.Found && runtime.Quota429Count > 0 {
		reasons = append(reasons, "runtime quota pressure")
	}
	if balance.Found && strings.EqualFold(balance.Status, "warning") {
		reasons = append(reasons, "balance warning")
	}
	return providerScore, overallStatus, reasons, updatedAt, isStale
}

func aggregateRuntimeSignals(snapshots []coreauth.AuthRuntimeSnapshot, meta providerHealthMeta) health.RuntimeSignal {
	out := health.RuntimeSignal{}
	if len(snapshots) == 0 {
		return out
	}
	var latencyWeighted float64
	var latencyWeight float64
	for _, snapshot := range snapshots {
		if !runtimeSnapshotMatches(snapshot, meta) || snapshot.Disabled {
			continue
		}
		if !runtimeSnapshotHasSignal(snapshot) {
			continue
		}
		out.SuccessCount += snapshot.Metrics.SuccessCount
		out.FailureCount += snapshot.Metrics.FailureCount
		out.Quota429Count += snapshot.Metrics.Quota429Count
		if snapshot.Metrics.LastUpdatedAt.After(out.LastUpdatedAt) {
			out.LastUpdatedAt = snapshot.Metrics.LastUpdatedAt
		}
		if snapshot.Metrics.AvgLatencyMS > 0 {
			weight := float64(snapshot.Metrics.SuccessCount + snapshot.Metrics.FailureCount)
			if weight <= 0 {
				weight = 1
			}
			latencyWeighted += snapshot.Metrics.AvgLatencyMS * weight
			latencyWeight += weight
		}
		out.Found = true
	}
	if latencyWeight > 0 {
		out.AvgLatencyMS = latencyWeighted / latencyWeight
	}
	return out
}

func runtimeSnapshotHasSignal(snapshot coreauth.AuthRuntimeSnapshot) bool {
	metrics := snapshot.Metrics
	return metrics.SuccessCount > 0 ||
		metrics.FailureCount > 0 ||
		metrics.Quota429Count > 0 ||
		metrics.AvgLatencyMS > 0 ||
		metrics.InflightRequests > 0 ||
		metrics.RecentRPS10 > 0 ||
		metrics.RecentErrorRate30 > 0 ||
		metrics.P95LatencyMS30 > 0 ||
		!metrics.CooldownUntil.IsZero() ||
		!metrics.LastUpdatedAt.IsZero()
}

func runtimeSnapshotMatches(snapshot coreauth.AuthRuntimeSnapshot, meta providerHealthMeta) bool {
	prefix := strings.ToLower(strings.TrimSpace(meta.Prefix))
	baseURL := strings.ToLower(strings.TrimSpace(meta.BaseURL))
	displayName := strings.ToLower(strings.TrimSpace(meta.DisplayName))
	if prefix != "" && strings.ToLower(strings.TrimSpace(snapshot.Prefix)) == prefix {
		return true
	}
	if baseURL != "" && strings.ToLower(strings.TrimSpace(snapshot.BaseURL)) == baseURL {
		return true
	}
	if displayName != "" && strings.ToLower(strings.TrimSpace(snapshot.Label)) == displayName {
		return true
	}
	return false
}

func findProbeEntry(snapshot *health.ProbeSnapshot, prefix, baseURL, key string) *health.ProbeEntry {
	if snapshot == nil {
		return nil
	}
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	baseURL = strings.ToLower(strings.TrimSpace(baseURL))
	for i := range snapshot.Entries {
		entry := &snapshot.Entries[i]
		if providerHealthKey(entry.ProviderPrefix, entry.BaseURL) != key {
			continue
		}
		return entry
	}
	for i := range snapshot.Entries {
		entry := &snapshot.Entries[i]
		if prefix != "" && strings.EqualFold(strings.TrimSpace(entry.ProviderPrefix), prefix) {
			return entry
		}
		if baseURL != "" && strings.EqualFold(strings.TrimSpace(entry.BaseURL), baseURL) {
			return entry
		}
	}
	return nil
}

func probeSnapshotEntries(snapshot *health.ProbeSnapshot) []health.ProbeEntry {
	if snapshot == nil {
		return nil
	}
	return snapshot.Entries
}

func countProviderHealthStatuses(models []providerHealthModel) map[string]int {
	counts := map[string]int{
		"healthy":     0,
		"degraded":    0,
		"unavailable": 0,
		"unknown":     0,
	}
	for _, model := range models {
		status := strings.ToLower(strings.TrimSpace(model.Status))
		if _, ok := counts[status]; !ok {
			status = "unknown"
		}
		counts[status]++
	}
	return counts
}

func summarizeOverallProviderStatus(counts map[string]int) string {
	if counts["healthy"] > 0 {
		return "healthy"
	}
	if counts["degraded"] > 0 || counts["unavailable"] > 0 || counts["unknown"] > 0 {
		return "unavailable"
	}
	return "unavailable"
}

func normalizeProviderOverallDisplayStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "healthy", "degraded":
		return "healthy"
	default:
		return "unavailable"
	}
}

func classifyCompatCategory(entry config.OpenAICompatibility) string {
	name := strings.ToLower(strings.TrimSpace(entry.Name))
	baseURL := strings.ToLower(strings.TrimSpace(entry.BaseURL))
	prefix := strings.ToLower(strings.TrimSpace(entry.Prefix))
	if name == "nvidia" || strings.Contains(baseURL, "integrate.api.nvidia.com") {
		return "other"
	}
	if _, ok := matchOfficialDailyUsageAdapter(providerBalanceRequest{
		DisplayName: entry.Name,
		Prefix:      entry.Prefix,
		BaseURL:     entry.BaseURL,
	}); ok {
		return "codex"
	}
	if strings.Contains(name, "codex") || strings.Contains(prefix, "codex") || strings.Contains(baseURL, "chatgpt.com/backend-api/codex") {
		return "codex"
	}
	for _, model := range entry.Models {
		modelName := strings.ToLower(strings.TrimSpace(model.Name))
		modelAlias := strings.ToLower(strings.TrimSpace(model.Alias))
		switch {
		case strings.HasPrefix(modelName, "claude"), strings.HasPrefix(modelAlias, "claude"):
			return "claude"
		case strings.HasPrefix(modelName, "gemini"), strings.HasPrefix(modelAlias, "gemini"):
			return "gemini"
		case strings.Contains(modelName, "codex"), strings.Contains(modelAlias, "codex"):
			return "codex"
		}
	}
	if strings.Contains(baseURL, "openai.com") || strings.Contains(name, "openai") || strings.Contains(name, "gpt") {
		return "openai-compat"
	}
	return "openai-compat"
}

func compatProviderKey(entry config.OpenAICompatibility) string {
	name := strings.ToLower(strings.TrimSpace(entry.Name))
	if name != "" {
		return name
	}
	return "compat"
}

func providerHealthKey(prefix, baseURL string) string {
	return strings.ToLower(strings.TrimSpace(prefix)) + "||" + strings.ToLower(strings.TrimSpace(baseURL))
}

func providerCategoryOrder(category string) int {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "codex":
		return 0
	case "claude":
		return 1
	case "gemini":
		return 2
	case "vertex":
		return 3
	case "openai-compat":
		return 4
	default:
		return 5
	}
}

func providerStatusRank(status string) int {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "unavailable":
		return 3
	case "degraded":
		return 2
	case "unknown":
		return 1
	default:
		return 0
	}
}

func parseHealthTime(raw string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	return parsed
}

func formatHealthTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
