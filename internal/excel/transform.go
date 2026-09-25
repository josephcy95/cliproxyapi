package excel

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Transform rewrites the backend's SSE stream into the Responses events the
// downstream client expects.
//
// Three things happen here:
//
//  1. Reasoning items are dropped. Their encrypted payload is signed for the
//     public Codex backend and cannot be replayed to this one, so exposing them
//     would only produce an unverifiable item on the next turn.
//  2. The model-reported name is mapped back to the caller's alias.
//  3. The marker relay is intercepted: text is streamed normally until a marker
//     open tag appears, at which point the remaining text and the raw terminal
//     event are held back. At completion the marker is parsed and replaced with
//     a real function_call / custom_tool_call item, so the client never sees the
//     relay protocol.
//
// The transform is incremental: each Write may be called with arbitrary byte
// boundaries, so partial events are buffered until the terminating blank line.
type Transform struct {
	catalog    *Catalog
	clientName string

	buffer    []byte
	assembled strings.Builder
	emitted   int
	template  map[string]any
	lastIndex int

	// heldText carries events that may still be forwarded if the response turns
	// out to be an ordinary answer (assistant text, message items). They are
	// stored decoded so their text fields can be rewritten if the marker could
	// not be relayed.
	heldText []heldEvent
	// heldNative carries backend tool-call events. They are discarded rather
	// than released: their arguments follow the spreadsheet-agent schema, which
	// the client cannot execute, so exposing them would only provoke retries.
	heldNative [][]byte

	markerMode bool
	sawFinish  bool
	terminal   map[string]any
	// producedOutput records whether any assistant text or tool call reached the
	// client. A turn that produced none fails explicitly instead of falsely
	// completing the downstream agent loop.
	producedOutput bool
	messageSeen    bool

	currentEvent string
	currentData  strings.Builder
}

// NewTransform builds a stream transform for one response.
func NewTransform(catalog *Catalog, clientName string) *Transform {
	return &Transform{
		catalog:    catalog,
		clientName: clientName,
		template:   map[string]any{},
		lastIndex:  0,
	}
}

// Write consumes a chunk of upstream bytes and returns the events to forward.
func (t *Transform) Write(chunk []byte) [][]byte {
	t.buffer = append(t.buffer, chunk...)
	var out [][]byte
	for {
		index := bytes.IndexByte(t.buffer, '\n')
		if index < 0 {
			break
		}
		line := t.buffer[:index]
		t.buffer = t.buffer[index+1:]
		out = append(out, t.dispatch(strings.TrimRight(string(line), "\r"))...)
	}
	return out
}

// Close flushes any buffered tail and releases held events.
func (t *Transform) Close() [][]byte {
	var out [][]byte
	if len(bytes.TrimSpace(t.buffer)) > 0 {
		out = append(out, t.dispatch(strings.TrimRight(string(t.buffer), "\r"))...)
		t.buffer = nil
	}
	// Dispatch a final data line even when EOF arrives without an SSE blank line.
	out = append(out, t.dispatchEvent()...)
	// A stream that ended without a terminal event still needs the held text
	// released, otherwise the client loses assistant output entirely.
	if !t.sawFinish {
		released, _ := t.release()
		out = append(out, released...)
	}
	return out
}

// Terminal returns the completed response payload, if the stream finished. The
// payload has the caller's model name restored and reasoning items removed.
func (t *Transform) Terminal() map[string]any { return t.terminal }

// dispatch handles one SSE line.
func (t *Transform) dispatch(line string) [][]byte {
	switch {
	case line == "":
		return t.dispatchEvent()
	case strings.HasPrefix(line, "event:"):
		t.currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		t.currentData.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	default:
		// Comments, retry hints and unknown fields are ignored.
	}
	return nil
}

