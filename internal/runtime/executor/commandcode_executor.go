package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxyusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	commandCodeDefaultURL = "https://api.commandcode.ai/alpha/generate"
	commandCodeAuthType   = "commandcode"
)

// CommandCodeExecutor proxies requests to the Command Code CLI endpoint.
type CommandCodeExecutor struct {
	cfg *config.Config
}

// NewCommandCodeExecutor creates a new Command Code executor instance.
func NewCommandCodeExecutor(cfg *config.Config) *CommandCodeExecutor {
	return &CommandCodeExecutor{cfg: cfg}
}

// Identifier returns the executor identifier.
func (e *CommandCodeExecutor) Identifier() string { return commandCodeAuthType }

// PrepareRequest injects Command Code credentials into the outgoing HTTP request.
func (e *CommandCodeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, projectSlug, version, cliEnv, tasteLearning, coFlag := e.resolveCredentials(auth)
	if apiKey == "" {
		return fmt.Errorf("commandcode executor: missing api_key")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-command-code-version", version)
	req.Header.Set("x-cli-environment", cliEnv)
	req.Header.Set("x-project-slug", projectSlug)
	req.Header.Set("x-taste-learning", tasteLearning)
	req.Header.Set("x-co-flag", coFlag)

	if auth != nil {
		applyCustomHeadersFromAttrs(req, auth.Attributes)
	}
	return nil
}

// HttpRequest executes an HTTP request with Command Code credentials.
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
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming request.
func (e *CommandCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	to := sdktranslator.FromString("commandcode")
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: false,
	})

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)

	apiKey, _, version, cliEnv, tasteLearning, coFlag := e.resolveCredentials(auth)
	if apiKey == "" {
		return resp, fmt.Errorf("commandcode executor: missing api_key")
	}

	url := commandCodeDefaultURL
	if auth != nil && auth.Attributes != nil {
		if customURL := strings.TrimSpace(auth.Attributes["base_url"]); customURL != "" {
			url = customURL
		}
	}

	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}

	workingDir := gjson.GetBytes(translated, "config.workingDir").String()
	dynamicProjectSlug := projectSlugFromPath(workingDir)

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("User-Agent", "axios/1.7.2")
	httpReq.Header.Set("x-command-code-version", version)
	httpReq.Header.Set("x-cli-environment", cliEnv)
	httpReq.Header.Set("x-project-slug", dynamicProjectSlug)
	httpReq.Header.Set("x-taste-learning", tasteLearning)
	httpReq.Header.Set("x-co-flag", coFlag)

	if auth != nil {
		applyCustomHeadersFromAttrs(httpReq, auth.Attributes)
	}

	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("commandcode executor: close response body error: %v", errClose)
		}
	}()

	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	bodyBytes, errRead := readUpstreamResponseBody("commandcode", httpResp.Body)
	if errRead != nil {
		recorder.RecordResponseError(errRead)
		err = errRead
		return resp, err
	}
	recorder.AppendResponseChunk(bodyBytes)

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(bodyBytes))
		err = statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		return resp, err
	}

	reporter.publishWithContent(execCtx.Context, parseCommandCodeUsage(bodyBytes), string(req.Payload), string(bodyBytes))
	var param any
	converted := sdktranslator.TranslateNonStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, translated, bodyBytes, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(converted), Headers: httpResp.Header.Clone()}
	reporter.ensurePublished(execCtx.Context)

	return resp, nil
}

// ExecuteStream performs a streaming request.
func (e *CommandCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (result *cliproxyexecutor.StreamResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	to := sdktranslator.FromString("commandcode")
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      to,
		TranslateAsStream: true,
	})

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)

	apiKey, _, version, cliEnv, tasteLearning, coFlag := e.resolveCredentials(auth)
	if apiKey == "" {
		return nil, fmt.Errorf("commandcode executor: missing api_key")
	}

	url := commandCodeDefaultURL
	if auth != nil && auth.Attributes != nil {
		if customURL := strings.TrimSpace(auth.Attributes["base_url"]); customURL != "" {
			url = customURL
		}
	}

	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}

	workingDir := gjson.GetBytes(translated, "config.workingDir").String()
	dynamicProjectSlug := projectSlugFromPath(workingDir)

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("User-Agent", "axios/1.7.2")
	httpReq.Header.Set("x-command-code-version", version)
	httpReq.Header.Set("x-cli-environment", cliEnv)
	httpReq.Header.Set("x-project-slug", dynamicProjectSlug)
	httpReq.Header.Set("x-taste-learning", tasteLearning)
	httpReq.Header.Set("x-co-flag", coFlag)

	if auth != nil {
		applyCustomHeadersFromAttrs(httpReq, auth.Attributes)
	}

	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}

	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := readUpstreamResponseBody("commandcode", httpResp.Body)
		_ = httpResp.Body.Close()
		recorder.AppendResponseChunk(bodyBytes)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(bodyBytes))
		err = statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))

	go func(resp *http.Response) {
		defer close(out)
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("commandcode executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(resp.Body)
		var param any

		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)

			if len(line) == 0 {
				continue
			}

			// Clean SSE line prefix
			payload := line
			if bytes.HasPrefix(line, []byte("data: ")) {
				payload = line[6:]
			} else if bytes.HasPrefix(line, []byte("data:")) {
				payload = line[5:]
			} else {
				// If not prefixed with data: but contains json
				trimmed := bytes.TrimSpace(line)
				if !bytes.HasPrefix(trimmed, []byte("{")) {
					continue
				}
				payload = trimmed
			}

			if detail, ok := parseCommandCodeStreamUsage(payload); ok {
				reporter.publish(execCtx.Context, detail)
			}

			chunks := sdktranslator.TranslateStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, translated, bytes.Clone(payload), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}

		tail := sdktranslator.TranslateStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, translated, []byte("[DONE]"), &param)
		for i := range tail {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(tail[i])}
		}

		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		} else {
			reporter.ensurePublished(execCtx.Context)
		}
	}(httpResp)

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens is not supported.
func (e *CommandCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("commandcode executor: CountTokens not implemented")
}

