package executor

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	apiAttemptsKey = "API_UPSTREAM_ATTEMPTS"
	apiRequestKey  = "API_REQUEST"
	apiResponseKey = "API_RESPONSE"

	defaultRequestLogMaxBodyBytes   = 65536
	defaultRequestLogStreamMaxBytes = 262144
)

type apiResponseCapture struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

// upstreamRequestLog captures the outbound upstream request details for logging.
type upstreamRequestLog struct {
	URL       string
	Method    string
	Headers   http.Header
	Body      []byte
	Provider  string
	AuthID    string
	AuthLabel string
	AuthType  string
	AuthValue string
}

type upstreamAttempt struct {
	index                int
	request              string
	response             *strings.Builder
	responseBytes        int
	responseIntroWritten bool
	statusWritten        bool
	headersWritten       bool
	bodyStarted          bool
	bodyHasContent       bool
	errorWritten         bool
	responseTruncated    bool
}

// recordAPIRequest stores the upstream request metadata in Gin context for request logging.
func recordAPIRequest(ctx context.Context, cfg *config.Config, info upstreamRequestLog) {
	if cfg == nil || !cfg.RequestLog {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}

	attempts := getAttempts(ginCtx)
	index := len(attempts) + 1

	builder := &strings.Builder{}
	builder.WriteString(fmt.Sprintf("=== API REQUEST %d ===\n", index))
	builder.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	if info.URL != "" {
		builder.WriteString(fmt.Sprintf("Upstream URL: %s\n", info.URL))
	} else {
		builder.WriteString("Upstream URL: <unknown>\n")
	}
	if info.Method != "" {
		builder.WriteString(fmt.Sprintf("HTTP Method: %s\n", info.Method))
	}
	if auth := formatAuthInfo(info); auth != "" {
		builder.WriteString(fmt.Sprintf("Auth: %s\n", auth))
	}
	builder.WriteString("\nHeaders:\n")
	writeHeaders(builder, info.Headers)
	builder.WriteString("\nBody:\n")
	if len(info.Body) > 0 {
		builder.Write(TruncateLoggedPayload(info.Body, RequestLogMaxBodyBytes(&cfg.SDKConfig), "upstream request body"))
	} else {
		builder.WriteString("<empty>")
	}
	builder.WriteString("\n\n")

	attempt := &upstreamAttempt{
		index:    index,
		request:  builder.String(),
		response: &strings.Builder{},
	}
	attempts = append(attempts, attempt)
	ginCtx.Set(apiAttemptsKey, attempts)
	updateAggregatedRequest(ginCtx, attempts)
}

