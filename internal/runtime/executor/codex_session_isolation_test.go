package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexAccountScopedExplicitSessionIDStablePerAccount(t *testing.T) {
	authA := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	authB := codexSessionIsolationAuth("auth-b", "acct-b", "b@example.com")

	gotA1 := codexAccountScopedExplicitSessionID(authA, "prompt-cache")
	gotA2 := codexAccountScopedExplicitSessionID(authA, " prompt-cache ")
	gotB := codexAccountScopedExplicitSessionID(authB, "prompt-cache")

	if gotA1 == "prompt-cache" || !strings.HasPrefix(gotA1, "prompt-cache-") {
		t.Fatalf("scoped id = %q, want prompt-cache-*", gotA1)
	}
	if gotA1 != gotA2 {
		t.Fatalf("same auth scoped ids differ: %q vs %q", gotA1, gotA2)
	}
	if gotA1 == gotB {
		t.Fatalf("different accounts produced same scoped id: %q", gotA1)
	}
}

func TestCodexCacheHelperScopesOpenAIResponsePromptCacheKeyByAccount(t *testing.T) {
	authA := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	authB := codexSessionIsolationAuth("auth-b", "acct-b", "b@example.com")
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","prompt_cache_key":"shared-session"}`),
	}

	httpReqA := codexCacheHelperRequest(t, authA, sdktranslator.FromString("openai-response"), req)
	httpReqB := codexCacheHelperRequest(t, authB, sdktranslator.FromString("openai-response"), req)
	bodyA := readRequestBody(t, httpReqA)
	bodyB := readRequestBody(t, httpReqB)
	sessionA := gjson.GetBytes(bodyA, "prompt_cache_key").String()
	sessionB := gjson.GetBytes(bodyB, "prompt_cache_key").String()

	if sessionA == "" || sessionB == "" {
		t.Fatalf("prompt_cache_key missing: %s / %s", bodyA, bodyB)
	}
	if sessionA == "shared-session" || sessionB == "shared-session" {
		t.Fatalf("prompt_cache_key was not scoped: %q / %q", sessionA, sessionB)
	}
	if sessionA == sessionB {
		t.Fatalf("different accounts produced same prompt_cache_key: %q", sessionA)
	}
	if httpReqA.Header.Get("Conversation_id") != sessionA || httpReqA.Header.Get("Session_id") != sessionA {
		t.Fatalf("headers A = %#v, want scoped session %q", httpReqA.Header, sessionA)
	}
	if httpReqB.Header.Get("Conversation_id") != sessionB || httpReqB.Header.Get("Session_id") != sessionB {
		t.Fatalf("headers B = %#v, want scoped session %q", httpReqB.Header, sessionB)
	}
}

func TestCodexCacheHelperAddsStablePromptCacheKeyForOpenAIChatCompletions(t *testing.T) {
	authA := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	authB := codexSessionIsolationAuth("auth-b", "acct-b", "b@example.com")
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`),
	}

	httpReqA1 := codexCacheHelperRequest(t, authA, sdktranslator.FromString("openai"), req)
	httpReqA2 := codexCacheHelperRequest(t, authA, sdktranslator.FromString("openai"), req)
	httpReqB := codexCacheHelperRequest(t, authB, sdktranslator.FromString("openai"), req)
	sessionA1 := gjson.GetBytes(readRequestBody(t, httpReqA1), "prompt_cache_key").String()
	sessionA2 := gjson.GetBytes(readRequestBody(t, httpReqA2), "prompt_cache_key").String()
	sessionB := gjson.GetBytes(readRequestBody(t, httpReqB), "prompt_cache_key").String()

	if sessionA1 == "" {
		t.Fatal("prompt_cache_key missing for openai chat completions")
	}
	if sessionA1 != sessionA2 {
		t.Fatalf("same account prompt_cache_key differs: %q vs %q", sessionA1, sessionA2)
	}
	if sessionA1 == sessionB {
		t.Fatalf("different accounts produced same prompt_cache_key: %q", sessionA1)
	}
	if httpReqA1.Header.Get("Conversation_id") != sessionA1 || httpReqA1.Header.Get("Session_id") != sessionA1 {
		t.Fatalf("headers A = %#v, want scoped session %q", httpReqA1.Header, sessionA1)
	}
}

func TestCodexCacheHelperUsesInboundSessionForOpenAIChatCompletions(t *testing.T) {
	auth := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`),
	}

	httpReqA1 := codexCacheHelperRequestWithHeaders(t, auth, sdktranslator.FromString("openai"), req, http.Header{"Session-Id": []string{"chat-a"}})
	httpReqA2 := codexCacheHelperRequestWithHeaders(t, auth, sdktranslator.FromString("openai"), req, http.Header{"Session-Id": []string{"chat-a"}})
	httpReqB := codexCacheHelperRequestWithHeaders(t, auth, sdktranslator.FromString("openai"), req, http.Header{"Session-Id": []string{"chat-b"}})
	sessionA1 := gjson.GetBytes(readRequestBody(t, httpReqA1), "prompt_cache_key").String()
	sessionA2 := gjson.GetBytes(readRequestBody(t, httpReqA2), "prompt_cache_key").String()
	sessionB := gjson.GetBytes(readRequestBody(t, httpReqB), "prompt_cache_key").String()

	if sessionA1 == "" || sessionB == "" {
		t.Fatalf("prompt_cache_key missing: %q / %q", sessionA1, sessionB)
	}
	if sessionA1 != sessionA2 {
		t.Fatalf("same inbound session produced different prompt_cache_key: %q vs %q", sessionA1, sessionA2)
	}
	if sessionA1 == sessionB {
		t.Fatalf("different inbound sessions produced same prompt_cache_key: %q", sessionA1)
	}
}

