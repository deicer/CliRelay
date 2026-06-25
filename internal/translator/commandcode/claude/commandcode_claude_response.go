package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type commandCodeClaudeResponseState struct {
	HasFirstResponse bool
	ResponseType     int // 0=none, 1=text, 2=reasoning, 3=tool
	ResponseIndex    int
	ID               string
	HasToolUse       bool
	HasContent       bool
}

// ConvertCommandCodeResponseToClaude translates SSE events from Command Code to Claude SSE format.
func ConvertCommandCodeResponseToClaude(_ context.Context, modelName string, _, _, rawJSON []byte, param *any) []string {
	if *param == nil {
		*param = &commandCodeClaudeResponseState{
			HasFirstResponse: false,
			ResponseType:     0,
			ResponseIndex:    0,
			ID:               "msg_" + uuid.New().String(),
		}
	}
	state := (*param).(*commandCodeClaudeResponseState)

	if string(rawJSON) == "[DONE]" {
		var output strings.Builder
		if state.HasContent {
			if state.ResponseType != 0 {
				output.WriteString("event: content_block_stop\n")
				output.WriteString(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, state.ResponseIndex))
				output.WriteString("\n\n\n")
				state.ResponseType = 0
			}
			stopReason := "end_turn"
			if state.HasToolUse {
				stopReason = "tool_use"
			}
			output.WriteString("event: message_delta\n")
			output.WriteString(fmt.Sprintf(`data: {"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"output_tokens":0}}`, stopReason))
			output.WriteString("\n\n\n")
			output.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n\n")
			return []string{output.String()}
		}
		return []string{}
	}

	typeResult := gjson.GetBytes(rawJSON, "type")
	if !typeResult.Exists() {
		return nil
	}

	eventType := typeResult.String()
	var output strings.Builder

	// Initialize streaming session with message_start
	if !state.HasFirstResponse {
		output.WriteString("event: message_start\n")
		msgStart := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","content":[],"model":"","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`
		msgStart, _ = sjson.Set(msgStart, "message.id", state.ID)
		msgStart, _ = sjson.Set(msgStart, "message.model", modelName)

		// Set usage if exists in rawJSON (some endpoints send usage early or we just wait)
		usage := gjson.GetBytes(rawJSON, "totalUsage")
		if usage.Exists() {
			msgStart, _ = sjson.Set(msgStart, "message.usage.input_tokens", usage.Get("inputTokens").Int())
			msgStart, _ = sjson.Set(msgStart, "message.usage.output_tokens", usage.Get("outputTokens").Int())
		}
		output.WriteString(fmt.Sprintf("data: %s\n\n\n", msgStart))
		state.HasFirstResponse = true
	}

	switch eventType {
	case "text-delta":
		text := gjson.GetBytes(rawJSON, "text").String()
		if text == "" {
			return nil
		}

		if state.ResponseType != 1 {
			if state.ResponseType != 0 {
				output.WriteString("event: content_block_stop\n")
				output.WriteString(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, state.ResponseIndex))
				output.WriteString("\n\n\n")
				state.ResponseIndex++
			}
			output.WriteString("event: content_block_start\n")
			output.WriteString(fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, state.ResponseIndex))
			output.WriteString("\n\n\n")
			state.ResponseType = 1
		}

		output.WriteString("event: content_block_delta\n")
		delta, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, state.ResponseIndex), "delta.text", text)
		output.WriteString(fmt.Sprintf("data: %s\n\n\n", delta))
		state.HasContent = true
		return []string{output.String()}

	case "reasoning-delta":
		text := gjson.GetBytes(rawJSON, "text").String()
		if text == "" {
			return nil
		}

		if state.ResponseType != 2 {
			if state.ResponseType != 0 {
				output.WriteString("event: content_block_stop\n")
				output.WriteString(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, state.ResponseIndex))
				output.WriteString("\n\n\n")
				state.ResponseIndex++
			}
			output.WriteString("event: content_block_start\n")
			output.WriteString(fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, state.ResponseIndex))
			output.WriteString("\n\n\n")
			state.ResponseType = 2
		}

		output.WriteString("event: content_block_delta\n")
		delta, _ := sjson.Set(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`, state.ResponseIndex), "delta.thinking", text)
		output.WriteString(fmt.Sprintf("data: %s\n\n\n", delta))
		state.HasContent = true
		return []string{output.String()}

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

		state.HasToolUse = true

		if state.ResponseType != 3 {
			if state.ResponseType != 0 {
				output.WriteString("event: content_block_stop\n")
				output.WriteString(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, state.ResponseIndex))
				output.WriteString("\n\n\n")
				state.ResponseIndex++
			}
			output.WriteString("event: content_block_start\n")
			tcBlock := fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"%s","name":"%s","input":{}}}`, state.ResponseIndex, tcID, tcName)
			output.WriteString(fmt.Sprintf("data: %s\n\n\n", tcBlock))
			state.ResponseType = 3
		}

		output.WriteString("event: content_block_delta\n")
		delta := fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`, state.ResponseIndex)
		delta, _ = sjson.Set(delta, "delta.partial_json", argsStr)
		output.WriteString(fmt.Sprintf("data: %s\n\n\n", delta))
		state.HasContent = true
		return []string{output.String()}

	case "finish":
		if state.ResponseType != 0 {
			output.WriteString("event: content_block_stop\n")
			output.WriteString(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, state.ResponseIndex))
			output.WriteString("\n\n\n")
			state.ResponseType = 0
		}

		stopReason := gjson.GetBytes(rawJSON, "finishReason").String()
		if stopReason == "tool-calls" || state.HasToolUse {
			stopReason = "tool_use"
		} else if stopReason == "" {
			stopReason = "end_turn"
		}

		inTokens := int64(0)
		outTokens := int64(0)
		cachedTokens := int64(0)

		usage := gjson.GetBytes(rawJSON, "totalUsage")
		if usage.Exists() {
			inTokens = usage.Get("inputTokens").Int()
			outTokens = usage.Get("outputTokens").Int()
			cachedTokens = usage.Get("inputTokenDetails.cacheReadTokens").Int()
		}

		output.WriteString("event: message_delta\n")
		delta := fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"%s","stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`, stopReason, inTokens, outTokens)
		if cachedTokens > 0 {
			delta, _ = sjson.Set(delta, "usage.cache_read_input_tokens", cachedTokens)
		}
		output.WriteString(fmt.Sprintf("data: %s\n\n\n", delta))
		output.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n\n")
		state.HasContent = true
		return []string{output.String()}
	}

	return nil
}

