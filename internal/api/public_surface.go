package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
)

type cpamcPublicModel struct {
	ID        string   `json:"id"`
	Object    string   `json:"object"`
	Channel   string   `json:"channel"`
	Protocols []string `json:"protocols"`
}

type cpamcPublicHealth struct {
	ID             string   `json:"id"`
	Object         string   `json:"object"`
	Channel        string   `json:"channel"`
	Protocols      []string `json:"protocols,omitempty"`
	Status         string   `json:"status"`
	Score          float64  `json:"score,omitempty"`
	CandidateCount int      `json:"candidate_count,omitempty"`
	Continuation   string   `json:"continuation,omitempty"`
	TestedAt       string   `json:"tested_at,omitempty"`
	UpdatedAt      string   `json:"updated_at,omitempty"`
}

type cpamcPublicHealthProjection struct {
	Status         string
	Score          float64
	CandidateCount int
	Continuation   string
	TestedAt       time.Time
}

func markCPAMCPublicRequest() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("cpamc_public_strict", true)
		c.Next()
	}
}

func isCPAMCPublicRequest(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool("cpamc_public_strict")
}

func (s *Server) handleCPAMCPublicModels(c *gin.Context) {
	s.handleCPAMCPublicModelsWithChannel(c, strings.TrimSpace(c.Query("channel")))
}

func (s *Server) handleCPAMCPublicModelsForChannel(c *gin.Context) {
	s.handleCPAMCPublicModelsWithChannel(c, strings.TrimSpace(c.Param("channel")))
}

func (s *Server) handleCPAMCPublicModelsWithChannel(c *gin.Context, channelFilter string) {
	models, err := s.filteredCPAMCPublicModels(
		channelFilter,
		strings.TrimSpace(c.Query("model")),
	)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "invalid_cpamc_models_query",
			"detail": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   models,
	})
}

func (s *Server) handleCPAMCPublicHealth(c *gin.Context) {
	s.handleCPAMCPublicHealthWithChannel(c, strings.TrimSpace(c.Query("channel")))
}

func (s *Server) handleCPAMCPublicHealthForChannel(c *gin.Context) {
	s.handleCPAMCPublicHealthWithChannel(c, strings.TrimSpace(c.Param("channel")))
}

