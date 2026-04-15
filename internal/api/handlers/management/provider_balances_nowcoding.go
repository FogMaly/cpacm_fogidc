package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func isNowCodingProvider(req providerBalanceRequest) bool {
	host := providerHost(req.BaseURL)
	if strings.Contains(host, "nowcoding.ai") {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(req.DisplayName))
	prefix := strings.ToLower(strings.TrimSpace(req.Prefix))
	return strings.Contains(name, "nowcoding") || strings.Contains(prefix, "nowcoding")
}

func (h *Handler) fetchNowCodingQuota(ctx context.Context, req providerBalanceRequest) providerBalanceItem {
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: req.DisplayName,
		Status:      "unsupported",
		Label:       "NowCoding 余额需要控制台认证",
		Detail:      "请在该渠道 headers 中配置 X-NewAPI-Username/X-NewAPI-Password，或 X-NewAPI-Access-Token/X-NewAPI-User-Id。仅 API key 无法查询余额。",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	cfg := extractNewAPIConfig(req.Headers)
	hasLogin := strings.TrimSpace(cfg.Username) != "" && strings.TrimSpace(cfg.Password) != ""
	hasAccessToken := strings.TrimSpace(cfg.AccessToken) != "" && strings.TrimSpace(cfg.UserID) != ""
	if !hasLogin && !hasAccessToken {
		return item
	}

	root, err := providerRootURL(req.BaseURL)
	if err != nil {
		item.Status = "error"
		item.Label = "NowCoding 余额查询失败"
		item.Detail = err.Error()
		return h.providerBalanceWithCacheFallback(req, item)
	}

	proxyURL := ""
	if len(req.Keys) > 0 {
		proxyURL = strings.TrimSpace(req.Keys[0].ProxyURL)
	}

	statusInfo, wallet, topup, subscription, topupErr, subscriptionErr, err := h.fetchNowCodingConsoleSnapshot(ctx, req.Headers, cfg, root, proxyURL)
	if err != nil {
		item.Status = "error"
		item.Label = "NowCoding 余额查询失败"
		item.Detail = err.Error()
		return h.providerBalanceWithCacheFallback(req, item)
	}

	if applied := applyConsoleSubscriptionBalance(&item, statusInfo, wallet, topup, subscription, topupErr, subscriptionErr, "NowCoding"); applied {
		h.storeProviderBalanceCache(req, item)
		return item
	}

	currency := strings.TrimSpace(statusInfo.QuotaDisplayType)
	if currency == "" {
		currency = "USD"
	}
	symbol := balanceCurrencySymbol(currency, statusInfo.CustomCurrencySymbol)
	remaining := convertNewAPIQuota(wallet.Quota, statusInfo)
	used := convertNewAPIQuota(wallet.UsedQuota, statusInfo)

	item.Status = "ok"
	item.Kind = "remaining_balance"
	item.Currency = currency
	item.Amount = remaining
	item.Used = used
	item.Total = remaining + used
	item.Label = fmt.Sprintf("当前余额 %s%.2f", symbol, remaining)

	detailParts := []string{
		fmt.Sprintf("历史消耗 %s%.2f", symbol, used),
	}
	if topup.SuccessTopupCount > 0 {
		detailParts = append(detailParts, fmt.Sprintf("成功充值 ¥%.2f / %d 笔", topup.SuccessTopupMoney, topup.SuccessTopupCount))
	}
	if topup.SubscriptionOrderCount > 0 {
		detailParts = append(detailParts, fmt.Sprintf("订阅订单 %d 笔", topup.SubscriptionOrderCount))
	}
	if wallet.RequestCount > 0 {
		detailParts = append(detailParts, fmt.Sprintf("请求数 %d", wallet.RequestCount))
	}
	detailParts = append(detailParts, "来源: NowCoding /console/topup")
	if subscriptionErr != nil {
		detailParts = append(detailParts, "订阅读取失败: "+subscriptionErr.Error())
	}
	if topupErr != nil {
		detailParts = append(detailParts, "充值账单读取失败: "+topupErr.Error())
	}
	item.Detail = strings.Join(detailParts, " | ")

	h.storeProviderBalanceCache(req, item)
	return item
}

