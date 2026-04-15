package management

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/health"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

const providerBalanceTimeout = 8 * time.Second
const providerBalanceResultsTTL = 15 * time.Second

var providerBalanceChinaLocation = time.FixedZone("CST", 8*60*60)

type observedModelPrice struct {
	Prompt     float64
	Completion float64
	Cache      float64
}

var observedDefaultPricePrefixes = []struct {
	Prefix string
	Price  observedModelPrice
}{
	{Prefix: "claude-opus-4-6", Price: observedModelPrice{Prompt: 5, Completion: 25, Cache: 0.5}},
	{Prefix: "claude-sonnet-4-6", Price: observedModelPrice{Prompt: 3, Completion: 15, Cache: 0.3}},
	{Prefix: "claude-haiku-4-5", Price: observedModelPrice{Prompt: 1, Completion: 5, Cache: 0.1}},
	{Prefix: "gpt-5", Price: observedModelPrice{Prompt: 1.25, Completion: 10, Cache: 0.125}},
	{Prefix: "gpt-5.1", Price: observedModelPrice{Prompt: 1.25, Completion: 10, Cache: 0.125}},
	{Prefix: "gpt-5.2", Price: observedModelPrice{Prompt: 1.75, Completion: 14, Cache: 0.175}},
	{Prefix: "gpt-5.4", Price: observedModelPrice{Prompt: 2.5, Completion: 15, Cache: 0.25}},
	{Prefix: "gpt-5-pro", Price: observedModelPrice{Prompt: 15, Completion: 120, Cache: 0}},
	{Prefix: "gpt-5.2-pro", Price: observedModelPrice{Prompt: 21, Completion: 168, Cache: 0}},
	{Prefix: "gpt-5.4-pro", Price: observedModelPrice{Prompt: 30, Completion: 180, Cache: 0}},
	{Prefix: "gpt-5-mini", Price: observedModelPrice{Prompt: 0.25, Completion: 2, Cache: 0.025}},
	{Prefix: "gpt-5-nano", Price: observedModelPrice{Prompt: 0.05, Completion: 0.4, Cache: 0.005}},
	{Prefix: "gpt-5.4-mini", Price: observedModelPrice{Prompt: 0.75, Completion: 4.5, Cache: 0.075}},
	{Prefix: "gpt-5.4-nano", Price: observedModelPrice{Prompt: 0.2, Completion: 1.25, Cache: 0.02}},
	{Prefix: "gpt-5-chat-latest", Price: observedModelPrice{Prompt: 1.25, Completion: 10, Cache: 0.125}},
	{Prefix: "gpt-5.1-chat-latest", Price: observedModelPrice{Prompt: 1.25, Completion: 10, Cache: 0.125}},
	{Prefix: "gpt-5.2-chat-latest", Price: observedModelPrice{Prompt: 1.75, Completion: 14, Cache: 0.175}},
	{Prefix: "gpt-5-codex", Price: observedModelPrice{Prompt: 1.25, Completion: 10, Cache: 0.125}},
	{Prefix: "gpt-5.1-codex", Price: observedModelPrice{Prompt: 1.25, Completion: 10, Cache: 0.125}},
	{Prefix: "gpt-5.1-codex-max", Price: observedModelPrice{Prompt: 1.25, Completion: 10, Cache: 0.125}},
	{Prefix: "gpt-5.2-codex", Price: observedModelPrice{Prompt: 1.75, Completion: 14, Cache: 0.175}},
	{Prefix: "gpt-5.3-codex", Price: observedModelPrice{Prompt: 1.75, Completion: 14, Cache: 0.175}},
}

type providerBalanceKey struct {
	APIKey   string
	ProxyURL string
}

type providerBalanceRequest struct {
	Section     string
	Index       int
	ProviderKey string
	DisplayName string
	Prefix      string
	BaseURL     string
	Headers     map[string]string
	Keys        []providerBalanceKey
}

type providerBalanceItem struct {
	Section     string  `json:"section"`
	Index       int     `json:"index"`
	ProviderKey string  `json:"provider_key"`
	DisplayName string  `json:"display_name"`
	Prefix      string  `json:"prefix,omitempty"`
	BaseURL     string  `json:"base_url,omitempty"`
	Status      string  `json:"status"`
	Kind        string  `json:"kind,omitempty"`
	Label       string  `json:"label"`
	Detail      string  `json:"detail,omitempty"`
	Currency    string  `json:"currency,omitempty"`
	Amount      float64 `json:"amount,omitempty"`
	Total       float64 `json:"total,omitempty"`
	Used        float64 `json:"used,omitempty"`
	UpdatedAt   string  `json:"updated_at,omitempty"`
}

type cachedProviderBalanceResults struct {
	Requests  []providerBalanceRequest
	Items     []providerBalanceItem
	ExpiresAt time.Time
}

func (h *Handler) GetProviderBalances(c *gin.Context) {
	items := h.collectProviderBalances(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"items":        items,
	})
}

