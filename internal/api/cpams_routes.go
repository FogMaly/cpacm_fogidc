package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const cpamsResponseIDCaptureLimit = 256 * 1024

func (s *Server) registerCPAMSRoutes(openaiHandlers *openai.OpenAIAPIHandler, claudeHandlers *claude.ClaudeCodeAPIHandler, responsesHandlers *openai.OpenAIResponsesAPIHandler) {
	if s == nil || s.engine == nil || s.accessManager == nil || openaiHandlers == nil || claudeHandlers == nil || responsesHandlers == nil {
		return
	}

	group := s.engine.Group("/cpamc")
	group.Use(AuthMiddleware(s.accessManager, s.inboundLimiter), markCPAMCPublicRequest())
	{
		group.GET("/models", s.handleCPAMCPublicModels)
		group.GET("/health", s.handleCPAMCPublicHealth)
		group.GET("/:channel/models", s.handleCPAMCPublicModelsForChannel)
		group.GET("/:channel/health", s.handleCPAMCPublicHealthForChannel)

		group.POST("/:channel/chat/completions", s.wrapCPAMSChatCompletionsRoute(openaiHandlers.ChatCompletions, claudeHandlers.ClaudeMessages))
		group.POST("/:channel/completions", s.wrapCPAMSRoute("codex", openaiHandlers.Completions))
		group.POST("/:channel/responses", s.wrapCPAMSRoute("codex", responsesHandlers.Responses))
		group.POST("/:channel/responses/compact", s.wrapCPAMSRoute("codex", responsesHandlers.Compact))

		group.POST("/:channel/messages", s.wrapCPAMSRoute("claude", claudeHandlers.ClaudeMessages))
		group.POST("/:channel/messages/count_tokens", s.wrapCPAMSRoute("claude", claudeHandlers.ClaudeCountTokens))
		group.POST("/:channel/v1/messages", s.wrapCPAMSRoute("claude", claudeHandlers.ClaudeMessages))
		group.POST("/:channel/v1/messages/count_tokens", s.wrapCPAMSRoute("claude", claudeHandlers.ClaudeCountTokens))
	}
}

func (s *Server) wrapCPAMSChatCompletionsRoute(openaiDownstream gin.HandlerFunc, claudeDownstream gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		channel, err := normalizeCPAMSChannel(c.Param("channel"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid_cpams_channel",
				"detail": err.Error(),
			})
			return
		}

		raw, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid_request_body",
				"detail": err.Error(),
			})
			return
		}

		if len(bytes.TrimSpace(raw)) == 0 {
			raw = []byte(`{}`)
		}
		strictPublic := isCPAMCPublicRequest(c)
		requestedModel, err := normalizeCPAMSRequestedModel(channel, strings.TrimSpace(gjson.GetBytes(raw, "model").String()), strictPublic)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid_cpams_model",
				"detail": err.Error(),
			})
			return
		}

		resolution, rewritten, err := s.resolveCPAMSRequest(c, channel, requestedModel, raw)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "cpams_no_stable_model",
				"channel": channel,
				"model":   requestedModel,
				"detail":  err.Error(),
			})
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(rewritten))
		c.Request.ContentLength = int64(len(rewritten))
		c.Request.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
		s.applyCPAMSResolution(c, channel, resolution)
		c.Set("cpams_route", strings.TrimSpace(c.FullPath()))

		conversationKey := extractCPAMSConversationKey(c, raw)
		responseCapture := wrapCPAMSResponseCapture(c)
		downstream := openaiDownstream
		if channel == "claude" && cpamsLooksLikeNativeClaudeIngress(c, raw) {
			downstream = claudeDownstream
		}
		if downstream == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "cpams_downstream_unavailable",
				"channel": channel,
			})
			return
		}

		downstream(c)
		cpamsMaybeLogClaudeContinuation(c, channel, requestedModel, resolution, raw, responseCapture)
		s.maybeRememberCPAMSResponseIDs(channel, requestedModel, resolution, c.Writer.Status(), unwrapCPAMSResponseIDs(responseCapture))
		s.maybeInvalidateCPAMSResolution(channel, requestedModel, resolution, conversationKey, c.Writer.Status())
		if cpamsHasSemanticToolUseMismatch(responseCapture) {
			s.cpamsResolver.InvalidateModel(channel, requestedModel, resolution.Model, conversationKey, resolution.SnapshotUpdated)
		}
	}
}

