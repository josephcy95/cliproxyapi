package helps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Command Code stream event types.
const (
	commandCodeEventStart           = "start"
	commandCodeEventStartStep       = "start-step"
	commandCodeEventFinishStep      = "finish-step"
	commandCodeEventFinish          = "finish"
	commandCodeEventError           = "error"
	commandCodeEventTextStart       = "text-start"
	commandCodeEventTextDelta       = "text-delta"
	commandCodeEventTextEnd         = "text-end"
	commandCodeEventReasoningStart  = "reasoning-start"
	commandCodeEventReasoningDelta  = "reasoning-delta"
	commandCodeEventReasoningEnd    = "reasoning-end"
	commandCodeEventToolCall        = "tool-call"
	commandCodeEventToolInputStart  = "tool-input-start"
	commandCodeEventToolInputDelta  = "tool-input-delta"
	commandCodeEventToolInputEnd    = "tool-input-end"
	commandCodeEventToolResult      = "tool-result"
	commandCodeEventToolError       = "tool-error"
	commandCodeEventProviderMeta    = "provider-metadata"
	commandCodeOpenAIDoneSentinel   = "data: [DONE]"
	commandCodeFinishReasonStopped  = "stop"
	commandCodeFinishReasonToolCall = "tool_calls"
	commandCodeFinishReasonLength   = "length"
)

// commandCodeTerminalStreamMarkers are the upstream error markers that can never
// succeed on retry: the account's plan or credit state must change first. They
// map onto 402/403 so only the affected model cools down.
var commandCodeTerminalStreamMarkers = []struct {
	marker string
	status int
}{
	{marker: "premium_credits_exhausted", status: http.StatusForbidden},
	{marker: "model_not_in_plan", status: http.StatusForbidden},
	{marker: "insufficient credits", status: http.StatusPaymentRequired},
}

// CommandCodeStreamEvent is one decoded upstream JSONL event.
type CommandCodeStreamEvent struct {
	Type         string
	Text         string
	ToolCallID   string
	ToolName     string
	Input        []byte
	FinishReason string
	Usage        *commandCodeUsage
	Error        *CommandCodeStreamEventError
}

// CommandCodeStreamEventError is the payload of an upstream `error` event.
type CommandCodeStreamEventError struct {
	Message    string
	StatusCode int
	Retryable  bool
	HasRetry   bool
}

// commandCodeUsage mirrors totalUsage from the upstream finish event.
type commandCodeUsage struct {
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	NoCacheTokens     int64
	CacheReadTokens   int64
	CacheWriteTokens  int64
	HasCachedTokens   bool
}

// normalized applies the zero-output guard: a generation that produced no output
// tokens must not report a full prompt read.
func (u commandCodeUsage) normalized() commandCodeUsage {
	out := u
	if out.OutputTokens == 0 {
		out.InputTokens = 0
		out.CachedInputTokens = 0
		out.NoCacheTokens = 0
		out.CacheReadTokens = 0
		out.CacheWriteTokens = 0
	}
	return out
}

// openAIUsageJSON renders the OpenAI usage object carried by the terminal chunk.
func (u commandCodeUsage) openAIUsageJSON() []byte {
	out := u.normalized()
	usageJSON := []byte(`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`)
	usageJSON, _ = sjson.SetBytes(usageJSON, "prompt_tokens", out.InputTokens)
	usageJSON, _ = sjson.SetBytes(usageJSON, "completion_tokens", out.OutputTokens)
	usageJSON, _ = sjson.SetBytes(usageJSON, "total_tokens", out.InputTokens+out.OutputTokens)
	usageJSON, _ = sjson.SetBytes(usageJSON, "prompt_tokens_details.cached_tokens", out.CachedInputTokens)
	return usageJSON
}

// usageDetail renders the upstream counters as a usage record for monitoring.
func (u commandCodeUsage) usageDetail() usage.Detail {
	out := u.normalized()
	return usage.Detail{
		InputTokens:     out.InputTokens,
		OutputTokens:    out.OutputTokens,
		CachedTokens:    out.CachedInputTokens,
		CacheReadTokens: out.CacheReadTokens,
		TotalTokens:     out.InputTokens + out.OutputTokens,
	}
}

