package excel

import (
	"encoding/json"
	"strings"
	"testing"
)

// feed pushes an upstream stream through the transform in chunks and returns
// the forwarded SSE payloads.
func feed(t *testing.T, transform *Transform, stream string, chunkSize int) []string {
	t.Helper()
	var out []string
	for start := 0; start < len(stream); start += chunkSize {
		end := start + chunkSize
		if end > len(stream) {
			end = len(stream)
		}
		for _, event := range transform.Write([]byte(stream[start:end])) {
			out = append(out, string(event))
		}
	}
	for _, event := range transform.Close() {
		out = append(out, string(event))
	}
	return out
}

// eventTypes lists the event names present in forwarded SSE output.
func eventTypes(events []string) []string {
	var types []string
	for _, event := range events {
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "event: ") {
				types = append(types, strings.TrimPrefix(line, "event: "))
			}
		}
	}
	return types
}

func containsType(types []string, want string) bool {
	for _, value := range types {
		if value == want {
			return true
		}
	}
	return false
}

// upstreamTextStream renders the event shapes the backend emits for a plain
// text answer.
func upstreamTextStream(text string, reasoning bool) string {
	var builder strings.Builder
	builder.WriteString("event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.6-sol","status":"in_progress"}}` + "\n\n")
	if reasoning {
		builder.WriteString("event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","content":[],"encrypted_content":"gAAAAsecret"}}` + "\n\n")
		builder.WriteString("event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","content":[],"encrypted_content":"gAAAAsecret"}}` + "\n\n")
	}
	index := 0
	if reasoning {
		index = 1
	}
	builder.WriteString("event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":` + itoa(index) + `,"item":{"id":"msg_1","type":"message","status":"in_progress","content":[],"role":"assistant"}}` + "\n\n")
	builder.WriteString("event: response.content_part.added\ndata: " + `{"type":"response.content_part.added","content_index":0,"item_id":"msg_1","output_index":` + itoa(index) + `,"part":{"type":"output_text","text":""}}` + "\n\n")
	for _, chunk := range chunkString(text, 7) {
		delta, _ := json.Marshal(map[string]any{
			"type":          "response.output_text.delta",
			"content_index": 0,
			"delta":         chunk,
			"item_id":       "msg_1",
			"output_index":  index,
		})
		builder.WriteString("event: response.output_text.delta\ndata: " + string(delta) + "\n\n")
	}
	builder.WriteString("event: response.output_text.done\ndata: " + `{"type":"response.output_text.done","content_index":0,"item_id":"msg_1","output_index":` + itoa(index) + `,"text":` + encodeString(text) + `}` + "\n\n")
	completed, _ := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp_1",
			"model":  "gpt-5.6-sol",
			"status": "completed",
			"output": []any{
				map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "status": "completed", "content": []any{
					map[string]any{"type": "output_text", "text": text},
				}},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12},
		},
	})
	builder.WriteString("event: response.completed\ndata: " + string(completed) + "\n\n")
	return builder.String()
}

func TestTransformDropsReasoningItems(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})
	transform := NewTransform(catalog, "gpt-5.6-sol-excel")
	events := feed(t, transform, upstreamTextStream("All done.", true), 13)

	if strings.Contains(strings.Join(events, ""), "gpt-5.6-sol\"") {
		t.Fatal("the upstream model slug must not be exposed to the client")
	}
	if strings.Contains(strings.Join(events, ""), "encrypted_content") {
		t.Fatal("reasoning items must not be forwarded: their encrypted payload cannot be replayed to this backend")
	}
	if !strings.Contains(strings.Join(events, ""), "All done.") {
		t.Fatalf("assistant text was lost: %v", eventTypes(events))
	}
	if terminal := transform.Terminal(); terminal == nil || terminal["model"] != "gpt-5.6-sol-excel" {
		t.Fatalf("terminal payload must expose the client alias: %v", terminal)
	}
}

func TestTransformRelaysToolCallWithoutLeakingMarker(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})
	transform := NewTransform(catalog, "gpt-5.6-sol-excel")
	marker := `<codex_tool_call>{"name":"exec_command","arguments":{"cmd":"pwd"}}</codex_tool_call>`
	events := feed(t, transform, upstreamTextStream(marker, true), 5)

	joined := strings.Join(events, "")
	if strings.Contains(joined, MarkerOpen) || strings.Contains(joined, MarkerClose) {
		t.Fatalf("the relay marker leaked to the client:\n%s", joined)
	}
	types := eventTypes(events)
	for _, want := range []string{
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	} {
		if !containsType(types, want) {
			t.Fatalf("missing %q in %v", want, types)
		}
	}

	call := findFunctionCall(t, events)
	if call["name"] != "exec_command" {
		t.Fatalf("wrong tool name: %v", call["name"])
	}
	if call["call_id"] == "" || call["call_id"] == nil {
		t.Fatal("the synthesized call must carry a call_id so the client can return a result")
	}
	if !strings.Contains(call["arguments"].(string), "pwd") {
		t.Fatalf("arguments not relayed: %v", call["arguments"])
	}
}

