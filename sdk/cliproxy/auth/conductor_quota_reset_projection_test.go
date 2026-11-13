package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestResetQuotaPublishesRegisteredModelSpelling(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "audit-reset-case", Provider: "codex", Status: StatusActive}
	_, err := m.Register(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "audit-model"}})
	defer reg.UnregisterClient(a.ID)
	d := time.Minute
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: a.Provider, Model: "Audit-Model", Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"}, RetryAfter: &d})
	if reg.GetModelCount("audit-model") != 0 {
		t.Fatal("not suspended before reset")
	}
	_, _, err = m.ResetQuota(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reg.GetModelCount("audit-model") != 1 {
		t.Fatal("reset left registry native model suspended")
	}
}

type resetProjectionFailingStore struct{}

func (resetProjectionFailingStore) Load(context.Context) ([]CooldownStateRecord, error) {
	return nil, nil
}
func (resetProjectionFailingStore) Save(context.Context, []CooldownStateRecord) error {
	return errors.New("fixture cooldown save failure")
}

func TestResetQuotaRollbackPublishesRegisteredModelSpelling(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "reset-rollback-case", Provider: "codex", Status: StatusActive}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "reset-rollback-model"}})
	t.Cleanup(func() { reg.UnregisterClient(a.ID) })
	delay := time.Minute
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: a.Provider, Model: "Reset-Rollback-Model", Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "usage_limit_reached"}, RetryAfter: &delay})
	before, _ := m.GetByID(a.ID)
	m.mu.Lock()
	m.cooldownStore = resetProjectionFailingStore{}
	m.mu.Unlock()
	if _, _, err := m.ResetQuota(context.Background(), a.ID); err == nil {
		t.Fatal("expected persistence error")
	}
	restored, _ := m.GetByID(a.ID)
	if restored.Generation <= before.Generation {
		t.Fatalf("rollback generation=%d before=%d", restored.Generation, before.Generation)
	}
	if !restored.ModelStates["Reset-Rollback-Model"].NextRetryAfter.Equal(before.ModelStates["Reset-Rollback-Model"].NextRetryAfter) {
		t.Fatal("rollback lost original cooldown deadline")
	}
	if reg.GetModelCount("reset-rollback-model") != 0 {
		t.Fatal("rollback failed to restore registered model suspension")
	}
	projection := m.clientModelProjectionForAuth(restored, "reset-rollback-model", time.Now())
	if !projection.SuspendUntil.After(time.Now()) {
		t.Fatal("rollback projection lost timed expiry")
	}
}

type resetProjectionCallbackStore struct{ callback func() }

func (resetProjectionCallbackStore) Load(context.Context) ([]CooldownStateRecord, error) {
	return nil, nil
}
func (s resetProjectionCallbackStore) Save(context.Context, []CooldownStateRecord) error {
	s.callback()
	return errors.New("fixture save failure after concurrent publication")
}

