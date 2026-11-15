package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestPoolCooldownRespectsReservedPrivateInstructionsAuths(t *testing.T) {
	withQuotaCooldownEnabled(t)
	manager := NewManager(nil, nil, nil)
	cfg := &config.Config{}
	cfg.Codex.Instructions.ReserveMarkedAuths = true
	manager.SetConfig(cfg)
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "codex"})
	now := time.Now()
	deadline := now.Add(time.Hour)
	for _, marked := range []bool{false, true} {
		id := "sync-pool-normal"
		a := &Auth{Provider: "codex", Status: StatusActive}
		if marked {
			id = "sync-pool-reserved"
			a.Attributes = map[string]string{"allow_private_instructions": "true"}
		} else {
			a.ModelStates = map[string]*ModelState{"sync-pool-model": {Status: StatusError, Unavailable: true, NextRetryAfter: deadline, Quota: QuotaState{Exceeded: true, NextRecoverAt: deadline}}}
		}
		a.ID = id
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "sync-pool-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	got, unblocked, ok := manager.earliestPoolCooldownForModelAt(context.Background(), []string{"codex"}, "sync-pool-model", cliproxyexecutor.Options{}, now)
	if unblocked || !ok || !got.Equal(deadline) {
		t.Fatalf("reserved auth distorted pool deadline: %v, unblocked=%v ok=%v", got, unblocked, ok)
	}
}