// dispatchEvent processes one complete event and returns the events to forward.
func (t *Transform) dispatchEvent() [][]byte {
	eventName := t.currentEvent
	payload := t.currentData.String()
	t.currentEvent = ""
	t.currentData.Reset()
	if payload == "" {
		return nil
	}
	if payload == "[DONE]" {
		t.sawFinish = true
		return [][]byte{[]byte("data: [DONE]\n\n")}
	}

	var decoded map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payload), &decoded); errUnmarshal != nil {
		// Unparseable frames are dropped rather than forwarded, because a
		// malformed frame cannot be reasoned about or rewritten.
		return nil
	}
	kind := strings.ToLower(strings.TrimSpace(stringField(decoded, "type")))
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(eventName))
	}

	switch {
	case kind == "response.output_text.delta":
		return t.onTextDelta(decoded)
	case kind == "response.output_text.done":
		if t.markerMode {
			t.holdText(kind, decoded)
			return nil
		}
		return append(t.flushText(), t.passDecoded(kind, decoded)...)
	case strings.HasPrefix(kind, "response.reasoning"):
		return nil
	case kind == "response.function_call_arguments.delta" || kind == "response.function_call_arguments.done" ||
		kind == "response.custom_tool_call_input.delta" || kind == "response.custom_tool_call_input.done":
		t.holdNative(payload)
		return nil
	case kind == "response.output_item.added" || kind == "response.output_item.done":
		return t.onOutputItem(kind, decoded, payload)
	case kind == "response.content_part.added" || kind == "response.content_part.done":
		if part, okPart := decoded["part"].(map[string]any); okPart {
			if strings.HasPrefix(strings.ToLower(stringField(part, "type")), "reasoning") {
				return nil
			}
		}
		if t.markerMode {
			t.holdText(kind, decoded)
			return nil
		}
		return t.passDecoded(kind, decoded)
	case kind == "response.created" || kind == "response.in_progress":
		if sanitized := t.sanitizeEventPayload(kind, decoded); sanitized != nil {
			return [][]byte{sanitized}
		}
		return nil
	case kind == "response.completed" || kind == "response.failed" || kind == "response.incomplete":
		t.sawFinish = true
		return t.onTerminal(kind, decoded, payload)
	default:
		return t.passDecoded(kind, decoded)
	}
}

// onTextDelta accumulates assistant text and streams it, unless a marker may be
// in flight.
func (t *Transform) onTextDelta(decoded map[string]any) [][]byte {
	delta, _ := decoded["delta"].(string)
	if delta == "" {
		return nil
	}
	t.assembled.WriteString(delta)
	t.template = map[string]any{}
	for _, key := range []string{"item_id", "output_index", "content_index"} {
		if value, exists := decoded[key]; exists {
			t.template[key] = value
		}
	}
	if index, okIndex := decoded["output_index"].(float64); okIndex {
		t.lastIndex = int(index)
	}
	if t.markerMode {
		return nil
	}

	full := t.assembled.String()
	// Hold back a suffix that could still turn into a marker open tag, so a
	// marker split across chunk boundaries is never leaked to the client.
	if !t.catalog.Empty() {
		start := t.minSearch()
		if position := strings.Index(full[start:], MarkerOpen); position >= 0 {
			position += start
			pending := full[t.emitted:position]
			t.emitted = position
			t.markerMode = true
			if pending == "" {
				return nil
			}
			return [][]byte{t.deltaEvent(pending)}
		}
	}
	boundary := len(full) - markerHoldLength(full)
	if boundary <= t.emitted {
		return nil
	}
	pending := full[t.emitted:boundary]
	t.emitted = boundary
	return [][]byte{t.deltaEvent(pending)}
}

func (t *Transform) minSearch() int {
	start := t.emitted - len(MarkerOpen) + 1
	if start < 0 {
		return 0
	}
	return start
}

