package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	cachedProviderBalanceTTL = 20 * time.Minute
	newAPISessionTTL         = 30 * time.Minute
)

type newAPIBalanceConfig struct {
	Username    string
	Password    string
	TokenName   string
	AccessToken string
	UserID      string
}

type cachedProviderBalance struct {
	Item      providerBalanceItem
	ExpiresAt time.Time
}

type newAPISession struct {
	UserID    int
	Cookies   []*http.Cookie
	ExpiresAt time.Time
}

type newAPIStatusInfo struct {
	QuotaDisplayType           string
	QuotaPerUnit               float64
	USDExchangeRate            float64
	CustomCurrencySymbol       string
	CustomCurrencyExchangeRate float64
}

type newAPIWalletInfo struct {
	Quota        float64
	UsedQuota    float64
	RequestCount int
}

func isNewAPIProvider(req providerBalanceRequest) bool {
	cfg := extractNewAPIConfig(req.Headers)
	return (cfg.Username != "" && cfg.Password != "") || (cfg.AccessToken != "" && cfg.UserID != "")
}

func extractNewAPIConfig(headers map[string]string) newAPIBalanceConfig {
	return newAPIBalanceConfig{
		Username:    headerValueCI(headers, "X-NewAPI-Username", "X-New-API-Username"),
		Password:    headerValueCI(headers, "X-NewAPI-Password", "X-New-API-Password"),
		TokenName:   headerValueCI(headers, "X-NewAPI-Token-Name", "X-New-API-Token-Name"),
		AccessToken: headerValueCI(headers, "X-NewAPI-Access-Token", "X-New-API-Access-Token"),
		UserID:      headerValueCI(headers, "X-NewAPI-User-Id", "X-New-API-User-Id", "X-NewAPI-User-ID", "X-New-API-User-ID"),
	}
}