func (s *Server) handleCPAMCPublicHealthWithChannel(c *gin.Context, channelFilter string) {
	models, err := s.filteredCPAMCPublicModels(
		channelFilter,
		strings.TrimSpace(c.Query("model")),
	)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "invalid_cpamc_health_query",
			"detail": err.Error(),
		})
		return
	}
	if s == nil || s.cpamsResolver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cpams_resolver_unavailable"})
		return
	}

	now := time.Now().UTC()
	snapshot, snapshotUpdatedAt, err := s.cpamsResolver.loadSnapshotAllowStale(now)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "public_health_unavailable"})
		return
	}

	data := make([]cpamcPublicHealth, 0, len(models))
	for _, model := range models {
		candidates := buildCPAMSRequestedCandidates(model.Channel, model.ID, &snapshot, now)
		s.cpamsResolver.applyUnifiedHealth(model.Channel, model.ID, snapshotUpdatedAt, now, candidates)
		projection := publicHealthProjectionFromCandidates(model.Channel, candidates)
		data = append(data, cpamcPublicHealth{
			ID:             model.ID,
			Object:         "model_health",
			Channel:        model.Channel,
			Protocols:      cpamcProtocolsForChannel(model.Channel),
			Status:         projection.Status,
			Score:          projection.Score,
			CandidateCount: projection.CandidateCount,
			Continuation:   projection.Continuation,
			TestedAt:       formatCPAMCTime(projection.TestedAt),
			UpdatedAt:      formatCPAMCTime(snapshotUpdatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) filteredCPAMCPublicModels(channelFilter, modelFilter string) ([]cpamcPublicModel, error) {
	models := s.cpamcPublicModels()

	channel := ""
	if channelFilter != "" {
		var err error
		channel, err = normalizeCPAMSChannel(channelFilter)
		if err != nil {
			return nil, err
		}
	}

	model := strings.TrimSpace(modelFilter)
	if model != "" {
		if strings.Contains(model, "/") {
			return nil, errPublicModelNameRequired()
		}
		model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
		if model == "" {
			return nil, errPublicModelNameRequired()
		}
	}

	filtered := make([]cpamcPublicModel, 0, len(models))
	for _, item := range models {
		if channel != "" && item.Channel != channel {
			continue
		}
		if model != "" && !strings.EqualFold(item.ID, model) {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered, nil
}

func (s *Server) cpamcPublicModels() []cpamcPublicModel {
	reg := registry.GetGlobalRegistry()
	if reg == nil {
		return nil
	}

	seen := make(map[string]struct{})
	out := make([]cpamcPublicModel, 0)
	addModels := func(channel string, models []*registry.ModelInfo) {
		for _, model := range models {
			if model == nil {
				continue
			}
			id := normalizeCPAMCPublicModelID(channel, model.ID)
			if id == "" {
				continue
			}
			key := channel + ":" + strings.ToLower(id)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, cpamcPublicModel{
				ID:        id,
				Object:    "model",
				Channel:   channel,
				Protocols: cpamcProtocolsForChannel(channel),
			})
		}
	}

	addModels("codex", reg.GetAvailableModelsByProvider("codex"))
	addModels("claude", reg.GetAvailableModelsByProvider("claude"))

	sort.Slice(out, func(i, j int) bool {
		if out[i].Channel == out[j].Channel {
			return out[i].ID < out[j].ID
		}
		return out[i].Channel < out[j].Channel
	})
	return out
}

func normalizeCPAMCPublicModelID(channel, raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if aliasChannel, requested, ok := parseCPAMSModelAlias(trimmed); ok {
		if aliasChannel != channel {
			return ""
		}
		trimmed = requested
	}
	if prefix, upstream := splitCPAMSModel(trimmed); prefix != "" {
		if !cpamsChannelMatchesModel(channel, prefix, upstream) {
			return ""
		}
		trimmed = upstream
	}
	trimmed = strings.TrimSpace(thinking.ParseSuffix(trimmed).ModelName)
	if trimmed == "" || strings.Contains(trimmed, "/") {
		return ""
	}
	return trimmed
}

func cpamcProtocolsForChannel(channel string) []string {
	switch channel {
	case "codex":
		return []string{"chat/completions", "completions", "responses", "responses/compact"}
	case "claude":
		return []string{"messages", "messages/count_tokens"}
	default:
		return nil
	}
}

func publicHealthSummaryFromCandidates(candidates []cpamsCandidate) (string, float64) {
	bestRank := cpamsStatusUnknown
	bestScore := 0.0
	found := false
	for _, candidate := range candidates {
		score := candidate.UnifiedScore
		if score <= 0 {
			score = float64(candidate.Score)
		}
		if !found || candidate.StatusRank > bestRank || (candidate.StatusRank == bestRank && score > bestScore) {
			bestRank = candidate.StatusRank
			bestScore = score
			found = true
		}
	}
	switch bestRank {
	case cpamsStatusGreen:
		return "available", bestScore
	case cpamsStatusYellow:
		return "degraded", bestScore
	default:
		return "unavailable", bestScore
	}
}

func publicHealthProjectionFromCandidates(channel string, candidates []cpamsCandidate) cpamcPublicHealthProjection {
	status, score := publicHealthSummaryFromCandidates(candidates)
	projection := cpamcPublicHealthProjection{
		Status:         status,
		Score:          score,
		CandidateCount: len(candidates),
	}
	if len(candidates) == 0 {
		return projection
	}

	best := candidates[0]
	projection.TestedAt = best.LatestTestedAt
	if channel == "codex" {
		projection.Continuation = publicHealthContinuationStatus(best)
	}
	return projection
}

func publicHealthContinuationStatus(candidate cpamsCandidate) string {
	switch strings.ToLower(strings.TrimSpace(candidate.ResponsesContinuationStatus)) {
	case "ok":
		return "supported"
	case "failed":
		return "unsupported"
	}
	prefix, _ := splitCPAMSModel(candidate.FullModel)
	if cpamsSupportsStatefulCodexResponses(prefix, candidate.Reason, candidate.ResponsesContinuationStatus, candidate.ResponsesContinuationReason) {
		return "supported"
	}
	if strings.TrimSpace(candidate.ResponsesContinuationReason) != "" {
		return "unsupported"
	}
	return "unknown"
}

func formatCPAMCTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

func errPublicModelNameRequired() error {
	return fmt.Errorf("cpamc: public health queries only accept stable public model names")
}