// markerHoldLength returns the length of a text suffix that could still become
// a marker open tag.
func markerHoldLength(text string) int {
	limit := len(MarkerOpen) - 1
	if limit > len(text) {
		limit = len(text)
	}
	for probe := limit; probe > 0; probe-- {
		if strings.HasSuffix(text, MarkerOpen[:probe]) {
			return probe
		}
	}
	return 0
}

// onOutputItem drops reasoning items, holds native tool calls, and otherwise
// forwards the item.
func (t *Transform) onOutputItem(kind string, decoded map[string]any, payload string) [][]byte {
	item, _ := decoded["item"].(map[string]any)
	itemType := strings.ToLower(stringField(item, "type"))
	if itemType == "message" {
		t.messageSeen = true
	}
	if itemType == "reasoning" {
		return nil
	}
	if index, okIndex := decoded["output_index"].(float64); okIndex {
		t.lastIndex = int(index)
	}
	if itemType == "function_call" || itemType == "custom_tool_call" {
		// Native backend tool calls never reach the client: their arguments
		// follow the spreadsheet-agent schema, which the client cannot execute.
		t.holdNative(payload)
		return nil
	}
	if t.markerMode && itemType == "message" && kind == "response.output_item.done" {
		t.holdText(kind, decoded)
		return nil
	}
	return t.passDecoded(kind, decoded)
}

// onTerminal decides between a relayed tool call and an ordinary answer.
func (t *Transform) onTerminal(kind string, decoded map[string]any, payload string) [][]byte {
	response, _ := decoded["response"].(map[string]any)
	t.terminal = t.sanitizeResponse(response)

	if kind == "response.completed" {
		text := t.assembled.String()
		if text == "" && response != nil {
			text = responseText(response)
			// Some backends provide the answer only in the terminal payload.
			// Feed it through the same marker handling and text flush path.
			t.assembled.WriteString(text)
			if strings.Contains(text, MarkerOpen) {
				t.markerMode = true
			}
		}
		if call, okCall := t.catalog.ParseMarker(text); okCall {
			t.heldText = nil
			t.heldNative = nil
			t.emitted = len(t.assembled.String())
			return t.synthesize(call)
		}
	}
	// A complete payload without streamed items needs the full message lifecycle,
	// not an orphan text delta that strict clients silently ignore.
	if kind == "response.completed" && !t.messageSeen && !t.producedOutput && !t.markerMode && strings.TrimSpace(responseText(t.terminal)) != "" {
		return t.synthesizeMessage(responseText(t.terminal), kind)
	}
	out, scrubbed := t.release()
	if t.terminal == nil {
		return append(out, t.passDecoded(kind, decoded)...)
	}
	// Forward the sanitized payload: the raw one still advertises the backend
	// model slug and any native tool call the client must not see.
	terminal := t.terminal
	if scrubbed != "" {
		if rewritten, okRewrite := rewriteTextFields(terminal, scrubbed).(map[string]any); okRewrite && rewritten != nil {
			terminal = rewritten
		}
	}
	if !t.producedOutput && kind == "response.completed" {
		// Do not manufacture a successful final answer after dropping all output.
		// Clients interpret a successful empty turn as completion of the agent loop.
		kind = "response.failed"
		terminal["status"] = "failed"
		terminal["error"] = map[string]any{
			"type": "server_error", "code": "excel_no_usable_output",
			"message": "Excel backend returned no usable assistant output or client tool call; retry the request.",
		}
	}

	t.terminal = terminal
	// The event wraps the response object as {"type":...,"response":{...}}; it is
	// NOT the response object itself. Emitting the bare object is an event a
	// strict Responses client cannot recognise, so it would miss the terminal
	// frame entirely.
	return append(out, encodeEvent(map[string]any{"type": kind, "response": terminal}))
}