func (s *Server) wrapCPAMSModelAliasRoute(downstream gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid_request_body",
				"detail": err.Error(),
			})
			return
		}

		if len(bytes.TrimSpace(raw)) == 0 {
			raw = []byte(`{}`)
		}

		modelRaw := strings.TrimSpace(gjson.GetBytes(raw, "model").String())
		channel, requestedModel, hasAlias := parseCPAMSModelAlias(modelRaw)
		if !hasAlias {
			if forcedChannel, forcedRequested, ok := cpamsForcedRouteRequest(c, modelRaw, raw); ok {
				s.executeCPAMSResolvedDownstream(c, downstream, forcedChannel, forcedRequested, modelRaw, raw)
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(raw))
			c.Request.ContentLength = int64(len(raw))
			c.Request.Header.Set("Content-Length", strconv.Itoa(len(raw)))
			downstream(c)
			return
		}
		if cpamsShouldRejectNativeClaudeAliasOnOpenAIRoute(c, channel, cpamsProtocolHint(c), raw) {
			writeCPAMSNativeClaudeRouteError(c)
			return
		}

		if s == nil || s.cpamsResolver == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "cpams_resolver_unavailable",
			})
			return
		}

		s.executeCPAMSResolvedDownstream(c, downstream, channel, requestedModel, modelRaw, raw)
	}
}

func (s *Server) executeCPAMSResolvedDownstream(c *gin.Context, downstream gin.HandlerFunc, channel, requestedModel, aliasModel string, raw []byte) {
	if s == nil || s.cpamsResolver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "cpams_resolver_unavailable",
		})
		return
	}
	resolution, rewritten, err := s.resolveCPAMSRequest(c, channel, requestedModel, raw)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "cpams_no_stable_model",
			"channel": channel,
			"model":   requestedModel,
			"detail":  err.Error(),
		})
		return
	}

	c.Request.Body = io.NopCloser(bytes.NewReader(rewritten))
	c.Request.ContentLength = int64(len(rewritten))
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	s.applyCPAMSResolution(c, channel, resolution)
	if strings.TrimSpace(aliasModel) != "" {
		c.Set("cpams_alias_model", aliasModel)
	}
	conversationKey := extractCPAMSConversationKey(c, raw)
	responseCapture := wrapCPAMSResponseCapture(c)

	downstream(c)
	cpamsMaybeLogClaudeContinuation(c, channel, requestedModel, resolution, raw, responseCapture)
	s.maybeRememberCPAMSResponseIDs(channel, requestedModel, resolution, c.Writer.Status(), unwrapCPAMSResponseIDs(responseCapture))
	s.maybeInvalidateCPAMSResolution(channel, requestedModel, resolution, conversationKey, c.Writer.Status())
	if cpamsHasSemanticToolUseMismatch(responseCapture) {
		s.cpamsResolver.InvalidateModel(channel, requestedModel, resolution.Model, conversationKey, resolution.SnapshotUpdated)
	}
}

func cpamsForcedRouteRequest(c *gin.Context, modelRaw string, raw []byte) (channel, requestedModel string, ok bool) {
	if c == nil {
		return "", "", false
	}
	forced, _ := c.Get("cpams_force_channel")
	forcedChannel, _ := forced.(string)
	forcedChannel = strings.TrimSpace(forcedChannel)
	if forcedChannel == "" {
		return "", "", false
	}
	channel, err := normalizeCPAMSChannel(forcedChannel)
	if err != nil {
		return "", "", false
	}
	requestedModel, err = normalizeCPAMSRequestedModel(channel, modelRaw, isCPAMCPublicRequest(c))
	if err != nil {
		return "", "", false
	}
	if strings.TrimSpace(requestedModel) == "" {
		return "", "", false
	}
	return channel, requestedModel, true
}

func cpamsShouldRejectNativeClaudeAliasOnOpenAIRoute(c *gin.Context, channel, protocol string, raw []byte) bool {
	if channel != "claude" {
		return false
	}
	if c != nil {
		if allow, ok := c.Get("cpams_allow_native_claude_alias_on_openai_route"); ok {
			if allowed, ok := allow.(bool); ok && allowed {
				return false
			}
		}
	}
	switch protocol {
	case "chat/completions", "completions", "responses", "responses/compact":
	default:
		return false
	}
	return cpamsLooksLikeNativeClaudeIngress(c, raw)
}

