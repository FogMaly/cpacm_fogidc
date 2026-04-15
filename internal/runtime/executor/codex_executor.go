package executor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	codexClientVersion = "0.101.0"
	codexUserAgent     = "codex_cli_rs/0.101.0 (Mac OS 26.0.1; arm64) Apple_Terminal/464"

	codexShortContinuationMaxOutputTokens = 32
	codexShortContinuationMaxRunes        = 120
)

var dataTag = []byte("data:")

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg *config.Config
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func (e *CodexExecutor) Identifier() string { return "codex" }

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	return e.doRequest(ctx, auth, httpReq)
}

func (e *CodexExecutor) doRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	return doRequestWithTimeoutRetry(ctx, e.cfg, auth, req)
}

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		apiKey, baseURL := codexCreds(auth)
		if baseURL == "" {
			baseURL = "https://chatgpt.com/backend-api/codex"
		}
		if e.codexShouldUseClaudeMessagesBridge(auth, baseURL) {
			return e.executeViaClaudeMessages(ctx, auth, req, opts, apiKey, baseURL)
		}
		if e.codexShouldUseChatCompletionsBridgeForRequest(auth, baseURL, opts) {
			return e.executeViaChatCompletions(ctx, auth, req, opts, apiKey, baseURL)
		}
		return e.executeCompact(ctx, auth, req, opts)
	}
	baseModel := codexUpstreamModel(req.Model)

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	if e.codexShouldRelayOpenAIResponses(auth, baseURL, opts) {
		return e.executeOpenAIResponsesRelay(ctx, auth, req, opts, apiKey, baseURL)
	}
	if e.codexShouldUseClaudeMessagesBridge(auth, baseURL) {
		return e.executeViaClaudeMessages(ctx, auth, req, opts, apiKey, baseURL)
	}
	if e.codexShouldUseChatCompletionsBridgeForRequest(auth, baseURL, opts) {
		return e.executeViaChatCompletions(ctx, auth, req, opts, apiKey, baseURL)
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	compressionEnabled := shouldEnableCodexRequestCompression(body, auth)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	attempts := e.codexRetryAttempts(auth, baseURL)
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		httpReq, requestBody, cache, errReq := e.cacheHelper(ctx, from, url, req, body, true)
		if errReq != nil {
			err = errReq
			return resp, err
		}
		applyCodexHeaders(httpReq, auth, apiKey, true)
		if errCompress := applyCodexRequestCompression(httpReq, requestBody, compressionEnabled); errCompress != nil {
			err = errCompress
			return resp, err
		}
		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      requestBody,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})
		httpResp, errDo := e.doRequest(ctx, auth, httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			if codexRetryableRequestError(errDo) && attempt < attempts {
				delay := codexRetryDelay(attempt, nil)
				logWithRequestID(ctx).Debugf("codex executor: upstream request failed (%v), retrying attempt %d/%d in %s", errDo, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					err = errWait
					return resp, err
				}
				continue
			}
			err = errDo
			return resp, err
		}
		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			b, _ := io.ReadAll(httpResp.Body)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
			appendAPIResponseChunk(ctx, e.cfg, b)
			logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
			if codexShouldRetryStatus(httpResp.StatusCode, b) && attempt < attempts {
				delay := codexRetryDelay(attempt, httpResp)
				logWithRequestID(ctx).Debugf("codex executor: retryable status=%d, retrying attempt %d/%d in %s", httpResp.StatusCode, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					err = errWait
					return resp, err
				}
				continue
			}
			sErr := statusErr{code: httpResp.StatusCode, msg: string(b)}
			if retryAfter := parseRetryAfterHeader(httpResp.Header.Get("Retry-After")); retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
			err = sErr
			return resp, err
		}
		resp, errRead := e.readNonStreamingResponse(ctx, req, from, to, originalPayload, body, reporter, httpResp)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			if (codexRetryableRequestError(errRead) || codexRetryableSemanticError(errRead)) && attempt < attempts {
				delay := codexRetryDelay(attempt, nil)
				logWithRequestID(ctx).Debugf("codex executor: read response failed (%v), retrying attempt %d/%d in %s", errRead, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					err = errWait
					return resp, err
				}
				continue
			}
			err = errRead
			return resp, err
		}
		e.rememberResponseState(auth, resp.Payload, cache)
		return resp, nil
	}
	err = statusErr{code: 503, msg: "codex executor: all retry attempts exhausted"}
	return resp, err
}

func (e *CodexExecutor) readNonStreamingResponse(ctx context.Context, req cliproxyexecutor.Request, from, to sdktranslator.Format, originalPayload, translatedPayload []byte, reporter *usageReporter, httpResp *http.Response) (cliproxyexecutor.Response, error) {
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800) // 50MB
	var param any
	var accumulator codexResponsesStreamAccumulator
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		appendAPIResponseChunk(ctx, e.cfg, line)
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}

		data := bytes.TrimSpace(line[5:])
		accumulator.ingest(data)
		if gjson.GetBytes(data, "type").String() != "response.completed" {
			continue
		}
		data, err := accumulator.normalizeCompletedEvent(data)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}

		if detail, ok := parseCodexUsage(data); ok {
			reporter.publish(ctx, detail)
		}
		if codexShouldRejectShortContinuationResponse(originalPayload, data) {
			return cliproxyexecutor.Response{}, newCodexInvalidResponseError("responses payload returned short continuation-only output")
		}

		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, translatedPayload, data, &param)
		return cliproxyexecutor.Response{Payload: []byte(out)}, nil
	}
	if err := scanner.Err(); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{}, statusErr{code: 408, msg: "stream error: stream disconnected before completion: stream closed before response.completed"}
}

func isCodexIncompleteResponseError(err error) bool {
	var se statusErr
	if !errors.As(err, &se) {
		return false
	}
	return se.code == http.StatusRequestTimeout && strings.Contains(se.msg, "response.completed")
}

func newCodexInvalidResponseError(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "empty upstream response"
	}
	return statusErr{code: http.StatusBadGateway, msg: "codex executor: invalid upstream response: " + reason}
}

func codexResponseRoot(root gjson.Result) gjson.Result {
	if response := root.Get("response"); response.Exists() {
		return response
	}
	return root
}

func codexResponseContentMeaningful(content gjson.Result) bool {
	if !content.Exists() {
		return false
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) != ""
	}
	if !content.IsArray() {
		return false
	}
	meaningful := false
	content.ForEach(func(_, part gjson.Result) bool {
		partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
		switch partType {
		case "", "output_text", "input_text", "summary_text", "refusal":
			if strings.TrimSpace(part.Get("text").String()) != "" {
				meaningful = true
				return false
			}
		default:
			if strings.TrimSpace(part.Get("text").String()) != "" ||
				strings.TrimSpace(part.Get("arguments").String()) != "" ||
				strings.TrimSpace(part.Get("output").String()) != "" {
				meaningful = true
				return false
			}
		}
		return true
	})
	return meaningful
}

func codexToolCallsMeaningful(toolCalls gjson.Result) bool {
	if !toolCalls.Exists() || !toolCalls.IsArray() {
		return false
	}
	meaningful := false
	toolCalls.ForEach(func(_, call gjson.Result) bool {
		if strings.TrimSpace(call.Get("id").String()) != "" ||
			strings.TrimSpace(call.Get("name").String()) != "" ||
			strings.TrimSpace(call.Get("arguments").String()) != "" ||
			strings.TrimSpace(call.Get("function.name").String()) != "" ||
			strings.TrimSpace(call.Get("function.arguments").String()) != "" {
			meaningful = true
			return false
		}
		return true
	})
	return meaningful
}

func codexResponseItemMeaningful(item gjson.Result) bool {
	if !item.Exists() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "message":
		return codexResponseContentMeaningful(item.Get("content"))
	case "function_call", "custom_tool_call":
		return strings.TrimSpace(item.Get("name").String()) != "" ||
			strings.TrimSpace(item.Get("call_id").String()) != "" ||
			strings.TrimSpace(item.Get("arguments").String()) != ""
	case "reasoning":
		if strings.TrimSpace(item.Get("encrypted_content").String()) != "" {
			return true
		}
		return codexResponseContentMeaningful(item.Get("summary"))
	default:
		return codexResponseContentMeaningful(item.Get("content"))
	}
}

func codexResponsesMeaningful(root gjson.Result) bool {
	root = codexResponseRoot(root)
	output := root.Get("output")
	if !output.Exists() || !output.IsArray() {
		return false
	}
	meaningful := false
	output.ForEach(func(_, item gjson.Result) bool {
		if codexResponseItemMeaningful(item) {
			meaningful = true
			return false
		}
		return true
	})
	return meaningful
}

func validateCodexResponsesPayload(raw []byte) error {
	root := codexResponseRoot(gjson.ParseBytes(raw))
	if !root.Exists() {
		return newCodexInvalidResponseError("responses payload missing response object")
	}
	if errResult := root.Get("error"); errResult.Exists() && errResult.Type != gjson.Null {
		return newCodexInvalidResponseError("responses payload contains error")
	}
	if incomplete := root.Get("incomplete_details"); incomplete.Exists() && incomplete.Type != gjson.Null {
		return newCodexInvalidResponseError("responses payload is incomplete")
	}
	if !codexResponsesMeaningful(root) {
		return newCodexInvalidResponseError("responses completed without meaningful output")
	}
	return nil
}

