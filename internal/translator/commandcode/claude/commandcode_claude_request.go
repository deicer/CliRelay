package claude

import (
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeRequestToCommandCode parses and transforms a Claude request into Command Code format.
func ConvertClaudeRequestToCommandCode(modelName string, inputRawJSON []byte, _ bool) []byte {
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

	// Process system prompt
	systemResult := gjson.GetBytes(rawJSON, "system")
	systemPrompt := ""
	if systemResult.IsArray() {
		var parts []string
		for _, part := range systemResult.Array() {
			if part.Get("type").String() == "text" {
				parts = append(parts, part.Get("text").String())
			}
		}
		systemPrompt = strings.Join(parts, "\n")
	} else if systemResult.Type == gjson.String {
		systemPrompt = systemResult.String()
	}
	outStr, _ = sjson.Set(outStr, "params.system", systemPrompt)

	// Process messages
	messages := gjson.GetBytes(rawJSON, "messages")
	var ccMessages []interface{}

	if messages.IsArray() {
		msgArray := messages.Array()

		// Get paired tool call IDs to filter out incomplete tool calls
		pairedToolCalls := getClaudePairedToolCallIDs(msgArray)

		// Thread ID based on first user message
		threadID := ""
		for _, m := range msgArray {
			if m.Get("role").String() == "user" {
				content := m.Get("content")
				c := ""
				if content.Type == gjson.String {
					c = content.String()
				} else if content.IsArray() {
					for _, part := range content.Array() {
						if part.Get("type").String() == "text" {
							c = part.Get("text").String()
							break
						}
					}
				}
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

			if role == "user" {
				if content.Type == gjson.String {
					ccMessages = append(ccMessages, map[string]interface{}{
						"role":    "user",
						"content": content.String(),
					})
				} else if content.IsArray() {
					// Check if it's a tool result message or multi-part user message
					var parts []string
					isToolResultMsg := false
					var toolResults []interface{}

					for _, part := range content.Array() {
						partType := part.Get("type").String()
						if partType == "text" {
							parts = append(parts, part.Get("text").String())
						} else if partType == "tool_result" {
							isToolResultMsg = true
							toolCallID := part.Get("tool_use_id").String()
							if !pairedToolCalls[toolCallID] {
								continue
							}
							toolName := findClaudeToolName(msgArray[:i], toolCallID)
							if toolName == "" {
								toolName = "tool"
							}

							val := ""
							rawVal := part.Get("content")
							if rawVal.Type == gjson.String {
								val = rawVal.String()
							} else {
								val = rawVal.Raw
							}

							toolResults = append(toolResults, map[string]interface{}{
								"type":       "tool-result",
								"toolCallId": toolCallID,
								"toolName":   toolName,
								"output": map[string]interface{}{
									"type":  "text",
									"value": val,
								},
							})
						}
					}

					if isToolResultMsg {
						if len(toolResults) > 0 {
							ccMessages = append(ccMessages, map[string]interface{}{
								"role":    "tool",
								"content": toolResults,
							})
						}
					} else {
						ccMessages = append(ccMessages, map[string]interface{}{
							"role":    "user",
							"content": strings.Join(parts, "\n"),
						})
					}
				}
			} else if role == "assistant" {
				var contentParts []interface{}

				if content.Type == gjson.String {
					contentParts = append(contentParts, map[string]interface{}{
						"type": "text",
						"text": content.String(),
					})
				} else if content.IsArray() {
					for _, part := range content.Array() {
						partType := part.Get("type").String()
						switch partType {
						case "text":
							contentParts = append(contentParts, map[string]interface{}{
								"type": "text",
								"text": part.Get("text").String(),
							})
						case "thinking":
							contentParts = append(contentParts, map[string]interface{}{
								"type": "reasoning",
								"text": part.Get("thinking").String(),
							})
						case "tool_use":
							tcID := part.Get("id").String()
							if !pairedToolCalls[tcID] {
								continue
							}
							args := part.Get("input")
							var argsObj interface{}
							if args.IsObject() {
								argsObj = args.Value()
							} else {
								argsObj = map[string]interface{}{}
							}

							contentParts = append(contentParts, map[string]interface{}{
								"type":       "tool-call",
								"toolCallId": tcID,
								"toolName":   part.Get("name").String(),
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
			}
		}
	}

	outStr, _ = sjson.Set(outStr, "params.messages", ccMessages)

	// Process tools
	tools := gjson.GetBytes(rawJSON, "tools")
	if tools.IsArray() {
		var ccTools []interface{}
		for _, t := range tools.Array() {
			name := t.Get("name").String()
			desc := t.Get("description").String()
			schema := t.Get("input_schema")

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

func getClaudePairedToolCallIDs(messages []gjson.Result) map[string]bool {
	callIDs := make(map[string]bool)
	resultIDs := make(map[string]bool)
	for _, message := range messages {
		role := message.Get("role").String()
		content := message.Get("content")
		if content.IsArray() {
			for _, part := range content.Array() {
				partType := part.Get("type").String()
				if role == "assistant" && partType == "tool_use" {
					if id := part.Get("id").String(); id != "" {
						callIDs[id] = true
					}
				} else if (role == "user" || role == "tool") && partType == "tool_result" {
					if id := part.Get("tool_use_id").String(); id != "" {
						resultIDs[id] = true
					}
				}
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

func findClaudeToolName(messages []gjson.Result, targetID string) string {
	for _, msg := range messages {
		if msg.Get("role").String() == "assistant" {
			content := msg.Get("content")
			if content.IsArray() {
				for _, part := range content.Array() {
					if part.Get("type").String() == "tool_use" && part.Get("id").String() == targetID {
						return part.Get("name").String()
					}
				}
			}
		}
	}
	return ""
}
