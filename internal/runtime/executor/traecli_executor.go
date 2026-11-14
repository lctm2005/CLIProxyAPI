package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	logs "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	openairesponses "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	traeCLIProvider               = "traecli"
	traeCLIDefaultBaseURL         = "https://copilot-cn.bytedance.net"
	traeCLIDefaultAppID           = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	traeCLIDefaultFunction        = "traecli_next"
	traeCLIDefaultAuthFile        = "~/.trae/cli/auth.json"
	traeCLIDefaultModelsCache     = "~/.trae/cli/models_cache.json"
	traeCLIDefaultUserAgentPrefix = "codex_exec"
	traeCLIModeNative             = "native"
	traeCLIModeExec               = "exec"
	traeCLIDefaultExecPath        = "traecli"
	traeCLIDefaultExecSandbox     = "read-only"
	traeCLIExtraInfoTTL           = time.Hour
	traeCLIMaxCacheControls       = 4
)

// TraeCLIExecutor executes requests through TRAE CLI raw-chat or traecli exec.
type TraeCLIExecutor struct {
	cfg            *config.Config
	fallbackMu     sync.Mutex
	fallbackUntil  time.Time
	fallbackReason string
}

type traeCLIExtraInfoEntry struct {
	value  string
	expire time.Time
	order  uint64
}

var (
	traeCLIExtraInfoCache            = make(map[string]traeCLIExtraInfoEntry)
	traeCLIExtraInfoMu               sync.RWMutex
	traeCLIExtraInfoCacheCleanupOnce sync.Once
	traeCLIRequestOrder              atomic.Uint64
)

func NewTraeCLIExecutor(cfg *config.Config) *TraeCLIExecutor { return &TraeCLIExecutor{cfg: cfg} }

func (e *TraeCLIExecutor) Identifier() string { return traeCLIProvider }

func (e *TraeCLIExecutor) RequestToFormat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAI
}

func (e *TraeCLIExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if e.mode(auth) == traeCLIModeExec {
		return nil
	}
	token, err := e.traeToken(auth)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Cloud-CLI-JWT "+token)
	e.applyRawChatHeaders(req, auth)
	return nil
}

func (e *TraeCLIExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("traecli executor: request is nil")
	}
	if e.mode(auth) == traeCLIModeExec {
		return nil, fmt.Errorf("traecli executor: raw HTTP requests are not supported in exec mode")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewTraeHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(httpReq)
	if err == nil && resp != nil {
		resp.Header = traeCLIModeHeaders(resp.Header, traeCLIModeNative)
	}
	return resp, err
}

