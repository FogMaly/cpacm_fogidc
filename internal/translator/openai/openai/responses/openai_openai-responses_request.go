package responses

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIResponsesRequestToOpenAIChatCompletions converts OpenAI responses format to OpenAI chat completions format.
// It transforms the OpenAI responses API format (with instructions and input array) into the standard
// OpenAI chat completions format (with messages array and system content).
//
// The conversion handles:
// 1. Model name and streaming configuration
// 2. Instructions to system message conversion
// 3. Input array to messages array transformation
// 4. Tool definitions and tool choice conversion
// 5. Function calls and function results handling
// 6. Generation parameters mapping (max_tokens, reasoning, etc.)
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data in OpenAI responses format
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI chat completions format
func ConvertOpenAIResponsesRequestToOpenAIChatCompletions(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Base OpenAI chat completions template with default values
	out := `{"model":"","messages":[],"stream":false}`

	root := gjson.ParseBytes(rawJSON)

	// Set model name
	out, _ = sjson.Set(out, "model", modelName)

	// Set stream configuration
	out, _ = sjson.Set(out, "stream", stream)

	// Map generation parameters from responses format to chat completions format
	if maxTokens := root.Get("max_output_tokens"); maxTokens.Exists() {
		out, _ = sjson.Set(out, "max_tokens", maxTokens.Int())
	}

	if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Exists() {
		out, _ = sjson.Set(out, "parallel_tool_calls", parallelToolCalls.Bool())
	}

	// Convert instructions to system message
	if instructions := root.Get("instructions"); instructions.Exists() {
		systemMessage := `{"role":"system","content":""}`
		systemMessage, _ = sjson.Set(systemMessage, "content", instructions.String())
		out, _ = sjson.SetRaw(out, "messages.-1", systemMessage)
	}

	// Convert input array to messages
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		items := trimResponsesAssistantOnlyTailAfterLastUser(input.Array())
		for i := 0; i < len(items); i++ {
			item := items[i]
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}

			switch itemType {
			case "message", "":
				// Handle regular message conversion
				role := item.Get("role").String()
				if role == "developer" {
					role = "user"
				}
				message := `{"role":"","content":[]}`
				message, _ = sjson.Set(message, "role", role)

				if content := item.Get("content"); content.Exists() && content.IsArray() {
					var messageContent string
					var toolCalls []interface{}

					content.ForEach(func(_, contentItem gjson.Result) bool {
						contentType := contentItem.Get("type").String()
						if contentType == "" {
							contentType = "input_text"
						}

						switch contentType {
						case "input_text", "output_text":
							text := strings.TrimSpace(contentItem.Get("text").String())
							if text == "" {
								return true
							}
							contentPart := `{"type":"text","text":""}`
							contentPart, _ = sjson.Set(contentPart, "text", text)
							message, _ = sjson.SetRaw(message, "content.-1", contentPart)
						case "input_image":
							imageURL := contentItem.Get("image_url").String()
							contentPart := `{"type":"image_url","image_url":{"url":""}}`
							contentPart, _ = sjson.Set(contentPart, "image_url.url", imageURL)
							message, _ = sjson.SetRaw(message, "content.-1", contentPart)
						}
						return true
					})

					if messageContent != "" {
						message, _ = sjson.Set(message, "content", messageContent)
					}

					if len(toolCalls) > 0 {
						message, _ = sjson.Set(message, "tool_calls", toolCalls)
					}
				} else if content.Type == gjson.String {
					message, _ = sjson.Set(message, "content", content.String())
				}

				if content := gjson.Get(message, "content"); (content.IsArray() && len(content.Array()) > 0) || (content.Type == gjson.String && strings.TrimSpace(content.String()) != "") || gjson.Get(message, "tool_calls").Exists() {
					out, _ = sjson.SetRaw(out, "messages.-1", message)
				}

			case "function_call":
				if responsesFunctionArgumentsMalformed(item) && responsesNextItemIsMalformedFunctionCallOutput(items, i, item.Get("call_id").String()) {
					i++
					continue
				}

				// Handle function call conversion to assistant message with tool_calls
				assistantMessage := `{"role":"assistant","tool_calls":[]}`

				toolCall := `{"id":"","type":"function","function":{"name":"","arguments":""}}`

				if callId := item.Get("call_id"); callId.Exists() {
					toolCall, _ = sjson.Set(toolCall, "id", callId.String())
				}

				if name := item.Get("name"); name.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.name", name.String())
				}

				if arguments := item.Get("arguments"); arguments.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.arguments", normalizeResponsesFunctionArgumentsForChatCompletions(arguments.String()))
				} else {
					toolCall, _ = sjson.Set(toolCall, "function.arguments", "{}")
				}

				assistantMessage, _ = sjson.SetRaw(assistantMessage, "tool_calls.0", toolCall)
				out, _ = sjson.SetRaw(out, "messages.-1", assistantMessage)

			case "custom_tool_call":
				assistantMessage := `{"role":"assistant","tool_calls":[]}`
				toolCall := `{"id":"","type":"function","function":{"name":"","arguments":""}}`

				if callID := item.Get("call_id"); callID.Exists() {
					toolCall, _ = sjson.Set(toolCall, "id", callID.String())
				}
				if name := item.Get("name"); name.Exists() {
					toolCall, _ = sjson.Set(toolCall, "function.name", name.String())
				}
				toolCall, _ = sjson.Set(toolCall, "function.arguments", responsesWrapCustomToolInput(item.Get("input").String()))

				assistantMessage, _ = sjson.SetRaw(assistantMessage, "tool_calls.0", toolCall)
				out, _ = sjson.SetRaw(out, "messages.-1", assistantMessage)

			case "function_call_output":
				// Handle function call output conversion to tool message
				toolMessage := `{"role":"tool","tool_call_id":"","content":""}`

				if callId := item.Get("call_id"); callId.Exists() {
					toolMessage, _ = sjson.Set(toolMessage, "tool_call_id", callId.String())
				}

				if output := item.Get("output"); output.Exists() {
					toolMessage, _ = sjson.Set(toolMessage, "content", responsesToolOutputToString(output))
				}

				out, _ = sjson.SetRaw(out, "messages.-1", toolMessage)

			case "custom_tool_call_output":
				toolMessage := `{"role":"tool","tool_call_id":"","content":""}`

				if callID := item.Get("call_id"); callID.Exists() {
					toolMessage, _ = sjson.Set(toolMessage, "tool_call_id", callID.String())
				}
				if output := item.Get("output"); output.Exists() {
					toolMessage, _ = sjson.Set(toolMessage, "content", responsesToolOutputToString(output))
				}

				out, _ = sjson.SetRaw(out, "messages.-1", toolMessage)
			}
		}
	} else if input.Type == gjson.String {
		msg := "{}"
		msg, _ = sjson.Set(msg, "role", "user")
		msg, _ = sjson.Set(msg, "content", input.String())
		out, _ = sjson.SetRaw(out, "messages.-1", msg)
	}

	// Convert tools from responses format to chat completions format
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		var chatCompletionsTools []interface{}

		tools.ForEach(func(_, tool gjson.Result) bool {
			// Built-in tools (e.g. {"type":"web_search"}) are already compatible with the Chat Completions schema.
			// Only function tools need structural conversion because Chat Completions nests details under "function".
			toolType := tool.Get("type").String()
			if toolType != "" && toolType != "function" && toolType != "custom" && tool.IsObject() {
				// Almost all providers lack built-in tools, so we just ignore them.
				// chatCompletionsTools = append(chatCompletionsTools, tool.Value())
				return true
			}

			chatTool := `{"type":"function","function":{}}`

			// Convert tool structure from responses format to chat completions format
			function := `{"name":"","description":"","parameters":{}}`

			if name := tool.Get("name"); name.Exists() {
				function, _ = sjson.Set(function, "name", name.String())
			}

			if description := tool.Get("description"); description.Exists() {
				function, _ = sjson.Set(function, "description", description.String())
			}

			if parameters := tool.Get("parameters"); parameters.Exists() {
				function, _ = sjson.SetRaw(function, "parameters", util.SanitizeToolSchemaJSON(parameters.Raw))
			} else if toolType == "custom" {
				function, _ = sjson.SetRaw(function, "parameters", responsesCustomToolSchema())
			}

			chatTool, _ = sjson.SetRaw(chatTool, "function", function)
			chatCompletionsTools = append(chatCompletionsTools, gjson.Parse(chatTool).Value())

			return true
		})

		if len(chatCompletionsTools) > 0 {
			out, _ = sjson.Set(out, "tools", chatCompletionsTools)
		}
	}

	if reasoningEffort := root.Get("reasoning.effort"); reasoningEffort.Exists() {
		effort := strings.ToLower(strings.TrimSpace(reasoningEffort.String()))
		if effort != "" {
			out, _ = sjson.Set(out, "reasoning_effort", effort)
		}
	}

	// Convert tool_choice if present
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		switch {
		case toolChoice.Type == gjson.String:
			out, _ = sjson.Set(out, "tool_choice", toolChoice.String())
		case toolChoice.IsObject():
			switch toolChoice.Get("type").String() {
			case "function", "custom":
				choice := `{"type":"function","function":{"name":""}}`
				name := toolChoice.Get("name").String()
				if name == "" {
					name = toolChoice.Get("function.name").String()
				}
				choice, _ = sjson.Set(choice, "function.name", name)
				out, _ = sjson.SetRaw(out, "tool_choice", choice)
			default:
				out, _ = sjson.Set(out, "tool_choice", toolChoice.String())
			}
		default:
			out, _ = sjson.Set(out, "tool_choice", toolChoice.String())
		}
	}

	return []byte(out)
}

