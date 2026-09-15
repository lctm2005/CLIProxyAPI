package auth

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type resultOrderExecutor struct {
	calls              atomic.Int32
	firstStarted       chan struct{}
	releaseFirst       chan struct{}
	streamCalls        atomic.Int32
	firstStreamStarted chan struct{}
	releaseFirstStream chan struct{}
}

func (e *resultOrderExecutor) Identifier() string { return "result-order" }

func (e *resultOrderExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if e.calls.Add(1) == 1 {
		close(e.firstStarted)
		<-e.releaseFirst
		return cliproxyexecutor.Response{}, nil
	}
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusBadGateway, Message: "newer upstream failure"}
}

func (e *resultOrderExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if e.streamCalls.Add(1) != 1 {
		return nil, &Error{HTTPStatus: http.StatusBadGateway, Message: "newer upstream stream failure"}
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first chunk")}
	close(e.firstStreamStarted)
	go func() {
		<-e.releaseFirstStream
		close(chunks)
	}()
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *resultOrderExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *resultOrderExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *resultOrderExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type resultOrderRefreshExecutor struct {
	id                          string
	calls                       atomic.Int32
	streamBootstrapUnauthorized bool
	refreshStarted              chan struct{}
	releaseRefresh              chan struct{}
}

func (e *resultOrderRefreshExecutor) Identifier() string { return e.id }

func (e *resultOrderRefreshExecutor) attemptError() (int32, error) {
	call := e.calls.Add(1)
	switch call {
	case 1:
		return call, &Error{Code: "unauthorized", Message: "stale token", HTTPStatus: http.StatusUnauthorized}
	case 2:
		return call, &Error{Code: "bad_gateway", Message: "concurrent failure", HTTPStatus: http.StatusBadGateway}
	default:
		return call, nil
	}
}

func (e *resultOrderRefreshExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_, err := e.attemptError()
	return cliproxyexecutor.Response{}, err
}

func (e *resultOrderRefreshExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	call, err := e.attemptError()
	if call == 1 && err != nil && e.streamBootstrapUnauthorized {
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Err: err}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *resultOrderRefreshExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_, err := e.attemptError()
	return cliproxyexecutor.Response{}, err
}

func (e *resultOrderRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	close(e.refreshStarted)
	<-e.releaseRefresh
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = "fresh-access-token"
	return auth, nil
}

func (e *resultOrderRefreshExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManager_OlderSuccessDoesNotClearNewerFailure(t *testing.T) {
	const (
		authID = "result-order-auth"
		model  = "result-order-model"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "result-order", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	executor := &resultOrderExecutor{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: executor.Identifier()}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, errExecute := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		firstDone <- errExecute
	}()
	<-executor.firstStarted

	if _, errExecute := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("newer Execute() error = nil")
	}
	afterFailure, ok := manager.GetByID(authID)
	if !ok || afterFailure == nil || afterFailure.ModelStates[model] == nil || !afterFailure.ModelStates[model].Unavailable {
		t.Fatalf("state after newer failure = %#v, want unavailable", afterFailure)
	}

	close(executor.releaseFirst)
	if errExecute := <-firstDone; errExecute != nil {
		t.Fatalf("older Execute() error = %v", errExecute)
	}
	afterOldSuccess, ok := manager.GetByID(authID)
	if !ok || afterOldSuccess == nil || afterOldSuccess.ModelStates[model] == nil {
		t.Fatalf("state after older success = %#v, want model state", afterOldSuccess)
	}
	if !afterOldSuccess.ModelStates[model].Unavailable {
		t.Fatalf("older success cleared newer failure: %#v", afterOldSuccess.ModelStates[model])
	}
}

func TestManager_OlderSuccessDoesNotClearNewerFailureWhenCoolingDisabled(t *testing.T) {
	const (
		authID = "result-order-disable-cooling-auth"
		model  = "result-order-disable-cooling-model"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "result-order", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	executor := &resultOrderExecutor{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: executor.Identifier(),
		Metadata: map[string]any{"disable_cooling": true},
	}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, errExecute := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		firstDone <- errExecute
	}()
	<-executor.firstStarted

	if _, errExecute := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("newer Execute() error = nil")
	}
	afterFailure, ok := manager.GetByID(authID)
	if !ok || afterFailure == nil || afterFailure.ModelStates[model] == nil {
		t.Fatalf("state after newer failure = %#v, want model state", afterFailure)
	}
	if state := afterFailure.ModelStates[model]; state.Unavailable || state.LastError == nil {
		t.Fatalf("state after newer failure = %#v, want available state with diagnostic error", state)
	}

	close(executor.releaseFirst)
	if errExecute := <-firstDone; errExecute != nil {
		t.Fatalf("older Execute() error = %v", errExecute)
	}
	afterOldSuccess, ok := manager.GetByID(authID)
	if !ok || afterOldSuccess == nil || afterOldSuccess.ModelStates[model] == nil {
		t.Fatalf("state after older success = %#v, want model state", afterOldSuccess)
	}
	if state := afterOldSuccess.ModelStates[model]; state.LastError == nil || state.LastError.Message != "newer upstream failure" {
		t.Fatalf("older success cleared newer failure diagnostics: %#v", state)
	}
}

func TestManager_OlderStreamSuccessDoesNotClearNewerFailure(t *testing.T) {
	const (
		authID = "result-order-stream-auth"
		model  = "result-order-stream-model"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "result-order", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	executor := &resultOrderExecutor{
		firstStreamStarted: make(chan struct{}),
		releaseFirstStream: make(chan struct{}),
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: executor.Identifier()}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	first, errFirst := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errFirst != nil {
		t.Fatalf("older ExecuteStream() error = %v", errFirst)
	}
	<-executor.firstStreamStarted
	firstDone := make(chan struct{})
	go func() {
		for range first.Chunks {
		}
		close(firstDone)
	}()

	if _, errSecond := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errSecond == nil {
		t.Fatal("newer ExecuteStream() error = nil")
	}
	close(executor.releaseFirstStream)
	<-firstDone

	afterOldSuccess, ok := manager.GetByID(authID)
	if !ok || afterOldSuccess == nil || afterOldSuccess.ModelStates[model] == nil {
		t.Fatalf("state after older stream success = %#v, want model state", afterOldSuccess)
	}
	if !afterOldSuccess.ModelStates[model].Unavailable {
		t.Fatalf("older stream success cleared newer failure: %#v", afterOldSuccess.ModelStates[model])
	}
}

func TestManager_NewerSuccessClearsOlderFailure(t *testing.T) {
	const (
		authID = "result-order-recovery-auth"
		model  = "result-order-recovery-model"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "traecli"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.markResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "traecli",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "temporary upstream failure"},
	}, 1)
	manager.markResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "traecli",
		Model:    model,
		Success:  true,
	}, 2)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil || updated.ModelStates[model] == nil {
		t.Fatalf("state after newer success = %#v, want model state", updated)
	}
	if state := updated.ModelStates[model]; state.Unavailable || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		t.Fatalf("newer success did not clear older failure: %#v", state)
	}
}