func cpamsLooksLikeNativeClaudeIngress(c *gin.Context, raw []byte) bool {
	if c != nil && c.Request != nil {
		userAgent := strings.ToLower(strings.TrimSpace(c.Request.UserAgent()))
		if strings.Contains(userAgent, "claude-cli/") || strings.Contains(userAgent, "claude-code") {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(c.GetHeader("X-App")), "cli") {
			return true
		}
		for _, header := range []string{"Anthropic-Version", "Anthropic-Beta"} {
			if strings.TrimSpace(c.GetHeader(header)) != "" {
				return true
			}
		}
		for header := range c.Request.Header {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(header)), "x-stainless-") {
				return true
			}
		}
	}

	if len(raw) == 0 {
		return false
	}
	compact := strings.ToLower(string(raw))
	for _, marker := range []string{
		"x-anthropic-billing-header:",
		"resume this session with:",
		"you are claude code, anthropic's official cli for claude.",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

func writeCPAMSNativeClaudeRouteError(c *gin.Context) {
	if c == nil {
		return
	}
	const expectedPublic = "/v1/messages"
	const expectedCPAMC = "/cpamc/claude/messages"
	c.Header("X-CPAMS-Expected-Endpoint", expectedPublic)
	c.Header("X-CPAMS-Expected-CPAMC-Endpoint", expectedCPAMC)
	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{
			"message": fmt.Sprintf("claude native traffic must use %s or %s; /v1/chat/completions remains the OpenAI compatibility bridge", expectedPublic, expectedCPAMC),
			"type":    "invalid_request_error",
			"code":    "cpamc_native_claude_requires_messages",
		},
	})
}

func parseCPAMSModelAlias(modelRaw string) (channel string, requestedModel string, hasAlias bool) {
	parts := strings.Split(strings.TrimSpace(modelRaw), "/")
	if len(parts) < 2 {
		return "", "", false
	}

	switch strings.ToLower(strings.TrimSpace(parts[0])) {
	case "codex", "claude":
		requested := strings.TrimSpace(strings.Join(parts[1:], "/"))
		if requested == "" {
			return "", "", false
		}
		return strings.ToLower(strings.TrimSpace(parts[0])), requested, true
	case "cpams", "capms", "cpamc":
		if len(parts) < 3 {
			return "", "", false
		}
		channel = strings.ToLower(strings.TrimSpace(parts[1]))
		switch channel {
		case "codex", "claude":
			requested := strings.TrimSpace(strings.Join(parts[2:], "/"))
			if requested == "" {
				return "", "", false
			}
			return channel, requested, true
		default:
			return "", "", false
		}
	default:
		return "", "", false
	}
}

func (s *Server) wrapCPAMSRoute(expectedChannel string, downstream gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		channel, err := normalizeCPAMSChannel(c.Param("channel"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid_cpams_channel",
				"detail": err.Error(),
			})
			return
		}
		if channel != expectedChannel {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":            "cpams_channel_route_mismatch",
				"channel":          channel,
				"expected_channel": expectedChannel,
			})
			return
		}
		raw, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid_request_body",
				"detail": err.Error(),
			})
			return
		}

		if len(bytes.TrimSpace(raw)) == 0 {
			raw = []byte(`{}`)
		}

		strictPublic := isCPAMCPublicRequest(c)
		requestedModel, err := normalizeCPAMSRequestedModel(channel, strings.TrimSpace(gjson.GetBytes(raw, "model").String()), strictPublic)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid_cpams_model",
				"detail": err.Error(),
			})
			return
		}

		resolution, rewritten, err := s.resolveCPAMSRequest(c, channel, requestedModel, raw)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "cpams_no_stable_model",
				"channel": channel,
				"model":   requestedModel,
				"detail":  err.Error(),
			})
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(rewritten))
		c.Request.ContentLength = int64(len(rewritten))
		c.Request.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
		s.applyCPAMSResolution(c, channel, resolution)
		c.Set("cpams_route", strings.TrimSpace(c.FullPath()))
		conversationKey := extractCPAMSConversationKey(c, raw)
		responseCapture := wrapCPAMSResponseCapture(c)

		downstream(c)
		cpamsMaybeLogClaudeContinuation(c, channel, requestedModel, resolution, raw, responseCapture)
		s.maybeRememberCPAMSResponseIDs(channel, requestedModel, resolution, c.Writer.Status(), unwrapCPAMSResponseIDs(responseCapture))
		s.maybeInvalidateCPAMSResolution(channel, requestedModel, resolution, conversationKey, c.Writer.Status())
		if cpamsHasSemanticToolUseMismatch(responseCapture) {
			s.cpamsResolver.InvalidateModel(channel, requestedModel, resolution.Model, conversationKey, resolution.SnapshotUpdated)
		}
	}
}

