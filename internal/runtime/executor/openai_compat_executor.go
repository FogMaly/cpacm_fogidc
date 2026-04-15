package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAICompatExecutor implements a stateless executor for OpenAI-compatible providers.
// It performs request/response translation and executes against the provider base URL
// using per-auth credentials (API key) and per-auth HTTP transport (proxy) from context.
type OpenAICompatExecutor struct {
	provider string
	cfg      *config.Config
}

// NewOpenAICompatExecutor creates an executor bound to a provider key (e.g., "openrouter").
func NewOpenAICompatExecutor(provider string, cfg *config.Config) *OpenAICompatExecutor {
	return &OpenAICompatExecutor{provider: provider, cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *OpenAICompatExecutor) Identifier() string { return e.provider }

// PrepareRequest injects OpenAI-compatible credentials into the outgoing HTTP request.
func (e *OpenAICompatExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	util.ApplyReservedProviderHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects OpenAI-compatible credentials into the request and executes it.
func (e *OpenAICompatExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("openai compat executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	return doRequestWithTimeoutRetry(ctx, e.cfg, auth, httpReq)
}

func (e *OpenAICompatExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	endpoint := "/chat/completions"
	if opts.Alt == "responses/compact" {
		to = sdktranslator.FromString("openai-response")
		endpoint = "/responses/compact"
	}
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, opts.Stream)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, opts.Stream)
	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel)
	if opts.Alt == "responses/compact" {
		if updated, errDelete := sjson.DeleteBytes(translated, "stream"); errDelete == nil {
			translated = updated
		}
	}

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	url := strings.TrimSuffix(baseURL, "/") + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	util.ApplyReservedProviderHeadersFromAttrs(httpReq, attrs)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := doRequestWithTimeoutRetry(ctx, e.cfg, auth, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	body = normalizeCompatNonStreamBody(body, to)
	appendAPIResponseChunk(ctx, e.cfg, body)
	reporter.publish(ctx, parseOpenAIUsage(body))
	// Ensure we at least record the request even if upstream doesn't return usage
	reporter.ensurePublished(ctx)
	// Translate response back to source format when needed
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, body, &param)
	out = string(normalizeCompatFinalResponse([]byte(out), requestedModel, from.String()))
	resp = cliproxyexecutor.Response{Payload: []byte(out)}
	return resp, nil
}

func (e *OpenAICompatExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	requestedModel := payloadRequestedModel(opts, req.Model)
	translated = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", translated, originalTranslated, requestedModel)
	fallbackEligible := shouldUseCompatClaudeStreamFallback(baseURL, from)

	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	util.ApplyReservedProviderHeadersFromAttrs(httpReq, attrs)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := doRequestWithTimeoutRetry(ctx, e.cfg, auth, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		if fallbackEligible && compatStreamFallbackEligibleStatus(httpResp.StatusCode) {
			fallbackChunks, fallbackErr := e.compatClaudeStreamFallbackChunks(ctx, auth, req, opts, translated, requestedModel, baseURL, apiKey, reporter)
			if fallbackErr == nil {
				out := make(chan cliproxyexecutor.StreamChunk, len(fallbackChunks))
				for i := range fallbackChunks {
					out <- fallbackChunks[i]
				}
				close(out)
				return out, nil
			}
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		defer reporter.ensurePublished(ctx)
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		sentPayload := false
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			if len(line) == 0 {
				continue
			}

			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}

			// OpenAI-compatible streams are SSE: lines typically prefixed with "data: ".
			// Pass through translator; it yields one or more chunks for the target schema.
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, translated, bytes.Clone(line), &param)
			for i := range chunks {
				payload := []byte(chunks[i])
				if len(payload) == 0 {
					continue
				}
				sentPayload = true
				out <- cliproxyexecutor.StreamChunk{Payload: payload}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if !sentPayload && fallbackEligible {
				if fallbackChunks, fallbackErr := e.compatClaudeStreamFallbackChunks(ctx, auth, req, opts, translated, requestedModel, baseURL, apiKey, reporter); fallbackErr == nil {
					for i := range fallbackChunks {
						out <- fallbackChunks[i]
					}
					return
				}
			}
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		if !sentPayload && fallbackEligible {
			fallbackChunks, fallbackErr := e.compatClaudeStreamFallbackChunks(ctx, auth, req, opts, translated, requestedModel, baseURL, apiKey, reporter)
			if fallbackErr == nil {
				for i := range fallbackChunks {
					out <- fallbackChunks[i]
				}
				return
			}
			recordAPIResponseError(ctx, e.cfg, fallbackErr)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: fallbackErr}
		}
	}()
	return stream, nil
}

