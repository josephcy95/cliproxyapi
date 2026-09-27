package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func cooldownQuotaResult(authID, provider, model, message string, cooldown time.Duration) Result {
	return Result{
		AuthID:     authID,
		Provider:   provider,
		Model:      model,
		Success:    false,
		RetryAfter: &cooldown,
		Error: &Error{
			HTTPStatus: http.StatusTooManyRequests,
			Message:    message,
		},
	}
}

func TestManagerReconcileRegistryModelStatesPreservesActiveCooldowns(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		message  string
		counter  func(*ModelState) int
	}{
		{
			name:     "xai",
			provider: "xai",
			model:    "grok-4.5",
			message:  `{"code":"subscription:free-usage-exhausted","error":"included free usage"}`,
			counter:  func(state *ModelState) int { return state.FreeUsageExhaustionCount },
		},
		{
			name:     "codex",
			provider: "codex",
			model:    "gpt-5.4",
			message:  `{"error":{"type":"usage_limit_reached","message":"usage limit reached"}}`,
			counter:  func(state *ModelState) int { return state.UsageLimitCount },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{ID: "reconcile-" + tt.name, Provider: tt.provider, Metadata: map[string]any{"type": tt.provider}}
			if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}

			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: tt.model}})
			t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
			manager.RefreshSchedulerEntry(auth.ID)
			manager.MarkResult(WithSkipPersist(context.Background()), cooldownQuotaResult(auth.ID, tt.provider, tt.model, tt.message, 24*time.Hour))

			manager.ReconcileRegistryModelStates(WithSkipPersist(context.Background()), auth.ID)

			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("updated auth missing")
			}
			state := updated.ModelStates[tt.model]
			if state == nil || !state.Unavailable || !state.NextRetryAfter.After(time.Now()) {
				t.Fatalf("active cooldown was cleared by reconciliation: %+v", state)
			}
			if got := tt.counter(state); got != 1 {
				t.Fatalf("exhaustion counter = %d, want 1", got)
			}
			if picked, err := manager.scheduler.pickSingle(context.Background(), tt.provider, tt.model, cliproxyexecutor.Options{}, nil); picked != nil || err == nil {
				t.Fatalf("cooled auth became scheduler-eligible: picked=%v err=%v", picked, err)
			}
		})
	}
}