func codexResponsesEventMeaningful(raw []byte) bool {
	root := gjson.ParseBytes(raw)
	if codexResponseItemMeaningful(root.Get("item")) {
		return true
	}
	switch strings.TrimSpace(root.Get("type").String()) {
	case "response.output_text.delta":
		return strings.TrimSpace(root.Get("delta").String()) != ""
	case "response.output_text.done":
		return strings.TrimSpace(root.Get("text").String()) != ""
	case "response.reasoning_summary_text.delta":
		return strings.TrimSpace(root.Get("delta").String()) != ""
	case "response.reasoning_summary_text.done":
		return strings.TrimSpace(root.Get("text").String()) != ""
	case "response.function_call_arguments.delta":
		return strings.TrimSpace(root.Get("delta").String()) != ""
	case "response.function_call_arguments.done":
		return strings.TrimSpace(root.Get("arguments").String()) != ""
	case "response.completed":
		return codexResponsesMeaningful(root.Get("response"))
	default:
		return codexResponsesMeaningful(root.Get("response"))
	}
}

type codexResponsesStreamAccumulator struct {
	outputItems []string
	textDone    strings.Builder
	textDelta   strings.Builder
	lastItemID  string
}

func (a *codexResponsesStreamAccumulator) ingest(raw []byte) {
	if a == nil || !gjson.ValidBytes(raw) {
		return
	}
	root := gjson.ParseBytes(raw)
	switch strings.TrimSpace(root.Get("type").String()) {
	case "response.output_item.done":
		item := root.Get("item")
		if !codexResponseItemMeaningful(item) || strings.TrimSpace(item.Raw) == "" {
			return
		}
		a.outputItems = append(a.outputItems, item.Raw)
	case "response.output_text.delta":
		if delta := root.Get("delta").String(); delta != "" {
			a.textDelta.WriteString(delta)
		}
		if itemID := strings.TrimSpace(root.Get("item_id").String()); itemID != "" {
			a.lastItemID = itemID
		}
	case "response.output_text.done":
		if text := root.Get("text").String(); text != "" {
			a.textDone.WriteString(text)
		}
		if itemID := strings.TrimSpace(root.Get("item_id").String()); itemID != "" {
			a.lastItemID = itemID
		}
	}
}

func (a *codexResponsesStreamAccumulator) hasMeaningfulOutput() bool {
	if a == nil {
		return false
	}
	for _, raw := range a.outputItems {
		if codexResponseItemMeaningful(gjson.Parse(raw)) {
			return true
		}
	}
	return strings.TrimSpace(a.textDone.String()) != "" || strings.TrimSpace(a.textDelta.String()) != ""
}

func (a *codexResponsesStreamAccumulator) outputText() string {
	if a == nil {
		return ""
	}
	if text := a.textDone.String(); strings.TrimSpace(text) != "" {
		return text
	}
	return a.textDelta.String()
}

func (a *codexResponsesStreamAccumulator) outputItemsJSON() string {
	if a == nil {
		return ""
	}
	if len(a.outputItems) > 0 {
		return "[" + strings.Join(a.outputItems, ",") + "]"
	}
	text := a.outputText()
	if strings.TrimSpace(text) == "" {
		return ""
	}
	item := `{"id":"","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}]}`
	if a.lastItemID != "" {
		item, _ = sjson.Set(item, "id", a.lastItemID)
	}
	item, _ = sjson.Set(item, "content.0.text", text)
	return "[" + item + "]"
}

func isCodexMeaninglessPayloadError(err error) bool {
	var se statusErr
	return errors.As(err, &se) && strings.Contains(se.msg, "without meaningful output")
}

func (a *codexResponsesStreamAccumulator) normalizeCompletedEvent(raw []byte) ([]byte, error) {
	if err := validateCodexResponsesPayload(raw); err == nil {
		return raw, nil
	} else if !isCodexMeaninglessPayloadError(err) || !a.hasMeaningfulOutput() {
		return nil, err
	}

	outputItems := a.outputItemsJSON()
	if outputItems == "" {
		return nil, newCodexInvalidResponseError("responses completed without meaningful output")
	}
	updated, err := sjson.SetRawBytes(raw, "response.output", []byte(outputItems))
	if err != nil {
		return nil, err
	}
	if text := a.outputText(); strings.TrimSpace(text) != "" {
		updated, _ = sjson.SetBytes(updated, "response.output_text", text)
	}
	if err := validateCodexResponsesPayload(updated); err != nil {
		return nil, err
	}
	return updated, nil
}

func codexChatCompletionMeaningful(root gjson.Result) bool {
	if !root.Exists() {
		return false
	}
	choices := root.Get("choices")
	if !choices.Exists() || !choices.IsArray() {
		return false
	}
	meaningful := false
	choices.ForEach(func(_, choice gjson.Result) bool {
		if codexResponseContentMeaningful(choice.Get("message.content")) ||
			codexResponseContentMeaningful(choice.Get("delta.content")) ||
			strings.TrimSpace(choice.Get("message.reasoning_content").String()) != "" ||
			strings.TrimSpace(choice.Get("delta.reasoning_content").String()) != "" ||
			codexToolCallsMeaningful(choice.Get("message.tool_calls")) ||
			codexToolCallsMeaningful(choice.Get("delta.tool_calls")) ||
			strings.TrimSpace(choice.Get("finish_reason").String()) != "" {
			meaningful = true
			return false
		}
		return true
	})
	return meaningful
}

func validateCodexChatCompletionPayload(raw []byte) error {
	root := gjson.ParseBytes(raw)
	if errResult := root.Get("error"); errResult.Exists() && errResult.Type != gjson.Null {
		return newCodexInvalidResponseError("chat completions payload contains error")
	}
	if !codexChatCompletionMeaningful(root) {
		return newCodexInvalidResponseError("chat completions payload completed without meaningful output")
	}
	return nil
}

func codexShouldRejectShortContinuationResponse(requestPayload, responsePayload []byte) bool {
	if !codexRequestLooksLikeContinuation(requestPayload) {
		return false
	}
	text, outputTokens, hasToolOrReasoning := codexResponseSurfaceSummary(responsePayload)
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

func codexRequestLooksLikeContinuation(raw []byte) bool {
	text := strings.TrimSpace(codexLastUserText(raw))
	if text == "" {
		return false
	}
	normalized := strings.ToLower(text)
	normalized = strings.Trim(normalized, " \t\r\n.!?;:,_-`'\"[](){}<>")
	switch normalized {
	case "continue", "go on", "carry on", "keep going", "resume",
		"\u7ee7\u7eed", "\u63a5\u7740", "\u7ee7\u7eed\u5b8c\u6210", "\u7ee7\u7eed\u5904\u7406":
		return true
	default:
		return false
	}
}

func codexLastUserText(raw []byte) string {
	root := gjson.ParseBytes(raw)
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		items := messages.Array()
		for i := len(items) - 1; i >= 0; i-- {
			if !strings.EqualFold(strings.TrimSpace(items[i].Get("role").String()), "user") {
				continue
			}
			if text := strings.TrimSpace(codexTextFromContent(items[i].Get("content"))); text != "" {
				return text
			}
		}
	}
	if input := root.Get("input"); input.Exists() {
		if input.Type == gjson.String {
			return strings.TrimSpace(input.String())
		}
		if input.IsArray() {
			items := input.Array()
			for i := len(items) - 1; i >= 0; i-- {
				item := items[i]
				if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "message") &&
					strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
					if text := strings.TrimSpace(codexTextFromContent(item.Get("content"))); text != "" {
						return text
					}
				}
			}
		}
	}
	return ""
}

func codexTextFromContent(content gjson.Result) string {
	switch content.Type {
	case gjson.String:
		return content.String()
	}
	if !content.Exists() || !content.IsArray() {
		return ""
	}
	var parts []string
	content.ForEach(func(_, item gjson.Result) bool {
		partType := strings.TrimSpace(item.Get("type").String())
		switch partType {
		case "", "text", "input_text", "output_text":
			if text := strings.TrimSpace(item.Get("text").String()); text != "" {
				parts = append(parts, text)
			}
		}
		return true
	})
	return strings.Join(parts, "\n")
}

func codexResponseSurfaceSummary(raw []byte) (string, int64, bool) {
	root := gjson.ParseBytes(raw)
	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() {
		var parts []string
		hasToolOrReasoning := false
		choices.ForEach(func(_, choice gjson.Result) bool {
			if codexToolCallsMeaningful(choice.Get("message.tool_calls")) || codexToolCallsMeaningful(choice.Get("delta.tool_calls")) {
				hasToolOrReasoning = true
			}
			if strings.TrimSpace(choice.Get("message.reasoning_content").String()) != "" ||
				strings.TrimSpace(choice.Get("delta.reasoning_content").String()) != "" {
				hasToolOrReasoning = true
			}
			if text := strings.TrimSpace(codexTextFromContent(choice.Get("message.content"))); text != "" {
				parts = append(parts, text)
			}
			if text := strings.TrimSpace(codexTextFromContent(choice.Get("delta.content"))); text != "" {
				parts = append(parts, text)
			}
			return true
		})
		return strings.Join(parts, "\n"), parseOpenAIUsage(raw).OutputTokens, hasToolOrReasoning
	}

	root = codexResponseRoot(root)
	var parts []string
	hasToolOrReasoning := false
	output := root.Get("output")
	if output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			itemType := strings.TrimSpace(item.Get("type").String())
			switch itemType {
			case "function_call", "reasoning":
				hasToolOrReasoning = true
			case "message":
				if text := strings.TrimSpace(codexTextFromContent(item.Get("content"))); text != "" {
					parts = append(parts, text)
				}
			default:
				if text := strings.TrimSpace(codexTextFromContent(item.Get("content"))); text != "" {
					parts = append(parts, text)
				}
			}
			return true
		})
	}
	detail, ok := parseCodexUsage(raw)
	if !ok {
		detail = parseOpenAIUsage(raw)
	}
	return strings.Join(parts, "\n"), detail.OutputTokens, hasToolOrReasoning
}

