package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestModelSchedulerDemotesTokenThatExpiresAfterInsertion(t *testing.T) {
	now := time.Now()
	auth := &Auth{ID: "expires-after-insert", Provider: "codex", Metadata: map[string]any{
		"access_token": "token", "expired": now.Add(time.Minute).Format(time.RFC3339Nano),
	}}
	shard := &modelScheduler{entries: make(map[string]*scheduledAuth), readyByPriority: make(map[int]*readyBucket)}
	shard.upsertEntryLocked(&scheduledAuthMeta{auth: auth, weight: 1}, now)
	if got := shard.pickReadyLocked(false, schedulerStrategyFillFirst, nil); got == nil {
		t.Fatal("valid token should be selectable before expiry")
	}
	if len(shard.blocked) != 0 {
		t.Fatal("test requires an empty blocked queue before expiry")
	}
	shard.promoteExpiredLocked(now.Add(2 * time.Minute))
	if got := shard.pickReadyLocked(false, schedulerStrategyFillFirst, nil); got != nil {
		t.Fatalf("expired token remains selectable: %s", got.ID)
	}
}

type syncCreditsExecutor struct {
	schedulerTestExecutor
	incomplete bool
}

func (e syncCreditsExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"status":"completed","output":[]}`), Incomplete: e.incomplete}, nil
}

func TestAntigravityCreditsFallbackCarriesCompletionAndToken(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "incomplete"}[incomplete], func(t *testing.T) {
			const model = "claude-sync-credits-model"
			hook := &recordingHook{}
			manager := NewManager(nil, nil, hook)
			manager.RegisterExecutor(syncCreditsExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "antigravity"}, incomplete: incomplete})
			auth := &Auth{ID: "sync-credits", Provider: "antigravity", Metadata: map[string]any{"access_token": "credits-token"}}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			_, used, err := manager.tryAntigravityCreditsExecute(context.Background(), cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
			if err != nil || !used {
				t.Fatalf("credits fallback: used=%v err=%v", used, err)
			}
			result := hook.lastResult.Load()
			if result == nil || result.CompletedResponse == incomplete || result.AccessToken != "credits-token" {
				t.Fatalf("unexpected fallback result: %+v", result)
			}
			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated.InferenceVerified == incomplete {
				t.Fatalf("inference verification does not reflect completion: %+v", updated)
			}
		})
	}
}
