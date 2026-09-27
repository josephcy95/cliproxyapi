package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type playgroundCaptureExecutor struct {
	authIDs []string
	request coreexecutor.Request
}

func (e *playgroundCaptureExecutor) Identifier() string { return "playground-test" }

func (e *playgroundCaptureExecutor) Execute(_ context.Context, selected *auth.Auth, request coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.authIDs = append(e.authIDs, selected.ID)
	e.request = request
	return coreexecutor.Response{Payload: []byte(`{"id":"chatcmpl-test","model":"playground-model","choices":[{"message":{"role":"assistant","content":"working"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)}, nil
}

func (*playgroundCaptureExecutor) ExecuteStream(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}

func (*playgroundCaptureExecutor) Refresh(_ context.Context, credential *auth.Auth) (*auth.Auth, error) {
	return credential, nil
}

func (*playgroundCaptureExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}

func (*playgroundCaptureExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestPlaygroundChatPinsSelectedCredential(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	executor := &playgroundCaptureExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)

	first := &auth.Auth{ID: "playground-first", Provider: executor.Identifier(), Label: "First", Status: auth.StatusActive}
	second := &auth.Auth{ID: "playground-second", Provider: executor.Identifier(), Label: "Second", Status: auth.StatusActive}
	if _, errRegister := server.handlers.AuthManager.Register(context.Background(), first); errRegister != nil {
		t.Fatalf("register first credential: %v", errRegister)
	}
	if _, errRegister := server.handlers.AuthManager.Register(context.Background(), second); errRegister != nil {
		t.Fatalf("register second credential: %v", errRegister)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(first.ID, executor.Identifier(), []*registry.ModelInfo{{ID: "playground-model"}})
	reg.RegisterClient(second.ID, executor.Identifier(), []*registry.ModelInfo{{ID: "playground-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(first.ID)
		reg.UnregisterClient(second.ID)
	})

	body := `{"model":"playground-model","provider":"playground-test","auth_index":"` + second.EnsureIndex() + `","auth_id":"` + second.ID + `","messages":[{"role":"user","content":"hello"}]}`
	response := postPlaygroundChat(t, server, body)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if len(executor.authIDs) != 1 || executor.authIDs[0] != second.ID {
		t.Fatalf("selected auth IDs = %v, want [%s]", executor.authIDs, second.ID)
	}
	if executor.request.Model != "playground-model" {
		t.Fatalf("request model = %q, want playground-model", executor.request.Model)
	}
	var requestPayload map[string]any
	if errDecode := json.Unmarshal(executor.request.Payload, &requestPayload); errDecode != nil {
		t.Fatalf("decode executor request: %v", errDecode)
	}
	if _, ok := requestPayload["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort = %#v, want omitted", requestPayload["reasoning_effort"])
	}

	var payload playgroundChatResponse
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload.Message.Content != "working" || payload.Route.AuthIndex != second.Index || payload.Route.CredentialLabel != "Second" {
		t.Fatalf("unexpected playground response: %#v", payload)
	}
}

func TestPlaygroundChatForwardsReasoningEffort(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	executor := &playgroundCaptureExecutor{}
	server.handlers.AuthManager.RegisterExecutor(executor)

	credential := &auth.Auth{ID: "playground-effort", Provider: executor.Identifier(), Label: "Effort", Status: auth.StatusActive}
	if _, errRegister := server.handlers.AuthManager.Register(context.Background(), credential); errRegister != nil {
		t.Fatalf("register credential: %v", errRegister)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(credential.ID, executor.Identifier(), []*registry.ModelInfo{{ID: "playground-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(credential.ID)
	})

	body := `{"model":"playground-model","provider":"playground-test","auth_index":"` + credential.EnsureIndex() + `","auth_id":"` + credential.ID + `","reasoning_effort":" XHIGH ","messages":[{"role":"user","content":"hello"}]}`
	response := postPlaygroundChat(t, server, body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}

	var requestPayload map[string]any
	if errDecode := json.Unmarshal(executor.request.Payload, &requestPayload); errDecode != nil {
		t.Fatalf("decode executor request: %v", errDecode)
	}
	if requestPayload["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort = %#v, want xhigh", requestPayload["reasoning_effort"])
	}
}

func TestPlaygroundChatRejectsUnsupportedReasoningEffort(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	body := `{"model":"playground-model","provider":"playground-test","auth_index":"idx","auth_id":"id","reasoning_effort":"turbo","messages":[{"role":"user","content":"hello"}]}`
	response := postPlaygroundChat(t, server, body)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "unsupported reasoning_effort") {
		t.Fatalf("body = %s, want unsupported reasoning_effort", response.Body.String())
	}
}

func TestPlaygroundErrorStatusDoesNotExposeManagementUnauthorized(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if got := playgroundErrorStatus(status); got != http.StatusBadGateway {
			t.Fatalf("playgroundErrorStatus(%d) = %d, want %d", status, got, http.StatusBadGateway)
		}
	}
	if got := playgroundErrorStatus(http.StatusTooManyRequests); got != http.StatusTooManyRequests {
		t.Fatalf("playgroundErrorStatus(429) = %d, want 429", got)
	}
}

func TestPlaygroundChatRejectsProviderCredentialMismatch(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	credential := &auth.Auth{ID: "playground-mismatch", Provider: "codex", Status: auth.StatusActive}
	if _, errRegister := server.handlers.AuthManager.Register(context.Background(), credential); errRegister != nil {
		t.Fatalf("register credential: %v", errRegister)
	}

	body := `{"model":"playground-model","provider":"claude","auth_index":"` + credential.EnsureIndex() + `","auth_id":"` + credential.ID + `","messages":[{"role":"user","content":"hello"}]}`
	response := postPlaygroundChat(t, server, body)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
}

func postPlaygroundChat(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v0/management/playground/chat", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-management-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	return response
}
