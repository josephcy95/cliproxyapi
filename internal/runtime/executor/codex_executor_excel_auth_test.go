package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestExcelHeadersRequirePaidOAuth(t *testing.T) {
	e := NewCodexExecutor(nil)
	for _, tc := range []struct {
		name, provider, plan, key string
		want                      bool
	}{
		{"paid", "codex", "plus", "", true}, {"free", "codex", "free", "", false},
		{"unknown", "codex", "", "", false}, {"apikey", "codex", "plus", "key", false},
		{"kiro", "kiro", "plus", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &cliproxyauth.Auth{Provider: tc.provider, Attributes: map[string]string{"api_key": tc.key, "header:Authorization": "Bearer wrong", "header:Chatgpt-Account-Id": "wrong-account"}, Metadata: map[string]any{"access_token": "oauth-token", "plan_type": tc.plan, "account_id": "oauth-account"}}
			req, _ := http.NewRequest(http.MethodPost, "https://example.invalid", nil)
			err := e.prepareExcelHeaders(req, a)
			if (err == nil) != tc.want {
				t.Fatalf("error=%v want success=%v", err, tc.want)
			}
			if tc.want && (req.Header.Get("Authorization") != "Bearer oauth-token" || req.Header.Get("Chatgpt-Account-Id") != "oauth-account") {
				t.Fatal("custom headers replaced OAuth identity")
			}
		})
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid", nil)
	if e.prepareExcelHeaders(req, nil) == nil {
		t.Fatal("nil auth accepted")
	}
}

// Use an injected transport so these boundary tests need no sockets or credentials.
type excelAuthTestTransport func(*http.Request) (*http.Response, error)

func (f excelAuthTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExcelExecutionOAuthBoundary(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, eligible := range []bool{false, true} {
			name := fmt.Sprintf("stream=%v/eligible=%v", stream, eligible)
			t.Run(name, func(t *testing.T) {
				calls := 0
				ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", excelAuthTestTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Header.Get("Authorization") != "Bearer oauth-token" {
						t.Error("not using the OAuth access token")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(excelProtocolStream("hello client"))), Request: r}, nil
				}))
				a := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "oauth-token", "plan_type": "plus"}}
				if !eligible {
					a.Attributes = map[string]string{"api_key": "api-key"}
				}
				e := NewCodexExecutor(&config.Config{})
				req := coreexecutor.Request{Model: "gpt-5.6-sol-excel", Payload: []byte(`{"input":"hello"}`)}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				var err error
				var output strings.Builder
				if stream {
					var result *coreexecutor.StreamResult
					result, err = e.ExecuteStream(ctx, a, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
							output.Write(chunk.Payload)
						}
					}
				} else {
					var result coreexecutor.Response
					result, err = e.Execute(ctx, a, req, opts)
					output.Write(result.Payload)
				}
				if eligible {
					if err != nil || calls != 1 || !strings.Contains(output.String(), "hello client") {
						t.Fatalf("calls=%d error=%v output=%s", calls, err, output.String())
					}
				} else {
					if err == nil || calls != 0 {
						t.Fatalf("ineligible credential reached transport: calls=%d error=%v", calls, err)
					}
				}
			})
		}
	}
}