func TestManager_OlderFailureDoesNotOverrideNewerSuccess(t *testing.T) {
	const (
		authID = "result-order-stale-failure-auth"
		model  = "result-order-stale-failure-model"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "traecli"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.markResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "traecli",
		Model:    model,
		Success:  true,
	}, 2)
	manager.markResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "traecli",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "older temporary upstream failure"},
	}, 1)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil || updated.ModelStates[model] == nil {
		t.Fatalf("state after stale failure = %#v, want model state", updated)
	}
	if state := updated.ModelStates[model]; state.Unavailable || !state.NextRetryAfter.IsZero() || state.LastError != nil {
		t.Fatalf("older failure overrode newer success: %#v", state)
	}
}

func TestManager_OlderFailureDoesNotShortenNewerCooldown(t *testing.T) {
	const (
		authID = "result-order-failure-auth"
		model  = "result-order-failure-model"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "traecli"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.markResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "traecli",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Code: "unauthorized", Message: "newer unauthorized failure"},
	}, 2)
	manager.markResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "traecli",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Code: "bad_gateway", Message: "older transient failure"},
	}, 1)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil || updated.ModelStates[model] == nil {
		t.Fatalf("state after out-of-order failures = %#v, want model state", updated)
	}
	state := updated.ModelStates[model]
	if remaining := time.Until(state.NextRetryAfter); remaining < 20*time.Minute {
		t.Fatalf("cooldown remaining = %v, want newer 30-minute cooldown preserved", remaining)
	}
	if state.LastError == nil || state.LastError.Code != "unauthorized" {
		t.Fatalf("last error = %#v, want newer unauthorized failure", state.LastError)
	}
}

