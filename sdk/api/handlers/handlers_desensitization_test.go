package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/desensitization"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestDesensitizationFailClosedBeforeAuth(t *testing.T) {
	cfg := config.DefaultDesensitizationConfig()
	cfg.Enabled = true
	cfg.FailClosed = true
	eng := desensitization.Configure(cfg)
	eng.SetTestMaskFailure(fmt.Errorf("injected store failure"))
	t.Cleanup(func() { desensitization.Configure(config.DefaultDesensitizationConfig()) })

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	req := coreexecutor.Request{Model: "gpt-test", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}
	opts := coreexecutor.Options{Metadata: map[string]any{}}
	_, _, errMsg := handler.applyRequestInterceptorsBeforeAuth(context.Background(), "openai", req.Model, "req-fc-1", req, opts, "")
	if errMsg == nil || errMsg.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 fail_closed, got %#v", errMsg)
	}
	if !errMsg.DirectResponse || !strings.Contains(string(errMsg.Body), "desensitization failed") {
		t.Fatalf("expected direct desensitization error body, got %#v", errMsg)
	}
}

func TestDesensitizationFailClosedAfterAuth(t *testing.T) {
	cfg := config.DefaultDesensitizationConfig()
	cfg.Enabled = true
	cfg.FailClosed = true
	eng := desensitization.Configure(cfg)
	eng.SetTestMaskFailure(fmt.Errorf("injected store failure"))
	t.Cleanup(func() { desensitization.Configure(config.DefaultDesensitizationConfig()) })

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	out := handler.applyRequestInterceptorsAfterAuth(context.Background(), coreexecutor.RequestAfterAuthInterceptRequest{
		SourceFormat:   sdktranslator.FromString("openai"),
		Model:          "gpt-test",
		RequestedModel: "gpt-test",
		Provider:       "openai",
		AuthKind:       "apikey",
		Body:           []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		Metadata:       map[string]any{},
	}, "req-fc-2", "")
	if !out.Terminate || out.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected terminate 502, got %#v", out)
	}
	if !strings.Contains(string(out.ResponseBody), "desensitization failed") {
		t.Fatalf("expected fail_closed body, got %q", out.ResponseBody)
	}
}

func TestDesensitizationFailClosedSkipsNonJSON(t *testing.T) {
	cfg := config.DefaultDesensitizationConfig()
	cfg.Enabled = true
	cfg.FailClosed = true
	eng := desensitization.Configure(cfg)
	eng.SetTestMaskFailure(fmt.Errorf("injected store failure"))
	t.Cleanup(func() { desensitization.Configure(config.DefaultDesensitizationConfig()) })

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	raw := []byte("plain-text-body")
	req := coreexecutor.Request{Model: "gpt-test", Payload: raw}
	opts := coreexecutor.Options{Metadata: map[string]any{}}
	gotReq, _, errMsg := handler.applyRequestInterceptorsBeforeAuth(context.Background(), "openai", req.Model, "req-fc-3", req, opts, "")
	if errMsg != nil {
		t.Fatalf("non-JSON must not fail closed: %#v", errMsg)
	}
	if string(gotReq.Payload) != string(raw) {
		t.Fatalf("payload changed: %q", gotReq.Payload)
	}
}
