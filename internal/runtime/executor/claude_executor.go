package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

// ClaudeExecutor is a stateless executor for Anthropic Claude over the messages API.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg *config.Config
}

const claudeToolPrefix = "proxy_"
const covsClaudeMinMaxTokens int64 = 64
const covsClaudeRetryMaxTokens int64 = 4096
const covsClaudeContinuationRetryMaxTokens int64 = 1024
const covsClaudeTerseRetrySystemPrompt = "Return only the final answer. Do not include analysis, reasoning, or preamble. If the user requests a specific output format, follow it exactly."
const covsClaudeContinuationFallbackSystemPrompt = "You are handling a Claude continuation fallback for a relay that cannot reliably consume tool_result blocks. Use the provided completed tool outputs as authoritative results. Do not call any tools. Answer the user directly and do not claim the tool results are missing."
const covsClaudePseudoToolStubFallbackSystemPrompt = "Answer the user directly without calling tools. Do not mention tool availability, search limitations, or inability to browse. Do not emit tool-call tags, XML, placeholders, or scaffold text such as <claude:tool_call>. Return the final answer only."
const claudeThirdPartyRelayLargePromptBytes = 20_000
const claudeThirdPartyRelayLargePromptMaxTokens int64 = 256
const claudeThirdPartyRelayLargeToolPayloadBytes = 40_000
const claudeThirdPartyRelayLargeToolPayloadMaxTokens int64 = 1024
const claudeThirdPartyRelaySystemPrefix = "Follow these instructions in addition to the conversation:"

