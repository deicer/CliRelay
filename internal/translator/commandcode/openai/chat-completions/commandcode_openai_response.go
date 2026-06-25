package chat_completions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type commandCodeOpenAIResponseState struct {
	ID            string
	Created       int64
	ToolCallCount int
}

// ConvertCommandCodeResponseToOpenAI translates a single SSE chunk from Command Code CLI format to OpenAI format.
func ConvertCommandCodeResponseToOpenAI(_ context.Context, modelName string, _, _, rawJSON []byte, param *any) []string {
	if *param == nil {
		*param = &commandCodeOpenAIResponseState{
			ID:            "chatcmpl-" + uuid.New().String(),
			Created:       time.Now().Unix(),
			ToolCallCount: 0,
		}
	}
	state := (*param).(*commandCodeOpenAIResponseState)

	if string(rawJSON) == "[DONE]" {
		return []string{}
	}

	typeResult := gjson.GetBytes(rawJSON, "type")
	if !typeResult.Exists() {
		return nil
	}

	eventType := typeResult.String()

	// Base OpenAI template
	template := `{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":null}]}`
	template, _ = sjson.Set(template, "id", state.ID)
	template, _ = sjson.Set(template, "created", state.Created)
	template, _ = sjson.Set(template, "model", modelName)

	switch eventType {
	case "text-delta":
		text := gjson.GetBytes(rawJSON, "text").String()
		template, _ = sjson.Set(template, "choices.0.delta.content", text)
		return []string{template}

	case "reasoning-delta":
		text := gjson.GetBytes(rawJSON, "text").String()
		template, _ = sjson.Set(template, "choices.0.delta.reasoning_content", text)
		return []string{template}

	case "tool-call":
		tcID := gjson.GetBytes(rawJSON, "toolCallId").String()
		tcName := gjson.GetBytes(rawJSON, "toolName").String()
		tcArgs := gjson.GetBytes(rawJSON, "arguments")

		argsStr := "{}"
		if tcArgs.Exists() {
			if tcArgs.Type == gjson.JSON {
				argsStr = tcArgs.Raw
			} else {
				argsStr = tcArgs.String()
			}
		}

		toolCallTemplate := `{"id":"","index":0,"type":"function","function":{"name":"","arguments":""}}`
		toolCallTemplate, _ = sjson.Set(toolCallTemplate, "id", tcID)
		toolCallTemplate, _ = sjson.Set(toolCallTemplate, "index", state.ToolCallCount)
		toolCallTemplate, _ = sjson.Set(toolCallTemplate, "function.name", tcName)
		toolCallTemplate, _ = sjson.Set(toolCallTemplate, "function.arguments", argsStr)

		state.ToolCallCount++

		template, _ = sjson.SetRaw(template, "choices.0.delta.tool_calls", fmt.Sprintf("[%s]", toolCallTemplate))
		return []string{template}

	case "finish":
		finishReason := gjson.GetBytes(rawJSON, "finishReason").String()
		if finishReason == "tool-calls" {
			finishReason = "tool_calls"
		} else if finishReason == "" {
			finishReason = "stop"
		}

		template, _ = sjson.Set(template, "choices.0.finish_reason", finishReason)
		template, _ = sjson.Delete(template, "choices.0.delta")

		// Usage info
		usage := gjson.GetBytes(rawJSON, "totalUsage")
		if usage.Exists() {
			inTokens := usage.Get("inputTokens").Int()
			outTokens := usage.Get("outputTokens").Int()
			cachedTokens := usage.Get("inputTokenDetails.cacheReadTokens").Int()

			template, _ = sjson.Set(template, "usage.prompt_tokens", inTokens)
			template, _ = sjson.Set(template, "usage.completion_tokens", outTokens)
			template, _ = sjson.Set(template, "usage.total_tokens", inTokens+outTokens)
			if cachedTokens > 0 {
				template, _ = sjson.Set(template, "usage.prompt_tokens_details.cached_tokens", cachedTokens)
			}
		}

		return []string{template}
	}

	return nil
}

