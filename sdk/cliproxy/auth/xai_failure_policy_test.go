package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestManagerMarkResult_DisablesXAIAuthForPermissionDenied(t *testing.T) {
	autoDisable := true
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{XAI: internalconfig.XAIConfig{
		AutoDisablePermissionDenied: &autoDisable,
	}})

	auth := &Auth{
		ID:       "xai-permission-denied",
		Provider: "xai",
		Metadata: map[string]any{"type": "xai"},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reason := `{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.5",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusForbidden,
			Message:    reason,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("auth disabled state = disabled:%t status:%s, want disabled", updated.Disabled, updated.Status)
	}
	if got, _ := updated.Metadata["disabled"].(bool); !got {
		t.Fatalf("metadata disabled = %#v, want true", updated.Metadata["disabled"])
	}
	if got, _ := updated.Metadata["disabled_reason"].(string); got != reason {
		t.Fatalf("disabled_reason = %q, want %q", got, reason)
	}
}

func TestManagerMarkResult_KeepsUnknownXAI403Enabled(t *testing.T) {
	autoDisable := true
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{XAI: internalconfig.XAIConfig{
		AutoDisablePermissionDenied: &autoDisable,
	}})

	auth := &Auth{ID: "xai-unknown-forbidden", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.5",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusForbidden, Message: `{"error":"temporary rejection"}`},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Disabled {
		t.Fatal("unknown xAI 403 must not disable the auth")
	}
}

func TestManagerMarkResult_DisablesXAIAuthForUnauthorized(t *testing.T) {
	autoDisable := true
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{XAI: internalconfig.XAIConfig{
		AutoDisablePermissionDenied: &autoDisable,
	}})

	auth := &Auth{
		ID:       "xai-unauthorized",
		Provider: "xai",
		Metadata: map[string]any{"type": "xai"},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reason := `{"error":"Invalid or expired credentials (auth_kind=bearer)"}`
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.5",
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusUnauthorized,
			Message:    reason,
		},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("auth disabled state = disabled:%t status:%s, want disabled", updated.Disabled, updated.Status)
	}
	if got, _ := updated.Metadata["disabled"].(bool); !got {
		t.Fatalf("metadata disabled = %#v, want true", updated.Metadata["disabled"])
	}
	if got, _ := updated.Metadata["disabled_reason"].(string); got != reason {
		t.Fatalf("disabled_reason = %q, want %q", got, reason)
	}
}

func TestManagerMarkResult_KeepsXAIUnauthorizedWhenAutoDisableOff(t *testing.T) {
	autoDisable := false
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{XAI: internalconfig.XAIConfig{
		AutoDisablePermissionDenied: &autoDisable,
	}})

	auth := &Auth{ID: "xai-unauthorized-off", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.5",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusUnauthorized, Message: "Invalid or expired credentials"},
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("updated auth not found")
	}
	if updated.Disabled {
		t.Fatal("xAI 401 must not disable when auto-disable-permission-denied is false")
	}
}

func freeUsageExhaustedResult(authID, model string, retryAfter time.Duration) Result {
	result := Result{
		AuthID:   authID,
		Provider: "xai",
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for now."}`,
		},
	}
	if retryAfter >= 0 {
		result.RetryAfter = &retryAfter
	}
	return result
}

func otherForbiddenResult(authID, model string, retryAfter time.Duration) Result {
	result := Result{
		AuthID:   authID,
		Provider: "xai",
		Model:    model,
		Success:  false,
		Error: &Error{
			HTTPStatus: http.StatusForbidden,
			Message:    `{"error":"temporary rejection"}`,
		},
	}
	if retryAfter >= 0 {
		result.RetryAfter = &retryAfter
	}
	return result
}