func TestResetQuotaRollbackRebasesConcurrentAliasPublication(t *testing.T) {
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "reset-rebase-alias", Provider: "claude", Status: StatusActive}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "rebase-route", MetadataModelID: "rebase-target"}, {ID: "rebase-target"}})
	t.Cleanup(func() { reg.UnregisterClient(a.ID) })
	delay := time.Minute
	m.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: a.Provider, Model: "rebase-route", Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"}, RetryAfter: &delay})
	observed := time.Now().Add(time.Second)
	var publishedGeneration uint64
	m.mu.Lock()
	m.cooldownStore = resetProjectionCallbackStore{callback: func() {
		// Alias publication uses m.mu, independently of the credential mutation gate.
		m.mu.Lock()
		defer m.mu.Unlock()
		m.oauthModelAlias.Store(compileOAuthModelAliasTable(map[string][]internalconfig.OAuthModelAlias{"claude": {{Name: "rebase-target", Alias: "rebase-route"}}}))
		live := m.auths[a.ID]
		m.migrateAuthAliasModelStatesLocked(live, nil, time.Now(), false)
		if live.ModelStates == nil {
			live.ModelStates = make(map[string]*ModelState)
		}
		live.ModelStates["rebase-target"] = &ModelState{Status: StatusActive, Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{"new-observation": "preserved"}}}
		if live.Metadata == nil {
			live.Metadata = make(map[string]any)
		}
		live.Metadata["concurrent-marker"] = "preserved"
		live.Generation += 10
		publishedGeneration = live.Generation
	}}
	m.mu.Unlock()
	if _, _, err := m.ResetQuota(context.Background(), a.ID); err == nil {
		t.Fatal("expected persistence failure")
	}
	restored, _ := m.GetByID(a.ID)
	if restored.Generation <= publishedGeneration {
		t.Fatal("rollback generation did not advance past concurrent publication")
	}
	if restored.Metadata["concurrent-marker"] != "preserved" {
		t.Fatal("rollback discarded concurrent metadata")
	}
	if restored.ModelStates["rebase-route"] != nil {
		t.Fatal("rollback resurrected obsolete alias key")
	}
	state := restored.ModelStates["rebase-target"]
	if state == nil || !state.NextRetryAfter.After(time.Now()) {
		t.Fatal("rollback lost original cooldown at new target")
	}
	if state.Quota.Signals["new-observation"] != "preserved" || !state.Quota.ObservedAt.Equal(observed) {
		t.Fatal("rollback discarded newer observation")
	}
	if reg.GetModelCount("rebase-route") != 0 {
		t.Fatal("rollback projection did not follow new alias")
	}
}

func TestQuotaResetRollbackRebasePreservesModelLessCooldownAndCurrentPolicy(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cooling-disabled=%t", disabled), func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			now := time.Now()
			deadline := now.Add(time.Minute)
			original := &Auth{ID: "model-less-rebase", Provider: "claude", Status: StatusError, Unavailable: true, NextRetryAfter: deadline, Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline}, Generation: 3}
			reset := original.Clone()
			clearCooldownStateForAuth(reset, now)
			reset.Generation = 4
			current := reset.Clone()
			current.Generation = 20
			current.Quota.ObservedAt = now.Add(time.Second)
			current.Quota.Signals = map[string]string{"new": "observation"}
			m.auths[original.ID] = current
			if disabled {
				m.runtimeConfig.Store(&internalconfig.Config{DisableCooling: true})
			}
			m.mu.Lock()
			restored := m.rebaseQuotaResetRollbackLocked(original, reset, now)
			m.mu.Unlock()
			if restored.Generation <= 20 {
				t.Fatal("nonmonotonic generation")
			}
			if restored.Quota.Signals["new"] != "observation" {
				t.Fatal("passive observation lost")
			}
			if disabled {
				if restored.NextRetryAfter.After(now) {
					t.Fatal("rollback ignored current cooling policy")
				}
			} else if !restored.NextRetryAfter.Equal(deadline) || !restored.Quota.NextRecoverAt.Equal(deadline) {
				t.Fatal("model-less cooldown lost")
			}
		})
	}
}

func TestQuotaResetRollbackRebaseKeepsExactForcedLayer(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	forced := now.Add(time.Minute)
	original := &Auth{ID: "forced-rebase", Provider: "claude", Status: StatusError, Generation: 3, ModelStates: map[string]*ModelState{"model": {Status: StatusError, Unavailable: true, NextRetryAfter: forced, forceCooldownUntil: forced, forceCooldownSeeded: true, LastError: &Error{Code: ErrorCodeForceCooldown}}}}
	reset := original.Clone()
	clearCooldownStateForAuth(reset, now)
	reset.Generation = 4
	current := reset.Clone()
	current.Generation = 10
	current.ModelStates = map[string]*ModelState{"model": {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), forceCooldownSeeded: true}}
	m.auths[original.ID] = current
	m.runtimeConfig.Store(&internalconfig.Config{DisableCooling: true})
	m.mu.Lock()
	restored := m.rebaseQuotaResetRollbackLocked(original, reset, now)
	m.mu.Unlock()
	state := restored.ModelStates["model"]
	if state == nil || !state.NextRetryAfter.Equal(forced) || !state.forceCooldownUntil.Equal(forced) {
		t.Fatalf("exact forced deadline lost: %+v", state)
	}
}