// Refresh is a no-op.
func (e *CommandCodeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *CommandCodeExecutor) resolveCredentials(auth *cliproxyauth.Auth) (apiKey, projectSlug, version, cliEnv, tasteLearning, coFlag string) {
	version = "0.29.0"
	cliEnv = "production"
	tasteLearning = "true"
	coFlag = "false"

	if auth == nil {
		return
	}

	getVal := func(key string) string {
		if auth.Attributes != nil {
			if v := strings.TrimSpace(auth.Attributes[key]); v != "" {
				return v
			}
		}
		if auth.Metadata != nil {
			if v, ok := auth.Metadata[key].(string); ok {
				if v = strings.TrimSpace(v); v != "" {
					return v
				}
			}
			if attrs, ok := auth.Metadata["attributes"].(map[string]any); ok {
				if v, ok2 := attrs[key].(string); ok2 {
					if v = strings.TrimSpace(v); v != "" {
						return v
					}
				}
			}
		}
		return ""
	}

	if v := getVal("api_key"); v != "" {
		apiKey = v
	} else if v := getVal("token"); v != "" {
		apiKey = v
	}

	if v := getVal("project_slug"); v != "" {
		projectSlug = v
	} else if v := getVal("project"); v != "" {
		projectSlug = v
	}

	if v := getVal("version"); v != "" {
		version = v
	}

	if v := getVal("environment"); v != "" {
		cliEnv = v
	}

	if v := getVal("taste_learning"); v != "" {
		tasteLearning = v
	}

	if v := getVal("co_flag"); v != "" {
		coFlag = v
	}

	return
}

func parseCommandCodeUsage(body []byte) cliproxyusage.Detail {
	usageVal := gjson.GetBytes(body, "totalUsage")
	if !usageVal.Exists() {
		usageVal = gjson.GetBytes(body, "usage")
	}
	if !usageVal.Exists() {
		return cliproxyusage.Detail{}
	}

	inTokens := usageVal.Get("inputTokens").Int()
	if inTokens == 0 {
		inTokens = usageVal.Get("prompt_tokens").Int()
	}
	outTokens := usageVal.Get("outputTokens").Int()
	if outTokens == 0 {
		outTokens = usageVal.Get("completion_tokens").Int()
	}
	cachedTokens := usageVal.Get("inputTokenDetails.cacheReadTokens").Int()

	return cliproxyusage.Detail{
		InputTokens:     inTokens,
		OutputTokens:    outTokens,
		CacheReadTokens: cachedTokens,
		CachedTokens:    cachedTokens,
	}
}

func parseCommandCodeStreamUsage(payload []byte) (cliproxyusage.Detail, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return cliproxyusage.Detail{}, false
	}
	typeVal := gjson.GetBytes(payload, "type").String()
	if typeVal != "finish" {
		return cliproxyusage.Detail{}, false
	}
	usageVal := gjson.GetBytes(payload, "totalUsage")
	if !usageVal.Exists() {
		return cliproxyusage.Detail{}, false
	}
	inTokens := usageVal.Get("inputTokens").Int()
	outTokens := usageVal.Get("outputTokens").Int()
	cachedTokens := usageVal.Get("inputTokenDetails.cacheReadTokens").Int()

	return cliproxyusage.Detail{
		InputTokens:     inTokens,
		OutputTokens:    outTokens,
		CacheReadTokens: cachedTokens,
		CachedTokens:    cachedTokens,
	}, true
}

func applyCustomHeadersFromAttrs(req *http.Request, attrs map[string]string) {
	if req == nil || attrs == nil {
		return
	}
	for k, v := range attrs {
		if strings.HasPrefix(strings.ToLower(k), "header:") {
			headerName := k[7:]
			req.Header.Set(headerName, v)
		}
	}
}

func projectSlugFromPath(pathName string) string {
	slug := strings.ToLower(pathName)
	if len(slug) >= 2 && slug[1] == ':' && slug[0] >= 'a' && slug[0] <= 'z' {
		slug = slug[2:]
	}
	var sb strings.Builder
	lastDash := false
	for _, r := range slug {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			lastDash = false
		} else {
			if sb.Len() > 0 && !lastDash {
				sb.WriteRune('-')
				lastDash = true
			}
		}
	}
	res := sb.String()
	res = strings.Trim(res, "-")
	if res == "" {
		return "project"
	}
	return res
}