func codexTextLooksLikeWorkPromise(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	markers := []string{
		"i'll", "i will", "let me", "starting", "start by", "first i'll", "working on",
		"\u5f00\u59cb", "\u6211\u5148", "\u6211\u4f1a", "\u6211\u6b63\u5728", "\u5148\u641e", "\u7ee7\u7eed",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := codexUpstreamModel(req.Model)

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "stream")
	body = normalizeOpenAIResponsesRelayBody(body)
	compressionEnabled := shouldEnableCodexRequestCompression(body, auth)

	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	attempts := e.codexRetryAttempts(auth, baseURL)
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		httpReq, requestBody, cache, errReq := e.cacheHelper(ctx, from, url, req, body, true)
		if errReq != nil {
			err = errReq
			return resp, err
		}
		applyCodexHeaders(httpReq, auth, apiKey, false)
		if errCompress := applyCodexRequestCompression(httpReq, requestBody, compressionEnabled); errCompress != nil {
			err = errCompress
			return resp, err
		}
		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      requestBody,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})
		httpResp, errDo := e.doRequest(ctx, auth, httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			if codexRetryableRequestError(errDo) && attempt < attempts {
				delay := codexRetryDelay(attempt, nil)
				logWithRequestID(ctx).Debugf("codex executor: compact upstream request failed (%v), retrying attempt %d/%d in %s", errDo, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					err = errWait
					return resp, err
				}
				continue
			}
			err = errDo
			return resp, err
		}
		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			b, _ := io.ReadAll(httpResp.Body)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
			appendAPIResponseChunk(ctx, e.cfg, b)
			logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
			if shouldFallbackCompactToResponses(httpResp.StatusCode, b) {
				logWithRequestID(ctx).Debugf("codex executor: upstream does not support /responses/compact, falling back to /responses")
				fallbackOpts := opts
				fallbackOpts.Alt = ""
				return e.Execute(ctx, auth, req, fallbackOpts)
			}
			if codexShouldRetryStatus(httpResp.StatusCode, b) && attempt < attempts {
				delay := codexRetryDelay(attempt, httpResp)
				logWithRequestID(ctx).Debugf("codex executor: compact retryable status=%d, retrying attempt %d/%d in %s", httpResp.StatusCode, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					err = errWait
					return resp, err
				}
				continue
			}
			sErr := statusErr{code: httpResp.StatusCode, msg: string(b)}
			if retryAfter := parseRetryAfterHeader(httpResp.Header.Get("Retry-After")); retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
			err = sErr
			return resp, err
		}
		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			if codexRetryableRequestError(errRead) && attempt < attempts {
				delay := codexRetryDelay(attempt, nil)
				logWithRequestID(ctx).Debugf("codex executor: compact read failed (%v), retrying attempt %d/%d in %s", errRead, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					err = errWait
					return resp, err
				}
				continue
			}
			err = errRead
			return resp, err
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		if errValidate := validateCodexResponsesPayload(data); errValidate != nil {
			recordAPIResponseError(ctx, e.cfg, errValidate)
			if codexRetryableSemanticError(errValidate) && attempt < attempts {
				delay := codexRetryDelay(attempt, nil)
				logWithRequestID(ctx).Debugf("codex executor: compact invalid response (%v), retrying attempt %d/%d in %s", errValidate, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					err = errWait
					return resp, err
				}
				continue
			}
			err = errValidate
			return resp, err
		}
		if codexShouldRejectShortContinuationResponse(originalPayload, data) {
			errShort := newCodexInvalidResponseError("responses payload returned short continuation-only output")
			recordAPIResponseError(ctx, e.cfg, errShort)
			if codexRetryableSemanticError(errShort) && attempt < attempts {
				delay := codexRetryDelay(attempt, nil)
				logWithRequestID(ctx).Debugf("codex executor: compact low-substance continuation response, retrying attempt %d/%d in %s", attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					err = errWait
					return resp, err
				}
				continue
			}
			err = errShort
			return resp, err
		}
		reporter.publish(ctx, parseOpenAIUsage(data))
		reporter.ensurePublished(ctx)
		var param any
		out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, data, &param)
		resp = cliproxyexecutor.Response{Payload: []byte(out)}
		e.rememberResponseState(auth, resp.Payload, cache)
		return resp, nil
	}
	err = statusErr{code: 503, msg: "codex executor: compact all retry attempts exhausted"}
	return resp, err
}

func shouldFallbackCompactToResponses(statusCode int, body []byte) bool {
	if statusCode != http.StatusNotFound && statusCode != http.StatusMethodNotAllowed {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(string(body)))
	if msg == "" {
		return true
	}
	return strings.Contains(msg, "cannot post") ||
		strings.Contains(msg, "method not allowed") ||
		strings.Contains(msg, "responses/compact")
}

func (e *CodexExecutor) executeOpenAIResponsesRelay(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey, baseURL string) (resp cliproxyexecutor.Response, err error) {
	baseModel := codexUpstreamModel(req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	req = e.applyNativeResponsesContinuationAdaptation(ctx, auth, baseURL, req, opts)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	if len(body) == 0 {
		body = req.Payload
	}
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body = normalizeOpenAIResponsesRelayBody(body)
	streamNonStreaming := e.codexShouldUseStreamForNonStreamingResponses(auth, baseURL)
	if updated, errDelete := sjson.DeleteBytes(body, "stream"); errDelete == nil {
		body = updated
	}

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	attempts := e.codexRetryAttempts(auth, baseURL)
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		retriedAsStream := false
		for {
			effectiveStream := streamNonStreaming || retriedAsStream
			requestBodyPayload := body
			if effectiveStream {
				requestBodyPayload, _ = sjson.SetBytes(requestBodyPayload, "stream", true)
			}

			httpReq, requestBody, cache, errReq := e.cacheHelper(ctx, from, url, req, requestBodyPayload, false)
			if errReq != nil {
				err = errReq
				return resp, err
			}
			httpReq.Header.Set("Content-Type", "application/json")
			if effectiveStream {
				httpReq.Header.Set("Accept", "text/event-stream")
				httpReq.Header.Set("Cache-Control", "no-cache")
			} else {
				httpReq.Header.Set("Accept", "application/json")
			}
			httpReq.Header.Set("User-Agent", codexUserAgent)
			if apiKey != "" {
				httpReq.Header.Set("Authorization", "Bearer "+apiKey)
			}
			var attrs map[string]string
			if auth != nil {
				attrs = auth.Attributes
			}
			util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

			recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
				URL:       url,
				Method:    http.MethodPost,
				Headers:   httpReq.Header.Clone(),
				Body:      requestBody,
				Provider:  e.Identifier(),
				AuthID:    authID,
				AuthLabel: authLabel,
				AuthType:  authType,
				AuthValue: authValue,
			})

			httpResp, errDo := e.doRequest(ctx, auth, httpReq)
			if errDo != nil {
				recordAPIResponseError(ctx, e.cfg, errDo)
				if codexRetryableRequestError(errDo) && attempt < attempts {
					delay := codexRetryDelay(attempt, nil)
					logWithRequestID(ctx).Debugf("codex executor(relay): upstream request failed (%v), retrying attempt %d/%d in %s", errDo, attempt+1, attempts, delay)
					if errWait := codexWaitRetry(ctx, delay); errWait != nil {
						err = errWait
						return resp, err
					}
					break
				}
				err = errDo
				return resp, err
			}

			recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
			if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
				b, _ := io.ReadAll(httpResp.Body)
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor(relay): close response body error: %v", errClose)
				}
				appendAPIResponseChunk(ctx, e.cfg, b)
				logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
				if shouldFallbackNativeResponsesToChatCompletions(auth, baseURL, httpResp.StatusCode, b) {
					logWithRequestID(ctx).Debugf("codex executor(relay): upstream native /responses unavailable, falling back to /chat/completions")
					return e.executeViaChatCompletions(ctx, auth, req, opts, apiKey, baseURL)
				}
				if codexShouldRetryStatus(httpResp.StatusCode, b) && attempt < attempts {
					delay := codexRetryDelay(attempt, httpResp)
					logWithRequestID(ctx).Debugf("codex executor(relay): retryable status=%d, retrying attempt %d/%d in %s", httpResp.StatusCode, attempt+1, attempts, delay)
					if errWait := codexWaitRetry(ctx, delay); errWait != nil {
						err = errWait
						return resp, err
					}
					break
				}
				err = statusErr{code: httpResp.StatusCode, msg: string(b)}
				return resp, err
			}

			responseBody, errRead := io.ReadAll(httpResp.Body)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor(relay): close response body error: %v", errClose)
			}
			if errRead != nil {
				recordAPIResponseError(ctx, e.cfg, errRead)
				if codexRetryableRequestError(errRead) && attempt < attempts {
					delay := codexRetryDelay(attempt, nil)
					logWithRequestID(ctx).Debugf("codex executor(relay): read response failed (%v), retrying attempt %d/%d in %s", errRead, attempt+1, attempts, delay)
					if errWait := codexWaitRetry(ctx, delay); errWait != nil {
						err = errWait
						return resp, err
					}
					break
				}
				err = errRead
				return resp, err
			}
			appendAPIResponseChunk(ctx, e.cfg, responseBody)
			responseBody, errNormalize := codexNormalizeNativeResponsesPayload(responseBody)
			if errNormalize != nil {
				recordAPIResponseError(ctx, e.cfg, errNormalize)
				if !effectiveStream && shouldRetryNativeResponsesRelayAsStream(auth, baseURL, errNormalize) {
					retriedAsStream = true
					logWithRequestID(ctx).Debugf("codex executor(relay): empty native /responses payload, retrying once with SSE mode")
					continue
				}
				if codexRetryableSemanticError(errNormalize) && attempt < attempts {
					delay := codexRetryDelay(attempt, nil)
					logWithRequestID(ctx).Debugf("codex executor(relay): invalid native response (%v), retrying attempt %d/%d in %s", errNormalize, attempt+1, attempts, delay)
					if errWait := codexWaitRetry(ctx, delay); errWait != nil {
						err = errWait
						return resp, err
					}
					break
				}
				err = errNormalize
				return resp, err
			}
			if codexShouldRejectShortContinuationResponse(req.Payload, responseBody) {
				errShort := newCodexInvalidResponseError("responses payload returned short continuation-only output")
				recordAPIResponseError(ctx, e.cfg, errShort)
				if codexRetryableSemanticError(errShort) && attempt < attempts {
					delay := codexRetryDelay(attempt, nil)
					logWithRequestID(ctx).Debugf("codex executor(relay): low-substance continuation response, retrying attempt %d/%d in %s", attempt+1, attempts, delay)
					if errWait := codexWaitRetry(ctx, delay); errWait != nil {
						err = errWait
						return resp, err
					}
					break
				}
				err = errShort
				return resp, err
			}
			reporter.publish(ctx, parseOpenAIUsage(responseBody))
			reporter.ensurePublished(ctx)
			resp = cliproxyexecutor.Response{Payload: responseBody}
			e.rememberBridgeReplayState(req.Payload, resp.Payload, cache)
			e.rememberResponseState(auth, resp.Payload, cache)
			return resp, nil
		}
	}
	err = statusErr{code: 503, msg: "codex executor(relay): all retry attempts exhausted"}
	return resp, err
}