func (h *Handler) collectProviderBalances(ctx context.Context) []providerBalanceItem {
	_, items := h.collectProviderBalanceResultsCached(ctx)
	return items
}

func (h *Handler) ProviderBalanceSnapshot(ctx context.Context) []health.ProviderBalanceSnapshotItem {
	requests, items := h.collectProviderBalanceResultsCached(ctx)
	if len(requests) == 0 || len(items) == 0 {
		return nil
	}
	return providerBalanceSnapshotFromResults(requests, items)
}

func providerBalanceSnapshotFromResults(requests []providerBalanceRequest, items []providerBalanceItem) []health.ProviderBalanceSnapshotItem {
	if len(requests) == 0 || len(items) == 0 {
		return nil
	}
	out := make([]health.ProviderBalanceSnapshotItem, 0, len(items))
	for i := range items {
		req := requests[i]
		item := items[i]
		out = append(out, health.ProviderBalanceSnapshotItem{
			Section:     item.Section,
			ProviderKey: item.ProviderKey,
			DisplayName: item.DisplayName,
			Prefix:      req.Prefix,
			BaseURL:     req.BaseURL,
			Status:      item.Status,
			Kind:        item.Kind,
			Label:       item.Label,
			Detail:      item.Detail,
			Currency:    item.Currency,
			Amount:      item.Amount,
			Total:       item.Total,
			Used:        item.Used,
			UpdatedAt:   parseBalanceUpdatedAt(item.UpdatedAt),
		})
	}
	return out
}

func (h *Handler) collectProviderBalanceResults(ctx context.Context) ([]providerBalanceRequest, []providerBalanceItem) {
	if h == nil || h.cfg == nil {
		return nil, nil
	}
	requests := h.providerBalanceRequests()
	if len(requests) == 0 {
		return nil, nil
	}

	items := make([]providerBalanceItem, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := requests[i]
			item := h.fetchProviderBalance(ctx, req)
			item.Section = req.Section
			item.Index = req.Index
			item.ProviderKey = req.ProviderKey
			item.DisplayName = req.DisplayName
			item.Prefix = req.Prefix
			item.BaseURL = req.BaseURL
			items[i] = item
		}(i)
	}
	wg.Wait()
	return requests, items
}

func (h *Handler) collectProviderBalanceResultsCached(ctx context.Context) ([]providerBalanceRequest, []providerBalanceItem) {
	if h == nil {
		return nil, nil
	}

	now := time.Now()
	h.providerBalancesMu.Lock()
	cached := h.providerBalanceResults
	if cached.ExpiresAt.After(now) && len(cached.Items) > 0 {
		requests := append([]providerBalanceRequest(nil), cached.Requests...)
		items := append([]providerBalanceItem(nil), cached.Items...)
		h.providerBalancesMu.Unlock()
		return requests, items
	}
	h.providerBalancesMu.Unlock()

	requests, items := h.collectProviderBalanceResults(ctx)
	if len(items) == 0 {
		return requests, items
	}

	h.providerBalancesMu.Lock()
	h.providerBalanceResults = cachedProviderBalanceResults{
		Requests:  append([]providerBalanceRequest(nil), requests...),
		Items:     append([]providerBalanceItem(nil), items...),
		ExpiresAt: time.Now().Add(providerBalanceResultsTTL),
	}
	h.providerBalancesMu.Unlock()
	return requests, items
}

func (h *Handler) providerBalanceRequests() []providerBalanceRequest {
	if h == nil || h.cfg == nil {
		return nil
	}

	var out []providerBalanceRequest
	addSingleKey := func(section, providerKey string, index int, displayName, prefix, baseURL, proxyURL string, headers map[string]string, apiKey string) {
		out = append(out, providerBalanceRequest{
			Section:     section,
			Index:       index,
			ProviderKey: providerKey,
			DisplayName: strings.TrimSpace(displayName),
			Prefix:      strings.TrimSpace(prefix),
			BaseURL:     strings.TrimSpace(baseURL),
			Headers:     config.NormalizeHeaders(headers),
			Keys: []providerBalanceKey{{
				APIKey:   strings.TrimSpace(apiKey),
				ProxyURL: strings.TrimSpace(proxyURL),
			}},
		})
	}

	for i, entry := range h.cfg.GeminiKey {
		addSingleKey("gemini-api-key", "gemini", i, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "gemini", i), entry.Prefix, entry.BaseURL, entry.ProxyURL, entry.Headers, entry.APIKey)
	}
	for i, entry := range h.cfg.ClaudeKey {
		addSingleKey("claude-api-key", "claude", i, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "claude", i), entry.Prefix, entry.BaseURL, entry.ProxyURL, entry.Headers, entry.APIKey)
	}
	for i, entry := range h.cfg.CodexKey {
		addSingleKey("codex-api-key", "codex", i, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "codex", i), entry.Prefix, entry.BaseURL, entry.ProxyURL, entry.Headers, entry.APIKey)
	}
	for i, entry := range h.cfg.VertexCompatAPIKey {
		addSingleKey("vertex-api-key", "vertex", i, deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, "vertex", i), entry.Prefix, entry.BaseURL, entry.ProxyURL, entry.Headers, entry.APIKey)
	}

	nvidia, compat := splitOpenAICompatByNVIDIA(h.cfg.OpenAICompatibility)
	addCompat := func(section, providerKey string, entries []config.OpenAICompatibility) {
		for i, entry := range entries {
			keys := make([]providerBalanceKey, 0, len(entry.APIKeyEntries))
			for _, keyEntry := range entry.APIKeyEntries {
				keys = append(keys, providerBalanceKey{
					APIKey:   strings.TrimSpace(keyEntry.APIKey),
					ProxyURL: strings.TrimSpace(keyEntry.ProxyURL),
				})
			}
			out = append(out, providerBalanceRequest{
				Section:     section,
				Index:       i,
				ProviderKey: providerKey,
				DisplayName: deriveProviderDisplayName(entry.Name, entry.Prefix, entry.BaseURL, providerKey, i),
				Prefix:      strings.TrimSpace(entry.Prefix),
				BaseURL:     strings.TrimSpace(entry.BaseURL),
				Headers:     config.NormalizeHeaders(entry.Headers),
				Keys:        keys,
			})
		}
	}
	addCompat("nvidia-api-key", "nvidia", nvidia)
	addCompat("openai-compatibility", "compat", compat)

	return out
}

