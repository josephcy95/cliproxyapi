package auth

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestManagerMarkResult_DisablesXAI401WithEmptyConfigDefaults(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{}) // xai block omitted

	auth := &Auth{ID: "xai-empty-cfg", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: "xai", Model: "grok-4.5", Success: false,
		Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "Invalid or expired credentials"},
	})
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if !updated.Disabled {
		t.Fatalf("empty Config.XAI must default auto-disable=true; disabled=%v status=%s", updated.Disabled, updated.Status)
	}
}

func TestManagerMarkResult_DisablesXAI401WithoutSetConfig(t *testing.T) {
	manager := NewManager(nil, nil, nil) // only NewManager empty Config{}
	auth := &Auth{ID: "xai-no-setconfig", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: "xai", Model: "grok-4.5", Success: false,
		Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "Invalid or expired credentials"},
	})
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if !updated.Disabled {
		t.Fatalf("NewManager empty Config must default auto-disable=true; disabled=%v", updated.Disabled)
	}
}