func (s *Server) resolveCPAMSRequest(c *gin.Context, channel, requestedModel string, raw []byte) (cpamsResolution, []byte, error) {
	if s == nil || s.cpamsResolver == nil {
		return cpamsResolution{}, nil, fmt.Errorf("cpams_resolver_unavailable")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}
	defaultThinking := !cpamsHasExplicitThinkingConfig(requestedModel, raw)
	requestedForResolution := requestedModel
	if defaultThinking {
		requestedForResolution = cpamsEnsureThinkingSuffix(requestedForResolution, string(thinking.LevelXHigh))
	}
	protocolHint := cpamsProtocolHint(c)
	if channel == "claude" && cpamsLooksLikeNativeClaudeIngress(c, raw) && cpamsIsExternalClaudeCLIRequest(c) {
		protocolHint = cpamsAppendProtocolTrait(protocolHint, "claude-external-native")
	}
	requireStateful := cpamsRequiresStatefulCodexResponses(channel, protocolHint, raw)
	resolution, err := s.cpamsResolver.ResolveRequested(channel, requestedForResolution, extractCPAMSConversationKey(c, raw), s.cpamsRoutingStrategy(), protocolHint, requireStateful)
	if err != nil {
		return cpamsResolution{}, nil, err
	}
	if defaultThinking {
		resolution.Model = cpamsEnsureThinkingSuffix(resolution.Model, string(thinking.LevelXHigh))
		if resolution.RequestedModel != "" {
			resolution.RequestedModel = cpamsEnsureThinkingSuffix(resolution.RequestedModel, string(thinking.LevelXHigh))
		}
	}
	rewritten, err := sjson.SetBytes(raw, "model", resolution.Model)
	if err != nil {
		return cpamsResolution{}, nil, err
	}
	return resolution, rewritten, nil
}

func (s *Server) cpamsRoutingStrategy() string {
	if s == nil || s.cfg == nil {
		return "round-robin"
	}
	return normalizeCPAMSRoutingStrategy(s.cfg.Routing.Strategy)
}

func (s *Server) applyCPAMSResolution(c *gin.Context, channel string, resolution cpamsResolution) {
	if c == nil {
		return
	}
	if channel != "" {
		c.Set("cpams_channel", channel)
	}
	if resolution.Model != "" {
		c.Set("cpams_model", resolution.Model)
	}
	if resolution.RequestedModel != "" {
		c.Set("cpams_requested_model", resolution.RequestedModel)
	}
	if resolution.Decision != "" {
		c.Set("cpams_decision", resolution.Decision)
	}
	if channel != "" {
		c.Request.Header.Set("X-CPAMS-Channel", channel)
		c.Header("X-CPAMS-Channel", channel)
	}
	if resolution.RequestedModel != "" {
		c.Request.Header.Set("X-CPAMS-Requested-Model", resolution.RequestedModel)
		c.Header("X-CPAMS-Requested-Model", resolution.RequestedModel)
	}
	if resolution.Decision != "" {
		c.Request.Header.Set("X-CPAMS-Decision", resolution.Decision)
		c.Header("X-CPAMS-Decision", resolution.Decision)
	}
	if isCPAMCPublicRequest(c) {
		return
	}
	if resolution.Model != "" {
		c.Request.Header.Set("X-CPAMS-Model", resolution.Model)
		c.Header("X-CPAMS-Model", resolution.Model)
	}
}