func TestManagerMarkResult_FreeUsageExhaustionDisablesAfterThreshold(t *testing.T) {
	autoDisable := true
	disableAfter := 3
	freeCooldownHours := 24
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		XAI: internalconfig.XAIConfig{
			AutoDisablePermissionDenied:     &autoDisable,
			FreeUsageExhaustedCooldownHours: &freeCooldownHours,
			FreeUsageExhaustedDisableAfter:  &disableAfter,
		},
	})

	auth := &Auth{ID: "xai-free-usage", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// Hit 1: count and cool down.
	manager.MarkResult(context.Background(), freeUsageExhaustedResult(auth.ID, "grok-4.5", time.Hour))
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth missing after first free-usage hit")
	}
	if updated.Disabled {
		t.Fatal("auth must stay enabled after first free-usage hit")
	}
	state := updated.ModelStates["grok-4.5"]
	if state == nil || state.FreeUsageExhaustionCount != 1 {
		t.Fatalf("free usage count after hit 1 = %+v, want 1", state)
	}
	if !state.Unavailable || state.NextRetryAfter.IsZero() {
		t.Fatalf("expected cooldown after free-usage hit, state=%+v", state)
	}

	// Still in cooldown: do not double-count.
	manager.MarkResult(context.Background(), freeUsageExhaustedResult(auth.ID, "grok-4.5", time.Hour))
	updated, _ = manager.GetByID(auth.ID)
	state = updated.ModelStates["grok-4.5"]
	if state.FreeUsageExhaustionCount != 1 {
		t.Fatalf("free usage count during cooldown = %d, want 1", state.FreeUsageExhaustionCount)
	}

	// Expire cooldown and hit again (count 2).
	manager.mu.Lock()
	manager.auths[auth.ID].ModelStates["grok-4.5"].NextRetryAfter = time.Now().Add(-time.Second)
	manager.auths[auth.ID].ModelStates["grok-4.5"].Unavailable = false
	manager.mu.Unlock()
	manager.MarkResult(context.Background(), freeUsageExhaustedResult(auth.ID, "grok-4.5", time.Hour))
	updated, _ = manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("auth must stay enabled after second free-usage hit")
	}
	if got := updated.ModelStates["grok-4.5"].FreeUsageExhaustionCount; got != 2 {
		t.Fatalf("free usage count after hit 2 = %d, want 2", got)
	}

	// Expire cooldown and hit threshold (count 3 → disable).
	manager.mu.Lock()
	manager.auths[auth.ID].ModelStates["grok-4.5"].NextRetryAfter = time.Now().Add(-time.Second)
	manager.auths[auth.ID].ModelStates["grok-4.5"].Unavailable = false
	manager.mu.Unlock()
	manager.MarkResult(context.Background(), freeUsageExhaustedResult(auth.ID, "grok-4.5", time.Hour))
	updated, _ = manager.GetByID(auth.ID)
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("auth should be disabled after threshold, disabled=%t status=%s", updated.Disabled, updated.Status)
	}
	reason, _ := updated.Metadata["disabled_reason"].(string)
	if !strings.Contains(reason, "always exhausted") || !strings.Contains(reason, "counter=3") {
		t.Fatalf("disabled_reason = %q, want always exhausted with counter=3", reason)
	}
}

func TestManagerMarkResult_FreeUsageUsesConfiguredCooldownWithoutRetryAfter(t *testing.T) {
	// Reproduces the mid-stream path: free-usage body is present but RetryAfter was not
	// propagated, so MarkResult used to fall back to the 1s quota backoff ladder and the
	// same high-priority auth was reselected on every chat turn.
	freeCooldownHours := 24
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		XAI: internalconfig.XAIConfig{
			FreeUsageExhaustedCooldownHours: &freeCooldownHours,
		},
	})

	auth := &Auth{ID: "xai-free-usage-no-retry", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	before := time.Now()
	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.5-build-free",
		Success:  false,
		// No RetryAfter — simulates wrapStreamResult before the fix.
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now."}`,
		},
	})
	after := time.Now()

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth missing after free-usage hit")
	}
	state := updated.ModelStates["grok-4.5-build-free"]
	if state == nil {
		t.Fatal("expected model state for free-usage model")
	}
	if !state.Unavailable {
		t.Fatal("expected model unavailable after free-usage")
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("expected NextRetryAfter to be set without provider RetryAfter")
	}
	minNext := before.Add(24 * time.Hour).Add(-2 * time.Second)
	maxNext := after.Add(24 * time.Hour).Add(2 * time.Second)
	if state.NextRetryAfter.Before(minNext) || state.NextRetryAfter.After(maxNext) {
		t.Fatalf("NextRetryAfter = %v, want ~24h from now (between %v and %v)", state.NextRetryAfter, minNext, maxNext)
	}
	if !state.Quota.Exceeded {
		t.Fatal("expected quota exceeded for free-usage")
	}

	// Selector must block this auth for the free-usage model.
	blocked, reason, next := isAuthBlockedForModel(updated, "grok-4.5-build-free", time.Now())
	if !blocked {
		t.Fatal("auth must be blocked for free-usage model after cooldown")
	}
	if reason != blockReasonCooldown {
		t.Fatalf("block reason = %v, want cooldown", reason)
	}
	if next.IsZero() || next.Before(time.Now()) {
		t.Fatalf("block next = %v, want future cooldown time", next)
	}
}