func parseBalanceUpdatedAt(raw string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	return parsed
}

func deriveProviderDisplayName(name, prefix, baseURL, fallback string, index int) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	prefix = strings.TrimSpace(prefix)
	if prefix != "" {
		return prefix
	}
	if short := providerShortBaseURL(baseURL); short != "" {
		return short
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		fallback = "provider"
	}
	return fmt.Sprintf("%s-%d", fallback, index+1)
}

func providerShortBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.TrimSpace(parsed.Host)
	path := strings.TrimRight(strings.TrimSpace(parsed.Path), "/")
	if host == "" {
		return raw
	}
	if path == "" || path == "/" {
		return host
	}
	return host + path
}

func (h *Handler) fetchProviderBalance(ctx context.Context, req providerBalanceRequest) providerBalanceItem {
	if adapter, ok := matchOfficialDailyUsageAdapter(req); ok {
		return h.fetchOfficialDailyUsage(ctx, req, adapter)
	}

	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: req.DisplayName,
		Status:      "unsupported",
		Label:       providerUnsupportedLabel(req.ProviderKey),
		Detail:      providerUnsupportedDetail(req.ProviderKey),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	switch {
	case isNowCodingProvider(req):
		return h.fetchNowCodingQuota(ctx, req)
	case isNewAPIProvider(req):
		return h.fetchNewAPIQuota(ctx, req)
	case isOpenRouterProvider(req):
		return h.fetchOpenRouterCredits(ctx, req)
	case isSub2APIUsageProvider(req):
		return h.fetchSub2APIUsage(ctx, req)
	case isYunyiProvider(req):
		return h.fetchYunyiQuota(ctx, req)
	case isAnthropicAdminProvider(req):
		return h.fetchAnthropicCost(ctx, req)
	default:
		if h != nil && h.usageStats != nil {
			return h.fetchObservedUsageBalance(req)
		}
		return item
	}
}

func isOpenRouterProvider(req providerBalanceRequest) bool {
	if host := providerHost(req.BaseURL); strings.Contains(host, "openrouter.ai") {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(req.DisplayName))
	return strings.Contains(name, "openrouter")
}

func isAnthropicAdminProvider(req providerBalanceRequest) bool {
	if strings.TrimSpace(req.ProviderKey) != "claude" {
		return false
	}
	for _, key := range req.Keys {
		if strings.HasPrefix(strings.TrimSpace(key.APIKey), "sk-ant-admin") {
			return true
		}
	}
	return false
}

func isSub2APIUsageProvider(req providerBalanceRequest) bool {
	name := strings.ToLower(strings.TrimSpace(req.DisplayName))
	if strings.Contains(name, "sub2api") || strings.Contains(name, "soapapi") {
		return true
	}
	host := providerHost(req.BaseURL)
	return strings.Contains(host, "soapapi.top")
}

func providerHost(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parsed.Host))
}

func providerUnsupportedLabel(providerKey string) string {
	switch strings.TrimSpace(providerKey) {
	case "gemini":
		return "AI Studio 未开放余额接口"
	case "claude":
		return "当前密钥不支持额度接口"
	case "codex":
		return "未发现官方余额接口"
	case "vertex":
		return "Vertex 兼容源未开放余额接口"
	case "nvidia":
		return "NVIDIA 未开放余额接口"
	default:
		return "当前提供商未适配额度接口"
	}
}

