package helps

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

// commandCodeEnvelopeKeys is the verified nine-key envelope contract. The
// gateway validates their presence, so a missing key is a hard failure.
var commandCodeEnvelopeKeys = []string{
	"config", "memory", "taste", "skills", "permissionMode",
	"threadId", "mode", "promptCache", "params",
}

func decodeEnvelope(t *testing.T, payload []byte) map[string]json.RawMessage {
	t.Helper()
	var decoded map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(payload, &decoded); errUnmarshal != nil {
		t.Fatalf("envelope is not a JSON object: %v\n%s", errUnmarshal, payload)
	}
	return decoded
}

func buildTestEnvelope(t *testing.T, payload string, in CommandCodeEnvelopeInput) []byte {
	t.Helper()
	envelope, errBuild := BuildCommandCodeEnvelope([]byte(payload), in)
	if errBuild != nil {
		t.Fatalf("BuildCommandCodeEnvelope returned an error: %v", errBuild)
	}
	return envelope
}

// fixedTestUUID is a valid UUID used wherever the envelope requires one.
const fixedTestUUID = "3f1d0a52-9b0c-4f4a-8f4e-1c2d3e4f5a6b"

func TestBuildCommandCodeEnvelopeHasNineTopLevelKeys(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`,
		CommandCodeEnvelopeInput{Model: "deepseek/deepseek-v4-flash", ThreadID: fixedTestUUID})

	decoded := decodeEnvelope(t, envelope)
	if len(decoded) != len(commandCodeEnvelopeKeys) {
		t.Fatalf("envelope has %d top-level keys, want %d: %s", len(decoded), len(commandCodeEnvelopeKeys), envelope)
	}
	for _, key := range commandCodeEnvelopeKeys {
		if _, ok := decoded[key]; !ok {
			t.Errorf("envelope is missing required key %q: %s", key, envelope)
		}
	}
	if got := gjson.GetBytes(envelope, "memory").Raw; got != "null" {
		t.Errorf("memory = %s, want null", got)
	}
	if got := gjson.GetBytes(envelope, "taste").Raw; got != "null" {
		t.Errorf("taste = %s, want null", got)
	}
	if got := gjson.GetBytes(envelope, "skills").Raw; got != "null" {
		t.Errorf("skills = %s, want null", got)
	}
	if got := gjson.GetBytes(envelope, "permissionMode").String(); got != CommandCodeDefaultPermissionMode {
		t.Errorf("permissionMode = %q, want %q", got, CommandCodeDefaultPermissionMode)
	}
	if got := gjson.GetBytes(envelope, "mode").String(); got != CommandCodeDefaultMode {
		t.Errorf("mode = %q, want %q", got, CommandCodeDefaultMode)
	}
	if got := gjson.GetBytes(envelope, "promptCache").String(); got != CommandCodeDefaultPromptCache {
		t.Errorf("promptCache = %q, want %q", got, CommandCodeDefaultPromptCache)
	}
	if got := gjson.GetBytes(envelope, "threadId").String(); got != fixedTestUUID {
		t.Errorf("threadId = %q, want the session uuid %q", got, fixedTestUUID)
	}
}

// TestBuildCommandCodeEnvelopeModeAndPromptCacheMatchLiveValidation pins the two
// envelope values a live upstream request rejected.
//
// Upstream returned HTTP 400 with:
//
//	Validation error: Invalid option: expected one of "agent"|"learning"|
//	"custom-agent"|"custom-agent-create"|"title-gen"|"tool-desc"|"compact"|
//	"vision" at "mode"; Invalid input: expected "off" at "promptCache"
//
// So `mode` must be "agent" and `promptCache` must be "off". These look
// arbitrary and will tempt a future cleanup, hence the explicit test.
func TestBuildCommandCodeEnvelopeModeAndPromptCacheMatchLiveValidation(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`,
		CommandCodeEnvelopeInput{Model: "deepseek/deepseek-v4-flash", ThreadID: fixedTestUUID})

	if got := gjson.GetBytes(envelope, "mode").String(); got != "agent" {
		t.Errorf("mode = %q, want the upstream-accepted %q", got, "agent")
	}
	if got := gjson.GetBytes(envelope, "promptCache").String(); got != "off" {
		t.Errorf("promptCache = %q, want the upstream-accepted %q", got, "off")
	}
	// Guard the constants themselves so a change to either is caught here.
	if CommandCodeDefaultMode != "agent" {
		t.Errorf("CommandCodeDefaultMode = %q, want agent", CommandCodeDefaultMode)
	}
	if CommandCodeDefaultPromptCache != "off" {
		t.Errorf("CommandCodeDefaultPromptCache = %q, want off", CommandCodeDefaultPromptCache)
	}
}