func (e *TraeCLIExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if e.mode(auth) == traeCLIModeExec {
		return e.executeViaExec(ctx, auth, req, opts)
	}
	if e.nativeFallbackActive(auth) {
		resp, errExecFallback := e.executeViaExec(ctx, auth, req, opts)
		if errExecFallback == nil {
			markTraeCLINativeFallbackHeaders(resp.Headers)
		}
		return resp, errExecFallback
	}
	order := traeCLIRequestOrder.Add(1)
	rawResp, headers, translated, sessionKey, errExec := e.executeRawChat(ctx, auth, req, opts, false)
	if errExec != nil {
		if e.shouldFallbackNativeToExec(auth, errExec) {
			e.activateNativeFallback(auth, errExec)
			resp, errExecFallback := e.executeViaExec(ctx, auth, req, opts)
			if errExecFallback == nil {
				markTraeCLINativeFallbackHeaders(resp.Headers)
			}
			return resp, errExecFallback
		}
		return resp, errExec
	}
	parsed, errParse := parseTraeCLIRawChatPayloadDetailed(rawResp, opts.OriginalRequest)
	if errParse != nil {
		return resp, errParse
	}
	if !parsed.Completed {
		return resp, newTraeCLIStreamError(ctx, "upstream stream closed before completion")
	}
	cacheTraeCLIExtraInfoBestEffort(sessionKey, parsed.ExtraInfo, order)
	openAIResp, errBuild := buildTraeCLIOpenAIResponse(req.Model, parsed)
	if errBuild != nil {
		return resp, errBuild
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, responseFormat, req.Model, opts.OriginalRequest, translated, openAIResp, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

func (e *TraeCLIExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e.mode(auth) == traeCLIModeExec {
		return e.executeViaExecStream(ctx, auth, req, opts)
	}
	if e.nativeFallbackActive(auth) {
		result, errExecFallback := e.executeViaExecStream(ctx, auth, req, opts)
		if errExecFallback == nil && result != nil {
			markTraeCLINativeFallbackHeaders(result.Headers)
		}
		return result, errExecFallback
	}
	result, errExec := e.executeRawChatStream(ctx, auth, req, opts)
	if errExec != nil {
		if e.shouldFallbackNativeToExec(auth, errExec) {
			e.activateNativeFallback(auth, errExec)
			fallbackResult, errExecFallback := e.executeViaExecStream(ctx, auth, req, opts)
			if errExecFallback == nil && fallbackResult != nil {
				markTraeCLINativeFallbackHeaders(fallbackResult.Headers)
			}
			return fallbackResult, errExecFallback
		}
		return nil, errExec
	}
	return result, nil
}

func (e *TraeCLIExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	body := sdktranslator.TranslateRequest(from, sdktranslator.FormatOpenAI, baseModel, bytes.Clone(req.Payload), false)
	enc, err := helps.TokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("traecli executor: tokenizer init failed: %w", err)
	}
	count, err := helps.CountOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("traecli executor: token counting failed: %w", err)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, sdktranslator.FormatOpenAI, responseFormat, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

func (e *TraeCLIExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

func (e *TraeCLIExecutor) executeViaExec(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	text, headers, translated, err := e.runTraeCLIExec(ctx, auth, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	openAIResp, errBuild := buildTraeCLIOpenAIResponse(req.Model, traeCLIRawChatPayload{Text: text})
	if errBuild != nil {
		return cliproxyexecutor.Response{}, errBuild
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, responseFormat, req.Model, opts.OriginalRequest, translated, openAIResp, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

func (e *TraeCLIExecutor) nativeFallbackActive(auth *cliproxyauth.Auth) bool {
	if !e.nativeFallbackToExec(auth) {
		return false
	}
	e.fallbackMu.Lock()
	defer e.fallbackMu.Unlock()
	if e.fallbackUntil.IsZero() {
		return false
	}
	now := time.Now()
	if !now.Before(e.fallbackUntil) {
		e.fallbackUntil = time.Time{}
		e.fallbackReason = ""
		return false
	}
	return true
}

func (e *TraeCLIExecutor) activateNativeFallback(auth *cliproxyauth.Auth, err error) {
	if !e.nativeFallbackToExec(auth) {
		return
	}
	cooldown := e.nativeFallbackCooldown(auth)
	if cooldown <= 0 {
		return
	}
	e.fallbackMu.Lock()
	e.fallbackUntil = time.Now().Add(cooldown)
	e.fallbackReason = traeCLIErrorMessage(err)
	until := e.fallbackUntil
	reason := e.fallbackReason
	e.fallbackMu.Unlock()
	log.WithFields(log.Fields{
		"provider": e.Identifier(),
		"until":    until.Format(time.RFC3339),
	}).Warnf("traecli native raw-chat failed; falling back to traecli exec: %s", reason)
}

func (e *TraeCLIExecutor) shouldFallbackNativeToExec(auth *cliproxyauth.Auth, err error) bool {
	if !e.nativeFallbackToExec(auth) || err == nil {
		return false
	}
	if status, ok := err.(interface{ StatusCode() int }); ok {
		code := status.StatusCode()
		if code == http.StatusUnauthorized {
			return false
		}
		if code == http.StatusForbidden || code == http.StatusTooManyRequests {
			return true
		}
		if code >= 500 && code < 600 {
			return true
		}
	}
	msg := strings.ToLower(traeCLIErrorMessage(err))
	for _, marker := range []string{
		"risk",
		"security",
		"forbidden",
		"blocked",
		"captcha",
		"verify",
		"verification",
		"rate limit",
		"too many requests",
		"风控",
		"封禁",
		"封控",
		"验证",
		"安全",
		"频率",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func (e *TraeCLIExecutor) nativeFallbackToExec(auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["native_fallback_to_exec"]); v != "" {
			return parseTraeCLIBool(v)
		}
	}
	return e != nil && e.cfg != nil && e.cfg.TraeCLI.NativeFallbackToExec
}

func (e *TraeCLIExecutor) nativeFallbackCooldown(auth *cliproxyauth.Auth) time.Duration {
	seconds := 0
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["native_fallback_cooldown_seconds"]); v != "" {
			if parsed, err := strconv.Atoi(v); err == nil {
				seconds = parsed
			}
		}
	}
	if seconds == 0 && e != nil && e.cfg != nil {
		seconds = e.cfg.TraeCLI.NativeFallbackCooldownSeconds
	}
	if seconds <= 0 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func traeCLIErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func markTraeCLINativeFallbackHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	headers.Set("X-TraeCLI-Mode", traeCLIModeExec)
	headers.Set("X-TraeCLI-Native-Fallback", "true")
}

func (e *TraeCLIExecutor) executeViaExecStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	text, headers, translated, err := e.runTraeCLIExec(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	return e.streamText(ctx, req.Model, opts, translated, headers, text), nil
}

func (e *TraeCLIExecutor) runTraeCLIExec(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (string, http.Header, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	translated := sdktranslator.TranslateRequest(from, sdktranslator.FormatOpenAI, baseModel, bytes.Clone(req.Payload), false)
	var err error
	translated, err = thinking.ApplyThinking(translated, req.Model, from.String(), sdktranslator.FormatOpenAI.String(), e.Identifier())
	if err != nil {
		return "", nil, nil, err
	}
	prompt := buildTraeCLIExecPrompt(translated)
	if strings.TrimSpace(prompt) == "" {
		return "", nil, nil, fmt.Errorf("traecli executor: empty exec prompt")
	}

	outputFile, err := createTraeCLIExecOutputFile()
	if err != nil {
		return "", nil, nil, err
	}
	defer func() {
		if errRemove := os.Remove(outputFile); errRemove != nil && !os.IsNotExist(errRemove) {
			log.Debugf("traecli executor: remove output file %s: %v", outputFile, errRemove)
		}
	}()

	workDir := e.execWorkDirForRequest(auth, opts.OriginalRequest, req.Payload, translated)
	args := e.execArgs(auth, baseModel, outputFile, workDir)
	cmd := osexec.CommandContext(ctx, e.execPath(auth), args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = e.execEnv(ctx, auth)
	if workDir != "" {
		cmd.Dir = workDir
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       e.execPath(auth) + " " + strings.Join(args, " "),
		Method:    "EXEC",
		Body:      []byte(prompt),
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	combined, errRun := cmd.CombinedOutput()
	if len(combined) > 0 {
		helps.AppendAPIResponseChunk(ctx, e.cfg, combined)
	}
	if errRun != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRun)
		msg := traeCLIExecErrorMessage(errRun, combined)
		return "", nil, nil, statusErr{code: traeCLIUpstreamErrorStatusCode(http.StatusBadGateway, []byte(msg)), msg: msg}
	}

	textRaw, errRead := os.ReadFile(outputFile)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return "", nil, nil, fmt.Errorf("traecli executor: read exec output: %w", errRead)
	}
	text := strings.TrimSpace(string(textRaw))
	if text == "" {
		text = strings.TrimSpace(string(combined))
	}
	headers := http.Header{}
	headers.Set("X-TraeCLI-Mode", traeCLIModeExec)
	helps.RecordAPIResponseMetadata(ctx, e.cfg, http.StatusOK, headers)
	if text != "" {
		helps.AppendAPIResponseChunk(ctx, e.cfg, []byte(text))
	}
	return text, headers, translated, nil
}

func (e *TraeCLIExecutor) executeRawChat(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) ([]byte, http.Header, []byte, string, error) {
	httpReq, translated, sessionKey, err := e.prepareRawChatRequest(ctx, auth, req, opts, stream)
	if err != nil {
		return nil, nil, nil, "", err
	}

	httpClient := helps.NewTraeHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, nil, sessionKey, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("traecli executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	data, err := readTraeCLIResponseBody(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, nil, sessionKey, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errStatus := statusErr{code: traeCLIUpstreamErrorStatusCode(httpResp.StatusCode, data), msg: string(data)}
		logs.CtxError(ctx, "traecli executor: upstream error (status %d)", errStatus.code)
		return nil, nil, nil, sessionKey, withTraeCLIRetryAfter(ctx, errStatus, httpResp.Header)
	}
	if errStatus := traeCLIStatusError(data); errStatus != nil {
		return nil, nil, nil, sessionKey, withTraeCLIRetryAfter(ctx, errStatus, httpResp.Header)
	}
	return data, traeCLIModeHeaders(httpResp.Header, traeCLIModeNative), translated, sessionKey, nil
}

func (e *TraeCLIExecutor) prepareRawChatRequest(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (*http.Request, []byte, string, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	translated := sdktranslator.TranslateRequest(from, sdktranslator.FormatOpenAI, baseModel, bytes.Clone(req.Payload), stream)
	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), sdktranslator.FormatOpenAI.String(), e.Identifier())
	if err != nil {
		return nil, nil, "", err
	}

	rawBody, sessionKey, err := e.buildRawChatBodyForRequest(ctx, auth, req, opts, translated, opts.OriginalRequest, baseModel)
	if err != nil {
		return nil, nil, "", err
	}
	url := strings.TrimRight(e.baseURL(auth), "/") + "/api/ide/v2/llm_raw_chat"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawBody))
	if err != nil {
		return nil, nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, nil, "", err
	}
	applyTraeCLITraceHeaderFromBody(httpReq, rawBody)
	applyTraeCLIRequestIdentityHeaders(httpReq, rawBody)
	applyTraeCLIRepoHeaders(httpReq, rawBody)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      rawBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	return httpReq, translated, sessionKey, nil
}

func (e *TraeCLIExecutor) executeRawChatStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	order := traeCLIRequestOrder.Add(1)
	httpReq, translated, sessionKey, err := e.prepareRawChatRequest(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}
	httpClient := helps.NewTraeHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, errRead := readTraeCLIResponseBody(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("traecli executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		errStatus := statusErr{code: traeCLIUpstreamErrorStatusCode(httpResp.StatusCode, data), msg: string(data)}
		logs.CtxError(ctx, "traecli executor: upstream error (status %d)", errStatus.code)
		return nil, withTraeCLIRetryAfter(ctx, errStatus, httpResp.Header)
	}

	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	chunks := make(chan cliproxyexecutor.StreamChunk, 6)
	go func() {
		defer close(chunks)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("traecli executor: close response body error: %v", errClose)
			}
		}()

		var translatorParam any
		sendPayload := func(payload []byte) bool {
			translatedChunks := sdktranslator.TranslateStream(
				ctx,
				sdktranslator.FormatOpenAI,
				responseFormat,
				req.Model,
				opts.OriginalRequest,
				translated,
				payload,
				&translatorParam,
			)
			for _, translatedChunk := range translatedChunks {
				if len(translatedChunk) == 0 {
					continue
				}
				select {
				case chunks <- cliproxyexecutor.StreamChunk{Payload: translatedChunk}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		sendError := func(errStream error) {
			errStream = withTraeCLIRetryAfter(ctx, errStream, httpResp.Header)
			helps.RecordAPIResponseError(ctx, e.cfg, errStream)
			select {
			case chunks <- cliproxyexecutor.StreamChunk{Err: errStream}:
			case <-ctx.Done():
			}
		}

		accumulator := newTraeCLIRawChatAccumulator(ctx, opts.OriginalRequest)
		forwardLine := func(line []byte) bool {
			delta, errConsume := accumulator.consumeLine(line)
			if errConsume != nil {
				sendError(errConsume)
				return false
			}
			return delta == "" || sendPayload(buildTraeCLIChatCompletionChunk(req.Model, delta, false))
		}
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if !forwardLine(line) {
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			logs.CtxError(ctx, "traecli executor: read stream: %v", errScan)
			sendError(errScan)
			return
		}
		if !forwardLine(nil) {
			return
		}
		if !accumulator.completed {
			sendError(newTraeCLIStreamError(ctx, "upstream stream closed before completion"))
			return
		}

		result := accumulator.result(opts.OriginalRequest)
		cacheTraeCLIExtraInfoBestEffort(sessionKey, result.ExtraInfo, order)
		if len(result.ToolCalls) > 0 && !sendPayload(buildTraeCLIChatCompletionToolCallChunk(req.Model, result.ToolCalls)) {
			return
		}
		finishReason := "stop"
		if len(result.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		if !sendPayload(buildTraeCLIChatCompletionFinishChunk(req.Model, finishReason, result.Usage, result.UsageSeen)) {
			return
		}
		_ = sendPayload([]byte("data: [DONE]\n\n"))
	}()
	return &cliproxyexecutor.StreamResult{Headers: traeCLIModeHeaders(httpResp.Header, traeCLIModeNative), Chunks: chunks}, nil
}

func traeCLIModeHeaders(headers http.Header, mode string) http.Header {
	out := headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	out.Set("X-TraeCLI-Mode", mode)
	return out
}

func (e *TraeCLIExecutor) buildRawChatBody(auth *cliproxyauth.Auth, translated, original []byte, model string) ([]byte, error) {
	body, _, err := e.buildRawChatBodyForRequest(context.Background(), auth, cliproxyexecutor.Request{Model: model, Payload: original}, cliproxyexecutor.Options{
		OriginalRequest: original,
	}, translated, original, model)
	return body, err
}

func (e *TraeCLIExecutor) buildRawChatBodyForRequest(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, translated, original []byte, model string) ([]byte, string, error) {
	body := e.presetBody(auth)
	hasPreset := len(body) > 0
	if !hasPreset {
		body = []byte(`{"config_name":"","model_name":"","user_input":"","messages":[],"session_id":"","conversation_id":"","is_preset":true,"parallel_tool_calls":true}`)
	}
	userInput := latestOpenAIUserText(translated)
	body, _ = sjson.SetBytes(body, "config_name", e.rawConfigName(auth, model))
	body, _ = sjson.SetBytes(body, "model_name", e.rawModelName(auth, model))
	body, _ = sjson.SetBytes(body, "user_input", userInput)
	if hasPreset {
		body = rebuildTraeCLIPresetMessages(body, userInput)
	} else {
		body, _ = sjson.SetRawBytes(body, "messages", openAIChatMessagesToTraeRaw(translated, original, e.baseInstructions(auth, model)))
	}
	conversationID := e.rawChatConversationID(ctx, req, opts, translated, original)
	if conversationID == "" {
		conversationID = e.rawChatSessionID(ctx, req, opts, translated, original)
	}
	if conversationID == "" {
		conversationID = uuidV7Like()
	}
	sessionID := e.rawChatRequestSessionID(ctx, req, opts, translated, original)
	if sessionID == "" {
		sessionID = stableTraeCLIUUID("request:" + conversationID)
	}
	body, _ = sjson.SetBytes(body, "session_id", sessionID)
	body, _ = sjson.SetBytes(body, "conversation_id", conversationID)
	if repoURL := strings.TrimSpace(gjson.GetBytes(original, "metadata.git.repository_url").String()); repoURL != "" {
		body, _ = sjson.SetBytes(body, "biz_context.repo_urls.0", repoURL)
	}
	if maxTokens := gjson.GetBytes(translated, "max_tokens"); maxTokens.Exists() {
		if !hasPreset {
			body, _ = sjson.SetRawBytes(body, "max_tokens", []byte(maxTokens.Raw))
		}
	} else {
		body, _ = sjson.SetBytes(body, "max_tokens", 32768)
	}
	if effort := e.rawReasoningEffort(auth, model, gjson.GetBytes(translated, "reasoning_effort").String()); effort != "" {
		if !hasPreset {
			body, _ = sjson.SetBytes(body, "reasoning_effort", effort)
		}
	}
	if e.forwardTools(auth) {
		if tools := gjson.GetBytes(translated, "tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
			if normalizedTools := normalizeTraeCLIToolsForRawChat(tools); len(normalizedTools) > 0 {
				body, _ = sjson.SetRawBytes(body, "tools", normalizedTools)
			}
		}
	}
	if parallel := gjson.GetBytes(translated, "parallel_tool_calls"); parallel.Exists() {
		body, _ = sjson.SetRawBytes(body, "parallel_tool_calls", []byte(parallel.Raw))
	}
	if extraInfo, ok := getTraeCLIExtraInfo(conversationID); ok {
		body, _ = sjson.SetRawBytes(body, "extra_info", []byte(extraInfo))
	}
	body = enforceTraeCLICacheControlLimit(body, traeCLIMaxCacheControls)
	return body, conversationID, nil
}

func enforceTraeCLICacheControlLimit(body []byte, limit int) []byte {
	if limit < 0 {
		limit = 0
	}
	var paths []string
	for toolIndex, tool := range gjson.GetBytes(body, "tools").Array() {
		for _, suffix := range []string{"cache_control", "function.cache_control"} {
			if tool.Get(suffix).Exists() {
				paths = append(paths, fmt.Sprintf("tools.%d.%s", toolIndex, suffix))
			}
		}
	}
	for messageIndex, message := range gjson.GetBytes(body, "messages").Array() {
		for contentIndex, content := range message.Get("content").Array() {
			if content.Get("cache_control").Exists() {
				paths = append(paths, fmt.Sprintf("messages.%d.content.%d.cache_control", messageIndex, contentIndex))
			}
		}
	}
	for len(paths) > limit {
		var errDelete error
		body, errDelete = sjson.DeleteBytes(body, paths[0])
		if errDelete != nil {
			return body
		}
		paths = paths[1:]
	}
	return body
}

func (e *TraeCLIExecutor) rawChatRequestSessionID(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payloads ...[]byte) string {
	for _, raw := range append([][]byte{req.Payload, opts.OriginalRequest}, payloads...) {
		if sessionID := strings.TrimSpace(gjson.GetBytes(raw, "traecli_session_id").String()); sessionID != "" {
			return stableTraeCLIUUID(sessionID)
		}
		if sessionID := strings.TrimSpace(gjson.GetBytes(raw, "metadata.traecli_session_id").String()); sessionID != "" {
			return stableTraeCLIUUID(sessionID)
		}
	}
	if headerValue := firstTraeCLIHeaderValue(opts.Headers, "X-TraeCLI-Session-ID"); headerValue != "" {
		return stableTraeCLIUUID(headerValue)
	}
	return ""
}

func (e *TraeCLIExecutor) rawChatConversationID(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payloads ...[]byte) string {
	for _, raw := range append([][]byte{req.Payload, opts.OriginalRequest}, payloads...) {
		if conversationID := strings.TrimSpace(gjson.GetBytes(raw, "conversation_id").String()); conversationID != "" {
			return stableTraeCLIUUID(conversationID)
		}
		if conversationID := strings.TrimSpace(gjson.GetBytes(raw, "metadata.conversation_id").String()); conversationID != "" {
			return stableTraeCLIUUID(conversationID)
		}
	}
	if value := firstTraeCLIHeaderValue(opts.Headers, "Conversation_id", "Conversation-Id"); value != "" {
		return stableTraeCLIUUID(value)
	}
	return ""
}

func (e *TraeCLIExecutor) rawChatSessionID(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payloads ...[]byte) string {
	for _, metadata := range []map[string]any{opts.Metadata, req.Metadata} {
		if value := metadataString(metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
			return stableTraeCLIUUID(value)
		}
	}
	if sessionID := helps.ExtractClaudeCodeSessionID(ctx, opts.OriginalRequest, opts.Headers); sessionID != "" {
		return stableTraeCLIUUID(sessionID)
	}
	for _, raw := range append([][]byte{req.Payload, opts.OriginalRequest}, payloads...) {
		if sessionID := traeCLISessionIDFromPayload(ctx, raw); sessionID != "" {
			return stableTraeCLIUUID(sessionID)
		}
	}
	if headerValue := firstTraeCLIHeaderValue(opts.Headers, "X-Session-ID", "Session_id", "Session-Id", "X-Client-Request-Id"); headerValue != "" {
		return stableTraeCLIUUID(headerValue)
	}
	return ""
}

func traeCLISessionIDFromPayload(ctx context.Context, raw []byte) string {
	if len(raw) == 0 || !json.Valid(raw) {
		return ""
	}
	if sessionID := helps.ExtractClaudeCodeSessionID(ctx, raw, nil); sessionID != "" {
		return sessionID
	}
	for _, path := range []string{
		"session_id",
		"conversation_id",
		"metadata.session_id",
		"metadata.conversation_id",
		"client.session_id",
		"client.conversation_id",
	} {
		if value := strings.TrimSpace(gjson.GetBytes(raw, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func firstTraeCLIHeaderValue(headers http.Header, names ...string) string {
	if len(headers) == 0 {
		return ""
	}
	for _, name := range names {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func stableTraeCLIUUID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := uuid.Parse(value); err == nil {
		return parsed.String()
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:traecli:raw-chat:"+value)).String()
}

func (e *TraeCLIExecutor) forwardTools(auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["forward_tools"]); value != "" {
			return parseTraeCLIBool(value)
		}
	}
	if e != nil && e.cfg != nil {
		if value := strings.TrimSpace(e.cfg.TraeCLI.Headers["x-traecli-forward-tools"]); value != "" {
			return parseTraeCLIBool(value)
		}
	}
	return false
}

func addTraeCLIFunctionDeclarations(body []byte, tools gjson.Result) []byte {
	functionDeclarations := []byte("[]")
	count := 0
	for _, tool := range tools.Array() {
		fn := tool.Get("function")
		if !fn.Exists() {
			fn = tool
		}
		name := strings.TrimSpace(fn.Get("name").String())
		if name == "" {
			continue
		}
		declaration := []byte(`{"name":"","description":"","parameters":{}}`)
		declaration, _ = sjson.SetBytes(declaration, "name", name)
		if desc := strings.TrimSpace(fn.Get("description").String()); desc != "" {
			declaration, _ = sjson.SetBytes(declaration, "description", desc)
		}
		parameters := fn.Get("parameters")
		if !parameters.Exists() {
			parameters = fn.Get("parametersJsonSchema")
		}
		if !parameters.Exists() {
			parameters = fn.Get("input_schema")
		}
		if parameters.Exists() && parameters.Type == gjson.JSON {
			declaration, _ = sjson.SetRawBytes(declaration, "parameters", []byte(parameters.Raw))
		}
		functionDeclarations, _ = sjson.SetRawBytes(functionDeclarations, "-1", declaration)
		count++
	}
	if count == 0 {
		return body
	}
	body, _ = sjson.SetRawBytes(body, "functionDeclarations", functionDeclarations)
	body, _ = sjson.SetRawBytes(body, "toolConfig.functionCallingConfig.allowedFunctionNames", traeCLIFunctionDeclarationNames(functionDeclarations))
	return body
}

func normalizeTraeCLIToolsForRawChat(tools gjson.Result) []byte {
	out := []byte("[]")
	count := 0
	for _, tool := range tools.Array() {
		normalized := normalizeTraeCLIToolForRawChat(tool)
		if len(normalized) == 0 {
			continue
		}
		out, _ = sjson.SetRawBytes(out, "-1", normalized)
		count++
	}
	if count == 0 {
		return nil
	}
	return out
}

func normalizeTraeCLIToolForRawChat(tool gjson.Result) []byte {
	fn := tool.Get("function")
	if !fn.Exists() {
		fn = tool
	}
	name := strings.TrimSpace(fn.Get("name").String())
	if name == "" {
		return nil
	}
	out := []byte(`{"type":"function","function":{"name":"","description":"","parameters":"{}"}}`)
	out, _ = sjson.SetBytes(out, "function.name", name)
	if desc := strings.TrimSpace(fn.Get("description").String()); desc != "" {
		out, _ = sjson.SetBytes(out, "function.description", desc)
	}
	parameters := fn.Get("parameters")
	if !parameters.Exists() {
		parameters = fn.Get("parametersJsonSchema")
	}
	if !parameters.Exists() {
		parameters = fn.Get("input_schema")
	}
	if parameters.Exists() {
		if parameters.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "function.parameters", helps.NormalizeTraeCLIToolParameters(parameters.String()))
		} else if parameters.Raw != "" {
			out, _ = sjson.SetBytes(out, "function.parameters", helps.NormalizeTraeCLIToolParameters(parameters.Raw))
		}
	}
	return out
}

func traeCLIFunctionDeclarationNames(functionDeclarations []byte) []byte {
	out := []byte("[]")
	for _, declaration := range gjson.ParseBytes(functionDeclarations).Array() {
		if name := strings.TrimSpace(declaration.Get("name").String()); name != "" {
			out, _ = sjson.SetBytes(out, "-1", name)
		}
	}
	return out
}

func addTraeCLIToolConfig(body []byte, toolChoice gjson.Result) []byte {
	mode := "AUTO"
	var allowed []string
	if toolChoice.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
		case "none":
			mode = "NONE"
		case "required":
			mode = "ANY"
		default:
			mode = "AUTO"
		}
	} else if toolChoice.IsObject() {
		switch strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String())) {
		case "none":
			mode = "NONE"
		case "any", "required":
			mode = "ANY"
		case "tool", "function":
			mode = "ANY"
			if name := strings.TrimSpace(firstStringResult(toolChoice, "name", "function.name")); name != "" {
				allowed = append(allowed, name)
			}
		default:
			mode = "AUTO"
		}
	}
	body, _ = sjson.SetBytes(body, "toolConfig.functionCallingConfig.mode", mode)
	if len(allowed) > 0 {
		body, _ = sjson.SetBytes(body, "toolConfig.functionCallingConfig.allowedFunctionNames", allowed)
	}
	return body
}

func purgeExpiredTraeCLIExtraInfo() {
	now := time.Now()
	traeCLIExtraInfoMu.Lock()
	for key, entry := range traeCLIExtraInfoCache {
		if !entry.expire.After(now) {
			delete(traeCLIExtraInfoCache, key)
		}
	}
	traeCLIExtraInfoMu.Unlock()
}

func startTraeCLIExtraInfoCleanup() {
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredTraeCLIExtraInfo()
		}
	}()
}

