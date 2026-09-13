package helps

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// commandCodeTranslateLines feeds upstream JSONL lines through the translator and
// returns every downstream OpenAI SSE payload together with the final state.
func commandCodeTranslateLines(t *testing.T, model string, lines ...string) (*CommandCodeStreamTranslator, []string) {
	t.Helper()
	translator := NewCommandCodeStreamTranslator(model)
	frames := make([]string, 0, len(lines))
	for _, line := range lines {
		produced, done, errTranslate := translator.Translate([]byte(line))
		if errTranslate != nil {
			t.Fatalf("Translate(%s) returned an error: %v", line, errTranslate)
		}
		for _, frame := range produced {
			frames = append(frames, string(frame))
		}
		if done {
			break
		}
	}
	return translator, frames
}

func TestCommandCodeStreamTextDeltasBecomeOpenAIChunks(t *testing.T) {
	_, frames := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"start"}`,
		`{"type":"start-step"}`,
		`{"type":"text-start","id":"t1"}`,
		`{"type":"text-delta","id":"t1","text":"Hel"}`,
		`{"type":"text-delta","id":"t1","text":"lo"}`,
		`{"type":"text-end","id":"t1"}`,
		`{"type":"finish-step","finishReason":"stop"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":2}}`,
	)

	var content strings.Builder
	for _, frame := range frames {
		if got := gjson.Get(frame, "object").String(); got != "chat.completion.chunk" {
			t.Fatalf("frame object = %q, want chat.completion.chunk: %s", got, frame)
		}
		content.WriteString(gjson.Get(frame, "choices.0.delta.content").String())
	}
	if got := content.String(); got != "Hello" {
		t.Fatalf("assembled content = %q, want Hello", got)
	}
	if !strings.Contains(strings.Join(frames, "\n"), `"finish_reason":"stop"`) {
		t.Fatalf("no terminal frame carried finish_reason=stop: %v", frames)
	}
}

func TestCommandCodeStreamReasoningDeltasUseReasoningContent(t *testing.T) {
	// reasoning_content is the OpenAI-side field the Claude translator maps back
	// into thinking blocks; a different field name silently breaks thinking-mode
	// tool loops.
	_, frames := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"reasoning-start"}`,
		`{"type":"reasoning-delta","text":"why "}`,
		`{"type":"reasoning-delta","text":"not"}`,
		`{"type":"reasoning-end"}`,
		`{"type":"text-delta","text":"answer"}`,
		`{"type":"finish","finishReason":"stop"}`,
	)

	var reasoning strings.Builder
	for _, frame := range frames {
		reasoning.WriteString(gjson.Get(frame, "choices.0.delta.reasoning_content").String())
	}
	if got := reasoning.String(); got != "why not" {
		t.Fatalf("assembled reasoning_content = %q, want %q", got, "why not")
	}
}

func TestCommandCodeStreamToolCallEmitsCompleteCall(t *testing.T) {
	_, frames := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"tool-input-start","toolCallId":"call_1"}`,
		`{"type":"tool-input-delta","toolCallId":"call_1","delta":"{"}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"q":"x"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":5,"outputTokens":3}}`,
	)

	var toolFrame string
	for _, frame := range frames {
		if gjson.Get(frame, "choices.0.delta.tool_calls").Exists() {
			toolFrame = frame
		}
	}
	if toolFrame == "" {
		t.Fatalf("no frame carried tool_calls: %v", frames)
	}
	if got := gjson.Get(toolFrame, "choices.0.delta.tool_calls.0.id").String(); got != "call_1" {
		t.Errorf("tool call id = %q", got)
	}
	if got := gjson.Get(toolFrame, "choices.0.delta.tool_calls.0.type").String(); got != "function" {
		t.Errorf("tool call type = %q, want function", got)
	}
	if got := gjson.Get(toolFrame, "choices.0.delta.tool_calls.0.function.name").String(); got != "lookup" {
		t.Errorf("tool call name = %q", got)
	}
	// The upstream object input is serialized as the JSON string OpenAI clients expect.
	if got := gjson.Get(toolFrame, "choices.0.delta.tool_calls.0.function.arguments").String(); got != `{"q":"x"}` {
		t.Errorf("tool call arguments = %q, want the serialized object", got)
	}
	if got := gjson.Get(toolFrame, "choices.0.delta.tool_calls.0.index").Int(); got != 0 {
		t.Errorf("tool call index = %d, want 0", got)
	}

	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish reason should map tool-calls to tool_calls: %v", frames)
	}
}

