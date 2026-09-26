package excel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseCatalogFlattensNamespaces(t *testing.T) {
	tools := []any{
		map[string]any{"type": "function", "name": "exec_command", "description": "run", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "custom", "name": "apply_patch", "format": map[string]any{"type": "text"}},
		map[string]any{"type": "namespace", "name": "image_gen", "tools": []any{
			map[string]any{"type": "function", "name": "imagegen"},
		}},
	}
	catalog := ParseCatalog(tools)
	if len(catalog.Specs) != 3 {
		t.Fatalf("expected 3 specs, got %d", len(catalog.Specs))
	}
	if !catalog.Has("exec_command") || !catalog.Has("apply_patch") || !catalog.Has("image_gen.imagegen") {
		t.Fatalf("namespace flattening failed: %+v", catalog.Specs)
	}
	if kind, ok := catalog.TypeOf("apply_patch"); !ok || kind != "custom" {
		t.Fatalf("custom tool type not preserved")
	}
}

func TestToolChoiceDisabledYieldsNoCatalogUse(t *testing.T) {
	body := map[string]any{
		"model":       "gpt-5.6-sol-excel",
		"tool_choice": "none",
		"tools":       []any{map[string]any{"type": "function", "name": "exec_command"}},
	}
	if !ToolChoiceDisabled(body) {
		t.Fatal("tool_choice none was not detected")
	}
}

func TestPrepareBodyWireShape(t *testing.T) {
	body := map[string]any{
		"model":  "gpt-5.6-sol-excel",
		"stream": true,
		"store":  true,
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
		},
		"tool_choice":          "auto",
		"max_output_tokens":    1234,
		"previous_response_id": "resp_prev",
		"reasoning":            map[string]any{"effort": "high"},
		"input": []any{
			map[string]any{"type": "reasoning", "encrypted_content": "abc"},
			map[string]any{"type": "item_reference", "id": "x"},
			map[string]any{
				"type":    "message",
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
			},
		},
	}
	upstream, errPrepare := PrepareBody(body, ParseCatalog(body["tools"]))
	if errPrepare != nil {
		t.Fatalf("prepare failed: %v", errPrepare)
	}

	// Fields the backend rejects or derives must be gone.
	for _, forbidden := range []string{"tools", "tool_choice", "max_output_tokens", "previous_response_id", "reasoning", "include"} {
		if _, exists := upstream[forbidden]; exists {
			t.Fatalf("field %q must not be forwarded", forbidden)
		}
	}
	if upstream["model"] != "gpt-5.6-sol" {
		t.Fatalf("alias was not resolved to the upstream slug: %v", upstream["model"])
	}
	if upstream["model_selection"] != "explicit" {
		t.Fatal("model_selection must be explicit so the backend does not re-route the model")
	}
	if upstream["store"] != false {
		t.Fatal("store must be false")
	}
	if upstream["reasoning_effort"] != "high" {
		t.Fatalf("reasoning effort not forwarded: %v", upstream["reasoning_effort"])
	}

	metadata, _ := upstream["metadata"].(map[string]any)
	if metadata["task_id"] == nil || metadata["turn_id"] == nil {
		t.Fatalf("backend requires task_id and turn_id: %v", metadata)
	}

	items, _ := upstream["input"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected prologue + 1 surviving item, got %d", len(items))
	}
	prologue, _ := items[0].(map[string]any)
	if prologue["role"] != "developer" {
		t.Fatalf("prologue must be a developer message, got %v", prologue["role"])
	}
	if !strings.Contains(renderedText(t, prologue), MarkerOpen) {
		t.Fatal("prologue must teach the marker protocol")
	}
}

func TestPrepareBodyDerivesStableIdentities(t *testing.T) {
	body := map[string]any{
		"model":  "gpt-5.6-sol-excel",
		"stream": true,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
	}
	first, errFirst := PrepareBody(body, &Catalog{})
	if errFirst != nil {
		t.Fatalf("prepare failed: %v", errFirst)
	}
	second, errSecond := PrepareBody(body, &Catalog{})
	if errSecond != nil {
		t.Fatalf("prepare failed: %v", errSecond)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("identical requests must serialise identically so the upstream cache and turn identity stay stable")
	}
}

