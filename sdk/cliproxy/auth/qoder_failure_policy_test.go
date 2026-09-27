package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestRecoverQoderQuotaProbeDisable(t *testing.T) {
	now := time.Now()
	auth := &Auth{
		Provider: "qodercn",
		Disabled: true,
		Status:   StatusDisabled,
		LastError: &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"code":"401","message":"token is not active"}`,
		},
		Metadata: map[string]any{
			"disabled":        true,
			"disabled_reason": `{"code":"401","message":"token is not active"}`,
		},
	}

	if !recoverQoderQuotaProbeDisable(auth, now) {
		t.Fatal("expected v7.2.109 quota-probe disable to be recovered")
	}
	if auth.Disabled || auth.Status != StatusActive || auth.LastError != nil {
		t.Fatalf("recovered auth = disabled:%t status:%s error:%#v", auth.Disabled, auth.Status, auth.LastError)
	}
	if disabled, _ := auth.Metadata["disabled"].(bool); disabled {
		t.Fatalf("metadata disabled = %#v, want false", auth.Metadata["disabled"])
	}
	if _, ok := auth.Metadata["disabled_reason"]; ok {
		t.Fatalf("disabled_reason = %#v, want removed", auth.Metadata["disabled_reason"])
	}
}

func TestRecoverQoderQuotaProbeDisableKeepsExecutionFailureDisabled(t *testing.T) {
	auth := &Auth{
		Provider: "qoder",
		Disabled: true,
		Status:   StatusDisabled,
		Metadata: map[string]any{
			"disabled":        true,
			"disabled_reason": `Qoder API error 401: {"code":"401","message":"token is not active"}`,
		},
	}

	if recoverQoderQuotaProbeDisable(auth, time.Now()) {
		t.Fatal("execution-path inactive-token failure must remain disabled")
	}
	if !auth.Disabled || auth.Status != StatusDisabled {
		t.Fatalf("auth disabled state = disabled:%t status:%s, want disabled", auth.Disabled, auth.Status)
	}
}

func TestManagerMarkResult_DisablesQoderInactiveToken(t *testing.T) {
	autoDisable := true
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Qoder: internalconfig.QoderConfig{
		AutoDisableInactiveToken: &autoDisable,
	}})

	auth := &Auth{ID: "qodercn-inactive-token", Provider: "qodercn", Metadata: map[string]any{"type": "qodercn"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reason := "Failed to load quota: 401 token is not active"
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: reason},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("auth disabled state = disabled:%t status:%s, want disabled", updated.Disabled, updated.Status)
	}
	if got, _ := updated.Metadata["disabled_reason"].(string); got != reason {
		t.Fatalf("disabled_reason = %q, want %q", got, reason)
	}
}

func TestManagerMarkResult_KeepsOtherQoder401Enabled(t *testing.T) {
	autoDisable := true
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Qoder: internalconfig.QoderConfig{
		AutoDisableInactiveToken: &autoDisable,
	}})

	auth := &Auth{ID: "qoder-unknown-401", Provider: "qoder", Metadata: map[string]any{"type": "qoder"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "qmodel_preview",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "temporary authorization gateway error"},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Disabled {
		t.Fatal("unclassified Qoder 401 must not disable the auth")
	}
}

func TestManagerMarkResult_CoolsQueuedQoder403WithoutDisablingAuth(t *testing.T) {
	cooldownMinutes := 5
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Qoder: internalconfig.QoderConfig{
		QueuedForbiddenCooldownMinutes: &cooldownMinutes,
	}})

	auth := &Auth{ID: "qodercn-queued", Provider: "qodercn", Metadata: map[string]any{"type": "qodercn"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    "qmodel_preview",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusForbidden, Message: `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true,\\\"modelKey\\\":\\\"qmodel_preview\\\"}\"}"}`},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Disabled {
		t.Fatal("queued Qoder 403 must not disable the auth")
	}
	state := updated.ModelStates["qmodel_preview"]
	if state == nil || !state.Unavailable {
		t.Fatalf("queued Qoder model state = %#v, want unavailable", state)
	}
	remaining := time.Until(state.NextRetryAfter)
	if remaining < 4*time.Minute || remaining > 6*time.Minute {
		t.Fatalf("queued Qoder cooldown remaining = %v, want about 5m", remaining)
	}
}

func TestManagerMarkResult_KeepsInactiveQoderTokenWhenAutoDisableOff(t *testing.T) {
	autoDisable := false
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Qoder: internalconfig.QoderConfig{
		AutoDisableInactiveToken: &autoDisable,
	}})

	auth := &Auth{ID: "qoder-inactive-token-off", Provider: "qoder", Metadata: map[string]any{"type": "qoder"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "401 token is not active"},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Disabled {
		t.Fatal("inactive Qoder token must not disable when auto-disable-inactive-token is false")
	}
}
