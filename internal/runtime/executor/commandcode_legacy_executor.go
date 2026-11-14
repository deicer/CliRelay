package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	commandCodeDefaultBaseURL = "https://api.commandcode.ai"
	// The legacy gateway rejects requests from clients that do not identify a
	// Command Code version. Keep this in sync with the installed command-code
	// client; callers can override it through the provider's custom headers.
	commandCodeClientVersion = "0.52.1"
)

// IsCommandCodeProvider reports whether an OpenAI-compatible channel should
// use Command Code's native /alpha/generate protocol instead of /v1/chat/completions.
func IsCommandCodeProvider(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	values := []string{auth.Provider, auth.Label}
	if auth.Attributes != nil {
		values = append(values, auth.Attributes["compat_name"], auth.Attributes["provider_key"])
	}
	for _, value := range values {
		value = normalizeCommandCodeProviderName(value)
		if value == "commandcode" || value == "cmdcode" {
			return true
		}
	}
	return false
}

func normalizeCommandCodeProviderName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var normalized strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

// CommandCodeExecutor speaks the undocumented Command Code CLI gateway. The
// gateway returns newline-delimited JSON events rather than SSE.
type CommandCodeExecutor struct {
	cfg *config.Config
}

func NewCommandCodeExecutor(cfg *config.Config) *CommandCodeExecutor {
	return &CommandCodeExecutor{cfg: cfg}
}

func (e *CommandCodeExecutor) Identifier() string { return commandCodeProvider }

func (e *CommandCodeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if auth != nil && auth.Attributes != nil {
		if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}
	return nil
}

func (e *CommandCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("commandcode executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	return newProxyAwareHTTPClient(ctx, e.cfg, auth, 0).Do(httpReq)
}

func (e *CommandCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	execCtx, body, err := e.prepare(ctx, auth, req, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	resp, err := e.doRequest(execCtx.Context, auth, body, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), resp.Body)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		return cliproxyexecutor.Response{}, statusErr{code: resp.StatusCode, msg: string(b), retryAfter: parseHTTPRetryAfterHeader(resp.Header, time.Now())}
	}

	state, err := consumeCommandCodeNDJSON(resp.Body, nil, nil)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	responseJSON := state.chatResponse(execCtx.BaseModel)
	reporter.publishWithContent(execCtx.Context, state.usage, string(req.Payload), string(responseJSON))
	var param any
	out := sdktranslator.TranslateNonStream(execCtx.Context, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, body, responseJSON, &param)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: resp.Header.Clone()}, nil
}

func (e *CommandCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	execCtx, body, err := e.prepare(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	resp, err := e.doRequest(execCtx.Context, auth, body, opts, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), resp.Body)
		_ = resp.Body.Close()
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		return nil, statusErr{code: resp.StatusCode, msg: string(b), retryAfter: parseHTTPRetryAfterHeader(resp.Header, time.Now())}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer resp.Body.Close()
		state := newCommandCodeStreamState(execCtx.BaseModel)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			reporter.appendOutputChunk(line)
			events, errLine := state.consumeLine(line)
			if errLine != nil {
				reporter.publishFailure(execCtx.Context)
				out <- cliproxyexecutor.StreamChunk{Err: errLine}
				return
			}
			for _, event := range events {
				if event.usage != (coreusage.Detail{}) {
					reporter.publish(execCtx.Context, event.usage)
				}
				canonical := event.openAIChunk
				if len(canonical) == 0 {
					continue
				}
				chunks := sdktranslator.TranslateStream(execCtx.Context, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, body, canonical, &param)
				for _, chunk := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}
		if !state.finished {
			for _, event := range state.finishEvents() {
				chunks := sdktranslator.TranslateStream(execCtx.Context, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, body, event.openAIChunk, &param)
				for _, chunk := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}
				}
			}
		}
		// OpenAI chat handlers add their own terminal marker. Other source
		// formats need the normal translator flush to emit message_stop or
		// response.completed lifecycle events.
		if opts.SourceFormat != sdktranslator.FormatOpenAI {
			chunks := sdktranslator.TranslateStream(execCtx.Context, sdktranslator.FromString("openai"), opts.SourceFormat, req.Model, opts.OriginalRequest, body, []byte("data: [DONE]"), &param)
			for _, chunk := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunk)}
			}
		}
		reporter.ensurePublished(execCtx.Context)
	}()
	return &cliproxyexecutor.StreamResult{Headers: resp.Header.Clone(), Chunks: out}, nil
}

