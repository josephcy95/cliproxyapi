package cliproxy

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestServiceModelReregistrationPreservesPersistedCooldown(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	authID := "service-xai-cooldown"
	model := "grok-4.5"
	until := time.Now().Add(24 * time.Hour)
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(authID) })

	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "xai",
		Metadata: map[string]any{
			"type": "xai",
			"runtime": map[string]any{
				"models": map[string]any{
					model: map[string]any{
						"cooldown_until":  until.Format(time.RFC3339Nano),
						"reason":          "quota",
						"http_status":     429,
						"free_usage_hits": 1,
					},
				},
			},
		},
	}
	prepared := service.prepareCoreAuthForModelRegistration(context.Background(), auth)
	if prepared == nil {
		t.Fatal("prepared auth is nil")
	}
	service.completeModelRegistrationForAuth(context.Background(), prepared)

	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("registered auth missing")
	}
	state := updated.ModelStates[model]
	if state == nil || !state.Unavailable || !state.NextRetryAfter.After(time.Now()) || state.FreeUsageExhaustionCount != 1 {
		t.Fatalf("service re-registration erased persisted cooldown: %+v", state)
	}
}
