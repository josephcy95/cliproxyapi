package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBuildModelSourceCandidateForModelUsesActiveModelCooldown(t *testing.T) {
	now := time.Now()
	auth := &coreauth.Auth{
		ID:       "model-state-auth",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		ModelStates: map[string]*coreauth.ModelState{
			"gpt-test": {
				Status:         coreauth.StatusActive,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Hour),
			},
		},
	}

	candidate := buildModelSourceCandidateForModel(auth, "gpt-test")
	if !candidate.Unavailable || candidate.Reason != "quota exhausted" {
		t.Fatalf("candidate = %#v, want unavailable model-state reason", candidate)
	}
}

func TestGetModelSources_OrdersByPriorityAndKeyPriority(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := registry.GetGlobalRegistry()

	// Isolate registry clients for this test.
	const highID = "test-model-sources-high"
	const lowID = "test-model-sources-low"
	const midID = "test-model-sources-mid"
	modelID := "test-model-sources-alias"

	reg.UnregisterClient(highID)
	reg.UnregisterClient(lowID)
	reg.UnregisterClient(midID)
	t.Cleanup(func() {
		reg.UnregisterClient(highID)
		reg.UnregisterClient(lowID)
		reg.UnregisterClient(midID)
	})

	info := &registry.ModelInfo{ID: modelID, Object: "model", OwnedBy: "test"}
	reg.RegisterClient(highID, "openai-compatible-demo", []*registry.ModelInfo{info})
	reg.RegisterClient(midID, "openai-compatible-demo", []*registry.ModelInfo{info})
	reg.RegisterClient(lowID, "claude", []*registry.ModelInfo{info})

	manager := coreauth.NewManager(nil, nil, nil)
	_, _ = manager.Register(nil, &coreauth.Auth{
		ID:       lowID,
		Provider: "claude",
		Label:    "claude-low",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"priority": "10",
		},
	})
	_, _ = manager.Register(nil, &coreauth.Auth{
		ID:       midID,
		Provider: "openai-compatible-demo",
		Label:    "demo-mid",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"priority":     "14",
			"key_priority": "1",
			"base_url":     "https://demo.example/v1",
		},
	})
	_, _ = manager.Register(nil, &coreauth.Auth{
		ID:       highID,
		Provider: "openai-compatible-demo",
		Label:    "demo-high",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"priority":     "14",
			"key_priority": "2",
			"base_url":     "https://demo.example/v1",
		},
	})

	h := &Handler{authManager: manager}
	router := gin.New()
	router.GET("/model-sources", h.GetModelSources)

	req := httptest.NewRequest(http.MethodGet, "/model-sources", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var body struct {
		Models map[string][]modelSourceCandidate `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sources := body.Models[modelID]
	if len(sources) != 3 {
		t.Fatalf("sources len = %d, want 3: %#v", len(sources), sources)
	}
	if sources[0].Label != "demo-high" || !sources[0].Preferred {
		t.Fatalf("first source = %#v, want preferred demo-high", sources[0])
	}
	if sources[0].Priority != 14 || sources[0].KeyPriority != 2 {
		t.Fatalf("first priority fields = %#v", sources[0])
	}
	if sources[1].Label != "demo-mid" || sources[1].KeyPriority != 1 {
		t.Fatalf("second source = %#v, want demo-mid key_priority 1", sources[1])
	}
	if sources[2].Label != "claude-low" || sources[2].Priority != 10 {
		t.Fatalf("third source = %#v, want claude-low priority 10", sources[2])
	}
	if sources[1].Preferred || sources[2].Preferred {
		t.Fatalf("only first ready source should be preferred: %#v", sources)
	}
}