func TestManagerMarkResult_FreeUsageBodyWithoutStatusStillCooldowns(t *testing.T) {
	freeCooldownHours := 24
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		XAI: internalconfig.XAIConfig{
			FreeUsageExhaustedCooldownHours: &freeCooldownHours,
		},
	})
	auth := &Auth{ID: "xai-free-usage-no-status", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.5",
		Success:  false,
		Error: &Error{
			// Status lost on some stream paths; body still identifies free-usage.
			Message: `{"code":"subscription:free-usage-exhausted","error":"included free usage exhausted"}`,
		},
	})

	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["grok-4.5"]
	if state == nil || !state.Unavailable || state.NextRetryAfter.IsZero() {
		t.Fatalf("expected free-usage cooldown from body alone, state=%+v", state)
	}
	if time.Until(state.NextRetryAfter) < 23*time.Hour {
		t.Fatalf("NextRetryAfter too soon: %v (want ~24h)", state.NextRetryAfter)
	}
}

func TestManagerMarkResult_FreeUsageSuccessResetsCounter(t *testing.T) {
	disableAfter := 3
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		XAI: internalconfig.XAIConfig{
			FreeUsageExhaustedDisableAfter: &disableAfter,
		},
	})

	auth := &Auth{ID: "xai-free-reset", Provider: "xai"}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkResult(context.Background(), freeUsageExhaustedResult(auth.ID, "grok-4.5", time.Hour))
	manager.mu.Lock()
	manager.auths[auth.ID].ModelStates["grok-4.5"].NextRetryAfter = time.Now().Add(-time.Second)
	manager.auths[auth.ID].ModelStates["grok-4.5"].Unavailable = false
	manager.mu.Unlock()

	manager.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: "xai",
		Model:    "grok-4.5",
		Success:  true,
	})
	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["grok-4.5"]
	if state == nil || state.FreeUsageExhaustionCount != 0 {
		t.Fatalf("expected free usage counter reset on success, state=%+v", state)
	}
}

func TestManagerMarkResult_Other403ExhaustionDisablesAfterThreshold(t *testing.T) {
	autoDisable := true
	disableAfter := 2
	otherCooldownHours := 6
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		XAI: internalconfig.XAIConfig{
			AutoDisablePermissionDenied: &autoDisable,
			OtherForbiddenCooldownHours: &otherCooldownHours,
			OtherForbiddenDisableAfter:  &disableAfter,
		},
	})

	auth := &Auth{ID: "xai-other-403", Provider: "xai"}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.MarkResult(context.Background(), otherForbiddenResult(auth.ID, "grok-4.5", time.Hour))
	updated, _ := manager.GetByID(auth.ID)
	if updated.Disabled {
		t.Fatal("auth must stay enabled after first other-403 hit")
	}
	if got := updated.ModelStates["grok-4.5"].OtherForbiddenCount; got != 1 {
		t.Fatalf("other 403 count after hit 1 = %d, want 1", got)
	}

	manager.mu.Lock()
	manager.auths[auth.ID].ModelStates["grok-4.5"].NextRetryAfter = time.Now().Add(-time.Second)
	manager.auths[auth.ID].ModelStates["grok-4.5"].Unavailable = false
	manager.mu.Unlock()
	manager.MarkResult(context.Background(), otherForbiddenResult(auth.ID, "grok-4.5", time.Hour))
	updated, _ = manager.GetByID(auth.ID)
	if !updated.Disabled {
		t.Fatal("auth should be disabled after other-403 threshold")
	}
	reason, _ := updated.Metadata["disabled_reason"].(string)
	if !strings.Contains(reason, "other 403 always exhausted") || !strings.Contains(reason, "counter=2") {
		t.Fatalf("disabled_reason = %q, want other 403 always exhausted with counter=2", reason)
	}
}