// CommandCodeStreamError is a classified upstream stream failure.
//
// Terminal plan/billing markers map to 402/403 so the auth lifecycle scopes the
// cooldown to the single model instead of taking the credential offline, and
// non-terminal upstream blips keep their reported status so the normal retry
// path stays available.
type CommandCodeStreamError struct {
	code      int
	message   string
	retryable bool
	terminal  bool
}

func (e *CommandCodeStreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.message != "" {
		return e.message
	}
	return fmt.Sprintf("status %d", e.code)
}

// StatusCode implements the status-carrying error contract used by the conductor.
func (e *CommandCodeStreamError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.code
}

// Retryable reports whether the same credential may be retried.
func (e *CommandCodeStreamError) Retryable() bool {
	return e != nil && e.retryable
}

// Terminal reports whether upstream signalled an unrecoverable plan/billing state.
func (e *CommandCodeStreamError) Terminal() bool {
	return e != nil && e.terminal
}

// CommandCodeTerminalMarker reports which terminal marker a message contains and
// the status the marker maps to.
func CommandCodeTerminalMarker(message string) (marker string, status int, ok bool) {
	lowered := strings.ToLower(message)
	for _, entry := range commandCodeTerminalStreamMarkers {
		if strings.Contains(lowered, entry.marker) {
			return entry.marker, entry.status, true
		}
	}
	return "", 0, false
}

// classifyCommandCodeErrorEvent classifies an upstream error event. It returns
// false when the event carries no error payload.
func classifyCommandCodeErrorEvent(event CommandCodeStreamEvent) (*CommandCodeStreamError, bool) {
	if event.Error == nil {
		return nil, false
	}
	message := event.Error.Message
	if message == "" {
		message = "upstream stream error"
	}
	if _, status, terminal := CommandCodeTerminalMarker(message); terminal {
		return &CommandCodeStreamError{code: status, message: message, retryable: false, terminal: true}, true
	}
	status := event.Error.StatusCode
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	return &CommandCodeStreamError{
		code:      NormalizeCommandCodeStatusError(status, []byte(message)),
		message:   message,
		retryable: !event.Error.HasRetry || event.Error.Retryable,
	}, true
}

// parseCommandCodeStreamEvent decodes one upstream line. Lines that carry no
// event (blank lines, SSE comments, the [DONE] sentinel, keepalives) report
// false.
func parseCommandCodeStreamEvent(line []byte) (CommandCodeStreamEvent, bool) {
	trimmed := commandCodeStripStreamPrefix(line)
	if len(trimmed) == 0 {
		return CommandCodeStreamEvent{}, false
	}
	if !json.Valid(trimmed) {
		return CommandCodeStreamEvent{}, false
	}
	node := gjson.ParseBytes(trimmed)
	eventType := strings.TrimSpace(node.Get("type").String())
	if eventType == "" {
		eventType = strings.TrimSpace(node.Get("event").String())
	}
	if eventType == "" {
		return CommandCodeStreamEvent{}, false
	}

	event := CommandCodeStreamEvent{Type: eventType}
	event.Text = commandCodeFirstString(node, "text", "delta", "textDelta")
	event.ToolCallID = commandCodeFirstString(node, "toolCallId", "tool_call_id")
	event.ToolName = commandCodeFirstString(node, "toolName", "tool_name")
	event.FinishReason = commandCodeFirstString(node, "finishReason", "finish_reason")
	if input := node.Get("input"); input.Exists() && input.Raw != "" && input.Raw != "null" {
		event.Input = []byte(input.Raw)
	}
	for _, path := range []string{"totalUsage", "usage"} {
		if node := node.Get(path); node.Exists() && node.IsObject() {
			event.Usage = parseCommandCodeUsage(node)
			break
		}
	}
	if eventType == commandCodeEventError {
		event.Error = parseCommandCodeErrorPayload(node)
	}
	return event, true
}