func normalizeResponsesFunctionArgumentsForChatCompletions(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	if gjson.Valid(trimmed) {
		parsed := gjson.Parse(trimmed)
		if parsed.IsObject() || parsed.IsArray() {
			return trimmed
		}
	}

	fallback, err := json.Marshal(map[string]string{
		"_raw_arguments": trimmed,
		"_error":         "arguments were not valid JSON",
	})
	if err != nil {
		return `{"_error":"arguments were not valid JSON"}`
	}
	return string(fallback)
}

func responsesFunctionArgumentsMalformed(item gjson.Result) bool {
	arguments := item.Get("arguments")
	if !arguments.Exists() {
		return false
	}
	trimmed := strings.TrimSpace(arguments.String())
	if trimmed == "" {
		return false
	}
	if !gjson.Valid(trimmed) {
		return true
	}
	parsed := gjson.Parse(trimmed)
	return !parsed.IsObject() && !parsed.IsArray()
}

func responsesNextItemIsMalformedFunctionCallOutput(items []gjson.Result, idx int, callID string) bool {
	if idx+1 >= len(items) || strings.TrimSpace(callID) == "" {
		return false
	}
	next := items[idx+1]
	if next.Get("type").String() != "function_call_output" {
		return false
	}
	if next.Get("call_id").String() != callID {
		return false
	}
	output := strings.TrimSpace(next.Get("output").String())
	return strings.Contains(strings.ToLower(output), "failed to parse function arguments")
}

