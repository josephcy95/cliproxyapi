package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func xaiTerminalSSEAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:         "xai-terminal-sse",
		Provider:   "xai",
		Attributes: map[string]string{"base_url": baseURL, "auth_kind": "oauth"},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
}

func TestXAIExecuteReturnsTerminalFreeUsageSSEError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"error","status":429,"error":{"code":"subscription:free-usage-exhausted","message":"included free usage"}}` + "\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	_, err := exec.Execute(context.Background(), xaiTerminalSSEAuth(server.URL), cliproxyexecutor.Request{
		Model: "grok-4.5", Payload: []byte(`{"model":"grok-4.5","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	status, ok := err.(statusErr)
	if !ok || status.StatusCode() != http.StatusTooManyRequests || status.RetryAfter() == nil {
		t.Fatalf("terminal free-usage SSE error = %T %v, want 429 with RetryAfter", err, err)
	}
}

func TestXAIExecuteStreamEmitsResponseFailedSSEError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"subscription:free-usage-exhausted","message":"included free usage","status":429}}}` + "\n\n"))
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	result, err := exec.ExecuteStream(context.Background(), xaiTerminalSSEAuth(server.URL), cliproxyexecutor.Request{
		Model: "grok-4.5", Payload: []byte(`{"model":"grok-4.5","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err == nil {
			continue
		}
		status, ok := chunk.Err.(statusErr)
		if !ok || status.StatusCode() != http.StatusTooManyRequests || status.RetryAfter() == nil {
			t.Fatalf("terminal stream error = %T %v, want 429 with RetryAfter", chunk.Err, chunk.Err)
		}
		return
	}
	t.Fatal("response.failed was forwarded as a successful stream")
}