var claudeRelaySystemReminderPattern = regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`)
var claudeRelayBlankLinePattern = regexp.MustCompile(`\n{3,}`)

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

func (e *ClaudeExecutor) Identifier() string { return "claude" }

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := claudeCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	if shouldUseClaudeAPIKeyAuth(req, attrs) {
		req.Header.Del("Authorization")
		req.Header.Set("x-api-key", apiKey)
	} else {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	util.ApplyReservedProviderHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
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

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if shouldUseClaudeOpenAICompatProfile(auth) {
		return NewOpenAICompatExecutor(e.Identifier(), e.cfg).Execute(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)
	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	nativePassthrough := shouldUseClaudeNativePassthrough(auth, from, to)
	preserveClaudeCodePrompt := shouldPreserveClaudeCodePromptForExternalCLICompat(ctx, from, to)
	rawUpstreamPassthrough := shouldUseRawClaudeUpstreamPayload(baseURL, nativePassthrough, from, to)
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, stream)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	requestedModel := payloadRequestedModel(opts, req.Model)
	if rawUpstreamPassthrough {
		body = bytes.Clone(originalPayload)
		body, _ = sjson.SetBytes(body, "model", baseModel)
	} else {
		body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
		if err != nil {
			return resp, err
		}

		// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
		// based on client type and configuration. Third-party Claude relays get a minimal payload.
		if !shouldUseClaudeThirdPartyRelayProfile(baseURL) {
			body = applyCloaking(ctx, e.cfg, auth, body, baseModel)
		} else if preserveClaudeCodePrompt {
			body = checkSystemInstructions(body)
		}

		body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

		// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
		body = disableThinkingIfToolChoiceForced(body)
		body = normalizeClaudeMessagesPayload(body)
		body = normalizeCOVSClaudeRequest(body, auth)

		// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
		if shouldUseClaudeThirdPartyRelayProfile(baseURL) {
			body = minimizeClaudeThirdPartyRelayPayloadWithOptions(body, preserveClaudeCodePrompt)
		} else if countCacheControls(body) == 0 {
			body = ensureCacheControl(body)
		}
	}
	body = normalizeCOVSClaudeRequest(body, auth)

	// Extract betas from body and convert to header
	var extraBetas []string
	if !nativePassthrough {
		extraBetas, body = extractAndRemoveBetas(body)
	}
	bodyForTranslation := body
	bodyForUpstream := body
	if !nativePassthrough && isClaudeOAuthToken(apiKey) {
		bodyForUpstream = applyClaudeToolPrefix(body, claudeToolPrefix)
	}

	url := buildClaudeMessagesRequestURL(baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return resp, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, nativePassthrough)
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
		Body:      bodyForUpstream,
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
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if !nativePassthrough {
		if retried, recovered, retryErr := e.retryCOVSClaudeEmptyNonStream(ctx, auth, apiKey, url, extraBetas, bodyForUpstream, data); retryErr != nil {
			return resp, retryErr
		} else if recovered {
			data = retried
		}
		if covsClaudeShouldRetryToolContinuation(bodyForUpstream, data, auth) {
			if retried, recovered, retryErr := e.retryCOVSClaudeToolContinuation(ctx, auth, apiKey, url, extraBetas, bodyForUpstream); retryErr != nil {
				return resp, retryErr
			} else if recovered {
				data = retried
			}
		}
		if retried, recovered, retryErr := e.retryCOVSClaudePseudoToolStub(ctx, auth, apiKey, url, extraBetas, bodyForUpstream, data); retryErr != nil {
			return resp, retryErr
		} else if recovered {
			data = retried
		}
	}
	if !nativePassthrough && claudeShouldRejectShortContinuationResponse(bodyForTranslation, data) {
		text, outputTokens, _ := claudeResponseSurfaceSummary(data)
		errShort := statusErr{
			code: http.StatusBadGateway,
			msg:  fmt.Sprintf("claude executor: rejected short continuation response (output_tokens=%d text=%q)", outputTokens, strings.TrimSpace(text)),
		}
		recordAPIResponseError(ctx, e.cfg, errShort)
		return resp, errShort
	}
	if !nativePassthrough && claudeShouldRejectForeignAssistantIdentityResponse(data) {
		text, _, _ := claudeResponseSurfaceSummary(data)
		errForeign := statusErr{
			code: http.StatusBadGateway,
			msg:  fmt.Sprintf("claude executor: rejected foreign assistant identity response (text=%q)", strings.TrimSpace(text)),
		}
		recordAPIResponseError(ctx, e.cfg, errForeign)
		return resp, errForeign
	}
	if !nativePassthrough && claudeShouldRejectInterruptedAgentScaffoldResponse(bodyForTranslation, data) {
		text, _, _ := claudeResponseSurfaceSummary(data)
		errInterrupted := statusErr{
			code: http.StatusBadGateway,
			msg:  fmt.Sprintf("claude executor: rejected interrupted agent scaffold response (text=%q)", strings.TrimSpace(text)),
		}
		recordAPIResponseError(ctx, e.cfg, errInterrupted)
		return resp, errInterrupted
	}
	if !nativePassthrough && claudeShouldRejectPseudoToolStubResponse(data) {
		text := strings.TrimSpace(covsClaudeSurfaceText(data))
		errPseudoToolStub := statusErr{
			code: http.StatusBadGateway,
			msg:  fmt.Sprintf("claude executor: rejected pseudo tool stub response (text=%q)", text),
		}
		recordAPIResponseError(ctx, e.cfg, errPseudoToolStub)
		return resp, errPseudoToolStub
	}
	if stream {
		lines := bytes.Split(data, []byte("\n"))
		for _, line := range lines {
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
		}
	} else {
		reporter.publish(ctx, parseClaudeUsage(data))
	}
	if !nativePassthrough && isClaudeOAuthToken(apiKey) {
		data = stripClaudeToolPrefixFromResponse(data, claudeToolPrefix)
	}
	if nativePassthrough {
		resp.Payload = data
		return resp, nil
	}
	translationData := data
	if stream && gjson.ValidBytes(data) {
		if synthesized := synthesizeClaudeStreamLinesFromResponse(data, requestedModel); len(synthesized) > 0 {
			translationData = bytes.Join(synthesized, []byte("\n"))
		}
	}
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		to,
		from,
		req.Model,
		opts.OriginalRequest,
		bodyForTranslation,
		translationData,
		&param,
	)
	out = string(normalizeClaudeFinalResponse([]byte(out), requestedModel, from.String()))
	resp = cliproxyexecutor.Response{Payload: []byte(out)}
	return resp, nil
}

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	if shouldUseClaudeOpenAICompatProfile(auth) {
		return NewOpenAICompatExecutor(e.Identifier(), e.cfg).ExecuteStream(ctx, auth, req, opts)
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)
	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	nativePassthrough := shouldUseClaudeNativePassthrough(auth, from, to)
	preserveClaudeCodePrompt := shouldPreserveClaudeCodePromptForExternalCLICompat(ctx, from, to)
	rawUpstreamPassthrough := shouldUseRawClaudeUpstreamPayload(baseURL, nativePassthrough, from, to)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	requestedModel := payloadRequestedModel(opts, req.Model)
	if rawUpstreamPassthrough {
		body = bytes.Clone(originalPayload)
		body, _ = sjson.SetBytes(body, "model", baseModel)
	} else {
		body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
		if err != nil {
			return nil, err
		}

		// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
		// based on client type and configuration. Third-party Claude relays get a minimal payload.
		if !shouldUseClaudeThirdPartyRelayProfile(baseURL) {
			body = applyCloaking(ctx, e.cfg, auth, body, baseModel)
		} else if preserveClaudeCodePrompt {
			body = checkSystemInstructions(body)
		}

		body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

		// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
		body = disableThinkingIfToolChoiceForced(body)
		body = normalizeClaudeMessagesPayload(body)
		body = normalizeCOVSClaudeRequest(body, auth)

		// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
		if shouldUseClaudeThirdPartyRelayProfile(baseURL) {
			body = minimizeClaudeThirdPartyRelayPayloadWithOptions(body, preserveClaudeCodePrompt)
		} else if countCacheControls(body) == 0 {
			body = ensureCacheControl(body)
		}
	}
	body = normalizeCOVSClaudeRequest(body, auth)

	// Extract betas from body and convert to header
	var extraBetas []string
	if !nativePassthrough {
		extraBetas, body = extractAndRemoveBetas(body)
	}
	bodyForTranslation := body
	bodyForUpstream := body
	if !nativePassthrough && isClaudeOAuthToken(apiKey) {
		bodyForUpstream = applyClaudeToolPrefix(body, claudeToolPrefix)
	}

	url := buildClaudeMessagesRequestURL(baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return nil, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, true, extraBetas, nativePassthrough)
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
		Body:      bodyForUpstream,
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
			log.Errorf("response body close error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()

		if shouldBufferCOVSClaudeStream(auth, nativePassthrough, from, to, bodyForTranslation) {
			buffered, errRead := readBufferedClaudeStream(decodedBody, ctx, e.cfg, apiKey, isYunyiClaudeRelayAuth(auth) || isYunyiClaudeRelayBaseURL(baseURL))
			if errRead != nil {
				recordAPIResponseError(ctx, e.cfg, errRead)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errRead}
				return
			}

			if buffered.shouldFallback() {
				if retried, recovered, retryErr := e.retryCOVSClaudeEmptyNonStream(ctx, auth, apiKey, url, extraBetas, bodyForUpstream, nil); retryErr != nil {
					recordAPIResponseError(ctx, e.cfg, retryErr)
					reporter.publishFailure(ctx)
					out <- cliproxyexecutor.StreamChunk{Err: retryErr}
					return
				} else if recovered {
					reporter.publish(ctx, parseClaudeUsage(retried))
					fallbackLines := synthesizeClaudeStreamLinesFromResponse(retried, requestedModel)
					if from == to {
						emitNormalizedClaudeStream(out, newClaudeStreamNormalizer(requestedModel, auth), fallbackLines)
					} else {
						emitTranslatedClaudeStream(ctx, out, to, from, req.Model, opts.OriginalRequest, bodyForTranslation, fallbackLines)
					}
					return
				}
			}
			bufferedRaw := flattenBufferedClaudeEvents(buffered.events)
			if covsClaudeShouldRetryToolContinuation(bodyForUpstream, bufferedRaw, auth) {
				if retried, recovered, retryErr := e.retryCOVSClaudeToolContinuation(ctx, auth, apiKey, url, extraBetas, bodyForUpstream); retryErr != nil {
					recordAPIResponseError(ctx, e.cfg, retryErr)
					reporter.publishFailure(ctx)
					out <- cliproxyexecutor.StreamChunk{Err: retryErr}
					return
				} else if recovered {
					reporter.publish(ctx, parseClaudeUsage(retried))
					fallbackLines := synthesizeClaudeStreamLinesFromResponse(retried, requestedModel)
					if from == to {
						emitNormalizedClaudeStream(out, newClaudeStreamNormalizer(requestedModel, auth), fallbackLines)
					} else {
						emitTranslatedClaudeStream(ctx, out, to, from, req.Model, opts.OriginalRequest, bodyForTranslation, fallbackLines)
					}
					return
				}
			}
			if retried, recovered, retryErr := e.retryCOVSClaudePseudoToolStub(ctx, auth, apiKey, url, extraBetas, bodyForUpstream, bufferedRaw); retryErr != nil {
				recordAPIResponseError(ctx, e.cfg, retryErr)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: retryErr}
				return
			} else if recovered {
				reporter.publish(ctx, parseClaudeUsage(retried))
				fallbackLines := synthesizeClaudeStreamLinesFromResponse(retried, requestedModel)
				if from == to {
					emitNormalizedClaudeStream(out, newClaudeStreamNormalizer(requestedModel, auth), fallbackLines)
				} else {
					emitTranslatedClaudeStream(ctx, out, to, from, req.Model, opts.OriginalRequest, bodyForTranslation, fallbackLines)
				}
				return
			}
			if !nativePassthrough && claudeShouldRejectPseudoToolStubResponse(bufferedRaw) {
				text := strings.TrimSpace(covsClaudeSurfaceText(bufferedRaw))
				errPseudoToolStub := statusErr{
					code: http.StatusBadGateway,
					msg:  fmt.Sprintf("claude executor: rejected pseudo tool stub response (text=%q)", text),
				}
				recordAPIResponseError(ctx, e.cfg, errPseudoToolStub)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errPseudoToolStub}
				return
			}

			if buffered.usage.TotalTokens > 0 || buffered.usage.InputTokens > 0 || buffered.usage.OutputTokens > 0 || buffered.usage.CachedTokens > 0 {
				reporter.publish(ctx, buffered.usage)
			}
			if from == to {
				emitNormalizedClaudeEvents(out, newClaudeStreamNormalizer(requestedModel, auth), buffered.events)
			} else {
				normalizedEvents := normalizeClaudeEvents(newClaudeStreamNormalizer(requestedModel, auth), buffered.events)
				emitTranslatedClaudeEvents(ctx, out, to, from, req.Model, opts.OriginalRequest, bodyForTranslation, normalizedEvents)
			}
			return
		}

		// If from == to (Claude → Claude), directly forward the SSE stream without translation
		if from == to {
			if nativePassthrough {
				scanner := bufio.NewScanner(decodedBody)
				scanner.Buffer(nil, 52_428_800) // 50MB
				var sawMessageStop bool
				var sawVisibleContent bool
				for scanner.Scan() {
					line := scanner.Bytes()
					appendAPIResponseChunk(ctx, e.cfg, line)
					if claudeStreamSawMessageStop(line) {
						sawMessageStop = true
					}
					if claudeStreamSawVisibleContent(line) {
						sawVisibleContent = true
					}
					if detail, ok := parseClaudeStreamUsage(line); ok {
						reporter.publish(ctx, detail)
					}
					cloned := make([]byte, len(line)+1)
					copy(cloned, line)
					cloned[len(line)] = '\n'
					out <- cliproxyexecutor.StreamChunk{Payload: cloned}
				}
				if ctx != nil && ctx.Err() != nil {
					return
				}
				if errScan := scanner.Err(); errScan != nil {
					if suppressClaudeTailStreamError(ctx, errScan, sawMessageStop, shouldTolerateClaudeMissingMessageStop(auth, baseURL, sawVisibleContent), sawVisibleContent) {
						return
					}
					recordAPIResponseError(ctx, e.cfg, errScan)
					reporter.publishFailure(ctx)
					out <- cliproxyexecutor.StreamChunk{Err: errScan}
					return
				}
				if !sawMessageStop {
					if shouldTolerateClaudeMissingMessageStop(auth, baseURL, sawVisibleContent) {
						logWithRequestID(ctx).Warn("accepting Claude stream without message_stop after visible content")
						return
					}
					errIncomplete := claudeIncompleteStreamError()
					recordAPIResponseError(ctx, e.cfg, errIncomplete)
					reporter.publishFailure(ctx)
					out <- cliproxyexecutor.StreamChunk{Err: errIncomplete}
				}
				return
			}
			normalizer := newClaudeStreamNormalizer(requestedModel, auth)
			scanner := bufio.NewScanner(decodedBody)
			scanner.Buffer(nil, 52_428_800) // 50MB
			var eventLines [][]byte
			var sawMessageStop bool
			var sawVisibleContent bool
			flushEvent := func(force bool) {
				if len(eventLines) == 0 {
					return
				}
				normalized := normalizer.NormalizeEvent(eventLines)
				for _, normalizedLine := range normalized {
					cloned := make([]byte, len(normalizedLine)+1)
					copy(cloned, normalizedLine)
					cloned[len(normalizedLine)] = '\n'
					out <- cliproxyexecutor.StreamChunk{Payload: cloned}
				}
				if len(normalized) > 0 && !force {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte("\n")}
				}
				eventLines = nil
			}
			for scanner.Scan() {
				line := scanner.Bytes()
				appendAPIResponseChunk(ctx, e.cfg, line)
				if claudeStreamSawMessageStop(line) {
					sawMessageStop = true
				}
				if detail, ok := parseClaudeStreamUsage(line); ok {
					reporter.publish(ctx, detail)
				}
				if isClaudeOAuthToken(apiKey) {
					line = stripClaudeToolPrefixFromStreamLine(line, claudeToolPrefix)
				}
				if claudeStreamSawVisibleContent(line) {
					sawVisibleContent = true
				}
				if len(bytes.TrimSpace(line)) == 0 {
					flushEvent(false)
					continue
				}
				eventLines = append(eventLines, bytes.Clone(line))
			}
			flushEvent(true)
			if ctx != nil && ctx.Err() != nil {
				return
			}
			if errScan := scanner.Err(); errScan != nil {
				if suppressClaudeTailStreamError(ctx, errScan, sawMessageStop, shouldTolerateClaudeMissingMessageStop(auth, baseURL, sawVisibleContent), sawVisibleContent) {
					return
				}
				recordAPIResponseError(ctx, e.cfg, errScan)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errScan}
				return
			}
			if !sawMessageStop {
				if shouldTolerateClaudeMissingMessageStop(auth, baseURL, sawVisibleContent) {
					logWithRequestID(ctx).Warn("accepting Claude stream without message_stop after visible content")
					return
				}
				errIncomplete := claudeIncompleteStreamError()
				recordAPIResponseError(ctx, e.cfg, errIncomplete)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errIncomplete}
			}
			return
		}

		// For other formats, use translation
		scanner := bufio.NewScanner(decodedBody)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		var sawMessageStop bool
		var sawVisibleContent bool
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if claudeStreamSawMessageStop(line) {
				sawMessageStop = true
			}
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			if isClaudeOAuthToken(apiKey) {
				line = stripClaudeToolPrefixFromStreamLine(line, claudeToolPrefix)
			}
			if claudeStreamSawVisibleContent(line) {
				sawVisibleContent = true
			}
			chunks := sdktranslator.TranslateStream(
				ctx,
				to,
				from,
				req.Model,
				opts.OriginalRequest,
				bodyForTranslation,
				bytes.Clone(line),
				&param,
			)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if ctx != nil && ctx.Err() != nil {
			return
		}
		if errScan := scanner.Err(); errScan != nil {
			if suppressClaudeTailStreamError(ctx, errScan, sawMessageStop, shouldTolerateClaudeMissingMessageStop(auth, baseURL, sawVisibleContent), sawVisibleContent) {
				return
			}
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		if !sawMessageStop {
			if shouldTolerateClaudeMissingMessageStop(auth, baseURL, sawVisibleContent) {
				logWithRequestID(ctx).Warn("accepting Claude stream without message_stop after visible content")
				return
			}
			errIncomplete := claudeIncompleteStreamError()
			recordAPIResponseError(ctx, e.cfg, errIncomplete)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errIncomplete}
		}
	}()
	return stream, nil
}

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if shouldUseClaudeOpenAICompatProfile(auth) {
		return NewOpenAICompatExecutor(e.Identifier(), e.cfg).CountTokens(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	nativePassthrough := shouldUseClaudeNativePassthrough(auth, from, to)
	preserveClaudeCodePrompt := shouldPreserveClaudeCodePromptForExternalCLICompat(ctx, from, to)
	rawUpstreamPassthrough := shouldUseRawClaudeUpstreamPayload(baseURL, nativePassthrough, from, to)
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	if rawUpstreamPassthrough {
		body = bytes.Clone(req.Payload)
		body, _ = sjson.SetBytes(body, "model", baseModel)
	} else {
		if !shouldUseClaudeThirdPartyRelayProfile(baseURL) && !strings.HasPrefix(baseModel, "claude-3-5-haiku") {
			body = checkSystemInstructions(body)
		} else if preserveClaudeCodePrompt && !strings.HasPrefix(baseModel, "claude-3-5-haiku") {
			body = checkSystemInstructions(body)
		}
		body = normalizeClaudeMessagesPayload(body)
		if shouldUseClaudeThirdPartyRelayProfile(baseURL) {
			body = minimizeClaudeThirdPartyRelayPayloadWithOptions(body, preserveClaudeCodePrompt)
		}
	}

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	if !nativePassthrough {
		extraBetas, body = extractAndRemoveBetas(body)
	}
	if !nativePassthrough && isClaudeOAuthToken(apiKey) {
		body = applyClaudeToolPrefix(body, claudeToolPrefix)
	}

	url := buildClaudeMessagesCountTokensRequestURL(baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, nativePassthrough)
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
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	resp, err := doRequestWithTimeoutRetry(ctx, e.cfg, auth, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: string(b)}
	}
	decodedBody, err := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	count := gjson.GetBytes(data, "input_tokens").Int()
	out := sdktranslator.TranslateTokenCount(ctx, to, from, count, data)
	return cliproxyexecutor.Response{Payload: []byte(out)}, nil
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	svc := claudeauth.NewClaudeAuth(e.cfg)
	td, err := svc.RefreshTokens(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

// extractAndRemoveBetas extracts the "betas" array from the body and removes it.
// Returns the extracted betas as a string slice and the modified body.
func extractAndRemoveBetas(body []byte) ([]string, []byte) {
	betasResult := gjson.GetBytes(body, "betas")
	if !betasResult.Exists() {
		return nil, body
	}
	var betas []string
	if betasResult.IsArray() {
		for _, item := range betasResult.Array() {
			if s := strings.TrimSpace(item.String()); s != "" {
				betas = append(betas, s)
			}
		}
	} else if s := strings.TrimSpace(betasResult.String()); s != "" {
		betas = append(betas, s)
	}
	body, _ = sjson.DeleteBytes(body, "betas")
	return betas, body
}

// disableThinkingIfToolChoiceForced checks if tool_choice forces tool use and disables thinking.
// Anthropic API does not allow thinking when tool_choice is set to "any" or a specific tool.
// See: https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking#important-considerations
func disableThinkingIfToolChoiceForced(body []byte) []byte {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		// Remove thinking configuration entirely to avoid API error
		body, _ = sjson.DeleteBytes(body, "thinking")
	}
	return body
}

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for i := range c.closers {
		if c.closers[i] == nil {
			continue
		}
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func decodeResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	if contentEncoding == "" {
		return body, nil
	}
	encodings := strings.Split(contentEncoding, ",")
	for _, raw := range encodings {
		encoding := strings.TrimSpace(strings.ToLower(raw))
		switch encoding {
		case "", "identity":
			continue
		case "gzip":
			gzipReader, err := gzip.NewReader(body)
			if err != nil {
				_ = body.Close()
				return nil, fmt.Errorf("failed to create gzip reader: %w", err)
			}
			return &compositeReadCloser{
				Reader: gzipReader,
				closers: []func() error{
					gzipReader.Close,
					func() error { return body.Close() },
				},
			}, nil
		case "deflate":
			deflateReader := flate.NewReader(body)
			return &compositeReadCloser{
				Reader: deflateReader,
				closers: []func() error{
					deflateReader.Close,
					func() error { return body.Close() },
				},
			}, nil
		case "br":
			return &compositeReadCloser{
				Reader: brotli.NewReader(body),
				closers: []func() error{
					func() error { return body.Close() },
				},
			}, nil
		case "zstd":
			decoder, err := zstd.NewReader(body)
			if err != nil {
				_ = body.Close()
				return nil, fmt.Errorf("failed to create zstd reader: %w", err)
			}
			return &compositeReadCloser{
				Reader: decoder,
				closers: []func() error{
					func() error { decoder.Close(); return nil },
					func() error { return body.Close() },
				},
			}, nil
		default:
			continue
		}
	}
	return body, nil
}

func normalizeCOVSClaudeRequest(body []byte, auth *cliproxyauth.Auth) []byte {
	if !isCOVSClaudeAuth(auth) {
		return body
	}
	updated := stripCOVSClaudeCodeBootstrapPrompt(body)
	maxTokens := gjson.GetBytes(updated, "max_tokens")
	if maxTokens.Exists() && maxTokens.Int() >= covsClaudeMinMaxTokens {
		return updated
	}
	updated, err := sjson.SetBytes(updated, "max_tokens", covsClaudeMinMaxTokens)
	if err != nil {
		return updated
	}
	return updated
}

func stripCOVSClaudeCodeBootstrapPrompt(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	out := body
	if system := gjson.GetBytes(out, "system"); system.Exists() {
		switch {
		case system.Type == gjson.String:
			if isCOVSClaudeCodeBootstrapText(system.String()) {
				out, _ = sjson.DeleteBytes(out, "system")
			}
		case system.IsArray():
			filtered := "[]"
			kept := 0
			changed := false
			for _, part := range system.Array() {
				if part.Get("type").String() == "text" && isCOVSClaudeCodeBootstrapText(part.Get("text").String()) {
					changed = true
					continue
				}
				filtered, _ = sjson.SetRaw(filtered, "-1", part.Raw)
				kept++
			}
			if changed {
				if kept == 0 {
					out, _ = sjson.DeleteBytes(out, "system")
				} else {
					out, _ = sjson.SetRawBytes(out, "system", []byte(filtered))
				}
			}
		}
	}

	messages := gjson.GetBytes(out, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return out
	}
	filteredMessages := "[]"
	changed := false
	for _, message := range messages.Array() {
		messageRaw, keep := stripCOVSClaudeCodeBootstrapSystemMessage(message)
		if !keep {
			changed = true
			continue
		}
		if messageRaw != message.Raw {
			changed = true
		}
		filteredMessages, _ = sjson.SetRaw(filteredMessages, "-1", messageRaw)
	}
	if !changed {
		return out
	}
	out, _ = sjson.SetRawBytes(out, "messages", []byte(filteredMessages))
	return out
}

func stripCOVSClaudeCodeBootstrapSystemMessage(message gjson.Result) (string, bool) {
	if !strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
		return message.Raw, true
	}

	content := message.Get("content")
	if !content.Exists() {
		return message.Raw, true
	}
	if content.Type == gjson.String {
		if isCOVSClaudeCodeBootstrapText(content.String()) {
			return "", false
		}
		return message.Raw, true
	}
	if !content.IsArray() {
		return message.Raw, true
	}

	filtered := "[]"
	kept := 0
	changed := false
	for _, part := range content.Array() {
		if part.Get("type").String() == "text" && isCOVSClaudeCodeBootstrapText(part.Get("text").String()) {
			changed = true
			continue
		}
		filtered, _ = sjson.SetRaw(filtered, "-1", part.Raw)
		kept++
	}
	if !changed {
		return message.Raw, true
	}
	if kept == 0 {
		return "", false
	}
	updated, err := sjson.SetRaw(message.Raw, "content", filtered)
	if err != nil {
		return message.Raw, true
	}
	return updated, true
}

func isCOVSClaudeCodeBootstrapText(text string) bool {
	compact := strings.ToLower(strings.TrimSpace(text))
	if compact == "" {
		return false
	}
	return strings.Contains(compact, "x-anthropic-billing-header:") &&
		strings.Contains(compact, "you are claude code, anthropic's official cli for claude.")
}

func buildCOVSClaudeRetryBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	out := append([]byte(nil), body...)
	out, _ = sjson.SetBytes(out, "max_tokens", covsClaudeRetryMaxTokens)
	out, _ = sjson.DeleteBytes(out, "stream")
	return out
}

func compactCOVSClaudeEmptyRetryBody(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}

	out := append([]byte(nil), body...)
	for _, path := range []string{
		"tools",
		"tool_choice",
		"thinking",
		"context_management",
		"output_config",
		"metadata",
		"system",
	} {
		out, _ = sjson.DeleteBytes(out, path)
	}
	out = stripClaudeThirdPartyRelayInlinedSystemPrefix(out)
	out = compactClaudeThirdPartyRelayMessageText(out)
	out = normalizeClaudeMessagesPayload(out)
	return out
}

func buildCOVSClaudeTruncatedRetryBody(body []byte) []byte {
	out := buildCOVSClaudeRetryBody(body)
	if len(out) == 0 {
		return out
	}
	return prependClaudeSystemText(out, covsClaudeTerseRetrySystemPrompt)
}

func buildCOVSClaudeContinuationRetryBody(body []byte) ([]byte, bool) {
	prompt := buildCOVSClaudeContinuationPrompt(body)
	if strings.TrimSpace(prompt) == "" {
		return nil, false
	}

	out := append([]byte(nil), body...)
	out, _ = sjson.DeleteBytes(out, "tools")
	out, _ = sjson.DeleteBytes(out, "tool_choice")
	out, _ = sjson.DeleteBytes(out, "thinking")
	out, _ = sjson.DeleteBytes(out, "stream")
	out = ensureClaudeMaxTokensAtLeast(out, covsClaudeContinuationRetryMaxTokens)
	out = prependClaudeSystemText(out, covsClaudeContinuationFallbackSystemPrompt)

	userMessage := `{"role":"user","content":[{"type":"text","text":""}]}`
	userMessage, _ = sjson.Set(userMessage, "content.0.text", prompt)
	out, _ = sjson.SetRawBytes(out, "messages", []byte("["+userMessage+"]"))
	return out, true
}

func buildCOVSClaudePseudoToolRetryBody(body []byte) []byte {
	out := buildCOVSClaudeRetryBody(body)
	if len(out) == 0 {
		return out
	}
	out = compactCOVSClaudeEmptyRetryBody(out)
	return prependClaudeSystemText(out, covsClaudePseudoToolStubFallbackSystemPrompt)
}

func prependClaudeSystemText(body []byte, text string) []byte {
	if len(body) == 0 || strings.TrimSpace(text) == "" {
		return body
	}
	system := gjson.GetBytes(body, "system")
	prefix := map[string]any{
		"type": "text",
		"text": text,
	}
	switch {
	case !system.Exists():
		out, err := sjson.SetBytes(body, "system", []map[string]any{prefix})
		if err == nil {
			return out
		}
	case system.IsArray():
		items := make([]map[string]any, 0, len(system.Array())+1)
		items = append(items, prefix)
		for _, item := range system.Array() {
			items = append(items, map[string]any{
				"type": item.Get("type").String(),
				"text": item.Get("text").String(),
			})
		}
		out, err := sjson.SetBytes(body, "system", items)
		if err == nil {
			return out
		}
	case system.Type == gjson.String:
		out, err := sjson.SetBytes(body, "system", []map[string]any{
			prefix,
			{
				"type": "text",
				"text": system.String(),
			},
		})
		if err == nil {
			return out
		}
	}
	return body
}

func covsClaudeHasVisibleContent(raw []byte) bool {
	if len(raw) == 0 || !gjson.ValidBytes(raw) {
		return false
	}
	content := gjson.GetBytes(raw, "content")
	if !content.Exists() || !content.IsArray() {
		return false
	}
	for _, item := range content.Array() {
		switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
		case "text":
			if strings.TrimSpace(item.Get("text").String()) != "" {
				return true
			}
		case "tool_use":
			return true
		}
	}
	return false
}

func claudeShouldRejectShortContinuationResponse(requestPayload, responsePayload []byte) bool {
	if !codexRequestLooksLikeContinuation(requestPayload) || len(responsePayload) == 0 || !gjson.ValidBytes(responsePayload) {
		return false
	}
	text, outputTokens, hasToolOrReasoning := claudeResponseSurfaceSummary(responsePayload)
	if hasToolOrReasoning {
		return false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if !codexTextLooksLikeWorkPromise(text) {
		return false
	}
	if outputTokens > codexShortContinuationMaxOutputTokens {
		return false
	}
	if utf8.RuneCountInString(text) > codexShortContinuationMaxRunes {
		return false
	}
	if strings.Contains(text, "```") || strings.Contains(text, "`") {
		return false
	}
	return true
}

func claudeShouldRejectInterruptedAgentScaffoldResponse(requestPayload, responsePayload []byte) bool {
	if !codexRequestLooksLikeContinuation(requestPayload) || len(responsePayload) == 0 || !gjson.ValidBytes(responsePayload) {
		return false
	}
	text, outputTokens, hasToolOrReasoning := claudeResponseSurfaceSummary(responsePayload)
	if hasToolOrReasoning {
		return false
	}
	return claudeTextLooksLikeInterruptedAgentScaffold(text, outputTokens)
}

func claudeShouldRejectForeignAssistantIdentityResponse(responsePayload []byte) bool {
	if len(responsePayload) == 0 || !gjson.ValidBytes(responsePayload) {
		return false
	}
	text, _, _ := claudeResponseSurfaceSummary(responsePayload)
	return claudeTextLooksLikeForeignAssistantIdentity(text)
}

func claudeShouldRejectPseudoToolStubResponse(responsePayload []byte) bool {
	return claudeResponseLooksLikePseudoToolStub(responsePayload)
}

func claudeTextLooksLikeInterruptedAgentScaffold(text string, outputTokens int64) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if outputTokens > codexShortContinuationMaxOutputTokens*4 {
		return false
	}

	normalized := strings.ToLower(text)
	compact := strings.NewReplacer(
		" ", "",
		"\n", "",
		"\r", "",
		"\t", "",
		"·", "",
		"-", "",
		"_", "",
		"(", "",
		")", "",
		"[", "",
		"]", "",
	).Replace(normalized)

	if strings.Contains(compact, "interrupted") && strings.Contains(compact, "whatshouldclaudedo") {
		return true
	}
	if strings.Contains(compact, "ctrl+otoexpand") &&
		(strings.Contains(compact, "searchedfor1pattern") ||
			strings.Contains(compact, "read1file") ||
			strings.Contains(compact, "read2files") ||
			strings.Contains(compact, "recalled1memory")) {
		return true
	}
	return false
}

func claudeTextLooksLikeForeignAssistantIdentity(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	compact := strings.NewReplacer(
		" ", "",
		"\n", "",
		"\r", "",
		"\t", "",
		"，", "",
		"。", "",
		"！", "",
		"!", "",
		"：", "",
		":", "",
		"、", "",
		",", "",
		".", "",
		"*", "",
		"_", "",
		"`", "",
		"“", "",
		"”", "",
		"‘", "",
		"’", "",
		"(", "",
		")", "",
		"[", "",
		"]", "",
		"{", "",
		"}", "",
		"<", "",
		">", "",
		"'", "",
		"’", "",
	).Replace(normalized)
	if !strings.Contains(compact, "cursor") {
		return false
	}
	selfIntro := strings.HasPrefix(compact, "我是cursor") ||
		strings.HasPrefix(compact, "你好我是cursor") ||
		strings.HasPrefix(compact, "您好我是cursor") ||
		strings.HasPrefix(compact, "嗨我是cursor") ||
		strings.HasPrefix(compact, "hiimcursor") ||
		strings.HasPrefix(compact, "hiiamcursor") ||
		strings.HasPrefix(compact, "helloimcursor") ||
		strings.HasPrefix(compact, "helloiamcursor") ||
		strings.HasPrefix(compact, "imcursor") ||
		strings.HasPrefix(compact, "iamcursor")
	if !selfIntro {
		return false
	}
	return strings.Contains(compact, "anysphere") ||
		strings.Contains(compact, "ai助手") ||
		strings.Contains(compact, "aiassistant") ||
		strings.Contains(compact, "代码助手") ||
		strings.Contains(compact, "codingassistant")
}

