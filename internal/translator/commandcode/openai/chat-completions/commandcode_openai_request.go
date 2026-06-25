package chat_completions

import (
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIRequestToCommandCode converts an OpenAI Chat Completions request (raw JSON)
// into a complete Command Code CLI request JSON.
func ConvertOpenAIRequestToCommandCode(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	// Base envelope for Command Code CLI
	outStr := `{
		"config": {
			"workingDir": "/home/deicer/SimX5/courier-iot-integrations",
			"date": "2026-06-25",
			"environment": "linux-x64, Node.js v24.17.0",
			"structure": [],
			"isGitRepo": false,
			"currentBranch": "",
			"mainBranch": "",
			"gitStatus": "",
			"recentCommits": []
		},
		"memory": null,
		"taste": null,
		"skills": null,
		"params": {
			"model": "deepseek/deepseek-v4-pro",
			"messages": [],
			"tools": [],
			"system": "",
			"max_tokens": 64000,
			"temperature": 0.3,
			"stream": true
		},
		"threadId": ""
	}`

	// Set workingDir from env or fallback
	workingDir := os.Getenv("CC_WORKING_DIR")
	if workingDir == "" {
		workingDir = "/courier-iot-integrations"
	}
	outStr, _ = sjson.Set(outStr, "config.workingDir", workingDir)
	outStr, _ = sjson.Set(outStr, "config.date", time.Now().Format("2006-01-02"))

	// Determine environment
	env := os.Getenv("CC_ENVIRONMENT")
	if env == "" {
		env = "linux-x64, Node.js v24.17.0"
	}
	outStr, _ = sjson.Set(outStr, "config.environment", env)

	// Set parameters
	outStr, _ = sjson.Set(outStr, "params.model", modelName)

	if maxTokens := gjson.GetBytes(rawJSON, "max_tokens"); maxTokens.Exists() && maxTokens.Type == gjson.Number {
		outStr, _ = sjson.Set(outStr, "params.max_tokens", maxTokens.Int())
	}
	if temp := gjson.GetBytes(rawJSON, "temperature"); temp.Exists() && temp.Type == gjson.Number {
		outStr, _ = sjson.Set(outStr, "params.temperature", temp.Num)
	}
	stream := true
	if strVal := gjson.GetBytes(rawJSON, "stream"); strVal.Exists() {
		stream = strVal.Bool()
	}
	outStr, _ = sjson.Set(outStr, "params.stream", stream)

	// Process messages
	messages := gjson.GetBytes(rawJSON, "messages")
	var systemPrompt strings.Builder
	var ccMessages []interface{}

	if messages.IsArray() {
		msgArray := messages.Array()

		// Get paired tool call IDs to filter out incomplete tool calls
		pairedToolCalls := getPairedToolCallIDs(msgArray)

		// Thread ID based on first user message
		threadID := ""
		for _, m := range msgArray {
			if m.Get("role").String() == "user" {
				c := m.Get("content").String()
				if c != "" {
					u := uuid.NewSHA1(uuid.NameSpaceDNS, []byte(c))
					threadID = u.String()
					break
				}
			}
		}
		if threadID == "" {
			threadID = uuid.New().String()
		}
		outStr, _ = sjson.Set(outStr, "threadId", threadID)

		for i, m := range msgArray {
			role := m.Get("role").String()
			content := m.Get("content")

			if role == "system" || role == "developer" {
				if content.Type == gjson.String {
					if systemPrompt.Len() > 0 {
						systemPrompt.WriteString("\n")
					}
					systemPrompt.WriteString(content.String())
				} else if content.IsArray() {
					for _, part := range content.Array() {
						if part.Get("type").String() == "text" {
							if systemPrompt.Len() > 0 {
								systemPrompt.WriteString("\n")
							}
							systemPrompt.WriteString(part.Get("text").String())
						}
					}
				}
			} else if role == "user" {
				userContent := ""
				if content.Type == gjson.String {
					userContent = content.String()
				} else if content.IsArray() {
					var parts []string
					for _, part := range content.Array() {
						if part.Get("type").String() == "text" {
							parts = append(parts, part.Get("text").String())
						}
					}
					userContent = strings.Join(parts, "\n")
				}
				ccMessages = append(ccMessages, map[string]interface{}{
					"role":    "user",
					"content": userContent,
				})
			} else if role == "assistant" {
				var contentParts []interface{}

				// 1. Check reasoning_content / thoughts
				reasoning := m.Get("reasoning_content")
				if reasoning.Exists() && reasoning.String() != "" {
					contentParts = append(contentParts, map[string]interface{}{
						"type": "reasoning",
						"text": reasoning.String(),
					})
				}

				// 2. Text content
				if content.Type == gjson.String && content.String() != "" {
					contentParts = append(contentParts, map[string]interface{}{
						"type": "text",
						"text": content.String(),
					})
				} else if content.IsArray() {
					for _, part := range content.Array() {
						if part.Get("type").String() == "text" {
							contentParts = append(contentParts, map[string]interface{}{
								"type": "text",
								"text": part.Get("text").String(),
							})
						}
					}
				}

				// 3. Tool calls (only if paired with a result)
				toolCalls := m.Get("tool_calls")
				if toolCalls.IsArray() {
					for _, tc := range toolCalls.Array() {
						if tc.Get("type").String() == "function" {
							tcID := tc.Get("id").String()
							if !pairedToolCalls[tcID] {
								continue
							}
							argsStr := tc.Get("function.arguments").String()
							var argsObj interface{}
							if gjson.Valid(argsStr) {
								argsObj = gjson.Parse(argsStr).Value()
							} else {
								argsObj = map[string]interface{}{}
							}

							contentParts = append(contentParts, map[string]interface{}{
								"type":       "tool-call",
								"toolCallId": tcID,
								"toolName":   tc.Get("function.name").String(),
								"input":      argsObj,
							})
						}
					}
				}

				if len(contentParts) > 0 {
					ccMessages = append(ccMessages, map[string]interface{}{
						"role":    "assistant",
						"content": contentParts,
					})
				}
			} else if role == "tool" {
				toolCallID := m.Get("tool_call_id").String()
				if !pairedToolCalls[toolCallID] {
					continue
				}
				toolName := findToolName(msgArray[:i], toolCallID)
				if toolName == "" {
					toolName = "tool"
				}

				toolContent := ""
				if content.Type == gjson.String {
					toolContent = content.String()
				} else {
					toolContent = content.Raw
				}

				ccMessages = append(ccMessages, map[string]interface{}{
					"role": "tool",
					"content": []interface{}{
						map[string]interface{}{
							"type":       "tool-result",
							"toolCallId": toolCallID,
							"toolName":   toolName,
							"output": map[string]interface{}{
								"type":  "text",
								"value": toolContent,
							},
						},
					},
				})
			}
		}
	}

	outStr, _ = sjson.Set(outStr, "params.messages", ccMessages)
	outStr, _ = sjson.Set(outStr, "params.system", systemPrompt.String())

	// Translate tools
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() {
		var ccTools []interface{}
		for _, t := range tools.Array() {
			name := t.Get("function.name").String()
			desc := t.Get("function.description").String()
			schema := t.Get("function.parameters")

			ccTools = append(ccTools, map[string]interface{}{
				"type":         "function",
				"name":         name,
				"description":  desc,
				"input_schema": schema.Value(),
			})
		}
		outStr, _ = sjson.Set(outStr, "params.tools", ccTools)
	}

	return []byte(outStr)
}

func getPairedToolCallIDs(messages []gjson.Result) map[string]bool {
	callIDs := make(map[string]bool)
	resultIDs := make(map[string]bool)
	for _, message := range messages {
		role := message.Get("role").String()
		if role == "assistant" {
			tcs := message.Get("tool_calls")
			if tcs.IsArray() {
				for _, tc := range tcs.Array() {
					if id := tc.Get("id").String(); id != "" {
						callIDs[id] = true
					}
				}
			}
		} else if role == "tool" {
			if id := message.Get("tool_call_id").String(); id != "" {
				resultIDs[id] = true
			}
		}
	}
	paired := make(map[string]bool)
	for id := range callIDs {
		if resultIDs[id] {
			paired[id] = true
		}
	}
	return paired
}

func findToolName(messages []gjson.Result, targetID string) string {
	for _, msg := range messages {
		if msg.Get("role").String() == "assistant" {
			tcs := msg.Get("tool_calls")
			if tcs.IsArray() {
				for _, tc := range tcs.Array() {
					if tc.Get("id").String() == targetID {
						return tc.Get("function.name").String()
					}
				}
			}
		}
	}
	return ""
}
