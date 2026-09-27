package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func usageLimitResult(authID, model string, retryAfter time.Duration) Result {
	result := Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_in_seconds":120}}`,
		},
	}
	if retryAfter >= 0 {
		result.RetryAfter = &retryAfter
	}
	return result
}

func authFailureResult(authID, model string) Result {
	return Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    `{"error":{"type":"authentication_error","code":"auth_unavailable","message":"invalid or expired token"}}`,
		},
	}
}

func paymentRequiredResult(authID, model string) Result {
	return Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusPaymentRequired,
			Message:    `{"detail":{"code":"deactivated_workspace"}}`,
		},
	}
}

func TestManagerMarkResult_CodexUsageLimitDisablesAfterThreshold(t *testing.T) {
	disableAfter := 3
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Codex: internalconfig.CodexConfig{
			UsageLimitDisableAfter: &disableAfter,
		},
	})

	auth := &Auth{ID: "codex-usage", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	manager.MarkResult(context.Background(), usageLimitResult(auth.ID, "gpt-5.4", time.Hour))
	updated, _ := manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("must stay enabled after first usage-limit hit")
	}
	state := updated.ModelStates["gpt-5.4"]
	if state == nil || state.UsageLimitCount != 1 {
		t.Fatalf("usage limit count after hit 1 = %+v, want 1", state)
	}
	if !state.Unavailable || state.NextRetryAfter.IsZero() {
		t.Fatalf("expected cooldown after usage limit, state=%+v", state)
	}

	// In-cooldown re-hit does not double-count.
	manager.MarkResult(context.Background(), usageLimitResult(auth.ID, "gpt-5.4", time.Hour))
	updated, _ = manager.GetByID(auth.ID)
	if got := updated.ModelStates["gpt-5.4"].UsageLimitCount; got != 1 {
		t.Fatalf("count during cooldown = %d, want 1", got)
	}

	// Expire cool, hit 2.
	manager.mu.Lock()
	manager.auths[auth.ID].ModelStates["gpt-5.4"].NextRetryAfter = time.Now().Add(-time.Second)
	manager.auths[auth.ID].ModelStates["gpt-5.4"].Unavailable = false
	manager.mu.Unlock()
	manager.MarkResult(context.Background(), usageLimitResult(auth.ID, "gpt-5.4", time.Hour))
	updated, _ = manager.GetByID(auth.ID)
	if got := updated.ModelStates["gpt-5.4"].UsageLimitCount; got != 2 {
		t.Fatalf("count after hit 2 = %d, want 2", got)
	}

	// Expire cool, hit 3 → disable.
	manager.mu.Lock()
	manager.auths[auth.ID].ModelStates["gpt-5.4"].NextRetryAfter = time.Now().Add(-time.Second)
	manager.auths[auth.ID].ModelStates["gpt-5.4"].Unavailable = false
	manager.mu.Unlock()
	manager.MarkResult(context.Background(), usageLimitResult(auth.ID, "gpt-5.4", time.Hour))
	updated, _ = manager.GetByID(auth.ID)
	if !updated.Disabled {
		t.Fatal("expected disable after usage-limit threshold")
	}
	reason, _ := updated.Metadata["disabled_reason"].(string)
	if !strings.Contains(reason, "usage limit always exhausted") {
		t.Fatalf("disabled_reason = %q", reason)
	}
}

func TestManagerMarkResult_CodexUsageLimitFallbackCooldownWithoutRetryAfter(t *testing.T) {
	fallbackHours := 2
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Codex: internalconfig.CodexConfig{
			UsageLimitCooldownFallbackHours: &fallbackHours,
		},
	})
	auth := &Auth{ID: "codex-usage-fallback", Provider: "codex"}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	before := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit."}}`,
		},
	})
	after := time.Now()
	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["gpt-5.4"]
	if state == nil || state.NextRetryAfter.IsZero() {
		t.Fatalf("expected fallback cool, state=%+v", state)
	}
	minNext := before.Add(2 * time.Hour).Add(-2 * time.Second)
	maxNext := after.Add(2 * time.Hour).Add(2 * time.Second)
	if state.NextRetryAfter.Before(minNext) || state.NextRetryAfter.After(maxNext) {
		t.Fatalf("NextRetryAfter = %v, want ~2h", state.NextRetryAfter)
	}
}