// recordAPIResponseMetadata captures upstream response status/header information for the latest attempt.
func recordAPIResponseMetadata(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	if cfg == nil || !cfg.RequestLog {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	_, attempt := ensureAttempt(ginCtx)
	ensureResponseIntro(attempt)

	if status > 0 && !attempt.statusWritten {
		attempt.response.WriteString(fmt.Sprintf("Status: %d\n", status))
		attempt.statusWritten = true
	}
	if !attempt.headersWritten {
		attempt.response.WriteString("Headers:\n")
		writeHeaders(attempt.response, headers)
		attempt.headersWritten = true
		attempt.response.WriteString("\n")
	}
}

// recordAPIResponseError adds an error entry for the latest attempt when no HTTP response is available.
func recordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	if cfg == nil || !cfg.RequestLog || err == nil {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	_, attempt := ensureAttempt(ginCtx)
	ensureResponseIntro(attempt)

	if attempt.bodyStarted && !attempt.bodyHasContent {
		// Ensure body does not stay empty marker if error arrives first.
		attempt.bodyStarted = false
	}
	if attempt.errorWritten {
		attempt.response.WriteString("\n")
	}
	attempt.response.WriteString(fmt.Sprintf("Error: %s\n", err.Error()))
	attempt.errorWritten = true
}

// appendAPIResponseChunk appends an upstream response chunk to Gin context for request logging.
func appendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	if cfg == nil || !cfg.RequestLog {
		return
	}
	data := bytes.TrimSpace(chunk)
	if len(data) == 0 {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	_, attempt := ensureAttempt(ginCtx)
	ensureResponseIntro(attempt)

	if !attempt.headersWritten {
		attempt.response.WriteString("Headers:\n")
		writeHeaders(attempt.response, nil)
		attempt.headersWritten = true
		attempt.response.WriteString("\n")
	}
	if !attempt.bodyStarted {
		attempt.response.WriteString("Body:\n")
		attempt.bodyStarted = true
	}
	if attempt.bodyHasContent {
		attempt.response.WriteString("\n\n")
	}
	limit := requestLogStreamMaxBytes(cfg)
	remaining := limit - attempt.responseBytes
	if remaining <= 0 {
		writeResponseTruncatedMarker(attempt, limit)
		return
	}
	if len(data) > remaining {
		attempt.response.Write(data[:remaining])
		attempt.responseBytes += remaining
		attempt.bodyHasContent = true
		writeResponseTruncatedMarker(attempt, limit)
		return
	}
	attempt.response.Write(data)
	attempt.responseBytes += len(data)
	attempt.bodyHasContent = true
}

func ginContextFrom(ctx context.Context) *gin.Context {
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	return ginCtx
}

func getAttempts(ginCtx *gin.Context) []*upstreamAttempt {
	if ginCtx == nil {
		return nil
	}
	if value, exists := ginCtx.Get(apiAttemptsKey); exists {
		if attempts, ok := value.([]*upstreamAttempt); ok {
			return attempts
		}
	}
	return nil
}

func ensureAttempt(ginCtx *gin.Context) ([]*upstreamAttempt, *upstreamAttempt) {
	attempts := getAttempts(ginCtx)
	if len(attempts) == 0 {
		attempt := &upstreamAttempt{
			index:    1,
			request:  "=== API REQUEST 1 ===\n<missing>\n\n",
			response: &strings.Builder{},
		}
		attempts = []*upstreamAttempt{attempt}
		ginCtx.Set(apiAttemptsKey, attempts)
		updateAggregatedRequest(ginCtx, attempts)
	}
	return attempts, attempts[len(attempts)-1]
}

func ensureResponseIntro(attempt *upstreamAttempt) {
	if attempt == nil || attempt.response == nil || attempt.responseIntroWritten {
		return
	}
	attempt.response.WriteString(fmt.Sprintf("=== API RESPONSE %d ===\n", attempt.index))
	attempt.response.WriteString(fmt.Sprintf("Timestamp: %s\n", time.Now().Format(time.RFC3339Nano)))
	attempt.response.WriteString("\n")
	attempt.responseIntroWritten = true
}

func requestLogStreamMaxBytes(cfg *config.Config) int {
	if cfg == nil || cfg.RequestLogStreamMaxBytes <= 0 {
		return defaultRequestLogStreamMaxBytes
	}
	return cfg.RequestLogStreamMaxBytes
}

// RequestLogMaxBodyBytes returns the configured cap for non-stream request-log payloads.
func RequestLogMaxBodyBytes(cfg *config.SDKConfig) int {
	if cfg == nil || cfg.RequestLogMaxBodyBytes <= 0 {
		return defaultRequestLogMaxBodyBytes
	}
	return cfg.RequestLogMaxBodyBytes
}

// TruncateLoggedPayload clones payload data and caps it to a bounded size for request logging.
func TruncateLoggedPayload(data []byte, limit int, label string) []byte {
	if len(data) == 0 {
		return nil
	}
	limit = normalizeRequestLogBodyLimit(limit)
	if len(data) <= limit {
		return bytes.Clone(data)
	}
	var buffer bytes.Buffer
	AppendTruncatedLogBytes(&buffer, data, limit, new(bool), label)
	return buffer.Bytes()
}

// AppendAPIResponseLog stores non-stream API response data in Gin context without repeated full-buffer copies.
func AppendAPIResponseLog(ginCtx *gin.Context, data []byte, limit int) {
	if ginCtx == nil || len(data) == 0 {
		return
	}
	if _, exists := ginCtx.Get("API_RESPONSE_TIMESTAMP"); !exists {
		ginCtx.Set("API_RESPONSE_TIMESTAMP", time.Now())
	}

	limit = normalizeRequestLogBodyLimit(limit)

	if existing, exists := ginCtx.Get(apiResponseKey); exists {
		if capture, ok := existing.(*apiResponseCapture); ok && capture != nil {
			capture.Append(data)
			return
		}
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			capture := &apiResponseCapture{limit: limit}
			capture.Append(existingBytes)
			capture.Append(data)
			ginCtx.Set(apiResponseKey, capture)
			return
		}
	}

	capture := &apiResponseCapture{limit: limit}
	capture.Append(data)
	ginCtx.Set(apiResponseKey, capture)
}

