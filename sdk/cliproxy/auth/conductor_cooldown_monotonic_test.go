package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// newCooldownMonotonicManager is shared by ResultPolicy and related cooldown tests.
// Upstream monotonic deadline-preservation tests (#5501) were not ported: this
// fork's MarkResult path combines adaptive Codex observation, xAI/Codex
// exhaustion, and ResultPolicy hooks that differ from upstream's model.
func newCooldownMonotonicManager(t *testing.T, models ...string) (*Manager, *Auth) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-monotonic-" + models[0], Provider: "claude"}
	reg := registry.GetGlobalRegistry()
	infos := make([]*registry.ModelInfo, 0, len(models))
	now := time.Now().Unix()
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model, Created: now})
	}
	reg.RegisterClient(auth.ID, auth.Provider, infos)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	return m, auth
}

func TestManager_MarkResult_CredentialScopeDoesNotInheritModelQuotaDeadline(t *testing.T) {
	withQuotaCooldownEnabled(t)

	testCases := []struct {
		name            string
		credentialModel string
	}{
		{name: "cross_model", credentialModel: "claude-sonnet-5"},
		{name: "same_model", credentialModel: "claude-fable-5-1"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			m, auth := newCooldownMonotonicManager(t, "claude-fable-5-1", "claude-sonnet-5", "claude-opus-5-5", "claude-sonnet-4")
			ctx := context.Background()
			for _, model := range []string{"claude-fable-5-1", "claude-sonnet-5", "claude-opus-5-5"} {
				m.MarkResult(ctx, Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Success: true})
			}

			before := time.Now()
			modelOnlyRetry := 8 * 24 * time.Hour
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "claude-fable-5-1",
				RetryAfter: &modelOnlyRetry,
				Error:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "Usage credits are required for this model."},
			})

			modelOnlyAuth, _ := m.GetByID(auth.ID)
			if modelOnlyAuth.Quota.Reason != "quota" || !modelOnlyAuth.Quota.NextRecoverAt.After(before.Add(7*24*time.Hour)) {
				t.Fatalf("precondition failed: model quota deadline was not aggregated: quota=%+v", modelOnlyAuth.Quota)
			}
			if blocked, _, _ := isAuthBlockedForModel(modelOnlyAuth, "claude-opus-5-5", time.Now()); blocked {
				t.Fatal("model-only cooldown should not block an unrelated model")
			}

			credentialRetry := 3 * time.Hour
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: testCase.credentialModel,
				RetryAfter: &credentialRetry, CredentialScope: true,
				Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "shared window rejected"},
			})

			updated, _ := m.GetByID(auth.ID)
			minCredentialDeadline := before.Add(credentialRetry - time.Hour)
			maxCredentialDeadline := before.Add(credentialRetry + time.Hour)
			if updated.Quota.Reason != "credential_quota" ||
				updated.Quota.NextRecoverAt.Before(minCredentialDeadline) ||
				updated.Quota.NextRecoverAt.After(maxCredentialDeadline) {
				t.Fatalf("credential cooldown inherited a model-only deadline: reason=%q deadline=%v (in %v)",
					updated.Quota.Reason, updated.Quota.NextRecoverAt, updated.Quota.NextRecoverAt.Sub(before))
			}

			opusBlocked, _, opusNext := isAuthBlockedForModel(updated, "claude-opus-5-5", before.Add(credentialRetry+time.Minute))
			if opusBlocked {
				t.Fatalf("opus remained blocked beyond credential cooldown: next=%v (in %v)", opusNext, opusNext.Sub(before))
			}
			fableBlocked, _, fableNext := isAuthBlockedForModel(updated, "claude-fable-5-1", before.Add(credentialRetry+time.Minute))
			if !fableBlocked || !fableNext.After(before.Add(7*24*time.Hour)) {
				t.Fatalf("model-only cooldown was not retained for Fable: blocked=%v next=%v", fableBlocked, fableNext)
			}

			credentialDeadline := updated.Quota.NextRecoverAt
			shorterCredentialRetry := 30 * time.Minute
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "claude-sonnet-4",
				RetryAfter: &shorterCredentialRetry, CredentialScope: true,
				Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "shorter shared window rejection"},
			})
			updated, _ = m.GetByID(auth.ID)
			if !updated.Quota.NextRecoverAt.Equal(credentialDeadline) {
				t.Fatalf("later credential-scoped cooldown shortened the active credential deadline: got=%v want=%v",
					updated.Quota.NextRecoverAt, credentialDeadline)
			}
		})
	}
}