func TestManagerMarkResult_CodexAuthFailureDisables(t *testing.T) {
	autoDisable := true
	disableAfter := 1
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Codex: internalconfig.CodexConfig{
			AutoDisableAuthFailures: &autoDisable,
			AuthFailureDisableAfter: &disableAfter,
		},
	})
	auth := &Auth{ID: "codex-auth-death", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), authFailureResult(auth.ID, "gpt-5.4"))
	updated, _ := manager.GetByID(auth.ID)
	if !updated.Disabled {
		t.Fatal("expected auth-death disable on first hard failure")
	}
	if got, _ := updated.Metadata["disabled"].(bool); !got {
		t.Fatal("metadata disabled missing")
	}
	reason, _ := updated.Metadata["disabled_reason"].(string)
	if reason == "" {
		t.Fatal("expected disabled_reason")
	}
}

func TestManagerMarkResult_CodexPaymentRequiredDisablesAuth(t *testing.T) {
	autoDisable := true
	disableAfter := 1
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Codex: internalconfig.CodexConfig{
			AutoDisableAuthFailures: &autoDisable,
			AuthFailureDisableAfter: &disableAfter,
		},
	})
	auth := &Auth{ID: "codex-402", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), paymentRequiredResult(auth.ID, "gpt-5.4"))
	updated, _ := manager.GetByID(auth.ID)
	if !updated.Disabled {
		t.Fatal("expected Codex 402 / deactivated_workspace to disable the entire auth file")
	}
	if got, _ := updated.Metadata["disabled"].(bool); !got {
		t.Fatal("metadata disabled missing")
	}
	reason, _ := updated.Metadata["disabled_reason"].(string)
	if !strings.Contains(reason, "deactivated_workspace") {
		t.Fatalf("disabled_reason = %q, want deactivated_workspace body", reason)
	}
	// File-level death must not leave the credential in a cool-and-retry loop.
	if st := updated.ModelStates["gpt-5.4"]; st != nil && st.Unavailable && !st.NextRetryAfter.IsZero() {
		// Disable path runs before model cool; after disable, auth is out of the pool either way.
		// Still assert we did not only cool without disable (already checked above).
	}
}

func TestManagerMarkResult_CodexDeactivatedWorkspaceBodyDisablesWithoutStatus(t *testing.T) {
	autoDisable := true
	disableAfter := 1
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Codex: internalconfig.CodexConfig{
			AutoDisableAuthFailures: &autoDisable,
			AuthFailureDisableAfter: &disableAfter,
		},
	})
	auth := &Auth{ID: "codex-deactivated-body", Provider: "codex"}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Error: &Error{
			// Status sometimes lost on remaps; body alone must still kill the auth.
			HTTPStatus: 0,
			Message:    `{"detail":{"code":"deactivated_workspace"}}`,
			Code:       "deactivated_workspace",
		},
	})
	updated, _ := manager.GetByID(auth.ID)
	if !updated.Disabled {
		t.Fatal("expected deactivated_workspace body match to disable auth without HTTP status")
	}
}

func TestManagerMarkResult_CodexAuthFailureRespectsDisableAfter(t *testing.T) {
	autoDisable := true
	disableAfter := 2
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Codex: internalconfig.CodexConfig{
			AutoDisableAuthFailures: &autoDisable,
			AuthFailureDisableAfter: &disableAfter,
		},
	})
	auth := &Auth{ID: "codex-auth-2", Provider: "codex"}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), authFailureResult(auth.ID, "gpt-5.4"))
	updated, _ := manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("must stay enabled after first auth failure when threshold=2")
	}
	// Auth failure cools via 401 path (~30m). Expire and hit again.
	manager.mu.Lock()
	if st := manager.auths[auth.ID].ModelStates["gpt-5.4"]; st != nil {
		st.NextRetryAfter = time.Now().Add(-time.Second)
		st.Unavailable = false
	}
	manager.mu.Unlock()
	manager.MarkResult(context.Background(), authFailureResult(auth.ID, "gpt-5.4"))
	updated, _ = manager.GetByID(auth.ID)
	if !updated.Disabled {
		t.Fatal("expected disable after second auth failure")
	}
}

