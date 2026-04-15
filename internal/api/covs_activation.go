package api

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/covsactivation"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (s *Server) registerCOVSActivationRoutes() {
	if s == nil || s.engine == nil || s.covsActivation == nil {
		return
	}
	// /api/user/* is already claimed by the Amp module.
	// Requests to the documented activation endpoints are intercepted there
	// via handleCOVSActivationAmpBypass and routed to these handlers.
}

func (s *Server) handleCOVSActivate(c *gin.Context) {
	if s == nil || s.covsActivation == nil || !s.covsActivation.Enabled() {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	var req covsactivation.ActivationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "请求体无效", "type": "invalid_request_error"}})
		return
	}
	resp, status, err := s.covsActivation.Activate(req)
	if err != nil {
		c.JSON(status, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}
	c.JSON(status, resp)
}

func (s *Server) handleCOVSActivationAmpBypass(c *gin.Context) bool {
	if s == nil || s.covsActivation == nil || !s.covsActivation.Enabled() || c == nil || c.Request == nil {
		return false
	}
	switch {
	case c.Request.Method == http.MethodPost && c.Request.URL.Path == "/api/user/activate":
		s.handleCOVSActivate(c)
		return true
	default:
		return false
	}
}

func (s *Server) covsActivationRequestMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.covsActivation == nil || !s.covsActivation.Enabled() {
			c.Next()
			return
		}
		if c.GetString("accessProvider") != covsactivation.AccessProviderName {
			c.Next()
			return
		}
		product := strings.TrimSpace(accessMetadataValue(c, "product"))
		if product != "claude" {
			c.Next()
			return
		}
		switch c.FullPath() {
		case "/v1/messages", "/v1/messages/count_tokens":
			rewriteCOVSModelPrefix(c, accessMetadataValue(c, "model_prefix"))
		case "/v1/chat/completions":
			if covsShouldTreatChatCompletionsAsNativeClaude(c) {
				rewriteCOVSModelPrefix(c, accessMetadataValue(c, "model_prefix"))
			}
		}
		c.Next()
	}
}

func covsShouldTreatChatCompletionsAsNativeClaude(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	if !strings.HasSuffix(strings.TrimSpace(c.FullPath()), "/chat/completions") {
		return false
	}
	raw, err := c.GetRawData()
	if err != nil {
		return false
	}
	c.Request.Body = ioNopCloser(raw)
	c.Request.ContentLength = int64(len(raw))
	return cpamsLooksLikeNativeClaudeIngress(c, raw)
}

func rewriteCOVSModelPrefix(c *gin.Context, prefix string) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return
	}
	raw, err := c.GetRawData()
	if err != nil || len(raw) == 0 {
		return
	}
	model := strings.TrimSpace(gjson.GetBytes(raw, "model").String())
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if model == "" || prefix == "" || strings.Contains(model, "/") {
		c.Request.Body = ioNopCloser(raw)
		return
	}
	updated, err := sjson.SetBytes(raw, "model", prefix+"/"+model)
	if err != nil {
		updated = raw
	}
	c.Request.Body = ioNopCloser(updated)
	c.Request.ContentLength = int64(len(updated))
}

func accessMetadataValue(c *gin.Context, key string) string {
	if c == nil || key == "" {
		return ""
	}
	raw, ok := c.Get("accessMetadata")
	if !ok {
		return ""
	}
	meta, ok := raw.(map[string]string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(meta[key])
}

func ioNopCloser(body []byte) *readCloser {
	return &readCloser{Reader: bytes.NewReader(body)}
}

type readCloser struct {
	*bytes.Reader
}

func (r *readCloser) Close() error { return nil }