func TestApplyCodexPromptCacheHeadersScopesOpenAIResponsePromptCacheKeyByAccount(t *testing.T) {
	authA := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	authB := codexSessionIsolationAuth("auth-b", "acct-b", "b@example.com")
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","prompt_cache_key":"shared-session"}`),
	}

	bodyA, headersA := applyCodexPromptCacheHeaders(authA, sdktranslator.FromString("openai-response"), req, req.Payload)
	bodyB, headersB := applyCodexPromptCacheHeaders(authB, sdktranslator.FromString("openai-response"), req, req.Payload)
	sessionA := gjson.GetBytes(bodyA, "prompt_cache_key").String()
	sessionB := gjson.GetBytes(bodyB, "prompt_cache_key").String()

	if sessionA == "" || sessionB == "" {
		t.Fatalf("prompt_cache_key missing: %s / %s", bodyA, bodyB)
	}
	if sessionA == sessionB {
		t.Fatalf("different accounts produced same websocket prompt_cache_key: %q", sessionA)
	}
	if headersA.Get("Conversation_id") != sessionA || headersA.Get("Session_id") != sessionA {
		t.Fatalf("headers A = %#v, want scoped session %q", headersA, sessionA)
	}
	if headersB.Get("Conversation_id") != sessionB || headersB.Get("Session_id") != sessionB {
		t.Fatalf("headers B = %#v, want scoped session %q", headersB, sessionB)
	}
}

func TestCodexClaudePromptCacheMapKeyIncludesAccountScope(t *testing.T) {
	authA := codexSessionIsolationAuth("auth-a", "acct-a", "a@example.com")
	authB := codexSessionIsolationAuth("auth-b", "acct-b", "b@example.com")

	keyA1 := codexPromptCacheMapKey(authA, "gpt-5-codex", "user-1")
	keyA2 := codexPromptCacheMapKey(authA, "gpt-5-codex", "user-1")
	keyB := codexPromptCacheMapKey(authB, "gpt-5-codex", "user-1")

	if keyA1 != keyA2 {
		t.Fatalf("same account keys differ: %q vs %q", keyA1, keyA2)
	}
	if keyA1 == keyB {
		t.Fatalf("different accounts produced same claude prompt cache map key: %q", keyA1)
	}
	if !strings.Contains(keyA1, "gpt-5-codex-user-1") {
		t.Fatalf("key = %q, want model/user suffix", keyA1)
	}
}

func codexCacheHelperRequest(t *testing.T, auth *cliproxyauth.Auth, from sdktranslator.Format, req cliproxyexecutor.Request) *http.Request {
	t.Helper()
	got, err := (&CodexExecutor{}).cacheHelper(context.Background(), auth, from, "https://chatgpt.com/backend-api/codex/responses", req, req.Payload)
	if err != nil {
		t.Fatalf("cacheHelper() error = %v", err)
	}
	return got
}

func codexCacheHelperRequestWithHeaders(t *testing.T, auth *cliproxyauth.Auth, from sdktranslator.Format, req cliproxyexecutor.Request, headers http.Header) *http.Request {
	t.Helper()
	ginCtx := &gin.Context{Request: httptestRequestWithHeaders(headers)}
	ctx := context.WithValue(context.Background(), util.ContextKeyGin, ginCtx)
	got, err := (&CodexExecutor{}).cacheHelper(ctx, auth, from, "https://chatgpt.com/backend-api/codex/responses", req, req.Payload)
	if err != nil {
		t.Fatalf("cacheHelper() error = %v", err)
	}
	return got
}

func httptestRequestWithHeaders(headers http.Header) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header = headers
	return req
}

func readRequestBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return body
}

func codexSessionIsolationAuth(id, accountID, email string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: "codex",
		Metadata: map[string]any{
			"account_id": accountID,
			"email":      email,
		},
	}
}
