// Package openai provides response translation functionality for Gemini CLI to OpenAI API compatibility.
// This package handles the conversion of Gemini CLI API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIResponseToOpenAI translates a single chunk of a streaming response from the
// Gemini CLI API format to the OpenAI Chat Completions streaming format.
// It processes various Gemini CLI event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini CLI API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - []string: A slice of strings, each containing an OpenAI-compatible JSON response
func ConvertOpenAIResponseToOpenAI(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []string {
	_ = requestRawJSON
	_ = param
	requestedModel := requestModelForCompatibility(modelName, originalRequestRawJSON)
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		trimmed := bytes.TrimSpace(rawJSON[5:])
		if bytes.Equal(trimmed, []byte("[DONE]")) {
			return []string{}
		}
		return []string{string(normalizeOpenAICompatResponse(trimmed, requestedModel))}
	}
	if bytes.Equal(bytes.TrimSpace(rawJSON), []byte("[DONE]")) {
		return []string{}
	}
	return []string{string(normalizeOpenAICompatResponse(rawJSON, requestedModel))}
}

// ConvertOpenAIResponseToOpenAINonStream converts a non-streaming Gemini CLI response to a non-streaming OpenAI response.
// This function processes the complete Gemini CLI response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Gemini CLI API
//   - param: A pointer to a parameter object for the conversion
//
// Returns:
//   - string: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertOpenAIResponseToOpenAINonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) string {
	_ = ctx
	_ = requestRawJSON
	_ = param
	return string(normalizeOpenAICompatResponse(rawJSON, requestModelForCompatibility(modelName, originalRequestRawJSON)))
}

func requestModelForCompatibility(modelName string, originalRequestRawJSON []byte) string {
	requested := strings.TrimSpace(gjson.GetBytes(originalRequestRawJSON, "model").String())
	if requested == "" {
		return strings.TrimSpace(modelName)
	}
	if slash := strings.LastIndex(requested, "/"); slash >= 0 && slash < len(requested)-1 {
		return strings.TrimSpace(requested[slash+1:])
	}
	return requested
}

func normalizeOpenAICompatResponse(rawJSON []byte, requestedModel string) []byte {
	if len(rawJSON) == 0 || !gjson.ValidBytes(rawJSON) {
		return rawJSON
	}

	out := append([]byte(nil), rawJSON...)
	if requestedModel != "" {
		out, _ = sjson.SetBytes(out, "model", requestedModel)
	}

	if !requestedModelAllowsThinking(requestedModel) {
		choices := gjson.GetBytes(out, "choices")
		if choices.Exists() && choices.IsArray() {
			for idx := range choices.Array() {
				out, _ = sjson.DeleteBytes(out, "choices."+strconv.Itoa(idx)+".message.reasoning_content")
				out, _ = sjson.DeleteBytes(out, "choices."+strconv.Itoa(idx)+".message.reasoning")
				out, _ = sjson.DeleteBytes(out, "choices."+strconv.Itoa(idx)+".delta.reasoning_content")
				out, _ = sjson.DeleteBytes(out, "choices."+strconv.Itoa(idx)+".delta.reasoning")
			}
		}
	}

	return out
}

func requestedModelAllowsThinking(modelName string) bool {
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