func TestBuildCommandCodeEnvelopeConfigBlock(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		CommandCodeEnvelopeInput{
			Model:      "m",
			ThreadID:   fixedTestUUID,
			WorkingDir: `C:\Users\dev\projects\web-a3f2`,
			Date:       "2026-01-01",
			Platform:   "win32",
		})

	if got := gjson.GetBytes(envelope, "config.workingDir").String(); got != `C:\Users\dev\projects\web-a3f2` {
		t.Errorf("config.workingDir = %q", got)
	}
	if got := gjson.GetBytes(envelope, "config.date").String(); got != "2026-01-01" {
		t.Errorf("config.date = %q", got)
	}
	// config.environment is a platform word in the real CLI, not a Node banner.
	if got := gjson.GetBytes(envelope, "config.environment").String(); got != "win32" {
		t.Errorf("config.environment = %q, want the platform word", got)
	}
	for _, path := range []string{"config.structure", "config.recentCommits"} {
		node := gjson.GetBytes(envelope, path)
		if !node.IsArray() {
			t.Errorf("%s should be an empty array, got %s", path, node.Raw)
		}
	}
	if gjson.GetBytes(envelope, "config.isGitRepo").Bool() {
		t.Error("config.isGitRepo should default to false")
	}
}

func TestBuildCommandCodeEnvelopeAlwaysStreams(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`,
		CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})

	if got := gjson.GetBytes(envelope, "params.stream"); got.Type != gjson.True {
		t.Fatalf("params.stream = %s, want true: the CLI transport always streams", got.Raw)
	}
	if got := gjson.GetBytes(envelope, "params.max_tokens").Int(); got != CommandCodeDefaultMaxTokens {
		t.Errorf("params.max_tokens = %d, want %d", got, int64(CommandCodeDefaultMaxTokens))
	}
}

func TestBuildCommandCodeEnvelopeMaxTokensFromRequest(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","max_tokens":1234,"messages":[{"role":"user","content":"hi"}]}`,
		CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})
	if got := gjson.GetBytes(envelope, "params.max_tokens").Int(); got != 1234 {
		t.Errorf("params.max_tokens = %d, want 1234", got)
	}

	// max_completion_tokens wins when both are present, matching OpenAI clients
	// that send only the newer field.
	envelope = buildTestEnvelope(t, `{"model":"m","max_tokens":1234,"max_completion_tokens":99,"messages":[{"role":"user","content":"hi"}]}`,
		CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})
	if got := gjson.GetBytes(envelope, "params.max_tokens").Int(); got != 99 {
		t.Errorf("params.max_tokens = %d, want 99", got)
	}
}

func TestBuildCommandCodeEnvelopeEmptySystemPromptPlaceholder(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})
	if got := gjson.GetBytes(envelope, "params.system").String(); got != CommandCodeEmptySystemPrompt {
		t.Fatalf("params.system = %q, want a single space so the gateway does not inject its own system prompt", got)
	}
}