func claudeTextLooksLikePseudoToolStub(text string, outputTokens int64) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	if outputTokens > codexShortContinuationMaxOutputTokens*4 {
		return false
	}
	const pseudoToolTag = "<claude:tool_call>"
	const pseudoToolPair = "<claude:tool_call></claude:tool_call>"
	compact := strings.NewReplacer(
		" ", "",
		"\n", "",
		"\r", "",
		"\t", "",
		"`", "",
		"*", "",
	).Replace(strings.ToLower(text))
	switch compact {
	case pseudoToolPair, pseudoToolTag, "claude:tool_call", "●claude:tool_call":
		return true
	}
	if strings.HasPrefix(compact, pseudoToolTag) &&
		strings.HasSuffix(compact, "</claude:tool_call>") &&
		utf8.RuneCountInString(compact) <= 128 {
		return true
	}
	if idx := strings.Index(compact, pseudoToolTag); idx >= 0 {
		prefixRunes := utf8.RuneCountInString(compact[:idx])
		suffix := compact[idx:]
		if prefixRunes <= 48 {
			return suffix == pseudoToolTag ||
				suffix == pseudoToolPair ||
				(strings.HasPrefix(suffix, pseudoToolTag) &&
					strings.HasSuffix(suffix, "</claude:tool_call>") &&
					utf8.RuneCountInString(suffix) <= 128)
		}
	}
	if idx := strings.Index(compact, "●claude:tool_call"); idx >= 0 {
		prefixRunes := utf8.RuneCountInString(compact[:idx])
		suffix := compact[idx:]
		return prefixRunes <= 48 && suffix == "●claude:tool_call"
	}
	return false
}

func claudeResponseLooksLikePseudoToolStub(raw []byte) bool {
	if len(raw) == 0 || covsClaudeResponseRequestsToolContinuation(raw) {
		return false
	}
	text := covsClaudeVisibleText(raw)
	if strings.TrimSpace(text) == "" {
		text = covsClaudeSurfaceText(raw)
	}
	var outputTokens int64
	if gjson.ValidBytes(raw) {
		_, outputTokens, _ = claudeResponseSurfaceSummary(raw)
	}
	return claudeTextLooksLikePseudoToolStub(text, outputTokens)
}