func trimResponsesAssistantOnlyTailAfterLastUser(items []gjson.Result) []gjson.Result {
	lastUserIdx := -1
	for i, item := range items {
		itemType := item.Get("type").String()
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		if itemType == "" && role != "" {
			itemType = "message"
		}
		if itemType == "message" && (role == "user" || role == "developer" || role == "system") {
			lastUserIdx = i
		}
	}
	if lastUserIdx < 0 || lastUserIdx >= len(items)-1 {
		return items
	}

	for i := lastUserIdx + 1; i < len(items); i++ {
		item := items[i]
		itemType := item.Get("type").String()
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		if itemType == "" && role != "" {
			itemType = "message"
		}

		switch itemType {
		case "reasoning":
			continue
		case "message":
			if role == "assistant" {
				continue
			}
			return items
		case "function_call":
			if responsesFunctionArgumentsMalformed(item) && responsesNextItemIsMalformedFunctionCallOutput(items, i, item.Get("call_id").String()) {
				continue
			}
			return items
		case "function_call_output":
			prevIdx := i - 1
			if prevIdx >= 0 && responsesFunctionArgumentsMalformed(items[prevIdx]) && responsesNextItemIsMalformedFunctionCallOutput(items, prevIdx, items[prevIdx].Get("call_id").String()) {
				continue
			}
			return items
		default:
			return items
		}
	}

	return items[:lastUserIdx+1]
}