func TestBuildCommandCodeEnvelopeFoldsSystemMessages(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[
		{"role":"system","content":"first"},
		{"role":"developer","content":[{"type":"text","text":"second"}]},
		{"role":"user","content":"hi"}
	]}`, CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})

	if got := gjson.GetBytes(envelope, "params.system").String(); got != "first\nsecond" {
		t.Errorf("params.system = %q, want the folded system text", got)
	}
	messages := gjson.GetBytes(envelope, "params.messages")
	if length := len(messages.Array()); length != 1 {
		t.Fatalf("params.messages has %d entries, want 1 (system/developer folded out): %s", length, messages.Raw)
	}
	if got := messages.Get("0.role").String(); got != "user" {
		t.Errorf("params.messages.0.role = %q, want user", got)
	}
}

func TestBuildCommandCodeEnvelopeMessageRolesAndBlocks(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]},
		{"role":"assistant","content":"answer"},
		{"role":"tool","tool_call_id":"call_1","content":"42"}
	]}`, CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})

	messages := gjson.GetBytes(envelope, "params.messages").Array()
	if len(messages) != 3 {
		t.Fatalf("params.messages has %d entries, want 3: %s", len(messages), gjson.GetBytes(envelope, "params.messages").Raw)
	}

	// user: text then image, in the CLI's own shapes.
	if got := messages[0].Get("content.0.type").String(); got != "text" {
		t.Errorf("user block 0 type = %q, want text", got)
	}
	if got := messages[0].Get("content.0.text").String(); got != "look" {
		t.Errorf("user block 0 text = %q", got)
	}
	if got := messages[0].Get("content.1.type").String(); got != "image" {
		t.Errorf("user block 1 type = %q, want image", got)
	}
	if got := messages[0].Get("content.1.image").String(); got != "https://example.com/a.png" {
		t.Errorf("user image block url = %q", got)
	}

	// assistant: string content becomes a text block.
	if got := messages[1].Get("role").String(); got != "assistant" {
		t.Errorf("role = %q", got)
	}
	if got := messages[1].Get("content.0.type").String(); got != "text" {
		t.Errorf("assistant block type = %q, want text", got)
	}
	if got := messages[1].Get("content.0.text").String(); got != "answer" {
		t.Errorf("assistant text = %q", got)
	}

	// tool: tool-result with the output envelope.
	if got := messages[2].Get("role").String(); got != "tool" {
		t.Errorf("role = %q, want tool", got)
	}
	if got := messages[2].Get("content.0.type").String(); got != "tool-result" {
		t.Errorf("tool block type = %q", got)
	}
	if got := messages[2].Get("content.0.toolCallId").String(); got != "call_1" {
		t.Errorf("toolCallId = %q", got)
	}
	if got := messages[2].Get("content.0.output.type").String(); got != "text" {
		t.Errorf("tool output type = %q, want text", got)
	}
	if got := messages[2].Get("content.0.output.value").String(); got != "42" {
		t.Errorf("tool output value = %q", got)
	}
}

func TestBuildCommandCodeEnvelopeReasoningPrecedesTextAndToolCalls(t *testing.T) {
	// Replaying reasoning first is required: the gateway rejects DeepSeek
	// thinking-mode tool loops whose assistant turn lost its reasoning block.
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","reasoning_content":"why","content":"answer","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}
		]}
	]}`, CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})

	blocks := gjson.GetBytes(envelope, "params.messages.1.content")
	if length := len(blocks.Array()); length != 3 {
		t.Fatalf("assistant content has %d blocks, want reasoning+text+tool-call: %s", length, blocks.Raw)
	}
	if got := blocks.Get("0.type").String(); got != "reasoning" {
		t.Errorf("block 0 type = %q, want reasoning first", got)
	}
	if got := blocks.Get("0.text").String(); got != "why" {
		t.Errorf("block 0 text = %q", got)
	}
	if got := blocks.Get("1.type").String(); got != "text" {
		t.Errorf("block 1 type = %q, want text", got)
	}
	if got := blocks.Get("2.type").String(); got != "tool-call" {
		t.Errorf("block 2 type = %q, want tool-call", got)
	}
	if got := blocks.Get("2.toolCallId").String(); got != "call_1" {
		t.Errorf("toolCallId = %q", got)
	}
	if got := blocks.Get("2.toolName").String(); got != "lookup" {
		t.Errorf("toolName = %q", got)
	}
	// input must be the parsed object, not the JSON string.
	if got := blocks.Get("2.input"); got.Type != gjson.JSON || got.Get("q").String() != "x" {
		t.Errorf("tool-call input = %s, want the parsed object", got.Raw)
	}
}

func TestBuildCommandCodeEnvelopeReasoningOnlyAssistantTurn(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","reasoning_content":"thinking"}
	]}`, CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})
	blocks := gjson.GetBytes(envelope, "params.messages.1.content")
	if got := blocks.Get("0.type").String(); got != "reasoning" {
		t.Fatalf("assistant reasoning-only turn = %s", blocks.Raw)
	}
}

