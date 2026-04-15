package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) getRateLimitStatus(c *gin.Context) {
	if s == nil || s.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "server_config_unavailable",
		})
		return
	}

	cfg := s.cfg
	quota := cfg.QuotaExceeded
	routingEngine := strings.TrimSpace(cfg.Routing.Engine)
	if routingEngine == "" {
		routingEngine = "v1"
	}
	routingStrategy := strings.TrimSpace(cfg.Routing.Strategy)
	if routingStrategy == "" {
		routingStrategy = "round-robin"
	}

	resp := gin.H{
		"generated_at":               time.Now().UTC().Format(time.RFC3339),
		"inbound_rate_limit_enabled": cfg.InboundRateLimit.PerKeyQPS > 0 || cfg.InboundRateLimit.GlobalConcurrency > 0,
		"inbound_rate_limit_note":    "configured_via_inbound_rate_limit",

		"disable_cooling":    cfg.DisableCooling,
		"request_retry":      cfg.RequestRetry,
		"max_retry_interval": cfg.MaxRetryInterval,
		"inbound_rate_limit": gin.H{
			"per_key_qps":        cfg.InboundRateLimit.PerKeyQPS,
			"global_concurrency": cfg.InboundRateLimit.GlobalConcurrency,
		},
		"quota_exceeded": gin.H{
			"switch_project":       quota.SwitchProject,
			"switch_preview_model": quota.SwitchPreviewModel,
		},
		"routing_engine":       routingEngine,
		"routing_strategy":     routingStrategy,
		"provider_concurrency": cfg.Routing.ProviderConcurrency,
	}

	if s != nil && s.handlers != nil && s.handlers.AuthManager != nil {
		resp["routing_snapshot"] = s.handlers.AuthManager.RoutingSnapshot(20)
	}

	if s.cpamsResolver != nil {
		resp["cpams"] = gin.H{
			"token_ttl_seconds": int64(s.cpamsResolver.TokenTTL().Seconds()),
			"snapshot_path":     s.cpamsResolver.SnapshotPath(),
		}
	}

	c.JSON(http.StatusOK, resp)
}