func (s *Server) maybeInvalidateCPAMSResolution(channel, requestedModel string, resolution cpamsResolution, conversationKey string, status int) {
	if s == nil || s.cpamsResolver == nil {
		return
	}
	mode := cpamsInvalidationMode(status)
	if mode == cpamsInvalidateCooldown && cpamsShouldSuppressTransientInvalidation(resolution) {
		mode = cpamsInvalidateNone
	}
	switch mode {
	case cpamsInvalidateBlock:
		s.cpamsResolver.InvalidateModel(channel, requestedModel, resolution.Model, conversationKey, resolution.SnapshotUpdated)
	case cpamsInvalidateCooldown:
		s.cpamsResolver.CooldownModel(channel, requestedModel, resolution.Model, conversationKey, resolution.SnapshotUpdated, cpamsTransientFailureCooldown())
	default:
		return
	}
	if s.mgmt != nil {
		if prefix, _ := splitCPAMSModel(resolution.Model); prefix != "" {
			s.mgmt.NotifyAdaptiveProbeFailure(prefix, resolution.Model, status, fmt.Sprintf("cpams_invalidate_status_%d", status))
		}
	}
}

func cpamsShouldSuppressTransientInvalidation(resolution cpamsResolution) bool {
	if resolution.CandidateCount != 1 {
		return false
	}
	model := cpamsSelectionModelName(resolution.Model)
	if model == "" {
		return false
	}
	return cpamsIsBridgeCandidate(cpamsCandidate{FullModel: model})
}

func (s *Server) maybeRememberCPAMSResponseIDs(channel, requestedModel string, resolution cpamsResolution, status int, responseIDs []string) {
	if s == nil || s.cpamsResolver == nil || len(responseIDs) == 0 {
		return
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return
	}
	s.cpamsResolver.RememberResponseIDs(channel, requestedModel, resolution.Model, responseIDs, resolution.SnapshotUpdated)
}

type cpamsInvalidateMode int

const (
	cpamsInvalidateNone cpamsInvalidateMode = iota
	cpamsInvalidateCooldown
	cpamsInvalidateBlock
)

func cpamsInvalidationMode(status int) cpamsInvalidateMode {
	switch status {
	case http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusPaymentRequired,
		http.StatusRequestTimeout,
		http.StatusTooManyRequests:
		return cpamsInvalidateBlock
	default:
		if status >= http.StatusInternalServerError {
			return cpamsInvalidateCooldown
		}
		return cpamsInvalidateNone
	}
}

func cpamsTransientFailureCooldown() time.Duration {
	cooldown := 5 * time.Second
	if raw := strings.TrimSpace(os.Getenv("CPAMS_TRANSIENT_FAILURE_COOLDOWN_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return cooldown
}

type cpamsResponseCaptureWriter struct {
	gin.ResponseWriter
	bodyBuffer              bytes.Buffer
	lineBuffer              bytes.Buffer
	responseIDs             map[string]struct{}
	semanticToolUseMismatch bool
}

func wrapCPAMSResponseCapture(c *gin.Context) *cpamsResponseCaptureWriter {
	if c == nil {
		return nil
	}
	capture := &cpamsResponseCaptureWriter{
		ResponseWriter: c.Writer,
		responseIDs:    make(map[string]struct{}),
	}
	c.Writer = capture
	return capture
}

func unwrapCPAMSResponseIDs(capture *cpamsResponseCaptureWriter) []string {
	if capture == nil {
		return nil
	}
	return capture.ResponseIDs()
}

func cpamsHasSemanticToolUseMismatch(capture *cpamsResponseCaptureWriter) bool {
	if capture == nil {
		return false
	}
	if capture.semanticToolUseMismatch {
		return true
	}
	if capture.bodyBuffer.Len() == 0 {
		return false
	}
	return cpamsPayloadHasSemanticToolUseMismatch(capture.bodyBuffer.Bytes())
}

func (w *cpamsResponseCaptureWriter) Write(data []byte) (int, error) {
	if len(data) > 0 {
		w.capture(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *cpamsResponseCaptureWriter) WriteString(data string) (int, error) {
	if data != "" {
		w.capture([]byte(data))
	}
	return w.ResponseWriter.WriteString(data)
}

func (w *cpamsResponseCaptureWriter) capture(data []byte) {
	if w == nil || len(data) == 0 {
		return
	}
	if w.isStreamingResponse(data) {
		w.captureSSE(data)
		return
	}
	if w.bodyBuffer.Len() >= cpamsResponseIDCaptureLimit {
		return
	}
	remaining := cpamsResponseIDCaptureLimit - w.bodyBuffer.Len()
	if remaining > len(data) {
		remaining = len(data)
	}
	w.bodyBuffer.Write(data[:remaining])
}

func (w *cpamsResponseCaptureWriter) isStreamingResponse(data []byte) bool {
	if w == nil {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(w.Header().Get("Content-Type")))
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}
	return bytes.Contains(data, []byte("data:"))
}

func (w *cpamsResponseCaptureWriter) captureSSE(data []byte) {
	if w == nil || len(data) == 0 {
		return
	}
	if w.lineBuffer.Len() < cpamsResponseIDCaptureLimit {
		remaining := cpamsResponseIDCaptureLimit - w.lineBuffer.Len()
		if remaining > len(data) {
			remaining = len(data)
		}
		w.lineBuffer.Write(data[:remaining])
	}
	for {
		line, err := w.lineBuffer.ReadBytes('\n')
		if err != nil {
			if len(line) > 0 {
				w.lineBuffer.Reset()
				if len(line) > cpamsResponseIDCaptureLimit {
					line = line[len(line)-cpamsResponseIDCaptureLimit:]
				}
				w.lineBuffer.Write(line)
			}
			return
		}
		w.captureSSELine(bytes.TrimSpace(line))
	}
}

func (w *cpamsResponseCaptureWriter) captureSSELine(line []byte) {
	if w == nil || len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[5:])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if cpamsPayloadHasSemanticToolUseMismatch(payload) {
		w.semanticToolUseMismatch = true
	}
	if responseID := cpamsExtractResponseID(payload); responseID != "" {
		w.responseIDs[responseID] = struct{}{}
	}
}

func cpamsPayloadHasSemanticToolUseMismatch(raw []byte) bool {
	message := strings.TrimSpace(gjson.GetBytes(raw, "error.message").String())
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(raw, "message").String())
	}
	if message == "" {
		message = strings.TrimSpace(string(raw))
	}
	if message == "" {
		return false
	}
	return strings.Contains(strings.ToLower(message), "invalid tool_use_id in tool_result")
}