func getTraeCLIExtraInfo(sessionID string) (string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", false
	}
	traeCLIExtraInfoCacheCleanupOnce.Do(startTraeCLIExtraInfoCleanup)
	now := time.Now()
	traeCLIExtraInfoMu.RLock()
	entry, ok := traeCLIExtraInfoCache[sessionID]
	traeCLIExtraInfoMu.RUnlock()
	if !ok || strings.TrimSpace(entry.value) == "" || !entry.expire.After(now) {
		return "", false
	}
	return entry.value, true
}

func cacheTraeCLIExtraInfoBestEffort(sessionID string, extraInfo []byte, order uint64) {
	sessionID = strings.TrimSpace(sessionID)
	extraInfo = bytes.TrimSpace(extraInfo)
	if sessionID == "" || len(extraInfo) == 0 || !json.Valid(extraInfo) {
		return
	}
	traeCLIExtraInfoCacheCleanupOnce.Do(startTraeCLIExtraInfoCleanup)
	traeCLIExtraInfoMu.Lock()
	defer traeCLIExtraInfoMu.Unlock()
	// Compare request start order, not completion time. Failed requests and
	// responses without usable state do not displace the last valid update.
	if previous, exists := traeCLIExtraInfoCache[sessionID]; exists && previous.order > order {
		return
	}
	traeCLIExtraInfoCache[sessionID] = traeCLIExtraInfoEntry{
		value:  string(bytes.Clone(extraInfo)),
		expire: time.Now().Add(traeCLIExtraInfoTTL),
		order:  order,
	}
}

func (e *TraeCLIExecutor) streamText(ctx context.Context, model string, opts cliproxyexecutor.Options, translated []byte, headers http.Header, text string) *cliproxyexecutor.StreamResult {
	return e.streamRawChatResult(ctx, model, opts, translated, headers, traeCLIRawChatPayload{Text: text})
}