func TestCommandCodeStreamIgnoresContentlessEvents(t *testing.T) {
	_, frames := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"start"}`,
		`{"type":"start-step"}`,
		`{"type":"text-start"}`,
		`{"type":"text-end"}`,
		`{"type":"reasoning-start"}`,
		`{"type":"reasoning-end"}`,
		`{"type":"tool-input-start"}`,
		`{"type":"tool-input-delta","delta":"x"}`,
		`{"type":"tool-input-end"}`,
		`{"type":"tool-result"}`,
		`{"type":"tool-error"}`,
		`{"type":"provider-metadata","foo":"bar"}`,
		`: keepalive`,
		``,
	)
	if len(frames) != 0 {
		t.Fatalf("contentless events produced %d frames, want none: %v", len(frames), frames)
	}
}

func TestCommandCodeStreamAcceptsDataPrefix(t *testing.T) {
	_, frames := commandCodeTranslateLines(t, "cc-flash",
		`data: {"type":"text-delta","text":"hi"}`,
		`data: {"type":"finish","finishReason":"stop"}`,
	)
	if len(frames) == 0 {
		t.Fatal("data:-prefixed events produced no frames")
	}
	if got := gjson.Get(frames[0], "choices.0.delta.content").String(); got != "hi" {
		t.Fatalf("content = %q, want hi", got)
	}
}

func TestCommandCodeStreamRoleSentOnce(t *testing.T) {
	_, frames := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"text-delta","text":"a"}`,
		`{"type":"text-delta","text":"b"}`,
	)
	if len(frames) < 2 {
		t.Fatalf("expected two chunk frames, got %v", frames)
	}
	if got := gjson.Get(frames[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("first delta role = %q, want assistant", got)
	}
	if gjson.Get(frames[1], "choices.0.delta.role").Exists() {
		t.Fatalf("role must only be sent on the first delta: %s", frames[1])
	}
}

func TestCommandCodeStreamFinishReasons(t *testing.T) {
	tests := []struct {
		upstream string
		want     string
	}{
		{upstream: "tool-calls", want: "tool_calls"},
		{upstream: "tool_calls", want: "tool_calls"},
		{upstream: "length", want: "length"},
		{upstream: "max_tokens", want: "length"},
		{upstream: "max_output_tokens", want: "length"},
		{upstream: "stop", want: "stop"},
		{upstream: "end_turn", want: "stop"},
	}
	for _, test := range tests {
		t.Run(test.upstream, func(t *testing.T) {
			_, frames := commandCodeTranslateLines(t, "cc-flash",
				`{"type":"text-delta","text":"x"}`,
				`{"type":"finish","finishReason":"`+test.upstream+`"}`,
			)
			joined := strings.Join(frames, "\n")
			if !strings.Contains(joined, `"finish_reason":"`+test.want+`"`) {
				t.Errorf("finishReason %q produced %v, want %q", test.upstream, frames, test.want)
			}
		})
	}
}