func TestPrepareBodyTurnSpansToolIterations(t *testing.T) {
	base := func(extra []any) map[string]any {
		items := []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "do it"}}},
		}
		items = append(items, extra...)
		return map[string]any{"model": "gpt-5.6-sol-excel", "input": items}
	}

	first, _ := PrepareBody(base(nil), &Catalog{})
	withOneResult, _ := PrepareBody(base([]any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "exec_command", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
	}), &Catalog{})

	firstMeta, _ := first["metadata"].(map[string]any)
	secondMeta, _ := withOneResult["metadata"].(map[string]any)
	if firstMeta["turn_id"] != secondMeta["turn_id"] {
		t.Fatal("a tool result must not advance the turn; the backend would discard its plan state")
	}
	if firstMeta["agent_iteration"] == secondMeta["agent_iteration"] {
		t.Fatal("agent_iteration must advance within a turn")
	}
}

func TestParseMarker(t *testing.T) {
	catalog := ParseCatalog([]any{
		map[string]any{"type": "function", "name": "exec_command"},
		map[string]any{"type": "custom", "name": "apply_patch"},
	})

	t.Run("exact marker", func(t *testing.T) {
		call, ok := catalog.ParseMarker(`<codex_tool_call>{"name":"exec_command","arguments":{"cmd":"pwd"}}</codex_tool_call>`)
		if !ok {
			t.Fatal("marker was not parsed")
		}
		if call.Name != "exec_command" || !strings.Contains(call.Arguments, "pwd") || call.Custom {
			t.Fatalf("unexpected call: %+v", call)
		}
	})

	t.Run("custom tool uses input", func(t *testing.T) {
		call, ok := catalog.ParseMarker(`<codex_tool_call>{"name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}</codex_tool_call>`)
		if !ok || !call.Custom {
			t.Fatalf("custom call not recognised: %+v ok=%v", call, ok)
		}
		if !strings.Contains(call.Arguments, "Begin Patch") {
			t.Fatalf("custom input lost: %q", call.Arguments)
		}
	})

	t.Run("prose around the marker is irrelevant", func(t *testing.T) {
		call, ok := catalog.ParseMarker(`sure, running that now <codex_tool_call>{"name":"exec_command","arguments":{"cmd":"ls"}}</codex_tool_call>`)
		if !ok || call.Name != "exec_command" {
			t.Fatalf("marker with leading prose not parsed: %+v", call)
		}
	})

	t.Run("unknown tool is rejected", func(t *testing.T) {
		if _, ok := catalog.ParseMarker(`<codex_tool_call>{"name":"read_ranges","arguments":{}}</codex_tool_call>`); ok {
			t.Fatal("a native workbook tool must not be relayed as a client tool")
		}
	})

	t.Run("plain answer yields no call", func(t *testing.T) {
		if _, ok := catalog.ParseMarker("The answer is 42."); ok {
			t.Fatal("plain text must not produce a call")
		}
	})

	t.Run("empty catalog never matches", func(t *testing.T) {
		var empty Catalog
		if _, ok := empty.ParseMarker(`<codex_tool_call>{"name":"exec_command","arguments":{}}</codex_tool_call>`); ok {
			t.Fatal("a request without tools must not relay anything")
		}
	})
}

func TestNormalizeEffort(t *testing.T) {
	cases := []struct {
		model  string
		effort string
		want   string
	}{
		{"gpt-5.6-sol-excel", "x-high", "xhigh"},
		{"gpt-5.6-sol-excel", "HIGH", "high"},
		{"gpt-5.6-sol-excel", "bogus", "medium"},
		{"gpt-5.6-sol-excel", "", "medium"},
		{"gpt-6-astra-excel", "low", "low"},
		{"gpt-6-astra-excel", "xhigh", "xhigh"},
	}
	for _, testCase := range cases {
		if got := NormalizeEffort(testCase.model, testCase.effort); got != testCase.want {
			t.Errorf("NormalizeEffort(%q, %q) = %q, want %q", testCase.model, testCase.effort, got, testCase.want)
		}
	}
}