func shouldUseCompatClaudeStreamFallback(baseURL string, from sdktranslator.Format) bool {
	return from == sdktranslator.FromString("claude") && shouldUseClaudeThirdPartyRelayProfile(baseURL)
}

func compatStreamFallbackEligibleStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	default:
		return status >= http.StatusInternalServerError
	}
}

func (e *OpenAICompatExecutor) compatClaudeStreamFallbackChunks(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, requestBody []byte, requestedModel, baseURL, apiKey string, reporter *usageReporter) ([]cliproxyexecutor.StreamChunk, error) {
	if !shouldUseCompatClaudeStreamFallback(baseURL, opts.SourceFormat) {
		return nil, fmt.Errorf("openai compat executor: claude stream fallback not applicable")
	}

	fallbackBody := bytes.Clone(requestBody)
	if updated, err := sjson.SetBytes(fallbackBody, "stream", false); err == nil {
		fallbackBody = updated
	}

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(fallbackBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	util.ApplyReservedProviderHeadersFromAttrs(httpReq, attrs)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      fallbackBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := doRequestWithTimeoutRetry(ctx, e.cfg, auth, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		recordAPIResponseError(ctx, e.cfg, readErr)
		return nil, readErr
	}
	appendAPIResponseChunk(ctx, e.cfg, body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, statusErr{code: httpResp.StatusCode, msg: string(body)}
	}

	body = normalizeCompatNonStreamBody(body, sdktranslator.FromString("openai"))
	reporter.publish(ctx, parseOpenAIUsage(body))
	reporter.ensurePublished(ctx)

	var param any
	translated := sdktranslator.TranslateNonStream(ctx, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, fallbackBody, body, &param)
	translated = string(normalizeCompatFinalResponse([]byte(translated), requestedModel, opts.SourceFormat.String()))
	fallbackLines := synthesizeClaudeStreamLinesFromResponse([]byte(translated), requestedModel)
	if len(fallbackLines) == 0 {
		return nil, fmt.Errorf("openai compat executor: claude stream fallback produced no events")
	}

	chunks := make([]cliproxyexecutor.StreamChunk, 0, len(fallbackLines))
	for i := range fallbackLines {
		line := bytes.Clone(fallbackLines[i])
		if len(line) == 0 {
			line = []byte{}
		}
		chunks = append(chunks, cliproxyexecutor.StreamChunk{Payload: line})
	}
	return chunks, nil
}

func (e *OpenAICompatExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	modelForCounting := baseModel

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := tokenizerForModel(modelForCounting)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("openai compat executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translatedUsage)}, nil
}

// Refresh is a no-op for API-key based compatibility providers.
func (e *OpenAICompatExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("openai compat executor: refresh called")
	_ = ctx
	return auth, nil
}

func (e *OpenAICompatExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = normalizeOpenAICompatibleBaseURL(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

func (e *OpenAICompatExecutor) resolveCompatConfig(auth *cliproxyauth.Auth) *config.OpenAICompatibility {
	if auth == nil || e.cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["compat_name"]); v != "" {
			candidates = append(candidates, v)
		}
		if v := strings.TrimSpace(auth.Attributes["provider_key"]); v != "" {
			candidates = append(candidates, v)
		}
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range e.cfg.OpenAICompatibility {
		compat := &e.cfg.OpenAICompatibility[i]
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func (e *OpenAICompatExecutor) overrideModel(payload []byte, model string) []byte {
	if len(payload) == 0 || model == "" {
		return payload
	}
	payload, _ = sjson.SetBytes(payload, "model", model)
	return payload
}

func normalizeCompatFinalResponse(raw []byte, requestedModel, sourceFormat string) []byte {
	if len(raw) == 0 || !gjson.ValidBytes(raw) {
		return raw
	}
	displayModel := compatDisplayModel(requestedModel)
	if displayModel == "" {
		return raw
	}

	out := append([]byte(nil), raw...)
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "openai":
		out, _ = sjson.SetBytes(out, "model", displayModel)
		if !compatRequestedModelAllowsThinking(requestedModel) {
			choices := gjson.GetBytes(out, "choices")
			if choices.Exists() && choices.IsArray() {
				for idx := range choices.Array() {
					indexPath := fmt.Sprintf("choices.%d", idx)
					out, _ = sjson.DeleteBytes(out, indexPath+".message.reasoning_content")
					out, _ = sjson.DeleteBytes(out, indexPath+".message.reasoning")
					out, _ = sjson.DeleteBytes(out, indexPath+".delta.reasoning_content")
					out, _ = sjson.DeleteBytes(out, indexPath+".delta.reasoning")
				}
			}
		}
	case "claude":
		out, _ = sjson.SetBytes(out, "model", displayModel)
		if !compatRequestedModelAllowsThinking(requestedModel) {
			content := gjson.GetBytes(out, "content")
			if content.Exists() && content.IsArray() {
				filtered := make([]map[string]any, 0, len(content.Array()))
				for _, item := range content.Array() {
					typ := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
					if typ == "thinking" || typ == "redacted_thinking" {
						continue
					}
					if itemMap, ok := item.Value().(map[string]any); ok {
						filtered = append(filtered, itemMap)
					}
				}
				out, _ = sjson.SetBytes(out, "content", filtered)
			}
		}
	}

	return out
}

func normalizeCompatNonStreamBody(raw []byte, target sdktranslator.Format) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || gjson.ValidBytes(trimmed) {
		return trimmed
	}

	candidate := compatExtractSSEJSON(trimmed, target)
	if len(candidate) != 0 {
		return candidate
	}

	return trimmed
}

func compatExtractSSEJSON(raw []byte, target sdktranslator.Format) []byte {
	var candidate []byte

	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		payload := jsonPayload(line)
		if len(payload) == 0 || !gjson.ValidBytes(payload) {
			continue
		}

		switch target {
		case sdktranslator.FromString("openai-response"):
			if extracted := compatExtractResponsesPayload(payload); len(extracted) != 0 {
				candidate = extracted
			}
		default:
			if extracted := compatExtractOpenAIPayload(payload); len(extracted) != 0 {
				candidate = extracted
			}
		}
	}

	return candidate
}

func compatExtractResponsesPayload(payload []byte) []byte {
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "type").String()), "response.completed") {
		response := gjson.GetBytes(payload, "response")
		if response.Exists() && gjson.Valid(response.Raw) {
			return []byte(response.Raw)
		}
	}

	objectType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "object").String()))
	if strings.HasPrefix(objectType, "response") || gjson.GetBytes(payload, "output").Exists() {
		return payload
	}

	return nil
}