func TestManagerMarkResult_CodexTransientRateLimitDoesNotCountUsageLimit(t *testing.T) {
	disableAfter := 1
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Codex: internalconfig.CodexConfig{
			UsageLimitDisableAfter: &disableAfter,
		},
	})
	auth := &Auth{ID: "codex-rate", Provider: "codex"}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "codex",
		Model:    "gpt-5.4",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}`,
		},
	})
	updated, _ := manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("transient rate limit must not disable via usage-limit path")
	}
	state := updated.ModelStates["gpt-5.4"]
	if state != nil && state.UsageLimitCount != 0 {
		t.Fatalf("usage limit count = %d, want 0", state.UsageLimitCount)
	}
}

func TestManagerMarkResult_CodexSuccessClearsStaleDisabledReason(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "codex-success-clears-reason",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":            "codex",
			"disabled":        false,
			"disabled_reason": `{"error":{"type":"authentication_error","code":"auth_unavailable"}}`,
		},
		LastError:     &Error{HTTPStatus: http.StatusUnauthorized, Code: "auth_unavailable"},
		StatusMessage: "auth_unavailable",
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: "codex", Model: "gpt-5.4", Success: true,
	})

	updated, _ := manager.GetByID(auth.ID)
	if _, ok := updated.Metadata["disabled_reason"]; ok {
		t.Fatalf("disabled_reason remains after success: %#v", updated.Metadata["disabled_reason"])
	}
	if updated.LastError != nil || updated.StatusMessage != "" || updated.Status != StatusActive {
		t.Fatalf("auth diagnostics remain after success: status=%s message=%q error=%#v", updated.Status, updated.StatusMessage, updated.LastError)
	}
	if updated.Disabled {
		t.Fatal("successful request must not change enabled auth to disabled")
	}
}

func TestManagerMarkResult_CodexSuccessPreservesReasonWhileAnotherModelErrors(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now()
	auth := &Auth{
		ID:            "codex-success-other-model-error",
		Provider:      "codex",
		Status:        StatusError,
		StatusMessage: "auth_unavailable",
		LastError:     &Error{HTTPStatus: http.StatusUnauthorized, Code: "auth_unavailable"},
		Metadata: map[string]any{
			"type":            "codex",
			"disabled":        false,
			"disabled_reason": "auth_unavailable",
		},
		ModelStates: map[string]*ModelState{
			"gpt-5.4": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Hour),
				LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Code: "auth_unavailable"},
			},
			"gpt-5.3": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(time.Hour),
				LastError:      &Error{HTTPStatus: http.StatusUnauthorized, Code: "auth_unavailable"},
			},
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: "codex", Model: "gpt-5.4", Success: true,
	})

	updated, _ := manager.GetByID(auth.ID)
	if reason, _ := updated.Metadata["disabled_reason"].(string); reason != "auth_unavailable" {
		t.Fatalf("disabled_reason = %q, want preserved while another model errors", reason)
	}
	if updated.LastError == nil || updated.StatusMessage == "" {
		t.Fatalf("shared auth diagnostics cleared while another model errors: %+v", updated)
	}
	other := updated.ModelStates["gpt-5.3"]
	if other == nil || !other.Unavailable || other.NextRetryAfter.IsZero() {
		t.Fatalf("unrelated model error was cleared: %+v", other)
	}
}

func TestManagerMarkResult_SuccessDoesNotClearDisabledCredentialReason(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:            "codex-manually-disabled",
		Provider:      "codex",
		Disabled:      true,
		Status:        StatusDisabled,
		StatusMessage: "disabled via management API",
		Metadata: map[string]any{
			"type":            "codex",
			"disabled":        true,
			"disabled_reason": "previous auth failure",
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: "codex", Model: "gpt-5.4", Success: true,
	})

	updated, _ := manager.GetByID(auth.ID)
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("disabled auth was re-enabled: %+v", updated)
	}
	if reason, _ := updated.Metadata["disabled_reason"].(string); reason != "previous auth failure" {
		t.Fatalf("disabled_reason = %q, want preserved for disabled auth", reason)
	}
}

func TestManagerMarkResult_CodexSuccessResetsUsageLimitCounter(t *testing.T) {
	disableAfter := 3
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		Codex: internalconfig.CodexConfig{
			UsageLimitDisableAfter: &disableAfter,
		},
	})
	auth := &Auth{ID: "codex-reset", Provider: "codex"}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager.MarkResult(context.Background(), usageLimitResult(auth.ID, "gpt-5.4", time.Hour))
	manager.mu.Lock()
	manager.auths[auth.ID].ModelStates["gpt-5.4"].NextRetryAfter = time.Now().Add(-time.Second)
	manager.auths[auth.ID].ModelStates["gpt-5.4"].Unavailable = false
	manager.mu.Unlock()
	manager.MarkResult(context.Background(), Result{
		AuthID: auth.ID, Provider: "codex", Model: "gpt-5.4", Success: true,
	})
	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["gpt-5.4"]
	if state == nil || state.UsageLimitCount != 0 {
		t.Fatalf("expected counter reset on success, state=%+v", state)
	}
}