// ConvertCommandCodeResponseToClaudeNonStream converts a non-streaming Command Code CLI response to a Claude response.
func ConvertCommandCodeResponseToClaudeNonStream(_ context.Context, modelName string, _, _, rawJSON []byte, _ *any) string {
	parsed := gjson.ParseBytes(rawJSON)

	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`

	id := parsed.Get("threadId").String()
	if id == "" {
		id = "msg_" + uuid.New().String()
	}
	out, _ = sjson.Set(out, "id", id)
	out, _ = sjson.Set(out, "model", modelName)

	hasToolCall := false

	contentResult := parsed.Get("content")
	if !contentResult.Exists() {
		resp := parsed.Get("response")
		if resp.Exists() {
			contentResult = resp.Get("content")
		}
	}

	if contentResult.IsArray() {
		for _, part := range contentResult.Array() {
			partType := part.Get("type").String()
			switch partType {
			case "text":
				block := map[string]string{
					"type": "text",
					"text": part.Get("text").String(),
				}
				out, _ = sjson.SetRaw(out, "content.-1", string(mustMarshal(block)))
			case "reasoning":
				block := map[string]string{
					"type":     "thinking",
					"thinking": part.Get("text").String(),
				}
				out, _ = sjson.SetRaw(out, "content.-1", string(mustMarshal(block)))
			case "tool-call":
				hasToolCall = true
				args := part.Get("input").Value()
				block := map[string]interface{}{
					"type":  "tool_use",
					"id":    part.Get("toolCallId").String(),
					"name":  part.Get("toolName").String(),
					"input": args,
				}
				out, _ = sjson.SetRaw(out, "content.-1", string(mustMarshal(block)))
			}
		}
	} else if parsed.Get("text").Exists() {
		block := map[string]string{
			"type": "text",
			"text": parsed.Get("text").String(),
		}
		out, _ = sjson.SetRaw(out, "content.-1", string(mustMarshal(block)))
	}

	stopReason := "end_turn"
	if hasToolCall {
		stopReason = "tool_use"
	} else {
		finish := parsed.Get("finishReason").String()
		if finish == "stop" || finish == "" {
			stopReason = "end_turn"
		} else {
			stopReason = finish
		}
	}
	out, _ = sjson.Set(out, "stop_reason", stopReason)

	// Usage
	usage := parsed.Get("totalUsage")
	if !usage.Exists() {
		usage = parsed.Get("usage")
	}
	if usage.Exists() {
		inTokens := usage.Get("inputTokens").Int()
		if inTokens == 0 {
			inTokens = usage.Get("input_tokens").Int()
		}
		outTokens := usage.Get("outputTokens").Int()
		if outTokens == 0 {
			outTokens = usage.Get("output_tokens").Int()
		}
		cachedTokens := usage.Get("inputTokenDetails.cacheReadTokens").Int()

		out, _ = sjson.Set(out, "usage.input_tokens", inTokens)
		out, _ = sjson.Set(out, "usage.output_tokens", outTokens)
		if cachedTokens > 0 {
			out, _ = sjson.Set(out, "usage.cache_read_input_tokens", cachedTokens)
		}
	}

	return out
}

func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