// synthesizeMessage emits the lifecycle strict Responses clients require when
// the backend returned text only in the terminal object.
func (t *Transform) synthesizeMessage(text, kind string) [][]byte {
	id := "msg_" + t.callID("assistant")
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	item := map[string]any{"id": id, "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
	t.terminal["output"] = []any{item}
	t.producedOutput = true
	t.emitted = len(t.assembled.String())
	t.heldText = nil
	t.heldNative = nil
	return [][]byte{
		encodeEvent(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}),
		encodeEvent(map[string]any{"type": "response.content_part.added", "output_index": 0, "item_id": id, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}),
		encodeEvent(map[string]any{"type": "response.output_text.delta", "output_index": 0, "item_id": id, "content_index": 0, "delta": text}),
		encodeEvent(map[string]any{"type": "response.output_text.done", "output_index": 0, "item_id": id, "content_index": 0, "text": text}),
		encodeEvent(map[string]any{"type": "response.content_part.done", "output_index": 0, "item_id": id, "content_index": 0, "part": part}),
		encodeEvent(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}),
		encodeEvent(map[string]any{"type": kind, "response": t.terminal}),
	}
}

// release forwards held text and message events, and drops held backend tool
// calls so no spreadsheet-agent tool can ever reach the client.
//
// If a marker was detected but could not be turned into a tool call, the marker
// regions are scrubbed out of the text. Leaking the raw relay marker would show
// the user a protocol artifact instead of either an answer or a tool call.
func (t *Transform) release() ([][]byte, string) {
	scrubbed := ""
	if t.markerMode {
		scrubbed = t.scrubPendingMarker()
	}
	var out [][]byte
	out = append(out, t.flushText()...)
	for _, held := range t.heldText {
		decoded := held.decoded
		if scrubbed != "" {
			// The done/message events carry the whole assistant text, so they
			// would otherwise re-introduce the marker the deltas just scrubbed.
			decoded, _ = rewriteTextFields(decoded, scrubbed).(map[string]any)
			if decoded == nil {
				continue
			}
		}
		if payload, okEncode := marshalEvent(decoded); okEncode {
			out = append(out, []byte("event: "+held.kind+"\ndata: "+payload+"\n\n"))
		}
	}
	t.heldText = nil
	t.heldNative = nil
	t.markerMode = false
	return out, scrubbed
}

// rewriteTextFields replaces any string containing the relay marker with the
// replacement text, recursively.
func rewriteTextFields(value any, replacement string) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			out[key] = rewriteTextFields(entry, replacement)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, entry := range typed {
			out[index] = rewriteTextFields(entry, replacement)
		}
		return out
	case string:
		if strings.Contains(typed, MarkerOpen) || strings.Contains(typed, MarkerClose) {
			return replacement
		}
		return typed
	default:
		return value
	}
}

// scrubPendingMarker removes unrelayable marker text from the pending output and
// leaves a short note when nothing else remains, so the turn is not silently
// blank.
func (t *Transform) scrubPendingMarker() string {
	full := t.assembled.String()
	if len(full) <= t.emitted {
		return ""
	}
	remaining := full[t.emitted:]
	start := strings.Index(remaining, MarkerOpen)
	if start < 0 {
		return ""
	}
	before := remaining[:start]
	rest := remaining[start:]
	if end := strings.Index(rest, MarkerClose); end >= 0 {
		rest = rest[end+len(MarkerClose):]
	} else {
		rest = ""
	}
	cleaned := strings.TrimSpace(before + " " + rest)
	if cleaned == "" {
		cleaned = "(the model attempted a tool call that could not be relayed; no action was taken)"
	}
	// Rebuild the buffer so subsequent flushes emit only the scrubbed text.
	t.assembled.Reset()
	t.assembled.WriteString(full[:t.emitted])
	t.assembled.WriteString(cleaned)
	return cleaned
}

// flushText emits any accumulated text that has not been forwarded yet.
func (t *Transform) flushText() [][]byte {
	full := t.assembled.String()
	if len(full) <= t.emitted {
		return nil
	}
	pending := full[t.emitted:]
	t.emitted = len(full)
	return [][]byte{t.deltaEvent(pending)}
}

