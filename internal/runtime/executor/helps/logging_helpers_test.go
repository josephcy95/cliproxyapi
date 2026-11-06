package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestRecordAPIRequestClonesDeferredBodyWhenRequestLogDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	body := []byte(`{"model":"original"}`)

	RecordAPIRequest(ctx, &config.Config{}, UpstreamRequestLog{
		URL:    "https://api.example.com/v1/responses",
		Method: http.MethodPost,
		Body:   body,
	})
	body[10] = 'X'

	value, exists := ginCtx.Get(logging.DeferredAPIRequestContextKey)
	if !exists {
		t.Fatal("deferred API request was not captured")
	}
	requests, ok := value.([]logging.DeferredAPIRequest)
	if !ok || len(requests) != 1 {
		t.Fatalf("deferred API requests = %#v, want one request", value)
	}
	captured := string(requests[0]())
	if !strings.Contains(captured, `{"model":"original"}`) {
		t.Fatalf("captured API request = %q, want original body", captured)
	}
}

func TestRecordAPIRequestDeferredCapturePolicy(t *testing.T) {
	for _, tt := range []struct {
		name      string
		disabled  bool
		fullLog   bool
		wantDefer bool
	}{
		{name: "HTTP with upgrade headers", wantDefer: true},
		{name: "established WebSocket", disabled: true},
		{name: "WebSocket with full logging", disabled: true, fullLog: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ginCtx.Request.Header.Set("Connection", "Upgrade")
			ginCtx.Request.Header.Set("Upgrade", "websocket")
			ctx := context.WithValue(context.Background(), "gin", ginCtx)
			if tt.disabled {
				ctx = logging.WithDeferredAPIRequestCaptureDisabled(ctx)
			}
			cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: tt.fullLog}}
			RecordAPIRequest(ctx, cfg, UpstreamRequestLog{Method: http.MethodPost, Body: []byte("request-payload")})
			value, exists := ginCtx.Get(logging.DeferredAPIRequestContextKey)
			if exists != tt.wantDefer {
				t.Fatalf("deferred capture present = %t, want %t", exists, tt.wantDefer)
			}
			if tt.wantDefer {
				requests, _ := value.([]logging.DeferredAPIRequest)
				if len(requests) != 1 || !strings.Contains(string(requests[0]()), "request-payload") {
					t.Fatal("ordinary HTTP lost its error-only request body")
				}
			}
			if tt.fullLog {
				logged, _ := ginCtx.Get(apiRequestKey)
				if body, _ := logged.([]byte); !strings.Contains(string(body), "request-payload") {
					t.Fatal("full logging lost the upstream request body")
				}
			}
		})
	}
}

func TestRecordAPIRequestDisabledCaptureDoesNotRetainLargeTurns(t *testing.T) {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := logging.WithDeferredAPIRequestCaptureDisabled(context.WithValue(context.Background(), "gin", ginCtx))
	body := []byte(strings.Repeat("x", 1<<20))
	for range 40 {
		RecordAPIRequest(ctx, &config.Config{}, UpstreamRequestLog{Body: body})
	}
	for _, key := range []string{logging.DeferredAPIRequestContextKey, deferredAPIRequestBytesKey} {
		if _, exists := ginCtx.Get(key); exists {
			t.Fatalf("disabled capture retained %s after 40 MiB of upstream requests", key)
		}
	}
}

func TestRecordAPIRequestAsyncCapturePolicySurvivesGinContextReuse(t *testing.T) {
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	parent := context.WithValue(context.Background(), "gin", ginCtx)
	socketCtx, cancel := context.WithCancel(logging.WithDeferredAPIRequestCaptureDisabled(parent))
	cancel()
	callbackCtx := context.WithoutCancel(socketCtx)

	// A completed socket can leave asynchronous plugin callbacks holding the
	// same Gin pointer that is now serving an unrelated HTTP request.
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	RecordAPIRequest(parent, &config.Config{}, UpstreamRequestLog{Body: []byte("new-http-request")})
	var callbacks sync.WaitGroup
	for range 8 {
		callbacks.Go(func() {
			RecordAPIRequest(callbackCtx, &config.Config{}, UpstreamRequestLog{Body: []byte("old-websocket-request")})
		})
	}
	callbacks.Wait()
	value, _ := ginCtx.Get(logging.DeferredAPIRequestContextKey)
	requests, _ := value.([]logging.DeferredAPIRequest)
	if len(requests) != 1 || !strings.Contains(string(requests[0]()), "new-http-request") {
		t.Fatal("old WebSocket callbacks changed the new HTTP request's deferred log")
	}
	used, _ := ginCtx.Get(deferredAPIRequestBytesKey)
	if used != len("new-http-request") {
		t.Fatalf("deferred bytes = %v, want only the new HTTP body", used)
	}
}

func TestRecordAPIResponseMetadataStoresHeadersWhenRequestLogDisabled(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	headers := http.Header{}
	headers.Add("X-Upstream-Request-Id", "upstream-req-1")

	RecordAPIResponseMetadata(ctx, &config.Config{}, http.StatusOK, headers)
	headers.Set("X-Upstream-Request-Id", "mutated")

	got := logging.GetResponseHeaders(ctx)
	if got.Get("X-Upstream-Request-Id") != "upstream-req-1" {
		t.Fatalf("response header = %q, want %q", got.Get("X-Upstream-Request-Id"), "upstream-req-1")
	}
}