func claudeResponseSurfaceSummary(raw []byte) (string, int64, bool) {
	if len(raw) == 0 || !gjson.ValidBytes(raw) {
		return "", 0, false
	}

	parts := make([]string, 0, 4)
	hasToolOrReasoning := false
	appendText := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	appendBlocks := func(blocks gjson.Result) {
		if !blocks.Exists() {
			return
		}
		switch {
		case blocks.Type == gjson.String:
			appendText(blocks.String())
		case blocks.IsArray():
			for _, item := range blocks.Array() {
				switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
				case "text", "output_text":
					appendText(item.Get("text").String())
				case "tool_use", "tool_call", "function_call", "thinking", "redacted_thinking", "reasoning":
					hasToolOrReasoning = true
				}
			}
		}
	}

	appendBlocks(gjson.GetBytes(raw, "content"))

	if choices := gjson.GetBytes(raw, "choices"); choices.Exists() && choices.IsArray() {
		for _, choice := range choices.Array() {
			message := choice.Get("message")
			appendBlocks(message.Get("content"))
			appendText(message.Get("reasoning_content").String())
			if message.Get("tool_calls").Exists() {
				hasToolOrReasoning = true
			}
			appendBlocks(choice.Get("delta.content"))
			appendText(choice.Get("delta.reasoning_content").String())
			if choice.Get("delta.tool_calls").Exists() {
				hasToolOrReasoning = true
			}
		}
	}

	appendClaudeResponseOutputItems := func(items gjson.Result) {
		if !items.Exists() || !items.IsArray() {
			return
		}
		for _, item := range items.Array() {
			switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
			case "message":
				appendBlocks(item.Get("content"))
			case "function_call", "tool_call", "reasoning":
				hasToolOrReasoning = true
			}
		}
	}
	appendClaudeResponseOutputItems(gjson.GetBytes(raw, "output"))
	appendClaudeResponseOutputItems(gjson.GetBytes(raw, "response.output"))

	outputTokens := parseClaudeUsage(raw).OutputTokens
	if outputTokens == 0 {
		outputTokens = parseOpenAIUsage(raw).OutputTokens
	}
	if outputTokens == 0 {
		if detail, ok := parseCodexUsage(raw); ok {
			outputTokens = detail.OutputTokens
		}
	}
	return strings.Join(parts, "\n"), outputTokens, hasToolOrReasoning
}

func covsClaudeShouldRetryEmptyResponse(raw []byte, auth *cliproxyauth.Auth) bool {
	if !isCOVSClaudeAuth(auth) || len(raw) == 0 || !gjson.ValidBytes(raw) {
		return false
	}
	if covsClaudeHasVisibleContent(raw) {
		return false
	}
	text, _, hasToolOrReasoning := claudeResponseSurfaceSummary(raw)
	if strings.TrimSpace(text) != "" || hasToolOrReasoning {
		return false
	}
	return gjson.GetBytes(raw, "usage").Exists() ||
		gjson.GetBytes(raw, "type").Exists() ||
		gjson.GetBytes(raw, "stop_reason").Exists() ||
		gjson.GetBytes(raw, "role").Exists()
}

func covsClaudeShouldRetryTruncatedResponse(raw, bodyForUpstream []byte, auth *cliproxyauth.Auth) bool {
	if !isCOVSClaudeAuth(auth) || len(raw) == 0 || !gjson.ValidBytes(raw) {
		return false
	}
	requestedMaxTokens := gjson.GetBytes(bodyForUpstream, "max_tokens").Int()
	if requestedMaxTokens >= covsClaudeRetryMaxTokens {
		return false
	}
	if !covsClaudeHasVisibleContent(raw) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(raw, "stop_reason").String()), "max_tokens") {
		return true
	}
	if requestedMaxTokens <= 0 {
		return false
	}
	return gjson.GetBytes(raw, "usage.output_tokens").Int() >= requestedMaxTokens-1
}

func covsClaudeShouldRetryToolContinuation(requestBody, responseBody []byte, auth *cliproxyauth.Auth) bool {
	if !isCOVSClaudeAuth(auth) {
		return false
	}
	if !claudeRequestHasToolResults(requestBody) {
		return false
	}
	return covsClaudeResponseMissedToolResults(responseBody)
}

func covsClaudeShouldRetryPseudoToolStubResponse(raw []byte, auth *cliproxyauth.Auth) bool {
	return isCOVSClaudeAuth(auth) && claudeResponseLooksLikePseudoToolStub(raw)
}

func isCOVSClaudeAuth(auth *cliproxyauth.Auth) bool {
	_, baseURL := claudeCreds(auth)
	if strings.TrimSpace(baseURL) == "" {
		return false
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return strings.Contains(strings.ToLower(baseURL), "rsxermu666.cn")
	}
	return strings.EqualFold(parsed.Hostname(), "rsxermu666.cn")
}

type claudeStreamNormalizer struct {
	requestedModel   string
	suppressThinking bool
	suppressed       map[int]struct{}
}

func newClaudeStreamNormalizer(requestedModel string, auth *cliproxyauth.Auth) *claudeStreamNormalizer {
	_, baseURL := claudeCreds(auth)
	return &claudeStreamNormalizer{
		requestedModel:   requestedModel,
		suppressThinking: shouldUseClaudeThirdPartyRelayProfile(baseURL) && !claudeRequestedModelAllowsThinking(requestedModel),
		suppressed:       make(map[int]struct{}),
	}
}

func (n *claudeStreamNormalizer) NormalizeEvent(lines [][]byte) [][]byte {
	if len(lines) == 0 {
		return nil
	}
	if n == nil || !n.suppressThinking {
		return lines
	}
	dataLineIndex := -1
	for idx := range lines {
		if bytes.HasPrefix(bytes.TrimSpace(lines[idx]), []byte("data:")) {
			dataLineIndex = idx
			break
		}
	}
	if dataLineIndex < 0 {
		return lines
	}

	dataLine := lines[dataLineIndex]
	payload := bytes.TrimSpace(dataLine)
	payload = bytes.TrimSpace(payload[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !gjson.ValidBytes(payload) {
		return lines
	}

	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	indexValue := gjson.GetBytes(payload, "index")
	indexExists := indexValue.Exists()
	index := int(indexValue.Int())

	switch eventType {
	case "content_block_start":
		blockType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "content_block.type").String()))
		if blockType == "thinking" || blockType == "redacted_thinking" {
			if indexExists {
				n.suppressed[index] = struct{}{}
			}
			return nil
		}
	case "content_block_delta":
		deltaType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "delta.type").String()))
		if deltaType == "thinking_delta" {
			if indexExists {
				n.suppressed[index] = struct{}{}
			}
			return nil
		}
	case "content_block_stop":
		if indexExists {
			if _, ok := n.suppressed[index]; ok {
				return nil
			}
		}
	}
	if !indexExists {
		return lines
	}
	if _, ok := n.suppressed[index]; ok {
		return nil
	}
	remapped := n.remapIndex(index)
	if remapped == index {
		return lines
	}
	updated, err := sjson.SetBytes(payload, "index", remapped)
	if err != nil {
		return lines
	}
	out := make([][]byte, len(lines))
	copy(out, lines)
	out[dataLineIndex] = append([]byte("data: "), updated...)
	return out
}

type bufferedClaudeStream struct {
	events       [][][]byte
	sawTextDelta bool
	sawToolUse   bool
	sawThinking  bool
	usage        usage.Detail
}

func (b bufferedClaudeStream) hasVisibleContent() bool {
	return b.sawTextDelta || b.sawToolUse
}

func (b bufferedClaudeStream) shouldFallback() bool {
	return !b.hasVisibleContent()
}

func readBufferedClaudeStream(reader io.Reader, ctx context.Context, cfg *config.Config, apiKey string, tolerateMissingStop bool) (bufferedClaudeStream, error) {
	var buffered bufferedClaudeStream
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(nil, 52_428_800) // 50MB
	var eventLines [][]byte
	var sawMessageStop bool
	flushEvent := func() {
		if len(eventLines) == 0 {
			return
		}
		buffered.events = append(buffered.events, eventLines)
		eventLines = nil
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		appendAPIResponseChunk(ctx, cfg, line)
		if claudeStreamSawMessageStop(line) {
			sawMessageStop = true
		}
		if isClaudeOAuthToken(apiKey) {
			line = stripClaudeToolPrefixFromStreamLine(line, claudeToolPrefix)
		}
		if detail, ok := parseClaudeStreamUsage(line); ok {
			buffered.usage = detail
		}
		updateBufferedClaudeStreamMetadata(&buffered, line)
		if len(bytes.TrimSpace(line)) == 0 {
			flushEvent()
			continue
		}
		eventLines = append(eventLines, bytes.Clone(line))
	}
	flushEvent()
	if ctx != nil && ctx.Err() != nil {
		return buffered, ctx.Err()
	}
	if err := scanner.Err(); err != nil {
		if suppressClaudeTailStreamError(ctx, err, sawMessageStop, tolerateMissingStop, buffered.hasVisibleContent()) {
			return buffered, nil
		}
		return buffered, err
	}
	if !sawMessageStop {
		if tolerateMissingStop && buffered.hasVisibleContent() {
			logWithRequestID(ctx).Warn("accepting Claude stream without message_stop after visible content")
			return buffered, nil
		}
		return buffered, claudeIncompleteStreamError()
	}
	return buffered, nil
}

func flattenBufferedClaudeEvents(events [][][]byte) []byte {
	if len(events) == 0 {
		return nil
	}
	var raw bytes.Buffer
	for _, eventLines := range events {
		for _, line := range eventLines {
			raw.Write(line)
			raw.WriteByte('\n')
		}
		raw.WriteByte('\n')
	}
	return raw.Bytes()
}

func claudeStreamSawMessageStop(line []byte) bool {
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "type").String()), "message_stop")
}

func claudeStreamSawVisibleContent(line []byte) bool {
	var buffered bufferedClaudeStream
	updateBufferedClaudeStreamMetadata(&buffered, line)
	return buffered.hasVisibleContent()
}

func suppressClaudeTailStreamError(ctx context.Context, err error, sawMessageStop bool, tolerateVisibleTail bool, sawVisibleContent bool) bool {
	if err == nil {
		return false
	}
	if !sawMessageStop && !(tolerateVisibleTail && sawVisibleContent && isBenignClaudeTailStreamError(err)) {
		return false
	}
	logWithRequestID(ctx).WithField("error", err.Error()).Warn("suppressing Claude tail stream error after message_stop")
	return true
}

func claudeIncompleteStreamError() error {
	return statusErr{code: http.StatusRequestTimeout, msg: "stream disconnected before completion: stream closed before message_stop"}
}

func shouldTolerateClaudeMissingMessageStop(auth *cliproxyauth.Auth, baseURL string, sawVisibleContent bool) bool {
	return sawVisibleContent && (isYunyiClaudeRelayAuth(auth) || isYunyiClaudeRelayBaseURL(baseURL))
}

func isBenignClaudeTailStreamError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if msg == "" {
		return false
	}
	return strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "use of closed network connection")
}

func updateBufferedClaudeStreamMetadata(buffered *bufferedClaudeStream, line []byte) {
	if buffered == nil {
		return
	}
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	eventType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "type").String()))
	switch eventType {
	case "content_block_start":
		blockType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "content_block.type").String()))
		if blockType == "tool_use" {
			buffered.sawToolUse = true
		}
	case "content_block_delta":
		deltaType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "delta.type").String()))
		switch deltaType {
		case "text_delta":
			if strings.TrimSpace(gjson.GetBytes(payload, "delta.text").String()) != "" {
				buffered.sawTextDelta = true
			}
		case "thinking_delta":
			buffered.sawThinking = true
		}
	}
}

func emitNormalizedClaudeEvents(out chan<- cliproxyexecutor.StreamChunk, normalizer *claudeStreamNormalizer, events [][][]byte) {
	for _, eventLines := range events {
		normalized := normalizer.NormalizeEvent(eventLines)
		for _, normalizedLine := range normalized {
			cloned := make([]byte, len(normalizedLine)+1)
			copy(cloned, normalizedLine)
			cloned[len(normalizedLine)] = '\n'
			out <- cliproxyexecutor.StreamChunk{Payload: cloned}
		}
		if len(normalized) > 0 {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte("\n")}
		}
	}
}

func normalizeClaudeEvents(normalizer *claudeStreamNormalizer, events [][][]byte) [][][]byte {
	if len(events) == 0 {
		return nil
	}
	if normalizer == nil {
		return events
	}
	out := make([][][]byte, 0, len(events))
	for _, eventLines := range events {
		normalized := normalizer.NormalizeEvent(eventLines)
		if len(normalized) == 0 {
			continue
		}
		cloned := make([][]byte, len(normalized))
		for i := range normalized {
			cloned[i] = bytes.Clone(normalized[i])
		}
		out = append(out, cloned)
	}
	return out
}

func emitNormalizedClaudeStream(out chan<- cliproxyexecutor.StreamChunk, normalizer *claudeStreamNormalizer, lines [][]byte) {
	events := make([][][]byte, 0, 8)
	var current [][]byte
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			if len(current) > 0 {
				events = append(events, current)
				current = nil
			}
			continue
		}
		current = append(current, bytes.Clone(line))
	}
	if len(current) > 0 {
		events = append(events, current)
	}
	emitNormalizedClaudeEvents(out, normalizer, events)
}

func emitTranslatedClaudeEvents(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, from, to sdktranslator.Format, model string, originalRequest, bodyForTranslation []byte, events [][][]byte) {
	var param any
	for _, eventLines := range events {
		for _, line := range eventLines {
			chunks := sdktranslator.TranslateStream(ctx, from, to, model, originalRequest, bodyForTranslation, bytes.Clone(line), &param)
			for _, chunk := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}
			}
		}
		chunks := sdktranslator.TranslateStream(ctx, from, to, model, originalRequest, bodyForTranslation, []byte{}, &param)
		for _, chunk := range chunks {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}
		}
	}
}