func (e *TraeCLIExecutor) streamRawChatResult(ctx context.Context, model string, opts cliproxyexecutor.Options, translated []byte, headers http.Header, result traeCLIRawChatPayload) *cliproxyexecutor.StreamResult {
	if ctx == nil {
		ctx = context.Background()
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	chunks := make(chan cliproxyexecutor.StreamChunk, 6)
	go func() {
		defer close(chunks)
		var param any
		send := func(payload []byte) bool {
			translatedChunks := sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAI, responseFormat, model, opts.OriginalRequest, translated, payload, &param)
			for _, translatedChunk := range translatedChunks {
				if len(translatedChunk) == 0 {
					continue
				}
				select {
				case chunks <- cliproxyexecutor.StreamChunk{Payload: translatedChunk}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		if result.Text != "" {
			if !send(buildTraeCLIChatCompletionChunk(model, result.Text, false)) {
				return
			}
		}
		if len(result.ToolCalls) > 0 {
			if !send(buildTraeCLIChatCompletionToolCallChunk(model, result.ToolCalls)) {
				return
			}
		}
		finishReason := "stop"
		if len(result.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		if !send(buildTraeCLIChatCompletionFinishChunk(model, finishReason, result.Usage, result.UsageSeen)) {
			return
		}
		_ = send([]byte("data: [DONE]\n\n"))
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: chunks}
}

func (e *TraeCLIExecutor) mode(auth *cliproxyauth.Auth) string {
	mode := ""
	if auth != nil && auth.Attributes != nil {
		mode = strings.TrimSpace(auth.Attributes["mode"])
	}
	if mode == "" && e != nil && e.cfg != nil {
		mode = strings.TrimSpace(e.cfg.TraeCLI.Mode)
	}
	switch strings.ToLower(mode) {
	case "", traeCLIModeNative, "raw", "direct", "raw-chat", "raw_chat":
		return traeCLIModeNative
	case traeCLIModeExec, "cli", "command":
		return traeCLIModeExec
	default:
		log.Warnf("traecli executor: unknown mode %q, using native", mode)
		return traeCLIModeNative
	}
}

func (e *TraeCLIExecutor) execPath(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["exec_path"]); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	if e != nil && e.cfg != nil {
		if v := strings.TrimSpace(e.cfg.TraeCLI.ExecPath); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	return traeCLIDefaultExecPath
}

func (e *TraeCLIExecutor) execWorkDir(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["exec_workdir"]); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	if e != nil && e.cfg != nil {
		if v := strings.TrimSpace(e.cfg.TraeCLI.ExecWorkDir); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

func (e *TraeCLIExecutor) execWorkDirForRequest(auth *cliproxyauth.Auth, originals ...[]byte) string {
	for _, raw := range originals {
		if cwd := clientCWDFromRequest(raw); cwd != "" {
			return cwd
		}
	}
	return e.execWorkDir(auth)
}

func clientCWDFromRequest(raw []byte) string {
	if len(raw) == 0 || !json.Valid(raw) {
		return ""
	}
	for _, path := range []string{
		"cwd",
		"working_directory",
		"workingDirectory",
		"metadata.cwd",
		"metadata.working_directory",
		"client.cwd",
		"client.working_directory",
		"environment.cwd",
	} {
		if cwd := cleanTraeCLIClientCWD(gjson.GetBytes(raw, path).String()); cwd != "" {
			return cwd
		}
	}
	if cwd := cwdFromClaudeSystem(gjson.GetBytes(raw, "system")); cwd != "" {
		return cwd
	}
	if messages := gjson.GetBytes(raw, "messages"); messages.IsArray() {
		for _, msg := range messages.Array() {
			if msg.Get("role").String() != "system" {
				continue
			}
			if cwd := cwdFromText(openAIContentText(msg.Get("content"))); cwd != "" {
				return cwd
			}
		}
	}
	return ""
}

func cwdFromClaudeSystem(system gjson.Result) string {
	if !system.Exists() {
		return ""
	}
	if system.IsArray() {
		for _, part := range system.Array() {
			if part.Get("type").String() != "" && part.Get("type").String() != "text" {
				continue
			}
			if cwd := cwdFromText(part.Get("text").String()); cwd != "" {
				return cwd
			}
		}
		return ""
	}
	return cwdFromText(system.String())
}

func cwdFromText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		for _, marker := range []string{
			"current working directory:",
			"current directory:",
			"working directory:",
			"workspace:",
			"cwd:",
			"<cwd>",
		} {
			if !strings.Contains(lower, marker) {
				continue
			}
			candidate := strings.TrimSpace(line[strings.Index(lower, marker)+len(marker):])
			if marker == "<cwd>" {
				if end := strings.Index(strings.ToLower(candidate), "</cwd>"); end >= 0 {
					candidate = candidate[:end]
				}
			}
			if cwd := cleanTraeCLIClientCWD(candidate); cwd != "" {
				return cwd
			}
		}
	}
	return ""
}

func cleanTraeCLIClientCWD(cwd string) string {
	cwd = strings.TrimSpace(cwd)
	cwd = strings.Trim(cwd, "`\"'")
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	if stat, err := os.Stat(cwd); err != nil || !stat.IsDir() {
		return ""
	}
	return filepath.Clean(cwd)
}

func (e *TraeCLIExecutor) execSandbox(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["exec_sandbox"]); v != "" {
			return v
		}
	}
	if e != nil && e.cfg != nil {
		if v := strings.TrimSpace(e.cfg.TraeCLI.ExecSandbox); v != "" {
			return v
		}
	}
	return traeCLIDefaultExecSandbox
}

func (e *TraeCLIExecutor) execPermissionMode(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["exec_permission_mode"]); v != "" {
			return v
		}
	}
	if e != nil && e.cfg != nil {
		return strings.TrimSpace(e.cfg.TraeCLI.ExecPermission)
	}
	return ""
}

func (e *TraeCLIExecutor) execExtraArgs(auth *cliproxyauth.Auth) []string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["exec_extra_args"]); raw != "" {
			var out []string
			if err := json.Unmarshal([]byte(raw), &out); err == nil {
				return cleanTraeCLIExecArgs(out)
			}
		}
	}
	if e != nil && e.cfg != nil {
		return cleanTraeCLIExecArgs(e.cfg.TraeCLI.ExecExtraArgs)
	}
	return nil
}

func (e *TraeCLIExecutor) execEnv(ctx context.Context, auth *cliproxyauth.Auth) []string {
	env := os.Environ()
	if e.execInheritShellEnv(auth) {
		if shellEnv, err := e.captureShellEnv(ctx, auth); err == nil && len(shellEnv) > 0 {
			env = mergeTraeCLIEnv(env, shellEnv)
		} else if err != nil {
			log.Warnf("traecli executor: failed to capture shell environment: %v", err)
		}
	}
	overrides := e.execEnvOverrides(auth)
	if len(overrides) == 0 {
		return env
	}
	return mergeTraeCLIEnv(env, envMapToSlice(overrides, true))
}

func mergeTraeCLIEnv(base, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overrides))
	overrideMap := make(map[string]string, len(overrides))
	for _, item := range overrides {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			continue
		}
		overrideMap[key] = value
	}
	seen := make(map[string]struct{}, len(overrideMap))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			out = append(out, item)
			continue
		}
		if value, exists := overrideMap[key]; exists {
			out = append(out, key+"="+value)
			seen[key] = struct{}{}
			continue
		}
		out = append(out, item)
	}
	for key, value := range overrideMap {
		if _, exists := seen[key]; exists {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
}

func envMapToSlice(in map[string]string, expand bool) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for key, value := range in {
		if expand {
			value = expandTraeCLIEnvValue(value)
		}
		out = append(out, key+"="+value)
	}
	return out
}

func (e *TraeCLIExecutor) captureShellEnv(ctx context.Context, auth *cliproxyauth.Auth) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	shell := e.execShell(auth)
	if shell == "" {
		return nil, fmt.Errorf("shell is empty")
	}
	cmd := osexec.CommandContext(ctx, shell, "-lic", "env -0")
	raw, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(raw, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 || !bytes.Contains(part, []byte("=")) {
			continue
		}
		out = append(out, string(part))
	}
	return out, nil
}

func (e *TraeCLIExecutor) execInheritShellEnv(auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["exec_inherit_shell_env"]); v != "" {
			return parseTraeCLIBool(v)
		}
	}
	return e != nil && e.cfg != nil && e.cfg.TraeCLI.ExecInheritShellEnv
}

func (e *TraeCLIExecutor) execShell(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["exec_shell"]); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	if e != nil && e.cfg != nil {
		if v := strings.TrimSpace(e.cfg.TraeCLI.ExecShell); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func (e *TraeCLIExecutor) execEnvOverrides(auth *cliproxyauth.Auth) map[string]string {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["exec_env"]); raw != "" {
			var out map[string]string
			if err := json.Unmarshal([]byte(raw), &out); err == nil {
				return cleanTraeCLIExecEnv(out)
			}
		}
	}
	if e != nil && e.cfg != nil {
		return cleanTraeCLIExecEnv(e.cfg.TraeCLI.ExecEnv)
	}
	return nil
}

