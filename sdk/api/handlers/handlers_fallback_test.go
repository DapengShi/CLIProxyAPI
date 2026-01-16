package handlers

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

type recordedCall struct {
	model   string
	payload []byte
}

type fallbackMockExecutor struct {
	mu          sync.Mutex
	calls       []recordedCall
	streamCalls []recordedCall
}

func (e *fallbackMockExecutor) Identifier() string { return "codex" }

func (e *fallbackMockExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.record(&e.calls, req)
	switch req.Model {
	case "primary-model":
		return coreexecutor.Response{}, &coreauth.Error{Code: "quota", Message: "quota", HTTPStatus: http.StatusTooManyRequests}
	case "fallback-model":
		return coreexecutor.Response{Payload: []byte(`{"ok":true,"model":"fallback-model"}`)}, nil
	default:
		return coreexecutor.Response{}, &coreauth.Error{Code: "bad_model", Message: "bad model", HTTPStatus: http.StatusBadRequest}
	}
}

func (e *fallbackMockExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (<-chan coreexecutor.StreamChunk, error) {
	e.record(&e.streamCalls, req)
	ch := make(chan coreexecutor.StreamChunk, 1)
	switch req.Model {
	case "primary-model":
		ch <- coreexecutor.StreamChunk{Err: &coreauth.Error{Code: "quota", Message: "quota", HTTPStatus: http.StatusTooManyRequests}}
	case "fallback-model":
		ch <- coreexecutor.StreamChunk{Payload: []byte("stream-ok")}
	default:
		ch <- coreexecutor.StreamChunk{Err: &coreauth.Error{Code: "bad_model", Message: "bad model", HTTPStatus: http.StatusBadRequest}}
	}
	close(ch)
	return ch, nil
}

func (e *fallbackMockExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *fallbackMockExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *fallbackMockExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *fallbackMockExecutor) record(target *[]recordedCall, req coreexecutor.Request) {
	payloadCopy := make([]byte, len(req.Payload))
	copy(payloadCopy, req.Payload)

	e.mu.Lock()
	*target = append(*target, recordedCall{model: req.Model, payload: payloadCopy})
	e.mu.Unlock()
}

func (e *fallbackMockExecutor) Calls() []recordedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]recordedCall(nil), e.calls...)
}

func (e *fallbackMockExecutor) StreamCalls() []recordedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]recordedCall(nil), e.streamCalls...)
}

func setupFallbackHandler(t *testing.T, executor *fallbackMockExecutor) *BaseAPIHandler {
	t.Helper()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0)
	manager.RegisterExecutor(executor)

	authEntry := &coreauth.Auth{
		ID:       "auth1",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "test@example.com"},
	}
	if _, err := manager.Register(context.Background(), authEntry); err != nil {
		t.Fatalf("manager.Register(authEntry): %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(authEntry.ID, authEntry.Provider, []*registry.ModelInfo{{ID: "primary-model"}, {ID: "fallback-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(authEntry.ID)
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler.UpdateModelMappings([]sdkconfig.AmpModelMapping{{From: "primary-model", To: "fallback-model"}})

	return handler
}

func TestExecuteWithAuthManager_GlobalFallbackOn429(t *testing.T) {
	executor := &fallbackMockExecutor{}
	handler := setupFallbackHandler(t, executor)

	resp, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "primary-model", []byte(`{"model":"primary-model","input":"hi"}`), "")
	if errMsg != nil {
		t.Fatalf("unexpected error: %+v", errMsg)
	}
	if got := gjson.GetBytes(resp, "model").String(); got != "fallback-model" {
		t.Fatalf("expected fallback model in response, got %q", got)
	}

	calls := executor.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 execute calls, got %d", len(calls))
	}
	if got := gjson.GetBytes(calls[0].payload, "model").String(); got != "primary-model" {
		t.Fatalf("expected primary model in first payload, got %q", got)
	}
	if got := gjson.GetBytes(calls[1].payload, "model").String(); got != "fallback-model" {
		t.Fatalf("expected fallback model in second payload, got %q", got)
	}
}

func TestExecuteStreamWithAuthManager_GlobalFallbackOn429(t *testing.T) {
	executor := &fallbackMockExecutor{}
	handler := setupFallbackHandler(t, executor)

	dataChan, errChan := handler.ExecuteStreamWithAuthManager(context.Background(), "openai", "primary-model", []byte(`{"model":"primary-model"}`), "")
	if dataChan == nil || errChan == nil {
		t.Fatalf("expected non-nil channels")
	}

	var got []byte
	for chunk := range dataChan {
		got = append(got, chunk...)
	}
	for msg := range errChan {
		if msg != nil {
			t.Fatalf("unexpected error: %+v", msg)
		}
	}

	if string(got) != "stream-ok" {
		t.Fatalf("expected stream-ok payload, got %q", string(got))
	}

	calls := executor.StreamCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 stream calls, got %d", len(calls))
	}
	if got := gjson.GetBytes(calls[0].payload, "model").String(); got != "primary-model" {
		t.Fatalf("expected primary model in first stream payload, got %q", got)
	}
	if got := gjson.GetBytes(calls[1].payload, "model").String(); got != "fallback-model" {
		t.Fatalf("expected fallback model in second stream payload, got %q", got)
	}
}