func TestManager_MarkResult_CredentialScopeBackoffPersistsAcrossWindows(t *testing.T) {
	withQuotaCooldownEnabled(t)

	m, auth := newCooldownMonotonicManager(t, "model-a", "model-b")
	// Use a provider without the fork's deliberate fixed Claude 429 fallback.
	auth.Provider = "gemini"
	if _, err := m.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.MarkResult(ctx, Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-a", CredentialScope: true,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded"},
	})

	updated, _ := m.GetByID(auth.ID)
	if updated.Quota.BackoffLevel != 1 {
		t.Fatalf("first credential quota failure did not retain backoff level 1: quota=%+v", updated.Quota)
	}

	expired := time.Now().Add(-time.Minute)
	m.mu.Lock()
	storedAuth := m.auths[auth.ID]
	storedAuth.Quota.NextRecoverAt = expired
	storedAuth.NextRetryAfter = expired
	for _, state := range storedAuth.ModelStates {
		if state != nil {
			state.Quota.NextRecoverAt = expired
			state.NextRetryAfter = expired
		}
	}
	m.mu.Unlock()

	secondFailureAt := time.Now()
	m.MarkResult(ctx, Result{
		AuthID: auth.ID, Provider: auth.Provider, Model: "model-b", CredentialScope: true,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded again"},
	})

	updated, _ = m.GetByID(auth.ID)
	if updated.Quota.BackoffLevel != 2 {
		t.Fatalf("second credential quota failure did not advance backoff to level 2: quota=%+v", updated.Quota)
	}
	if updated.Quota.NextRecoverAt.Before(secondFailureAt.Add(2 * quotaBackoffBase)) {
		t.Fatalf("second credential quota failure did not use the next backoff window: deadline=%v", updated.Quota.NextRecoverAt)
	}
}

func TestManager_MarkResult_CredentialScopeDoesNotInheritModelBackoffLevel(t *testing.T) {
	withQuotaCooldownEnabled(t)

	retryAfter := 10 * time.Second
	tests := []struct {
		name                 string
		retryAfter           *time.Duration
		wantBackoffLevel     int
		wantCooldownDuration time.Duration
	}{
		{name: "without_retry_hint", wantBackoffLevel: 1, wantCooldownDuration: quotaBackoffBase},
		{name: "with_retry_hint", retryAfter: &retryAfter, wantCooldownDuration: retryAfter},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			m, auth := newCooldownMonotonicManager(t, "model-a", "model-b")
			// Use a provider without the fork's deliberate fixed Claude 429 fallback.
			auth.Provider = "gemini"
			if _, err := m.Update(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "model-a",
				Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model-only quota exceeded"},
			})

			modelOnlyAuth, _ := m.GetByID(auth.ID)
			modelState := existingModelState(modelOnlyAuth, canonicalModelKey("model-a"))
			if modelOnlyAuth.Quota.Reason != "quota" || modelState == nil || modelState.Quota.BackoffLevel != 1 {
				t.Fatalf("precondition failed: model-only quota/backoff missing: auth=%+v state=%+v", modelOnlyAuth.Quota, modelState)
			}

			secondFailureStarted := time.Now()
			m.MarkResult(ctx, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: "model-a", RetryAfter: testCase.retryAfter,
				CredentialScope: true,
				Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded"},
			})
			secondFailureCompleted := time.Now()

			updated, _ := m.GetByID(auth.ID)
			if updated.Quota.BackoffLevel != testCase.wantBackoffLevel {
				t.Fatalf("credential cooldown inherited model backoff: got level %d, want %d", updated.Quota.BackoffLevel, testCase.wantBackoffLevel)
			}
			minDeadline := secondFailureStarted.Add(testCase.wantCooldownDuration)
			maxDeadline := secondFailureCompleted.Add(testCase.wantCooldownDuration)
			if updated.Quota.NextRecoverAt.Before(minDeadline) || updated.Quota.NextRecoverAt.After(maxDeadline) {
				t.Fatalf("credential cooldown deadline=%v, want between %v and %v",
					updated.Quota.NextRecoverAt, minDeadline, maxDeadline)
			}
		})
	}
}