func TestBuildCommandCodeEnvelopeToolResultNameFromReverseMap(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"result"}
	]}`, CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})

	if got := gjson.GetBytes(envelope, "params.messages.2.content.0.toolName").String(); got != "lookup" {
		t.Errorf("toolName = %q, want the name resolved from the prior assistant tool_calls", got)
	}
}

func TestBuildCommandCodeEnvelopeToolErrorOutputType(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[
		{"role":"tool","tool_call_id":"call_1","name":"lookup","is_error":true,"content":"boom"}
	]}`, CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})
	if got := gjson.GetBytes(envelope, "params.messages.0.content.0.output.type").String(); got != "error-text" {
		t.Errorf("tool output type = %q, want error-text", got)
	}
}

func TestBuildCommandCodeEnvelopeToolsShape(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[
		{"type":"function","function":{"name":"lookup","description":"look things up","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}},
		{"type":"web_search"}
	],"tool_choice":"auto"}`, CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})

	tools := gjson.GetBytes(envelope, "params.tools")
	if length := len(tools.Array()); length != 1 {
		t.Fatalf("params.tools has %d entries, want 1 (only function tools have a CLI equivalent): %s", length, tools.Raw)
	}
	if got := tools.Get("0.type").String(); got != "function" {
		t.Errorf("tool type = %q", got)
	}
	if got := tools.Get("0.name").String(); got != "lookup" {
		t.Errorf("tool name = %q, want it flattened", got)
	}
	if got := tools.Get("0.input_schema.type").String(); got != "object" {
		t.Errorf("input_schema.type = %q", got)
	}
	if got := gjson.GetBytes(envelope, "params.tool_choice.type").String(); got != "auto" {
		t.Errorf("params.tool_choice.type = %q, want auto", got)
	}
}

func TestBuildCommandCodeEnvelopeOmitsAbsentOptionals(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})
	for _, path := range []string{"params.temperature", "params.reasoning_effort", "params.tools", "params.tool_choice"} {
		if gjson.GetBytes(envelope, path).Exists() {
			t.Errorf("%s should be omitted when the client did not send it: %s", path, envelope)
		}
	}
}

func TestBuildCommandCodeEnvelopeForwardsOptionals(t *testing.T) {
	envelope := buildTestEnvelope(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.3,"reasoning_effort":"high","parallel_tool_calls":true}`,
		CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID})
	if got := gjson.GetBytes(envelope, "params.temperature").Float(); got != 0.3 {
		t.Errorf("params.temperature = %v, want 0.3", got)
	}
	if got := gjson.GetBytes(envelope, "params.reasoning_effort").String(); got != "high" {
		t.Errorf("params.reasoning_effort = %q", got)
	}
	if got := gjson.GetBytes(envelope, "params.parallel_tool_calls"); got.Type != gjson.True {
		t.Errorf("params.parallel_tool_calls = %s, want true", got.Raw)
	}
}

func TestBuildCommandCodeEnvelopeRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		in      CommandCodeEnvelopeInput
	}{
		{name: "missing model", payload: `{"messages":[{"role":"user","content":"hi"}]}`, in: CommandCodeEnvelopeInput{ThreadID: fixedTestUUID}},
		{name: "missing messages", payload: `{"model":"m"}`, in: CommandCodeEnvelopeInput{Model: "m", ThreadID: fixedTestUUID}},
		{name: "non-uuid threadId", payload: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, in: CommandCodeEnvelopeInput{Model: "m", ThreadID: "not-a-uuid"}},
		{name: "empty threadId", payload: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, in: CommandCodeEnvelopeInput{Model: "m"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, errBuild := BuildCommandCodeEnvelope([]byte(test.payload), test.in); errBuild == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestCommandCodeProjectSlug(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "windows drive path", in: `C:\Users\Dev\Projects\Web-A3F2`, want: "users-dev-projects-web-a3f2"},
		{name: "unix path", in: "/home/dev/projects/web", want: "home-dev-projects-web"},
		{name: "already a slug", in: "my-project", want: "my-project"},
		{name: "collapses runs", in: "/a//b___c", want: "a-b-c"},
		{name: "trims dashes", in: "__a__", want: "a"},
		{name: "empty falls back", in: "", want: "project"},
		{name: "punctuation only falls back", in: "///", want: "project"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CommandCodeProjectSlug(test.in); got != test.want {
				t.Errorf("CommandCodeProjectSlug(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestCommandCodeSessionUUIDIsStableAndValid(t *testing.T) {
	first := CommandCodeSessionUUID("auth-1")
	second := CommandCodeSessionUUID("auth-1")
	if first != second {
		t.Fatalf("session uuid is not stable per auth: %q vs %q", first, second)
	}
	if first == CommandCodeSessionUUID("auth-2") {
		t.Fatal("different auths must not share a session uuid")
	}
	if !commandCodeIsUUID(first) {
		t.Fatalf("session uuid %q is not a UUID", first)
	}
	if !commandCodeIsUUID(CommandCodeSessionUUID("")) {
		t.Fatal("an empty auth id must still produce a real UUID")
	}
}

func TestNormalizeCommandCodeStatusError(t *testing.T) {
	tests := []struct {
		name       string
		httpStatus int
		body       string
		want       int
	}{
		{
			name:       "billing 400 promotes to 402",
			httpStatus: http.StatusBadRequest,
			body:       `{"error":{"message":"You have insufficient credits to make this request."}}`,
			want:       http.StatusPaymentRequired,
		},
		{
			name:       "existing 402 stays 402",
			httpStatus: http.StatusPaymentRequired,
			body:       `{"error":{"message":"insufficient credits"}}`,
			want:       http.StatusPaymentRequired,
		},
		{
			name:       "non-billing 400 stays 400",
			httpStatus: http.StatusBadRequest,
			body:       `{"error":{"message":"unknown model: nope","type":"invalid_request_error"}}`,
			want:       http.StatusBadRequest,
		},
		{
			name:       "unrelated status is untouched",
			httpStatus: http.StatusTooManyRequests,
			body:       `{"error":{"message":"insufficient credits"}}`,
			want:       http.StatusTooManyRequests,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := NormalizeCommandCodeStatusError(test.httpStatus, []byte(test.body)); got != test.want {
				t.Errorf("NormalizeCommandCodeStatusError(%d, %s) = %d, want %d", test.httpStatus, test.body, got, test.want)
			}
		})
	}
}

func TestApplyCommandCodeHeaders(t *testing.T) {
	req, errRequest := http.NewRequest(http.MethodPost, "https://api.commandcode.ai/alpha/generate", nil)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	ApplyCommandCodeHeaders(req, nil, "user_abc", fixedTestUUID)

	// User-Agent is literally "cli" in the real CLI.
	if got := req.Header.Get("User-Agent"); got != "cli" {
		t.Errorf("User-Agent = %q, want cli", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer user_abc" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("x-command-code-version"); got != CommandCodeDefaultVersion {
		t.Errorf("x-command-code-version = %q, want %q", got, CommandCodeDefaultVersion)
	}
	if got := req.Header.Get("x-cli-environment"); got != "production" {
		t.Errorf("x-cli-environment = %q, want production", got)
	}
	if got := req.Header.Get("x-project-slug"); got != "tmp" {
		t.Errorf("x-project-slug = %q, want the slug of the default working dir", got)
	}
	if got := req.Header.Get("x-taste-learning"); got != "true" {
		t.Errorf("x-taste-learning = %q, want true", got)
	}
	if got := req.Header.Get("x-session-id"); got != fixedTestUUID {
		t.Errorf("x-session-id = %q, want the session uuid", got)
	}
	// x-co-flag does not exist in the real CLI and must never be sent.
	if value := req.Header.Get("x-co-flag"); value != "" {
		t.Errorf("x-co-flag must not be sent, got %q", value)
	}
	for name := range req.Header {
		if strings.EqualFold(name, "x-co-flag") {
			t.Errorf("x-co-flag must not be present, found %q", name)
		}
	}
}

func TestCommandCodeHeaderOverride(t *testing.T) {
	if got := CommandCodeHeaderOverride(nil, "x-command-code-version"); got != "" {
		t.Errorf("nil entry = %q, want empty", got)
	}
	entry := &config.CommandCodeKey{Headers: map[string]string{"X-Command-Code-Version": "9.9.9"}}
	if got := CommandCodeHeaderOverride(entry, "x-command-code-version"); got != "9.9.9" {
		t.Errorf("override = %q, want 9.9.9 (header names match case-insensitively)", got)
	}
	if got := CommandCodeHeaderOverride(entry, "x-project-slug"); got != "" {
		t.Errorf("unset header = %q, want empty", got)
	}

	req, errRequest := http.NewRequest(http.MethodPost, "https://api.commandcode.ai/alpha/generate", nil)
	if errRequest != nil {
		t.Fatalf("build request: %v", errRequest)
	}
	ApplyCommandCodeHeaders(req, entry, "user_abc", fixedTestUUID)
	if got := req.Header.Get("x-command-code-version"); got != "9.9.9" {
		t.Errorf("x-command-code-version = %q, want the configured override", got)
	}
}

// TestCommandCodeModelUpstreamModelResolvesAlias pins the alias -> upstream
// mapping. It is load-bearing: the conductor's alias tables have no
// "commandcode" case, so the requested model reaches the executor verbatim and
// this function is the only thing standing between a client's alias and an
// invalid upstream model name. Verified live by capturing the outbound request
// for a configured alias (`cc-flash` -> `deepseek/deepseek-v4-flash`).
func TestCommandCodeModelUpstreamModelResolvesAlias(t *testing.T) {
	models := []commandCodeTestModelEntry{
		{name: "deepseek/deepseek-v4-flash", alias: "cc-flash"},
		{name: "deepseek/deepseek-v4-pro"},
	}

	cases := []struct {
		name      string
		requested string
		fallback  string
		want      string
	}{
		{"alias resolves to upstream name", "cc-flash", "unused", "deepseek/deepseek-v4-flash"},
		{"alias match is case-insensitive", "CC-FLASH", "unused", "deepseek/deepseek-v4-flash"},
		{"upstream name passes through", "deepseek/deepseek-v4-pro", "unused", "deepseek/deepseek-v4-pro"},
		{"entry without an alias uses its own name", "deepseek/deepseek-v4-pro", "unused", "deepseek/deepseek-v4-pro"},
		// An unmatched model passes through unchanged: the configured list may be
		// a subset of the live catalog, so a valid-but-unlisted name must not be
		// rewritten. The fallback only applies when nothing was requested.
		{"unmatched model passes through", "not-configured", "fallback-model", "not-configured"},
		{"empty requested uses the fallback", "", "fallback-model", "fallback-model"},
		{"empty requested with no fallback stays empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommandCodeModelUpstreamModel(models, tc.requested, tc.fallback); got != tc.want {
				t.Errorf("upstream model for %q = %q, want %q", tc.requested, got, tc.want)
			}
		})
	}
}

// commandCodeTestModelEntry satisfies CommandCodeModelEntry for the alias test.
type commandCodeTestModelEntry struct {
	name  string
	alias string
}

func (m commandCodeTestModelEntry) GetName() string  { return m.name }
func (m commandCodeTestModelEntry) GetAlias() string { return m.alias }