func codexNormalizeNativeResponsesPayload(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, newCodexInvalidResponseError("empty upstream response")
	}

	var leadingErr error
	if normalized, ok, err := codexExtractLeadingNativeResponsesPayload(trimmed); ok {
		if err == nil {
			return normalized, nil
		}
		leadingErr = err
	}

	normalized, err := codexExtractCompletedResponseFromSSE(trimmed)
	if err == nil {
		return normalized, nil
	}
	if leadingErr != nil {
		return nil, leadingErr
	}
	return nil, err
}

func normalizeOpenAIResponsesRelayBody(raw []byte) []byte {
	inputResult := gjson.GetBytes(raw, "input")
	if inputResult.Type != gjson.String {
		return raw
	}

	normalizedInput := `[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`
	normalizedInput, _ = sjson.Set(normalizedInput, "0.content.0.text", inputResult.String())
	updated, err := sjson.SetRawBytes(raw, "input", []byte(normalizedInput))
	if err != nil {
		return raw
	}
	return updated
}

func claudeMessagesShouldRetryWithoutToolChoice(body []byte, statusCode int, responseBody []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	if gjson.GetBytes(body, "tool_choice.type").String() != "tool" {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(string(responseBody)))
	return strings.Contains(msg, "tool_choice.function")
}

func stripClaudeMessagesToolChoice(body []byte) []byte {
	updated, err := sjson.DeleteBytes(body, "tool_choice")
	if err != nil {
		return body
	}
	return updated
}

func shouldFallbackNativeResponsesToChatCompletions(auth *cliproxyauth.Auth, baseURL string, statusCode int, body []byte) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return false
	}
	normalizedBase := strings.ToLower(strings.TrimSpace(baseURL))
	if normalizedBase == "" {
		normalizedBase = strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	}
	if normalizedBase == "" || strings.Contains(normalizedBase, "chatgpt.com/backend-api/codex") {
		return false
	}

	msg := strings.ToLower(strings.TrimSpace(string(body)))
	switch statusCode {
	case http.StatusBadRequest:
		return strings.Contains(msg, "unsupported content type") ||
			strings.Contains(msg, "unsupported media type") ||
			strings.Contains(msg, "unsupported protocol")
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnsupportedMediaType:
		return msg == "" ||
			strings.Contains(msg, "not found") ||
			strings.Contains(msg, "cannot post") ||
			strings.Contains(msg, "method not allowed") ||
			strings.Contains(msg, "unsupported") ||
			strings.Contains(msg, "responses")
	default:
		return false
	}
}

func shouldRetryNativeResponsesRelayAsStream(auth *cliproxyauth.Auth, baseURL string, err error) bool {
	if !isCodexMeaninglessPayloadError(err) {
		return false
	}
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return false
	}
	normalizedBase := strings.ToLower(strings.TrimSpace(baseURL))
	if normalizedBase == "" {
		normalizedBase = strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	}
	return normalizedBase != "" && !strings.Contains(normalizedBase, "chatgpt.com/backend-api/codex")
}

func codexExtractLeadingNativeResponsesPayload(raw []byte) ([]byte, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var payload json.RawMessage
	if err := decoder.Decode(&payload); err != nil {
		return nil, false, nil
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, false, nil
	}
	normalized, err := codexNormalizeValidatedNativeResponsesPayload(payload)
	if err != nil {
		return nil, true, err
	}
	return normalized, true, nil
}

func codexNormalizeValidatedNativeResponsesPayload(raw []byte) ([]byte, error) {
	if err := validateCodexResponsesPayload(raw); err != nil {
		return nil, err
	}
	if response := gjson.GetBytes(raw, "response"); response.Exists() && strings.TrimSpace(response.Raw) != "" {
		return []byte(response.Raw), nil
	}
	return raw, nil
}

func codexExtractCompletedResponseFromSSE(raw []byte) ([]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(nil, 52_428_800)
	var accumulator codexResponsesStreamAccumulator
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}
		data := bytes.TrimSpace(line[len(dataTag):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) || !gjson.ValidBytes(data) {
			continue
		}
		accumulator.ingest(data)
		if gjson.GetBytes(data, "type").String() != "response.completed" {
			continue
		}
		data, err := accumulator.normalizeCompletedEvent(data)
		if err != nil {
			return nil, err
		}
		response, err := codexNormalizeValidatedNativeResponsesPayload(data)
		if err != nil {
			return nil, err
		}
		return response, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, newCodexInvalidResponseError("stream closed before response.completed")
}