func compatExtractOpenAIPayload(payload []byte) []byte {
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "type").String()), "response.completed") {
		response := gjson.GetBytes(payload, "response")
		if response.Exists() && gjson.Valid(response.Raw) {
			return []byte(response.Raw)
		}
	}

	objectType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "object").String()))
	switch objectType {
	case "chat.completion", "text_completion", "response", "response.compaction":
		return payload
	}

	if gjson.GetBytes(payload, "id").Exists() && gjson.GetBytes(payload, "choices").Exists() {
		return payload
	}

	return nil
}

func compatDisplayModel(requestedModel string) string {
	trimmed := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if trimmed == "" {
		return ""
	}
	if slash := strings.LastIndex(trimmed, "/"); slash >= 0 && slash < len(trimmed)-1 {
		return strings.TrimSpace(trimmed[slash+1:])
	}
	return trimmed
}

func compatRequestedModelAllowsThinking(modelName string) bool {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return true
	}
	suffix := thinking.ParseSuffix(trimmed)
	if strings.Contains(strings.ToLower(suffix.ModelName), "thinking") {
		return true
	}
	if !suffix.HasSuffix {
		return false
	}
	rawSuffix := strings.ToLower(strings.TrimSpace(suffix.RawSuffix))
	return rawSuffix != "" && rawSuffix != "none" && rawSuffix != "0"
}

type statusErr struct {
	code       int
	msg        string
	retryAfter *time.Duration
}

func (e statusErr) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("status %d", e.code)
}
func (e statusErr) StatusCode() int            { return e.code }
func (e statusErr) RetryAfter() *time.Duration { return e.retryAfter }
