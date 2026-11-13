package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestAntigravityExecutorCanonicalEffortRouting(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	const authID = "antigravity-canonical-routing"
	var variants []*registry.ModelInfo
	for _, level := range []string{"low", "medium", "high"} {
		variants = append(variants, &registry.ModelInfo{ID: "gemini-3.8-flash-" + level, Type: "antigravity", Thinking: &registry.ThinkingSupport{Min: 1, Max: 65535, DynamicAllowed: true, Levels: []string{"low", "medium", "high"}}})
	}
	reg.RegisterClient(authID, "antigravity", registry.GroupAntigravityEffortModels(variants))
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	for _, stream := range []bool{false, true} {
		for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatGemini} {
			for _, level := range []string{"low", "medium", "high"} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", format, level, stream), func(t *testing.T) {
					var captured []byte
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						captured, _ = io.ReadAll(req.Body)
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"OK\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}}\n\n")), Request: req}, nil
					}))
					var payload string
					switch format {
					case sdktranslator.FormatOpenAI:
						payload = fmt.Sprintf(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"Reply OK"}],"reasoning_effort":%q}`, level)
					case sdktranslator.FormatOpenAIResponse:
						payload = fmt.Sprintf(`{"model":"gemini-3.8-flash","input":"Reply OK","reasoning":{"effort":%q}}`, level)
					case sdktranslator.FormatGemini:
						payload = fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"Reply OK"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":%q}}}`, level)
					}
					auth := &cliproxyauth.Auth{ID: authID, Provider: "antigravity", Metadata: map[string]any{"access_token": "token", "project_id": "project-1", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}}
					req := cliproxyexecutor.Request{Model: "gemini-3.8-flash", Payload: []byte(payload)}
					opts := cliproxyexecutor.Options{SourceFormat: format, ResponseFormat: format, OriginalRequest: []byte(payload), Stream: stream}
					executor := NewAntigravityExecutor(&config.Config{})
					if stream {
						res, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range res.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
					if got := ojson.GetBytes(captured, "model").String(); got != "gemini-3.8-flash-"+level {
						t.Fatalf("wire model=%q, payload=%s", got, captured)
					}
					if got := ojson.GetBytes(captured, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != level {
						t.Fatalf("wire effort=%q, want %s", got, level)
					}
				})
			}
		}
	}
}