func (e *CodexExecutor) executeStreamOpenAIResponsesRelay(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey, baseURL string) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	baseModel := codexUpstreamModel(req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai-response")
	req = e.applyNativeResponsesContinuationAdaptation(ctx, auth, baseURL, req, opts)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	if len(body) == 0 {
		body = req.Payload
	}
	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.SetBytes(body, "stream", true)
	body = normalizeOpenAIResponsesRelayBody(body)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, requestBody, cache, err := e.cacheHelper(ctx, from, url, req, body, false)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("User-Agent", codexUserAgent)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

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
		Body:      requestBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := e.doRequest(ctx, auth, httpReq)
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
			log.Errorf("codex executor(relay): close response body error: %v", errClose)
		}
		if shouldFallbackNativeResponsesToChatCompletions(auth, baseURL, httpResp.StatusCode, b) {
			logWithRequestID(ctx).Debugf("codex executor(relay stream): upstream native /responses unavailable, falling back to /chat/completions")
			return e.executeStreamViaChatCompletions(ctx, auth, req, opts, apiKey, baseURL)
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
				log.Errorf("codex executor(relay): close response body error: %v", errClose)
			}
		}()
		defer reporter.ensurePublished(ctx)

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		block := make([][]byte, 0, 4)
		var accumulator codexResponsesStreamAccumulator
		flushBlock := func() {
			if len(block) == 0 {
				return
			}
			payload := bytes.Join(block, []byte("\n"))
			payload = append(payload, '\n')
			block = block[:0]
			out <- cliproxyexecutor.StreamChunk{Payload: payload}
		}

		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			appendAPIResponseChunk(ctx, e.cfg, line)
			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				accumulator.ingest(data)
				if detail, ok := parseCodexUsage(data); ok {
					reporter.publish(ctx, detail)
				}
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					if normalizedData, errNormalize := accumulator.normalizeCompletedEvent(data); errNormalize == nil {
						line = append([]byte("data: "), normalizedData...)
						normalized, _ := codexNormalizeValidatedNativeResponsesPayload(normalizedData)
						e.rememberBridgeReplayState(req.Payload, normalized, cache)
						e.rememberResponseState(auth, normalized, cache)
					}
				}
			}
			if len(line) == 0 {
				flushBlock()
				continue
			}
			block = append(block, line)
		}
		flushBlock()
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return stream, nil
}

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	baseModel := codexUpstreamModel(req.Model)

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	if e.codexShouldRelayOpenAIResponses(auth, baseURL, opts) {
		return e.executeStreamOpenAIResponsesRelay(ctx, auth, req, opts, apiKey, baseURL)
	}
	if e.codexShouldUseClaudeMessagesBridge(auth, baseURL) {
		return e.executeStreamViaClaudeMessages(ctx, auth, req, opts, apiKey, baseURL)
	}
	if e.codexShouldUseChatCompletionsBridgeForRequest(auth, baseURL, opts) {
		return e.executeStreamViaChatCompletions(ctx, auth, req, opts, apiKey, baseURL)
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.SetBytes(body, "model", baseModel)
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	compressionEnabled := shouldEnableCodexRequestCompression(body, auth)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	attempts := e.codexRetryAttempts(auth, baseURL)
	if attempts < 1 {
		attempts = 1
	}
	var httpResp *http.Response
	var cache codexCache
	for attempt := 1; attempt <= attempts; attempt++ {
		httpReq, requestBody, resolvedCache, errReq := e.cacheHelper(ctx, from, url, req, body, true)
		if errReq != nil {
			return nil, errReq
		}
		cache = resolvedCache
		applyCodexHeaders(httpReq, auth, apiKey, true)
		if errCompress := applyCodexRequestCompression(httpReq, requestBody, compressionEnabled); errCompress != nil {
			return nil, errCompress
		}
		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      requestBody,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpResp, err = e.doRequest(ctx, auth, httpReq)
		if err != nil {
			recordAPIResponseError(ctx, e.cfg, err)
			if codexRetryableRequestError(err) && attempt < attempts {
				delay := codexRetryDelay(attempt, nil)
				logWithRequestID(ctx).Debugf("codex executor: stream upstream request failed (%v), retrying attempt %d/%d in %s", err, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					return nil, errWait
				}
				continue
			}
			return nil, err
		}
		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			data, readErr := io.ReadAll(httpResp.Body)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
			if readErr != nil {
				recordAPIResponseError(ctx, e.cfg, readErr)
				if codexRetryableRequestError(readErr) && attempt < attempts {
					delay := codexRetryDelay(attempt, nil)
					logWithRequestID(ctx).Debugf("codex executor: stream read error (%v), retrying attempt %d/%d in %s", readErr, attempt+1, attempts, delay)
					if errWait := codexWaitRetry(ctx, delay); errWait != nil {
						return nil, errWait
					}
					continue
				}
				return nil, readErr
			}
			appendAPIResponseChunk(ctx, e.cfg, data)
			logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
			if codexShouldRetryStatus(httpResp.StatusCode, data) && attempt < attempts {
				delay := codexRetryDelay(attempt, httpResp)
				logWithRequestID(ctx).Debugf("codex executor: stream retryable status=%d, retrying attempt %d/%d in %s", httpResp.StatusCode, attempt+1, attempts, delay)
				if errWait := codexWaitRetry(ctx, delay); errWait != nil {
					return nil, errWait
				}
				continue
			}
			sErr := statusErr{code: httpResp.StatusCode, msg: string(data)}
			if retryAfter := parseRetryAfterHeader(httpResp.Header.Get("Retry-After")); retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
			return nil, sErr
		}
		break
	}
	if httpResp == nil {
		return nil, statusErr{code: 503, msg: "codex executor: stream all retry attempts exhausted"}
	}
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800) // 50MB
	var param any
	bufferedChunks := make([]cliproxyexecutor.StreamChunk, 0, 8)
	meaningfulSeen := false
	var accumulator codexResponsesStreamAccumulator
	for !meaningfulSeen {
		if !scanner.Scan() {
			if errScan := scanner.Err(); errScan != nil {
				recordAPIResponseError(ctx, e.cfg, errScan)
				reporter.publishFailure(ctx)
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close response body error: %v", errClose)
				}
				return nil, errScan
			}
			reporter.publishFailure(ctx)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
			return nil, newCodexInvalidResponseError("stream ended before any meaningful response output")
		}
		line := bytes.Clone(scanner.Bytes())
		appendAPIResponseChunk(ctx, e.cfg, line)
		if bytes.HasPrefix(line, dataTag) {
			data := bytes.TrimSpace(line[5:])
			accumulator.ingest(data)
			if gjson.GetBytes(data, "type").String() == "response.completed" {
				normalizedData, errValidate := accumulator.normalizeCompletedEvent(data)
				if errValidate != nil {
					recordAPIResponseError(ctx, e.cfg, errValidate)
					reporter.publishFailure(ctx)
					if errClose := httpResp.Body.Close(); errClose != nil {
						log.Errorf("codex executor: close response body error: %v", errClose)
					}
					return nil, errValidate
				}
				line = append([]byte("data: "), normalizedData...)
				data = normalizedData
				if detail, ok := parseCodexUsage(data); ok {
					reporter.publish(ctx, detail)
				}
			}
			if codexResponsesEventMeaningful(data) {
				meaningfulSeen = true
			}
		}
		chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, body, bytes.Clone(line), &param)
		for i := range chunks {
			payload := []byte(chunks[i])
			e.rememberResponseState(auth, payload, cache)
			bufferedChunks = append(bufferedChunks, cliproxyexecutor.StreamChunk{Payload: payload})
		}
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()
		defer reporter.ensurePublished(ctx)
		for i := range bufferedChunks {
			out <- bufferedChunks[i]
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)

			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				accumulator.ingest(data)
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					normalizedData, errValidate := accumulator.normalizeCompletedEvent(data)
					if errValidate != nil {
						recordAPIResponseError(ctx, e.cfg, errValidate)
						reporter.publishFailure(ctx)
						out <- cliproxyexecutor.StreamChunk{Err: errValidate}
						return
					}
					line = append([]byte("data: "), normalizedData...)
					data = normalizedData
					if detail, ok := parseCodexUsage(data); ok {
						reporter.publish(ctx, detail)
					}
				}
			}

			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, body, bytes.Clone(line), &param)
			for i := range chunks {
				payload := []byte(chunks[i])
				e.rememberResponseState(auth, payload, cache)
				out <- cliproxyexecutor.StreamChunk{Payload: payload}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return stream, nil
}

func (e *CodexExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := codexUpstreamModel(req.Model)

	from := opts.SourceFormat
	to := sdktranslator.FromString("codex")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err := thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	body, _ = sjson.SetBytes(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.SetBytes(body, "stream", false)
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	enc, err := tokenizerForCodexModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: tokenizer init failed: %w", err)
	}

	count, err := countCodexInputTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

func (e *CodexExecutor) executeViaChatCompletions(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey, baseURL string) (resp cliproxyexecutor.Response, err error) {
	baseModel := codexUpstreamModel(req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	req = e.applyBridgeReplayRequest(ctx, req, opts)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	if opts.Alt == "responses/compact" {
		body, _ = sjson.DeleteBytes(body, "stream")
	}

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, requestBody, cache, err := e.cacheHelper(ctx, from, url, req, body, false)
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", codexUserAgent)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

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
		Body:      requestBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := e.doRequest(ctx, auth, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor(chat bridge): close response body error: %v", errClose)
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
	responseBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, responseBody)
	if errValidate := validateCodexChatCompletionPayload(responseBody); errValidate != nil {
		recordAPIResponseError(ctx, e.cfg, errValidate)
		return resp, errValidate
	}
	if codexShouldRejectShortContinuationResponse(originalPayload, responseBody) {
		errShort := newCodexInvalidResponseError("chat completions payload returned short continuation-only output")
		recordAPIResponseError(ctx, e.cfg, errShort)
		return resp, errShort
	}
	reporter.publish(ctx, parseOpenAIUsage(responseBody))
	reporter.ensurePublished(ctx)

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, body, responseBody, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out)}
	e.rememberBridgeReplayState(req.Payload, resp.Payload, cache)
	e.rememberResponseState(auth, resp.Payload, cache)
	return resp, nil
}

func (e *CodexExecutor) executeStreamViaChatCompletions(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey, baseURL string) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	baseModel := codexUpstreamModel(req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	req = e.applyBridgeReplayRequest(ctx, req, opts)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, requestBody, cache, err := e.cacheHelper(ctx, from, url, req, body, false)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("User-Agent", codexUserAgent)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)

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
		Body:      requestBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := e.doRequest(ctx, auth, httpReq)
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
			log.Errorf("codex executor(chat bridge): close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)
	var param any
	bufferedChunks := make([]cliproxyexecutor.StreamChunk, 0, 8)
	meaningfulSeen := false
	for !meaningfulSeen {
		if !scanner.Scan() {
			if errScan := scanner.Err(); errScan != nil {
				recordAPIResponseError(ctx, e.cfg, errScan)
				reporter.publishFailure(ctx)
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor(chat bridge): close response body error: %v", errClose)
				}
				return nil, errScan
			}
			reporter.publishFailure(ctx)
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor(chat bridge): close response body error: %v", errClose)
			}
			return nil, newCodexInvalidResponseError("chat completions stream ended before any meaningful response output")
		}
		line := bytes.Clone(scanner.Bytes())
		appendAPIResponseChunk(ctx, e.cfg, line)
		if detail, ok := parseOpenAIStreamUsage(line); ok {
			reporter.publish(ctx, detail)
		}
		if len(line) != 0 && bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimSpace(line[5:])
			if !bytes.Equal(data, []byte("[DONE]")) && codexChatCompletionMeaningful(gjson.ParseBytes(data)) {
				meaningfulSeen = true
			}
		}
		chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, body, bytes.Clone(line), &param)
		for i := range chunks {
			payload := []byte(chunks[i])
			e.rememberBridgeReplayState(req.Payload, payload, cache)
			e.rememberResponseState(auth, payload, cache)
			bufferedChunks = append(bufferedChunks, cliproxyexecutor.StreamChunk{Payload: payload})
		}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor(chat bridge): close response body error: %v", errClose)
			}
		}()
		defer reporter.ensurePublished(ctx)
		for i := range bufferedChunks {
			out <- bufferedChunks[i]
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, body, bytes.Clone(line), &param)
			for i := range chunks {
				payload := []byte(chunks[i])
				e.rememberBridgeReplayState(req.Payload, payload, cache)
				e.rememberResponseState(auth, payload, cache)
				out <- cliproxyexecutor.StreamChunk{Payload: payload}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
		reporter.ensurePublished(ctx)
	}()
	return stream, nil
}

