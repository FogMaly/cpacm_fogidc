// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the request logging middleware that captures comprehensive
// request and response data when enabled through configuration.
package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// RequestLoggingMiddleware creates a Gin middleware that logs HTTP requests and responses.
// It captures detailed information about the request and response, including headers and body,
// and uses the provided RequestLogger to record this data. When logging is disabled in the
// logger, it still captures data so that upstream errors can be persisted.
func RequestLoggingMiddleware(logger logging.RequestLogger, cfg *config.SDKConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if logger == nil {
			c.Next()
			return
		}

		if c.Request.Method == http.MethodGet {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldLogRequest(path) {
			c.Next()
			return
		}

		// Capture request information
		bodyLimit := runtimeexecutor.RequestLogMaxBodyBytes(cfg)
		requestInfo, err := captureRequestInfo(c, bodyLimit)
		if err != nil {
			log.WithError(err).Warn("request logging capture failed")
			c.Next()
			return
		}

		// Create response writer wrapper
		wrapper := NewResponseWriterWrapper(c.Writer, logger, requestInfo, bodyLimit)
		if !logger.IsEnabled() {
			wrapper.logOnErrorOnly = true
		}
		c.Writer = wrapper

		// Process the request
		c.Next()

		// Finalize logging after request processing
		if err = wrapper.Finalize(c); err != nil {
			log.WithError(err).Warn("request logging finalize failed")
		}
	}
}

// captureRequestInfo extracts relevant information from the incoming HTTP request.
// It captures the URL, method, headers, and a bounded preview of the body. The body
// prefix is replayed back into the request stream so downstream handlers still receive
// the full payload without requiring the middleware to load the entire body into memory.
func captureRequestInfo(c *gin.Context, bodyLimit int) (*RequestInfo, error) {
	// Capture URL with sensitive query parameters masked
	maskedQuery := util.MaskSensitiveQuery(c.Request.URL.RawQuery)
	url := c.Request.URL.Path
	if maskedQuery != "" {
		url += "?" + maskedQuery
	}

	// Capture method
	method := c.Request.Method

	// Capture headers
	headers := make(map[string][]string)
	for key, values := range c.Request.Header {
		headers[key] = values
	}

	// Capture request body
	var body []byte
	if c.Request.Body != nil {
		bodyBytes, restoredBody, err := captureRequestBodyPrefix(c.Request.Body, bodyLimit)
		if err != nil {
			return nil, err
		}
		c.Request.Body = restoredBody
		body = bodyBytes
	}

	return &RequestInfo{
		URL:       url,
		Method:    method,
		Headers:   headers,
		Body:      body,
		RequestID: logging.GetGinRequestID(c),
		Timestamp: time.Now(),
	}, nil
}

func captureRequestBodyPrefix(body io.ReadCloser, bodyLimit int) ([]byte, io.ReadCloser, error) {
	if body == nil {
		return nil, nil, nil
	}

	bodyLimit = runtimeexecutor.RequestLogMaxBodyBytes(&config.SDKConfig{RequestLogMaxBodyBytes: bodyLimit})
	prefix, err := io.ReadAll(io.LimitReader(body, int64(bodyLimit+1)))
	if err != nil {
		return nil, nil, err
	}

	restoredBody := io.NopCloser(io.MultiReader(bytes.NewReader(prefix), body))
	return runtimeexecutor.TruncateLoggedPayload(prefix, bodyLimit, "client request body"), restoredBody, nil
}

// shouldLogRequest determines whether the request should be logged.
// It skips management endpoints to avoid leaking secrets but allows
// all other routes, including module-provided ones, to honor request-log.
func shouldLogRequest(path string) bool {
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management") {
		return false
	}

	if strings.HasPrefix(path, "/api") {
		return strings.HasPrefix(path, "/api/provider")
	}

	return true
}
