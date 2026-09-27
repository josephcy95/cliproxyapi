package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecuteNormalizesNullInstructions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses")
	}
	if gjson.GetBytes(gotBody, "instructions").Type != gjson.String {
		t.Fatalf("instructions type = %v, want string", gjson.GetBytes(gotBody, "instructions").Type)
	}
	if gjson.GetBytes(gotBody, "instructions").String() != "" {
		t.Fatalf("instructions = %q, want empty string", gjson.GetBytes(gotBody, "instructions").String())
	}
}

func TestCodexExecutorExecuteStreamNormalizesNullInstructions(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for range result.Chunks {
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want %q", gotPath, "/responses")
	}
	if gjson.GetBytes(gotBody, "instructions").Type != gjson.String {
		t.Fatalf("instructions type = %v, want string", gjson.GetBytes(gotBody, "instructions").Type)
	}
	if gjson.GetBytes(gotBody, "instructions").String() != "" {
		t.Fatalf("instructions = %q, want empty string", gjson.GetBytes(gotBody, "instructions").String())
	}
}

func TestCodexExecutorCountTokensTreatsNullInstructionsAsEmpty(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})

	nullResp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":null,"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("CountTokens(null) error: %v", err)
	}

	emptyResp, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","instructions":"","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("CountTokens(empty) error: %v", err)
	}

	if string(nullResp.Payload) != string(emptyResp.Payload) {
		t.Fatalf("token count payload mismatch:\nnull=%s\nempty=%s", string(nullResp.Payload), string(emptyResp.Payload))
	}
}

func TestApplyCodexConfiguredInstructionsPrependsForOAuthMatchingModel(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{Instructions: config.CodexInstructionsConfig{
		Enabled: true,
		Mode:    "prepend",
		Content: "private prefs",
		Models:  []string{"gpt-5*"},
	}}})
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"type": "codex", "allow_private_instructions": true}, Attributes: map[string]string{
		"allow_private_instructions": "true",
		"base_url": captureCodexInstructionsServer(t, func(body []byte) {
			got := gjson.GetBytes(body, "instructions").String()
			want := "private prefs\n\nbase instructions"
			if got != want {
				t.Fatalf("instructions = %q, want %q", got, want)
			}
		}),
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","instructions":"base instructions","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata:     map[string]any{cliproxyexecutor.CodexPrivateInstructionsMetadataKey: true},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
}

func TestApplyCodexConfiguredInstructionsSkipsAPIKeyAuth(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{Instructions: config.CodexInstructionsConfig{
		Enabled: true,
		Content: "private prefs",
		Models:  []string{"gpt-5*"},
	}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": captureCodexInstructionsServer(t, func(body []byte) {
			if got := gjson.GetBytes(body, "instructions").String(); got != "base instructions" {
				t.Fatalf("instructions = %q, want base instructions", got)
			}
		}),
		"api_key": "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","instructions":"base instructions","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
}

func TestApplyCodexConfiguredInstructionsSkipsNonMatchingModel(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{Instructions: config.CodexInstructionsConfig{
		Enabled: true,
		Content: "private prefs",
		Models:  []string{"gpt-5.5"},
	}}})
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"type": "codex"}, Attributes: map[string]string{
		"base_url": captureCodexInstructionsServer(t, func(body []byte) {
			if got := gjson.GetBytes(body, "instructions").String(); got != "base instructions" {
				t.Fatalf("instructions = %q, want base instructions", got)
			}
		}),
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-4.1",
		Payload: []byte(`{"model":"gpt-4.1","instructions":"base instructions","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
}

func TestApplyCodexConfiguredInstructionsSkipsWithoutPrivateMarker(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{Instructions: config.CodexInstructionsConfig{
		Enabled: true,
		Content: "private prefs",
		Models:  []string{"gpt-5*"},
	}}})
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"type": "codex", "allow_private_instructions": true}, Attributes: map[string]string{
		"allow_private_instructions": "true",
		"base_url": captureCodexInstructionsServer(t, func(body []byte) {
			if got := gjson.GetBytes(body, "instructions").String(); got != "base instructions" {
				t.Fatalf("instructions = %q, want base instructions", got)
			}
		}),
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","instructions":"base instructions","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
}

func TestApplyCodexConfiguredInstructionsSkipsUnmarkedAuth(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{Instructions: config.CodexInstructionsConfig{
		Enabled: true,
		Content: "private prefs",
		Models:  []string{"gpt-5*"},
	}}})
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"type": "codex"}, Attributes: map[string]string{
		"base_url": captureCodexInstructionsServer(t, func(body []byte) {
			if got := gjson.GetBytes(body, "instructions").String(); got != "base instructions" {
				t.Fatalf("instructions = %q, want base instructions", got)
			}
		}),
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","instructions":"base instructions","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata:     map[string]any{cliproxyexecutor.CodexPrivateInstructionsMetadataKey: true},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
}

func captureCodexInstructionsServer(t *testing.T, check func([]byte)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		check(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":0,\"status\":\"completed\",\"background\":false,\"error\":null}}\n\n"))
	}))
	t.Cleanup(server.Close)
	return server.URL
}
