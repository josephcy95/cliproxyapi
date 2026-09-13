package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// commandCodeUpstreamEventStream is a representative upstream JSONL event stream:
// a reasoning delta, a text delta, a complete tool call, and the terminal finish
// carrying totalUsage.
const commandCodeUpstreamEventStream = "" +
	"{\"type\":\"start\"}\n" +
	"{\"type\":\"start-step\"}\n" +
	"{\"type\":\"reasoning-start\"}\n" +
	"{\"type\":\"reasoning-delta\",\"text\":\"thinking\"}\n" +
	"{\"type\":\"reasoning-end\"}\n" +
	"{\"type\":\"text-start\"}\n" +
	"{\"type\":\"text-delta\",\"text\":\"Hello\"}\n" +
	"{\"type\":\"text-end\"}\n" +
	"{\"type\":\"tool-call\",\"toolCallId\":\"call_1\",\"toolName\":\"lookup\",\"input\":{\"q\":\"x\"}}\n" +
	"{\"type\":\"finish\",\"finishReason\":\"tool-calls\",\"totalUsage\":{\"inputTokens\":100,\"outputTokens\":20,\"cachedInputTokens\":40}}\n"

// newCommandCodeTestServer captures the last upstream request and replays a
// canned JSONL body.
func newCommandCodeTestServer(t *testing.T, body string, status int, captured *http.Request, capturedBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			*captured = *r
		}
		if capturedBody != nil {
			payload, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read upstream body: %v", errRead)
			}
			*capturedBody = payload
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func commandCodeTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "cc-auth",
		Provider: constant.CommandCode,
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "user_test",
			"base_url": baseURL,
		},
	}
}

