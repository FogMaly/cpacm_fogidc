package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) getCPAMSStatus(c *gin.Context) {
	if s == nil || s.cpamsResolver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "cpams_resolver_unavailable",
		})
		return
	}

	channel := strings.TrimSpace(c.Query("channel"))
	states, err := s.cpamsResolver.Snapshot(channel)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "invalid_cpams_channel",
			"detail": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"channel":           strings.ToLower(channel),
		"token_ttl_seconds": int64(s.cpamsResolver.TokenTTL().Seconds()),
		"snapshot_path":     s.cpamsResolver.SnapshotPath(),
		"states":            states,
	})
}
