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

func codexTerminalSSEAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Attributes: map[string]string{"base_url": baseURL, "api_key": "test-key"}}
}

func TestCodexExecuteReturnsTerminalAuthenticationSSEError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"authentication_error","code":"auth_unavailable","message":"invalid or expired token"}}` + "\n\n"))
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{})
	_, err := exec.Execute(context.Background(), codexTerminalSSEAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	status, ok := err.(statusErr)
	if !ok || status.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("terminal authentication SSE error = %T %v, want 401", err, err)
	}
}

func TestCodexExecuteStreamEmitsTerminalAuthenticationSSEError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"status":"failed","error":{"type":"authentication_error","code":"auth_unavailable","message":"invalid or expired token"}}}` + "\n\n"))
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{})
	result, err := exec.ExecuteStream(context.Background(), codexTerminalSSEAuth(server.URL), cliproxyexecutor.Request{
		Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err == nil {
			continue
		}
		status, ok := chunk.Err.(statusErr)
		if !ok || status.StatusCode() != http.StatusUnauthorized {
			t.Fatalf("terminal stream error = %T %v, want 401", chunk.Err, chunk.Err)
		}
		return
	}
	t.Fatal("terminal authentication error was forwarded as a successful stream")
}