func commandCodeTestRequest() cliproxyexecutor.Request {
	return cliproxyexecutor.Request{
		Model:   "cc-flash",
		Payload: []byte(`{"model":"cc-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}
}

func TestCommandCodeExecutorPostsEnvelopeToAlphaGenerate(t *testing.T) {
	var captured http.Request
	var capturedBody []byte
	server := newCommandCodeTestServer(t, "{\"type\":\"finish\",\"finishReason\":\"stop\"}\n", http.StatusOK, &captured, &capturedBody)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAI,
		OriginalRequest: commandCodeTestRequest().Payload,
	}
	result, errStream := exec.ExecuteStream(context.Background(), commandCodeTestAuth(server.URL), commandCodeTestRequest(), opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream returned an error: %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	if captured.URL == nil {
		t.Fatal("upstream never received a request")
	}
	if got := captured.URL.Path; got != "/alpha/generate" {
		t.Errorf("upstream path = %q, want /alpha/generate", got)
	}
	// Headers the real CLI sends.
	if got := captured.Header.Get("User-Agent"); got != "cli" {
		t.Errorf("User-Agent = %q, want cli", got)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer user_test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := captured.Header.Get("x-command-code-version"); got != "1.53.1" {
		t.Errorf("x-command-code-version = %q, want the pinned 1.53.1", got)
	}
	if got := captured.Header.Get("x-cli-environment"); got != "production" {
		t.Errorf("x-cli-environment = %q", got)
	}
	if got := captured.Header.Get("x-taste-learning"); got != "true" {
		t.Errorf("x-taste-learning = %q", got)
	}
	// x-session-id and the envelope's threadId must be the same UUID.
	sessionID := captured.Header.Get("x-session-id")
	if sessionID == "" {
		t.Fatal("x-session-id is missing")
	}
	if got := gjson.GetBytes(capturedBody, "threadId").String(); got != sessionID {
		t.Errorf("body threadId = %q, want it to equal x-session-id %q", got, sessionID)
	}
	// x-co-flag does not exist in the real CLI.
	if value := captured.Header.Get("x-co-flag"); value != "" {
		t.Errorf("x-co-flag must not be sent, got %q", value)
	}
	// The transport always streams upstream, even for non-streaming clients.
	if got := gjson.GetBytes(capturedBody, "params.stream"); got.Type != gjson.True {
		t.Errorf("params.stream = %s, want true", got.Raw)
	}
}

func TestCommandCodeExecutorStreamTranslatesToOpenAISSE(t *testing.T) {
	server := newCommandCodeTestServer(t, commandCodeUpstreamEventStream, http.StatusOK, nil, nil)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	req := commandCodeTestRequest()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload}
	result, errStream := exec.ExecuteStream(context.Background(), commandCodeTestAuth(server.URL), req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream returned an error: %v", errStream)
	}

	var content, reasoning strings.Builder
	var sawToolCall, sawFinish bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload := strings.TrimSpace(string(chunk.Payload))
		payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
		// The [DONE] sentinel is consumed by the OpenAI->OpenAI translator on
		// purpose; the HTTP handler writes its own terminal sentinel at channel
		// close, so it must not be asserted here.
		if payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			continue
		}
		content.WriteString(gjson.Get(payload, "choices.0.delta.content").String())
		reasoning.WriteString(gjson.Get(payload, "choices.0.delta.reasoning_content").String())
		if gjson.Get(payload, "choices.0.delta.tool_calls").Exists() {
			sawToolCall = true
		}
		// Non-terminal chunks carry "finish_reason":null, and gjson reports that
		// as existing, so match on the string type instead.
		if finishReason := gjson.Get(payload, "choices.0.finish_reason"); finishReason.Type == gjson.String {
			sawFinish = true
			if got := finishReason.String(); got != "tool_calls" {
				t.Errorf("finish_reason = %q, want tool_calls", got)
			}
			if got := gjson.Get(payload, "usage.completion_tokens").Int(); got != 20 {
				t.Errorf("usage.completion_tokens = %d, want 20", got)
			}
		}
	}

	if got := content.String(); got != "Hello" {
		t.Errorf("assembled content = %q, want Hello", got)
	}
	if got := reasoning.String(); got != "thinking" {
		t.Errorf("assembled reasoning_content = %q, want thinking", got)
	}
	if !sawToolCall {
		t.Error("no tool call delta reached the client")
	}
	if !sawFinish {
		t.Error("no finish frame reached the client")
	}
}

// TestCommandCodeExecutorStreamSendsTerminalSentinelForClaudeClients proves the
// translator is fed the terminal [DONE] sentinel: Anthropic clients need it to
// emit message_stop, unlike OpenAI clients whose handler writes its own.
func TestCommandCodeExecutorStreamSendsTerminalSentinelForClaudeClients(t *testing.T) {
	server := newCommandCodeTestServer(t, commandCodeUpstreamEventStream, http.StatusOK, nil, nil)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	req := commandCodeTestRequest()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: req.Payload}
	result, errStream := exec.ExecuteStream(context.Background(), commandCodeTestAuth(server.URL), req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream returned an error: %v", errStream)
	}

	var sawMessageStart, sawMessageStop, sawThinking, sawText, sawToolUse bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		event := gjson.GetBytes(chunk.Payload, "type").String()
		switch event {
		case "message_start":
			sawMessageStart = true
		case "message_stop":
			sawMessageStop = true
		case "content_block_start":
			switch gjson.GetBytes(chunk.Payload, "content_block.type").String() {
			case "thinking":
				sawThinking = true
			case "text":
				sawText = true
			case "tool_use":
				sawToolUse = true
			}
		}
	}
	// Every frame must be fed to the translators as an SSE "data:" line; the
	// Claude translator silently drops bare JSON, which would deliver an empty
	// response with only a terminal event.
	if !sawMessageStart {
		t.Error("Claude client never received message_start")
	}
	if !sawThinking {
		t.Error("Claude client never received the thinking content block (reasoning_content was dropped)")
	}
	if !sawText {
		t.Error("Claude client never received the text content block")
	}
	if !sawToolUse {
		t.Error("Claude client never received the tool_use content block")
	}
	if !sawMessageStop {
		t.Error("Claude client never received message_stop; the terminal sentinel was not forwarded")
	}
}

func TestCommandCodeExecutorAssemblesNonStreamingResponse(t *testing.T) {
	server := newCommandCodeTestServer(t, commandCodeUpstreamEventStream, http.StatusOK, nil, nil)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	req := commandCodeTestRequest()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload}
	resp, errExecute := exec.Execute(context.Background(), commandCodeTestAuth(server.URL), req, opts)
	if errExecute != nil {
		t.Fatalf("Execute returned an error: %v", errExecute)
	}

	if got := gjson.GetBytes(resp.Payload, "object").String(); got != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion: %s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "Hello" {
		t.Errorf("content = %q", got)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.reasoning_content").String(); got != "thinking" {
		t.Errorf("reasoning_content = %q", got)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.tool_calls.0.function.name").String(); got != "lookup" {
		t.Errorf("tool call name = %q", got)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Errorf("finish_reason = %q", got)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.completion_tokens").Int(); got != 20 {
		t.Errorf("completion_tokens = %d", got)
	}
}

func TestCommandCodeExecutorTranslatesClaudeClientPayload(t *testing.T) {
	// A Claude client sends Anthropic-shaped JSON; the executor must translate it
	// to OpenAI first, which folds the system prompt into params.system.
	var capturedBody []byte
	server := newCommandCodeTestServer(t, "{\"type\":\"text-delta\",\"text\":\"ok\"}\n{\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":1,\"outputTokens\":1}}\n",
		http.StatusOK, nil, &capturedBody)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	payload := []byte(`{"model":"cc-flash","max_tokens":100,"system":"be brief","messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"because"},{"type":"text","text":"hello"}]},
		{"role":"user","content":[{"type":"text","text":"again"}]}
	]}`)
	req := cliproxyexecutor.Request{Model: "cc-flash", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload}
	result, errStream := exec.ExecuteStream(context.Background(), commandCodeTestAuth(server.URL), req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream returned an error: %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	if got := gjson.GetBytes(capturedBody, "params.system").String(); got != "be brief" {
		t.Errorf("params.system = %q, want the Claude system prompt", got)
	}
	// Without is-compat the fork's Claude->OpenAI translator deliberately drops
	// unsigned thinking blocks, so the assistant turn carries only its text here.
	blocks := gjson.GetBytes(capturedBody, "params.messages.1.content")
	if got := blocks.Get("0.type").String(); got != "text" {
		t.Fatalf("assistant block 0 = %s, want text", blocks.Raw)
	}
}

func TestCommandCodeExecutorPromotesBillingStatus(t *testing.T) {
	server := newCommandCodeTestServer(t, `{"error":{"message":"You have insufficient credits to make this request."}}`,
		http.StatusBadRequest, nil, nil)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	req := commandCodeTestRequest()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload}

	_, errExecute := exec.Execute(context.Background(), commandCodeTestAuth(server.URL), req, opts)
	if errExecute == nil {
		t.Fatal("expected an error for the billing failure")
	}
	status, ok := errExecute.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose a status code", errExecute)
	}
	if got := status.StatusCode(); got != http.StatusPaymentRequired {
		t.Errorf("StatusCode() = %d, want 402 so only the model cools down", got)
	}
}

func TestCommandCodeExecutorStreamPromotesBillingStatus(t *testing.T) {
	server := newCommandCodeTestServer(t, `{"error":{"message":"insufficient credits"}}`, http.StatusBadRequest, nil, nil)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	req := commandCodeTestRequest()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload}
	_, errStream := exec.ExecuteStream(context.Background(), commandCodeTestAuth(server.URL), req, opts)
	if errStream == nil {
		t.Fatal("expected an error for the billing failure")
	}
	status, ok := errStream.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose a status code", errStream)
	}
	if got := status.StatusCode(); got != http.StatusPaymentRequired {
		t.Errorf("StatusCode() = %d, want 402", got)
	}
}

func TestCommandCodeExecutorNonBillingStatusNotPromoted(t *testing.T) {
	server := newCommandCodeTestServer(t, `{"error":{"message":"unknown model: nope","type":"invalid_request_error"}}`,
		http.StatusBadRequest, nil, nil)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	req := commandCodeTestRequest()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload}
	_, errExecute := exec.Execute(context.Background(), commandCodeTestAuth(server.URL), req, opts)
	if errExecute == nil {
		t.Fatal("expected an error")
	}
	status, ok := errExecute.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose a status code", errExecute)
	}
	if got := status.StatusCode(); got != http.StatusBadRequest {
		t.Errorf("StatusCode() = %d, want 400 for a non-billing failure", got)
	}
}

func TestCommandCodeExecutorStreamTerminalMarkerIsHardError(t *testing.T) {
	// A terminal plan/billing marker mid-stream must fail the client request
	// rather than completing it as an empty answer.
	server := newCommandCodeTestServer(t, "{\"type\":\"text-delta\",\"text\":\"par\"}\n"+
		"{\"type\":\"error\",\"error\":{\"message\":\"model_not_in_plan\"}}\n", http.StatusOK, nil, nil)
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	req := commandCodeTestRequest()
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: req.Payload}
	result, errStream := exec.ExecuteStream(context.Background(), commandCodeTestAuth(server.URL), req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream returned an error: %v", errStream)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("the terminal marker must surface as a stream error")
	}
	status, ok := streamErr.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose a status code", streamErr)
	}
	if got := status.StatusCode(); got != http.StatusForbidden {
		t.Errorf("StatusCode() = %d, want 403 so only the model cools down", got)
	}
	if !strings.Contains(streamErr.Error(), "model_not_in_plan") {
		t.Errorf("error %q should carry the marker", streamErr.Error())
	}
}

func TestCommandCodeExecutorMissingBaseURL(t *testing.T) {
	// An entry with no configured base URL must resolve to the vendor host. This
	// asserts the resolution itself instead of issuing a request: a real request
	// would add bound-dependent outbound I/O to the unit suite AND could not
	// distinguish "default base URL resolved" from "DNS failed", "context
	// deadline exceeded", or any other transport failure.
	if got := commandCodeBaseURL("", nil); got != helps.CommandCodeDefaultBaseURL {
		t.Errorf("base URL with no auth and no entry = %q, want %q", got, helps.CommandCodeDefaultBaseURL)
	}
	if got := commandCodeBaseURL("", &config.CommandCodeKey{APIKey: "user_test"}); got != helps.CommandCodeDefaultBaseURL {
		t.Errorf("base URL for an entry without base-url = %q, want %q", got, helps.CommandCodeDefaultBaseURL)
	}
	if got := commandCodeBaseURL("https://alt.example.com", &config.CommandCodeKey{APIKey: "user_test"}); got != "https://alt.example.com" {
		t.Errorf("base URL = %q, want the auth attribute to win", got)
	}
	// The entry is the fallback when the auth carries no base_url attribute.
	if got := commandCodeBaseURL("", &config.CommandCodeKey{BaseURL: "https://from-config.example.com"}); got != "https://from-config.example.com" {
		t.Errorf("base URL = %q, want the config entry fallback", got)
	}
}

func TestCommandCodeExecutorIdentifierAndPrepareRequest(t *testing.T) {
	exec := NewCommandCodeExecutor(&config.Config{})
	if got := exec.Identifier(); got != constant.CommandCode {
		t.Errorf("Identifier() = %q, want %q", got, constant.CommandCode)
	}

	httpReq, errRequest := http.NewRequest(http.MethodPost, "https://api.commandcode.ai/alpha/generate", nil)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	if errPrepare := exec.PrepareRequest(httpReq, commandCodeTestAuth("https://api.commandcode.ai")); errPrepare != nil {
		t.Fatalf("PrepareRequest returned an error: %v", errPrepare)
	}
	if got := httpReq.Header.Get("Authorization"); got != "Bearer user_test" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestCommandCodeExecutorResolveConfigByIndex(t *testing.T) {
	cfg := &config.Config{
		CommandCodeKey: []config.CommandCodeKey{
			{APIKey: "user_a", BaseURL: "https://a.example"},
			{APIKey: "user_b", BaseURL: "https://b.example", Models: []config.CommandCodeModel{{Name: "m", Alias: "alias"}}},
		},
	}
	exec := NewCommandCodeExecutor(cfg)
	auth := &cliproxyauth.Auth{
		ID:       "cc-auth",
		Provider: constant.CommandCode,
		Attributes: map[string]string{
			"api_key":      "user_b",
			"base_url":     "https://b.example",
			"config_index": "1",
			"source":       "config:commandcode[abc]",
		},
	}
	entry := exec.resolveConfig(auth)
	if entry == nil {
		t.Fatal("resolveConfig returned nil for an indexed auth")
	}
	if entry.APIKey != "user_b" {
		t.Errorf("resolved APIKey = %q, want user_b", entry.APIKey)
	}
}
