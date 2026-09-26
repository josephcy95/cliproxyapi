package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestServerStopPreservesGracefulDrain(t *testing.T) {
	for _, expire := range []bool{false, true} {
		name := "request_completes"
		if expire {
			name = "deadline_expires"
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				_, _ = io.WriteString(w, "completed")
			})}
			defer httpServer.Close()
			server := &Server{server: httpServer}
			go func() { _ = httpServer.Serve(listener) }()
			done := make(chan error, 1)
			go func() {
				resp, err := http.Get("http://" + listener.Addr().String())
				if err == nil {
					var body []byte
					body, err = io.ReadAll(resp.Body)
					resp.Body.Close()
					if err == nil && string(body) != "completed" {
						err = errors.New("response truncated")
					}
				}
				done <- err
			}()
			<-started
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if expire {
				cancel()
			}
			stopped := make(chan error, 1)
			go func() { stopped <- server.Stop(ctx) }()
			if expire {
				select {
				case err := <-stopped:
					if err == nil {
						t.Fatal("expired drain context returned nil")
					}
				case <-time.After(time.Second):
					t.Fatal("expired stop did not return")
				}
			} else {
				select {
				case err := <-stopped:
					t.Fatalf("stop interrupted active request: %v", err)
				case <-time.After(40 * time.Millisecond):
				}
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("active request did not drain")
			}
			if !expire {
				select {
				case err := <-stopped:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("shutdown did not finish after drain")
				}
			}
		})
	}
}

func TestWriteModelListResponseWithoutHandlersPreservesCompactJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	payload := []byte(`{"models":[{"slug":"test"}]}`)
	(&Server{}).writeModelListResponse(ctx, "openai", payload)
	if recorder.Code != http.StatusOK || recorder.Body.String() != string(payload) {
		t.Fatalf("compact JSON encoded incorrectly: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