func headerValueCI(headers map[string]string, keys ...string) string {
	if len(headers) == 0 || len(keys) == 0 {
		return ""
	}
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		for existing, value := range headers {
			if strings.EqualFold(strings.TrimSpace(existing), trimmed) {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func (h *Handler) fetchNewAPIQuota(ctx context.Context, req providerBalanceRequest) providerBalanceItem {
	item := providerBalanceItem{
		Section:     req.Section,
		Index:       req.Index,
		ProviderKey: req.ProviderKey,
		DisplayName: req.DisplayName,
		Status:      "error",
		Label:       "额度查询失败",
		Detail:      "未拿到可用的 New API 钱包额度响应。",
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	cfg := extractNewAPIConfig(req.Headers)
	if cfg.AccessToken == "" && (cfg.Username == "" || cfg.Password == "") {
		item.Status = "unsupported"
		item.Label = "未配置 New API 认证信息"
		item.Detail = "请配置 X-NewAPI-Username/X-NewAPI-Password，或 X-NewAPI-Access-Token/X-NewAPI-User-Id。"
		return item
	}
	if len(req.Keys) == 0 {
		item.Detail = "当前 provider 未配置 API key。"
		return item
	}

	root, err := providerRootURL(req.BaseURL)
	if err != nil {
		item.Detail = err.Error()
		return h.providerBalanceWithCacheFallback(req, item)
	}

	proxyURL := ""
	if len(req.Keys) > 0 {
		proxyURL = strings.TrimSpace(req.Keys[0].ProxyURL)
	}

	statusInfo, wallet, topup, subscription, topupErr, subscriptionErr, snapshotErr := h.fetchNowCodingConsoleSnapshot(ctx, req.Headers, cfg, root, proxyURL)
	if snapshotErr == nil {
		if applied := applyConsoleSubscriptionBalance(&item, statusInfo, wallet, topup, subscription, topupErr, subscriptionErr, req.DisplayName); applied {
			h.storeProviderBalanceCache(req, item)
			return item
		}

		currency := strings.TrimSpace(statusInfo.QuotaDisplayType)
		if currency == "" {
			currency = "USD"
		}
		remaining := convertNewAPIQuota(wallet.Quota, statusInfo)
		used := convertNewAPIQuota(wallet.UsedQuota, statusInfo)
		total := remaining + used

		item.Status = "ok"
		item.Kind = "remaining_balance"
		item.Currency = currency
		item.Amount = remaining
		item.Used = used
		item.Total = total
		item.Label = fmt.Sprintf("钱包余额 %s%.2f", balanceCurrencySymbol(currency, statusInfo.CustomCurrencySymbol), remaining)
		item.Detail = fmt.Sprintf("累计已用 %s%.2f / 累计总额 %s%.2f | 来源: 账户钱包", balanceCurrencySymbol(currency, statusInfo.CustomCurrencySymbol), used, balanceCurrencySymbol(currency, statusInfo.CustomCurrencySymbol), total)
		if wallet.RequestCount > 0 {
			item.Detail += fmt.Sprintf(" | 请求数 %d", wallet.RequestCount)
		}
		h.storeProviderBalanceCache(req, item)
		return item
	}

	tokenInfo, statusInfo, tokenErr := h.fetchNewAPITokenQuotaFallback(ctx, root, req.Headers, cfg, req.DisplayName, req.Keys)
	if tokenErr != nil {
		item.Detail = snapshotErr.Error()
		if tokenErr != nil && tokenErr.Error() != "" {
			item.Detail += " | token 兜底失败: " + tokenErr.Error()
		}
		return h.providerBalanceWithCacheFallback(req, item)
	}

	item.Status = "ok"
	item.Kind = "remaining_balance"
	item.Currency = strings.TrimSpace(statusInfo.QuotaDisplayType)
	if item.Currency == "" {
		item.Currency = "USD"
	}
	item.Amount = convertNewAPIQuota(tokenInfo.RemainQuota, statusInfo)
	item.Used = convertNewAPIQuota(tokenInfo.UsedQuota, statusInfo)
	if !tokenInfo.UnlimitedQuota {
		item.Total = item.Amount + item.Used
	}
	if tokenInfo.UnlimitedQuota {
		item.Label = "无限额度"
		item.Detail = "钱包额度读取失败，已回退到 token unlimited_quota。"
	} else {
		symbol := balanceCurrencySymbol(item.Currency, statusInfo.CustomCurrencySymbol)
		item.Label = fmt.Sprintf("剩余额度 %s%.2f", symbol, item.Amount)
		item.Detail = fmt.Sprintf("钱包额度读取失败，已回退到 token 配额 | 已用 %s%.2f / 总额 %s%.2f", symbol, item.Used, symbol, item.Total)
	}
	if strings.TrimSpace(tokenInfo.Name) != "" {
		if item.Detail != "" {
			item.Detail += " | "
		}
		item.Detail += "token: " + strings.TrimSpace(tokenInfo.Name)
	}
	h.storeProviderBalanceCache(req, item)
	return item
}

type newAPITokenQuotaInfo struct {
	Name           string  `json:"name"`
	Key            string  `json:"key"`
	RemainQuota    float64 `json:"remain_quota"`
	UsedQuota      float64 `json:"used_quota"`
	UnlimitedQuota bool    `json:"unlimited_quota"`
}

func (h *Handler) fetchNewAPITokenQuotaKey(ctx context.Context, root string, headers map[string]string, key providerBalanceKey, cfg newAPIBalanceConfig, fallbackName string) (newAPITokenQuotaInfo, newAPIStatusInfo, error) {
	client, jar, err := h.newAPIClient(strings.TrimSpace(key.ProxyURL))
	if err != nil {
		return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, err
	}

	sessionKey := newAPISessionKey(root, cfg.Username)
	if accessUserID, ok := parseNewAPIUserID(cfg.UserID); ok && strings.TrimSpace(cfg.AccessToken) != "" {
		statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
		if err != nil {
			return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, err
		}
		tokens, err := h.fetchNewAPITokens(ctx, client, root, headers, accessUserID)
		if err != nil {
			return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, err
		}
		return h.matchNewAPIToken(tokens, statusInfo, cfg, key, fallbackName)
	}

	if cached, ok := h.loadNewAPISession(sessionKey); ok {
		applyNewAPISessionCookies(jar, root, cached)
		statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
		if err != nil {
			return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, err
		}
		userID := cached.UserID
		tokens, err := h.fetchNewAPITokens(ctx, client, root, headers, userID)
		if err == nil {
			h.storeNewAPISession(sessionKey, userID, currentNewAPISessionCookies(jar, root))
			return h.matchNewAPIToken(tokens, statusInfo, cfg, key, fallbackName)
		}
		h.deleteNewAPISession(sessionKey)
	}

	userID, err := h.newAPILogin(ctx, client, root, headers, cfg)
	if err != nil {
		return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, err
	}
	h.storeNewAPISession(sessionKey, userID, currentNewAPISessionCookies(jar, root))

	statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
	if err != nil {
		return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, err
	}

	tokens, err := h.fetchNewAPITokens(ctx, client, root, headers, userID)
	if err != nil {
		return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, err
	}
	h.storeNewAPISession(sessionKey, userID, currentNewAPISessionCookies(jar, root))
	return h.matchNewAPIToken(tokens, statusInfo, cfg, key, fallbackName)
}

func (h *Handler) matchNewAPIToken(tokens []newAPITokenQuotaInfo, statusInfo newAPIStatusInfo, cfg newAPIBalanceConfig, key providerBalanceKey, fallbackName string) (newAPITokenQuotaInfo, newAPIStatusInfo, error) {

	expectedName := strings.TrimSpace(cfg.TokenName)
	if expectedName == "" {
		expectedName = strings.TrimSpace(fallbackName)
	}
	token, found := findNewAPIToken(tokens, expectedName, strings.TrimSpace(key.APIKey))
	if !found {
		if expectedName != "" {
			return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, fmt.Errorf("new-api token not found for %q", expectedName)
		}
		return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, fmt.Errorf("new-api token not found for configured api key")
	}
	return token, statusInfo, nil
}

func (h *Handler) newAPILogin(ctx context.Context, client *http.Client, root string, headers map[string]string, cfg newAPIBalanceConfig) (int, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	payload := map[string]string{
		"username": cfg.Username,
		"password": cfg.Password,
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodPost, strings.TrimRight(root, "/")+"/api/user/login", bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
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
		return 0, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return 0, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 0, fmt.Errorf("new-api login HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payloadResp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			ID int `json:"id"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payloadResp); errUnmarshal != nil {
		return 0, errUnmarshal
	}
	if !payloadResp.Success {
		if strings.TrimSpace(payloadResp.Message) == "" {
			return 0, fmt.Errorf("new-api login failed")
		}
		return 0, fmt.Errorf("new-api login failed: %s", strings.TrimSpace(payloadResp.Message))
	}
	if payloadResp.Data.ID <= 0 {
		return 0, fmt.Errorf("new-api login returned invalid user id")
	}
	return payloadResp.Data.ID, nil
}

func (h *Handler) fetchNewAPIStatus(ctx context.Context, client *http.Client, root string, headers map[string]string) (newAPIStatusInfo, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, strings.TrimRight(root, "/")+"/api/status", nil)
	if err != nil {
		return newAPIStatusInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
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
		return newAPIStatusInfo{}, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return newAPIStatusInfo{}, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return newAPIStatusInfo{}, fmt.Errorf("new-api status HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			QuotaDisplayType           string  `json:"quota_display_type"`
			QuotaPerUnit               float64 `json:"quota_per_unit"`
			USDExchangeRate            float64 `json:"usd_exchange_rate"`
			CustomCurrencySymbol       string  `json:"custom_currency_symbol"`
			CustomCurrencyExchangeRate float64 `json:"custom_currency_exchange_rate"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return newAPIStatusInfo{}, errUnmarshal
	}
	info := newAPIStatusInfo{
		QuotaDisplayType:           strings.TrimSpace(payload.Data.QuotaDisplayType),
		QuotaPerUnit:               payload.Data.QuotaPerUnit,
		USDExchangeRate:            payload.Data.USDExchangeRate,
		CustomCurrencySymbol:       strings.TrimSpace(payload.Data.CustomCurrencySymbol),
		CustomCurrencyExchangeRate: payload.Data.CustomCurrencyExchangeRate,
	}
	if info.QuotaDisplayType == "" {
		info.QuotaDisplayType = "USD"
	}
	if info.QuotaPerUnit <= 0 {
		info.QuotaPerUnit = 500000
	}
	if info.USDExchangeRate <= 0 {
		info.USDExchangeRate = 1
	}
	if info.CustomCurrencyExchangeRate <= 0 {
		info.CustomCurrencyExchangeRate = 1
	}
	return info, nil
}

func (h *Handler) fetchNewAPITokens(ctx context.Context, client *http.Client, root string, headers map[string]string, userID int) ([]newAPITokenQuotaInfo, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, strings.TrimRight(root, "/")+"/api/token/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if userID > 0 {
		req.Header.Set("New-API-User", fmt.Sprintf("%d", userID))
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
		return nil, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("new-api token list HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Items []struct {
				Name           string `json:"name"`
				Key            string `json:"key"`
				RemainQuota    any    `json:"remain_quota"`
				UsedQuota      any    `json:"used_quota"`
				UnlimitedQuota bool   `json:"unlimited_quota"`
			} `json:"items"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if !payload.Success {
		if strings.TrimSpace(payload.Message) == "" {
			return nil, fmt.Errorf("new-api token list request failed")
		}
		return nil, fmt.Errorf("new-api token list request failed: %s", strings.TrimSpace(payload.Message))
	}

	items := make([]newAPITokenQuotaInfo, 0, len(payload.Data.Items))
	for _, token := range payload.Data.Items {
		items = append(items, newAPITokenQuotaInfo{
			Name:           strings.TrimSpace(token.Name),
			Key:            strings.TrimSpace(token.Key),
			RemainQuota:    anyToFloat(token.RemainQuota),
			UsedQuota:      anyToFloat(token.UsedQuota),
			UnlimitedQuota: token.UnlimitedQuota,
		})
	}
	return items, nil
}

func findNewAPIToken(tokens []newAPITokenQuotaInfo, expectedName, apiKey string) (newAPITokenQuotaInfo, bool) {
	expectedName = strings.TrimSpace(expectedName)
	apiKey = strings.TrimSpace(apiKey)

	if expectedName != "" {
		for _, token := range tokens {
			if strings.EqualFold(strings.TrimSpace(token.Name), expectedName) {
				return token, true
			}
		}
	}
	if apiKey != "" {
		for _, token := range tokens {
			if newAPIMaskedKeyMatches(strings.TrimSpace(token.Key), apiKey) {
				return token, true
			}
		}
	}
	if expectedName == "" && apiKey == "" && len(tokens) == 1 {
		return tokens[0], true
	}
	return newAPITokenQuotaInfo{}, false
}

func newAPIMaskedKeyMatches(maskedKey, apiKey string) bool {
	maskedKey = strings.TrimSpace(maskedKey)
	apiKey = strings.TrimSpace(apiKey)
	if maskedKey == "" || apiKey == "" {
		return false
	}
	firstStar := strings.Index(maskedKey, "*")
	lastStar := strings.LastIndex(maskedKey, "*")
	if firstStar < 0 || lastStar < 0 || lastStar < firstStar {
		return strings.EqualFold(maskedKey, apiKey)
	}
	prefix := strings.TrimSpace(maskedKey[:firstStar])
	suffix := strings.TrimSpace(maskedKey[lastStar+1:])
	if prefix != "" && !strings.HasPrefix(apiKey, prefix) {
		return false
	}
	if suffix != "" && !strings.HasSuffix(apiKey, suffix) {
		return false
	}
	return len(apiKey) >= len(prefix)+len(suffix)
}

func balanceCurrencySymbol(currency string, customSymbol string) string {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "USD":
		return "$"
	case "CNY":
		return "¥"
	case "EUR":
		return "€"
	case "CUSTOM":
		return strings.TrimSpace(customSymbol)
	default:
		return ""
	}
}

func convertNewAPIQuota(raw float64, statusInfo newAPIStatusInfo) float64 {
	switch strings.ToUpper(strings.TrimSpace(statusInfo.QuotaDisplayType)) {
	case "TOKENS":
		return raw
	case "CNY":
		return raw / statusInfo.QuotaPerUnit * statusInfo.USDExchangeRate
	case "CUSTOM":
		return raw / statusInfo.QuotaPerUnit * statusInfo.CustomCurrencyExchangeRate
	default:
		return raw / statusInfo.QuotaPerUnit
	}
}

func (h *Handler) newAPIClient(proxyURL string) (*http.Client, http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, nil, err
	}
	client := &http.Client{
		Timeout:   providerBalanceTimeout,
		Transport: h.providerBalanceTransport(strings.TrimSpace(proxyURL)),
		Jar:       jar,
	}
	return client, jar, nil
}

func (h *Handler) fetchNewAPIWallet(ctx context.Context, root string, headers map[string]string, cfg newAPIBalanceConfig, proxyURL string) (newAPIWalletInfo, newAPIStatusInfo, error) {
	client, jar, err := h.newAPIClient(proxyURL)
	if err != nil {
		return newAPIWalletInfo{}, newAPIStatusInfo{}, err
	}

	sessionKey := newAPISessionKey(root, cfg.Username)
	if accessUserID, ok := parseNewAPIUserID(cfg.UserID); ok && strings.TrimSpace(cfg.AccessToken) != "" {
		statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
		if err != nil {
			return newAPIWalletInfo{}, newAPIStatusInfo{}, err
		}
		wallet, err := h.fetchNewAPIUserSelf(ctx, client, root, headers, accessUserID)
		if err != nil {
			return newAPIWalletInfo{}, newAPIStatusInfo{}, err
		}
		return wallet, statusInfo, nil
	}

	if cached, ok := h.loadNewAPISession(sessionKey); ok {
		applyNewAPISessionCookies(jar, root, cached)
		statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
		if err == nil {
			wallet, err := h.fetchNewAPIUserSelf(ctx, client, root, headers, cached.UserID)
			if err == nil {
				h.storeNewAPISession(sessionKey, cached.UserID, currentNewAPISessionCookies(jar, root))
				return wallet, statusInfo, nil
			}
		}
		h.deleteNewAPISession(sessionKey)
	}

	userID, err := h.newAPILogin(ctx, client, root, headers, cfg)
	if err != nil {
		return newAPIWalletInfo{}, newAPIStatusInfo{}, err
	}
	h.storeNewAPISession(sessionKey, userID, currentNewAPISessionCookies(jar, root))

	statusInfo, err := h.fetchNewAPIStatus(ctx, client, root, headers)
	if err != nil {
		return newAPIWalletInfo{}, newAPIStatusInfo{}, err
	}
	wallet, err := h.fetchNewAPIUserSelf(ctx, client, root, headers, userID)
	if err != nil {
		return newAPIWalletInfo{}, newAPIStatusInfo{}, err
	}
	h.storeNewAPISession(sessionKey, userID, currentNewAPISessionCookies(jar, root))
	return wallet, statusInfo, nil
}

func (h *Handler) fetchNewAPIUserSelf(ctx context.Context, client *http.Client, root string, headers map[string]string, userID int) (newAPIWalletInfo, error) {
	ctxTimeout, cancel := context.WithTimeout(defaultBalanceContext(ctx), providerBalanceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, strings.TrimRight(root, "/")+"/api/user/self", nil)
	if err != nil {
		return newAPIWalletInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
	if userID > 0 {
		req.Header.Set("New-API-User", fmt.Sprintf("%d", userID))
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
		return newAPIWalletInfo{}, errDo
	}
	defer func() { _ = resp.Body.Close() }()

	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return newAPIWalletInfo{}, errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return newAPIWalletInfo{}, fmt.Errorf("new-api self HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Quota        any `json:"quota"`
			UsedQuota    any `json:"used_quota"`
			RequestCount int `json:"request_count"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil {
		return newAPIWalletInfo{}, errUnmarshal
	}
	if !payload.Success {
		if strings.TrimSpace(payload.Message) == "" {
			return newAPIWalletInfo{}, fmt.Errorf("new-api self request failed")
		}
		return newAPIWalletInfo{}, fmt.Errorf("new-api self request failed: %s", strings.TrimSpace(payload.Message))
	}
	return newAPIWalletInfo{
		Quota:        anyToFloat(payload.Data.Quota),
		UsedQuota:    anyToFloat(payload.Data.UsedQuota),
		RequestCount: payload.Data.RequestCount,
	}, nil
}

func (h *Handler) fetchNewAPITokenQuotaFallback(ctx context.Context, root string, headers map[string]string, cfg newAPIBalanceConfig, fallbackName string, keys []providerBalanceKey) (newAPITokenQuotaInfo, newAPIStatusInfo, error) {
	var lastErr error
	for _, key := range keys {
		apiKey := strings.TrimSpace(key.APIKey)
		if apiKey == "" {
			continue
		}
		tokenInfo, statusInfo, err := h.fetchNewAPITokenQuotaKey(ctx, root, headers, key, cfg, fallbackName)
		if err == nil {
			return tokenInfo, statusInfo, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, lastErr
	}
	return newAPITokenQuotaInfo{}, newAPIStatusInfo{}, fmt.Errorf("current provider has no valid api key")
}

func newAPISessionKey(root, username string) string {
	return strings.TrimSpace(root) + "||" + strings.ToLower(strings.TrimSpace(username))
}

func (h *Handler) storeProviderBalanceCache(req providerBalanceRequest, item providerBalanceItem) {
	if h == nil {
		return
	}
	h.providerBalancesMu.Lock()
	defer h.providerBalancesMu.Unlock()
	h.providerBalances[h.providerBalanceCacheKey(req)] = cachedProviderBalance{
		Item:      item,
		ExpiresAt: time.Now().Add(cachedProviderBalanceTTL),
	}
}

func (h *Handler) providerBalanceWithCacheFallback(req providerBalanceRequest, item providerBalanceItem) providerBalanceItem {
	if h == nil {
		return item
	}
	h.providerBalancesMu.Lock()
	defer h.providerBalancesMu.Unlock()

	cacheKey := h.providerBalanceCacheKey(req)
	cached, ok := h.providerBalances[cacheKey]
	if !ok {
		return item
	}
	if time.Now().After(cached.ExpiresAt) {
		delete(h.providerBalances, cacheKey)
		return item
	}

	cachedItem := cached.Item
	if strings.TrimSpace(item.Detail) != "" {
		if strings.TrimSpace(cachedItem.Detail) != "" {
			cachedItem.Detail += " | "
		}
		cachedItem.Detail += "当前刷新失败，展示上次成功数据: " + strings.TrimSpace(item.Detail)
	}
	return cachedItem
}

func (h *Handler) providerBalanceCacheKey(req providerBalanceRequest) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", req.Section, req.ProviderKey, req.Index, strings.TrimSpace(req.DisplayName), strings.TrimSpace(req.BaseURL))
}

func (h *Handler) loadNewAPISession(sessionKey string) (newAPISession, bool) {
	if h == nil {
		return newAPISession{}, false
	}
	h.newAPISessionsMu.Lock()
	defer h.newAPISessionsMu.Unlock()

	session, ok := h.newAPISessions[sessionKey]
	if !ok {
		return newAPISession{}, false
	}
	if session.UserID <= 0 || len(session.Cookies) == 0 || time.Now().After(session.ExpiresAt) {
		delete(h.newAPISessions, sessionKey)
		return newAPISession{}, false
	}
	return session, true
}

func (h *Handler) storeNewAPISession(sessionKey string, userID int, cookies []*http.Cookie) {
	if h == nil || userID <= 0 || len(cookies) == 0 {
		return
	}
	h.newAPISessionsMu.Lock()
	defer h.newAPISessionsMu.Unlock()
	h.newAPISessions[sessionKey] = newAPISession{
		UserID:    userID,
		Cookies:   cloneCookies(cookies),
		ExpiresAt: time.Now().Add(newAPISessionTTL),
	}
}

func (h *Handler) deleteNewAPISession(sessionKey string) {
	if h == nil {
		return
	}
	h.newAPISessionsMu.Lock()
	defer h.newAPISessionsMu.Unlock()
	delete(h.newAPISessions, sessionKey)
}

func applyNewAPISessionCookies(jar http.CookieJar, root string, session newAPISession) {
	if jar == nil || len(session.Cookies) == 0 {
		return
	}
	u, err := url.Parse(strings.TrimSpace(root))
	if err != nil {
		return
	}
	jar.SetCookies(u, cloneCookies(session.Cookies))
}

func currentNewAPISessionCookies(jar http.CookieJar, root string) []*http.Cookie {
	if jar == nil {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(root))
	if err != nil {
		return nil
	}
	return cloneCookies(jar.Cookies(u))
}

func cloneCookies(cookies []*http.Cookie) []*http.Cookie {
	if len(cookies) == 0 {
		return nil
	}
	out := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		cloned := *cookie
		out = append(out, &cloned)
	}
	return out
}

func parseNewAPIUserID(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}