func applyConsoleSubscriptionBalance(
	item *providerBalanceItem,
	statusInfo newAPIStatusInfo,
	wallet newAPIWalletInfo,
	topup nowCodingTopupSummary,
	subscription nowCodingSubscriptionSummary,
	topupErr error,
	subscriptionErr error,
	sourceName string,
) bool {
	if item == nil {
		return false
	}

	preferSubscription := subscription.HasActive &&
		(subscription.BillingPreference == "subscription_first" || subscription.BillingPreference == "subscription_only")
	if !preferSubscription {
		return false
	}

	currency := strings.TrimSpace(statusInfo.QuotaDisplayType)
	if currency == "" {
		currency = "USD"
	}
	symbol := balanceCurrencySymbol(currency, statusInfo.CustomCurrencySymbol)
	subscriptionRemaining := convertNewAPIQuota(subscription.AmountTotal-subscription.AmountUsed, statusInfo)
	subscriptionUsed := convertNewAPIQuota(subscription.AmountUsed, statusInfo)
	subscriptionTotal := convertNewAPIQuota(subscription.AmountTotal, statusInfo)
	if subscriptionRemaining < 0 {
		subscriptionRemaining = 0
	}

	item.Status = "ok"
	item.Kind = "remaining_balance"
	item.Currency = currency
	item.Amount = subscriptionRemaining
	item.Used = subscriptionUsed
	item.Total = subscriptionTotal
	item.Label = fmt.Sprintf("当日订阅额度 %s%.2f", symbol, subscriptionRemaining)

	detailParts := []string{
		fmt.Sprintf("今日已用 %s%.2f / 日总 %s%.2f", symbol, subscriptionUsed, symbol, subscriptionTotal),
	}
	if !subscription.NextResetAt.IsZero() {
		detailParts[0] = appendResetCountdown(detailParts[0], time.Now(), subscription.NextResetAt)
	}
	if !subscription.EndTime.IsZero() {
		detailParts = append(detailParts, "订阅到期 "+subscription.EndTime.Local().Format("2006-01-02 15:04"))
	}
	if wallet.RequestCount > 0 {
		detailParts = append(detailParts, fmt.Sprintf("请求数 %d", wallet.RequestCount))
	}
	if topup.SuccessTopupCount > 0 {
		detailParts = append(detailParts, fmt.Sprintf("充值账单 ¥%.2f / %d 笔", topup.SuccessTopupMoney, topup.SuccessTopupCount))
	}

	sourceName = strings.TrimSpace(sourceName)
	if sourceName == "" {
		sourceName = "控制台"
	}
	detailParts = append(detailParts, "来源: "+sourceName+" 订阅 /console/topup")
	if subscriptionErr != nil {
		detailParts = append(detailParts, "订阅读取失败: "+subscriptionErr.Error())
	}
	if topupErr != nil {
		detailParts = append(detailParts, "充值账单读取失败: "+topupErr.Error())
	}
	item.Detail = strings.Join(detailParts, " | ")
	return true
}

type nowCodingTopupItem struct {
	TradeNo string  `json:"trade_no"`
	Money   float64 `json:"money"`
	Status  string  `json:"status"`
}

type nowCodingTopupSummary struct {
	SuccessTopupMoney      float64
	SuccessTopupCount      int
	SubscriptionOrderCount int
}

type nowCodingSubscriptionSummary struct {
	BillingPreference string
	HasActive         bool
	AmountTotal       float64
	AmountUsed        float64
	NextResetAt       time.Time
	EndTime           time.Time
}

