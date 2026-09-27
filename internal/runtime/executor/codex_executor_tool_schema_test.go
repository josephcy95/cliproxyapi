package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

// Issue #5551: Tool schema with 13-branch oneOf causes ChatGPT Codex upstream
// to abort silently. The executor must simplify the complex union without destroying
// property names, enum values, or sibling properties.
func TestCodexExecutorSimplifiesComplexOneOfToolSchema(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read request body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":   "test",
			"base_url":  server.URL,
			"plan_type": "pro",
		},
	}

	// 13-branch oneOf reproduction payload from Issue #5551
	requestPayload := []byte(`{
		"model": "gpt-5.5",
		"messages": [{"role": "user", "content": "Reply with exactly: OK"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "t1",
				"description": "test tool",
				"parameters": {
					"type": "object",
					"properties": {
						"action": {
							"type": "string",
							"enum": ["p.list","m.list","s.list","s.create","s.send","s.fork","s.status","s.messages","sch.list","sch.create","sch.run","sch.delete","sch.toggle"],
							"oneOf": [
								{"const": "p.list", "description": "List projects"},
								{"const": "m.list", "description": "List models"},
								{"const": "s.list", "description": "List sessions"},
								{"const": "s.create", "description": "Create session"},
								{"const": "s.send", "description": "Send prompt"},
								{"const": "s.fork", "description": "Fork session"},
								{"const": "s.status", "description": "Session status"},
								{"const": "s.messages", "description": "Session messages"},
								{"const": "sch.list", "description": "List schedule"},
								{"const": "sch.create", "description": "Create schedule"},
								{"const": "sch.run", "description": "Run schedule"},
								{"const": "sch.delete", "description": "Delete schedule"},
								{"const": "sch.toggle", "description": "Toggle schedule"}
							],
							"description": "Action to perform"
						},
						"target_id": {
							"type": "string",
							"description": "Optional target"
						}
					},
					"required": ["action"]
				}
			}
		}]
	}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: requestPayload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	tools := gjson.GetBytes(gotBody, "tools").Array()
	if len(tools) == 0 {
		t.Fatalf("expected tools in upstream payload, got: %s", gotBody)
	}

	// Find tool t1
	var t1Tool gjson.Result
	for _, tool := range tools {
		if tool.Get("name").String() == "t1" {
			t1Tool = tool
			break
		}
	}
	if !t1Tool.Exists() {
		t1Tool = tools[0]
	}

	// Complex oneOf must be removed from action property
	if t1Tool.Get("parameters.properties.action.oneOf").Exists() {
		t.Fatalf("expected complex oneOf to be removed from tool parameters, got: %s", t1Tool.Get("parameters").Raw)
	}
	// Action type and enum must be preserved
	if t1Tool.Get("parameters.properties.action.type").String() != "string" {
		t.Fatalf("expected action.type = string, got: %s", t1Tool.Get("parameters.properties.action.type").String())
	}
	if len(t1Tool.Get("parameters.properties.action.enum").Array()) != 13 {
		t.Fatalf("expected action.enum to retain all 13 items")
	}
	// Sibling property target_id must be preserved
	if t1Tool.Get("parameters.properties.target_id.type").String() != "string" {
		t.Fatalf("sibling property target_id was dropped")
	}
	// Required field must be preserved
	if t1Tool.Get("parameters.required.0").String() != "action" {
		t.Fatalf("required list was dropped")
	}
}

// Issue #5551: If ChatGPT Codex upstream terminates immediately with response.incomplete,
// reason: "max_output_tokens", and output_tokens: 0 (no output produced), it must be treated
// as an upstream failure rather than a silent successful completion.
