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

const yunyiQuotaEndpoint = "https://yunyi.rdzhvip.com/user/api/v1/me"

func isYunyiProvider(req providerBalanceRequest) bool {
	host := providerHost(req.BaseURL)
	if strings.Contains(host, "yunyi.rdzhvip.com") {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(req.DisplayName))
	return strings.Contains(name, "yunyi") || strings.Contains(name, "云驿")
}

func (h *Handler) fetchYunyiQuota(ctx context.Context, req providerBalanceRequest) providerBalanceItem {
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: req.DisplayName,
		Status:      "error",
		Label:       "额度查询失败",
		Detail:      "未拿到可用的云驿 quota 响应。",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	if len(req.Keys) == 0 {
		item.Detail = "当前 provider 未配置 API key。"
		return item
	}

	var lastErr error
	quotaURL := resolveYunyiQuotaEndpoint(req.BaseURL)
	now := time.Now()
	for _, key := range req.Keys {
		apiKey := strings.TrimSpace(key.APIKey)
		if apiKey == "" {
			continue
		}
		remaining, total, used, status, nextResetAt, errFetch := h.fetchYunyiQuotaKey(ctx, quotaURL, req.Headers, key)
		if errFetch != nil {
			lastErr = errFetch
			continue
		}
		item.Status = "ok"
		if !status {
			item.Status = "partial"
		}
		item.Kind = "remaining_balance"
		item.Currency = "USD"
		item.Amount = remaining
		item.Total = total
		item.Used = used
		item.Label = fmt.Sprintf("当日剩余额度 $%.2f", remaining)
		item.Detail = fmt.Sprintf("今日已用 $%.2f / 日总 $%.2f", used, total)
		item.Detail = appendResetCountdown(item.Detail, now, nextResetAt)
		if !status {
			item.Detail += " | 账号状态不是 active"
		}
		return item
	}

	if lastErr != nil {
		item.Detail = lastErr.Error()
	}
	return item
}

func resolveYunyiQuotaEndpoint(baseURL string) string {
	root, err := providerRootURL(baseURL)
	if err == nil {
		host := providerHost(baseURL)
		switch {
		case strings.Contains(host, "cdn1.yunyi.cfd"),
			strings.Contains(host, "ai.last.ee"),
			strings.Contains(host, "yunyi.rdzhvip.com"):
			return yunyiQuotaEndpoint
		default:
			return strings.TrimRight(root, "/") + "/user/api/v1/me"
		}
	}
	return yunyiQuotaEndpoint
}

func (h *Handler) fetchYunyiQuotaKey(ctx context.Context, endpoint string, headers map[string]string, key providerBalanceKey) (float64, float64, float64, bool, time.Time, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, 0, 0, false, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key.APIKey))
	req.Header.Set("User-Agent", "cc-switch/1.0")
	req.Header.Set("Accept", "application/json")
	applySupplementalHeaders(req, headers, map[string]struct{}{
		"authorization": {},
		"user-agent":    {},
	})

	httpClient := &http.Client{
		Timeout:   providerBalanceTimeout,
		Transport: h.providerBalanceTransport(strings.TrimSpace(key.ProxyURL)),
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return 0, 0, 0, false, time.Time{}, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return 0, 0, 0, false, time.Time{}, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 0, 0, 0, false, time.Time{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Status string `json:"status"`
		Quota  struct {
			DailyQuota     any    `json:"daily_quota"`
			DailySpent     any    `json:"daily_spent"`
			DailyRemaining any    `json:"daily_remaining"`
			NextResetAt    string `json:"next_reset_at"`
		} `json:"quota"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return 0, 0, 0, false, time.Time{}, errUnmarshal
	}

	total := anyToFloat(payload.Quota.DailyQuota) / 100.0
	used := anyToFloat(payload.Quota.DailySpent) / 100.0
	remaining := anyToFloat(payload.Quota.DailyRemaining) / 100.0
	if remaining <= 0 && total > 0 {
		remaining = total - used
	}
	if total <= 0 && (remaining > 0 || used > 0) {
		total = remaining + used
	}
	if used <= 0 && total > 0 && remaining >= 0 {
		used = total - remaining
	}
	if remaining < 0 {
		remaining = 0
	}
	var nextResetAt time.Time
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(payload.Quota.NextResetAt)); err == nil {
		nextResetAt = parsed
	}
	return remaining, total, used, strings.EqualFold(strings.TrimSpace(payload.Status), "active"), nextResetAt, nil
}