func cleanTraeCLIExecEnv(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		key = strings.TrimSpace(key)
		if key == "" || strings.Contains(key, "=") {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func expandTraeCLIEnvValue(value string) string {
	value = strings.ReplaceAll(value, "${PATH}", os.Getenv("PATH"))
	value = strings.ReplaceAll(value, "$PATH", os.Getenv("PATH"))
	return os.ExpandEnv(value)
}

func parseTraeCLIBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func (e *TraeCLIExecutor) execArgs(auth *cliproxyauth.Auth, model, outputFile, workDir string) []string {
	extraArgs := e.execExtraArgs(auth)
	bypassSandbox := traeCLIExecArgsBypassSandbox(extraArgs)
	requestedModel := model
	model = e.execModelName(model)
	args := []string{
		"exec",
		"--skip-git-repo-check",
		"--ephemeral",
		"--model", model,
		"--config", "model_provider='trae'",
		"--output-last-message", outputFile,
	}
	if variant := e.backendVariantForModel(auth, requestedModel); variant != "" {
		args = append(args, "--config", fmt.Sprintf("model_backend_variant='%s'", variant))
	}
	if sandbox := e.execSandbox(auth); sandbox != "" && !bypassSandbox {
		args = append(args, "--sandbox", sandbox)
	}
	if permissionMode := e.execPermissionMode(auth); permissionMode != "" && !bypassSandbox {
		args = append(args, "--permission-mode", permissionMode)
	}
	if workDir != "" {
		args = append(args, "--cd", workDir)
	}
	args = append(args, extraArgs...)
	args = append(args, "-")
	return args
}

func cleanTraeCLIExecArgs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, arg := range in {
		if strings.TrimSpace(arg) == "" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func traeCLIExecArgsBypassSandbox(args []string) bool {
	for _, arg := range args {
		if strings.TrimSpace(arg) == "--dangerously-bypass-approvals-and-sandbox" {
			return true
		}
	}
	return false
}

func (e *TraeCLIExecutor) backendVariant(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if variant := strings.TrimSpace(auth.Attributes["backend_variant"]); variant != "" {
			return variant
		}
	}
	if e != nil && e.cfg != nil {
		return strings.TrimSpace(e.cfg.TraeCLI.BackendVariant)
	}
	return ""
}

func (e *TraeCLIExecutor) backendVariantForModel(auth *cliproxyauth.Auth, model string) string {
	if entry, ok := e.configuredModelEntry(model); ok {
		modelName := strings.TrimSpace(entry.ModelName)
		if variantIndex := strings.LastIndex(modelName, "__"); variantIndex > 0 {
			switch strings.ToLower(strings.TrimSpace(modelName[variantIndex+2:])) {
			case "max":
				return "max"
			case "dev", "standard":
				return ""
			}
		}
	}
	variant := strings.TrimSpace(e.backendVariant(auth))
	if !strings.EqualFold(variant, "max") {
		return variant
	}
	metadata, ok := e.modelMetadata(auth, model)
	if !ok {
		return variant
	}
	variants := metadata.Get("business_metadata.variants")
	if variants.Exists() && strings.TrimSpace(variants.Get("max_key").String()) == "" && strings.TrimSpace(variants.Get("standard_key").String()) != "" {
		return ""
	}
	return variant
}

func createTraeCLIExecOutputFile() (string, error) {
	f, err := os.CreateTemp("", "cliproxy-traecli-exec-*.txt")
	if err != nil {
		return "", fmt.Errorf("traecli executor: create exec output file: %w", err)
	}
	path := f.Name()
	if errClose := f.Close(); errClose != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("traecli executor: close exec output file: %w", errClose)
	}
	return path, nil
}

func buildTraeCLIExecPrompt(payload []byte) string {
	var sections []string
	if system := openAIChatSystemText(payload); system != "" {
		sections = append(sections, system)
	}
	if history := openAIChatConversationText(payload); history != "" {
		sections = append(sections, history)
	}
	return strings.TrimSpace(strings.Join(sections, "\n\n"))
}

func openAIChatSystemText(payload []byte) string {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return ""
	}
	var parts []string
	for _, msg := range messages.Array() {
		if msg.Get("role").String() != "system" {
			continue
		}
		if text := openAIContentText(msg.Get("content")); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func openAIChatConversationText(payload []byte) string {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return strings.TrimSpace(latestOpenAIUserText(payload))
	}
	var lines []string
	for _, msg := range messages.Array() {
		role := strings.TrimSpace(msg.Get("role").String())
		if role == "" || role == "system" {
			continue
		}
		text := strings.TrimSpace(openAIContentText(msg.Get("content")))
		if text == "" {
			continue
		}
		switch role {
		case "assistant":
			lines = append(lines, "Assistant: "+text)
		case "tool":
			if toolID := msg.Get("tool_call_id").String(); toolID != "" {
				lines = append(lines, "Tool "+toolID+": "+text)
			} else {
				lines = append(lines, "Tool: "+text)
			}
		default:
			lines = append(lines, "User: "+text)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n\n"))
}

func traeCLIExecErrorMessage(err error, output []byte) string {
	msg := strings.TrimSpace(string(output))
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if msg == "" {
		msg = "traecli exec failed"
	}
	return msg
}

func (e *TraeCLIExecutor) presetBody(auth *cliproxyauth.Auth) []byte {
	path := ""
	if auth != nil && auth.Attributes != nil {
		path = strings.TrimSpace(auth.Attributes["preset_file"])
	}
	if path == "" && e != nil && e.cfg != nil {
		path = strings.TrimSpace(e.cfg.TraeCLI.PresetFile)
	}
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(expandTraeCLIPath(path))
	if err != nil || !json.Valid(raw) {
		return nil
	}
	return bytes.Clone(raw)
}

func rebuildTraeCLIPresetMessages(body []byte, text string) []byte {
	messages := gjson.GetBytes(body, "messages").Array()
	out := []byte("[]")
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Get("role").String() == "user" {
			lastUser = i
			break
		}
	}
	idx := -1
	limit := len(messages)
	if lastUser >= 0 {
		limit = lastUser
	}
	for i := 0; i < limit; i++ {
		idx++
		out, _ = sjson.SetRawBytes(out, fmt.Sprintf("%d", idx), []byte(messages[i].Raw))
	}
	idx++
	out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.role", idx), "user")
	out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.content.0.type", idx), "text")
	out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.content.0.text", idx), text)
	out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.content.0.cache_control.type", idx), "ephemeral")
	body, _ = sjson.SetRawBytes(body, "messages", out)
	return body
}

func (e *TraeCLIExecutor) applyRawChatHeaders(req *http.Request, auth *cliproxyauth.Auth) {
	req.Header.Set("Version", e.clientVersion())
	req.Header.Set("X-App-Id", e.appID(auth))
	req.Header.Set("X-IDE-Function", e.functionName(auth))
	req.Header.Set("X-IDE-Version-Code", time.Now().Format("20060102"))
	req.Header.Set("X-Flow-Traceparent", traeCLITraceparent(uuid.NewString()))
	req.Header.Set("Originator", "codex_exec")
	req.Header.Set("User-Agent", fmt.Sprintf("%s/%s (Ubuntu 20.4.0; x86_64) xterm-256color (%s; %s)", traeCLIDefaultUserAgentPrefix, e.clientVersion(), traeCLIDefaultUserAgentPrefix, e.clientVersion()))
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
}

func applyTraeCLITraceHeaderFromBody(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	if sessionID := strings.TrimSpace(gjson.GetBytes(body, "session_id").String()); sessionID != "" {
		req.Header.Set("X-Flow-Traceparent", traeCLITraceparent(sessionID))
	}
}

func applyTraeCLIRequestIdentityHeaders(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	sessionID := strings.TrimSpace(gjson.GetBytes(body, "session_id").String())
	conversationID := strings.TrimSpace(gjson.GetBytes(body, "conversation_id").String())
	if sessionID != "" {
		req.Header.Set("Session_id", sessionID)
		req.Header.Set("Thread-Id", sessionID)
		if req.Header.Get("X-Client-Request-Id") == "" {
			req.Header.Set("X-Client-Request-Id", stableTraeCLIRequestID(sessionID, body))
		}
	}
	if conversationID != "" {
		req.Header.Set("Conversation_id", conversationID)
	}
}

func applyTraeCLIRepoHeaders(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	repos := gjson.GetBytes(body, "biz_context.repo_urls")
	if !repos.IsArray() {
		return
	}
	var values []string
	for _, repo := range repos.Array() {
		if value := strings.TrimSpace(repo.String()); value != "" {
			values = append(values, value)
		}
	}
	if len(values) > 0 {
		req.Header.Set("X-Custom-Repo-Urls", strings.Join(values, ","))
	}
}

func stableTraeCLIRequestID(sessionID string, body []byte) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return uuid.NewString()
	}
	userInput := strings.TrimSpace(gjson.GetBytes(body, "user_input").String())
	messagesRaw := strings.TrimSpace(gjson.GetBytes(body, "messages").Raw)
	sum := sha256.Sum256([]byte(sessionID + "\n" + userInput + "\n" + messagesRaw))
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:traecli:request:"+hex.EncodeToString(sum[:]))).String()
}

func traeCLITraceparent(sessionID string) string {
	traceID := strings.ReplaceAll(strings.TrimSpace(sessionID), "-", "")
	if len(traceID) < 16 {
		traceID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if len(traceID) > 32 {
		traceID = traceID[:32]
	}
	for len(traceID) < 32 {
		traceID += "0"
	}
	return "00-" + traceID + "-" + traceID[:16] + "-01"
}

func (e *TraeCLIExecutor) traeToken(auth *cliproxyauth.Auth) (string, error) {
	path := e.traeAuthFile(auth)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("traecli executor: read auth file %s: %w", path, err)
	}
	token := strings.TrimSpace(gjson.GetBytes(raw, "trae.access_token").String())
	if token == "" {
		return "", fmt.Errorf("traecli executor: missing trae.access_token in %s", path)
	}
	return token, nil
}

func (e *TraeCLIExecutor) baseURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return v
		}
	}
	if e != nil && e.cfg != nil {
		if v := strings.TrimSpace(e.cfg.TraeCLI.BaseURL); v != "" {
			return v
		}
	}
	return traeCLIDefaultBaseURL
}

func (e *TraeCLIExecutor) appID(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["app_id"]); v != "" {
			return v
		}
	}
	if e != nil && e.cfg != nil {
		if v := strings.TrimSpace(e.cfg.TraeCLI.AppID); v != "" {
			return v
		}
	}
	return traeCLIDefaultAppID
}

func (e *TraeCLIExecutor) functionName(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["function"]); v != "" {
			return v
		}
	}
	if e != nil && e.cfg != nil {
		if v := strings.TrimSpace(e.cfg.TraeCLI.Function); v != "" {
			return v
		}
	}
	return traeCLIDefaultFunction
}

func (e *TraeCLIExecutor) clientVersion() string {
	if e != nil && e.cfg != nil {
		if raw, err := os.ReadFile(traeCLIModelsCachePath(nil, e.cfg)); err == nil {
			if v := strings.TrimSpace(gjson.GetBytes(raw, "client_version").String()); v != "" {
				return v
			}
		}
	}
	return "0.200.17"
}

func (e *TraeCLIExecutor) baseInstructions(auth *cliproxyauth.Auth, model string) string {
	metadata, ok := e.modelMetadata(auth, model)
	if !ok {
		return ""
	}
	return strings.TrimSpace(metadata.Get("base_instructions").String())
}

func (e *TraeCLIExecutor) rawConfigName(auth *cliproxyauth.Auth, model string) string {
	metadata, ok := e.modelMetadata(auth, model)
	if !ok {
		return model
	}
	if configName := strings.TrimSpace(metadata.Get("config_name").String()); configName != "" {
		return configName
	}
	return model
}

func (e *TraeCLIExecutor) rawReasoningEffort(auth *cliproxyauth.Auth, model, requested string) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		requested = "xhigh"
	}
	metadata, ok := e.modelMetadata(auth, model)
	if !ok {
		return requested
	}
	for _, level := range metadata.Get("supported_reasoning_levels").Array() {
		if strings.EqualFold(strings.TrimSpace(level.Get("effort").String()), requested) {
			return requested
		}
	}
	if defaultEffort := strings.ToLower(strings.TrimSpace(metadata.Get("default_reasoning_level").String())); defaultEffort != "" {
		return defaultEffort
	}
	return requested
}

func (e *TraeCLIExecutor) modelMetadata(auth *cliproxyauth.Auth, model string) (gjson.Result, bool) {
	model = e.metadataModelName(model)
	path := traeCLIModelsCachePath(auth, nil)
	if e != nil {
		path = traeCLIModelsCachePath(auth, e.cfg)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return gjson.Result{}, false
	}
	for _, entry := range gjson.GetBytes(raw, "models").Array() {
		configName := strings.TrimSpace(entry.Get("config_name").String())
		slug := strings.TrimSpace(entry.Get("slug").String())
		if strings.EqualFold(configName, model) || strings.EqualFold(slug, model) {
			return entry, true
		}
	}
	return gjson.Result{}, false
}

func (e *TraeCLIExecutor) metadataModelName(model string) string {
	model = strings.TrimSpace(model)
	entry, ok := e.configuredModelEntry(model)
	if !ok {
		return model
	}
	mapped := strings.TrimSpace(entry.ModelName)
	if mapped == "" {
		if primary := traeCLIModelEntryPrimaryName(entry); primary != "" {
			return primary
		}
		return model
	}
	if variantIndex := strings.LastIndex(mapped, "__"); variantIndex > 0 {
		mapped = strings.TrimSpace(mapped[:variantIndex])
	}
	return mapped
}

func (e *TraeCLIExecutor) execModelName(model string) string {
	model = strings.TrimSpace(model)
	entry, ok := e.configuredModelEntry(model)
	if !ok {
		return model
	}
	if mapped := strings.TrimSpace(entry.ModelName); mapped != "" {
		if variantIndex := strings.LastIndex(mapped, "__"); variantIndex > 0 {
			mapped = strings.TrimSpace(mapped[:variantIndex])
		}
		if mapped != "" {
			return mapped
		}
	}
	if primary := traeCLIModelEntryPrimaryName(entry); primary != "" {
		return primary
	}
	return model
}

func (e *TraeCLIExecutor) configuredModelEntry(model string) (config.TraeCLIModel, bool) {
	if e == nil || e.cfg == nil {
		return config.TraeCLIModel{}, false
	}
	return resolveTraeCLIModelEntry(e.cfg.TraeCLI.Models, model)
}

func traeCLIModelEntryPrimaryName(entry config.TraeCLIModel) string {
	if alias := strings.TrimSpace(entry.Alias); alias != "" {
		return alias
	}
	return strings.TrimSpace(entry.Name)
}

