package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	"github.com/tidwall/gjson"
)

func excelProtocolStream(text string) string {
	item := map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_1", "model": "gpt-5.6-sol", "status": "in_progress"}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "content": []any{}}},
		{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": text},
		{"type": "response.output_text.done", "item_id": "msg_1", "output_index": 0, "content_index": 0, "text": text},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp_1", "model": "gpt-5.6-sol", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}},
	}
	var out strings.Builder
	for _, event := range events {
		raw, _ := json.Marshal(event)
		fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", event["type"], raw)
	}
	return out.String()
}

func TestExcelClientProtocols(t *testing.T) {
	cases := []struct {
		name                                              string
		format                                            sdktranslator.Format
		request, textPath, toolPath, toolName, streamTool string
	}{
		{"responses", sdktranslator.FormatOpenAIResponse, `{"input":[{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`, "output.0.content.0.text", "output.0.name", "lookup", "function_call"},
		{"chat", sdktranslator.FormatOpenAI, `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, "choices.0.message.content", "choices.0.message.tool_calls.0.function.name", "lookup", "tool_calls"},
		{"claude", sdktranslator.FormatClaude, `{"messages":[{"role":"user","content":"hello"}],"max_tokens":100,"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`, "content.0.text", "content.0.name", "lookup", "tool_use"},
		{"gemini", sdktranslator.FormatGemini, `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}]}`, "candidates.0.content.parts.0.text", "candidates.0.content.parts.0.functionCall.name", "lookup", "functionCall"},
		{"interactions", sdktranslator.FormatInteractions, `{"input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`, "steps.0.content.0.text", "steps.0.name", "lookup", "function_call"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			for _, tool := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/tool=%v", tc.name, stream, tool), func(t *testing.T) {
					answer := "hello client"
					if tool {
						answer = `<codex_tool_call>{"name":"lookup","arguments":{"query":"test"}}</codex_tool_call>`
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						raw, _ := io.ReadAll(r.Body)
						if r.URL.Path != "/basispoints/api/responses" {
							t.Errorf("upstream path = %s", r.URL.Path)
						}
						if gjson.GetBytes(raw, "model").String() != "gpt-5.6-sol" {
							t.Errorf("upstream model: %s", raw)
						}
						if !strings.Contains(string(raw), "hello") {
							t.Errorf("lost user input: %s", raw)
						}
						if gjson.GetBytes(raw, "tools").Exists() {
							t.Errorf("leaked native tools: %s", raw)
						}
						if !strings.Contains(string(raw), "lookup") {
							t.Errorf("lost tool catalog: %s", raw)
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, excelProtocolStream(answer))
					}))
					defer server.Close()
					auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "test", "plan_type": "plus"}, Attributes: map[string]string{"excel_base_url": server.URL}}
					req := cliproxyexecutor.Request{Model: "gpt-5.6-sol-excel", Payload: []byte(tc.request)}
					opts := cliproxyexecutor.Options{SourceFormat: tc.format, OriginalRequest: req.Payload}
					e := NewCodexExecutor(&config.Config{})
					if stream {
						result, err := e.ExecuteStream(context.Background(), auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						var out strings.Builder
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							out.Write(chunk.Payload)
						}
						value := out.String()
						if tool {
							if !strings.Contains(value, tc.streamTool) || !strings.Contains(value, "lookup") {
								t.Fatalf("missing translated tool call: %s", value)
							}
						} else if !strings.Contains(value, "hello client") {
							t.Fatalf("missing text: %s", value)
						}
						if strings.Contains(value, "<codex_tool_call>") {
							t.Fatalf("leaked marker: %s", value)
						}
						if tc.format != sdktranslator.FormatOpenAIResponse && strings.Contains(value, `"type":"response.`) {
							t.Fatalf("leaked Responses events: %s", value)
						}
					} else {
						result, err := e.Execute(context.Background(), auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						path, want := tc.textPath, "hello client"
						if tool {
							path, want = tc.toolPath, tc.toolName
						}
						if got := gjson.GetBytes(result.Payload, path).String(); got != want {
							t.Fatalf("%s = %q, want %q: %s", path, got, want, result.Payload)
						}
					}
				})
			}
		}
	}
}