func providerUnsupportedDetail(providerKey string) string {
	switch strings.TrimSpace(providerKey) {
	case "gemini":
		return "Gemini AI Studio API key 当前没有公开的剩余额度查询接口。"
	case "claude":
		return "Claude 标准 API key 无法直接查询剩余额度；若使用 Admin key，可显示近 30 天成本。"
	case "codex":
		return "OpenAI/Codex 当前没有稳定公开的剩余额度接口。"
	case "vertex":
		return "Vertex 兼容 API key 由具体上游决定，当前未内置余额查询策略。"
	case "nvidia":
		return "NVIDIA/NIM 当前未内置余额查询策略。"
	default:
		return "该 OpenAI 兼容提供商暂未内置余额查询策略。"
	}
}

func normalizeObservedModelPriceKey(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return ""
	}
	if slash := strings.LastIndex(value, "/"); slash >= 0 && slash+1 < len(value) {
		value = value[slash+1:]
	}
	for strings.HasPrefix(value, "[") {
		end := strings.Index(value, "]")
		if end <= 0 {
			break
		}
		value = strings.TrimSpace(value[end+1:])
	}
	return value
}

func resolveObservedModelPrice(modelName string) (observedModelPrice, bool) {
	normalized := normalizeObservedModelPriceKey(modelName)
	if normalized == "" {
		return observedModelPrice{}, false
	}
	for _, entry := range observedDefaultPricePrefixes {
		if normalized == entry.Prefix || strings.HasPrefix(normalized, entry.Prefix+"-") {
			return entry.Price, true
		}
	}
	return observedModelPrice{}, false
}

func calculateObservedUsageCost(detail usage.RequestDetail, modelName string) (float64, bool) {
	price, ok := resolveObservedModelPrice(modelName)
	if !ok {
		return 0, false
	}
	inputTokens := float64(maxInt64(detail.Tokens.InputTokens, 0))
	completionTokens := float64(maxInt64(detail.Tokens.OutputTokens, 0))
	cachedTokens := float64(maxInt64(detail.Tokens.CachedTokens, 0))
	promptTokens := inputTokens - cachedTokens
	if promptTokens < 0 {
		promptTokens = 0
	}
	total := (promptTokens/1_000_000)*price.Prompt +
		(cachedTokens/1_000_000)*price.Cache +
		(completionTokens/1_000_000)*price.Completion
	if total <= 0 {
		return 0, false
	}
	return total, true
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}

func chinaDayKey(t time.Time) string {
	return t.In(providerBalanceChinaLocation).Format("2006-01-02")
}

func maskObservedAPIKey(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	visibleChars := 2
	if len(trimmed) < 4 {
		visibleChars = 1
	}
	start := trimmed[:visibleChars]
	end := trimmed[len(trimmed)-visibleChars:]
	maskedLength := 10 - visibleChars*2
	if maskedLength < 1 {
		maskedLength = 1
	}
	return start + strings.Repeat("*", maskedLength) + end
}

func observedAPIKeyHash(raw string) string {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(strings.TrimSpace(raw)))
	return strconv.FormatUint(hasher.Sum64(), 16)
}

func providerBalanceCandidateSourceIDs(req providerBalanceRequest) map[string]struct{} {
	out := make(map[string]struct{})
	addCandidate := func(value string, isPrefix bool) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		if isPrefix {
			out["t:"+trimmed] = struct{}{}
			return
		}
		out["k:"+observedAPIKeyHash(trimmed)] = struct{}{}
		out["m:"+maskObservedAPIKey(trimmed)] = struct{}{}
	}
	if prefix := strings.TrimSpace(req.Prefix); prefix != "" {
		addCandidate(prefix, true)
	}
	for _, key := range req.Keys {
		addCandidate(key.APIKey, false)
	}
	for headerKey, headerValue := range req.Headers {
		lowerKey := strings.ToLower(strings.TrimSpace(headerKey))
		value := strings.TrimSpace(headerValue)
		if value == "" {
			continue
		}
		if lowerKey == "authorization" && strings.HasPrefix(strings.ToLower(value), "bearer ") {
			addCandidate(strings.TrimSpace(value[len("Bearer "):]), false)
			continue
		}
		if strings.Contains(lowerKey, "key") || strings.Contains(lowerKey, "token") || strings.Contains(lowerKey, "authorization") {
			addCandidate(value, false)
		}
	}
	return out
}