func resolveTraeCLIModelEntry(entries []config.TraeCLIModel, model string) (config.TraeCLIModel, bool) {
	model = strings.TrimSpace(model)
	for _, entry := range entries {
		if strings.EqualFold(traeCLIModelEntryPrimaryName(entry), model) {
			return entry, true
		}
	}
	for _, entry := range entries {
		for _, alias := range entry.Aliases {
			if strings.EqualFold(strings.TrimSpace(alias), model) {
				return entry, true
			}
		}
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Alias) != "" && strings.EqualFold(strings.TrimSpace(entry.Name), model) {
			return entry, true
		}
	}
	return config.TraeCLIModel{}, false
}

func (e *TraeCLIExecutor) rawModelName(auth *cliproxyauth.Auth, model string) string {
	if entry, ok := e.configuredModelEntry(model); ok {
		if v := strings.TrimSpace(entry.ModelName); v != "" {
			return v
		}
		if primary := traeCLIModelEntryPrimaryName(entry); primary != "" {
			model = primary
		}
	}
	if cachedModelName := e.cachedRawModelName(auth, model); cachedModelName != "" {
		return cachedModelName
	}
	if auth != nil && auth.Attributes != nil {
		if variant := strings.TrimSpace(auth.Attributes["backend_variant"]); variant != "" && !strings.Contains(model, "__") {
			return model + "__" + variant
		}
	}
	if e != nil && e.cfg != nil {
		if variant := strings.TrimSpace(e.cfg.TraeCLI.BackendVariant); variant != "" && !strings.Contains(model, "__") {
			return model + "__" + variant
		}
	}
	return model
}

func (e *TraeCLIExecutor) cachedRawModelName(auth *cliproxyauth.Auth, model string) string {
	metadata, ok := e.modelMetadata(auth, model)
	if !ok {
		return ""
	}
	variants := metadata.Get("business_metadata.variants")
	if !variants.Exists() {
		return ""
	}
	variant := strings.ToLower(strings.TrimSpace(e.backendVariant(auth)))
	if variant == "max" {
		if maxKey := strings.TrimSpace(variants.Get("max_key").String()); maxKey != "" {
			return maxKey
		}
	}
	if variant == "" || variant == "max" || variant == "dev" || variant == "standard" {
		return strings.TrimSpace(variants.Get("standard_key").String())
	}
	return ""
}

func (e *TraeCLIExecutor) traeAuthFile(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["auth_file"]); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	if e != nil && e.cfg != nil {
		if v := strings.TrimSpace(e.cfg.TraeCLI.AuthFile); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	return expandTraeCLIPath(traeCLIDefaultAuthFile)
}

func traeCLIModelsCachePath(auth *cliproxyauth.Auth, cfg *config.Config) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["models_cache"]); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	if cfg != nil {
		if v := strings.TrimSpace(cfg.TraeCLI.ModelsCache); v != "" {
			return expandTraeCLIPath(v)
		}
	}
	return expandTraeCLIPath(traeCLIDefaultModelsCache)
}