func emitTranslatedClaudeStream(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, from, to sdktranslator.Format, model string, originalRequest, bodyForTranslation []byte, lines [][]byte) {
	var param any
	for _, line := range lines {
		chunks := sdktranslator.TranslateStream(ctx, from, to, model, originalRequest, bodyForTranslation, bytes.Clone(line), &param)
		for _, chunk := range chunks {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}
		}
	}
}

func synthesizeClaudeStreamLinesFromResponse(raw []byte, requestedModel string) [][]byte {
	if len(raw) == 0 || !gjson.ValidBytes(raw) {
		return nil
	}

	messageID := strings.TrimSpace(gjson.GetBytes(raw, "id").String())
	if messageID == "" {
		messageID = "msg_cpa_covs_retry"
	}
	model := strings.TrimSpace(gjson.GetBytes(raw, "model").String())
	if displayModel := claudeDisplayModel(requestedModel); displayModel != "" {
		model = displayModel
	}
	if model == "" {
		model = "claude"
	}

	lines := make([][]byte, 0, 16)
	appendEvent := func(name string, payload []byte) {
		lines = append(lines, []byte("event: "+name))
		lines = append(lines, append([]byte("data: "), payload...))
		lines = append(lines, []byte{})
	}

	start := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","content":[],"model":"","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`
	start, _ = sjson.Set(start, "message.id", messageID)
	start, _ = sjson.Set(start, "message.model", model)
	start, _ = sjson.Set(start, "message.usage.input_tokens", gjson.GetBytes(raw, "usage.input_tokens").Int())
	start, _ = sjson.Set(start, "message.usage.output_tokens", 0)
	appendEvent("message_start", []byte(start))

	index := 0
	for _, item := range gjson.GetBytes(raw, "content").Array() {
		typ := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if !claudeRequestedModelAllowsThinking(requestedModel) && (typ == "thinking" || typ == "redacted_thinking") {
			continue
		}
		switch typ {
		case "text":
			text := item.Get("text").String()
			if strings.TrimSpace(text) == "" {
				continue
			}
			startPayload := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, index)
			appendEvent("content_block_start", []byte(startPayload))
			deltaPayload := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, index)
			deltaPayload, _ = sjson.Set(deltaPayload, "delta.text", text)
			appendEvent("content_block_delta", []byte(deltaPayload))
			stopPayload := fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
			appendEvent("content_block_stop", []byte(stopPayload))
			index++
		case "tool_use":
			toolID := item.Get("id").String()
			toolName := item.Get("name").String()
			inputRaw := item.Get("input").Raw
			if !gjson.ValidBytes([]byte(inputRaw)) {
				inputRaw = `{}`
			}
			startPayload := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`, index)
			startPayload, _ = sjson.Set(startPayload, "content_block.id", toolID)
			startPayload, _ = sjson.Set(startPayload, "content_block.name", toolName)
			startPayload, _ = sjson.SetRaw(startPayload, "content_block.input", inputRaw)
			appendEvent("content_block_start", []byte(startPayload))
			stopPayload := fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
			appendEvent("content_block_stop", []byte(stopPayload))
			index++
		}
	}

	stopReason := strings.TrimSpace(gjson.GetBytes(raw, "stop_reason").String())
	if stopReason == "" {
		stopReason = "end_turn"
	}
	messageDelta := `{"type":"message_delta","delta":{"stop_reason":"","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`
	messageDelta, _ = sjson.Set(messageDelta, "delta.stop_reason", stopReason)
	messageDelta, _ = sjson.Set(messageDelta, "usage.input_tokens", gjson.GetBytes(raw, "usage.input_tokens").Int())
	messageDelta, _ = sjson.Set(messageDelta, "usage.output_tokens", gjson.GetBytes(raw, "usage.output_tokens").Int())
	appendEvent("message_delta", []byte(messageDelta))
	appendEvent("message_stop", []byte(`{"type":"message_stop"}`))

	return lines
}

func (e *ClaudeExecutor) retryCOVSClaudeEmptyNonStream(ctx context.Context, auth *cliproxyauth.Auth, apiKey, requestURL string, extraBetas []string, bodyForUpstream, currentData []byte) ([]byte, bool, error) {
	if !isCOVSClaudeAuth(auth) {
		return nil, false, nil
	}
	shouldRetryEmpty := covsClaudeShouldRetryEmptyResponse(currentData, auth)
	shouldRetryTruncated := covsClaudeShouldRetryTruncatedResponse(currentData, bodyForUpstream, auth)
	if len(currentData) > 0 && !shouldRetryEmpty && !shouldRetryTruncated {
		return nil, false, nil
	}
	if shouldRetryEmpty && gjson.GetBytes(bodyForUpstream, "max_tokens").Int() >= covsClaudeRetryMaxTokens {
		return nil, false, covsClaudeEmptyVisibleContentError()
	}

	retryBody := buildCOVSClaudeRetryBody(bodyForUpstream)
	if shouldRetryTruncated {
		retryBody = buildCOVSClaudeTruncatedRetryBody(bodyForUpstream)
	} else {
		retryBody = compactCOVSClaudeEmptyRetryBody(retryBody)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(retryBody))
	if err != nil {
		return nil, false, nil
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, false)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       requestURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      retryBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := doRequestWithTimeoutRetry(ctx, e.cfg, auth, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, false, covsClaudeEmptyVisibleContentError()
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		return nil, false, covsClaudeEmptyVisibleContentError()
	}

	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, false, covsClaudeEmptyVisibleContentError()
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		if shouldRetryEmpty {
			return nil, false, covsClaudeEmptyVisibleContentError()
		}
		return nil, false, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if !covsClaudeHasVisibleContent(data) {
		text, _, hasToolOrReasoning := claudeResponseSurfaceSummary(data)
		if strings.TrimSpace(text) == "" && !hasToolOrReasoning {
			return nil, false, covsClaudeEmptyVisibleContentError()
		}
	}
	return data, true, nil
}

func (e *ClaudeExecutor) retryCOVSClaudeToolContinuation(ctx context.Context, auth *cliproxyauth.Auth, apiKey, requestURL string, extraBetas []string, bodyForUpstream []byte) ([]byte, bool, error) {
	if !isCOVSClaudeAuth(auth) || !claudeRequestHasToolResults(bodyForUpstream) {
		return nil, false, nil
	}

	retryBody, ok := buildCOVSClaudeContinuationRetryBody(bodyForUpstream)
	if !ok {
		return nil, false, nil
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(retryBody))
	if err != nil {
		return nil, false, nil
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, false)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       requestURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      retryBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := doRequestWithTimeoutRetry(ctx, e.cfg, auth, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, false, covsClaudeToolContinuationError("covs continuation fallback request failed")
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		return nil, false, covsClaudeToolContinuationError("covs continuation fallback upstream returned non-success status")
	}

	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, false, covsClaudeToolContinuationError("covs continuation fallback response decode failed")
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		return nil, false, covsClaudeToolContinuationError("covs continuation fallback response read failed")
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if covsClaudeResponseRequestsToolContinuation(data) {
		return nil, false, covsClaudeToolContinuationError("covs continuation fallback still requested tools")
	}
	if !covsClaudeHasVisibleContent(data) {
		text, _, _ := claudeResponseSurfaceSummary(data)
		if strings.TrimSpace(text) == "" {
			return nil, false, covsClaudeToolContinuationError("covs continuation fallback returned no final answer")
		}
	}
	cliproxyauth.ReportRuntimeOutageSignal(ctx, http.StatusBadGateway, "covs_claude_tool_semantics_failed")
	return data, true, nil
}

func (e *ClaudeExecutor) retryCOVSClaudePseudoToolStub(ctx context.Context, auth *cliproxyauth.Auth, apiKey, requestURL string, extraBetas []string, bodyForUpstream, currentData []byte) ([]byte, bool, error) {
	if !covsClaudeShouldRetryPseudoToolStubResponse(currentData, auth) {
		return nil, false, nil
	}

	retryBody := buildCOVSClaudePseudoToolRetryBody(bodyForUpstream)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(retryBody))
	if err != nil {
		return nil, false, nil
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, false)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       requestURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      retryBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := doRequestWithTimeoutRetry(ctx, e.cfg, auth, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, false, covsClaudePseudoToolStubError()
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		return nil, false, covsClaudePseudoToolStubError()
	}

	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, false, covsClaudePseudoToolStubError()
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		return nil, false, covsClaudePseudoToolStubError()
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if claudeResponseLooksLikePseudoToolStub(data) {
		return nil, false, covsClaudePseudoToolStubError()
	}
	if !covsClaudeHasVisibleContent(data) {
		text, _, hasToolOrReasoning := claudeResponseSurfaceSummary(data)
		if strings.TrimSpace(text) == "" && !hasToolOrReasoning {
			return nil, false, covsClaudePseudoToolStubError()
		}
	}
	cliproxyauth.ReportRuntimeOutageSignal(ctx, http.StatusBadGateway, "covs_claude_pseudo_tool_stub")
	return data, true, nil
}

func covsClaudeEmptyVisibleContentError() error {
	return statusErr{
		code: http.StatusBadGateway,
		msg:  "covs upstream returned empty visible content after fallback retry",
	}
}

func covsClaudePseudoToolStubError() error {
	return statusErr{
		code: http.StatusBadGateway,
		msg:  "covs upstream returned pseudo tool stub response after fallback retry",
	}
}

func covsClaudeToolContinuationError(message string) error {
	return statusErr{
		code: http.StatusBadGateway,
		msg:  message,
	}
}

func claudeRequestHasToolResults(raw []byte) bool {
	if len(raw) == 0 || !gjson.ValidBytes(raw) {
		return false
	}
	messages := gjson.GetBytes(raw, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return false
	}
	for _, message := range messages.Array() {
		content := message.Get("content")
		if !content.Exists() || !content.IsArray() {
			continue
		}
		for _, part := range content.Array() {
			if strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "tool_result") {
				return true
			}
		}
	}
	return false
}

func covsClaudeResponseRequestsToolContinuation(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	if !gjson.ValidBytes(raw) {
		return covsClaudeSSERequestsToolContinuation(raw)
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(raw, "stop_reason").String()), "tool_use") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(raw, "choices.0.finish_reason").String()), "tool_calls") {
		return true
	}
	content := gjson.GetBytes(raw, "content")
	if content.Exists() && content.IsArray() {
		for _, part := range content.Array() {
			if strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "tool_use") {
				return true
			}
		}
	}
	return len(gjson.GetBytes(raw, "choices.0.message.tool_calls").Array()) > 0
}

func covsClaudeSSERequestsToolContinuation(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		payload := jsonPayload(line)
		if len(payload) == 0 || !gjson.ValidBytes(payload) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "delta.stop_reason").String()), "tool_use") {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(payload, "content_block.type").String()), "tool_use") {
			return true
		}
	}
	return false
}

func covsClaudeResponseMissedToolResults(raw []byte) bool {
	text := covsClaudeSurfaceText(raw)
	if strings.TrimSpace(text) == "" {
		return false
	}
	return covsClaudeTextLooksLikeMissingToolResults(text)
}

func covsClaudeSurfaceText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if gjson.ValidBytes(raw) {
		text, _, _ := claudeResponseSurfaceSummary(raw)
		return text
	}
	parts := make([]string, 0, 8)
	appendText := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		payload := jsonPayload(line)
		if len(payload) == 0 || !gjson.ValidBytes(payload) {
			continue
		}
		appendText(gjson.GetBytes(payload, "delta.text").String())
		appendText(gjson.GetBytes(payload, "delta.thinking").String())
		appendText(gjson.GetBytes(payload, "content_block.text").String())
		appendText(gjson.GetBytes(payload, "content_block.thinking").String())
	}
	return strings.Join(parts, "\n")
}

func covsClaudeVisibleText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if gjson.ValidBytes(raw) {
		text, _, _ := claudeResponseSurfaceSummary(raw)
		return text
	}
	parts := make([]string, 0, 8)
	appendText := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		payload := jsonPayload(line)
		if len(payload) == 0 || !gjson.ValidBytes(payload) {
			continue
		}
		appendText(gjson.GetBytes(payload, "delta.text").String())
		appendText(gjson.GetBytes(payload, "content_block.text").String())
	}
	return strings.Join(parts, "\n")
}

