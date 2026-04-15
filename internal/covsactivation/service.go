package covsactivation

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const AccessProviderName = "covs-activation"

type Service struct {
	mu             sync.RWMutex
	cfg            *config.Config
	configFilePath string
	state          activationState
}

type activationState struct {
	Cards       map[string]cardRecord       `json:"cards"`
	Activations map[string]activationRecord `json:"activations"`
}

type cardRecord struct {
	Code         string `json:"code"`
	Type         string `json:"type"`
	Product      string `json:"product"`
	DurationDays int    `json:"duration_days"`
	Disabled     bool   `json:"disabled,omitempty"`
	Remark       string `json:"remark,omitempty"`
}

type activationRecord struct {
	Token       string         `json:"token"`
	CardCode    string         `json:"card_code"`
	DeviceID    string         `json:"device_id"`
	DeviceInfo  map[string]any `json:"device_info,omitempty"`
	ActivatedAt time.Time      `json:"activated_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

type ActivationRequest struct {
	CardCode   string         `json:"cardCode"`
	DeviceID   string         `json:"deviceId"`
	DeviceInfo map[string]any `json:"deviceInfo"`
}

type ActivationResponse struct {
	ActivatedAt       string         `json:"activatedAt"`
	AnthropicBaseURL  string         `json:"anthropicBaseUrl,omitempty"`
	APIKey            string         `json:"apiKey"`
	CardType          string         `json:"cardType"`
	ExpiresAt         string         `json:"expiresAt"`
	KeyID             string         `json:"keyId"`
	OpenAPI           map[string]any `json:"openApi,omitempty"`
	PreviewOnly       bool           `json:"previewOnly"`
	Product           string         `json:"product"`
	SettingsUpdatedAt string         `json:"settingsUpdatedAt,omitempty"`
	Token             string         `json:"token"`
}

type AuthResult struct {
	Token       string
	Product     string
	CardType    string
	ModelPrefix string
	ActivatedAt time.Time
	ExpiresAt   time.Time
}

func NewService(cfg *config.Config, configFilePath string) *Service {
	s := &Service{
		cfg:            cfg,
		configFilePath: configFilePath,
		state: activationState{
			Cards:       make(map[string]cardRecord),
			Activations: make(map[string]activationRecord),
		},
	}
	s.reloadLocked()
	return s
}

func (s *Service) SetConfig(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	s.reloadLocked()
}

func (s *Service) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabledLocked()
}

func (s *Service) AuthenticateToken(token string) (*AuthResult, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabledLocked() {
		return nil, false
	}

	record, ok := s.state.Activations[token]
	if !ok {
		return nil, false
	}
	if !record.ExpiresAt.IsZero() && time.Now().After(record.ExpiresAt) {
		delete(s.state.Activations, token)
		_ = s.persistLocked()
		return nil, false
	}
	card, ok := s.state.Cards[record.CardCode]
	if !ok {
		return nil, false
	}

	return &AuthResult{
		Token:       token,
		Product:     normalizeProduct(card.Product),
		CardType:    normalizeCardType(card.Type),
		ModelPrefix: s.defaultClaudePrefixLocked(),
		ActivatedAt: record.ActivatedAt,
		ExpiresAt:   record.ExpiresAt,
	}, true
}

func (s *Service) Activate(req ActivationRequest) (*ActivationResponse, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabledLocked() {
		return nil, http.StatusNotFound, fmt.Errorf("activation service disabled")
	}

	cardCode := strings.TrimSpace(req.CardCode)
	deviceID := strings.TrimSpace(req.DeviceID)
	if cardCode == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("卡密为空")
	}
	if deviceID == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("deviceId 为空")
	}

	card, ok := s.state.Cards[cardCode]
	if !ok || card.Disabled {
		return nil, http.StatusBadRequest, fmt.Errorf("卡密无效或已禁用")
	}

	if existing, ok := s.findActivationByCardCodeLocked(cardCode); ok {
		if !existing.ExpiresAt.IsZero() && time.Now().After(existing.ExpiresAt) {
			delete(s.state.Activations, existing.Token)
		} else if existing.DeviceID != deviceID {
			return nil, http.StatusBadRequest, fmt.Errorf("该卡已绑定其他设备")
		} else {
			return s.buildActivationResponseLocked(card, existing), http.StatusOK, nil
		}
	}

	token, err := newToken()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("生成 token 失败")
	}
	now := time.Now().UTC()
	record := activationRecord{
		Token:       token,
		CardCode:    cardCode,
		DeviceID:    deviceID,
		DeviceInfo:  cloneMap(req.DeviceInfo),
		ActivatedAt: now,
		ExpiresAt:   now.Add(time.Duration(cardDurationDays(card)) * 24 * time.Hour),
	}
	s.state.Activations[token] = record
	if err := s.persistLocked(); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return s.buildActivationResponseLocked(card, record), http.StatusOK, nil
}

func (s *Service) reloadLocked() {
	if s.state.Cards == nil {
		s.state.Cards = make(map[string]cardRecord)
	}
	if s.state.Activations == nil {
		s.state.Activations = make(map[string]activationRecord)
	}
	if path := s.stateFilePathLocked(); path != "" {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			var next activationState
			if json.Unmarshal(data, &next) == nil {
				if next.Cards == nil {
					next.Cards = make(map[string]cardRecord)
				}
				if next.Activations == nil {
					next.Activations = make(map[string]activationRecord)
				}
				s.state = next
			}
		}
	}
	s.syncSeedCardsLocked()
	_ = s.persistLocked()
}

func (s *Service) syncSeedCardsLocked() {
	if s.cfg == nil {
		return
	}
	for _, seeded := range s.cfg.COVSActivation.Cards {
		code := strings.TrimSpace(seeded.Code)
		if code == "" {
			continue
		}
		current, exists := s.state.Cards[code]
		if !exists {
			current = cardRecord{Code: code}
		}
		current.Type = normalizeCardType(firstNonEmpty(seeded.Type, current.Type))
		current.Product = normalizeProduct(firstNonEmpty(seeded.Product, current.Product))
		current.DurationDays = maxInt(seeded.DurationDays, current.DurationDays)
		if current.DurationDays <= 0 {
			current.DurationDays = defaultDurationDays(current.Type)
		}
		current.Disabled = seeded.Disabled
		if strings.TrimSpace(seeded.Remark) != "" {
			current.Remark = strings.TrimSpace(seeded.Remark)
		}
		s.state.Cards[code] = current
	}
}

func (s *Service) buildActivationResponseLocked(card cardRecord, record activationRecord) *ActivationResponse {
	product := normalizeProduct(card.Product)
	resp := &ActivationResponse{
		ActivatedAt:       record.ActivatedAt.Format(time.RFC3339),
		APIKey:            record.Token,
		CardType:          normalizeCardType(card.Type),
		ExpiresAt:         record.ExpiresAt.Format(time.RFC3339),
		KeyID:             card.Code,
		OpenAPI:           map[string]any{"billingType": 1},
		PreviewOnly:       false,
		Product:           product,
		SettingsUpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Token:             record.Token,
	}
	if product == "claude" {
		resp.AnthropicBaseURL = s.publicBaseURLLocked()
	}
	return resp
}

func (s *Service) persistLocked() error {
	path := s.stateFilePathLocked()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("prepare activation state directory: %w", err)
	}
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal activation state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write activation state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("commit activation state: %w", err)
	}
	return nil
}

func (s *Service) enabledLocked() bool {
	return s.cfg != nil && s.cfg.COVSActivation.Enable
}

func (s *Service) stateFilePathLocked() string {
	if s.cfg == nil {
		return ""
	}
	path := strings.TrimSpace(s.cfg.COVSActivation.StateFile)
	if path == "" {
		baseDir := filepath.Dir(s.configFilePath)
		if strings.TrimSpace(baseDir) == "" {
			baseDir = "."
		}
		path = filepath.Join(baseDir, ".cpapi-state", "covs-activation-state.json")
	}
	if !filepath.IsAbs(path) {
		baseDir := filepath.Dir(s.configFilePath)
		if strings.TrimSpace(baseDir) == "" {
			baseDir = "."
		}
		path = filepath.Join(baseDir, path)
	}
	return path
}

func (s *Service) publicBaseURLLocked() string {
	if s.cfg == nil {
		return "http://127.0.0.1:8080"
	}
	if base := strings.TrimSpace(s.cfg.COVSActivation.PublicBaseURL); base != "" {
		return strings.TrimRight(base, "/")
	}
	host := strings.TrimSpace(s.cfg.Host)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if s.cfg.TLS.Enable {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, s.cfg.Port)
}

func (s *Service) defaultClaudePrefixLocked() string {
	if s.cfg == nil {
		return "covs"
	}
	prefix := strings.TrimSpace(s.cfg.COVSActivation.DefaultClaudeProviderPrefix)
	if prefix == "" {
		prefix = "covs"
	}
	return prefix
}

func (s *Service) findActivationByCardCodeLocked(cardCode string) (activationRecord, bool) {
	for _, record := range s.state.Activations {
		if record.CardCode == cardCode {
			return record, true
		}
	}
	return activationRecord{}, false
}

func normalizeProduct(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "claude") {
		return "claude"
	}
	return "claude"
}

func normalizeCardType(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "year":
		return "year"
	case "lifetime":
		return "lifetime"
	default:
		return "month"
	}
}

func defaultDurationDays(cardType string) int {
	switch normalizeCardType(cardType) {
	case "year":
		return 365
	case "lifetime":
		return 36500
	default:
		return 30
	}
}

func cardDurationDays(card cardRecord) int {
	if card.DurationDays > 0 {
		return card.DurationDays
	}
	return defaultDurationDays(card.Type)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func newToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-covs-" + hex.EncodeToString(buf), nil
}

func cloneMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