func TestSlugForRejectsUnknownModel(t *testing.T) {
	if _, ok := SlugFor("gpt-5.6-sol"); !ok {
		t.Fatal("a bare upstream slug must resolve")
	}
	if _, ok := SlugFor("gpt-5.6-sol-excel"); !ok {
		t.Fatal("an alias must resolve")
	}
	if _, ok := SlugFor("gpt-5.5-codex"); ok {
		t.Fatal("a foreign model must not resolve to an Excel slug")
	}
}

func renderedText(t *testing.T, message map[string]any) string {
	t.Helper()
	content, _ := message["content"].([]any)
	var builder strings.Builder
	for _, raw := range content {
		part, okPart := raw.(map[string]any)
		if !okPart {
			continue
		}
		if text, okText := part["text"].(string); okText {
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func TestParseMarkerToleratesModelDeviations(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})

	cases := []struct {
		name string
		text string
	}{
		{
			// Observed in live traffic: the model appends prose after the object
			// but still inside the marker.
			name: "trailing prose inside the marker",
			text: `<codex_tool_call>{"name":"exec_command","arguments":{"cmd":"pwd"}} responding to user in final because external tool call protocol.</codex_tool_call>`,
		},
		{
			name: "trailing space before the close tag",
			text: `<codex_tool_call>{"name":"exec_command","arguments":{"cmd":"pwd"}} </codex_tool_call>`,
		},
		{
			name: "missing close tag",
			text: `<codex_tool_call>{"name":"exec_command","arguments":{"cmd":"pwd"}}`,
		},
		{
			name: "code fence wrapper",
			text: "<codex_tool_call>\n```json\n{\"name\":\"exec_command\",\"arguments\":{\"cmd\":\"pwd\"}}\n```\n</codex_tool_call>",
		},
		{
			name: "assignment prefix",
			text: `<codex_tool_call>const request = {"name":"exec_command","arguments":{"cmd":"pwd"}}</codex_tool_call>`,
		},
		{
			// A brace inside a string value must not terminate the object early.
			name: "braces inside a shell command",
			text: `<codex_tool_call>{"name":"exec_command","arguments":{"cmd":"awk '{print $1}' file"}}</codex_tool_call>`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			call, ok := catalog.ParseMarker(testCase.text)
			if !ok {
				t.Fatalf("marker not recognised: %s", testCase.text)
			}
			if call.Name != "exec_command" {
				t.Fatalf("wrong tool: %q", call.Name)
			}
			if !strings.Contains(call.Arguments, "pwd") && !strings.Contains(call.Arguments, "awk") {
				t.Fatalf("arguments not recovered: %q", call.Arguments)
			}
		})
	}
}

func TestParseMarkerIgnoresProseAfterCloseTag(t *testing.T) {
	catalog := ParseCatalog([]any{map[string]any{"type": "function", "name": "exec_command"}})
	call, ok := catalog.ParseMarker(`<codex_tool_call>{"name":"exec_command","arguments":{"cmd":"ls"}}</codex_tool_call> and then I will continue.`)
	if !ok || call.Name != "exec_command" {
		t.Fatalf("marker followed by prose not parsed: %+v ok=%v", call, ok)
	}
	if strings.Contains(call.Arguments, "continue") {
		t.Fatalf("trailing prose leaked into arguments: %q", call.Arguments)
	}
}

func TestCleanInputAcceptsImplicitMessageType(t *testing.T) {
	input, err := CleanInput([]any{map[string]any{"role": "user", "content": "do not drop this"}})
	if err != nil || len(input) != 1 {
		t.Fatalf("valid Responses message lost: %v %v", input, err)
	}
}