func covsClaudeTextLooksLikeMissingToolResults(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	for _, phrase := range []string{
		"haven't executed any tools yet",
		"no tool results to reference",
		"tool results are missing",
		"don't have any tool results",
		"没有已执行的工具结果",
		"没有工具结果可供查询",
		"没有工具结果",
		"工具结果缺失",
		"看不到工具结果",
	} {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	return false
}

func buildCOVSClaudeContinuationPrompt(raw []byte) string {
	if len(raw) == 0 || !gjson.ValidBytes(raw) {
		return ""
	}

	type toolResult struct {
		Name    string
		Content string
	}

	toolNames := make(map[string]string)
	results := make([]toolResult, 0, 4)
	requestText := ""
	messages := gjson.GetBytes(raw, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	for _, message := range messages.Array() {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		content := message.Get("content")
		switch role {
		case "assistant":
			if !content.Exists() || !content.IsArray() {
				continue
			}
			for _, part := range content.Array() {
				if !strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "tool_use") {
					continue
				}
				toolID := strings.TrimSpace(part.Get("id").String())
				toolName := strings.TrimSpace(part.Get("name").String())
				if toolID != "" && toolName != "" {
					toolNames[toolID] = toolName
				}
			}
		case "user":
			if !content.Exists() || !content.IsArray() {
				if text := strings.TrimSpace(codexTextFromContent(content)); text != "" {
					requestText = text
				}
				continue
			}
			var textParts []string
			var sawToolResult bool
			for _, part := range content.Array() {
				partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
				switch partType {
				case "text", "":
					if text := strings.TrimSpace(part.Get("text").String()); text != "" {
						textParts = append(textParts, text)
					}
				case "tool_result":
					sawToolResult = true
					toolID := strings.TrimSpace(part.Get("tool_use_id").String())
					toolName := strings.TrimSpace(toolNames[toolID])
					if toolName == "" {
						toolName = toolID
					}
					if toolName == "" {
						toolName = "tool_result"
					}
					contentText := strings.TrimSpace(claudeToolResultContent(part.Get("content")))
					if contentText == "" {
						contentText = strings.TrimSpace(part.Raw)
					}
					results = append(results, toolResult{Name: toolName, Content: contentText})
				}
			}
			if !sawToolResult && len(textParts) > 0 {
				requestText = strings.Join(textParts, "\n")
			}
		}
	}

	if len(results) == 0 {
		return ""
	}
	if strings.TrimSpace(requestText) == "" {
		requestText = "Continue the task using the completed tool outputs."
	}

	var prompt strings.Builder
	prompt.WriteString("Original user request:\n")
	prompt.WriteString(strings.TrimSpace(requestText))
	prompt.WriteString("\n\nCompleted tool outputs:\n")
	for _, result := range results {
		prompt.WriteString("[")
		prompt.WriteString(strings.TrimSpace(result.Name))
		prompt.WriteString("]\n")
		prompt.WriteString(strings.TrimSpace(result.Content))
		prompt.WriteString("\n\n")
	}
	prompt.WriteString("Now answer the original user request directly based only on the completed tool outputs above. Do not call any tools. Do not say the tool results are missing.")
	return prompt.String()
}

func claudeToolResultContent(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	switch content.Type {
	case gjson.String:
		return content.String()
	case gjson.JSON:
		if content.IsArray() {
			if text := strings.TrimSpace(codexTextFromContent(content)); text != "" {
				return text
			}
		}
		return content.Raw
	default:
		return content.Raw
	}
}

func ensureClaudeMaxTokensAtLeast(body []byte, minimum int64) []byte {
	if len(body) == 0 || minimum <= 0 {
		return body
	}
	if current := gjson.GetBytes(body, "max_tokens"); current.Exists() && current.Int() >= minimum {
		return body
	}
	out, err := sjson.SetBytes(body, "max_tokens", minimum)
	if err != nil {
		return body
	}
	return out
}

func (n *claudeStreamNormalizer) remapIndex(index int) int {
	if n == nil || len(n.suppressed) == 0 {
		return index
	}
	shift := 0
	for suppressedIndex := range n.suppressed {
		if suppressedIndex < index {
			shift++
		}
	}
	return index - shift
}

func applyClaudeHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool, extraBetas []string, nativePassthrough bool) {
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	if shouldUseClaudeAPIKeyAuth(r, attrs) {
		r.Header.Del("Authorization")
		r.Header.Set("x-api-key", apiKey)
	} else {
		r.Header.Del("x-api-key")
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}
	r.Header.Set("Content-Type", "application/json")

	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	if nativePassthrough {
		copyClaudePassthroughHeaders(r.Header, ginHeaders)
		if len(extraBetas) > 0 {
			existing := strings.TrimSpace(r.Header.Get("Anthropic-Beta"))
			if existing == "" {
				r.Header.Set("Anthropic-Beta", strings.Join(extraBetas, ","))
			} else {
				existingSet := make(map[string]bool)
				for _, beta := range strings.Split(existing, ",") {
					existingSet[strings.TrimSpace(beta)] = true
				}
				for _, beta := range extraBetas {
					beta = strings.TrimSpace(beta)
					if beta != "" && !existingSet[beta] {
						existing += "," + beta
						existingSet[beta] = true
					}
				}
				r.Header.Set("Anthropic-Beta", existing)
			}
		}
	} else {
		misc.EnsureHeader(r.Header, ginHeaders, "Anthropic-Version", "2023-06-01")
	}

	if !nativePassthrough && shouldUseClaudeCodeCompatibilityProfile(r.URL.String()) {
		promptCachingBeta := "prompt-caching-2024-07-31"
		baseBetas := "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14," + promptCachingBeta
		if val := strings.TrimSpace(ginHeaders.Get("Anthropic-Beta")); val != "" {
			baseBetas = val
			if !strings.Contains(val, "oauth") {
				baseBetas += ",oauth-2025-04-20"
			}
		}
		if !strings.Contains(baseBetas, promptCachingBeta) {
			baseBetas += "," + promptCachingBeta
		}

		// Merge extra betas from request body
		if len(extraBetas) > 0 {
			existingSet := make(map[string]bool)
			for _, b := range strings.Split(baseBetas, ",") {
				existingSet[strings.TrimSpace(b)] = true
			}
			for _, beta := range extraBetas {
				beta = strings.TrimSpace(beta)
				if beta != "" && !existingSet[beta] {
					baseBetas += "," + beta
					existingSet[beta] = true
				}
			}
		}
		r.Header.Set("Anthropic-Beta", baseBetas)
		misc.EnsureHeader(r.Header, ginHeaders, "Anthropic-Dangerous-Direct-Browser-Access", "true")
		misc.EnsureHeader(r.Header, ginHeaders, "X-App", "cli")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Helper-Method", "stream")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Retry-Count", "0")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Runtime-Version", "v24.3.0")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Package-Version", "0.55.1")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Runtime", "node")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Lang", "js")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Arch", "arm64")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Os", "MacOS")
		misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Timeout", "60")
		misc.EnsureHeader(r.Header, ginHeaders, "User-Agent", "claude-cli/1.0.83 (external, cli)")
		r.Header.Set("Connection", "keep-alive")
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	} else if !nativePassthrough {
		r.Header.Del("Anthropic-Beta")
		r.Header.Del("Anthropic-Dangerous-Direct-Browser-Access")
		r.Header.Del("X-App")
		r.Header.Del("X-Stainless-Helper-Method")
		r.Header.Del("X-Stainless-Retry-Count")
		r.Header.Del("X-Stainless-Runtime-Version")
		r.Header.Del("X-Stainless-Package-Version")
		r.Header.Del("X-Stainless-Runtime")
		r.Header.Del("X-Stainless-Lang")
		r.Header.Del("X-Stainless-Arch")
		r.Header.Del("X-Stainless-Os")
		r.Header.Del("X-Stainless-Timeout")
		r.Header.Del("Connection")
		r.Header.Del("Accept-Encoding")
	}
	if stream {
		if nativePassthrough {
			if strings.TrimSpace(r.Header.Get("Accept")) == "" {
				r.Header.Set("Accept", "text/event-stream")
			}
		} else {
			r.Header.Set("Accept", "text/event-stream")
		}
	} else {
		if nativePassthrough {
			if strings.TrimSpace(r.Header.Get("Accept")) == "" {
				r.Header.Set("Accept", "application/json")
			}
		} else {
			r.Header.Set("Accept", "application/json")
		}
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	util.ApplyReservedProviderHeadersFromAttrs(r, attrs)
}

func copyClaudePassthroughHeaders(dst, src http.Header) {
	if dst == nil || src == nil {
		return
	}
	for key, values := range src {
		if !shouldForwardClaudePassthroughHeader(key) {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldForwardClaudePassthroughHeader(key string) bool {
	normalized := strings.TrimSpace(http.CanonicalHeaderKey(key))
	if normalized == "" {
		return false
	}
	switch {
	case strings.EqualFold(normalized, "Accept"):
		return true
	case strings.EqualFold(normalized, "Accept-Encoding"):
		return true
	case strings.EqualFold(normalized, "Content-Type"):
		return true
	case strings.EqualFold(normalized, "User-Agent"):
		return true
	case strings.EqualFold(normalized, "X-App"):
		return true
	case strings.HasPrefix(strings.ToLower(normalized), "anthropic-"):
		return true
	case strings.HasPrefix(strings.ToLower(normalized), "x-stainless-"):
		return true
	default:
		return false
	}
}

func shouldUseClaudeOpenAICompatProfile(auth *cliproxyauth.Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	if util.HeaderValueFromAttrsCI(
		auth.Attributes,
		"X-NewAPI-Username",
		"X-New-API-Username",
		"X-NewAPI-Access-Token",
		"X-New-API-Access-Token",
	) != "" {
		return true
	}
	baseURL := strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	return strings.Contains(baseURL, "llm.whitedream.top")
}

func shouldPassThroughClaudePayload(from, to sdktranslator.Format) bool {
	return from == to
}

func shouldUseRawClaudeUpstreamPayload(baseURL string, nativePassthrough bool, from, to sdktranslator.Format) bool {
	if !shouldPassThroughClaudePayload(from, to) {
		return false
	}
	if nativePassthrough {
		return true
	}
	return !shouldUseClaudeThirdPartyRelayProfile(baseURL)
}

func shouldBufferCOVSClaudeStream(auth *cliproxyauth.Auth, nativePassthrough bool, from, to sdktranslator.Format, requestBody []byte) bool {
	if !isCOVSClaudeAuth(auth) {
		return false
	}
	if claudeRequestHasToolResults(requestBody) {
		return true
	}
	if nativePassthrough {
		return false
	}
	// Buffered replay keeps the legacy empty/truncated-response fallback for Claude-native
	// traffic, but translated streams must stay incremental for Claude CLI tool workflows.
	return from == to
}

func shouldUseClaudeNativePassthrough(auth *cliproxyauth.Auth, from, to sdktranslator.Format) bool {
	if !shouldPassThroughClaudePayload(from, to) || auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	if isYunyiClaudeRelayAuth(auth) {
		return true
	}
	val := strings.TrimSpace(auth.Attributes["native_passthrough"])
	switch {
	case strings.EqualFold(val, "1"),
		strings.EqualFold(val, "true"),
		strings.EqualFold(val, "yes"),
		strings.EqualFold(val, "on"):
		return true
	default:
		return false
	}
}

func isYunyiClaudeRelayAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Prefix), "yunyi-claude") {
		return true
	}
	if len(auth.Attributes) == 0 {
		return false
	}
	return isYunyiClaudeRelayBaseURL(auth.Attributes["base_url"])
}

func isYunyiClaudeRelayBaseURL(baseURL string) bool {
	normalized := strings.ToLower(strings.TrimSpace(baseURL))
	if normalized == "" {
		return false
	}
	return strings.Contains(normalized, "cdn1.yunyi.cfd/claude")
}

func shouldUseClaudeAPIKeyAuth(r *http.Request, attrs map[string]string) bool {
	if r == nil || len(attrs) == 0 || strings.TrimSpace(attrs["api_key"]) == "" {
		return false
	}
	if r.URL != nil && strings.EqualFold(r.URL.Scheme, "https") && strings.EqualFold(r.URL.Hostname(), "api.anthropic.com") {
		return true
	}
	return hasConfiguredHeader(attrs, "x-api-key")
}

func hasConfiguredHeader(attrs map[string]string, headerName string) bool {
	target := strings.ToLower(strings.TrimSpace(headerName))
	if target == "" {
		return false
	}
	for key, value := range attrs {
		if !strings.HasPrefix(key, "header:") || strings.TrimSpace(value) == "" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(key, "header:")))
		if name == target {
			return true
		}
	}
	return false
}

func claudeCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func checkSystemInstructions(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	claudeCodeInstructions := `[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]`
	if system.IsArray() {
		if gjson.GetBytes(payload, "system.0.text").String() != "You are Claude Code, Anthropic's official CLI for Claude." {
			system.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					claudeCodeInstructions, _ = sjson.SetRaw(claudeCodeInstructions, "-1", part.Raw)
				}
				return true
			})
			payload, _ = sjson.SetRawBytes(payload, "system", []byte(claudeCodeInstructions))
		}
	} else {
		payload, _ = sjson.SetRawBytes(payload, "system", []byte(claudeCodeInstructions))
	}
	return payload
}

func isClaudeOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

func applyClaudeToolPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(index, tool gjson.Result) bool {
			// Skip built-in tools (web_search, code_execution, etc.) which have
			// a "type" field and require their name to remain unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				return true
			}
			name := tool.Get("name").String()
			if name == "" || strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("tools.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, prefix+name)
			return true
		})
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				if part.Get("type").String() != "tool_use" {
					return true
				}
				name := part.Get("name").String()
				if name == "" || strings.HasPrefix(name, prefix) {
					return true
				}
				path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
				body, _ = sjson.SetBytes(body, path, prefix+name)
				return true
			})
			return true
		})
	}

	return body
}

func stripClaudeToolPrefixFromResponse(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		if part.Get("type").String() != "tool_use" {
			return true
		}
		name := part.Get("name").String()
		if !strings.HasPrefix(name, prefix) {
			return true
		}
		path := fmt.Sprintf("content.%d.name", index.Int())
		body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(name, prefix))
		return true
	})
	return body
}