func TestTransformRelaysCustomToolCall(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "custom", "name": "apply_patch"}})
	transform := NewTransform(catalog, "gpt-5.6-sol-excel")
	patch := `*** Begin Patch\n*** End Patch`
	marker := `<codex_tool_call>` + mustJSON(map[string]any{"name": "apply_patch", "input": patch}) + `</codex_tool_call>`
	events := feed(t, transform, upstreamTextStream(marker, false), 9)

	if !containsType(eventTypes(events), "response.custom_tool_call_input.done") {
		t.Fatalf("custom tool must use the custom_tool_call event family: %v", eventTypes(events))
	}
	call := findFunctionCall(t, events)
	if call["type"] != "custom_tool_call" {
		t.Fatalf("wrong item type: %v", call["type"])
	}
	if !strings.Contains(call["input"].(string), "Begin Patch") {
		t.Fatalf("custom input not relayed: %v", call["input"])
	}
}

func TestTransformKeepsPlainAnswerIntact(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})
	transform := NewTransform(catalog, "gpt-5.6-sol-excel")
	// Text that begins like a marker but is not one must still reach the client.
	answer := "The <codex_tool is not a real marker here."
	events := feed(t, transform, upstreamTextStream(answer, false), 4)

	joined := strings.Join(events, "")
	if !strings.Contains(joined, "<codex_tool is not a real marker") {
		t.Fatalf("incomplete marker lookalike was swallowed:\n%s", joined)
	}
}

func TestTransformWithNoToolsStreamsTextUnchanged(t *testing.T) {
	transform := NewTransform(&Catalog{}, "gpt-5.6-sol-excel")
	events := feed(t, transform, upstreamTextStream("Plain answer.", false), 11)
	if !strings.Contains(strings.Join(events, ""), "Plain answer.") {
		t.Fatal("text was lost when the request declared no tools")
	}
}

func TestTransformDropsNativeToolCalls(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})
	transform := NewTransform(catalog, "gpt-5.6-sol-excel")

	var builder strings.Builder
	builder.WriteString("event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_2","model":"gpt-5.6-sol","status":"in_progress"}}` + "\n\n")
	builder.WriteString("event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"write_range","call_id":"call_1","arguments":""}}` + "\n\n")
	builder.WriteString("event: response.function_call_arguments.delta\ndata: " + `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"sheetId\":\"\"}"}` + "\n\n")
	builder.WriteString("event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"write_range","call_id":"call_1","arguments":"{}","status":"completed"}}` + "\n\n")
	builder.WriteString("event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_2","model":"gpt-5.6-sol","status":"completed","output":[{"id":"fc_1","type":"function_call","name":"write_range","call_id":"call_1","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n")

	events := feed(t, transform, builder.String(), 7)
	joined := strings.Join(events, "")
	if strings.Contains(joined, "write_range") {
		t.Fatalf("a native workbook tool call reached the client:\n%s", joined)
	}
	if terminal := transform.Terminal(); terminal != nil {
		output, _ := terminal["output"].([]any)
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item["name"] == "write_range" {
				t.Fatal("the terminal payload still advertises a native workbook tool")
			}
		}
	}
}

func findFunctionCall(t *testing.T, events []string) map[string]any {
	t.Helper()
	for _, event := range events {
		for _, line := range strings.Split(event, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var payload map[string]any
			if errUnmarshal := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); errUnmarshal != nil {
				continue
			}
			if payload["type"] != "response.output_item.done" {
				continue
			}
			item, _ := payload["item"].(map[string]any)
			if item == nil {
				continue
			}
			if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
				return item
			}
		}
	}
	t.Fatalf("no tool call was synthesized: %v", eventTypes(events))
	return nil
}