func (h *Handler) fetchObservedUsageBalance(req providerBalanceRequest) providerBalanceItem {
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: req.DisplayName,
		Status:      "unsupported",
		Label:       "未观测到本地消耗",
		Detail:      "当前渠道不支持直接查询上游剩余额度；这里将显示本地已观测到的请求与 token 消耗。",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if h == nil || h.usageStats == nil {
		return item
	}

	now := time.Now().UTC()
	snapshot := h.usageStats.Snapshot()
	snapshot, _ = usage.SnapshotWithPersistedDetails(snapshot, now, usage.DetailsRetention())
	chinaToday := chinaDayKey(now)

	var (
		requests       int64
		success        int64
		failure        int64
		totalTokens    int64
		pricedRequests int64
		totalCost      float64
		lastSeen       time.Time
	)
	for apiName, apiSnapshot := range snapshot.APIs {
		for modelName, modelSnapshot := range apiSnapshot.Models {
			for _, detail := range modelSnapshot.Details {
				if !providerBalanceUsageMatches(req, apiName, detail) {
					continue
				}
				if chinaDayKey(detail.Timestamp) != chinaToday {
					continue
				}
				requests++
				if detail.Failed {
					failure++
				} else {
					success++
				}
				totalTokens += detail.Tokens.TotalTokens
				if cost, ok := calculateObservedUsageCost(detail, modelName); ok {
					totalCost += cost
					pricedRequests++
				}
				if detail.Timestamp.After(lastSeen) {
					lastSeen = detail.Timestamp
				}
			}
		}
	}

	if requests == 0 {
		item.Status = "partial"
		item.Label = "本地尚未观测到今日消耗"
		item.Detail = "中国时间今日 00:00 至今未匹配到该渠道的本地 usage 记录；这不是上游剩余额度。"
		return item
	}

	item.Status = "ok"
	item.Kind = "estimated_observed_usage"
	item.UpdatedAt = lastSeen.UTC().Format(time.RFC3339)
	if lastSeen.IsZero() {
		item.UpdatedAt = now.Format(time.RFC3339)
	}

	// Claude-compatible reseller channels often have non-official pricing,
	// so a hardcoded USD estimate is visibly misleading. For unsupported
	// Claude channels, surface token usage only.
	if strings.EqualFold(strings.TrimSpace(req.ProviderKey), "claude") {
		item.Amount = float64(totalTokens)
		item.Label = fmt.Sprintf("今日已用 %d Tokens", totalTokens)
		item.Detail = fmt.Sprintf(
			"中国时间 00:00 至今：成功 %d / 失败 %d | %d 次请求 | %d tokens | Claude 非官方渠道不按内置美元单价估算，避免失真 | 非上游剩余额度",
			success,
			failure,
			requests,
			totalTokens,
		)
	} else if pricedRequests > 0 {
		item.Currency = "USD"
		item.Amount = totalCost
		item.Label = fmt.Sprintf("今日已用 $%.2f", totalCost)
		item.Detail = fmt.Sprintf(
			"中国时间 00:00 至今：成功 %d / 失败 %d | %d 次请求 | %d tokens | 非准确估计值 | 非上游剩余额度",
			success,
			failure,
			requests,
			totalTokens,
		)
	} else {
		item.Amount = float64(totalTokens)
		item.Label = fmt.Sprintf("今日已用 %d Tokens", totalTokens)
		item.Detail = fmt.Sprintf(
			"中国时间 00:00 至今：成功 %d / 失败 %d | %d 次请求 | %d tokens | 未命中默认模型单价 | 非准确估计值 | 非上游剩余额度",
			success,
			failure,
			requests,
			totalTokens,
		)
	}
	item.Detail = appendResetCountdown(item.Detail, now, nextResetAtChinaTime(now, 24, 0))
	return item
}

func providerBalanceUsageMatches(req providerBalanceRequest, apiName string, detail usage.RequestDetail) bool {
	source := strings.ToLower(strings.TrimSpace(detail.Source))
	apiKey := strings.ToLower(strings.TrimSpace(apiName))
	prefix := strings.ToLower(strings.TrimSpace(req.Prefix))
	displayName := strings.ToLower(strings.TrimSpace(req.DisplayName))
	candidateSourceIDs := providerBalanceCandidateSourceIDs(req)

	if prefix != "" && (source == prefix || apiKey == prefix) {
		return true
	}
	if displayName != "" && (source == displayName || apiKey == displayName) {
		return true
	}
	if _, ok := candidateSourceIDs[detail.Source]; ok {
		return true
	}
	if _, ok := candidateSourceIDs[source]; ok {
		return true
	}
	for _, key := range req.Keys {
		candidate := strings.ToLower(strings.TrimSpace(key.APIKey))
		if candidate == "" {
			continue
		}
		if source == candidate || apiKey == candidate {
			return true
		}
	}
	for headerKey, headerValue := range req.Headers {
		lowerKey := strings.ToLower(strings.TrimSpace(headerKey))
		value := strings.TrimSpace(headerValue)
		if value == "" {
			continue
		}
		candidate := value
		if lowerKey == "authorization" && strings.HasPrefix(strings.ToLower(value), "bearer ") {
			candidate = strings.TrimSpace(value[len("Bearer "):])
		}
		candidateLower := strings.ToLower(strings.TrimSpace(candidate))
		if candidateLower == "" {
			continue
		}
		if source == candidateLower || apiKey == candidateLower {
			return true
		}
	}
	return false
}

