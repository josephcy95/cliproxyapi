package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type excelUsageCapture struct {
	alias   string
	records chan usage.Record
}

func (p *excelUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.Alias == p.alias {
		select {
		case p.records <- record:
		default:
		}
	}
}

type excelUsageNoop struct{}

func (excelUsageNoop) HandleUsage(context.Context, usage.Record) {}

func TestExcelUsageLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                                                                            string
		stream, holdOpen, noUsage, broken, truncated, cancelBefore, cancelAfter, failed bool
	}{
		{name: "stream_success", stream: true},
		{name: "stream_terminal_before_eof", stream: true, holdOpen: true},
		{name: "stream_cancel_after_terminal", stream: true, holdOpen: true, cancelAfter: true},
		{name: "stream_without_usage", stream: true, noUsage: true},
		{name: "stream_without_usage_before_eof", stream: true, noUsage: true, holdOpen: true},
		{name: "nonstream_success"},
		{name: "nonstream_without_usage", noUsage: true},
		{name: "nonstream_terminal_before_eof", holdOpen: true},
		{name: "stream_read_failure", stream: true, broken: true},
		{name: "stream_truncated", stream: true, truncated: true},
		{name: "stream_cancel_before_terminal", stream: true, holdOpen: true, cancelBefore: true},
		{name: "stream_failed_terminal", stream: true, failed: true, holdOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &excelUsageCapture{alias: t.Name(), records: make(chan usage.Record, 16)}
			usage.RegisterNamedPlugin(t.Name(), capture)
			t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), excelUsageNoop{}) })
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.broken {
					w.Header().Set("Content-Length", "100000")
				}
				if tc.broken || tc.truncated || tc.cancelBefore {
					fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"usage-test\"}}\n\n")
				} else {
					tokens := `,"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}`
					if tc.noUsage {
						tokens = ""
					}
					status := "completed"
					if tc.failed {
						status = "failed"
						tokens += `,"error":{"type":"server_error","message":"test upstream failure"}`
					}
					fmt.Fprintf(w, "data: {\"type\":\"response.%s\",\"response\":{\"id\":\"usage-test\",\"status\":\"%s\",\"model\":\"gpt-5.6-sol\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]%s}}\n\n", status, status, tokens)
				}
				w.(http.Flusher).Flush()
				if tc.holdOpen {
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
			}))
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(usage.WithRequestedModelAlias(context.Background(), t.Name()))
			defer cancel()
			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: t.Name(), Index: "excel-test-index", Provider: "codex", Attributes: map[string]string{"api_key": "test-only", "excel_base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "gpt-5.6-sol-excel", Payload: []byte(`{"model":"gpt-5.6-sol-excel","input":"hello"}`)}
			wantFailure := tc.broken || tc.truncated || tc.cancelBefore || tc.failed
			if tc.stream {
				result, err := executor.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{})
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.After(2 * time.Second)
				completed, sawError := false, false
				draining := true
				for draining {
					select {
					case chunk, ok := <-result.Chunks:
						if !ok {
							draining = false
							continue
						}
						if chunk.Err != nil {
							sawError = true
						}
						if strings.Contains(string(chunk.Payload), "response.completed") || strings.Contains(string(chunk.Payload), "response.failed") {
							completed = true
							if tc.cancelAfter {
								cancel()
							}
						}
						if tc.cancelBefore {
							cancel()
						}
					case <-deadline:
						t.Fatal("stream did not finish without waiting for upstream EOF")
					}
				}
				if !wantFailure && !completed {
					t.Fatal("missing completed response")
				}
				if (tc.broken || tc.truncated) && !sawError {
					t.Fatal("missing stream error")
				}
			} else {
				done := make(chan error, 1)
				go func() { _, err := executor.Execute(ctx, auth, req, cliproxyexecutor.Options{}); done <- err }()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					cancel()
					<-done
					t.Fatal("nonstream response waited for upstream EOF")
				}
			}
			// A FIFO sentinel drains all earlier publications, detecting missing and duplicate records.
			usage.PublishRecord(ctx, usage.Record{Alias: t.Name(), Model: "excel-usage-barrier"})
			records := []usage.Record{}
			deadline := time.After(2 * time.Second)
			collecting := true
			for collecting {
				select {
				case record := <-capture.records:
					if record.Model == "excel-usage-barrier" {
						collecting = false
					} else {
						records = append(records, record)
					}
				case <-deadline:
					t.Fatal("usage dispatcher did not deliver barrier")
				}
			}
			if len(records) != 1 {
				t.Fatalf("got %d usage records, want exactly one", len(records))
			}
			record := records[0]
			if record.Provider != "codex" || record.Model != req.Model || record.AuthIndex != auth.Index || record.ExecutorType != "CodexExecutor" {
				t.Fatalf("incorrect attribution: %+v", record)
			}
			if record.Failed != wantFailure {
				t.Fatalf("failed=%v, want %v", record.Failed, wantFailure)
			}
			if !tc.noUsage && !tc.broken && !tc.truncated && !tc.cancelBefore && record.Detail.TotalTokens != 12 {
				t.Fatalf("total tokens=%d, want 12", record.Detail.TotalTokens)
			}
		})
	}
}