func (e *CommandCodeExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "Command Code token counting is not supported"}
}

func (e *CommandCodeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	_ = ctx
	return auth, nil
}

func (e *CommandCodeExecutor) prepare(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*ExecutionContext, []byte, error) {
	if opts.Alt == "responses/compact" {
		return nil, nil, statusErr{code: http.StatusNotImplemented, msg: "Command Code compact responses are not supported"}
	}
	execCtx := newExecutionContext(ctx, commandCodeProvider, e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("openai"),
		TranslateAsStream: stream,
	})
	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)
	translated, err := thinking.ApplyThinking(translated, req.Model, opts.SourceFormat.String(), "openai", e.Identifier())
	if err != nil {
		return nil, nil, err
	}
	body, err := buildCommandCodeGenerateBody(execCtx.BaseModel, translated, stream)
	if err != nil {
		return nil, nil, err
	}
	return execCtx, body, nil
}

func (e *CommandCodeExecutor) doRequest(ctx context.Context, auth *cliproxyauth.Auth, body []byte, opts cliproxyexecutor.Options, stream bool) (*http.Response, error) {
	baseURL := commandCodeBaseURL(auth)
	if baseURL == "" {
		baseURL = commandCodeDefaultBaseURL
	}
	endpoint := commandCodeGenerateURL(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	key := commandCodeAPIKey(auth)
	if key == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing Command Code API key"}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("x-command-code-version", commandCodeClientVersion)
	sessionID := opencodeGoSessionID(opts, auth)
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	req.Header.Set("x-session-id", sessionID)
	if attrs := authAttributes(auth); attrs != nil {
		util.ApplyCustomHeadersFromAttrs(req, attrs)
	}
	if stream {
		req.Header.Set("Cache-Control", "no-cache")
	}
	client := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return client.Do(req)
}

func commandCodeAPIKey(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		return strings.TrimSpace(auth.Attributes["api_key"])
	}
	return ""
}

func commandCodeBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	return strings.TrimSpace(auth.Attributes["base_url"])
}

func authAttributes(auth *cliproxyauth.Auth) map[string]string {
	if auth == nil {
		return nil
	}
	return auth.Attributes
}

func commandCodeGenerateURL(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base == "" {
		return commandCodeDefaultBaseURL + "/alpha/generate"
	}
	if parsed, err := url.Parse(base); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		path := strings.TrimRight(parsed.Path, "/")
		for _, suffix := range []string{"/provider/v1", "/provider", "/v1"} {
			if strings.HasSuffix(path, suffix) {
				path = strings.TrimSuffix(path, suffix)
				break
			}
		}
		parsed.Path = strings.TrimRight(path, "/") + "/alpha/generate"
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	return base + "/alpha/generate"
}