// deltaEvent builds a response.output_text.delta carrying the text.
func (t *Transform) deltaEvent(text string) []byte {
	if text != "" {
		t.producedOutput = true
	}
	event := map[string]any{
		"type":  "response.output_text.delta",
		"delta": text,
	}
	for key, value := range t.template {
		event[key] = value
	}
	return encodeEvent(event)
}

// synthesize replaces the held marker exchange with a real tool call.
func (t *Transform) synthesize(call RelayCall) [][]byte {
	t.producedOutput = true
	callID := t.callID(call.Name)
	itemType := "function_call"
	valueKey := "arguments"
	deltaEvent := "response.function_call_arguments.delta"
	doneEvent := "response.function_call_arguments.done"
	itemID := "fc_" + callID
	if call.Custom {
		itemType = "custom_tool_call"
		valueKey = "input"
		deltaEvent = "response.custom_tool_call_input.delta"
		doneEvent = "response.custom_tool_call_input.done"
		itemID = "ctc_" + callID
	}
	index := t.lastIndex

	inProgress := map[string]any{
		"type":    itemType,
		"id":      itemID,
		"call_id": callID,
		"name":    call.Name,
		"status":  "in_progress",
	}
	inProgress[valueKey] = ""
	completed := map[string]any{
		"type":    itemType,
		"id":      itemID,
		"call_id": callID,
		"name":    call.Name,
		"status":  "completed",
	}
	completed[valueKey] = call.Arguments
	if call.Namespace != "" {
		inProgress["namespace"] = call.Namespace
		completed["namespace"] = call.Namespace
	}

	deltaPayload := map[string]any{
		"type":         deltaEvent,
		"item_id":      itemID,
		"output_index": index,
		"delta":        call.Arguments,
	}
	donePayload := map[string]any{
		"type":         doneEvent,
		"item_id":      itemID,
		"output_index": index,
	}
	// The done event carries the whole value under the item's own field name
	// ("arguments" for a function call, "input" for a custom tool call). The
	// delta event always uses "delta".
	donePayload[valueKey] = call.Arguments

	out := [][]byte{
		encodeEvent(map[string]any{
			"type":         "response.output_item.added",
			"output_index": index,
			"item":         inProgress,
		}),
		encodeEvent(deltaPayload),
		encodeEvent(donePayload),
		encodeEvent(map[string]any{
			"type":         "response.output_item.done",
			"output_index": index,
			"item":         completed,
		}),
	}

	if t.terminal != nil {
		completedResponse := t.terminal
		completedResponse["output"] = []any{completed}
		completedResponse["status"] = "completed"
		completedResponse["error"] = nil
		completedResponse["incomplete_details"] = nil
		out = append(out, encodeEvent(map[string]any{
			"type":     "response.completed",
			"response": completedResponse,
		}))
	}
	return out
}