func codexTranslatedStreamChunkMeaningful(from sdktranslator.Format, payload []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		data := jsonPayload(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		switch from {
		case "openai-response":
			if codexResponsesEventMeaningful(data) {
				return true
			}
		case "openai":
			if codexChatCompletionMeaningful(gjson.ParseBytes(data)) {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func (e *CodexExecutor) executeViaClaudeMessages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey, baseURL string) (resp cliproxyexecutor.Response, err error) {
	baseModel := codexUpstreamModel(req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	streamUpstream := from != to
	req = e.applyBridgeReplayRequest(ctx, req, opts)

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, streamUpstream)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, streamUpstream)
	if len(body) == 0 {
		body = req.Payload
	}
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeMessagesPayload(body)

	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body

	url := buildClaudeMessagesRequestURL(baseURL)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	sendClaudeRequest := func(requestBody []byte) (*http.Response, []byte, error) {
		upstreamBody := requestBody
		if isClaudeOAuthToken(apiKey) {
			upstreamBody = applyClaudeToolPrefix(requestBody, claudeToolPrefix)
		}
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
		if errReq != nil {
			return nil, nil, errReq
		}
		applyClaudeHeaders(httpReq, auth, apiKey, streamUpstream, extraBetas, false)
		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      upstreamBody,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})
		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			return nil, nil, errDo
		}
		return httpResp, upstreamBody, nil
	}

	httpResp, _, err := sendClaudeRequest(body)
	if err != nil {
		return resp, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor(claude bridge): close response body error: %v", errClose)
		}
		if claudeMessagesShouldRetryWithoutToolChoice(body, httpResp.StatusCode, b) {
			body = stripClaudeMessagesToolChoice(body)
			bodyForTranslation = body
			httpResp, _, err = sendClaudeRequest(body)
			if err != nil {
				return resp, err
			}
			recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		}
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			err = statusErr{code: httpResp.StatusCode, msg: string(b)}
			return resp, err
		}
	}

	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor(claude bridge): close response body error: %v", errClose)
		}
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("codex executor(claude bridge): close response body error: %v", errClose)
		}
	}()

	data, err := io.ReadAll(decodedBody)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if streamUpstream {
		lines := bytes.Split(data, []byte("\n"))
		for _, line := range lines {
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
		}
	} else {
		reporter.publish(ctx, parseClaudeUsage(data))
	}
	if isClaudeOAuthToken(apiKey) {
		data = stripClaudeToolPrefixFromResponse(data, claudeToolPrefix)
	}

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, originalPayload, bodyForTranslation, data, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out)}
	if from == "openai-response" {
		if errValidate := validateCodexResponsesPayload(resp.Payload); errValidate != nil {
			recordAPIResponseError(ctx, e.cfg, errValidate)
			return cliproxyexecutor.Response{}, errValidate
		}
	}
	reporter.ensurePublished(ctx)

	cache := codexCache{}
	if from == "openai-response" {
		cache = resolveOpenAIResponsesCache(req.Payload)
	}
	e.rememberBridgeReplayState(req.Payload, resp.Payload, cache)
	e.rememberResponseState(auth, resp.Payload, cache)
	return resp, nil
}

func (e *CodexExecutor) executeStreamViaClaudeMessages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, apiKey, baseURL string) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	baseModel := codexUpstreamModel(req.Model)

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	req = e.applyBridgeReplayRequest(ctx, req, opts)
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	if len(body) == 0 {
		body = req.Payload
	}
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeMessagesPayload(body)

	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body

	url := buildClaudeMessagesRequestURL(baseURL)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	sendClaudeStreamRequest := func(requestBody []byte) (*http.Response, []byte, error) {
		upstreamBody := requestBody
		if isClaudeOAuthToken(apiKey) {
			upstreamBody = applyClaudeToolPrefix(requestBody, claudeToolPrefix)
		}
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(upstreamBody))
		if errReq != nil {
			return nil, nil, errReq
		}
		applyClaudeHeaders(httpReq, auth, apiKey, true, extraBetas, false)
		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      upstreamBody,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})
		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			return nil, nil, errDo
		}
		return httpResp, upstreamBody, nil
	}

	httpResp, _, err := sendClaudeStreamRequest(body)
	if err != nil {
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor(claude bridge): close response body error: %v", errClose)
		}
		if claudeMessagesShouldRetryWithoutToolChoice(body, httpResp.StatusCode, b) {
			body = stripClaudeMessagesToolChoice(body)
			bodyForTranslation = body
			httpResp, _, err = sendClaudeStreamRequest(body)
			if err != nil {
				return nil, err
			}
			recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		}
		if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
			err = statusErr{code: httpResp.StatusCode, msg: string(b)}
			return nil, err
		}
	}

	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor(claude bridge): close response body error: %v", errClose)
		}
		return nil, err
	}

	cache := codexCache{}
	if from == "openai-response" {
		cache = resolveOpenAIResponsesCache(req.Payload)
	}

	scanner := bufio.NewScanner(decodedBody)
	scanner.Buffer(nil, 52_428_800)
	var param any
	bufferedChunks := make([]cliproxyexecutor.StreamChunk, 0, 8)
	meaningfulSeen := false
	for !meaningfulSeen {
		if !scanner.Scan() {
			if errScan := scanner.Err(); errScan != nil {
				recordAPIResponseError(ctx, e.cfg, errScan)
				reporter.publishFailure(ctx)
				if errClose := decodedBody.Close(); errClose != nil {
					log.Errorf("codex executor(claude bridge): close response body error: %v", errClose)
				}
				return nil, errScan
			}
			reporter.publishFailure(ctx)
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("codex executor(claude bridge): close response body error: %v", errClose)
			}
			return nil, newCodexInvalidResponseError("claude messages stream ended before any meaningful response output")
		}
		line := bytes.Clone(scanner.Bytes())
		appendAPIResponseChunk(ctx, e.cfg, line)
		if detail, ok := parseClaudeStreamUsage(line); ok {
			reporter.publish(ctx, detail)
		}
		if isClaudeOAuthToken(apiKey) {
			line = stripClaudeToolPrefixFromStreamLine(line, claudeToolPrefix)
		}
		chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, bodyForTranslation, bytes.Clone(line), &param)
		for i := range chunks {
			payload := []byte(chunks[i])
			e.rememberBridgeReplayState(req.Payload, payload, cache)
			e.rememberResponseState(auth, payload, cache)
			if codexTranslatedStreamChunkMeaningful(from, payload) {
				meaningfulSeen = true
			}
			bufferedChunks = append(bufferedChunks, cliproxyexecutor.StreamChunk{Payload: payload})
		}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("codex executor(claude bridge): close response body error: %v", errClose)
			}
		}()
		defer reporter.ensurePublished(ctx)

		for i := range bufferedChunks {
			out <- bufferedChunks[i]
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := parseClaudeStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			if isClaudeOAuthToken(apiKey) {
				line = stripClaudeToolPrefixFromStreamLine(line, claudeToolPrefix)
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, bodyForTranslation, bytes.Clone(line), &param)
			for i := range chunks {
				payload := []byte(chunks[i])
				e.rememberBridgeReplayState(req.Payload, payload, cache)
				e.rememberResponseState(auth, payload, cache)
				out <- cliproxyexecutor.StreamChunk{Payload: payload}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return stream, nil
}

func tokenizerForCodexModel(model string) (tokenizer.Codec, error) {
	sanitized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case sanitized == "":
		return tokenizer.Get(tokenizer.Cl100kBase)
	case strings.HasPrefix(sanitized, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(sanitized, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(sanitized, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(sanitized, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(sanitized, "gpt-3.5"), strings.HasPrefix(sanitized, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	default:
		return tokenizer.Get(tokenizer.Cl100kBase)
	}
}

func codexUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.Index(model, "/"); idx >= 0 && idx < len(model)-1 {
		model = strings.TrimSpace(model[idx+1:])
	}
	return thinking.ParseSuffix(model).ModelName
}

func countCodexInputTokens(enc tokenizer.Codec, body []byte) (int64, error) {
	if enc == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(body) == 0 {
		return 0, nil
	}

	root := gjson.ParseBytes(body)
	var segments []string

	if inst := strings.TrimSpace(root.Get("instructions").String()); inst != "" {
		segments = append(segments, inst)
	}

	inputItems := root.Get("input")
	if inputItems.IsArray() {
		arr := inputItems.Array()
		for i := range arr {
			item := arr[i]
			switch item.Get("type").String() {
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					parts := content.Array()
					for j := range parts {
						part := parts[j]
						if text := strings.TrimSpace(part.Get("text").String()); text != "" {
							segments = append(segments, text)
						}
					}
				}
			case "function_call":
				if name := strings.TrimSpace(item.Get("name").String()); name != "" {
					segments = append(segments, name)
				}
				if args := strings.TrimSpace(item.Get("arguments").String()); args != "" {
					segments = append(segments, args)
				}
			case "function_call_output":
				if out := strings.TrimSpace(item.Get("output").String()); out != "" {
					segments = append(segments, out)
				}
			default:
				if text := strings.TrimSpace(item.Get("text").String()); text != "" {
					segments = append(segments, text)
				}
			}
		}
	}

	tools := root.Get("tools")
	if tools.IsArray() {
		tarr := tools.Array()
		for i := range tarr {
			tool := tarr[i]
			if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
				segments = append(segments, name)
			}
			if desc := strings.TrimSpace(tool.Get("description").String()); desc != "" {
				segments = append(segments, desc)
			}
			if params := tool.Get("parameters"); params.Exists() {
				val := params.Raw
				if params.Type == gjson.String {
					val = params.String()
				}
				if trimmed := strings.TrimSpace(val); trimmed != "" {
					segments = append(segments, trimmed)
				}
			}
		}
	}

	textFormat := root.Get("text.format")
	if textFormat.Exists() {
		if name := strings.TrimSpace(textFormat.Get("name").String()); name != "" {
			segments = append(segments, name)
		}
		if schema := textFormat.Get("schema"); schema.Exists() {
			val := schema.Raw
			if schema.Type == gjson.String {
				val = schema.String()
			}
			if trimmed := strings.TrimSpace(val); trimmed != "" {
				segments = append(segments, trimmed)
			}
		}
	}

	text := strings.Join(segments, "\n")
	if text == "" {
		return 0, nil
	}

	count, err := enc.Count(text)
	if err != nil {
		return 0, err
	}
	return int64(count), nil
}

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("codex executor: refresh called")
	if auth == nil {
		return nil, statusErr{code: 500, msg: "codex executor: auth is nil"}
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
	svc := codexauth.NewCodexAuth(e.cfg)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["id_token"] = td.IDToken
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		auth.Metadata["account_id"] = td.AccountID
	}
	auth.Metadata["email"] = td.Email
	// Use unified key in files
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "codex"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte, injectPromptCacheKey bool) (*http.Request, []byte, codexCache, error) {
	var cache codexCache
	effectiveJSON := rawJSON
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := fmt.Sprintf("%s-%s", req.Model, userIDResult.String())
			var ok bool
			if cache, ok = getCodexCache(key); !ok {
				cache = codexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				setCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		cache = resolveOpenAIResponsesCache(req.Payload)
	}

	if cache.ID != "" && injectPromptCacheKey {
		effectiveJSON, _ = sjson.SetBytes(effectiveJSON, "prompt_cache_key", cache.ID)
	}
	if promptCacheKey := cache.requestPromptCacheKey(); promptCacheKey != "" && !gjson.GetBytes(effectiveJSON, "prompt_cache_key").Exists() {
		effectiveJSON, _ = sjson.SetBytes(effectiveJSON, "prompt_cache_key", promptCacheKey)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(effectiveJSON))
	if err != nil {
		return nil, nil, codexCache{}, err
	}
	if conversationID := cache.requestConversationID(); conversationID != "" {
		httpReq.Header.Set("Conversation_id", conversationID)
	}
	if sessionID := cache.requestSessionID(); sessionID != "" {
		httpReq.Header.Set("Session_id", sessionID)
	}
	return httpReq, effectiveJSON, cache, nil
}

func resolveOpenAIResponsesCache(rawJSON []byte) codexCache {
	expireAt := time.Now().Add(1 * time.Hour)
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt_cache_key").String()); promptCacheKey != "" {
		return codexCache{ID: promptCacheKey, Expire: expireAt}
	}
	if previousResponseID := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()); previousResponseID != "" {
		if cache, ok := getCodexResponseCache(previousResponseID); ok {
			return cache
		}
		return codexCache{ID: previousResponseID, Expire: expireAt}
	}
	return codexCache{ID: uuid.NewString(), Expire: expireAt}
}