func (h *Handler) fetchOpenRouterCredits(ctx context.Context, req providerBalanceRequest) providerBalanceItem {
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: req.DisplayName,
		Status:      "error",
		Label:       "额度查询失败",
		Detail:      "未拿到可用的 OpenRouter credits 响应。",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	if len(req.Keys) == 0 {
		item.Detail = "当前 provider 未配置 API key。"
		return item
	}

	baseRoot, err := providerRootURL(req.BaseURL)
	if err != nil {
		item.Detail = err.Error()
		return item
	}
	creditsURL := strings.TrimRight(baseRoot, "/") + "/api/v1/credits"

	var totalCredits float64
	var totalUsage float64
	var successCount int
	var errorMessages []string

	for _, key := range req.Keys {
		apiKey := strings.TrimSpace(key.APIKey)
		if apiKey == "" {
			continue
		}
		credits, usage, errFetch := h.fetchOpenRouterCreditKey(ctx, creditsURL, req.Headers, key)
		if errFetch != nil {
			errorMessages = append(errorMessages, errFetch.Error())
			continue
		}
		totalCredits += credits
		totalUsage += usage
		successCount++
	}

	if successCount == 0 {
		if len(errorMessages) > 0 {
			item.Detail = strings.Join(errorMessages, " | ")
		}
		return item
	}

	remaining := totalCredits - totalUsage
	if remaining < 0 {
		remaining = 0
	}
	item.Status = "ok"
	if successCount < len(req.Keys) {
		item.Status = "partial"
	}
	item.Kind = "remaining_credits"
	item.Currency = "USD"
	item.Amount = remaining
	item.Total = totalCredits
	item.Used = totalUsage
	item.Label = fmt.Sprintf("剩余额度 $%.2f", remaining)
	item.Detail = fmt.Sprintf("已用 $%.2f / 总充值 $%.2f", totalUsage, totalCredits)
	if successCount < len(req.Keys) && len(errorMessages) > 0 {
		item.Detail += " | 部分 key 查询失败: " + strings.Join(errorMessages, " | ")
	}
	return item
}