// commandCodeStripStreamPrefix removes an optional `data:` prefix and skips
// SSE framing lines that never carry JSON.
func commandCodeStripStreamPrefix(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	switch {
	case len(trimmed) == 0:
		return nil
	case bytes.Equal(trimmed, []byte("[DONE]")):
		return nil
	case trimmed[0] == ':':
		return nil
	case bytes.HasPrefix(trimmed, []byte("event:")),
		bytes.HasPrefix(trimmed, []byte("id:")),
		bytes.HasPrefix(trimmed, []byte("retry:")):
		return nil
	case bytes.HasPrefix(trimmed, []byte("data:")):
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
		if bytes.Equal(trimmed, []byte("[DONE]")) {
			return nil
		}
		return trimmed
	default:
		return trimmed
	}
}

func commandCodeFirstString(node gjson.Result, paths ...string) string {
	for _, path := range paths {
		value := node.Get(path)
		if value.Type == gjson.String && value.String() != "" {
			return value.String()
		}
	}
	return ""
}

// parseCommandCodeUsage decodes totalUsage/usage token counters.
func parseCommandCodeUsage(node gjson.Result) *commandCodeUsage {
	usageValue := &commandCodeUsage{
		InputTokens:       commandCodeTokenCount(node, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens"),
		OutputTokens:      commandCodeTokenCount(node, "outputTokens", "output_tokens", "completionTokens", "completion_tokens"),
		NoCacheTokens:     commandCodeTokenCount(node, "inputTokenDetails.noCacheTokens", "input_tokens_details.no_cache_tokens", "prompt_tokens_details.no_cache_tokens"),
		CacheReadTokens:   commandCodeTokenCount(node, "inputTokenDetails.cacheReadTokens", "input_tokens_details.cache_read_tokens", "prompt_tokens_details.cache_read_tokens"),
		CacheWriteTokens:  commandCodeTokenCount(node, "inputTokenDetails.cacheWriteTokens", "input_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_write_tokens"),
		HasCachedTokens:   false,
		CachedInputTokens: 0,
	}
	for _, path := range []string{"cachedInputTokens", "cached_input_tokens", "cachedTokens", "cached_tokens"} {
		if value := node.Get(path); value.Exists() && value.Type == gjson.Number {
			usageValue.CachedInputTokens = value.Int()
			usageValue.HasCachedTokens = true
			break
		}
	}
	if !usageValue.HasCachedTokens && usageValue.CacheReadTokens > 0 {
		usageValue.CachedInputTokens = usageValue.CacheReadTokens
		usageValue.HasCachedTokens = true
	}
	if !hasCommandCodeUsageCounters(usageValue) {
		return nil
	}
	return usageValue
}

func hasCommandCodeUsageCounters(value *commandCodeUsage) bool {
	if value == nil {
		return false
	}
	return value.InputTokens != 0 ||
		value.OutputTokens != 0 ||
		value.CachedInputTokens != 0 ||
		value.NoCacheTokens != 0 ||
		value.CacheReadTokens != 0 ||
		value.CacheWriteTokens != 0
}

func commandCodeTokenCount(node gjson.Result, paths ...string) int64 {
	for _, path := range paths {
		value := node.Get(path)
		if value.Exists() && value.Type == gjson.Number {
			return value.Int()
		}
	}
	return 0
}

// parseCommandCodeErrorPayload decodes the error member of an error event.
func parseCommandCodeErrorPayload(node gjson.Result) *CommandCodeStreamEventError {
	errorNode := node.Get("error")
	if !errorNode.Exists() || errorNode.Raw == "null" {
		message := commandCodeFirstString(node, "message", "errorMessage")
		if message == "" {
			return nil
		}
		return &CommandCodeStreamEventError{Message: message}
	}
	if errorNode.Type == gjson.String {
		return &CommandCodeStreamEventError{Message: errorNode.String()}
	}

	out := &CommandCodeStreamEventError{
		Message: commandCodeFirstString(errorNode, "message", "error", "msg"),
	}
	if out.Message == "" {
		out.Message = commandCodeFirstString(node, "message")
	}
	for _, path := range []string{"statusCode", "status_code", "status"} {
		if value := errorNode.Get(path); value.Exists() && value.Type == gjson.Number {
			out.StatusCode = int(value.Int())
			break
		}
	}
	for _, path := range []string{"isRetryable", "is_retryable", "retryable"} {
		value := errorNode.Get(path)
		if value.Type == gjson.True || value.Type == gjson.False {
			out.Retryable = value.Bool()
			out.HasRetry = true
			break
		}
	}
	if out.Message == "" && out.StatusCode == 0 {
		out.Message = strings.TrimSpace(errorNode.Raw)
	}
	return out
}

// commandCodeStreamTranslator converts the upstream JSONL event stream into
// OpenAI chat.completion.chunk SSE payloads and accumulates the same events so a
// non-streaming client can be served from one code path.
type CommandCodeStreamTranslator struct {
	model      string
	id         string
	created    int64
	roleSent   bool
	toolIndex  int
	toolCalls  bool
	finished   bool
	finish     string
	usage      *commandCodeUsage
	stepFinish string
	stepUsage  *commandCodeUsage
	content    strings.Builder
	reasoning  strings.Builder
	callOrder  []string
	callInputs map[string]string
	callNames  map[string]string
}

func NewCommandCodeStreamTranslator(model string) *CommandCodeStreamTranslator {
	return &CommandCodeStreamTranslator{
		model:      model,
		id:         "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		created:    time.Now().Unix(),
		callInputs: make(map[string]string),
		callNames:  make(map[string]string),
	}
}

// translate converts one upstream line into downstream OpenAI SSE payloads. It
// reports done once the terminal finish event has been consumed.
func (t *CommandCodeStreamTranslator) Translate(line []byte) (frames [][]byte, done bool, err error) {
	event, ok := parseCommandCodeStreamEvent(line)
	if !ok {
		return nil, false, nil
	}
	if t.finished {
		// The upstream stream is complete; late content is not replayed.
		return nil, true, nil
	}

	switch event.Type {
	case commandCodeEventTextDelta:
		if event.Text == "" {
			return nil, false, nil
		}
		t.content.WriteString(event.Text)
		delta := []byte(`{}`)
		delta = t.applyRole(delta)
		delta, _ = sjson.SetBytes(delta, "content", event.Text)
		return [][]byte{t.chunkWithDelta(delta)}, false, nil
	case commandCodeEventReasoningDelta:
		if event.Text == "" {
			return nil, false, nil
		}
		t.reasoning.WriteString(event.Text)
		delta := []byte(`{}`)
		delta = t.applyRole(delta)
		delta, _ = sjson.SetBytes(delta, "reasoning_content", event.Text)
		return [][]byte{t.chunkWithDelta(delta)}, false, nil
	case commandCodeEventToolCall:
		t.observeToolCall(event)
		delta := []byte(`{}`)
		delta = t.applyRole(delta)
		delta, _ = sjson.SetRawBytes(delta, "tool_calls", commandCodeToolCallDelta(t.toolIndex, event))
		t.toolIndex++
		return [][]byte{t.chunkWithDelta(delta)}, false, nil
	case commandCodeEventFinish:
		t.finished = true
		t.applyFinishEvent(event)
		return [][]byte{t.finishChunk(false)}, true, nil
	case commandCodeEventFinishStep:
		// finish-step carries no user-visible content; keep its usage and finish
		// reason only as a fallback for streams that end without a finish event.
		t.stepFinish = event.FinishReason
		if event.Usage != nil {
			t.stepUsage = event.Usage
		}
		return nil, false, nil
	case commandCodeEventError:
		classified, hasError := classifyCommandCodeErrorEvent(event)
		if !hasError {
			return nil, false, nil
		}
		t.finished = true
		return nil, true, classified
	default:
		// text-start/text-end/reasoning-start/reasoning-end/tool-input-*/
		// tool-result/tool-error/provider-metadata/start/start-step carry no
		// user-visible content.
		return nil, false, nil
	}
}

func (t *CommandCodeStreamTranslator) applyRole(delta []byte) []byte {
	if t.roleSent {
		return delta
	}
	t.roleSent = true
	updated, errSet := sjson.SetBytes(delta, "role", "assistant")
	if errSet != nil {
		return delta
	}
	return updated
}

// observeToolCall records a completed upstream tool call for the non-streaming
// response.
func (t *CommandCodeStreamTranslator) observeToolCall(event CommandCodeStreamEvent) {
	t.toolCalls = true
	id := strings.TrimSpace(event.ToolCallID)
	if id == "" {
		// Without a call id the call cannot be replayed in a non-streaming
		// response; the streaming frames still carry it.
		return
	}
	t.callOrder = append(t.callOrder, id)
	t.callNames[id] = event.ToolName
	t.callInputs[id] = string(commandCodeToolCallArguments(event.Input))
}

func (t *CommandCodeStreamTranslator) applyFinishEvent(event CommandCodeStreamEvent) {
	if strings.TrimSpace(event.FinishReason) != "" {
		t.finish = commandCodeFinishReason(event.FinishReason)
	}
	if event.Usage != nil {
		t.usage = event.Usage
	}
}

// finishReason resolves the terminal finish reason, falling back to the
// observed tool calls and then to "stop".
func (t *CommandCodeStreamTranslator) FinishReason() string {
	if t.finish != "" {
		return t.finish
	}
	if t.stepFinish != "" {
		return commandCodeFinishReason(t.stepFinish)
	}
	if t.toolCalls {
		return commandCodeFinishReasonToolCall
	}
	return commandCodeFinishReasonStopped
}

// resolvedUsage returns the terminal usage counters, preferring the finish event
// and falling back to the last finish-step.
func (t *CommandCodeStreamTranslator) ResolvedUsage() *commandCodeUsage {
	if t.usage != nil {
		return t.usage
	}
	return t.stepUsage
}

// finishChunk renders the terminal chunk carrying finish_reason and usage. The
// first argument reports whether the upstream stream ended without a finish
// event.
func (t *CommandCodeStreamTranslator) finishChunk(synthetic bool) []byte {
	chunk := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":null}]}`)
	chunk, _ = sjson.SetBytes(chunk, "id", t.id)
	chunk, _ = sjson.SetBytes(chunk, "created", t.created)
	chunk, _ = sjson.SetBytes(chunk, "model", t.model)
	chunk, _ = sjson.SetBytes(chunk, "choices.0.finish_reason", t.FinishReason())
	if usage := t.ResolvedUsage(); usage != nil {
		chunk, _ = sjson.SetRawBytes(chunk, "usage", usage.openAIUsageJSON())
	}
	if synthetic && !t.finished {
		t.finished = true
	}
	return chunk
}

// terminalFrames closes the downstream stream: a synthetic finish chunk when the
// upstream ended without one, followed by the OpenAI [DONE] sentinel.
func (t *CommandCodeStreamTranslator) TerminalFrames(includeFinish bool) [][]byte {
	frames := make([][]byte, 0, 2)
	if includeFinish {
		frames = append(frames, t.finishChunk(true))
	}
	frames = append(frames, []byte(commandCodeOpenAIDoneSentinel))
	return frames
}

// chunkWithDelta renders one OpenAI chat.completion.chunk around a delta object.
func (t *CommandCodeStreamTranslator) chunkWithDelta(delta []byte) []byte {
	chunk := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{},"finish_reason":null}]}`)
	chunk, _ = sjson.SetBytes(chunk, "id", t.id)
	chunk, _ = sjson.SetBytes(chunk, "created", t.created)
	chunk, _ = sjson.SetBytes(chunk, "model", t.model)
	chunk, _ = sjson.SetRawBytes(chunk, "choices.0.delta", delta)
	return chunk
}

// commandCodeToolCallDelta renders the OpenAI streaming tool_call delta for a
// complete upstream tool call.
func commandCodeToolCallDelta(index int, event CommandCodeStreamEvent) []byte {
	delta := []byte(`[{"index":0,"id":"","type":"function","function":{"name":"","arguments":"{}"}}]`)
	delta, _ = sjson.SetBytes(delta, "0.index", index)
	delta, _ = sjson.SetBytes(delta, "0.id", strings.TrimSpace(event.ToolCallID))
	delta, _ = sjson.SetBytes(delta, "0.function.name", strings.TrimSpace(event.ToolName))
	delta, _ = sjson.SetBytes(delta, "0.function.arguments", string(commandCodeToolCallArguments(event.Input)))
	return delta
}

// commandCodeToolCallArguments normalizes the upstream object-shaped tool input
// into the JSON string OpenAI clients expect.
func commandCodeToolCallArguments(input []byte) []byte {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return []byte("{}")
	}
	if trimmed[0] == '"' {
		// The upstream sometimes forwards already-encoded argument JSON.
		var decoded string
		if errUnmarshal := json.Unmarshal(trimmed, &decoded); errUnmarshal == nil && json.Valid([]byte(decoded)) {
			return []byte(decoded)
		}
	}
	return trimmed
}

// commandCodeFinishReason maps upstream finish reasons onto OpenAI values.
func commandCodeFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool-calls", "tool_calls", "toolcalls":
		return commandCodeFinishReasonToolCall
	case "length", "max_tokens", "max-tokens", "max_output_tokens", "maxoutputtokens", "max_tokens_reached":
		return commandCodeFinishReasonLength
	default:
		return commandCodeFinishReasonStopped
	}
}

// Finished reports whether the upstream stream reached a terminal event.
func (t *CommandCodeStreamTranslator) Finished() bool {
	return t != nil && t.finished
}

// UsageDetail renders the terminal usage counters for monitoring. It reports
// false when the upstream sent no usage at all.
func (t *CommandCodeStreamTranslator) UsageDetail() (usage.Detail, bool) {
	if t == nil {
		return usage.Detail{}, false
	}
	resolved := t.ResolvedUsage()
	if resolved == nil {
		return usage.Detail{}, false
	}
	return resolved.usageDetail(), true
}

// ChatCompletion assembles the accumulated stream into a non-streaming
// OpenAI chat.completion payload.
func (t *CommandCodeStreamTranslator) ChatCompletion() []byte {
	message := []byte(`{"role":"assistant","content":""}`)
	if t.reasoning.Len() > 0 {
		message, _ = sjson.SetBytes(message, "reasoning_content", t.reasoning.String())
	}
	message, _ = sjson.SetBytes(message, "content", t.content.String())

	toolCalls := t.AssembledToolCalls()
	if len(toolCalls) > 0 {
		message, _ = sjson.SetRawBytes(message, "tool_calls", toolCalls)
	}

	completion := []byte(`{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{},"finish_reason":null}],"usage":{}}`)
	completion, _ = sjson.SetBytes(completion, "id", t.id)
	completion, _ = sjson.SetBytes(completion, "created", t.created)
	completion, _ = sjson.SetBytes(completion, "model", t.model)
	completion, _ = sjson.SetRawBytes(completion, "choices.0.message", message)
	completion, _ = sjson.SetBytes(completion, "choices.0.finish_reason", t.FinishReason())
	if usage := t.ResolvedUsage(); usage != nil {
		completion, _ = sjson.SetRawBytes(completion, "usage", usage.openAIUsageJSON())
	} else {
		var errDelete error
		completion, errDelete = sjson.DeleteBytes(completion, "usage")
		if errDelete != nil {
			completion, _ = sjson.SetRawBytes(completion, "usage", []byte(`{}`))
		}
	}
	return completion
}

// assembledToolCalls renders the OpenAI non-streaming tool_calls array.
func (t *CommandCodeStreamTranslator) AssembledToolCalls() []byte {
	if len(t.callOrder) == 0 {
		return nil
	}
	out := make([]byte, 0, len(t.callOrder))
	for index, id := range t.callOrder {
		call := []byte(`{"id":"","type":"function","function":{"name":"","arguments":"{}"}}`)
		var errSet error
		call, errSet = sjson.SetBytes(call, "id", id)
		if errSet != nil {
			continue
		}
		call, errSet = sjson.SetBytes(call, "function.name", t.callNames[id])
		if errSet != nil {
			continue
		}
		arguments := t.callInputs[id]
		if !json.Valid([]byte(arguments)) {
			arguments = "{}"
		}
		call, errSet = sjson.SetBytes(call, "function.arguments", arguments)
		if errSet != nil {
			continue
		}
		if index > 0 {
			out = append(out, ',')
		}
		out = append(out, call...)
	}
	if len(out) == 0 {
		return nil
	}
	return []byte("[" + string(out) + "]")
}
