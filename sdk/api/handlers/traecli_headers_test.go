package handlers

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHandlerTraeCLIDiagnosticHeadersPassThroughWhenPassthroughDisabled(t *testing.T) {
	model := "handler-traecli-diagnostic-headers-model"
	executor := &interceptorCaptureExecutor{
		execute: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
			return coreexecutor.Response{
				Payload: []byte("ok"),
				Headers: http.Header{
					"X-TraeCLI-Mode":            []string{"exec"},
					"X-TraeCLI-Native-Fallback": []string{"true"},
					"X-Upstream":                []string{"raw"},
				},
			}, nil
		},
	}
	handler := newInterceptorHandler(t, model, executor, &sdkconfig.SDKConfig{PassthroughHeaders: false})
	handler.SetPluginHost(&handlerInterceptorTestHost{
		interceptResponse: func(ctx context.Context, req pluginapi.ResponseInterceptRequest) pluginapi.ResponseInterceptResponse {
			headers := cloneHeader(req.ResponseHeaders)
			headers.Set("X-Plugin", "response")
			return pluginapi.ResponseInterceptResponse{Headers: headers}
		},
	})

	_, headers, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %+v", errMsg)
	}
	if got := headers.Get("X-TraeCLI-Mode"); got != "exec" {
		t.Fatalf("X-TraeCLI-Mode = %q, want exec; headers=%#v", got, headers)
	}
	if got := headers.Get("X-TraeCLI-Native-Fallback"); got != "true" {
		t.Fatalf("X-TraeCLI-Native-Fallback = %q, want true; headers=%#v", got, headers)
	}
	if got := headers.Get("X-Plugin"); got != "response" {
		t.Fatalf("X-Plugin = %q, want response; headers=%#v", got, headers)
	}
	if headers.Get("X-Upstream") != "" {
		t.Fatalf("headers leaked raw upstream header with passthrough disabled: %#v", headers)
	}
}

func TestHandlerTraeCLIDiagnosticStreamHeadersPassThroughWhenPassthroughDisabled(t *testing.T) {
	model := "handler-traecli-diagnostic-stream-headers-model"
	executor := &interceptorCaptureExecutor{
		stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("ok")}
			close(chunks)
			return &coreexecutor.StreamResult{
				Headers: http.Header{
					"X-TraeCLI-Mode": []string{"native"},
					"X-Upstream":     []string{"raw"},
				},
				Chunks: chunks,
			}, nil
		},
	}
	handler := newInterceptorHandler(t, model, executor, &sdkconfig.SDKConfig{PassthroughHeaders: false})

	dataChan, headers, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", model, []byte(fmt.Sprintf(`{"model":%q,"stream":true}`, model)), "")
	if got := headers.Get("X-TraeCLI-Mode"); got != "native" {
		t.Fatalf("X-TraeCLI-Mode = %q, want native; headers=%#v", got, headers)
	}
	if headers.Get("X-Upstream") != "" {
		t.Fatalf("headers leaked raw upstream header with passthrough disabled: %#v", headers)
	}
	for range dataChan {
	}
	for errMsg := range errChan {
		if errMsg != nil {
			t.Fatalf("ExecuteStreamWithAuthManager() error = %+v", errMsg)
		}
	}
}