func TestExcelResponseOverrideAndHistory(t *testing.T) {
	e := NewCodexExecutor(&config.Config{})
	payload := []byte(`{"messages":[{"role":"system","content":"Keep the caller instruction"},{"role":"user","content":"lookup"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"test\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"lookup result"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		for _, want := range []string{"Keep the caller instruction", "call_1", "lookup result", "lookup"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("lost history %q: %s", want, raw)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, excelProtocolStream("history preserved"))
	}))
	defer server.Close()
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "test", "plan_type": "plus"}, Attributes: map[string]string{"excel_base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol-excel", Payload: payload}
	for _, stream := range []bool{false, true} {
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatClaude, OriginalRequest: payload}
		if stream {
			r, err := e.ExecuteStream(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			for c := range r.Chunks {
				if c.Err != nil {
					t.Fatal(c.Err)
				}
				out.Write(c.Payload)
			}
			if !strings.Contains(out.String(), "content_block_delta") || !strings.Contains(out.String(), "history preserved") {
				t.Fatal(out.String())
			}
		} else {
			r, err := e.Execute(context.Background(), auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(r.Payload, "content.0.text").String() != "history preserved" {
				t.Fatal(string(r.Payload))
			}
		}
	}
}

func TestExcelWebsocketClientUsesExcelBackend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/basispoints/api/responses" || r.Header.Get("Upgrade") != "" {
			t.Errorf("wrong transport: %s %v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, excelProtocolStream("websocket client"))
	}))
	defer server.Close()
	e := NewCodexAutoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "test", "plan_type": "plus"}, Attributes: map[string]string{"excel_base_url": server.URL, "base_url": server.URL, "websockets": "true"}}
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol-excel", Payload: []byte(`{"input":"hello"}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	r, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(r.Payload, "output.0.content.0.text").String() != "websocket client" {
		t.Fatal(string(r.Payload))
	}
	result, err := e.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	for c := range result.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
	}
	ctx = cliproxyexecutor.WithRequiredUpstreamWebsocket(ctx)
	if _, err := e.Execute(ctx, auth, req, opts); err == nil {
		t.Fatal("accepted incremental history tied to another upstream websocket")
	}
	if _, err := e.ExecuteStream(ctx, auth, req, opts); err == nil {
		t.Fatal("accepted streaming incremental history tied to another upstream websocket")
	}
}

func TestExcelPlanReasoningAndValidation(t *testing.T) {
	e := NewCodexExecutor(&config.Config{})
	for _, tc := range []struct {
		name, payload, model string
		from                 sdktranslator.Format
		wantError            bool
	}{
		{"suffix", `{"input":"hello","reasoning":{"effort":"low"}}`, "gpt-5.6-sol-excel(high)", sdktranslator.FormatOpenAIResponse, false},
		{"tool_none", `{"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"none"}`, "gpt-5.6-sol-excel", sdktranslator.FormatOpenAI, false},
		{"bad_json", `{`, "gpt-5.6-sol-excel", sdktranslator.FormatOpenAI, true},
		{"null", `null`, "gpt-5.6-sol-excel", sdktranslator.FormatOpenAIResponse, true},
		{"missing_input", `{}`, "gpt-5.6-sol-excel", sdktranslator.FormatOpenAIResponse, true},
		{"unknown_protocol", `{"input":"hello"}`, "gpt-5.6-sol-excel", sdktranslator.Format("unknown"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := e.prepareExcelPlan(context.Background(), nil, cliproxyexecutor.Request{Model: tc.model, Payload: []byte(tc.payload)}, cliproxyexecutor.Options{SourceFormat: tc.from}, "gpt-5.6-sol-excel")
			if tc.wantError {
				if err == nil {
					t.Fatal("expected validation error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !gjson.GetBytes(plan.body, "stream").Bool() {
				t.Fatal("backend request must always stream")
			}
			if tc.name == "suffix" && gjson.GetBytes(plan.body, "reasoning_effort").String() != "high" {
				t.Fatal(string(plan.body))
			}
			if tc.name == "tool_none" && !plan.catalog.Empty() {
				t.Fatal("tool_choice none ignored")
			}
		})
	}
	_, err := e.Execute(context.Background(), nil, cliproxyexecutor.Request{Model: "gpt-5.6-sol-excel", Payload: []byte(`{"input":"hello"}`)}, cliproxyexecutor.Options{Alt: "responses/compact"})
	if err == nil || !strings.Contains(err.Error(), "not supported for Excel-backed") {
		t.Fatalf("compact must not hit public Codex: %v", err)
	}
}

func TestExcelEmptyOutputFailsAcrossProtocols(t *testing.T) {
	for _, from := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, sdktranslator.FormatClaude} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", from, stream), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"empty\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"empty\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"name\":\"write_range\",\"arguments\":\"{}\"}]}}\n\n")
				}))
				defer server.Close()
				auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "test", "plan_type": "plus"}, Attributes: map[string]string{"excel_base_url": server.URL}}
				payload := `{"input":"hello"}`
				if from != sdktranslator.FormatOpenAIResponse {
					payload = `{"messages":[{"role":"user","content":"hello"}],"max_tokens":100}`
				}
				e := NewCodexExecutor(&config.Config{})
				req := cliproxyexecutor.Request{Model: "gpt-5.6-sol-excel", Payload: []byte(payload)}
				opts := cliproxyexecutor.Options{SourceFormat: from}
				if !stream {
					_, err := e.Execute(context.Background(), auth, req, opts)
					if err == nil || !strings.Contains(err.Error(), "excel_no_usable_output") {
						t.Fatalf("empty turn returned success: %v", err)
					}
					return
				}
				result, err := e.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				failure := false
				for c := range result.Chunks {
					if c.Err != nil || strings.Contains(string(c.Payload), `"type":"response.failed"`) {
						failure = true
					}
					if strings.Contains(string(c.Payload), `"type":"response.completed"`) {
						t.Fatal("empty turn falsely completed")
					}
				}
				if !failure {
					t.Fatal("empty turn silently closed")
				}
			})
		}
	}
}

