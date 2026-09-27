package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestGetAuthFileModelsShowsQoderCatalogForDisabledAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "qodercn-disabled",
		FileName: "qodercn-disabled.json",
		Provider: "qodercn",
		Disabled: true,
		Status:   coreauth.StatusDisabled,
		Metadata: map[string]any{"type": "qodercn", "disabled": true},
	}
	if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodGet,
		"/v0/management/auth-file-models?name=qodercn-disabled.json",
		nil,
	)

	h.GetAuthFileModels(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Models) == 0 {
		t.Fatalf("models = %#v, want static Qoder CN catalog", payload.Models)
	}
	if payload.Models[0].ID != "qodercn/auto" {
		t.Fatalf("first model = %q, want qodercn/auto", payload.Models[0].ID)
	}
}