func (e *CodexExecutor) applyBridgeReplayRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) cliproxyexecutor.Request {
	if !strings.EqualFold(strings.TrimSpace(string(opts.SourceFormat)), "openai-response") {
		return req
	}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(req.Payload, "previous_response_id").String())
	if previousResponseID == "" {
		previousResponseID = strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String())
	}
	if previousResponseID == "" {
		return req
	}

	previousTemplate, ok := getCodexBridgeReplay(previousResponseID)
	if !ok {
		return req
	}
	merged, ok := mergeOpenAIResponsesReplayTemplate(previousTemplate, req.Payload)
	if !ok || len(merged) == 0 {
		return req
	}
	logWithRequestID(ctx).Debugf("codex executor: replayed bridge continuation for previous_response_id=%s", previousResponseID)
	req.Payload = merged
	return req
}

func (e *CodexExecutor) applyNativeResponsesContinuationAdaptation(ctx context.Context, auth *cliproxyauth.Auth, baseURL string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) cliproxyexecutor.Request {
	if !strings.EqualFold(strings.TrimSpace(string(opts.SourceFormat)), "openai-response") {
		return req
	}
	if auth == nil || auth.Attributes == nil || strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return req
	}
	normalizedBase := strings.ToLower(strings.TrimSpace(baseURL))
	if normalizedBase == "" {
		normalizedBase = strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	}
	if normalizedBase == "" || strings.Contains(normalizedBase, "chatgpt.com/backend-api/codex") {
		return req
	}

	previousResponseID := strings.TrimSpace(gjson.GetBytes(req.Payload, "previous_response_id").String())
	if previousResponseID == "" {
		previousResponseID = strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String())
	}
	if previousResponseID == "" {
		return req
	}

	previousTemplate, ok := getCodexBridgeReplay(previousResponseID)
	if !ok {
		return req
	}
	merged, ok := mergeOpenAIResponsesReplayTemplate(previousTemplate, req.Payload)
	if !ok || len(merged) == 0 {
		return req
	}
	merged, _ = sjson.DeleteBytes(merged, "previous_response_id")
	if cache, ok := getCodexResponseCache(previousResponseID); ok {
		if promptCacheKey := cache.requestPromptCacheKey(); promptCacheKey != "" && !gjson.GetBytes(merged, "prompt_cache_key").Exists() {
			merged, _ = sjson.SetBytes(merged, "prompt_cache_key", promptCacheKey)
		}
	}
	logWithRequestID(ctx).Debugf("codex executor: adapted native continuation for previous_response_id=%s", previousResponseID)
	req.Payload = merged
	return req
}

func openAIResponsesRequestHasFunctionCallOutput(raw []byte) bool {
	input := gjson.GetBytes(raw, "input")
	if !input.Exists() || !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "function_call_output") {
			found = true
			return false
		}
		return true
	})
	return found
}

func (e *CodexExecutor) rememberBridgeReplayState(requestPayload, responsePayload []byte, cache codexCache) {
	if len(requestPayload) == 0 || len(responsePayload) == 0 {
		return
	}
	completedPayloads := extractOpenAIResponsesCompletedPayloads(responsePayload)
	if len(completedPayloads) == 0 {
		return
	}
	if cache.Expire.IsZero() {
		cache.Expire = time.Now().Add(1 * time.Hour)
	}
	for _, completedPayload := range completedPayloads {
		template := buildOpenAIResponsesReplayTemplate(requestPayload, completedPayload)
		if len(template) == 0 {
			continue
		}
		responseIDs := extractOpenAIResponseIDs(completedPayload)
		if len(responseIDs) == 0 {
			continue
		}
		setCodexBridgeReplay(responseIDs, template, cache.Expire)
	}
}

func (e *CodexExecutor) rememberResponseState(auth *cliproxyauth.Auth, payload []byte, cache codexCache) {
	responseIDs := extractOpenAIResponseIDs(payload)
	if len(responseIDs) == 0 {
		return
	}
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(payload, "prompt_cache_key").String()); promptCacheKey != "" {
		cache.PromptCacheKey = promptCacheKey
	}
	if conversationID := strings.TrimSpace(gjson.GetBytes(payload, "conversation_id").String()); conversationID != "" {
		cache.ConversationID = conversationID
	}
	if sessionID := strings.TrimSpace(gjson.GetBytes(payload, "session_id").String()); sessionID != "" {
		cache.SessionID = sessionID
	}
	if cache.ID != "" {
		setCodexResponseCache(responseIDs, cache)
	}
	if auth != nil && strings.TrimSpace(auth.ID) != "" {
		cliproxyauth.RememberResponseAffinityIDs(responseIDs, auth.ID)
	}
}

func shouldEnableCodexRequestCompression(rawJSON []byte, auth *cliproxyauth.Auth) bool {
	if gjson.GetBytes(rawJSON, "features.enable_request_compression").Exists() {
		return gjson.GetBytes(rawJSON, "features.enable_request_compression").Bool()
	}
	if gjson.GetBytes(rawJSON, "enable_request_compression").Exists() {
		return gjson.GetBytes(rawJSON, "enable_request_compression").Bool()
	}
	if auth == nil || auth.Attributes == nil {
		return false
	}
	raw := strings.TrimSpace(auth.Attributes["enable_request_compression"])
	if raw == "" {
		return false
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return enabled
}

func applyCodexRequestCompression(req *http.Request, rawJSON []byte, enabled bool) error {
	if !enabled || req == nil || len(rawJSON) == 0 {
		return nil
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(rawJSON); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	data := buf.Bytes()
	req.Body = io.NopCloser(bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Encoding", "gzip")
	return nil
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	misc.EnsureHeader(r.Header, ginHeaders, "Version", codexClientVersion)
	misc.EnsureHeader(r.Header, ginHeaders, "Openai-Beta", "responses=experimental")
	misc.EnsureHeader(r.Header, ginHeaders, "Session_id", uuid.NewString())
	misc.EnsureHeader(r.Header, ginHeaders, "User-Agent", codexUserAgent)

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	if !isAPIKey {
		r.Header.Set("Originator", "codex_cli_rs")
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = normalizeOpenAICompatibleBaseURL(a.Attributes["base_url"])
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range e.cfg.CodexKey {
		entry := &e.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range e.cfg.CodexKey {
			entry := &e.cfg.CodexKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func (e *CodexExecutor) codexShouldUseChatCompletionsBridge(auth *cliproxyauth.Auth, baseURL string) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return false
	}
	normalizedBase := strings.ToLower(strings.TrimSpace(baseURL))
	if normalizedBase == "" {
		normalizedBase = strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	}
	if strings.Contains(normalizedBase, "aixj.vip") {
		return false
	}
	return normalizedBase != "" && !strings.Contains(normalizedBase, "chatgpt.com/backend-api/codex")
}

func (e *CodexExecutor) codexShouldUseChatCompletionsBridgeForRequest(auth *cliproxyauth.Auth, baseURL string, opts cliproxyexecutor.Options) bool {
	if !e.codexShouldUseChatCompletionsBridge(auth, baseURL) {
		return false
	}
	if e.codexShouldUseClaudeMessagesBridge(auth, baseURL) {
		return false
	}
	if protocols, hasExplicit := codexExecutorExplicitProtocols(auth); hasExplicit {
		if protocols["responses"] && !protocols["chat/completions"] {
			return false
		}
	}
	if codexRequestPrefersNativeResponses(auth, baseURL, opts) {
		return false
	}
	return true
}

func (e *CodexExecutor) codexShouldUseClaudeMessagesBridge(auth *cliproxyauth.Auth, baseURL string) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return false
	}
	_ = baseURL
	if protocols, hasExplicit := codexExecutorExplicitProtocols(auth); hasExplicit {
		return protocols["claude-messages"]
	}
	return false
}

func (e *CodexExecutor) codexShouldRelayOpenAIResponses(auth *cliproxyauth.Auth, baseURL string, opts cliproxyexecutor.Options) bool {
	if strings.EqualFold(strings.TrimSpace(opts.Alt), "responses/compact") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(string(opts.SourceFormat)), "openai-response") {
		return false
	}
	return codexNativeResponsesSupported(auth, baseURL)
}

func codexRequestPrefersNativeResponses(auth *cliproxyauth.Auth, baseURL string, opts cliproxyexecutor.Options) bool {
	if auth == nil {
		return false
	}
	if codexShouldForceResponsesBridge(auth, baseURL) {
		return false
	}
	if cliproxyauth.CodexRequestProtocol(opts) == "responses" {
		return codexNativeResponsesSupported(auth, baseURL)
	}
	return false
}

func codexNativeResponsesSupported(auth *cliproxyauth.Auth, baseURL string) bool {
	if auth == nil {
		return false
	}
	if codexShouldForceResponsesBridge(auth, baseURL) {
		return false
	}
	if protocols, hasExplicit := codexExecutorExplicitProtocols(auth); hasExplicit {
		return protocols["responses"]
	}
	return true
}

func codexExecutorExplicitProtocols(auth *cliproxyauth.Auth) (map[string]bool, bool) {
	if auth == nil {
		return nil, false
	}

	rawValues := []string{}
	if auth.Attributes != nil {
		for _, key := range []string{"supported_protocols", "protocols"} {
			if raw := strings.TrimSpace(auth.Attributes[key]); raw != "" {
				rawValues = append(rawValues, raw)
			}
		}
	}
	if auth.Metadata != nil {
		for _, key := range []string{"supported_protocols", "protocols"} {
			if raw, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(raw) != "" {
				rawValues = append(rawValues, raw)
			}
		}
	}
	if len(rawValues) == 0 {
		return codexExecutorInferredProtocols(auth)
	}

	protocols := map[string]bool{}
	for _, raw := range rawValues {
		for _, token := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == '|' || r == ' '
		}) {
			switch strings.ToLower(strings.TrimSpace(token)) {
			case "responses", "/responses", "responses/compact", "/responses/compact":
				protocols["responses"] = true
			case "chat", "chat/completions", "/chat/completions", "chat-completions":
				protocols["chat/completions"] = true
			case "messages", "/messages", "claude-messages", "claude/messages", "anthropic/messages":
				protocols["claude-messages"] = true
			}
		}
	}
	if len(protocols) == 0 {
		return codexExecutorInferredProtocols(auth)
	}
	return protocols, true
}