// ConvertCommandCodeResponseToOpenAINonStream translates a non-streaming response from Command Code CLI format to OpenAI format.
func ConvertCommandCodeResponseToOpenAINonStream(ctx context.Context, modelName string, _, _, rawJSON []byte, _ *any) string {
	// Parse the response
	parsed := gjson.ParseBytes(rawJSON)

	// Build OpenAI non-streaming response
	out := `{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`

	id := parsed.Get("threadId").String()
	if id == "" {
		id = "chatcmpl-" + uuid.New().String()
	}
	out, _ = sjson.Set(out, "id", id)
	out, _ = sjson.Set(out, "created", time.Now().Unix())
	out, _ = sjson.Set(out, "model", modelName)

	var textBuilder strings.Builder
	var reasoningBuilder strings.Builder
	var toolCalls []interface{}

	// If Command Code CLI response contains a "content" array
	contentResult := parsed.Get("content")
	if !contentResult.Exists() {
		// Maybe the payload is just a simple JSON representation of the stream end?
		// Or maybe it's nested under another key? Let's check "response"
		resp := parsed.Get("response")
		if resp.Exists() {
			contentResult = resp.Get("content")
		}
	}

	if contentResult.IsArray() {
		for i, part := range contentResult.Array() {
			partType := part.Get("type").String()
			switch partType {
			case "text":
				textBuilder.WriteString(part.Get("text").String())
			case "reasoning":
				reasoningBuilder.WriteString(part.Get("text").String())
			case "tool-call":
				tcArgs := part.Get("input")
				argsStr := "{}"
				if tcArgs.Exists() {
					if tcArgs.Type == gjson.JSON {
						argsStr = tcArgs.Raw
					} else {
						argsStr = tcArgs.String()
					}
				}
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":    part.Get("toolCallId").String(),
					"index": i,
					"type":  "function",
					"function": map[string]string{
						"name":      part.Get("toolName").String(),
						"arguments": argsStr,
					},
				})
			}
		}
	} else if parsed.Get("text").Exists() {
		// Alternative simple string response
		textBuilder.WriteString(parsed.Get("text").String())
	}

	out, _ = sjson.Set(out, "choices.0.message.content", textBuilder.String())

	if reasoningBuilder.Len() > 0 {
		out, _ = sjson.Set(out, "choices.0.message.reasoning_content", reasoningBuilder.String())
	}

	if len(toolCalls) > 0 {
		tcBytes, _ := json.Marshal(toolCalls)
		out, _ = sjson.SetRaw(out, "choices.0.message.tool_calls", string(tcBytes))
		out, _ = sjson.Set(out, "choices.0.finish_reason", "tool_calls")
	} else {
		finish := parsed.Get("finishReason").String()
		if finish == "" {
			finish = "stop"
		}
		out, _ = sjson.Set(out, "choices.0.finish_reason", finish)
	}

	// Usage
	usage := parsed.Get("totalUsage")
	if !usage.Exists() {
		usage = parsed.Get("usage")
	}
	if usage.Exists() {
		inTokens := usage.Get("inputTokens").Int()
		if inTokens == 0 {
			inTokens = usage.Get("prompt_tokens").Int()
		}
		outTokens := usage.Get("outputTokens").Int()
		if outTokens == 0 {
			outTokens = usage.Get("completion_tokens").Int()
		}
		cachedTokens := usage.Get("inputTokenDetails.cacheReadTokens").Int()

		out, _ = sjson.Set(out, "usage.prompt_tokens", inTokens)
		out, _ = sjson.Set(out, "usage.completion_tokens", outTokens)
		out, _ = sjson.Set(out, "usage.total_tokens", inTokens+outTokens)
		if cachedTokens > 0 {
			out, _ = sjson.Set(out, "usage.prompt_tokens_details.cached_tokens", cachedTokens)
		}
	}

	return out
}