func (h *Handler) fetchOpenRouterCreditKey(ctx context.Context, endpoint string, headers map[string]string, key providerBalanceKey) (float64, float64, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key.APIKey))
	req.Header.Set("Accept", "application/json")
	applySupplementalHeaders(req, headers, map[string]struct{}{
		"authorization": {},
	})

	httpClient := &http.Client{
		Timeout:   providerBalanceTimeout,
		Transport: h.providerBalanceTransport(strings.TrimSpace(key.ProxyURL)),
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return 0, 0, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return 0, 0, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data map[string]any `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return 0, 0, errUnmarshal
	}
	return anyToFloat(payload.Data["total_credits"]), anyToFloat(payload.Data["total_usage"]), nil
}

func (h *Handler) fetchAnthropicCost(ctx context.Context, req providerBalanceRequest) providerBalanceItem {
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: req.DisplayName,
		Status:      "error",
		Label:       "额度查询失败",
		Detail:      "未拿到可用的 Anthropic 成本报表响应。",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	if len(req.Keys) == 0 {
		item.Detail = "当前 provider 未配置 API key。"
		return item
	}

	baseRoot, err := providerRootURL(req.BaseURL)
	if err != nil {
		item.Detail = err.Error()
		return item
	}
	costURL := strings.TrimRight(baseRoot, "/") + "/v1/organizations/cost_report"

	now := time.Now().UTC()
	startingAt := now.AddDate(0, 0, -30).Format(time.RFC3339)
	endingAt := now.Format(time.RFC3339)

	var lastErr error
	for _, key := range req.Keys {
		apiKey := strings.TrimSpace(key.APIKey)
		if apiKey == "" || !strings.HasPrefix(apiKey, "sk-ant-admin") {
			continue
		}
		amount, currency, errFetch := h.fetchAnthropicCostKey(ctx, costURL, startingAt, endingAt, req.Headers, key)
		if errFetch != nil {
			lastErr = errFetch
			continue
		}
		item.Status = "ok"
		item.Kind = "usage_30d"
		item.Currency = currency
		item.Amount = amount
		item.Label = fmt.Sprintf("近30天 $%.2f", amount)
		item.Detail = "Anthropic Admin API 成本报表，不是剩余额度。"
		return item
	}

	if lastErr != nil {
		item.Detail = lastErr.Error()
		return item
	}

	item.Status = "unsupported"
	item.Label = "需要 Admin key 才能查询"
	item.Detail = "Anthropic Usage & Cost API 需要 `sk-ant-admin...` 管理员密钥。"
	return item
}

func (h *Handler) fetchSub2APIUsage(ctx context.Context, req providerBalanceRequest) providerBalanceItem {
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: req.DisplayName,
		Status:      "error",
		Label:       "额度查询失败",
		Detail:      "未拿到可用的 Sub2API usage 响应。",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	if len(req.Keys) == 0 {
		item.Detail = "当前 provider 未配置 API key。"
		return item
	}

	usageURL, err := resolveSub2APIUsageEndpointForRequest(req)
	if err != nil {
		item.Detail = err.Error()
		return item
	}

	var lastErr error
	for _, key := range req.Keys {
		apiKey := strings.TrimSpace(key.APIKey)
		if apiKey == "" {
			continue
		}
		snapshot, errFetch := h.fetchSub2APIUsageKey(ctx, usageURL, req.Headers, key)
		if errFetch != nil {
			lastErr = errFetch
			continue
		}

		item.Status = "ok"
		if !snapshot.Valid || !strings.EqualFold(snapshot.Status, "active") {
			item.Status = "partial"
		}
		item.Kind = "remaining_balance"
		item.Currency = snapshot.Currency
		item.Amount = snapshot.Remaining
		item.Total = snapshot.Total
		item.Used = snapshot.Used
		item.Label = fmt.Sprintf("剩余额度 $%.2f", snapshot.Remaining)
		item.Detail = fmt.Sprintf("已用 $%.2f / 总额 $%.2f", snapshot.Used, snapshot.Total)
		if snapshot.PlanName != "" {
			item.Detail += " | " + snapshot.PlanName
		}
		if snapshot.Mode != "" {
			item.Detail += " | mode: " + snapshot.Mode
		}
		if snapshot.Status != "" && !strings.EqualFold(snapshot.Status, "active") {
			item.Detail += " | status: " + snapshot.Status
		}
		if !snapshot.Valid {
			item.Detail += " | isValid=false"
		}
		return item
	}

	if lastErr != nil {
		item.Detail = lastErr.Error()
	}
	return item
}

func resolveSub2APIUsageEndpointForRequest(req providerBalanceRequest) (string, error) {
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		return "", fmt.Errorf("provider base-url is empty")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider base-url")
	}

	name := strings.ToLower(strings.TrimSpace(req.DisplayName))
	prefix := strings.ToLower(strings.TrimSpace(req.Prefix))
	host := strings.ToLower(strings.TrimSpace(parsed.Host))
	if strings.Contains(name, "soapapi") || strings.Contains(prefix, "soapapi") || strings.Contains(host, "soapapi.top") {
		parsed.Path = "/v1/public/key-usage"
		parsed.RawPath = ""
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String(), nil
	}

	return resolveSub2APIUsageEndpoint(baseURL)
}

func resolveSub2APIUsageEndpoint(baseURL string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", fmt.Errorf("provider base-url is empty")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider base-url")
	}

	path := strings.TrimRight(strings.TrimSpace(parsed.Path), "/")
	switch {
	case path == "":
		path = "/v1/usage"
	case strings.HasSuffix(path, "/v1"):
		path += "/usage"
	default:
		path += "/v1/usage"
	}

	parsed.Path = path
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

type sub2APIUsageSnapshot struct {
	Remaining float64
	Total     float64
	Used      float64
	Currency  string
	Mode      string
	PlanName  string
	Status    string
	Valid     bool
}

func (h *Handler) fetchSub2APIUsageKey(ctx context.Context, endpoint string, headers map[string]string, key providerBalanceKey) (sub2APIUsageSnapshot, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, endpoint, nil)
	if err != nil {
		return sub2APIUsageSnapshot{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key.APIKey))
	req.Header.Set("Accept", "application/json")
	applySupplementalHeaders(req, headers, map[string]struct{}{
		"authorization": {},
	})

	httpClient := &http.Client{
		Timeout:   providerBalanceTimeout,
		Transport: h.providerBalanceTransport(strings.TrimSpace(key.ProxyURL)),
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return sub2APIUsageSnapshot{}, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return sub2APIUsageSnapshot{}, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return sub2APIUsageSnapshot{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		IsValid   *bool  `json:"isValid"`
		Mode      string `json:"mode"`
		Status    string `json:"status"`
		Unit      string `json:"unit"`
		Remaining any    `json:"remaining"`
		Balance   any    `json:"balance"`
		PlanName  string `json:"planName"`
		Quota     struct {
			Limit     any    `json:"limit"`
			Remaining any    `json:"remaining"`
			Used      any    `json:"used"`
			Unit      string `json:"unit"`
		} `json:"quota"`
		Subscription struct {
			DailyLimitUSD   any `json:"daily_limit_usd"`
			DailyUsageUSD   any `json:"daily_usage_usd"`
			WeeklyLimitUSD  any `json:"weekly_limit_usd"`
			WeeklyUsageUSD  any `json:"weekly_usage_usd"`
			MonthlyLimitUSD any `json:"monthly_limit_usd"`
			MonthlyUsageUSD any `json:"monthly_usage_usd"`
		} `json:"subscription"`
		RateLimits []struct {
			Limit     any    `json:"limit"`
			Remaining any    `json:"remaining"`
			Used      any    `json:"used"`
			Window    string `json:"window"`
		} `json:"rate_limits"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return sub2APIUsageSnapshot{}, errUnmarshal
	}

	snapshot := sub2APIUsageSnapshot{
		Currency: strings.TrimSpace(payload.Quota.Unit),
		Mode:     strings.TrimSpace(payload.Mode),
		PlanName: strings.TrimSpace(payload.PlanName),
		Status:   strings.TrimSpace(payload.Status),
		Valid:    payload.IsValid == nil || *payload.IsValid,
	}
	if snapshot.Currency == "" {
		snapshot.Currency = strings.TrimSpace(payload.Unit)
	}
	if snapshot.Currency == "" {
		snapshot.Currency = "USD"
	}

	snapshot.Remaining = anyToFloat(payload.Quota.Remaining)
	snapshot.Total = anyToFloat(payload.Quota.Limit)
	snapshot.Used = anyToFloat(payload.Quota.Used)

	if snapshot.Total <= 0 && len(payload.RateLimits) > 0 {
		selected := payload.RateLimits[0]
		for _, limit := range payload.RateLimits {
			if strings.EqualFold(strings.TrimSpace(limit.Window), "1d") {
				selected = limit
				break
			}
		}
		snapshot.Remaining = anyToFloat(selected.Remaining)
		snapshot.Total = anyToFloat(selected.Limit)
		snapshot.Used = anyToFloat(selected.Used)
	}

	if snapshot.Mode != "quota_limited" {
		if snapshot.Remaining <= 0 {
			snapshot.Remaining = anyToFloat(payload.Remaining)
		}
		if snapshot.Remaining <= 0 {
			snapshot.Remaining = anyToFloat(payload.Balance)
		}

		switch {
		case anyToFloat(payload.Subscription.MonthlyLimitUSD) > 0:
			snapshot.Total = anyToFloat(payload.Subscription.MonthlyLimitUSD)
			snapshot.Used = anyToFloat(payload.Subscription.MonthlyUsageUSD)
		case anyToFloat(payload.Subscription.WeeklyLimitUSD) > 0:
			snapshot.Total = anyToFloat(payload.Subscription.WeeklyLimitUSD)
			snapshot.Used = anyToFloat(payload.Subscription.WeeklyUsageUSD)
		case anyToFloat(payload.Subscription.DailyLimitUSD) > 0:
			snapshot.Total = anyToFloat(payload.Subscription.DailyLimitUSD)
			snapshot.Used = anyToFloat(payload.Subscription.DailyUsageUSD)
		}
	}

	if snapshot.Remaining <= 0 {
		snapshot.Remaining = anyToFloat(payload.Remaining)
	}
	if snapshot.Total <= 0 && (snapshot.Remaining > 0 || snapshot.Used > 0) {
		snapshot.Total = snapshot.Remaining + snapshot.Used
	}
	if snapshot.Used <= 0 && snapshot.Total > 0 && snapshot.Remaining >= 0 {
		snapshot.Used = snapshot.Total - snapshot.Remaining
	}
	if snapshot.Remaining < 0 {
		snapshot.Remaining = 0
	}
	if snapshot.Used < 0 {
		snapshot.Used = 0
	}
	if snapshot.Total < 0 {
		snapshot.Total = 0
	}

	return snapshot, nil
}