func codexExecutorInferredProtocols(auth *cliproxyauth.Auth) (map[string]bool, bool) {
	if auth == nil {
		return nil, false
	}

	matchesNowcoding := func(value string) bool {
		normalized := strings.ToLower(strings.TrimSpace(value))
		return normalized == "nowcoding" || strings.Contains(normalized, "nowcoding.ai")
	}

	candidates := []string{auth.Provider, auth.Prefix, auth.Label}
	if auth.Attributes != nil {
		candidates = append(candidates,
			auth.Attributes["prefix"],
			auth.Attributes["name"],
			auth.Attributes["base_url"],
		)
	}
	if auth.Metadata != nil {
		for _, key := range []string{"prefix", "name", "base_url"} {
			if raw, ok := auth.Metadata[key].(string); ok {
				candidates = append(candidates, raw)
			}
		}
	}
	for _, candidate := range candidates {
		if matchesNowcoding(candidate) {
			return map[string]bool{"claude-messages": true}, true
		}
	}
	return nil, false
}

func codexShouldForceResponsesBridge(auth *cliproxyauth.Auth, baseURL string) bool {
	_ = baseURL
	if protocols, hasExplicit := codexExecutorExplicitProtocols(auth); hasExplicit {
		return !protocols["responses"]
	}
	return false
}

func (e *CodexExecutor) codexRetryAttempts(auth *cliproxyauth.Auth, baseURL string) int {
	retry := 0
	if e != nil && e.cfg != nil {
		retry = e.cfg.RequestRetry
	}
	if auth != nil {
		if override, ok := auth.RequestRetryOverride(); ok {
			retry = override
		}
	}
	if e.codexShouldUseProviderRetry(auth, baseURL) && retry < 2 {
		retry = 2
	}
	if retry < 0 {
		retry = 0
	}
	if retry > 2 {
		retry = 2
	}
	attempts := retry + 1
	if attempts < 1 {
		return 1
	}
	return attempts
}

func (e *CodexExecutor) codexShouldUseProviderRetry(auth *cliproxyauth.Auth, baseURL string) bool {
	isEE := func(v string) bool {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "sub2api", "8200codex", "ee", "aixj", "nowcoding", "gmncode.cn":
			return true
		default:
			return false
		}
	}
	if auth != nil {
		if isEE(auth.Provider) || isEE(auth.Label) {
			return true
		}
		if auth.Attributes != nil {
			if isEE(auth.Attributes["prefix"]) || isEE(auth.Attributes["name"]) {
				return true
			}
		}
	}
	if entry := e.resolveCodexConfig(auth); entry != nil {
		if isEE(entry.Prefix) || isEE(entry.Name) {
			return true
		}
		if strings.Contains(strings.ToLower(strings.TrimSpace(entry.BaseURL)), "ai.last.ee") {
			return true
		}
	}
	normalizedBase := strings.ToLower(strings.TrimSpace(baseURL))
	if normalizedBase == "" && auth != nil && auth.Attributes != nil {
		normalizedBase = strings.ToLower(strings.TrimSpace(auth.Attributes["base_url"]))
	}
	return strings.Contains(normalizedBase, "ai.last.ee")
}

func (e *CodexExecutor) codexShouldUseStreamForNonStreamingResponses(auth *cliproxyauth.Auth, baseURL string) bool {
	switch codexResponsesNonStreamTransport(e, auth) {
	case "sse":
		return true
	case "json":
		return false
	}

	matchesProvider := func(value string) bool {
		normalized := strings.ToLower(strings.TrimSpace(value))
		switch {
		case normalized == "aixj", strings.Contains(normalized, "aixj.vip"):
			return true
		case normalized == "yunyi-codex", strings.Contains(normalized, "cdn1.yunyi.cfd/codex"):
			return true
		case normalized == "soapapi", strings.Contains(normalized, "soapapi.top"):
			return true
		default:
			return false
		}
	}
	if matchesProvider(baseURL) {
		return true
	}
	if auth != nil {
		for _, value := range []string{auth.Provider, auth.Prefix, auth.Label} {
			if matchesProvider(value) {
				return true
			}
		}
		if auth.Attributes != nil {
			for _, key := range []string{"prefix", "name", "base_url"} {
				if matchesProvider(auth.Attributes[key]) {
					return true
				}
			}
		}
	}
	if e != nil {
		if entry := e.resolveCodexConfig(auth); entry != nil {
			return matchesProvider(entry.Prefix) || matchesProvider(entry.Name) || matchesProvider(entry.BaseURL)
		}
	}
	return false
}

func codexResponsesNonStreamTransport(e *CodexExecutor, auth *cliproxyauth.Auth) string {
	normalize := func(raw string) string {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "", "auto":
			return ""
		case "sse", "stream", "event-stream", "text/event-stream":
			return "sse"
		case "json", "nonstream", "non-stream":
			return "json"
		default:
			return ""
		}
	}

	if auth != nil {
		if auth.Attributes != nil {
			for _, key := range []string{"responses_nonstream_transport", "responses_non_stream_transport"} {
				if mode := normalize(auth.Attributes[key]); mode != "" {
					return mode
				}
			}
		}
		if auth.Metadata != nil {
			for _, key := range []string{"responses_nonstream_transport", "responses_non_stream_transport"} {
				if raw, ok := auth.Metadata[key].(string); ok {
					if mode := normalize(raw); mode != "" {
						return mode
					}
				}
			}
		}
	}
	if e != nil {
		if entry := e.resolveCodexConfig(auth); entry != nil {
			if mode := normalize(entry.ResponsesNonStreamTransport); mode != "" {
				return mode
			}
		}
	}
	return ""
}

func codexRetryableStatusCode(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return statusCode >= 500 && statusCode <= 599
	}
}

func codexShouldRetryStatus(statusCode int, body []byte) bool {
	if !codexRetryableStatusCode(statusCode) {
		return false
	}
	if statusCode == http.StatusTooManyRequests && codexQuotaExceededBody(body) {
		return false
	}
	return true
}

func codexQuotaExceededBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(string(body)))
	if text == "" {
		return false
	}
	return strings.Contains(text, "daily_limit_exceeded") ||
		strings.Contains(text, "usage_limit_exceeded") ||
		strings.Contains(text, "monthly_limit_exceeded") ||
		strings.Contains(text, "insufficient_quota") ||
		strings.Contains(text, "billing_hard_limit_reached") ||
		strings.Contains(text, "quota exceeded")
}

func codexRetryableRequestError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

func codexRetryableSemanticError(err error) bool {
	if err == nil {
		return false
	}
	var se statusErr
	if !errors.As(err, &se) {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(se.msg))
	switch se.code {
	case http.StatusBadGateway:
		return strings.Contains(msg, "invalid upstream response")
	case http.StatusRequestTimeout:
		return strings.Contains(msg, "response.completed") ||
			strings.Contains(msg, "stream disconnected before completion") ||
			strings.Contains(msg, "stream closed before response.completed")
	default:
		return false
	}
}

func codexRetryDelay(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if wait := parseRetryAfterHeader(resp.Header.Get("Retry-After")); wait != nil && *wait > 0 {
			return *wait
		}
	}
	if attempt < 1 {
		attempt = 1
	}
	// Short capped backoff to avoid long user-facing latency on unstable upstreams.
	wait := time.Duration(attempt*attempt) * 300 * time.Millisecond
	if wait > 2*time.Second {
		wait = 2 * time.Second
	}
	return wait
}

func codexWaitRetry(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseRetryAfterHeader(raw string) *time.Duration {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if seconds, err := strconv.Atoi(trimmed); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		d := time.Duration(seconds) * time.Second
		return &d
	}
	if when, err := http.ParseTime(trimmed); err == nil {
		d := time.Until(when)
		if d < 0 {
			d = 0
		}
		return &d
	}
	return nil
}