type cpamsClaudeContinuationLog struct {
	Event                  string `json:"event"`
	Route                  string `json:"route,omitempty"`
	RequestedModel         string `json:"requested_model,omitempty"`
	SelectedModel          string `json:"selected_model,omitempty"`
	RequestToolResultCount int    `json:"request_tool_result_count"`
	ResponseStatus         int    `json:"response_status"`
	ResponseStopReason     string `json:"response_stop_reason,omitempty"`
	ResponseToolUseCount   int    `json:"response_tool_use_count"`
	ResponseToolCallCount  int    `json:"response_tool_call_count"`
	ResponseErrorMessage   string `json:"response_error_message,omitempty"`
}

type cpamsClaudeContinuationResponseAnalysis struct {
	StopReason    string
	ToolUseCount  int
	ToolCallCount int
	ErrorMessage  string
}

func cpamsMaybeLogClaudeContinuation(c *gin.Context, channel, requestedModel string, resolution cpamsResolution, raw []byte, capture *cpamsResponseCaptureWriter) {
	if strings.TrimSpace(strings.ToLower(channel)) != "claude" {
		return
	}
	requestToolResultCount := cpamsRequestToolResultCount(raw)
	if requestToolResultCount == 0 {
		return
	}

	analysis := cpamsAnalyzeClaudeContinuationResponse(nil)
	if capture != nil {
		analysis = cpamsAnalyzeClaudeContinuationResponse(capture.bodyBuffer.Bytes())
	}
	payload := cpamsClaudeContinuationLog{
		Event: "cpams_claude_continuation",
		Route: strings.TrimSpace(func() string {
			if c == nil {
				return ""
			}
			return c.FullPath()
		}()),
		RequestedModel:         strings.TrimSpace(requestedModel),
		SelectedModel:          strings.TrimSpace(resolution.Model),
		RequestToolResultCount: requestToolResultCount,
		ResponseStatus: func() int {
			if c == nil || c.Writer == nil {
				return 0
			}
			return c.Writer.Status()
		}(),
		ResponseStopReason:    strings.TrimSpace(analysis.StopReason),
		ResponseToolUseCount:  analysis.ToolUseCount,
		ResponseToolCallCount: analysis.ToolCallCount,
		ResponseErrorMessage:  strings.TrimSpace(analysis.ErrorMessage),
	}
	rawLog, err := json.Marshal(payload)
	if err != nil {
		log.WithError(err).Warn("cpams: failed to marshal claude continuation log")
		return
	}
	log.Info(string(rawLog))
}

