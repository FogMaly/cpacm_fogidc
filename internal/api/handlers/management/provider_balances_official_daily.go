package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type officialDailyUsageAdapter struct {
	DisplayName     string
	ErrorName       string
	HostContains    string
	NameContains    string
	DailyTotalUSD   float64
	ResetHour       int
	ResetMinute     int
	UseAPIRemaining bool
	UseAPITotal     bool
}

var officialDailyUsageAdapters = []officialDailyUsageAdapter{
	{
		DisplayName:     "aixj",
		ErrorName:       "AIXJ",
		HostContains:    "aixj.vip",
		NameContains:    "aixj",
		DailyTotalUSD:   100.0,
		ResetHour:       11,
		ResetMinute:     45,
		UseAPIRemaining: true,
		UseAPITotal:     true,
	},
	{
		DisplayName:   "gmncode",
		ErrorName:     "GMNCODE",
		HostContains:  "gmncode.cn",
		NameContains:  "gmncode",
		DailyTotalUSD: 90.0,
		ResetHour:     24,
		ResetMinute:   0,
	},
}

func matchOfficialDailyUsageAdapter(req providerBalanceRequest) (officialDailyUsageAdapter, bool) {
	host := providerHost(req.BaseURL)
	name := strings.ToLower(strings.TrimSpace(req.DisplayName))
	for _, adapter := range officialDailyUsageAdapters {
		if adapter.HostContains != "" && strings.Contains(host, adapter.HostContains) {
			return adapter, true
		}
		if adapter.NameContains != "" && strings.Contains(name, adapter.NameContains) {
			return adapter, true
		}
	}
	return officialDailyUsageAdapter{}, false
}

func (h *Handler) fetchOfficialDailyUsage(ctx context.Context, req providerBalanceRequest, adapter officialDailyUsageAdapter) providerBalanceItem {
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: adapter.DisplayName,
		Status:      "error",
		Label:       "额度查询失败",
		Detail:      fmt.Sprintf("未拿到可用的 %s 当日用量响应。", adapter.ErrorName),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	if len(req.Keys) == 0 {
		item.Detail = "当前 provider 未配置 API key。"
		return item
	}

	endpoint, err := resolveOfficialUsageEndpoint(req.BaseURL)
	if err != nil {
		item.Detail = err.Error()
		return item
	}

	var lastErr error
	now := time.Now()
	for _, key := range req.Keys {
		apiKey := strings.TrimSpace(key.APIKey)
		if apiKey == "" {
			continue
		}
		apiRemaining, apiTotal, used, errFetch := h.fetchOfficialUsageKey(ctx, endpoint, req.Headers, key)
		if errFetch != nil {
			if quotaItem, ok := officialDailyUsageQuotaExceededItem(req, adapter, now, errFetch); ok {
				return quotaItem
			}
			lastErr = errFetch
			continue
		}

		total := adapter.DailyTotalUSD
		if adapter.UseAPITotal && apiTotal > 0 {
			total = apiTotal
		}
		if total <= 0 && (apiRemaining > 0 || used > 0) {
			total = apiRemaining + used
		}

		remaining := total - used
		if adapter.UseAPIRemaining && (apiRemaining > 0 || used > 0 || apiTotal > 0) {
			remaining = apiRemaining
		}
		if remaining < 0 {
			remaining = 0
		}

		item.Status = "ok"
		item.Kind = "remaining_balance"
		item.Currency = "USD"
		item.Amount = remaining
		item.Total = adapter.DailyTotalUSD
		item.Used = used
		item.Label = fmt.Sprintf("当日剩余额度 $%.2f", remaining)
		item.Detail = fmt.Sprintf("今日已用 $%.2f / 日总 $%.2f", used, adapter.DailyTotalUSD)
		item.Detail = appendResetCountdown(item.Detail, now, nextResetAtChinaTime(now, adapter.ResetHour, adapter.ResetMinute))
		return item
	}

	if lastErr != nil {
		item.Detail = lastErr.Error()
	}
	return item
}

func officialDailyUsageQuotaExceededItem(req providerBalanceRequest, adapter officialDailyUsageAdapter, now time.Time, err error) (providerBalanceItem, bool) {
	if err == nil {
		return providerBalanceItem{}, false
	}
	detail := strings.TrimSpace(err.Error())
	normalized := strings.ToUpper(detail)
	if !strings.Contains(normalized, "HTTP 429") {
		return providerBalanceItem{}, false
	}
	if !strings.Contains(normalized, "USAGE_LIMIT_EXCEEDED") && !strings.Contains(normalized, "DAILY_LIMIT_EXCEEDED") {
		return providerBalanceItem{}, false
	}

	total := adapter.DailyTotalUSD
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: adapter.DisplayName,
		Status:      "warning",
		Kind:        "remaining_balance",
		Label:       "当日剩余额度 $0.00",
		Detail:      "上游返回 DAILY_LIMIT_EXCEEDED，判定当日额度已耗尽。",
		Currency:    "USD",
		Amount:      0,
		Total:       total,
		Used:        total,
		UpdatedAt:   now.UTC().Format(time.RFC3339),
	}
	if total > 0 {
		item.Detail = fmt.Sprintf("今日已用 $%.2f / 日总 $%.2f", total, total)
		item.Detail = appendResetCountdown(item.Detail, now, nextResetAtChinaTime(now, adapter.ResetHour, adapter.ResetMinute))
		item.Detail += " | 上游返回 DAILY_LIMIT_EXCEEDED。"
	} else {
		item.Detail = detail
	}
	return item, true
}

func resolveOfficialUsageEndpoint(baseURL string) (string, error) {
	root, err := providerRootURL(baseURL)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(root, "/") + "/v1/usage", nil
}

func (h *Handler) fetchOfficialUsageKey(ctx context.Context, endpoint string, headers map[string]string, key providerBalanceKey) (float64, float64, float64, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, 0, 0, err
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
		return 0, 0, 0, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return 0, 0, 0, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 0, 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return 0, 0, 0, errUnmarshal
	}

	total := 0.0
	if subscription, ok := payload["subscription"].(map[string]any); ok {
		if value, ok := firstFloatValue(subscription, "daily_limit_usd"); ok && value > 0 {
			total = value
		}
	}

	used := 0.0
	if usageNode, ok := payload["usage"].(map[string]any); ok {
		if todayNode, ok := usageNode["today"].(map[string]any); ok {
			if value, ok := firstFloatValue(todayNode, "actual_cost", "cost"); ok {
				used = value
			}
		}
	}
	if used <= 0 {
		if subscription, ok := payload["subscription"].(map[string]any); ok {
			if value, ok := firstFloatValue(subscription, "daily_usage_usd"); ok {
				used = value
			}
		}
	}

	remaining := 0.0
	hasRemaining := false
	if value, ok := firstFloatValue(payload, "remaining"); ok {
		remaining = value
		hasRemaining = true
	}
	if !hasRemaining && total > 0 {
		remaining = total - used
	}
	if remaining < 0 {
		remaining = 0
	}
	if total <= 0 && (remaining > 0 || used > 0) {
		total = remaining + used
	}
	return remaining, total, used, nil
}