func (h *Handler) fetchAnthropicCostKey(ctx context.Context, endpoint, startingAt, endingAt string, headers map[string]string, key providerBalanceKey) (float64, string, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	query := url.Values{}
	query.Set("starting_at", startingAt)
	query.Set("ending_at", endingAt)
	query.Set("bucket_width", "1d")
	query.Set("limit", "31")
	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("x-api-key", strings.TrimSpace(key.APIKey))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Accept", "application/json")
	applySupplementalHeaders(req, headers, map[string]struct{}{
		"x-api-key":         {},
		"anthropic-version": {},
		"authorization":     {},
	})

	httpClient := &http.Client{
		Timeout:   providerBalanceTimeout,
		Transport: h.providerBalanceTransport(strings.TrimSpace(key.ProxyURL)),
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return 0, "", errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return 0, "", errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 0, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data []struct {
			Results []struct {
				Amount   string `json:"amount"`
				Currency string `json:"currency"`
			} `json:"results"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return 0, "", errUnmarshal
	}

	var total float64
	currency := "USD"
	for _, bucket := range payload.Data {
		for _, result := range bucket.Results {
			total += anthropicAmountToUSD(result.Amount)
			if strings.TrimSpace(result.Currency) != "" {
				currency = strings.TrimSpace(result.Currency)
			}
		}
	}
	return total, currency, nil
}

func anthropicAmountToUSD(raw string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0
	}
	return value / 100.0
}

func providerRootURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("provider base-url is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid provider base-url")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func defaultBalanceContext(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func applySupplementalHeaders(req *http.Request, headers map[string]string, protected map[string]struct{}) {
	if req == nil || len(headers) == 0 {
		return
	}
	for key, value := range headers {
		k := strings.TrimSpace(key)
		v := strings.TrimSpace(value)
		if k == "" || v == "" {
			continue
		}
		if _, blocked := protected[strings.ToLower(k)]; blocked {
			continue
		}
		req.Header.Set(k, v)
	}
}

func (h *Handler) providerBalanceTransport(proxyURL string) http.RoundTripper {
	if transport := buildProxyTransport(strings.TrimSpace(proxyURL)); transport != nil {
		return transport
	}
	return h.apiCallTransport(nil)
}

func anyToFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		f, _ := typed.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return f
	default:
		return 0
	}
}

func firstFloatValue(values map[string]any, keys ...string) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		value, ok := values[trimmed]
		if !ok || value == nil {
			continue
		}
		return anyToFloat(value), true
	}
	return 0, false
}