func TestExcelPrivateInstructionsRemainOptIn(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{Instructions: config.CodexInstructionsConfig{Enabled: true, Content: "private prefs", Models: []string{"gpt-5*"}}}}
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"type": "codex", "allow_private_instructions": true}, Attributes: map[string]string{"allow_private_instructions": "true"}}
	e := NewCodexExecutor(cfg)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol-excel", Payload: []byte(`{"input":"hello","instructions":"caller preferences"}`)}
	for _, private := range []bool{false, true} {
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.CodexPrivateInstructionsMetadataKey: private}}
		plan, err := e.prepareExcelPlan(context.Background(), auth, req, opts, req.Model)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(plan.body), "private prefs") != private {
			t.Fatalf("private instruction opt-in %v: %s", private, plan.body)
		}
		if !strings.Contains(string(plan.body), "caller preferences") {
			t.Fatal("lost caller instructions")
		}
	}
}

// Excel native failures must reach conductor accounting, not merely the wire.
type excelFailureResultHook struct {
	cliproxyauth.NoopHook
	results chan cliproxyauth.Result
}

func (h *excelFailureResultHook) OnResult(_ context.Context, r cliproxyauth.Result) { h.results <- r }

func TestExcelNativeTerminalFailureReachesManager(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex} {
		for _, failure := range []string{"quota", "empty"} {
			t.Run(format.String()+"/"+failure, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"native-failure\",\"status\":\"in_progress\"}}\n\n")
					if failure == "quota" {
						fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"native-failure\",\"status\":\"failed\",\"error\":{\"type\":\"usage_limit_reached\",\"code\":\"usage_limit_reached\",\"message\":\"usage limit reached\"}}}\n\n")
					} else {
						fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"native-failure\",\"status\":\"completed\",\"output\":[]}}\n\n")
					}
				}))
				defer server.Close()
				hook := &excelFailureResultHook{results: make(chan cliproxyauth.Result, 8)}
				m := cliproxyauth.NewManager(nil, nil, hook)
				m.RegisterExecutor(NewCodexExecutor(&config.Config{}))
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Status: cliproxyauth.StatusActive, Metadata: map[string]any{"access_token": "test", "plan_type": "plus"}, Attributes: map[string]string{"excel_base_url": server.URL}}
				if _, err := m.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				const model = "gpt-5.6-sol-excel"
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				stream, err := m.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"input":"hello"}`)}, cliproxyexecutor.Options{SourceFormat: format})
				if err != nil {
					t.Fatal(err)
				}
				errors := 0
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						errors++
					}
				}
				if errors != 1 {
					t.Fatalf("terminal error chunks = %d, want 1", errors)
				}
				if len(hook.results) != 1 {
					t.Fatalf("result count = %d, want 1", len(hook.results))
				}
				result := <-hook.results
				if result.Success || result.Error == nil {
					t.Fatalf("manager recorded successful native failure: %+v", result)
				}
				updated, _ := m.GetByID(auth.ID)
				state := updated.ModelStates[model]
				if state == nil || !state.Unavailable || !state.NextRetryAfter.After(time.Now()) {
					t.Fatalf("native failure missing cooldown: %+v", state)
				}
				if failure == "quota" && (!state.Quota.Exceeded || state.UsageLimitCount != 1) {
					t.Fatalf("quota failure bypassed exhaustion policy: %+v", state)
				}
			})
		}
	}
}