// commandCodeGenerateBody converts the OpenAI-compatible request produced by
// the shared translator into the strict ModelMessage[] envelope used by the
// Command Code CLI gateway.
func buildCommandCodeGenerateBody(model string, openAIRequest []byte, _ bool) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(openAIRequest, &root); err != nil {
		return nil, fmt.Errorf("commandcode request is not JSON: %w", err)
	}
	var messages []json.RawMessage
	if raw := root["messages"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, fmt.Errorf("commandcode messages are invalid: %w", err)
		}
	}
	system, converted, err := commandCodeConvertMessages(messages)
	if err != nil {
		return nil, err
	}
	tools := commandCodeConvertTools(root["tools"])
	maxTokens := int64(8192)
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if raw := root[key]; len(raw) > 0 {
			var n int64
			if json.Unmarshal(raw, &n) == nil && n > 0 {
				maxTokens = n
				break
			}
		}
	}
	params := map[string]any{
		"model":      model,
		"system":     system,
		"messages":   converted,
		"tools":      tools,
		"max_tokens": maxTokens,
		// /alpha/generate is an event gateway. Always request its NDJSON
		// envelope, then aggregate it for non-streaming callers locally.
		"stream": true,
	}
	for _, key := range []string{"temperature", "reasoning_effort"} {
		if raw := root[key]; len(raw) > 0 {
			var value any
			if json.Unmarshal(raw, &value) == nil {
				params[key] = value
			}
		}
	}
	body := map[string]any{
		"config": map[string]any{
			"workingDir":    "",
			"date":          time.Now().UTC().Format("2006-01-02"),
			"environment":   "production",
			"structure":     []any{},
			"isGitRepo":     false,
			"currentBranch": "",
			"mainBranch":    "",
			"gitStatus":     "",
			"recentCommits": []any{},
		},
		"memory":         "",
		"taste":          nil,
		"skills":         nil,
		"permissionMode": "standard",
		"params":         params,
	}
	return json.Marshal(body)
}