func (h *Handler) fetchNowCodingConsoleSnapshot(ctx context.Context, headers map[string]string, cfg newAPIBalanceConfig, root, proxyURL string) (newAPIStatusInfo, newAPIWalletInfo, nowCodingTopupSummary, nowCodingSubscriptionSummary, error, error, error) {
	client, jar, err := h.newAPIClient(proxyURL)
	if err != nil {
		return newAPIStatusInfo{}, newAPIWalletInfo{}, nowCodingTopupSummary{}, nowCodingSubscriptionSummary{}, nil, nil, err
	}

	if accessUserID, ok := parseNewAPIUserID(cfg.UserID); ok && strings.TrimSpace(cfg.AccessToken) != "" {
		statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
		if err != nil {
			return newAPIStatusInfo{}, newAPIWalletInfo{}, nowCodingTopupSummary{}, nowCodingSubscriptionSummary{}, nil, nil, err
		}
		wallet, err := h.fetchNewAPIUserSelf(ctx, client, root, headers, accessUserID)
		if err != nil {
			return newAPIStatusInfo{}, newAPIWalletInfo{}, nowCodingTopupSummary{}, nowCodingSubscriptionSummary{}, nil, nil, err
		}
		topup, topupErr := h.fetchNowCodingTopupSummary(ctx, client, root, headers, accessUserID)
		subscription, subscriptionErr := h.fetchNowCodingSubscriptionSummary(ctx, client, root, headers, accessUserID)
		return statusInfo, wallet, topup, subscription, topupErr, subscriptionErr, nil
	}

	sessionKey := newAPISessionKey(root, cfg.Username)
	if cached, ok := h.loadNewAPISession(sessionKey); ok {
		applyNewAPISessionCookies(jar, root, cached)
		statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
		if err == nil {
			wallet, err := h.fetchNewAPIUserSelf(ctx, client, root, headers, cached.UserID)
			if err == nil {
				topup, topupErr := h.fetchNowCodingTopupSummary(ctx, client, root, headers, cached.UserID)
				subscription, subscriptionErr := h.fetchNowCodingSubscriptionSummary(ctx, client, root, headers, cached.UserID)
				h.storeNewAPISession(sessionKey, cached.UserID, currentNewAPISessionCookies(jar, root))
				return statusInfo, wallet, topup, subscription, topupErr, subscriptionErr, nil
			}
		}
		h.deleteNewAPISession(sessionKey)
	}

	userID, err := h.newAPILogin(ctx, client, root, headers, cfg)
	if err != nil {
		return newAPIStatusInfo{}, newAPIWalletInfo{}, nowCodingTopupSummary{}, nowCodingSubscriptionSummary{}, nil, nil, err
	}
	h.storeNewAPISession(sessionKey, userID, currentNewAPISessionCookies(jar, root))

	statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
	if err != nil {
		return newAPIStatusInfo{}, newAPIWalletInfo{}, nowCodingTopupSummary{}, nowCodingSubscriptionSummary{}, nil, nil, err
	}
	wallet, err := h.fetchNewAPIUserSelf(ctx, client, root, headers, userID)
	if err != nil {
		return newAPIStatusInfo{}, newAPIWalletInfo{}, nowCodingTopupSummary{}, nowCodingSubscriptionSummary{}, nil, nil, err
	}
	topup, topupErr := h.fetchNowCodingTopupSummary(ctx, client, root, headers, userID)
	subscription, subscriptionErr := h.fetchNowCodingSubscriptionSummary(ctx, client, root, headers, userID)
	h.storeNewAPISession(sessionKey, userID, currentNewAPISessionCookies(jar, root))
	return statusInfo, wallet, topup, subscription, topupErr, subscriptionErr, nil
}

func (h *Handler) fetchNowCodingTopupSummary(ctx context.Context, client *http.Client, root string, headers map[string]string, userID int) (nowCodingTopupSummary, error) {
	type topupPayload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Total int                  `json:"total"`
			Items []nowCodingTopupItem `json:"items"`
		} `json:"data"`
	}

	fetchPage := func(page, pageSize int) (topupPayload, error) {
		ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
		defer cancel()

		endpoint := fmt.Sprintf("%s/api/user/topup/self?p=%d&page_size=%d", strings.TrimRight(root, "/"), page, pageSize)
		req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, endpoint, nil)
		if err != nil {
			return topupPayload{}, err
		}
		req.Header.Set("Accept", "application/json")
		if userID > 0 {
			req.Header.Set("New-API-User", strconv.Itoa(userID))
		}
		if accessToken := strings.TrimSpace(headerValueCI(headers, "X-NewAPI-Access-Token", "X-New-API-Access-Token")); accessToken != "" {
			req.Header.Set("Authorization", "Bearer "+accessToken)
		}
		applySupplementalHeaders(req, headers, map[string]struct{}{
			"authorization":          {},
			"cookie":                 {},
			"new-api-user":           {},
			"x-newapi-username":      {},
			"x-new-api-username":     {},
			"x-newapi-password":      {},
			"x-new-api-password":     {},
			"x-newapi-token-name":    {},
			"x-new-api-token-name":   {},
			"x-newapi-access-token":  {},
			"x-new-api-access-token": {},
			"x-newapi-user-id":       {},
			"x-new-api-user-id":      {},
		})

		resp, errDo := client.Do(req)
		if errDo != nil {
			return topupPayload{}, errDo
		}
		defer func() { _ = resp.Body.Close() }()

		body, errRead := io.ReadAll(resp.Body)
		if errRead != nil {
			return topupPayload{}, errRead
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return topupPayload{}, fmt.Errorf("nowcoding topup HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var payload topupPayload
		if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
			return topupPayload{}, errUnmarshal
		}
		if !payload.Success {
			if strings.TrimSpace(payload.Message) == "" {
				return topupPayload{}, fmt.Errorf("nowcoding topup request failed")
			}
			return topupPayload{}, fmt.Errorf("nowcoding topup request failed: %s", strings.TrimSpace(payload.Message))
		}
		return payload, nil
	}

	const pageSize = 100
	first, err := fetchPage(1, pageSize)
	if err != nil {
		return nowCodingTopupSummary{}, err
	}

	summary := summarizeNowCodingTopupItems(first.Data.Items)
	totalPages := 1
	if first.Data.Total > len(first.Data.Items) {
		totalPages = (first.Data.Total + pageSize - 1) / pageSize
	}
	for page := 2; page <= totalPages && page <= 10; page++ {
		payload, err := fetchPage(page, pageSize)
		if err != nil {
			return summary, err
		}
		mergeNowCodingTopupSummary(&summary, summarizeNowCodingTopupItems(payload.Data.Items))
	}
	return summary, nil
}