func TestManagerMarkResult_ExhaustionCounterRunsWithoutSeparateStore(t *testing.T) {
	// Counters always run; runtime is written into auth.Metadata on persist.
	disableAfter := 1
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{
		XAI: internalconfig.XAIConfig{
			FreeUsageExhaustedDisableAfter: &disableAfter,
		},
	})

	auth := &Auth{ID: "xai-auth-runtime", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	manager.MarkResult(context.Background(), freeUsageExhaustedResult(auth.ID, "grok-4.5", time.Hour))
	updated, _ := manager.GetByID(auth.ID)
	if !updated.Disabled {
		t.Fatal("with disable-after=1, free-usage should disable auth on first hit")
	}
	reason, _ := updated.Metadata["disabled_reason"].(string)
	if !strings.Contains(reason, "always exhausted") {
		t.Fatalf("disabled_reason = %q", reason)
	}
}

func TestHydrateAuthRuntimeFromMetadata_RestoresCooldownAndCounters(t *testing.T) {
	until := time.Now().Add(2 * time.Hour).UTC()
	auth := &Auth{
		ID:       "auth-1",
		Provider: "xai",
		Metadata: map[string]any{
			"type": "xai",
			"runtime": map[string]any{
				"models": map[string]any{
					"grok-4": map[string]any{
						"cooldown_until":  until.Format(time.RFC3339Nano),
						"reason":          "free_usage_exhausted",
						"http_status":     429,
						"last_response":   "free usage exhausted",
						"free_usage_hits": 2,
					},
				},
			},
		},
	}
	hydrateAuthRuntimeFromMetadata(auth, time.Now())
	state := auth.ModelStates["grok-4"]
	if state == nil {
		t.Fatal("expected model state")
	}
	if state.FreeUsageExhaustionCount != 2 {
		t.Fatalf("free usage hits = %d, want 2", state.FreeUsageExhaustionCount)
	}
	if !state.Unavailable || state.NextRetryAfter.IsZero() {
		t.Fatalf("expected cooling state, got %+v", state)
	}
	if time.Until(state.NextRetryAfter) < time.Hour {
		t.Fatalf("cooldown too short: %v", state.NextRetryAfter)
	}

	// Round-trip into metadata map.
	syncAuthRuntimeMetadata(auth, time.Now())
	runtime, ok := auth.Metadata["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("runtime missing after sync: %#v", auth.Metadata["runtime"])
	}
	models, ok := runtime["models"].(map[string]any)
	if !ok {
		t.Fatalf("models missing: %#v", runtime)
	}
	entry, ok := models["grok-4"].(map[string]any)
	if !ok {
		t.Fatalf("model entry missing: %#v", models)
	}
	if entry["free_usage_hits"] != 2 {
		t.Fatalf("free_usage_hits = %#v, want 2", entry["free_usage_hits"])
	}
	if _, ok := entry["cooldown_until"]; !ok {
		t.Fatal("cooldown_until missing in runtime entry")
	}
}

func TestRegisterHydratesRuntimeFromAuthMetadata(t *testing.T) {
	until := time.Now().Add(3 * time.Hour).UTC()
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-hydrate",
		Provider: "xai",
		Metadata: map[string]any{
			"type": "xai",
			"runtime": map[string]any{
				"models": map[string]any{
					"grok-4.5": map[string]any{
						"cooldown_until": until.Format(time.RFC3339Nano),
						"reason":         "quota",
						"http_status":    429,
					},
				},
			},
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := manager.GetByID("auth-hydrate")
	if !ok {
		t.Fatal("auth missing")
	}
	state := got.ModelStates["grok-4.5"]
	if state == nil || !state.Unavailable {
		t.Fatalf("expected hydrated cooldown, state=%+v", state)
	}
	blocked, reason, next := isAuthBlockedForModel(got, "grok-4.5", time.Now())
	if !blocked || reason != blockReasonCooldown || next.Before(time.Now()) {
		t.Fatalf("blocked=%v reason=%v next=%v", blocked, reason, next)
	}
}