func TestManagerReconcileRegistryModelStatesKeepsStateWhenRegistrySnapshotIsEmpty(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	next := time.Now().Add(24 * time.Hour)
	auth := &Auth{
		ID:       "reconcile-empty-registry",
		Provider: "xai",
		Metadata: map[string]any{"type": "xai"},
		ModelStates: map[string]*ModelState{
			"grok-4.5": {
				Status:                   StatusError,
				Unavailable:              true,
				NextRetryAfter:           next,
				FreeUsageExhaustionCount: 1,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
				},
			},
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	manager.ReconcileRegistryModelStates(WithSkipPersist(context.Background()), auth.ID)

	updated, _ := manager.GetByID(auth.ID)
	if state := updated.ModelStates["grok-4.5"]; state == nil || !state.Unavailable || state.FreeUsageExhaustionCount != 1 {
		t.Fatalf("empty registry snapshot erased runtime state: %+v", state)
	}
}

func TestManagerUpdateHydratesAndMergesPersistedRuntimeCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now()
	existingUntil := now.Add(24 * time.Hour)
	auth := &Auth{
		ID:       "update-runtime-cooldown",
		Provider: "xai",
		Metadata: map[string]any{"type": "xai"},
		ModelStates: map[string]*ModelState{
			"grok-4.5": {
				Status:                   StatusError,
				Unavailable:              true,
				NextRetryAfter:           existingUntil,
				FreeUsageExhaustionCount: 3,
			},
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	incomingUntil := now.Add(12 * time.Hour)
	incoming := &Auth{
		ID:       auth.ID,
		Provider: auth.Provider,
		Metadata: map[string]any{
			"type": "xai",
			"runtime": map[string]any{
				"models": map[string]any{
					"grok-4.5": map[string]any{
						"cooldown_until":  incomingUntil.Format(time.RFC3339Nano),
						"reason":          "quota",
						"http_status":     429,
						"free_usage_hits": 2,
					},
					"grok-4": map[string]any{
						"cooldown_until": now.Add(8 * time.Hour).Format(time.RFC3339Nano),
						"reason":         "quota",
						"http_status":    429,
					},
				},
			},
		},
	}
	if _, err := manager.Update(WithSkipPersist(context.Background()), incoming); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["grok-4.5"]
	if state == nil || !state.Unavailable {
		t.Fatalf("existing active cooldown missing after merge: %+v", state)
	}
	if state.NextRetryAfter.Before(existingUntil.Add(-time.Second)) {
		t.Fatalf("cooldown shortened from %v to %v", existingUntil, state.NextRetryAfter)
	}
	if state.FreeUsageExhaustionCount != 3 {
		t.Fatalf("counter = %d, want max 3", state.FreeUsageExhaustionCount)
	}
	if incomingState := updated.ModelStates["grok-4"]; incomingState == nil || !incomingState.Unavailable || !incomingState.NextRetryAfter.After(now) {
		t.Fatalf("incoming persisted cooldown was ignored: %+v", incomingState)
	}
}

func TestManagerUpdateHydratesCodexPersistedRuntimeCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "update-codex-runtime", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	until := time.Now().Add(24 * time.Hour)
	incoming := &Auth{
		ID:       auth.ID,
		Provider: auth.Provider,
		Metadata: map[string]any{
			"type": "codex",
			"runtime": map[string]any{
				"models": map[string]any{
					"gpt-5.4": map[string]any{
						"cooldown_until":   until.Format(time.RFC3339Nano),
						"reason":           "usage_limit_reached",
						"http_status":      429,
						"usage_limit_hits": 2,
					},
				},
			},
		},
	}
	if _, err := manager.Update(WithSkipPersist(context.Background()), incoming); err != nil {
		t.Fatalf("update auth: %v", err)
	}

	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["gpt-5.4"]
	if state == nil || !state.Unavailable || !state.NextRetryAfter.After(time.Now()) || state.UsageLimitCount != 2 {
		t.Fatalf("Codex persisted cooldown was ignored: %+v", state)
	}
}

func TestManagerResetQuotaClearsPersistedRuntimeCooldown(t *testing.T) {
	store := &capturingAuthStore{}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "reset-codex-runtime",
		Provider: "codex",
		Metadata: map[string]any{
			"type": "codex",
			"runtime": map[string]any{
				"models": map[string]any{
					"gpt-5.6-sol": map[string]any{
						"cooldown_until":   time.Now().Add(24 * time.Hour).Format(time.RFC3339Nano),
						"reason":           "usage_limit_reached",
						"http_status":      http.StatusTooManyRequests,
						"usage_limit_hits": 1,
					},
				},
			},
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	if _, _, err := manager.ResetQuota(context.Background(), auth.ID); err != nil {
		t.Fatalf("reset quota: %v", err)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("reset auth missing")
	}
	if _, exists := updated.Metadata["runtime"]; exists {
		t.Fatalf("runtime metadata remains after reset: %#v", updated.Metadata["runtime"])
	}
	if saved := store.saved; saved == nil {
		t.Fatal("reset did not persist auth")
	} else if _, exists := saved.Metadata["runtime"]; exists {
		t.Fatalf("persisted runtime metadata remains after reset: %#v", saved.Metadata["runtime"])
	}
}

func TestManagerResetQuotaUsageLimitBeforeClearsStaleDisabledReason(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "recover-codex-clears-reason",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{
			"type":            "codex",
			"disabled":        false,
			"disabled_reason": "auth_unavailable",
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	if _, _, err := manager.ResetQuotaUsageLimitBefore(context.Background(), auth.ID, time.Now()); err != nil {
		t.Fatalf("recover quota: %v", err)
	}
	updated, _ := manager.GetByID(auth.ID)
	if _, ok := updated.Metadata["disabled_reason"]; ok {
		t.Fatalf("disabled_reason remains after successful quota recovery: %#v", updated.Metadata["disabled_reason"])
	}
}

func TestManagerResetQuotaUsageLimitBeforePreservesNewerFailureAndAuthCounter(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	observedAt := time.Now()
	oldRetry := observedAt.Add(time.Hour)
	newRetry := observedAt.Add(2 * time.Hour)
	auth := &Auth{
		ID:       "recover-codex-guarded",
		Provider: "codex",
		ModelStates: map[string]*ModelState{
			"old-model": {
				Status:           StatusError,
				StatusMessage:    "usage_limit_reached",
				Unavailable:      true,
				NextRetryAfter:   oldRetry,
				LastError:        &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"},
				Quota:            QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: oldRetry},
				UsageLimitCount:  2,
				AuthFailureCount: 3,
				UpdatedAt:        observedAt.Add(-time.Minute),
			},
			"new-model": {
				Status:         StatusError,
				StatusMessage:  "usage_limit_reached",
				Unavailable:    true,
				NextRetryAfter: newRetry,
				LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"},
				Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: newRetry},
				UpdatedAt:      observedAt.Add(time.Minute),
			},
		},
	}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	if _, models, err := manager.ResetQuotaUsageLimitBefore(context.Background(), auth.ID, observedAt); err != nil {
		t.Fatalf("recover quota: %v", err)
	} else if len(models) != 1 || models[0] != "old-model" {
		t.Fatalf("cleared models = %v, want [old-model]", models)
	}
	updated, _ := manager.GetByID(auth.ID)
	oldState := updated.ModelStates["old-model"]
	if oldState.Unavailable || oldState.Quota.Exceeded || oldState.AuthFailureCount != 3 {
		t.Fatalf("old state = %+v, want cooldown cleared and auth counter preserved", oldState)
	}
	newState := updated.ModelStates["new-model"]
	if !newState.Unavailable || !newState.Quota.Exceeded {
		t.Fatalf("newer state was cleared: %+v", newState)
	}
}

type capturingAuthStore struct {
	saved *Auth
}

func (s *capturingAuthStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *capturingAuthStore) Delete(context.Context, string) error  { return nil }
func (s *capturingAuthStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.saved = auth.Clone()
	return "", nil
}

func TestManagerMaxRetryCredentialsZeroSkipsCooledAuthAfterReregistration(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	executor := &authFallbackExecutor{id: "xai", executeErrors: map[string]error{}}
	manager.RegisterExecutor(executor)

	model := "grok-4.5"
	cooled := &Auth{ID: "aa-cooled-xai", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	ready := &Auth{ID: "bb-ready-xai", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	reg := registry.GetGlobalRegistry()
	for _, auth := range []*Auth{cooled, ready} {
		reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		manager.RefreshSchedulerEntry(auth.ID)
	}
	t.Cleanup(func() {
		reg.UnregisterClient(cooled.ID)
		reg.UnregisterClient(ready.ID)
	})

	manager.MarkResult(WithSkipPersist(context.Background()), cooldownQuotaResult(cooled.ID, "xai", model, `{"code":"subscription:free-usage-exhausted","error":"included free usage"}`, 24*time.Hour))
	// Reproduce the watcher path that used to erase the cooldown.
	reg.RegisterClient(cooled.ID, cooled.Provider, []*registry.ModelInfo{{ID: model}})
	manager.ReconcileRegistryModelStates(WithSkipPersist(context.Background()), cooled.ID)
	manager.RefreshSchedulerEntry(cooled.ID)

	response, err := manager.Execute(context.Background(), []string{"xai"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute with ready fallback: %v", err)
	}
	if string(response.Payload) != ready.ID {
		t.Fatalf("response auth = %q, want %q", response.Payload, ready.ID)
	}
	if calls := executor.ExecuteCalls(); len(calls) != 1 || calls[0] != ready.ID {
		t.Fatalf("executor calls = %v, want only ready auth", calls)
	}
}

type cooldownBlockingStore struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *cooldownBlockingStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *cooldownBlockingStore) Delete(context.Context, string) error  { return nil }
func (s *cooldownBlockingStore) Save(context.Context, *Auth) (string, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return "", nil
}

func TestManagerMarkResultUpdatesSchedulerBeforePersistenceCompletes(t *testing.T) {
	store := &cooldownBlockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	manager := NewManager(store, nil, nil)
	auth := &Auth{ID: "persist-order", Provider: "xai", Metadata: map[string]any{"type": "xai"}}
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "grok-4.5"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	manager.RefreshSchedulerEntry(auth.ID)

	done := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), cooldownQuotaResult(auth.ID, auth.Provider, "grok-4.5", `{"code":"subscription:free-usage-exhausted"}`, 24*time.Hour))
		close(done)
	}()

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("persistence was not reached")
	}

	picked, err := manager.scheduler.pickSingle(context.Background(), auth.Provider, "grok-4.5", cliproxyexecutor.Options{}, nil)
	close(store.release)
	<-done
	var cooldownErr *modelCooldownError
	if picked != nil || !errors.As(err, &cooldownErr) {
		t.Fatalf("scheduler selected auth while persistence was blocked: picked=%v err=%v", picked, err)
	}
}