func writeResponseTruncatedMarker(attempt *upstreamAttempt, limit int) {
	if attempt == nil || attempt.response == nil || attempt.responseTruncated {
		return
	}
	if attempt.bodyHasContent {
		attempt.response.WriteString("\n\n")
	}
	attempt.response.WriteString(fmt.Sprintf("[stream log truncated after %d bytes]\n", limit))
	attempt.responseTruncated = true
}

func updateAggregatedRequest(ginCtx *gin.Context, attempts []*upstreamAttempt) {
	if ginCtx == nil {
		return
	}
	var builder strings.Builder
	for _, attempt := range attempts {
		builder.WriteString(attempt.request)
	}
	ginCtx.Set(apiRequestKey, []byte(builder.String()))
}

func buildAggregatedResponse(attempts []*upstreamAttempt) []byte {
	var builder strings.Builder
	for idx, attempt := range attempts {
		if attempt == nil || attempt.response == nil {
			continue
		}
		responseText := attempt.response.String()
		if responseText == "" {
			continue
		}
		builder.WriteString(responseText)
		if !strings.HasSuffix(responseText, "\n") {
			builder.WriteString("\n")
		}
		if idx < len(attempts)-1 {
			builder.WriteString("\n")
		}
	}
	if builder.Len() == 0 {
		return nil
	}
	return []byte(builder.String())
}

// MaterializeAPIResponse builds the aggregated upstream response log on demand.
// This avoids rebuilding the full response text on every streaming chunk.
func MaterializeAPIResponse(ginCtx *gin.Context) []byte {
	if ginCtx == nil {
		return nil
	}
	attempts := getAttempts(ginCtx)
	if len(attempts) > 0 {
		data := buildAggregatedResponse(attempts)
		if len(data) > 0 {
			ginCtx.Set(apiResponseKey, data)
			return data
		}
	}
	apiResponse, isExist := ginCtx.Get(apiResponseKey)
	if !isExist {
		return nil
	}
	if capture, ok := apiResponse.(*apiResponseCapture); ok && capture != nil {
		data := capture.Bytes()
		if len(data) == 0 {
			return nil
		}
		ginCtx.Set(apiResponseKey, data)
		return data
	}
	data, ok := apiResponse.([]byte)
	if !ok || len(data) == 0 {
		return nil
	}
	return data
}

func (c *apiResponseCapture) Append(data []byte) {
	if c == nil || len(data) == 0 {
		return
	}
	if c.limit <= 0 {
		c.limit = defaultRequestLogMaxBodyBytes
	}
	if c.buffer.Len() > 0 && !c.truncated {
		AppendTruncatedLogBytes(&c.buffer, []byte("\n"), c.limit, &c.truncated, "upstream response body")
	}
	AppendTruncatedLogBytes(&c.buffer, data, c.limit, &c.truncated, "upstream response body")
}

func (c *apiResponseCapture) Bytes() []byte {
	if c == nil || c.buffer.Len() == 0 {
		return nil
	}
	return bytes.Clone(c.buffer.Bytes())
}

func normalizeRequestLogBodyLimit(limit int) int {
	if limit <= 0 {
		return defaultRequestLogMaxBodyBytes
	}
	return limit
}

// AppendTruncatedLogBytes writes data into dst until the configured cap is reached, then appends a truncation marker once.
func AppendTruncatedLogBytes(dst *bytes.Buffer, data []byte, limit int, truncated *bool, label string) {
	if dst == nil || len(data) == 0 || truncated == nil {
		return
	}
	limit = normalizeRequestLogBodyLimit(limit)
	if *truncated {
		return
	}
	remaining := limit - dst.Len()
	if remaining <= 0 {
		appendTruncationNotice(dst, limit, truncated, label)
		return
	}
	if len(data) > remaining {
		_, _ = dst.Write(data[:remaining])
		appendTruncationNotice(dst, limit, truncated, label)
		return
	}
	_, _ = dst.Write(data)
}