func expandTraeCLIPath(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			rest := strings.TrimLeft(strings.TrimPrefix(path, "~"), `/\`)
			if rest == "" {
				return filepath.Clean(home)
			}
			return filepath.Clean(filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(rest, "\\", "/"))))
		}
	}
	return filepath.Clean(path)
}

func openAIChatMessagesToTraeRaw(payload, original []byte, baseInstructions string) []byte {
	messages := gjson.GetBytes(payload, "messages")
	out := []byte("[]")
	idx := -1
	if systemContent := traeSystemContent(original, baseInstructions); len(systemContent) > 2 {
		idx++
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.role", idx), "system")
		out, _ = sjson.SetRawBytes(out, fmt.Sprintf("%d.content", idx), systemContent)
	}
	if !messages.IsArray() {
		return out
	}
	for _, msg := range messages.Array() {
		role := msg.Get("role").String()
		idx++
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.role", idx), role)
		content := msg.Get("content")
		contentRaw := traeRawContentFromOpenAI(content)
		out, _ = sjson.SetRawBytes(out, fmt.Sprintf("%d.content", idx), contentRaw)
		if role == "system" {
			out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.content.0.response_api_role", idx), "developer")
		}
		if name := msg.Get("name").String(); name != "" {
			out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.name", idx), name)
		}
		if toolCallID := msg.Get("tool_call_id").String(); toolCallID != "" {
			out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.tool_call_id", idx), toolCallID)
		}
		if toolCalls := msg.Get("tool_calls"); toolCalls.Exists() {
			out, _ = sjson.SetRawBytes(out, fmt.Sprintf("%d.tool_calls", idx), traeToolCallsForRawChat(toolCalls))
		}
	}
	return out
}

func traeToolCallsForRawChat(toolCalls gjson.Result) []byte {
	out := []byte("[]")
	if !toolCalls.IsArray() {
		return out
	}
	idx := -1
	for _, toolCall := range toolCalls.Array() {
		idx++
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.index", idx), idx)
		if rawIndex := toolCall.Get("index"); rawIndex.Exists() {
			out, _ = sjson.SetRawBytes(out, fmt.Sprintf("%d.index", idx), []byte(rawIndex.Raw))
		}
		if id := strings.TrimSpace(toolCall.Get("id").String()); id != "" {
			out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.id", idx), id)
		}
		typ := strings.TrimSpace(toolCall.Get("type").String())
		if typ == "" {
			typ = "function"
		}
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.type", idx), typ)
		if name := firstStringResult(toolCall, "function_call.name", "function.name", "name"); name != "" {
			out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.function_call.name", idx), name)
		}
		args := strings.TrimSpace(traeCLIToolCallArguments(toolCall))
		if args == "" {
			args = "{}"
		}
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.function_call.arguments", idx), args)
		out, _ = sjson.SetRawBytes(out, fmt.Sprintf("%d.function_call.partial_arguments", idx), []byte("null"))
	}
	return out
}

func traeSystemContent(original []byte, baseInstructions string) []byte {
	out := []byte("[]")
	idx := -1
	appendText := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		idx++
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.type", idx), "text")
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.text", idx), text)
	}
	baseInstructions = strings.TrimSpace(baseInstructions)
	claudeSystemContent := claudeSystemToTraeContent(original)
	claudeSystemText := openAIContentText(gjson.ParseBytes(claudeSystemContent))
	if baseInstructions != "" && !strings.Contains(claudeSystemText, baseInstructions) {
		appendText(baseInstructions)
	}
	for _, part := range gjson.ParseBytes(claudeSystemContent).Array() {
		if part.Get("type").String() == "text" {
			appendText(part.Get("text").String())
		}
	}
	return out
}

func claudeSystemToTraeContent(original []byte) []byte {
	system := gjson.GetBytes(original, "system")
	if !system.Exists() {
		return []byte("[]")
	}
	out := []byte("[]")
	idx := -1
	appendText := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		idx++
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.type", idx), "text")
		out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.text", idx), text)
	}
	if system.IsArray() {
		for _, part := range system.Array() {
			if part.Get("type").String() == "text" {
				appendText(part.Get("text").String())
			}
		}
		return out
	}
	appendText(system.String())
	return out
}

func traeRawContentFromOpenAI(content gjson.Result) []byte {
	out := []byte("[]")
	if !content.Exists() {
		return out
	}
	if content.IsArray() {
		idx := -1
		for _, part := range content.Array() {
			partType := part.Get("type").String()
			switch partType {
			case "text", "input_text":
				text := part.Get("text").String()
				if text == "" {
					continue
				}
				idx++
				out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.type", idx), "text")
				out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.text", idx), text)
				out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.cache_control.type", idx), "ephemeral")
			case "image_url":
				url := part.Get("image_url.url").String()
				if url == "" {
					continue
				}
				idx++
				out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.type", idx), "image_url")
				out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.image_url.url", idx), url)
				if detail := part.Get("image_url.detail").String(); detail != "" {
					out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.image_url.detail", idx), detail)
				}
			case "tool_use":
				idx++
				out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.type", idx), "tool_use")
				if id := strings.TrimSpace(part.Get("id").String()); id != "" {
					out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.id", idx), id)
				}
				if name := strings.TrimSpace(part.Get("name").String()); name != "" {
					out, _ = sjson.SetBytes(out, fmt.Sprintf("%d.name", idx), name)
				}
				input := part.Get("input")
				if input.Exists() && input.IsObject() {
					out, _ = sjson.SetRawBytes(out, fmt.Sprintf("%d.input", idx), []byte(input.Raw))
				} else {
					out, _ = sjson.SetRawBytes(out, fmt.Sprintf("%d.input", idx), []byte("{}"))
				}
			}
		}
		return out
	}
	text := content.String()
	if content.Type == gjson.String {
		out, _ = sjson.SetBytes(out, "0.type", "text")
		out, _ = sjson.SetBytes(out, "0.text", text)
		if text != "" {
			out, _ = sjson.SetBytes(out, "0.cache_control.type", "ephemeral")
		}
	}
	return out
}

func latestOpenAIUserText(payload []byte) string {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return ""
	}
	items := messages.Array()
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Get("role").String() == "user" {
			return openAIUserContentText(items[i].Get("content"))
		}
	}
	return ""
}

func openAIUserContentText(content gjson.Result) string {
	if text := openAIContentText(content); text != "" {
		return text
	}
	if !content.IsArray() {
		return ""
	}
	var parts []string
	for _, part := range content.Array() {
		if part.Get("type").String() != "tool_result" {
			continue
		}
		if text := openAIContentText(part.Get("content")); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func openAIContentText(content gjson.Result) string {
	if !content.Exists() {
		return ""
	}
	if !content.IsArray() {
		return content.String()
	}
	var parts []string
	for _, part := range content.Array() {
		if text := part.Get("text").String(); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func readTraeCLIResponseBody(body io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		line := scanner.Bytes()
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func traeCLIUpstreamErrorStatusCode(fallback int, raw []byte) int {
	if fallback == http.StatusTooManyRequests {
		return fallback
	}
	normalized := strings.ToLower(string(raw))
	for _, marker := range []string{
		"too_many_requests",
		"too many requests",
		"rate_limit_reached",
		"rate_limit_exceeded",
		"rate limit reached",
		"rate limit exceeded",
		"throughput limit",
		"quota exceeded",
	} {
		if strings.Contains(normalized, marker) {
			return http.StatusTooManyRequests
		}
	}
	// Trae can wrap request validation failures in a 502 api_error response.
	// Keep this provider-specific so generic upstream failures are not classified
	// from human-readable text across unrelated executors.
	wrappedErrorType := strings.TrimSpace(gjson.GetBytes(raw, "error.type").String())
	wrappedMessage := strings.ToLower(gjson.GetBytes(raw, "error.message").String())
	if fallback == http.StatusBadGateway && strings.EqualFold(wrappedErrorType, "api_error") && strings.Contains(wrappedMessage, "param is invalid") {
		return http.StatusBadRequest
	}
	// Do not expose private, unassigned upstream 5xx codes such as 515 to API
	// clients. The original status remains available in the upstream response log.
	if fallback >= 500 && fallback <= 599 && http.StatusText(fallback) == "" {
		return http.StatusBadGateway
	}
	return fallback
}

// withTraeCLIRetryAfter keeps retry hints on both HTTP errors and errors carried
// inside a successful HTTP/SSE response, after TRAE status normalization.
func withTraeCLIRetryAfter(ctx context.Context, err error, headers http.Header) error {
	status, ok := err.(statusErr)
	if !ok || (status.code != http.StatusTooManyRequests && status.code != http.StatusServiceUnavailable) {
		return err
	}
	status.retryAfter = helps.ParseRetryAfter(ctx, headers.Get("Retry-After"), time.Now())
	return status
}

func traeCLIStatusError(raw []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || !json.Valid(data) {
			continue
		}
		if code := gjson.GetBytes(data, "code"); code.Exists() {
			msg := gjson.GetBytes(data, "message").String()
			if msg == "" {
				msg = gjson.GetBytes(data, "error").String()
			}
			if msg == "" {
				msg = string(data)
			}
			return statusErr{code: traeCLIUpstreamErrorStatusCode(http.StatusBadGateway, data), msg: msg}
		}
	}
	return nil
}

type traeCLIRawChatPayload struct {
	Text      string
	Usage     openAIUsage
	UsageSeen bool
	ToolCalls [][]byte
	ExtraInfo []byte
	Completed bool
}

type traeCLIToolCallAccumulator struct {
	ID        string
	Type      string
	Name      string
	Arguments string
}

type traeCLIRawChatAccumulator struct {
	ctx             context.Context
	originalRequest []byte
	eventName       string
	eventData       [][]byte
	completed       bool
	textParts       []string
	usage           openAIUsage
	usageSeen       bool
	toolCalls       map[int]*traeCLIToolCallAccumulator
	extraInfo       []byte
	lastStatus      string
}

func newTraeCLIRawChatAccumulator(ctx context.Context, originalRequest []byte) *traeCLIRawChatAccumulator {
	return &traeCLIRawChatAccumulator{
		ctx:             ctx,
		originalRequest: originalRequest,
		toolCalls:       make(map[int]*traeCLIToolCallAccumulator),
	}
}

func newTraeCLIStreamError(ctx context.Context, message string) error {
	err := statusErr{code: http.StatusBadGateway, msg: "traecli executor: " + message}
	logs.CtxError(ctx, "%v", err)
	return err
}

// consumeLine preserves the SSE event name and joins data lines before parsing.
// Completion is recognized only from a dispatched frame, never a bare EOF.
func (a *traeCLIRawChatAccumulator) consumeLine(line []byte) (string, error) {
	line = bytes.TrimSpace(line)
	if len(line) != 0 {
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			a.eventName = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			a.eventData = append(a.eventData, bytes.Clone(bytes.TrimSpace(line[len("data:"):])))
		}
		return "", nil
	}
	eventName, data := a.eventName, bytes.Join(a.eventData, []byte("\n"))
	hasData := len(a.eventData) > 0
	a.eventName, a.eventData = "", nil
	if !hasData {
		return "", nil
	}
	// TRAE sends plain-text progress notices such as ;Processing_<id>.
	// They are informational and cannot complete a response.
	if eventName == "progress_notice" {
		return "", nil
	}
	delta, err := a.consume(data)
	if err == nil && eventName == "done" && json.Valid(data) {
		a.completed = true
	}
	return delta, err
}

func (a *traeCLIRawChatAccumulator) consume(data []byte) (string, error) {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		a.completed = true
		return "", nil
	}
	if len(data) == 0 {
		return "", nil
	}
	if !json.Valid(data) {
		return "", newTraeCLIStreamError(a.ctx, "invalid or incomplete SSE data")
	}
	if code := gjson.GetBytes(data, "code"); code.Exists() {
		msg := gjson.GetBytes(data, "message").String()
		if msg == "" {
			msg = gjson.GetBytes(data, "error").String()
		}
		err := statusErr{code: traeCLIUpstreamErrorStatusCode(http.StatusBadGateway, data), msg: msg}
		logs.CtxError(a.ctx, "traecli executor: upstream error (status %d)", err.code)
		return "", err
	}
	delta := firstStringJSONPath(data,
		"delta", "content", "answer", "text", "message", "response", "data.delta", "data.content",
		"data.answer", "data.text", "output", "choices.0.delta.content")
	eventRoot := gjson.ParseBytes(data)
	for _, path := range []string{"finish_reason", "data.finish_reason", "choices.0.finish_reason"} {
		if reason := eventRoot.Get(path); reason.Type == gjson.String && strings.TrimSpace(reason.String()) != "" {
			a.completed = true
		}
	}
	if isTraeCLIQueueStatusEvent(eventRoot) {
		if delta == a.lastStatus {
			delta = ""
		} else {
			a.lastStatus = delta
			delta = strings.TrimRight(delta, "\r\n") + "\n"
		}
	} else if delta != "" {
		a.lastStatus = ""
	}
	if delta != "" {
		a.textParts = append(a.textParts, delta)
	}
	if u := traeCLIUsageFromEvent(eventRoot); u.Exists() {
		a.usage = traeCLIOpenAIUsageSnapshot(u)
		a.usageSeen = true
	}
	if extraInfo := traeCLIExtraInfoFromEvent(eventRoot); len(extraInfo) > 0 {
		a.extraInfo = extraInfo
	}
	collectTraeCLIToolCalls(eventRoot, a.toolCalls, a.originalRequest)
	return delta, nil
}

func traeCLIOpenAIUsageSnapshot(raw gjson.Result) openAIUsage {
	promptTokens := raw.Get("prompt_tokens")
	if !promptTokens.Exists() {
		promptTokens = raw.Get("input_tokens")
	}
	completionTokens := raw.Get("completion_tokens")
	reasoningTokens := firstExistingJSONResult(raw,
		"completion_tokens_details.reasoning_tokens",
		"output_tokens_details.reasoning_tokens",
		"reasoning_output_tokens",
		"reasoning_tokens",
	)
	completionTokenCount := completionTokens.Int()
	if !completionTokens.Exists() {
		completionTokenCount = raw.Get("output_tokens").Int()
		if separateReasoningTokens := raw.Get("reasoning_output_tokens"); separateReasoningTokens.Exists() {
			completionTokenCount += separateReasoningTokens.Int()
		}
	}
	cachedTokens := firstExistingJSONResult(raw,
		"prompt_tokens_details.cached_tokens",
		"input_tokens_details.cached_tokens",
		"cached_input_tokens",
		"cached_tokens",
		"cache_read_input_tokens",
	)
	cacheCreationTokens := firstExistingJSONResult(raw,
		"prompt_tokens_details.cached_creation_tokens",
		"prompt_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_write_tokens",
		"input_tokens_details.cached_creation_tokens",
		"input_tokens_details.cache_creation_tokens",
		"input_tokens_details.cache_write_tokens",
		"cache_creation_input_tokens",
	)
	usage := openAIUsage{
		PromptTokens:        promptTokens.Int(),
		CompletionTokens:    completionTokenCount,
		CachedTokens:        cachedTokens.Int(),
		CacheCreationTokens: cacheCreationTokens.Int(),
		ReasoningTokens:     reasoningTokens.Int(),
	}
	if totalTokens := raw.Get("total_tokens"); totalTokens.Exists() {
		usage.TotalTokens = totalTokens.Int()
	} else {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}

func isTraeCLIQueueStatusEvent(root gjson.Result) bool {
	position := root.Get("position")
	message := root.Get("message")
	queueID := root.Get("queue_id")
	return position.Exists() && position.Type == gjson.Number && message.Type == gjson.String && queueID.Type == gjson.String
}

func (a *traeCLIRawChatAccumulator) result(originalRequest []byte) traeCLIRawChatPayload {
	if a == nil {
		return traeCLIRawChatPayload{}
	}
	usage := a.usage
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return traeCLIRawChatPayload{
		Text:      strings.Join(a.textParts, ""),
		Usage:     usage,
		UsageSeen: a.usageSeen,
		ToolCalls: buildTraeCLIToolCallPayloads(a.toolCalls, originalRequest),
		ExtraInfo: bytes.Clone(a.extraInfo),
		Completed: a.completed,
	}
}

func parseTraeCLIRawChatPayload(raw []byte) (string, openAIUsage, error) {
	parsed, err := parseTraeCLIRawChatPayloadDetailed(raw, nil)
	return parsed.Text, parsed.Usage, err
}

func parseTraeCLIRawChatPayloadDetailed(raw, originalRequest []byte) (traeCLIRawChatPayload, error) {
	accumulator := newTraeCLIRawChatAccumulator(context.Background(), originalRequest)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(nil, 52_428_800)
	for scanner.Scan() {
		if _, errConsume := accumulator.consumeLine(scanner.Bytes()); errConsume != nil {
			return accumulator.result(originalRequest), errConsume
		}
	}
	if err := scanner.Err(); err != nil {
		logs.CtxError(accumulator.ctx, "traecli executor: parse stream: %v", err)
		return accumulator.result(originalRequest), err
	}
	if _, errConsume := accumulator.consumeLine(nil); errConsume != nil {
		return accumulator.result(originalRequest), errConsume
	}
	return accumulator.result(originalRequest), nil
}

func traeCLIUsageFromEvent(root gjson.Result) gjson.Result {
	if u := firstJSONResult(root, "usage", "token_usage", "data.usage", "data.token_usage"); u.Exists() {
		return u
	}
	if root.Get("prompt_tokens").Exists() || root.Get("completion_tokens").Exists() || root.Get("total_tokens").Exists() {
		return root
	}
	return gjson.Result{}
}

func traeCLIExtraInfoFromEvent(root gjson.Result) []byte {
	for _, path := range []string{
		"extra_info",
		"data.extra_info",
		"message.extra_info",
		"response.extra_info",
		"choices.0.delta.extra_info",
		"choices.0.message.extra_info",
	} {
		value := root.Get(path)
		if !value.Exists() || value.Type != gjson.JSON {
			continue
		}
		raw := []byte(value.Raw)
		if json.Valid(raw) {
			return bytes.Clone(raw)
		}
	}
	return nil
}

func firstStringJSONPath(raw []byte, paths ...string) string {
	for _, path := range paths {
		if result := gjson.GetBytes(raw, path); result.Exists() && result.Type == gjson.String {
			return result.String()
		}
	}
	return ""
}

func firstStringResult(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if result := root.Get(path); result.Exists() && result.Type == gjson.String {
			return result.String()
		}
	}
	return ""
}

func firstExistingJSONResult(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if result := root.Get(path); result.Exists() {
			return result
		}
	}
	return gjson.Result{}
}

func firstJSONResult(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if result := root.Get(path); result.Exists() && result.Type == gjson.JSON {
			return result
		}
	}
	return gjson.Result{}
}

func collectTraeCLIToolCalls(root gjson.Result, accumulators map[int]*traeCLIToolCallAccumulator, originalRequest []byte) {
	for _, path := range []string{
		"choices.0.delta.tool_calls",
		"data.delta.tool_calls",
	} {
		mergeTraeCLIToolCalls(root.Get(path), accumulators, true)
	}
	if isTraeCLIToolCallDeltaEvent(root) {
		for _, path := range []string{
			"data.tool_calls",
			"tool_calls",
		} {
			node := root.Get(path)
			merge := func(index int, toolCall gjson.Result) {
				// Ignore only an identical, complete ordinary call. A JSON-shaped
				// fragment can belong to a nested object or string in an active
				// delta stream, so it must never replace accumulated arguments.
				id := firstStringResult(toolCall, "id", "tool_call_id")
				name := firstStringResult(toolCall, "function.name", "function_call.name", "name")
				args := traeCLIToolCallArguments(toolCall)
				if id != "" && name != "" && json.Valid([]byte(args)) && gjson.Parse(args).IsObject() &&
					!openairesponses.IsCustomTool(originalRequest, strings.TrimSpace(name)) {
					for _, previous := range accumulators {
						if previous != nil && previous.ID == id && previous.Name == name && previous.Arguments == args {
							return
						}
					}
				}
				mergeTraeCLIToolCall(toolCall, accumulators, index, true)
			}
			if node.IsArray() {
				for index, toolCall := range node.Array() {
					merge(index, toolCall)
				}
			} else if node.IsObject() {
				merge(len(accumulators), node)
			}
		}
		return
	}
	for _, path := range []string{
		"choices.0.message.tool_calls",
		"message.tool_calls",
		"data.message.tool_calls",
		"data.tool_calls",
		"tool_calls",
	} {
		mergeTraeCLIToolCalls(root.Get(path), accumulators, false)
	}
	for _, path := range []string{"tool_call", "data.tool_call"} {
		mergeTraeCLIToolCalls(root.Get(path), accumulators, false)
	}
	for _, path := range []string{"functionCall", "function_call", "data.functionCall", "data.function_call"} {
		mergeTraeCLIFunctionCall(root.Get(path), accumulators)
	}
}

func isTraeCLIToolCallDeltaEvent(root gjson.Result) bool {
	if !root.Get("tool_calls").Exists() && !root.Get("data.tool_calls").Exists() {
		return false
	}
	if root.Get("choices").Exists() || root.Get("message.tool_calls").Exists() || root.Get("data.message.tool_calls").Exists() {
		return false
	}
	if root.Get("finish_reason").Exists() || root.Get("data.finish_reason").Exists() {
		return false
	}
	return true
}

func mergeTraeCLIToolCalls(node gjson.Result, accumulators map[int]*traeCLIToolCallAccumulator, appendArguments bool) {
	if !node.Exists() {
		return
	}
	if node.IsArray() {
		for i, toolCall := range node.Array() {
			mergeTraeCLIToolCall(toolCall, accumulators, i, appendArguments)
		}
		return
	}
	if node.IsObject() {
		mergeTraeCLIToolCall(node, accumulators, len(accumulators), appendArguments)
	}
}

func mergeTraeCLIToolCall(toolCall gjson.Result, accumulators map[int]*traeCLIToolCallAccumulator, fallbackIndex int, appendArguments bool) {
	if !toolCall.Exists() || !toolCall.IsObject() {
		return
	}
	index := fallbackIndex
	if rawIndex := toolCall.Get("index"); rawIndex.Exists() {
		index = int(rawIndex.Int())
	} else if rawIndex := toolCall.Get("tool_call_index"); rawIndex.Exists() {
		index = int(rawIndex.Int())
	} else if id := firstStringResult(toolCall, "id", "tool_call_id"); id != "" {
		for existingIndex, existing := range accumulators {
			if existing != nil && existing.ID == id {
				index = existingIndex
				break
			}
		}
	}
	if index < 0 {
		index = len(accumulators)
	}
	acc := accumulators[index]
	if acc == nil {
		acc = &traeCLIToolCallAccumulator{}
		accumulators[index] = acc
	}
	if id := firstStringResult(toolCall, "id", "tool_call_id"); id != "" {
		acc.ID = id
	}
	if typ := strings.TrimSpace(toolCall.Get("type").String()); typ != "" {
		acc.Type = typ
	}
	if name := firstStringResult(toolCall, "function.name", "function_call.name", "name"); name != "" {
		acc.Name = name
	}
	if args := traeCLIToolCallArguments(toolCall); args != "" {
		if appendArguments {
			acc.Arguments += args
		} else {
			acc.Arguments = args
		}
	}
}

func mergeTraeCLIFunctionCall(functionCall gjson.Result, accumulators map[int]*traeCLIToolCallAccumulator) {
	if !functionCall.Exists() || !functionCall.IsObject() {
		return
	}
	index := len(accumulators)
	if rawIndex := functionCall.Get("index"); rawIndex.Exists() {
		index = int(rawIndex.Int())
	}
	acc := accumulators[index]
	if acc == nil {
		acc = &traeCLIToolCallAccumulator{}
		accumulators[index] = acc
	}
	if id := firstStringResult(functionCall, "id", "call_id", "callId"); id != "" {
		acc.ID = id
	}
	acc.Type = "function"
	if name := firstStringResult(functionCall, "name", "function.name"); name != "" {
		acc.Name = name
	}
	if args := traeCLIFunctionCallArgs(functionCall); args != "" {
		acc.Arguments = args
	}
}

func traeCLIFunctionCallArgs(functionCall gjson.Result) string {
	for _, path := range []string{"args", "arguments", "input", "function.arguments"} {
		value := functionCall.Get(path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.String {
			return value.String()
		}
		if value.Raw != "" {
			return value.Raw
		}
	}
	return ""
}

func traeCLIToolCallArguments(toolCall gjson.Result) string {
	for _, path := range []string{"function.arguments", "function_call.arguments", "arguments", "input"} {
		value := toolCall.Get(path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.String {
			return value.String()
		}
		if value.Raw != "" {
			return value.Raw
		}
	}
	return ""
}

func buildTraeCLIToolCallPayloads(accumulators map[int]*traeCLIToolCallAccumulator, originalRequest []byte) [][]byte {
	if len(accumulators) == 0 {
		return nil
	}
	maxIndex := -1
	for index := range accumulators {
		if index > maxIndex {
			maxIndex = index
		}
	}
	out := make([][]byte, 0, len(accumulators))
	for index := 0; index <= maxIndex; index++ {
		acc := accumulators[index]
		if acc == nil || strings.TrimSpace(acc.Name) == "" {
			continue
		}
		id := strings.TrimSpace(acc.ID)
		if id == "" {
			id = "call_" + randomHex(8)
		}
		typ := strings.TrimSpace(acc.Type)
		if typ == "" {
			typ = "function"
		}
		args := acc.Arguments
		// TRAE may return raw freeform input through its function-call envelope.
		// Preserve every byte, including whitespace and an empty input; JSON
		// repair would change quotes and escapes inside patches or source code.
		if !openairesponses.IsCustomTool(originalRequest, strings.TrimSpace(acc.Name)) {
			args = strings.TrimSpace(args)
			if args == "" {
				args = "{}"
			} else {
				args = util.FixJSON(args)
			}
		}
		raw := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
		raw, _ = sjson.SetBytes(raw, "id", id)
		raw, _ = sjson.SetBytes(raw, "type", typ)
		raw, _ = sjson.SetBytes(raw, "function.name", strings.TrimSpace(acc.Name))
		raw, _ = sjson.SetBytes(raw, "function.arguments", args)
		out = append(out, raw)
	}
	return out
}

type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	CachedTokens        int64 `json:"-"`
	CacheCreationTokens int64 `json:"-"`
	ReasoningTokens     int64 `json:"-"`
}

func (u openAIUsage) MarshalJSON() ([]byte, error) {
	type baseUsage openAIUsage
	payload, errMarshal := json.Marshal(baseUsage(u))
	if errMarshal != nil {
		return nil, errMarshal
	}
	if u.CachedTokens > 0 {
		payload, _ = sjson.SetBytes(payload, "prompt_tokens_details.cached_tokens", u.CachedTokens)
	}
	if u.CacheCreationTokens > 0 {
		payload, _ = sjson.SetBytes(payload, "prompt_tokens_details.cached_creation_tokens", u.CacheCreationTokens)
	}
	if u.ReasoningTokens > 0 {
		payload, _ = sjson.SetBytes(payload, "completion_tokens_details.reasoning_tokens", u.ReasoningTokens)
	}
	return payload, nil
}

func buildTraeCLIOpenAIResponse(model string, result traeCLIRawChatPayload) ([]byte, error) {
	finishReason := "stop"
	if len(result.ToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	payload := map[string]any{
		"id":      "chatcmpl-traecli-" + randomHex(8),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": finishReason,
			"message": map[string]any{
				"role":    "assistant",
				"content": result.Text,
			},
		}},
		"usage": result.Usage,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(result.ToolCalls) == 0 {
		return raw, nil
	}
	toolCalls := []byte("[]")
	for _, toolCall := range result.ToolCalls {
		toolCalls, _ = sjson.SetRawBytes(toolCalls, "-1", toolCall)
	}
	raw, _ = sjson.SetRawBytes(raw, "choices.0.message.tool_calls", toolCalls)
	return raw, nil
}

func buildTraeCLIChatCompletionChunk(model, text string, done bool) []byte {
	delta := map[string]any{}
	finishReason := any(nil)
	if done {
		finishReason = "stop"
	} else {
		delta["content"] = text
	}
	payload := map[string]any{
		"id":      "chatcmpl-traecli-" + randomHex(8),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	raw, _ := json.Marshal(payload)
	return append(append([]byte("data: "), raw...), []byte("\n\n")...)
}

func buildTraeCLIChatCompletionToolCallChunk(model string, toolCalls [][]byte) []byte {
	payload := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":null}]}`)
	payload, _ = sjson.SetBytes(payload, "id", "chatcmpl-traecli-"+randomHex(8))
	payload, _ = sjson.SetBytes(payload, "created", time.Now().Unix())
	payload, _ = sjson.SetBytes(payload, "model", model)
	items := []byte("[]")
	for index, toolCall := range toolCalls {
		item := bytes.Clone(toolCall)
		item, _ = sjson.SetBytes(item, "index", index)
		items, _ = sjson.SetRawBytes(items, "-1", item)
	}
	payload, _ = sjson.SetRawBytes(payload, "choices.0.delta.tool_calls", items)
	return append(append([]byte("data: "), payload...), []byte("\n\n")...)
}