func chunkString(text string, size int) []string {
	var out []string
	for start := 0; start < len(text); start += size {
		end := start + size
		if end > len(text) {
			end = len(text)
		}
		out = append(out, text[start:end])
	}
	return out
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func encodeString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestTransformStripsInjectedPromptAndNativeTools(t *testing.T) {
	transform := NewTransform(&Catalog{}, "gpt-5.6-sol-excel")

	created := `{"type":"response.created","response":{"id":"resp_9","model":"gpt-5.6-sol","status":"in_progress",` +
		`"instructions":"You are the spreadsheet agent for the ChatGPT Excel Plugin.","tools":[{"type":"function","name":"read_ranges"}],"tool_choice":"auto"}}`
	stream := "event: response.created\ndata: " + created + "\n\n"
	events := feed(t, transform, stream, 17)

	joined := strings.Join(events, "")
	if strings.Contains(joined, "spreadsheet agent") {
		t.Fatal("the backend's injected system prompt must not be forwarded to the client")
	}
	if strings.Contains(joined, "read_ranges") {
		t.Fatal("the backend's native tool list must not be forwarded to the client")
	}
	if !strings.Contains(joined, "gpt-5.6-sol-excel") {
		t.Fatal("the client alias must be reported instead of the upstream slug")
	}
}

func TestTransformDoesNotHTMLEscapeText(t *testing.T) {
	transform := NewTransform(&Catalog{}, "gpt-5.6-sol-excel")
	events := feed(t, transform, upstreamTextStream("a<b>&c", false), 3)
	joined := strings.Join(events, "")
	if strings.Contains(joined, `\u003c`) || strings.Contains(joined, `\u0026`) {
		t.Fatalf("text was HTML-escaped, breaking fidelity:\n%s", joined)
	}
	if !strings.Contains(joined, "a<b>&c") {
		t.Fatalf("text did not survive the transform:\n%s", joined)
	}
}

func TestTransformNeverLeaksUnrelayableMarker(t *testing.T) {
	catalog := ParseCatalog([]any{
		map[string]any{"type": "function", "name": "exec_command"},
		map[string]any{"type": "function", "name": "apply_patch"},
	})

	// The model emitted a marker but named a tool the client never declared, so
	// it cannot be relayed. The protocol text must still not reach the client.
	streams := map[string]string{
		"unknown tool":      `<codex_tool_call>{"name":"run_officejs","arguments":{"code":"x"}}</codex_tool_call>`,
		"malformed payload": `<codex_tool_call>{"name":"exec_command","arguments":{ oops</codex_tool_call>`,
	}
	for name, text := range streams {
		t.Run(name, func(t *testing.T) {
			transform := NewTransform(catalog, "gpt-5.6-sol-excel")
			events := feed(t, transform, upstreamTextStream(text, false), 6)
			joined := strings.Join(events, "")
			if strings.Contains(joined, MarkerOpen) || strings.Contains(joined, MarkerClose) {
				t.Fatalf("relay marker leaked to the client:\n%s", joined)
			}
		})
	}
}

func TestTransformTerminalEventEnvelope(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})

	// Every terminal event must be {"type":...,"response":{...}}. Emitting the
	// bare response object produces an event a Responses client cannot match, so
	// it silently loses the end of the turn.
	cases := map[string]string{
		"plain answer": upstreamTextStream("All done.", true),
		"relayed call": upstreamTextStream(`<codex_tool_call>{"name":"exec_command","arguments":{"cmd":"pwd"}}</codex_tool_call>`, true),
		"unrelayable":  upstreamTextStream(`<codex_tool_call>{"name":"nope","arguments":{}}</codex_tool_call>`, false),
	}

	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			transform := NewTransform(catalog, "gpt-5.6-sol-excel")
			events := feed(t, transform, stream, 8)

			found := false
			for _, event := range events {
				for _, line := range strings.Split(event, "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var payload map[string]any
					if errUnmarshal := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); errUnmarshal != nil {
						continue
					}
					if payload["type"] != "response.completed" {
						continue
					}
					found = true
					response, okResponse := payload["response"].(map[string]any)
					if !okResponse {
						t.Fatalf("terminal event has no response envelope: %s", line)
					}
					if response["model"] != "gpt-5.6-sol-excel" {
						t.Fatalf("terminal response model = %v, want the client alias", response["model"])
					}
					if response["status"] != "completed" {
						t.Fatalf("terminal response status = %v, want completed", response["status"])
					}
					if _, leaked := response["instructions"]; leaked {
						t.Fatal("terminal response leaks the injected system prompt")
					}
					if _, leaked := response["tools"]; leaked {
						t.Fatal("terminal response leaks the native tool list")
					}
				}
			}
			if !found {
				t.Fatalf("no terminal response.completed event was emitted: %v", eventTypes(events))
			}
		})
	}
}

