package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestIsCommandCodeProvider(t *testing.T) {
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want bool
	}{
		{name: "compat name", auth: &cliproxyauth.Auth{Attributes: map[string]string{"compat_name": "CommandCode"}}, want: true},
		{name: "provider key", auth: &cliproxyauth.Auth{Attributes: map[string]string{"provider_key": "command-code"}}, want: true},
		{name: "management label", auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Label: "cmdcode"}, want: true},
		{name: "other provider", auth: &cliproxyauth.Auth{Provider: "openai-compatibility", Label: "openrouter"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCommandCodeProvider(tt.auth); got != tt.want {
				t.Fatalf("IsCommandCodeProvider() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildCommandCodeGenerateBody(t *testing.T) {
	request := []byte(`{
		"model":"deepseek/deepseek-v4-flash",
		"messages":[
			{"role":"system","content":"Follow the project rules."},
			{"role":"user","content":[{"type":"text","text":"Hello"}]},
			{"role":"assistant","content":"", "tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"file contents"}
		],
		"tools":[{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],
		"max_tokens":128,
		"temperature":0.2
	}`)
	body, err := buildCommandCodeGenerateBody("deepseek/deepseek-v4-flash", request, true)
	if err != nil {
		t.Fatalf("buildCommandCodeGenerateBody() error = %v", err)
	}
	if got := gjson.GetBytes(body, "params.model").String(); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("model = %q", got)
	}
	if got := gjson.GetBytes(body, "params.system").String(); got != "Follow the project rules." {
		t.Fatalf("system = %q", got)
	}
	if got := gjson.GetBytes(body, "params.messages.1.content.1.type").String(); got != "tool-call" {
		t.Fatalf("tool call type = %q", got)
	}
	if got := gjson.GetBytes(body, "params.messages.2.content.0.output.value").String(); got != "file contents" {
		t.Fatalf("tool result = %q", got)
	}
	if got := gjson.GetBytes(body, "params.tools.0.input_schema.type").String(); got != "object" {
		t.Fatalf("tool schema type = %q", got)
	}
	if got := gjson.GetBytes(body, "params.stream").Bool(); !got {
		t.Fatal("params.stream = false, want true")
	}
}

func TestCommandCodeConvertMessagesDropsOrphanToolResults(t *testing.T) {
	body, err := buildCommandCodeGenerateBody("deepseek/deepseek-v4-flash", []byte(`{
		"messages":[
			{"role":"tool","tool_call_id":"missing","content":"stale"},
			{"role":"user","content":"hello"}
		]
	}`), true)
	if err != nil {
		t.Fatalf("buildCommandCodeGenerateBody() error = %v", err)
	}
	if got := gjson.GetBytes(body, "params.messages.#").Int(); got != 1 {
		t.Fatalf("messages = %d, want orphan tool result omitted", got)
	}
	if got := gjson.GetBytes(body, "params.messages.0.role").String(); got != "user" {
		t.Fatalf("remaining message role = %q, want user", got)
	}
}

func TestTrimCommandCodeHistoryLeavesPromptUnderGatewayLimit(t *testing.T) {
	large := strings.Repeat("code ", 120_000)
	messages := make([]commandCodeMessage, 0, 12)
	for i := 0; i < 12; i++ {
		messages = append(messages, commandCodeMessage{Role: "user", Content: []commandCodeContent{{Type: "text", Text: large}}})
	}
	trimmed := trimCommandCodeHistory("deepseek/deepseek-v4-flash", "", messages, nil)
	if len(trimmed) >= len(messages) {
		t.Fatalf("history was not trimmed: %d messages", len(trimmed))
	}
	if got := commandCodePromptTokens("deepseek/deepseek-v4-flash", "", trimmed, nil); got > commandCodeMaxPromptTokens {
		t.Fatalf("trimmed prompt tokens = %d, want <= %d", got, commandCodeMaxPromptTokens)
	}
}

func TestCommandCodeGenerateURL(t *testing.T) {
	tests := map[string]string{
		"https://api.commandcode.ai":              "https://api.commandcode.ai/alpha/generate",
		"https://api.commandcode.ai/v1":           "https://api.commandcode.ai/alpha/generate",
		"https://api.commandcode.ai/provider/v1":  "https://api.commandcode.ai/alpha/generate",
		"http://127.0.0.1:1234/proxy/provider/v1": "http://127.0.0.1:1234/proxy/alpha/generate",
		"http://127.0.0.1:1234/proxy":             "http://127.0.0.1:1234/proxy/alpha/generate",
	}
	for input, want := range tests {
		if got := commandCodeGenerateURL(input); got != want {
			t.Errorf("commandCodeGenerateURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCommandCodeStreamStateConvertsNDJSON(t *testing.T) {
	state := newCommandCodeStreamState("model-a")
	lines := []string{
		`{"type":"start"}`,
		`{"type":"text-delta","id":"txt-0","text":"hello"}`,
		`{"type":"finish","finishReason":"stop","usage":{"inputTokens":3,"outputTokens":2,"totalTokens":5}}`,
	}
	var chunks [][]byte
	for _, line := range lines {
		events, err := state.consumeLine([]byte(line))
		if err != nil {
			t.Fatalf("consumeLine() error = %v", err)
		}
		for _, event := range events {
			chunks = append(chunks, event.openAIChunk)
		}
	}
	if len(chunks) != 2 {
		t.Fatalf("converted chunks = %d, want 2", len(chunks))
	}
	if !strings.Contains(string(chunks[0]), `"content":"hello"`) {
		t.Fatalf("text chunk = %s", chunks[0])
	}
	if got := state.usage.TotalTokens; got != 5 {
		t.Fatalf("total tokens = %d, want 5", got)
	}
	var response map[string]any
	if err := json.Unmarshal(state.chatResponse("model-a"), &response); err != nil {
		t.Fatalf("chatResponse() is not JSON: %v", err)
	}
	if got := gjson.GetBytes(state.chatResponse("model-a"), "choices.0.message.content").String(); got != "hello" {
		t.Fatalf("response content = %q", got)
	}
}

func TestCommandCodeExecutorStreamsTranslatedOpenAIChunks(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, "{\"type\":\"start\"}\n{\"type\":\"text-delta\",\"id\":\"txt-0\",\"text\":\"hello\"}\n{\"type\":\"finish\",\"finishReason\":\"stop\",\"usage\":{\"inputTokens\":1,\"outputTokens\":1,\"totalTokens\":2}}\n")
	}))
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":  "user_test",
		"base_url": server.URL + "/provider/v1",
	}}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "model-a",
		Payload: []byte(`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var output strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if gotPath != "/alpha/generate" {
		t.Fatalf("request path = %q, want /alpha/generate", gotPath)
	}
	if !gjson.GetBytes(gotBody, "params.stream").Bool() {
		t.Fatal("params.stream = false, want true")
	}
	if !strings.Contains(output.String(), `"content":"hello"`) || !strings.Contains(output.String(), `"finish_reason":"stop"`) {
		t.Fatalf("translated stream = %s", output.String())
	}
}