func TestCommandCodeStreamUsageZeroOutputGuard(t *testing.T) {
	// A generation that produced no output tokens must not report a full prompt
	// read; otherwise a failed turn is billed as a complete cache miss.
	translator := NewCommandCodeStreamTranslator("cc-flash")
	produced, _, errTranslate := translator.Translate([]byte(
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":7653,"outputTokens":0,"cachedInputTokens":100}}`))
	if errTranslate != nil {
		t.Fatalf("translate finish: %v", errTranslate)
	}
	if len(produced) != 1 {
		t.Fatalf("finish produced %d frames, want 1", len(produced))
	}
	frame := produced[0]
	if got := gjson.GetBytes(frame, "usage.prompt_tokens").Int(); got != 0 {
		t.Errorf("usage.prompt_tokens = %d, want 0 under the zero-output guard", got)
	}
	if got := gjson.GetBytes(frame, "usage.prompt_tokens_details.cached_tokens").Int(); got != 0 {
		t.Errorf("cached_tokens = %d, want 0 under the zero-output guard", got)
	}
	if got := gjson.GetBytes(frame, "usage.completion_tokens").Int(); got != 0 {
		t.Errorf("completion_tokens = %d, want 0", got)
	}
}

func TestCommandCodeStreamUsagePassedThroughWhenOutputPresent(t *testing.T) {
	translator := NewCommandCodeStreamTranslator("cc-flash")
	produced, _, errTranslate := translator.Translate([]byte(
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":100,"outputTokens":20,"cachedInputTokens":40}}`))
	if errTranslate != nil {
		t.Fatalf("translate finish: %v", errTranslate)
	}
	if len(produced) != 1 {
		t.Fatalf("finish produced %d frames, want 1", len(produced))
	}
	if got := gjson.GetBytes(produced[0], "usage.prompt_tokens").Int(); got != 100 {
		t.Errorf("prompt_tokens = %d, want 100", got)
	}
	if got := gjson.GetBytes(produced[0], "usage.completion_tokens").Int(); got != 20 {
		t.Errorf("completion_tokens = %d, want 20", got)
	}
	if got := gjson.GetBytes(produced[0], "usage.total_tokens").Int(); got != 120 {
		t.Errorf("total_tokens = %d, want 120", got)
	}
	if got := gjson.GetBytes(produced[0], "usage.prompt_tokens_details.cached_tokens").Int(); got != 40 {
		t.Errorf("cached_tokens = %d, want 40", got)
	}

	detail, ok := translator.UsageDetail()
	if !ok {
		t.Fatal("UsageDetail reported no usage")
	}
	if detail.InputTokens != 100 || detail.OutputTokens != 20 || detail.TotalTokens != 120 {
		t.Errorf("usage detail = %+v", detail)
	}
}

func TestCommandCodeStreamTerminalMarkersAreHardErrors(t *testing.T) {
	tests := []struct {
		name     string
		marker   string
		wantCode int
	}{
		{name: "premium credits", marker: "premium_credits_exhausted", wantCode: http.StatusForbidden},
		{name: "model not in plan", marker: "model_not_in_plan", wantCode: http.StatusForbidden},
		{name: "insufficient credits", marker: "insufficient credits", wantCode: http.StatusPaymentRequired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			translator := NewCommandCodeStreamTranslator("cc-flash")
			_, done, errTranslate := translator.Translate([]byte(
				`{"type":"error","error":{"message":"` + test.marker + `"}}`))
			if errTranslate == nil {
				t.Fatal("terminal marker did not produce an error")
			}
			if !done {
				t.Error("terminal marker should end the stream")
			}
			streamErr, ok := errTranslate.(*CommandCodeStreamError)
			if !ok {
				t.Fatalf("error type = %T, want *CommandCodeStreamError", errTranslate)
			}
			if !streamErr.Terminal() {
				t.Error("marker should be classified as terminal")
			}
			if streamErr.Retryable() {
				t.Error("terminal markers must not be retryable")
			}
			if got := streamErr.StatusCode(); got != test.wantCode {
				t.Errorf("StatusCode() = %d, want %d so only the model cools down", got, test.wantCode)
			}
			if !strings.Contains(streamErr.Error(), test.marker) {
				t.Errorf("error message %q should carry the marker", streamErr.Error())
			}
		})
	}
}

func TestCommandCodeStreamRetryableErrorEvent(t *testing.T) {
	translator := NewCommandCodeStreamTranslator("cc-flash")
	_, done, errTranslate := translator.Translate([]byte(
		`{"type":"error","error":{"message":"upstream blip","statusCode":503,"isRetryable":true}}`))
	if errTranslate == nil || !done {
		t.Fatalf("error event should end the stream with an error (done=%v err=%v)", done, errTranslate)
	}
	streamErr, ok := errTranslate.(*CommandCodeStreamError)
	if !ok {
		t.Fatalf("error type = %T", errTranslate)
	}
	if streamErr.Terminal() {
		t.Error("a transient blip must not be terminal")
	}
	if !streamErr.Retryable() {
		t.Error("isRetryable:true should stay retryable")
	}
	if got := streamErr.StatusCode(); got != http.StatusServiceUnavailable {
		t.Errorf("StatusCode() = %d, want the reported 503", got)
	}
}

func TestCommandCodeStreamFinishWithoutUsage(t *testing.T) {
	translator, frames := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"text-delta","text":"x"}`,
		`{"type":"finish","finishReason":"stop"}`,
	)
	if _, ok := translator.UsageDetail(); ok {
		t.Error("usage should not be reported when upstream sent none")
	}
	joined := strings.Join(frames, "\n")
	if strings.Contains(joined, `"usage"`) {
		t.Errorf("finish frame should omit usage when upstream sent none: %s", joined)
	}
	if !translator.Finished() {
		t.Error("translator should report finished after a finish event")
	}
}

func TestCommandCodeStreamTerminalFrames(t *testing.T) {
	translator, _ := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"text-delta","text":"x"}`,
	)
	// A stream that ended without a finish event still needs an explicit finish
	// frame plus the [DONE] sentinel for non-OpenAI clients.
	frames := translator.TerminalFrames(!translator.Finished())
	if len(frames) != 2 {
		t.Fatalf("terminal frames = %d, want finish + [DONE]: %v", len(frames), frames)
	}
	if got := gjson.GetBytes(frames[0], "choices.0.finish_reason").String(); got != "stop" {
		t.Errorf("synthetic finish_reason = %q, want stop", got)
	}
	if got := string(frames[1]); got != "data: [DONE]" {
		t.Errorf("terminal sentinel = %q, want data: [DONE]", got)
	}

	// When the upstream already sent a finish event, only [DONE] is appended.
	finished := NewCommandCodeStreamTranslator("cc-flash")
	if _, _, errTranslate := finished.Translate([]byte(`{"type":"finish","finishReason":"stop"}`)); errTranslate != nil {
		t.Fatalf("translate finish: %v", errTranslate)
	}
	frames = finished.TerminalFrames(!finished.Finished())
	if len(frames) != 1 || string(frames[0]) != "data: [DONE]" {
		t.Fatalf("terminal frames = %v, want only the sentinel", frames)
	}
}

func TestCommandCodeStreamChatCompletionAssembly(t *testing.T) {
	translator, _ := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"reasoning-delta","text":"why"}`,
		`{"type":"text-delta","text":"answer"}`,
		`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"q":"x"}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":7,"outputTokens":3}}`,
	)
	completion := translator.ChatCompletion()

	if got := gjson.GetBytes(completion, "object").String(); got != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", got)
	}
	if got := gjson.GetBytes(completion, "model").String(); got != "cc-flash" {
		t.Errorf("model = %q", got)
	}
	if got := gjson.GetBytes(completion, "choices.0.message.role").String(); got != "assistant" {
		t.Errorf("message role = %q", got)
	}
	if got := gjson.GetBytes(completion, "choices.0.message.content").String(); got != "answer" {
		t.Errorf("message content = %q", got)
	}
	if got := gjson.GetBytes(completion, "choices.0.message.reasoning_content").String(); got != "why" {
		t.Errorf("message reasoning_content = %q", got)
	}
	if got := gjson.GetBytes(completion, "choices.0.message.tool_calls.0.id").String(); got != "call_1" {
		t.Errorf("tool call id = %q", got)
	}
	if got := gjson.GetBytes(completion, "choices.0.message.tool_calls.0.function.arguments").String(); got != `{"q":"x"}` {
		t.Errorf("tool call arguments = %q", got)
	}
	if got := gjson.GetBytes(completion, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Errorf("finish_reason = %q", got)
	}
	if got := gjson.GetBytes(completion, "usage.completion_tokens").Int(); got != 3 {
		t.Errorf("usage.completion_tokens = %d", got)
	}
}