func TestTransformEmptyTurnIsRejected(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})
	transform := NewTransform(catalog, "gpt-5.6-sol-excel")

	// The model reached for a backend tool whose call this route discards, so the
	// turn carries no text and no relayed call. It must fail rather than claim
	// the agent completed its work successfully.
	var builder strings.Builder
	builder.WriteString("event: response.created\ndata: " + `{"type":"response.created","response":{"id":"resp_3","model":"gpt-5.6-sol","status":"in_progress"}}` + "\n\n")
	builder.WriteString("event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_9","type":"function_call","name":"request_user_input_basispoints","call_id":"call_9","arguments":""}}` + "\n\n")
	builder.WriteString("event: response.function_call_arguments.done\ndata: " + `{"type":"response.function_call_arguments.done","item_id":"fc_9","output_index":0,"arguments":"{\"question\":\"which sheet?\"}"}` + "\n\n")
	builder.WriteString("event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_9","type":"function_call","name":"request_user_input_basispoints","call_id":"call_9","arguments":"{}","status":"completed"}}` + "\n\n")
	builder.WriteString("event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_3","model":"gpt-5.6-sol","status":"completed","output":[{"id":"fc_9","type":"function_call","name":"request_user_input_basispoints","call_id":"call_9","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":2}}}` + "\n\n")

	events := feed(t, transform, builder.String(), 9)
	joined := strings.Join(events, "")

	if strings.Contains(joined, "request_user_input_basispoints") || strings.Contains(joined, "which sheet") {
		t.Fatalf("a discarded backend tool call reached the client:\n%s", joined)
	}
	if !strings.Contains(joined, "excel_no_usable_output") {
		t.Fatalf("an empty turn must explain itself, got events: %v", eventTypes(events))
	}
	if terminal := transform.Terminal(); terminal == nil || terminal["status"] != "failed" {
		t.Fatalf("empty terminal must fail: %v", terminal)
	}
}

func TestTransformNormalAnswerIsNotGivenTheEmptyNotice(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})
	transform := NewTransform(catalog, "gpt-5.6-sol-excel")
	events := feed(t, transform, upstreamTextStream("Here is the answer.", false), 6)
	if strings.Contains(strings.Join(events, ""), "did not return an answer") {
		t.Fatal("a normal answer must not be replaced by the empty-turn notice")
	}
}

func TestTransformFlushesTerminalWithoutBlankLine(t *testing.T) {
	transform := NewTransform(&Catalog{}, "gpt-5.6-sol-excel")
	stream := strings.TrimRight(upstreamTextStream("complete answer", false), "\n")
	events := feed(t, transform, stream, 7)
	if transform.Terminal() == nil || !containsType(eventTypes(events), "response.completed") {
		t.Fatalf("lost final SSE event at EOF: %v", eventTypes(events))
	}
}

func TestTransformTerminalOnlyPreservesAnswer(t *testing.T) {
	transform := NewTransform(&Catalog{}, "gpt-5.6-sol-excel")
	stream := "data: " + mustJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "r1", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "actual answer"}}}}}}) + "\n\n"
	events := feed(t, transform, stream, 11)
	if got := responseText(transform.Terminal()); got != "actual answer" {
		t.Fatalf("terminal-only answer replaced: %q; events=%v", got, events)
	}
	if !strings.Contains(strings.Join(events, ""), `"delta":"actual answer"`) {
		t.Fatal("terminal-only answer not emitted to streaming clients")
	}
}

func TestTransformNamespacedCallPreservesNamespace(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "namespace", "name": "browser", "tools": []any{map[string]any{"type": "function", "name": "open"}}}})
	transform := NewTransform(catalog, "gpt-5.6-sol-excel")
	events := feed(t, transform, upstreamTextStream(`<codex_tool_call>{"name":"browser.open","arguments":{"url":"test"}}</codex_tool_call>`, false), 7)
	call := findFunctionCall(t, events)
	if call["namespace"] != "browser" || call["name"] != "open" {
		t.Fatalf("lost namespace: %v", call)
	}
}

func TestTransformEmptyCompletionIsFailureNotSuccess(t *testing.T) {
	transform := NewTransform(&Catalog{}, "gpt-5.6-sol-excel")
	events := feed(t, transform, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"empty\",\"status\":\"completed\",\"output\":[]}}\n\n", 7)
	if containsType(eventTypes(events), "response.completed") || !containsType(eventTypes(events), "response.failed") {
		t.Fatalf("empty output must not silently end the client's agent loop successfully: %v", eventTypes(events))
	}
	if transform.Terminal()["status"] != "failed" {
		t.Fatal(transform.Terminal())
	}
}

func TestTransformTerminalOnlyHasCompleteMessageLifecycle(t *testing.T) {
	transform := NewTransform(&Catalog{}, "gpt-5.6-sol-excel")
	terminal := map[string]any{"type": "response.completed", "response": map[string]any{"id": "r1", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "terminal answer"}}}}}}
	events := feed(t, transform, "data: "+mustJSON(terminal)+"\n\n", 8)
	for _, want := range []string{"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"} {
		if !containsType(eventTypes(events), want) {
			t.Errorf("missing %s: %v", want, eventTypes(events))
		}
	}
}