func appendTruncationNotice(dst *bytes.Buffer, limit int, truncated *bool, label string) {
	if dst == nil || truncated == nil || *truncated {
		return
	}
	notice := truncationNotice(limit, label)
	if dst.Len() > 0 {
		last := dst.Bytes()[dst.Len()-1]
		if last != '\n' {
			_, _ = dst.WriteString("\n")
		}
		_, _ = dst.WriteString("\n")
	}
	_, _ = dst.WriteString(notice)
	_, _ = dst.WriteString("\n")
	*truncated = true
}

func truncationNotice(limit int, label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "payload"
	}
	return fmt.Sprintf("[%s truncated after %d bytes]", label, limit)
}

func writeHeaders(builder *strings.Builder, headers http.Header) {
	if builder == nil {
		return
	}
	if len(headers) == 0 {
		builder.WriteString("<none>\n")
		return
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := headers[key]
		if len(values) == 0 {
			builder.WriteString(fmt.Sprintf("%s:\n", key))
			continue
		}
		for _, value := range values {
			masked := util.MaskSensitiveHeaderValue(key, value)
			builder.WriteString(fmt.Sprintf("%s: %s\n", key, masked))
		}
	}
}

func formatAuthInfo(info upstreamRequestLog) string {
	var parts []string
	if trimmed := strings.TrimSpace(info.Provider); trimmed != "" {
		parts = append(parts, fmt.Sprintf("provider=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthID); trimmed != "" {
		parts = append(parts, fmt.Sprintf("auth_id=%s", trimmed))
	}
	if trimmed := strings.TrimSpace(info.AuthLabel); trimmed != "" {
		parts = append(parts, fmt.Sprintf("label=%s", trimmed))
	}

	authType := strings.ToLower(strings.TrimSpace(info.AuthType))
	authValue := strings.TrimSpace(info.AuthValue)
	switch authType {
	case "api_key":
		if authValue != "" {
			parts = append(parts, fmt.Sprintf("type=api_key value=%s", util.HideAPIKey(authValue)))
		} else {
			parts = append(parts, "type=api_key")
		}
	case "oauth":
		parts = append(parts, "type=oauth")
	default:
		if authType != "" {
			if authValue != "" {
				parts = append(parts, fmt.Sprintf("type=%s value=%s", authType, authValue))
			} else {
				parts = append(parts, fmt.Sprintf("type=%s", authType))
			}
		}
	}

	return strings.Join(parts, ", ")
}

func summarizeErrorBody(contentType string, body []byte) string {
	isHTML := strings.Contains(strings.ToLower(contentType), "text/html")
	if !isHTML {
		trimmed := bytes.TrimSpace(bytes.ToLower(body))
		if bytes.HasPrefix(trimmed, []byte("<!doctype html")) || bytes.HasPrefix(trimmed, []byte("<html")) {
			isHTML = true
		}
	}
	if isHTML {
		if title := extractHTMLTitle(body); title != "" {
			return title
		}
		return "[html body omitted]"
	}

	// Try to extract error message from JSON response
	if message := extractJSONErrorMessage(body); message != "" {
		return message
	}

	return string(body)
}

func extractHTMLTitle(body []byte) string {
	lower := bytes.ToLower(body)
	start := bytes.Index(lower, []byte("<title"))
	if start == -1 {
		return ""
	}
	gt := bytes.IndexByte(lower[start:], '>')
	if gt == -1 {
		return ""
	}
	start += gt + 1
	end := bytes.Index(lower[start:], []byte("</title>"))
	if end == -1 {
		return ""
	}
	title := string(body[start : start+end])
	title = html.UnescapeString(title)
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	return strings.Join(strings.Fields(title), " ")
}

// extractJSONErrorMessage attempts to extract error.message from JSON error responses
func extractJSONErrorMessage(body []byte) string {
	result := gjson.GetBytes(body, "error.message")
	if result.Exists() && result.String() != "" {
		return result.String()
	}
	return ""
}

// logWithRequestID returns a logrus Entry with request_id field populated from context.
// If no request ID is found in context, it returns the standard logger.
func logWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	requestID := logging.GetRequestID(ctx)
	if requestID == "" {
		return log.NewEntry(log.StandardLogger())
	}
	return log.WithField("request_id", requestID)
}