func stripClaudeToolPrefixFromStreamLine(line []byte, prefix string) []byte {
	if prefix == "" {
		return line
	}
	payload := jsonPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() || contentBlock.Get("type").String() != "tool_use" {
		return line
	}
	name := contentBlock.Get("name").String()
	if !strings.HasPrefix(name, prefix) {
		return line
	}
	updated, err := sjson.SetBytes(payload, "content_block.name", strings.TrimPrefix(name, prefix))
	if err != nil {
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

func normalizeClaudeFinalResponse(raw []byte, requestedModel, sourceFormat string) []byte {
	if len(raw) == 0 || !gjson.ValidBytes(raw) {
		return raw
	}
	displayModel := claudeDisplayModel(requestedModel)
	if displayModel == "" {
		return raw
	}

	out := append([]byte(nil), raw...)
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "openai":
		out, _ = sjson.SetBytes(out, "model", displayModel)
		if !claudeRequestedModelAllowsThinking(requestedModel) {
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
		if !claudeRequestedModelAllowsThinking(requestedModel) {
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

func claudeDisplayModel(requestedModel string) string {
	trimmed := strings.TrimSpace(thinking.ParseSuffix(requestedModel).ModelName)
	if trimmed == "" {
		return ""
	}
	if slash := strings.LastIndex(trimmed, "/"); slash >= 0 && slash < len(trimmed)-1 {
		return strings.TrimSpace(trimmed[slash+1:])
	}
	return trimmed
}

func claudeRequestedModelAllowsThinking(modelName string) bool {
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

// getClientUserAgent extracts the client User-Agent from the gin context.
func getClientUserAgent(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.GetHeader("User-Agent")
	}
	return ""
}

func getClientXApp(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.GetHeader("X-App")
	}
	return ""
}

func isExternalClaudeCLICompatRequest(ctx context.Context) bool {
	userAgent := strings.ToLower(strings.TrimSpace(getClientUserAgent(ctx)))
	if !strings.HasPrefix(userAgent, "claude-cli/") {
		return false
	}
	xApp := strings.ToLower(strings.TrimSpace(getClientXApp(ctx)))
	return strings.Contains(userAgent, "external, cli") || xApp == "cli" || xApp == "external"
}

func shouldPreserveClaudeCodePromptForExternalCLICompat(ctx context.Context, from, to sdktranslator.Format) bool {
	return isExternalClaudeCLICompatRequest(ctx)
}

// getCloakConfigFromAuth extracts cloak configuration from auth attributes.
// Returns (cloakMode, strictMode, sensitiveWords).
func getCloakConfigFromAuth(auth *cliproxyauth.Auth) (string, bool, []string) {
	if auth == nil || auth.Attributes == nil {
		return "auto", false, nil
	}

	cloakMode := auth.Attributes["cloak_mode"]
	if cloakMode == "" {
		cloakMode = "auto"
	}

	strictMode := strings.ToLower(auth.Attributes["cloak_strict_mode"]) == "true"

	var sensitiveWords []string
	if wordsStr := auth.Attributes["cloak_sensitive_words"]; wordsStr != "" {
		sensitiveWords = strings.Split(wordsStr, ",")
		for i := range sensitiveWords {
			sensitiveWords[i] = strings.TrimSpace(sensitiveWords[i])
		}
	}

	return cloakMode, strictMode, sensitiveWords
}

// resolveClaudeKeyCloakConfig finds the matching ClaudeKey config and returns its CloakConfig.
func resolveClaudeKeyCloakConfig(cfg *config.Config, auth *cliproxyauth.Auth) *config.CloakConfig {
	if cfg == nil || auth == nil {
		return nil
	}

	apiKey, baseURL := claudeCreds(auth)
	if apiKey == "" {
		return nil
	}

	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)

		// Match by API key
		if strings.EqualFold(cfgKey, apiKey) {
			// If baseURL is specified, also check it
			if baseURL != "" && cfgBase != "" && !strings.EqualFold(cfgBase, baseURL) {
				continue
			}
			return entry.Cloak
		}
	}

	return nil
}

// injectFakeUserID generates and injects a fake user ID into the request metadata.
func injectFakeUserID(payload []byte) []byte {
	metadata := gjson.GetBytes(payload, "metadata")
	if !metadata.Exists() {
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", generateFakeUserID())
		return payload
	}

	existingUserID := gjson.GetBytes(payload, "metadata.user_id").String()
	if existingUserID == "" || !isValidUserID(existingUserID) {
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", generateFakeUserID())
	}
	return payload
}

// checkSystemInstructionsWithMode injects Claude Code system prompt.
// In strict mode, it replaces all user system messages.
// In non-strict mode (default), it prepends to existing system messages.
func checkSystemInstructionsWithMode(payload []byte, strictMode bool) []byte {
	system := gjson.GetBytes(payload, "system")
	claudeCodeInstructions := `[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]`

	if strictMode {
		// Strict mode: replace all system messages with Claude Code prompt only
		payload, _ = sjson.SetRawBytes(payload, "system", []byte(claudeCodeInstructions))
		return payload
	}

	// Non-strict mode (default): prepend Claude Code prompt to existing system messages
	if system.IsArray() {
		if gjson.GetBytes(payload, "system.0.text").String() != "You are Claude Code, Anthropic's official CLI for Claude." {
			system.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					claudeCodeInstructions, _ = sjson.SetRaw(claudeCodeInstructions, "-1", part.Raw)
				}
				return true
			})
			payload, _ = sjson.SetRawBytes(payload, "system", []byte(claudeCodeInstructions))
		}
	} else {
		payload, _ = sjson.SetRawBytes(payload, "system", []byte(claudeCodeInstructions))
	}
	return payload
}

// applyCloaking applies cloaking transformations to the payload based on config and client.
// Cloaking includes: system prompt injection, fake user ID, and sensitive word obfuscation.
func applyCloaking(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, payload []byte, model string) []byte {
	clientUserAgent := getClientUserAgent(ctx)

	// Get cloak config from ClaudeKey configuration
	cloakCfg := resolveClaudeKeyCloakConfig(cfg, auth)

	// Determine cloak settings
	var cloakMode string
	var strictMode bool
	var sensitiveWords []string

	if cloakCfg != nil {
		cloakMode = cloakCfg.Mode
		strictMode = cloakCfg.StrictMode
		sensitiveWords = cloakCfg.SensitiveWords
	}

	// Fallback to auth attributes if no config found
	if cloakMode == "" {
		attrMode, attrStrict, attrWords := getCloakConfigFromAuth(auth)
		cloakMode = attrMode
		if !strictMode {
			strictMode = attrStrict
		}
		if len(sensitiveWords) == 0 {
			sensitiveWords = attrWords
		}
	}

	// Determine if cloaking should be applied
	if !shouldCloak(cloakMode, clientUserAgent) {
		return payload
	}

	// Skip system instructions for claude-3-5-haiku models
	if !strings.HasPrefix(model, "claude-3-5-haiku") {
		payload = checkSystemInstructionsWithMode(payload, strictMode)
	}

	// Inject fake user ID
	payload = injectFakeUserID(payload)

	// Apply sensitive word obfuscation
	if len(sensitiveWords) > 0 {
		matcher := buildSensitiveWordMatcher(sensitiveWords)
		payload = obfuscateSensitiveWords(payload, matcher)
	}

	return payload
}

// ensureCacheControl injects cache_control breakpoints into the payload for optimal prompt caching.
// According to Anthropic's documentation, cache prefixes are created in order: tools -> system -> messages.
// This function adds cache_control to:
// 1. The LAST tool in the tools array (caches all tool definitions)
// 2. The LAST element in the system array (caches system prompt)
// 3. The SECOND-TO-LAST user turn (caches conversation history for multi-turn)
//
// Up to 4 cache breakpoints are allowed per request. Tools, System, and Messages are INDEPENDENT breakpoints.
// This enables up to 90% cost reduction on cached tokens (cache read = 0.1x base price).
// See: https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
func ensureCacheControl(payload []byte) []byte {
	// 1. Inject cache_control into the LAST tool (caches all tool definitions)
	// Tools are cached first in the hierarchy, so this is the most important breakpoint.
	payload = injectToolsCacheControl(payload)

	// 2. Inject cache_control into the LAST system prompt element
	// System is the second level in the cache hierarchy.
	payload = injectSystemCacheControl(payload)

	// 3. Inject cache_control into messages for multi-turn conversation caching
	// This caches the conversation history up to the second-to-last user turn.
	payload = injectMessagesCacheControl(payload)

	return payload
}

func minimizeClaudeThirdPartyRelayPayload(payload []byte) []byte {
	return minimizeClaudeThirdPartyRelayPayloadWithOptions(payload, false)
}

func minimizeClaudeThirdPartyRelayPayloadWithOptions(payload []byte, preserveClaudeCodePrompt bool) []byte {
	payload = hoistClaudeSystemMessages(payload, preserveClaudeCodePrompt)
	if !preserveClaudeCodePrompt {
		payload = stripClaudeCodeSystemPrompt(payload)
	}
	payload = inlineClaudeSystemIntoMessages(payload)
	payload = compactClaudeThirdPartyRelayMessageText(payload)
	payload = compactClaudeThirdPartyRelayTools(payload)
	payload = stripClaudeMetadataUserID(payload)
	payload = stripClaudeCacheControl(payload)
	payload = clampClaudeThirdPartyRelayMaxTokens(payload)
	return payload
}

func hoistClaudeSystemMessages(payload []byte, preserveClaudeCodePrompt bool) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	systemTexts := collectClaudeRelaySystemTexts(gjson.GetBytes(payload, "system"))
	messages := gjson.GetBytes(payload, "messages")
	if messages.Exists() && messages.IsArray() {
		filtered := "[]"
		changed := false
		for _, message := range messages.Array() {
			role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
			if role == "system" {
				systemTexts = append(systemTexts, collectClaudeRelaySystemTexts(message.Get("content"))...)
				changed = true
				continue
			}
			filtered, _ = sjson.SetRaw(filtered, "-1", message.Raw)
		}
		if changed {
			payload, _ = sjson.SetRawBytes(payload, "messages", []byte(filtered))
		}
	}

	systemTexts = compactClaudeRelaySystemTexts(systemTexts, preserveClaudeCodePrompt)
	if len(systemTexts) == 0 {
		payload, _ = sjson.DeleteBytes(payload, "system")
		return payload
	}

	systemParts := make([]map[string]any, 0, len(systemTexts))
	for _, text := range systemTexts {
		systemParts = append(systemParts, map[string]any{
			"type": "text",
			"text": text,
		})
	}
	payload, _ = sjson.SetBytes(payload, "system", systemParts)
	return payload
}

func collectClaudeRelaySystemTexts(content gjson.Result) []string {
	switch {
	case !content.Exists():
		return nil
	case content.Type == gjson.String:
		text := strings.TrimSpace(content.String())
		if text == "" {
			return nil
		}
		return []string{text}
	case content.IsArray():
		out := make([]string, 0, len(content.Array()))
		for _, item := range content.Array() {
			text := strings.TrimSpace(item.Get("text").String())
			if text == "" {
				continue
			}
			out = append(out, text)
		}
		return out
	default:
		text := strings.TrimSpace(content.Get("text").String())
		if text == "" {
			return nil
		}
		return []string{text}
	}
}

func compactClaudeRelaySystemTexts(texts []string, preserveClaudeCodePrompt bool) []string {
	if preserveClaudeCodePrompt {
		return []string{"You are Claude Code, Anthropic's official CLI for Claude."}
	}

	out := make([]string, 0, len(texts))
	seen := make(map[string]struct{}, len(texts))
	for _, text := range texts {
		text = strings.TrimSpace(text)
		if text == "" || isCOVSClaudeCodeBootstrapText(text) {
			continue
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
	}
	return out
}

func inlineClaudeSystemIntoMessages(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	systemText := collectClaudeSystemText(system)
	payload, _ = sjson.DeleteBytes(payload, "system")
	if strings.TrimSpace(systemText) == "" {
		return payload
	}

	prefixText := claudeThirdPartyRelaySystemPrefix + "\n\n" + systemText
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() || len(messages.Array()) == 0 {
		out, err := sjson.SetBytes(payload, "messages", []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type": "text",
				"text": prefixText,
			}},
		}})
		if err == nil {
			return out
		}
		return payload
	}

	firstMessage := messages.Array()[0]
	if !strings.EqualFold(strings.TrimSpace(firstMessage.Get("role").String()), "user") {
		prefixedMessage, err := json.Marshal(map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type": "text",
				"text": prefixText,
			}},
		})
		if err != nil {
			return payload
		}

		rawMessages := make([]string, 0, len(messages.Array())+1)
		rawMessages = append(rawMessages, string(prefixedMessage))
		for _, message := range messages.Array() {
			rawMessages = append(rawMessages, message.Raw)
		}
		out, err := sjson.SetRawBytes(payload, "messages", []byte("["+strings.Join(rawMessages, ",")+"]"))
		if err == nil {
			return out
		}
		return payload
	}

	content := firstMessage.Get("content")
	switch {
	case content.IsArray():
		prefixBlock, err := json.Marshal(map[string]any{
			"type": "text",
			"text": prefixText,
		})
		if err != nil {
			return payload
		}
		blocks := make([]string, 0, len(content.Array())+1)
		blocks = append(blocks, string(prefixBlock))
		for _, block := range content.Array() {
			blocks = append(blocks, block.Raw)
		}
		out, err := sjson.SetRawBytes(payload, "messages.0.content", []byte("["+strings.Join(blocks, ",")+"]"))
		if err == nil {
			return out
		}
	case content.Type == gjson.String:
		out, err := sjson.SetBytes(payload, "messages.0.content", prefixText+"\n\n"+content.String())
		if err == nil {
			return out
		}
	default:
		out, err := sjson.SetBytes(payload, "messages.0.content", prefixText)
		if err == nil {
			return out
		}
	}

	return payload
}