func cpamsRequestToolResultCount(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	count := 0
	messages := gjson.GetBytes(raw, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return 0
	}
	messages.ForEach(func(_, message gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "tool") {
			count++
			return true
		}
		content := message.Get("content")
		if !content.Exists() || !content.IsArray() {
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			if strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "tool_result") {
				count++
			}
			return true
		})
		return true
	})
	return count
}

func cpamsAnalyzeClaudeContinuationResponse(raw []byte) cpamsClaudeContinuationResponseAnalysis {
	if len(raw) == 0 {
		return cpamsClaudeContinuationResponseAnalysis{}
	}
	return cpamsClaudeContinuationResponseAnalysis{
		StopReason:    cpamsFirstNonEmptyJSONPath(raw, "stop_reason", "choices.0.finish_reason"),
		ToolUseCount:  cpamsCountContentBlocks(gjson.GetBytes(raw, "content"), "tool_use"),
		ToolCallCount: len(gjson.GetBytes(raw, "choices.0.message.tool_calls").Array()),
		ErrorMessage:  cpamsFirstNonEmptyJSONPath(raw, "error.message", "message"),
	}
}

func cpamsCountContentBlocks(content gjson.Result, wantType string) int {
	if !content.Exists() || !content.IsArray() {
		return 0
	}
	count := 0
	content.ForEach(func(_, part gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(part.Get("type").String()), wantType) {
			count++
		}
		return true
	})
	return count
}