func TestCommandCodeStreamChatCompletionWithoutUsageOmitsUsage(t *testing.T) {
	translator, _ := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"text-delta","text":"answer"}`,
		`{"type":"finish","finishReason":"stop"}`,
	)
	completion := translator.ChatCompletion()
	if gjson.GetBytes(completion, "usage").Exists() {
		t.Fatalf("usage should be omitted when upstream sent none: %s", completion)
	}
}

func TestCommandCodeStreamIgnoresTrafficAfterFinish(t *testing.T) {
	translator, frames := commandCodeTranslateLines(t, "cc-flash",
		`{"type":"text-delta","text":"a"}`,
		`{"type":"finish","finishReason":"stop"}`,
		`{"type":"text-delta","text":"late"}`,
	)
	if len(frames) != 2 {
		t.Fatalf("frames = %v, want one content chunk plus the finish chunk", frames)
	}
	if strings.Contains(strings.Join(frames, "\n"), "late") {
		t.Error("content after the finish event must not be replayed")
	}
	if got := translator.ChatCompletion(); strings.Contains(string(got), "late") {
		t.Errorf("late content leaked into the assembled response: %s", got)
	}
}

func TestCommandCodeStreamSkipsGarbageLines(t *testing.T) {
	translator := NewCommandCodeStreamTranslator("cc-flash")
	for _, line := range []string{"not json", `{"no":"type"}`, "[DONE]", "data: [DONE]", ": comment", ""} {
		produced, done, errTranslate := translator.Translate([]byte(line))
		if errTranslate != nil {
			t.Fatalf("Translate(%q) errored: %v", line, errTranslate)
		}
		if done || len(produced) != 0 {
			t.Fatalf("Translate(%q) produced frames=%v done=%v, want neither", line, produced, done)
		}
	}
}