func collectClaudeSystemText(system gjson.Result) string {
	switch {
	case !system.Exists():
		return ""
	case system.Type == gjson.String:
		return strings.TrimSpace(system.String())
	case system.IsArray():
		parts := make([]string, 0, len(system.Array()))
		for _, item := range system.Array() {
			text := strings.TrimSpace(item.Get("text").String())
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		return strings.TrimSpace(strings.Join(parts, "\n\n"))
	default:
		return strings.TrimSpace(system.Get("text").String())
	}
}

func clampClaudeThirdPartyRelayMaxTokens(payload []byte) []byte {
	maxTokens := gjson.GetBytes(payload, "max_tokens")
	if !maxTokens.Exists() {
		return payload
	}

	limit := int64(0)
	if hasClaudeToolsOrToolChoice(payload) {
		if len(payload) >= claudeThirdPartyRelayLargeToolPayloadBytes && maxTokens.Int() > claudeThirdPartyRelayLargeToolPayloadMaxTokens {
			limit = claudeThirdPartyRelayLargeToolPayloadMaxTokens
		}
	} else if len(payload) >= claudeThirdPartyRelayLargePromptBytes && maxTokens.Int() > claudeThirdPartyRelayLargePromptMaxTokens {
		limit = claudeThirdPartyRelayLargePromptMaxTokens
	}
	if limit <= 0 {
		return payload
	}

	out, err := sjson.SetBytes(payload, "max_tokens", limit)
	if err == nil {
		return out
	}
	return payload
}

func hasClaudeToolsOrToolChoice(payload []byte) bool {
	tools := gjson.GetBytes(payload, "tools")
	if tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		return true
	}
	return gjson.GetBytes(payload, "tool_choice").Exists()
}

func stripClaudeCodeSystemPrompt(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	const claudeCodePrompt = "You are Claude Code, Anthropic's official CLI for Claude."

	if system.IsArray() {
		parts := system.Array()
		kept := make([]string, 0, len(parts))
		for _, part := range parts {
			if part.Get("type").String() == "text" && strings.TrimSpace(part.Get("text").String()) == claudeCodePrompt {
				continue
			}
			kept = append(kept, part.Raw)
		}
		if len(kept) == len(parts) {
			return payload
		}
		if len(kept) == 0 {
			payload, _ = sjson.DeleteBytes(payload, "system")
			return payload
		}
		payload, _ = sjson.SetRawBytes(payload, "system", []byte("["+strings.Join(kept, ",")+"]"))
		return payload
	}

	if system.Type == gjson.String && strings.TrimSpace(system.String()) == claudeCodePrompt {
		payload, _ = sjson.DeleteBytes(payload, "system")
	}
	return payload
}

func stripClaudeMetadataUserID(payload []byte) []byte {
	metadata := gjson.GetBytes(payload, "metadata")
	if !metadata.Exists() {
		return payload
	}

	payload, _ = sjson.DeleteBytes(payload, "metadata.user_id")
	metadata = gjson.GetBytes(payload, "metadata")
	if metadata.Exists() && metadata.IsObject() && len(metadata.Map()) == 0 {
		payload, _ = sjson.DeleteBytes(payload, "metadata")
	}
	return payload
}

func stripClaudeCacheControl(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		for idx := range system.Array() {
			payload, _ = sjson.DeleteBytes(payload, fmt.Sprintf("system.%d.cache_control", idx))
		}
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		for idx := range tools.Array() {
			payload, _ = sjson.DeleteBytes(payload, fmt.Sprintf("tools.%d.cache_control", idx))
		}
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		for msgIdx, msg := range messages.Array() {
			content := msg.Get("content")
			if !content.IsArray() {
				continue
			}
			for contentIdx := range content.Array() {
				payload, _ = sjson.DeleteBytes(payload, fmt.Sprintf("messages.%d.content.%d.cache_control", msgIdx, contentIdx))
			}
		}
	}

	return payload
}

func compactClaudeThirdPartyRelayMessageText(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	for msgIdx, message := range messages.Array() {
		content := message.Get("content")
		switch {
		case content.IsArray():
			for contentIdx, part := range content.Array() {
				if !strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "text") {
					continue
				}
				text := compactClaudeThirdPartyRelayText(part.Get("text").String())
				path := fmt.Sprintf("messages.%d.content.%d.text", msgIdx, contentIdx)
				payload, _ = sjson.SetBytes(payload, path, text)
			}
		case content.Type == gjson.String:
			text := compactClaudeThirdPartyRelayText(content.String())
			path := fmt.Sprintf("messages.%d.content", msgIdx)
			payload, _ = sjson.SetBytes(payload, path, text)
		}
	}

	return payload
}

func stripClaudeThirdPartyRelayInlinedSystemPrefix(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	for msgIdx, message := range messages.Array() {
		content := message.Get("content")
		switch {
		case content.IsArray():
			for contentIdx, part := range content.Array() {
				if !strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "text") {
					continue
				}
				text := stripClaudeThirdPartyRelaySystemPrefixText(part.Get("text").String())
				path := fmt.Sprintf("messages.%d.content.%d.text", msgIdx, contentIdx)
				payload, _ = sjson.SetBytes(payload, path, text)
			}
		case content.Type == gjson.String:
			text := stripClaudeThirdPartyRelaySystemPrefixText(content.String())
			path := fmt.Sprintf("messages.%d.content", msgIdx)
			payload, _ = sjson.SetBytes(payload, path, text)
		}
	}

	return payload
}

func stripClaudeThirdPartyRelaySystemPrefixText(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, claudeThirdPartyRelaySystemPrefix) {
		return text
	}
	return ""
}

func compactClaudeThirdPartyRelayText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}

	if strings.Contains(trimmed, "x-anthropic-billing-header:") {
		if idx := strings.Index(trimmed, "\n"); idx >= 0 && idx < len(trimmed)-1 {
			trimmed = strings.TrimSpace(trimmed[idx+1:])
		} else {
			trimmed = strings.TrimSpace(strings.ReplaceAll(trimmed, "x-anthropic-billing-header:", ""))
		}
	}
	trimmed = claudeRelaySystemReminderPattern.ReplaceAllString(trimmed, "")
	trimmed = claudeRelayBlankLinePattern.ReplaceAllString(trimmed, "\n\n")
	return strings.TrimSpace(trimmed)
}

func compactClaudeThirdPartyRelayTools(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}

	for toolIdx, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) != "" {
			continue
		}
		path := fmt.Sprintf("tools.%d", toolIdx)
		if description := strings.TrimSpace(tool.Get("description").String()); description != "" {
			payload, _ = sjson.SetBytes(payload, path+".description", compactClaudeRelayToolDescription(description))
		}
		if !tool.Get("input_schema").Exists() {
			continue
		}
		var schema any
		if err := json.Unmarshal([]byte(tool.Get("input_schema").Raw), &schema); err != nil {
			continue
		}
		schema = sanitizeClaudeRelayToolSchema(schema)
		payload, _ = sjson.SetBytes(payload, path+".input_schema", schema)
	}

	return payload
}

func compactClaudeRelayToolDescription(description string) string {
	description = strings.Join(strings.Fields(strings.TrimSpace(description)), " ")
	const maxLen = 160
	if len(description) <= maxLen {
		return description
	}
	return strings.TrimSpace(description[:maxLen])
}

func sanitizeClaudeRelayToolSchema(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, raw := range value {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "description", "title", "default", "examples", "$comment":
				continue
			}
			out[key] = sanitizeClaudeRelayToolSchema(raw)
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, sanitizeClaudeRelayToolSchema(item))
		}
		return out
	default:
		return value
	}
}

func countCacheControls(payload []byte) int {
	count := 0

	// Check system
	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	// Check tools
	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				count++
			}
			return true
		})
	}

	// Check messages
	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					if item.Get("cache_control").Exists() {
						count++
					}
					return true
				})
			}
			return true
		})
	}

	return count
}

// injectMessagesCacheControl adds cache_control to the second-to-last user turn for multi-turn caching.
// Per Anthropic docs: "Place cache_control on the second-to-last User message to let the model reuse the earlier cache."
// This enables caching of conversation history, which is especially beneficial for long multi-turn conversations.
// Only adds cache_control if:
// - There are at least 2 user turns in the conversation
// - No message content already has cache_control
func injectMessagesCacheControl(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	// Check if ANY message content already has cache_control
	hasCacheControlInMessages := false
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if item.Get("cache_control").Exists() {
					hasCacheControlInMessages = true
					return false
				}
				return true
			})
		}
		return !hasCacheControlInMessages
	})
	if hasCacheControlInMessages {
		return payload
	}

	// Find all user message indices
	var userMsgIndices []int
	messages.ForEach(func(index gjson.Result, msg gjson.Result) bool {
		if msg.Get("role").String() == "user" {
			userMsgIndices = append(userMsgIndices, int(index.Int()))
		}
		return true
	})

	// Need at least 2 user turns to cache the second-to-last
	if len(userMsgIndices) < 2 {
		return payload
	}

	// Get the second-to-last user message index
	secondToLastUserIdx := userMsgIndices[len(userMsgIndices)-2]

	// Get the content of this message
	contentPath := fmt.Sprintf("messages.%d.content", secondToLastUserIdx)
	content := gjson.GetBytes(payload, contentPath)

	if content.IsArray() {
		// Add cache_control to the last content block of this message
		contentCount := int(content.Get("#").Int())
		if contentCount > 0 {
			cacheControlPath := fmt.Sprintf("messages.%d.content.%d.cache_control", secondToLastUserIdx, contentCount-1)
			result, err := sjson.SetBytes(payload, cacheControlPath, map[string]string{"type": "ephemeral"})
			if err != nil {
				log.Warnf("failed to inject cache_control into messages: %v", err)
				return payload
			}
			payload = result
		}
	} else if content.Type == gjson.String {
		// Convert string content to array with cache_control
		text := content.String()
		newContent := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, contentPath, newContent)
		if err != nil {
			log.Warnf("failed to inject cache_control into message string content: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}

// injectToolsCacheControl adds cache_control to the last tool in the tools array.
// Per Anthropic docs: "The cache_control parameter on the last tool definition caches all tool definitions."
// This only adds cache_control if NO tool in the array already has it.
func injectToolsCacheControl(payload []byte) []byte {
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return payload
	}

	toolCount := int(tools.Get("#").Int())
	if toolCount == 0 {
		return payload
	}

	// Check if ANY tool already has cache_control - if so, don't modify tools
	hasCacheControlInTools := false
	tools.ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("cache_control").Exists() {
			hasCacheControlInTools = true
			return false
		}
		return true
	})
	if hasCacheControlInTools {
		return payload
	}

	// Add cache_control to the last tool
	lastToolPath := fmt.Sprintf("tools.%d.cache_control", toolCount-1)
	result, err := sjson.SetBytes(payload, lastToolPath, map[string]string{"type": "ephemeral"})
	if err != nil {
		log.Warnf("failed to inject cache_control into tools array: %v", err)
		return payload
	}

	return result
}

// injectSystemCacheControl adds cache_control to the last element in the system prompt.
// Converts string system prompts to array format if needed.
// This only adds cache_control if NO system element already has it.
func injectSystemCacheControl(payload []byte) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		count := int(system.Get("#").Int())
		if count == 0 {
			return payload
		}

		// Check if ANY system element already has cache_control
		hasCacheControlInSystem := false
		system.ForEach(func(_, item gjson.Result) bool {
			if item.Get("cache_control").Exists() {
				hasCacheControlInSystem = true
				return false
			}
			return true
		})
		if hasCacheControlInSystem {
			return payload
		}

		// Add cache_control to the last system element
		lastSystemPath := fmt.Sprintf("system.%d.cache_control", count-1)
		result, err := sjson.SetBytes(payload, lastSystemPath, map[string]string{"type": "ephemeral"})
		if err != nil {
			log.Warnf("failed to inject cache_control into system array: %v", err)
			return payload
		}
		payload = result
	} else if system.Type == gjson.String {
		// Convert string system prompt to array with cache_control
		// "system": "text" -> "system": [{"type": "text", "text": "text", "cache_control": {"type": "ephemeral"}}]
		text := system.String()
		newSystem := []map[string]interface{}{
			{
				"type": "text",
				"text": text,
				"cache_control": map[string]string{
					"type": "ephemeral",
				},
			},
		}
		result, err := sjson.SetBytes(payload, "system", newSystem)
		if err != nil {
			log.Warnf("failed to inject cache_control into system string: %v", err)
			return payload
		}
		payload = result
	}

	return payload
}