func cpamsFirstNonEmptyJSONPath(raw []byte, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(gjson.GetBytes(raw, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func (w *cpamsResponseCaptureWriter) ResponseIDs() []string {
	if w == nil {
		return nil
	}
	if len(w.responseIDs) == 0 && w.bodyBuffer.Len() > 0 {
		if responseID := cpamsExtractResponseID(w.bodyBuffer.Bytes()); responseID != "" {
			w.responseIDs[responseID] = struct{}{}
		}
	}
	if len(w.responseIDs) == 0 {
		return nil
	}
	out := make([]string, 0, len(w.responseIDs))
	for responseID := range w.responseIDs {
		out = append(out, responseID)
	}
	sort.Strings(out)
	return out
}

func cpamsExtractResponseID(raw []byte) string {
	for _, path := range []string{"response.id", "id"} {
		if responseID := strings.TrimSpace(gjson.GetBytes(raw, path).String()); responseID != "" {
			return responseID
		}
	}
	return ""
}

func normalizeCPAMSRequestedModel(channel, modelRaw string, strictPublic bool) (string, error) {
	trimmed := strings.TrimSpace(modelRaw)
	if trimmed == "" {
		if strictPublic {
			return "", fmt.Errorf("cpamc: model is required")
		}
		return "", nil
	}
	if aliasChannel, requested, ok := parseCPAMSModelAlias(trimmed); ok {
		if aliasChannel != channel {
			return "", fmt.Errorf("cpams: request model channel %q does not match route channel %q", aliasChannel, channel)
		}
		if strictPublic {
			return "", fmt.Errorf("cpamc: public routes do not accept channel-prefixed model names")
		}
		return strings.TrimSpace(requested), nil
	}
	prefix, upstream := splitCPAMSModel(trimmed)
	if prefix != "" && cpamsChannelMatchesModel(channel, prefix, upstream) {
		if strictPublic {
			return "", fmt.Errorf("cpamc: public routes do not accept provider-prefixed model names")
		}
		return strings.TrimSpace(upstream), nil
	}
	if strictPublic && strings.Contains(trimmed, "/") {
		return "", fmt.Errorf("cpamc: public routes only accept stable public model names")
	}
	return trimmed, nil
}

func extractCPAMSConversationKey(c *gin.Context, raw []byte) string {
	if c != nil {
		for _, header := range []string{"X-CPAMS-Conversation-Key", "X-Conversation-ID", "Conversation-Id", "X-Session-ID", "Session-Id"} {
			if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
				return strings.ToLower(header) + ":" + value
			}
		}
	}
	for _, path := range []string{
		"prompt_cache_key",
		"previous_response_id",
		"conversation_id",
		"conversationId",
		"session_id",
		"sessionId",
		"metadata.conversation_id",
		"metadata.conversationId",
		"metadata.thread_id",
		"metadata.threadId",
		"metadata.chat_id",
		"metadata.chatId",
	} {
		if value := strings.TrimSpace(gjson.GetBytes(raw, path).String()); value != "" {
			return path + ":" + value
		}
	}
	for _, path := range []string{"metadata.user_id", "metadata.userID"} {
		if sessionID := util.ExtractClaudeAccountSessionID(gjson.GetBytes(raw, path).String()); sessionID != "" {
			return path + ".account__session:" + sessionID
		}
	}
	if c != nil && c.Request != nil {
		if seed := cpamsSyntheticConversationSeed(c.Request); seed != "" {
			return "session_affinity_seed:" + seed
		}
	}
	return ""
}

func cpamsSyntheticConversationSeed(r *http.Request) string {
	if r == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if tokenFingerprint := cpamsRequestIdentityFingerprint(r); tokenFingerprint != "" {
		parts = append(parts, "auth:"+tokenFingerprint)
	}
	if clientIP := strings.TrimSpace(util.ResolveClientIP(r)); clientIP != "" {
		parts = append(parts, "ip:"+clientIP)
	}
	if userAgent := strings.TrimSpace(r.UserAgent()); userAgent != "" {
		parts = append(parts, "ua:"+cpamsDigestAffinitySeed(userAgent))
	}
	if len(parts) == 0 {
		return ""
	}
	return "fallback:" + cpamsDigestAffinitySeed(strings.Join(parts, "|"))
}

func cpamsRequestIdentityFingerprint(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, header := range []string{"Authorization", "X-API-Key", "Api-Key"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return cpamsDigestAffinitySeed(value)
		}
	}
	return ""
}

func cpamsDigestAffinitySeed(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

func cpamsProtocolHint(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	path := strings.ToLower(strings.TrimSpace(c.Request.URL.Path))
	switch {
	case strings.HasSuffix(path, "/responses/compact"):
		return "responses/compact"
	case strings.HasSuffix(path, "/responses"):
		return "responses"
	case strings.HasSuffix(path, "/chat/completions"):
		return "chat/completions"
	case strings.HasSuffix(path, "/messages/count_tokens"):
		return "messages/count_tokens"
	case strings.HasSuffix(path, "/messages"):
		return "messages"
	default:
		return ""
	}
}

func cpamsAppendProtocolTrait(protocolHint, trait string) string {
	protocolHint = strings.TrimSpace(protocolHint)
	trait = strings.TrimSpace(trait)
	if trait == "" {
		return protocolHint
	}
	if protocolHint == "" {
		return trait
	}
	if strings.Contains(protocolHint, trait) {
		return protocolHint
	}
	return protocolHint + "+" + trait
}

func cpamsIsExternalClaudeCLIRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	userAgent := strings.ToLower(strings.TrimSpace(c.Request.UserAgent()))
	xApp := strings.ToLower(strings.TrimSpace(c.GetHeader("X-App")))
	return strings.Contains(userAgent, "external, cli") || xApp == "cli" || xApp == "external"
}

func cpamsRequiresStatefulCodexResponses(channel, protocolHint string, raw []byte) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	protocolHint = strings.ToLower(strings.TrimSpace(protocolHint))
	if channel != "codex" {
		return false
	}
	switch protocolHint {
	case "responses", "responses/compact":
	default:
		return false
	}
	return strings.TrimSpace(gjson.GetBytes(raw, "previous_response_id").String()) != ""
}

func cpamsHasExplicitThinkingConfig(model string, raw []byte) bool {
	if thinking.ParseSuffix(strings.TrimSpace(model)).HasSuffix {
		return true
	}
	for _, path := range []string{
		"reasoning_effort",
		"reasoning.effort",
		"thinking.type",
		"thinking.budget_tokens",
		"generationConfig.thinkingConfig.thinkingLevel",
		"generationConfig.thinkingConfig.thinking_level",
		"generationConfig.thinkingConfig.thinkingBudget",
		"generationConfig.thinkingConfig.thinking_budget",
	} {
		if gjson.GetBytes(raw, path).Exists() {
			return true
		}
	}
	return false
}

func cpamsEnsureThinkingSuffix(model, suffix string) string {
	trimmedModel := strings.TrimSpace(model)
	trimmedSuffix := strings.TrimSpace(suffix)
	if trimmedModel == "" || trimmedSuffix == "" {
		return trimmedModel
	}
	if thinking.ParseSuffix(trimmedModel).HasSuffix {
		return trimmedModel
	}
	return trimmedModel + "(" + trimmedSuffix + ")"
}