type commandCodeContent struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Image      string `json:"image,omitempty"`
	MediaType  string `json:"mediaType,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	Input      any    `json:"input,omitempty"`
	Output     any    `json:"output,omitempty"`
}

type commandCodeMessage struct {
	Role    string               `json:"role"`
	Content []commandCodeContent `json:"content"`
}

func commandCodeConvertMessages(messages []json.RawMessage) (string, []commandCodeMessage, error) {
	var systemParts []string
	out := make([]commandCodeMessage, 0, len(messages))
	toolNames := make(map[string]string)
	for _, raw := range messages {
		role := gjson.GetBytes(raw, "role").String()
		if role == "system" {
			if text := commandCodeContentText(gjson.GetBytes(raw, "content")); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		message := commandCodeMessage{Role: role}
		if role == "tool" {
			id := gjson.GetBytes(raw, "tool_call_id").String()
			name := toolNames[id]
			if name == "" {
				name = "unknown"
			}
			value := gjson.GetBytes(raw, "content")
			text := value.String()
			if value.IsObject() || value.IsArray() {
				text = value.Raw
			}
			message.Content = []commandCodeContent{{Type: "tool-result", ToolCallID: id, ToolName: name, Output: map[string]any{"type": "text", "value": text}}}
			out = append(out, message)
			continue
		}
		content := gjson.GetBytes(raw, "content")
		message.Content = append(message.Content, commandCodeConvertContent(content)...)
		if role == "assistant" {
			for _, call := range gjson.GetBytes(raw, "tool_calls").Array() {
				id := call.Get("id").String()
				name := call.Get("function.name").String()
				toolNames[id] = name
				args := call.Get("function.arguments").String()
				var input any = map[string]any{}
				if args != "" {
					_ = json.Unmarshal([]byte(args), &input)
				}
				message.Content = append(message.Content, commandCodeContent{Type: "tool-call", ToolCallID: id, ToolName: name, Input: input})
			}
		}
		if len(message.Content) > 0 {
			out = append(out, message)
		}
	}
	return strings.Join(systemParts, "\n\n"), out, nil
}

func commandCodeConvertContent(content gjson.Result) []commandCodeContent {
	if !content.Exists() || content.Type == gjson.Null {
		return nil
	}
	if content.Type == gjson.String {
		return []commandCodeContent{{Type: "text", Text: content.String()}}
	}
	if !content.IsArray() {
		return []commandCodeContent{{Type: "text", Text: content.Raw}}
	}
	out := make([]commandCodeContent, 0, len(content.Array()))
	for _, item := range content.Array() {
		switch item.Get("type").String() {
		case "text", "input_text", "output_text":
			out = append(out, commandCodeContent{Type: "text", Text: item.Get("text").String()})
		case "image_url", "input_image":
			image := item.Get("image_url.url").String()
			if image == "" {
				image = item.Get("image_url").String()
			}
			if image != "" {
				mediaType := ""
				if strings.HasPrefix(image, "data:") {
					if semi := strings.IndexByte(image, ';'); semi > len("data:") {
						mediaType = image[len("data:"):semi]
					}
				}
				out = append(out, commandCodeContent{Type: "image", Image: image, MediaType: mediaType})
			}
		}
	}
	return out
}

func commandCodeContentText(content gjson.Result) string {
	parts := commandCodeConvertContent(content)
	var out []string
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			out = append(out, part.Text)
		}
	}
	return strings.Join(out, "\n")
}

func commandCodeConvertTools(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return []map[string]any{}
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		name := gjson.GetBytes(tool, "function.name").String()
		if name == "" {
			continue
		}
		parameters := gjson.GetBytes(tool, "function.parameters")
		var schema any = map[string]any{}
		if parameters.Exists() {
			_ = json.Unmarshal([]byte(parameters.Raw), &schema)
		}
		out = append(out, map[string]any{
			"name":         name,
			"description":  gjson.GetBytes(tool, "function.description").String(),
			"input_schema": schema,
		})
	}
	return out
}

type commandCodeStreamEvent struct {
	openAIChunk []byte
	usage       coreusage.Detail
}

type commandCodeToolState struct {
	id        string
	name      string
	arguments strings.Builder
	index     int
}

type commandCodeStreamState struct {
	model     string
	id        string
	created   int64
	started   bool
	finished  bool
	finish    string
	text      strings.Builder
	reasoning strings.Builder
	tools     []*commandCodeToolState
	toolByID  map[string]*commandCodeToolState
	usage     coreusage.Detail
	usageSeen bool
}

func newCommandCodeStreamState(model string) *commandCodeStreamState {
	return &commandCodeStreamState{model: model, id: "chatcmpl-" + uuid.NewString(), created: time.Now().Unix(), toolByID: make(map[string]*commandCodeToolState)}
}

func (s *commandCodeStreamState) consumeLine(line []byte) ([]commandCodeStreamEvent, error) {
	var event map[string]any
	if err := json.Unmarshal(line, &event); err != nil {
		return nil, fmt.Errorf("invalid Command Code NDJSON: %w", err)
	}
	typeName, _ := event["type"].(string)
	switch typeName {
	case "start", "start-step":
		return nil, nil
	case "reasoning-start":
		return nil, nil
	case "reasoning-delta":
		text := commandCodeEventText(event)
		s.reasoning.WriteString(text)
		return []commandCodeStreamEvent{{openAIChunk: s.chunk(map[string]any{"reasoning_content": text}, "")}}, nil
	case "reasoning-end":
		return nil, nil
	case "text-start":
		return nil, nil
	case "text-delta":
		text := commandCodeEventText(event)
		s.text.WriteString(text)
		return []commandCodeStreamEvent{{openAIChunk: s.chunk(map[string]any{"content": text}, "")}}, nil
	case "text-end":
		return nil, nil
	case "tool-input-start":
		id, _ := event["id"].(string)
		if id == "" {
			id, _ = event["toolCallId"].(string)
		}
		name, _ := event["toolName"].(string)
		if name == "" {
			name, _ = event["name"].(string)
		}
		tool := s.addTool(id, name)
		return []commandCodeStreamEvent{{openAIChunk: s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": tool.index, "id": tool.id, "type": "function", "function": map[string]any{"name": tool.name, "arguments": ""}}}}, "")}}, nil
	case "tool-input-delta":
		id, _ := event["id"].(string)
		if id == "" {
			id, _ = event["toolCallId"].(string)
		}
		tool := s.toolByID[id]
		if tool == nil && id == "" && len(s.tools) > 0 {
			tool = s.tools[len(s.tools)-1]
		}
		if tool == nil {
			tool = s.addTool(id, "")
		}
		delta, _ := event["delta"].(string)
		emitted := appendCommandCodeToolArguments(tool, delta)
		if emitted == "" {
			return nil, nil
		}
		return []commandCodeStreamEvent{{openAIChunk: s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": tool.index, "function": map[string]any{"arguments": emitted}}}}, "")}}, nil
	case "tool-input-end":
		return nil, nil
	case "tool-call":
		id, _ := event["id"].(string)
		if id == "" {
			id, _ = event["toolCallId"].(string)
		}
		name, _ := event["toolName"].(string)
		if name == "" {
			name, _ = event["name"].(string)
		}
		tool := s.toolByID[id]
		if tool == nil {
			tool = s.addTool(id, name)
		}
		if input, ok := event["input"]; ok {
			arg := ""
			if rawInput, ok := input.(string); ok {
				arg = rawInput
			} else if raw, err := json.Marshal(input); err == nil {
				arg = string(raw)
			}
			if arg != "" {
				for _, existingTool := range s.tools {
					if existingTool != tool && commandCodeArgumentsEqual(existingTool.arguments.String(), arg) {
						return nil, nil
					}
				}
				emitted := appendCommandCodeToolArguments(tool, arg)
				if emitted == "" {
					return nil, nil
				}
				return []commandCodeStreamEvent{{openAIChunk: s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": tool.index, "function": map[string]any{"arguments": emitted}}}}, "")}}, nil
			}
		}
		return nil, nil
	case "finish-step":
		usage := commandCodeUsageDetail(event["usage"], event["totalUsage"])
		if usage != (coreusage.Detail{}) {
			s.usage = usage
			s.usageSeen = true
		}
		if reason, ok := event["finishReason"].(string); ok && reason != "" {
			s.finish = commandCodeFinishReason(reason)
		}
		return nil, nil
	case "finish":
		s.finished = true
		if reason, ok := event["finishReason"].(string); ok && reason != "" {
			s.finish = commandCodeFinishReason(reason)
		}
		usage := commandCodeUsageDetail(event["usage"], event["totalUsage"])
		if usage != (coreusage.Detail{}) {
			s.usage = usage
			s.usageSeen = true
		}
		return []commandCodeStreamEvent{{openAIChunk: s.finishChunk(), usage: s.usage}}, nil
	case "error":
		message, _ := event["message"].(string)
		if message == "" {
			message = fmt.Sprint(event["error"])
		}
		return nil, fmt.Errorf("Command Code stream error: %s", message)
	default:
		return nil, nil
	}
}

func appendCommandCodeToolArguments(tool *commandCodeToolState, value string) string {
	if tool == nil || value == "" {
		return ""
	}
	existing := tool.arguments.String()
	if commandCodeArgumentsEqual(existing, value) {
		return ""
	}
	if existing != "" && strings.HasPrefix(value, existing) {
		suffix := strings.TrimPrefix(value, existing)
		tool.arguments.WriteString(suffix)
		return suffix
	}
	tool.arguments.WriteString(value)
	return value
}

func commandCodeArgumentsEqual(left, right string) bool {
	if left == right {
		return true
	}
	var leftValue, rightValue any
	if json.Unmarshal([]byte(left), &leftValue) == nil && json.Unmarshal([]byte(right), &rightValue) == nil {
		leftCanonical, leftErr := json.Marshal(leftValue)
		rightCanonical, rightErr := json.Marshal(rightValue)
		if leftErr == nil && rightErr == nil {
			return bytes.Equal(leftCanonical, rightCanonical)
		}
	}
	var leftCompact, rightCompact bytes.Buffer
	if json.Compact(&leftCompact, []byte(left)) != nil || json.Compact(&rightCompact, []byte(right)) != nil {
		return false
	}
	return leftCompact.String() == rightCompact.String()
}

func commandCodeEventText(event map[string]any) string {
	if text, ok := event["text"].(string); ok {
		return text
	}
	if delta, ok := event["delta"].(string); ok {
		return delta
	}
	return ""
}

func (s *commandCodeStreamState) addTool(id, name string) *commandCodeToolState {
	if id == "" {
		id = "call_" + uuid.NewString()
	}
	if existing := s.toolByID[id]; existing != nil {
		if existing.name == "" {
			existing.name = name
		}
		return existing
	}
	tool := &commandCodeToolState{id: id, name: name, index: len(s.tools)}
	s.tools = append(s.tools, tool)
	s.toolByID[id] = tool
	return tool
}

func (s *commandCodeStreamState) chunk(delta map[string]any, finish string) []byte {
	if !s.started {
		delta["role"] = "assistant"
		s.started = true
	}
	root := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	}
	if finish != "" {
		root["choices"].([]any)[0].(map[string]any)["finish_reason"] = finish
	}
	data, _ := json.Marshal(root)
	return append([]byte("data: "), data...)
}

func (s *commandCodeStreamState) finishChunk() []byte {
	finish := s.finish
	if finish == "" {
		if len(s.tools) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	root := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	}
	if s.usageSeen {
		root["usage"] = map[string]any{
			"prompt_tokens":     s.usage.InputTokens,
			"completion_tokens": s.usage.OutputTokens,
			"total_tokens":      s.usage.TotalTokens,
		}
	}
	data, _ := json.Marshal(root)
	return append([]byte("data: "), data...)
}

func (s *commandCodeStreamState) finishEvents() []commandCodeStreamEvent {
	if s.finished {
		return nil
	}
	s.finished = true
	s.finish = "stop"
	return []commandCodeStreamEvent{{openAIChunk: s.finishChunk(), usage: s.usage}}
}

func (s *commandCodeStreamState) chatResponse(model string) []byte {
	message := map[string]any{"role": "assistant"}
	if s.text.Len() > 0 {
		message["content"] = s.text.String()
	} else {
		message["content"] = nil
	}
	if s.reasoning.Len() > 0 {
		message["reasoning_content"] = s.reasoning.String()
	}
	if len(s.tools) > 0 {
		calls := make([]map[string]any, 0, len(s.tools))
		for _, tool := range s.tools {
			calls = append(calls, map[string]any{"id": tool.id, "type": "function", "function": map[string]any{"name": tool.name, "arguments": tool.arguments.String()}})
		}
		message["tool_calls"] = calls
	}
	finish := s.finish
	if finish == "" {
		if len(s.tools) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	root := map[string]any{
		"id": s.id, "object": "chat.completion", "created": s.created, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": s.usage.InputTokens, "completion_tokens": s.usage.OutputTokens, "total_tokens": s.usage.TotalTokens},
	}
	data, _ := json.Marshal(root)
	return data
}

func consumeCommandCodeNDJSON(r io.Reader, state *commandCodeStreamState, _ func([]byte)) (*commandCodeStreamState, error) {
	if state == nil {
		state = newCommandCodeStreamState("")
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		if _, err := state.consumeLine(bytes.TrimSpace(scanner.Bytes())); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !state.finished {
		state.finishEvents()
	}
	return state, nil
}

func commandCodeFinishReason(value any) string {
	reason, _ := value.(string)
	switch reason {
	case "length":
		return "length"
	case "tool-calls", "tool_calls", "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func commandCodeUsageDetail(primary, fallback any) coreusage.Detail {
	value := primary
	if value == nil {
		value = fallback
	}
	if value == nil {
		return coreusage.Detail{}
	}
	data, _ := json.Marshal(value)
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return coreusage.Detail{}
	}
	get := func(keys ...string) int64 {
		for _, key := range keys {
			if number, ok := obj[key].(float64); ok {
				return int64(number)
			}
		}
		return 0
	}
	input := get("inputTokens", "input_tokens")
	output := get("outputTokens", "output_tokens")
	cached := get("cachedInputTokens", "cache_read_input_tokens")
	detail := coreusage.Detail{InputTokens: input, OutputTokens: output, CacheReadTokens: cached, CachedTokens: cached, CacheReadIncludedInInput: cached > 0, TotalTokens: get("totalTokens", "total_tokens")}
	if detail.TotalTokens == 0 {
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	}
	return detail
}