// callID derives a stable call identifier so a retried turn reuses the same id.
func (t *Transform) callID(name string) string {
	conversation := ""
	if taskID, okTask := t.terminal["id"].(string); okTask {
		conversation = taskID
	}
	seed := identityNamespace + conversation + "/call/" + strconv.Itoa(t.emitted) + "/" + name
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

// sanitizeResponse maps the model back to the caller's alias and removes
// reasoning items from the reported output.
func (t *Transform) sanitizeResponse(response map[string]any) map[string]any {
	if response == nil {
		return nil
	}
	if t.clientName != "" {
		response["model"] = t.clientName
	}
	// The backend injects a ~12k-token spreadsheet-agent prompt and ~20 native
	// workbook tools into the response object. Neither belongs in a client's
	// view of its own request.
	delete(response, "instructions")
	delete(response, "tools")
	delete(response, "tool_choice")
	output, okOutput := response["output"].([]any)
	if !okOutput {
		return response
	}
	filtered := make([]any, 0, len(output))
	for _, raw := range output {
		item, okItem := raw.(map[string]any)
		if !okItem {
			continue
		}
		itemType := strings.ToLower(stringField(item, "type"))
		if itemType == "reasoning" || itemType == "function_call" || itemType == "custom_tool_call" {
			// Reasoning cannot be replayed to this backend, and a backend tool
			// call is not executable by the client. A relayed call is re-added by
			// synthesize() as a client-owned item.
			continue
		}
		filtered = append(filtered, item)
	}
	response["output"] = filtered
	return response
}

// passDecoded forwards an upstream event after re-encoding it.
//
// Re-encoding normalises Go's HTML escaping, which would otherwise turn "<" into
// "\u003c" and mangle model output such as the relay marker. It also guarantees
// every forwarded frame is valid JSON.
func (t *Transform) passDecoded(kind string, decoded map[string]any) [][]byte {
	payload, okEncode := marshalEvent(decoded)
	if !okEncode {
		return nil
	}
	return [][]byte{[]byte("event: " + kind + "\ndata: " + payload + "\n\n")}
}

// heldEvent is a buffered upstream event whose text may need rewriting.
type heldEvent struct {
	kind    string
	decoded map[string]any
}

// holdText stores an event that may be released if no tool call materialises.
func (t *Transform) holdText(kind string, decoded map[string]any) {
	t.heldText = append(t.heldText, heldEvent{kind: kind, decoded: decoded})
}

// holdNative stores a backend tool-call payload that is never forwarded.
func (t *Transform) holdNative(payload string) {
	t.heldNative = append(t.heldNative, []byte("data: "+payload+"\n\n"))
}

// hasToolCallItem reports whether a payload describes a backend tool call.
func hasToolCallItem(payload string) bool {
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal([]byte(payload), &decoded); errUnmarshal != nil {
		return false
	}
	item, _ := decoded["item"].(map[string]any)
	if item == nil {
		return false
	}
	itemType := strings.ToLower(stringField(item, "type"))
	return itemType == "function_call" || itemType == "custom_tool_call"
}

// encodeEvent renders a synthesized event as SSE.
func encodeEvent(event map[string]any) []byte {
	encoded, okEncode := marshalEvent(event)
	if !okEncode {
		return nil
	}
	eventType, _ := event["type"].(string)
	return []byte("event: " + eventType + "\ndata: " + encoded + "\n\n")
}

// marshalEvent renders JSON without HTML escaping so text passes through
// byte-faithfully (a '<' must not become "\u003c").
func marshalEvent(value any) (string, bool) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(value); errEncode != nil {
		return "", false
	}
	return strings.TrimRight(buffer.String(), "\n"), true
}

// sanitizeEventPayload rewrites the response object carried by a lifecycle
// event so the client never sees the spreadsheet-agent prompt, the backend's
// native tool list, or the upstream model slug.
func (t *Transform) sanitizeEventPayload(kind string, decoded map[string]any) []byte {
	response, okResponse := decoded["response"].(map[string]any)
	if !okResponse {
		return nil
	}
	sanitized := t.sanitizeResponse(response)
	if sanitized == nil {
		return nil
	}
	payload, okEncode := marshalEvent(map[string]any{"type": kind, "response": sanitized})
	if !okEncode {
		return nil
	}
	return []byte("event: " + kind + "\ndata: " + payload + "\n\n")
}

// responseText flattens the assistant text carried by a completed response.
func responseText(response map[string]any) string {
	output, _ := response["output"].([]any)
	var builder strings.Builder
	for _, raw := range output {
		item, okItem := raw.(map[string]any)
		if !okItem {
			continue
		}
		parts, okParts := item["content"].([]any)
		if !okParts {
			continue
		}
		for _, rawPart := range parts {
			part, okPart := rawPart.(map[string]any)
			if !okPart {
				continue
			}
			if text, okText := part["text"].(string); okText {
				builder.WriteString(text)
			}
		}
	}
	return builder.String()
}