func TestManager_RefreshRetryUsesNewAttemptGeneration(t *testing.T) {
	tests := []struct {
		name                        string
		streamBootstrapUnauthorized bool
		run                         func(context.Context, *Manager, string, string) error
	}{
		{
			name: "execute",
			run: func(ctx context.Context, manager *Manager, provider, model string) error {
				_, errExecute := manager.Execute(ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return errExecute
			},
		},
		{
			name: "count_tokens",
			run: func(ctx context.Context, manager *Manager, provider, model string) error {
				_, errCount := manager.ExecuteCount(ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				return errCount
			},
		},
		{
			name: "stream",
			run: func(ctx context.Context, manager *Manager, provider, model string) error {
				stream, errStream := manager.ExecuteStream(ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if errStream != nil {
					return errStream
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						return chunk.Err
					}
				}
				return nil
			},
		},
		{
			name:                        "stream_bootstrap",
			streamBootstrapUnauthorized: true,
			run: func(ctx context.Context, manager *Manager, provider, model string) error {
				stream, errStream := manager.ExecuteStream(ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
				if errStream != nil {
					return errStream
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						return chunk.Err
					}
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authID := "result-order-refresh-auth-" + tt.name
			provider := "result-order-refresh-" + tt.name
			model := "result-order-refresh-model-" + tt.name
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(authID) })

			executor := &resultOrderRefreshExecutor{
				id:                          provider,
				streamBootstrapUnauthorized: tt.streamBootstrapUnauthorized,
				refreshStarted:              make(chan struct{}),
				releaseRefresh:              make(chan struct{}),
			}
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(executor)
			if _, errRegister := manager.Register(context.Background(), &Auth{
				ID:       authID,
				Provider: provider,
				Metadata: map[string]any{"access_token": "stale-access-token", "refresh_token": "refresh-token"},
			}); errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			firstDone := make(chan error, 1)
			go func() {
				firstDone <- tt.run(context.Background(), manager, provider, model)
			}()
			<-executor.refreshStarted

			if errRun := tt.run(context.Background(), manager, provider, model); errRun == nil {
				t.Fatal("concurrent request error = nil")
			}
			close(executor.releaseRefresh)
			select {
			case errRun := <-firstDone:
				if errRun != nil {
					t.Fatalf("refreshed request error = %v", errRun)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("refreshed request did not finish")
			}

			updated, ok := manager.GetByID(authID)
			if !ok || updated == nil || updated.ModelStates[model] == nil {
				t.Fatalf("state after refreshed success = %#v", updated)
			}
			if state := updated.ModelStates[model]; state.Unavailable || !state.NextRetryAfter.IsZero() {
				t.Fatalf("fresh retry success left concurrent failure cooldown active: %#v", state)
			}
		})
	}
}

func TestManager_StaleCredentialFailureDoesNotOverrideNewerOtherModelResult(t *testing.T) {
	const (
		authID = "result-order-credential-auth"
		modelA = "result-order-credential-model-a"
		modelB = "result-order-credential-model-b"
	)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "traecli"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	manager.markResult(context.Background(), Result{AuthID: authID, Provider: "traecli", Model: modelB, Success: true}, 2)
	retryAfter := 30 * time.Minute
	manager.markResult(context.Background(), Result{
		AuthID:          authID,
		Provider:        "traecli",
		Model:           modelA,
		Success:         false,
		CredentialScope: true,
		RetryAfter:      &retryAfter,
		Error:           &Error{Code: "rate_limit", Message: "stale credential limit", HTTPStatus: http.StatusTooManyRequests},
	}, 1)

	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("GetByID() = %#v, %v", updated, ok)
	}
	if updated.Unavailable || updated.Quota.Reason == "credential_quota" {
		t.Fatalf("stale credential-scoped failure blocked auth after newer result: %#v", updated.Quota)
	}
	if state := updated.ModelStates[modelB]; state == nil || state.Unavailable {
		t.Fatalf("stale credential-scoped failure blocked newer model state: %#v", state)
	}
	if state := updated.ModelStates[modelA]; state != nil {
		t.Fatalf("stale credential-scoped failure created model state: %#v", state)
	}
}

func TestManager_StaleFailureDoesNotDeleteRecoveredSessionBinding(t *testing.T) {
	const (
		authID   = "result-order-session-auth"
		provider = "result-order-session-provider"
		model    = "result-order-session-model"
	)
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: provider}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	opts := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": {"result-order-session"}}}
	cacheKey := provider + "::codex:result-order-session::" + model
	selector.cache.Set(cacheKey, authID)
	manager.markResult(context.Background(), Result{AuthID: authID, Provider: provider, Model: model, Success: true, Options: opts}, 2)
	manager.markResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error:    &Error{Code: "bad_gateway", Message: "stale failure", HTTPStatus: http.StatusBadGateway},
		Options:  opts,
	}, 1)

	if gotAuthID, ok := selector.cache.Get(cacheKey); !ok || gotAuthID != authID {
		t.Fatalf("stale failure deleted recovered session binding: got (%q, %v)", gotAuthID, ok)
	}
}

func TestManager_SchedulerTransientCooldownReturnsServiceUnavailable(t *testing.T) {
	const (
		authID = "scheduler-transient-auth"
		model  = "scheduler-transient-model"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "traecli", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: authID, Provider: "traecli"}); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: "traecli",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusBadGateway, Message: "temporary upstream failure"},
	})

	_, errPick := manager.scheduler.pickSingle(context.Background(), "traecli", model, cliproxyexecutor.Options{}, nil)
	var cooldownErr *modelCooldownError
	if !errors.As(errPick, &cooldownErr) {
		t.Fatalf("pickSingle() error = %v, want model cooldown error", errPick)
	}
	if got := cooldownErr.StatusCode(); got != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if got := cooldownErr.Headers().Get("Retry-After"); got == "" {
		t.Fatal("Retry-After = empty")
	}
}