func (h *Handler) fetchNowCodingSubscriptionSummary(ctx context.Context, client *http.Client, root string, headers map[string]string, userID int) (nowCodingSubscriptionSummary, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, strings.TrimRight(root, "/")+"/api/subscription/self", nil)
	if err != nil {
		return nowCodingSubscriptionSummary{}, err
	}
	req.Header.Set("Accept", "application/json")
	if userID > 0 {
		req.Header.Set("New-API-User", strconv.Itoa(userID))
	}
	if accessToken := strings.TrimSpace(headerValueCI(headers, "X-NewAPI-Access-Token", "X-New-API-Access-Token")); accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	applySupplementalHeaders(req, headers, map[string]struct{}{
		"authorization":          {},
		"cookie":                 {},
		"new-api-user":           {},
		"x-newapi-username":      {},
		"x-new-api-username":     {},
		"x-newapi-password":      {},
		"x-new-api-password":     {},
		"x-newapi-token-name":    {},
		"x-new-api-token-name":   {},
		"x-newapi-access-token":  {},
		"x-new-api-access-token": {},
		"x-newapi-user-id":       {},
		"x-new-api-user-id":      {},
	})

	resp, errDo := client.Do(req)
	if errDo != nil {
		return nowCodingSubscriptionSummary{}, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nowCodingSubscriptionSummary{}, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nowCodingSubscriptionSummary{}, fmt.Errorf("nowcoding subscription HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			BillingPreference string `json:"billing_preference"`
			Subscriptions     []struct {
				Subscription struct {
					AmountTotal any    `json:"amount_total"`
					AmountUsed  any    `json:"amount_used"`
					Status      string `json:"status"`
					NextReset   int64  `json:"next_reset_time"`
					EndTime     int64  `json:"end_time"`
				} `json:"subscription"`
			} `json:"subscriptions"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nowCodingSubscriptionSummary{}, errUnmarshal
	}
	if !payload.Success {
		if strings.TrimSpace(payload.Message) == "" {
			return nowCodingSubscriptionSummary{}, fmt.Errorf("nowcoding subscription request failed")
		}
		return nowCodingSubscriptionSummary{}, fmt.Errorf("nowcoding subscription request failed: %s", strings.TrimSpace(payload.Message))
	}

	summary := nowCodingSubscriptionSummary{
		BillingPreference: strings.TrimSpace(payload.Data.BillingPreference),
	}
	for _, entry := range payload.Data.Subscriptions {
		if !strings.EqualFold(strings.TrimSpace(entry.Subscription.Status), "active") {
			continue
		}
		summary.HasActive = true
		summary.AmountTotal += anyToFloat(entry.Subscription.AmountTotal)
		summary.AmountUsed += anyToFloat(entry.Subscription.AmountUsed)
		if entry.Subscription.NextReset > 0 {
			resetAt := time.Unix(entry.Subscription.NextReset, 0)
			if summary.NextResetAt.IsZero() || resetAt.Before(summary.NextResetAt) {
				summary.NextResetAt = resetAt
			}
		}
		if entry.Subscription.EndTime > 0 {
			endAt := time.Unix(entry.Subscription.EndTime, 0)
			if summary.EndTime.IsZero() || endAt.After(summary.EndTime) {
				summary.EndTime = endAt
			}
		}
	}
	return summary, nil
}

func summarizeNowCodingTopupItems(items []nowCodingTopupItem) nowCodingTopupSummary {
	var summary nowCodingTopupSummary
	for _, item := range items {
		tradeNo := strings.ToLower(strings.TrimSpace(item.TradeNo))
		if strings.HasPrefix(tradeNo, "sub") {
			if strings.EqualFold(strings.TrimSpace(item.Status), "success") {
				summary.SubscriptionOrderCount++
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(item.Status), "success") {
			continue
		}
		summary.SuccessTopupMoney += item.Money
		summary.SuccessTopupCount++
	}
	return summary
}

func mergeNowCodingTopupSummary(dst *nowCodingTopupSummary, src nowCodingTopupSummary) {
	if dst == nil {
		return
	}
	dst.SuccessTopupMoney += src.SuccessTopupMoney
	dst.SuccessTopupCount += src.SuccessTopupCount
	dst.SubscriptionOrderCount += src.SubscriptionOrderCount
}
