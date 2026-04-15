package management

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const liveMonitorBodyProbeLimit = 64 * 1024

func (h *Handler) LiveMonitorMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h == nil || h.liveMonitor == nil || !shouldTrackLiveMonitorRequest(c) {
			c.Next()
			return
		}

		requestID := logging.GetGinRequestID(c)
		if requestID == "" {
			c.Next()
			return
		}

		requestedModel := liveMonitorRequestedModel(c)
		if requestedModel == "" {
			c.Next()
			return
		}
		requestedType := inferLiveMonitorType(requestedModel)
		clientIP := strings.TrimSpace(util.ResolveClientIP(c.Request))
		h.liveMonitor.recordStart(liveMonitorPersistedEvent{
			Timestamp:      time.Now().UTC(),
			RequestID:      requestID,
			ClientIP:       clientIP,
			RequestedModel: requestedModel,
			RequestedType:  requestedType,
		})

		c.Next()

		status := c.Writer.Status()
		resultStatus := "success"
		if status >= http.StatusBadRequest {
			resultStatus = "failed"
		}
		h.liveMonitor.recordCompletion(liveMonitorPersistedEvent{
			Timestamp:      time.Now().UTC(),
			RequestID:      requestID,
			ClientIP:       clientIP,
			RequestedModel: requestedModel,
			RequestedType:  requestedType,
			ResultStatus:   resultStatus,
			HTTPStatus:     status,
			ErrorMessage:   strings.TrimSpace(c.Errors.String()),
		})
	}
}

func (h *Handler) handleLiveMonitorRoutingEvent(event coreauth.RoutingEvent) {
	if h == nil || h.liveMonitor == nil {
		return
	}
	timestamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(event.OccurredAt))
	if err != nil {
		timestamp = time.Now().UTC()
	}
	h.liveMonitor.recordRoute(liveMonitorPersistedEvent{
		Timestamp:       timestamp,
		RequestID:       strings.TrimSpace(event.RequestID),
		RequestedModel:  strings.TrimSpace(event.RequestedModel),
		ActualModel:     strings.TrimSpace(event.ChosenModel),
		ActualType:      inferLiveMonitorType(strings.TrimSpace(event.ChosenProvider)),
		ChannelPrefix:   firstNonEmpty(strings.TrimSpace(event.ChosenPrefix), strings.TrimSpace(event.ChosenLabel)),
		ChannelLabel:    strings.TrimSpace(event.ChosenLabel),
		ChannelBaseURL:  strings.TrimSpace(event.ChosenBaseURL),
		AuthID:          strings.TrimSpace(event.ChosenAuth),
		ResultStatus:    strings.TrimSpace(event.ResultStatus),
		StateTransition: strings.TrimSpace(event.StateTransition),
		FallbackReason:  strings.TrimSpace(event.FallbackReason),
		ErrorMessage:    strings.TrimSpace(event.ErrorMessage),
	})
}

func (h *Handler) GetLiveMonitorCards(c *gin.Context) {
	if h == nil || h.liveMonitor == nil {
		c.JSON(http.StatusOK, liveMonitorCardPage{GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano), Range: "6h", Page: 1, PageSize: liveMonitorDefaultPageSize, TotalPages: 1})
		return
	}
	page, _ := strconv.Atoi(strings.TrimSpace(c.Query("page")))
	pageSize, _ := strconv.Atoi(strings.TrimSpace(c.Query("page_size")))
	payload := h.liveMonitor.snapshot(parseLiveMonitorRange(c.Query("range")), page, pageSize, time.Now().UTC())
	c.JSON(http.StatusOK, payload)
}

func (h *Handler) GetLiveMonitorCard(c *gin.Context) {
	if h == nil || h.liveMonitor == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "live monitor unavailable"})
		return
	}
	detail, ok := h.liveMonitor.detail(c.Param("card_id"), parseLiveMonitorRange(c.Query("range")), time.Now().UTC())
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}
	c.JSON(http.StatusOK, detail)
}

func (h *Handler) GetLiveMonitorCardHistory(c *gin.Context) {
	if h == nil || h.liveMonitor == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "live monitor unavailable"})
		return
	}
	offset, _ := strconv.Atoi(strings.TrimSpace(c.Query("offset")))
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	payload, ok := h.liveMonitor.history(c.Param("card_id"), parseLiveMonitorRange(c.Query("range")), offset, limit, time.Now().UTC())
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}
	c.JSON(http.StatusOK, payload)
}

func (h *Handler) StreamLiveMonitor(c *gin.Context) {
	if h == nil || h.liveMonitor == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "live monitor unavailable"})
		return
	}
	id, ch := h.liveMonitor.subscribe()
	if ch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "live monitor unavailable"})
		return
	}
	defer h.liveMonitor.unsubscribe(id)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
		return
	}

	_, _ = c.Writer.Write([]byte(": connected\n\n"))
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
			flusher.Flush()
		case envelope, ok := <-ch:
			if !ok {
				return
			}
			payload, err := jsonMarshalNoEscape(envelope)
			if err != nil {
				continue
			}
			_, _ = c.Writer.Write([]byte("event: card\n"))
			_, _ = c.Writer.Write([]byte("data: "))
			_, _ = c.Writer.Write(payload)
			_, _ = c.Writer.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}

func shouldTrackLiveMonitorRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	if c.Request.Method != http.MethodPost {
		return false
	}
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/v1/") {
		return true
	}
	if strings.HasPrefix(path, "/v1beta/models/") {
		return true
	}
	return false
}

func liveMonitorRequestedModel(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	path := strings.TrimSpace(c.Request.URL.Path)
	if strings.HasPrefix(path, "/v1beta/models/") {
		modelPath := strings.TrimPrefix(path, "/v1beta/models/")
		if idx := strings.Index(modelPath, ":"); idx >= 0 {
			modelPath = modelPath[:idx]
		}
		modelPath = strings.Trim(modelPath, "/")
		return strings.TrimSpace(modelPath)
	}
	if c.Request.Body == nil {
		return ""
	}
	prefix, restored, err := liveMonitorCaptureBodyPrefix(c.Request.Body, liveMonitorBodyProbeLimit)
	if err != nil {
		return ""
	}
	c.Request.Body = restored
	model := strings.TrimSpace(gjson.GetBytes(prefix, "model").String())
	if model != "" {
		return model
	}
	return strings.TrimSpace(gjson.GetBytes(prefix, "model_name").String())
}

func liveMonitorCaptureBodyPrefix(body io.ReadCloser, limit int) ([]byte, io.ReadCloser, error) {
	if body == nil {
		return nil, nil, nil
	}
	prefix, err := io.ReadAll(io.LimitReader(body, int64(limit+1)))
	if err != nil {
		return nil, nil, err
	}
	restored := io.NopCloser(io.MultiReader(bytes.NewReader(prefix), body))
	if len(prefix) > limit {
		prefix = prefix[:limit]
	}
	return prefix, restored, nil
}

func inferLiveMonitorType(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	switch strings.ToLower(value) {
	case "openai-compatibility":
		return "openai-compat"
	case "gemini-cli":
		return "gemini"
	}
	providers := util.GetProviderName(value)
	if len(providers) > 0 {
		switch strings.ToLower(strings.TrimSpace(providers[0])) {
		case "openai-compatibility":
			return "openai-compat"
		case "gemini-cli":
			return "gemini"
		default:
			return strings.ToLower(strings.TrimSpace(providers[0]))
		}
	}
	return strings.ToLower(value)
}

func jsonMarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimSpace(buf.Bytes())
	return out, nil
}