func buildTraeCLIChatCompletionFinishChunk(model, finishReason string, usage openAIUsage, usageSeen bool) []byte {
	if finishReason == "" {
		finishReason = "stop"
	}
	payload := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":""}]}`)
	payload, _ = sjson.SetBytes(payload, "id", "chatcmpl-traecli-"+randomHex(8))
	payload, _ = sjson.SetBytes(payload, "created", time.Now().Unix())
	payload, _ = sjson.SetBytes(payload, "model", model)
	payload, _ = sjson.SetBytes(payload, "choices.0.finish_reason", finishReason)
	if usageSeen {
		if rawUsage, errMarshal := json.Marshal(usage); errMarshal == nil {
			payload, _ = sjson.SetRawBytes(payload, "usage", rawUsage)
		}
	}
	return append(append([]byte("data: "), payload...), []byte("\n\n")...)
}

func randomHex(bytesLen int) string {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func uuidV7Like() string {
	random := make([]byte, 10)
	if _, err := rand.Read(random); err != nil {
		return uuid.NewString()
	}
	ms := uint64(time.Now().UnixMilli())
	buf := make([]byte, 16)
	buf[0] = byte(ms >> 40)
	buf[1] = byte(ms >> 32)
	buf[2] = byte(ms >> 24)
	buf[3] = byte(ms >> 16)
	buf[4] = byte(ms >> 8)
	buf[5] = byte(ms)
	copy(buf[6:], random)
	buf[6] = (buf[6] & 0x0f) | 0x70
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}
