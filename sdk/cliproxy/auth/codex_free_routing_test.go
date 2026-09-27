package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestManagerCodexPreferFreeForSharedModels(t *testing.T) {
	model := "codex-free-routing-shared"
	selectors := []struct {
		name     string
		selector Selector
	}{
		{name: "round-robin", selector: &RoundRobinSelector{}},
		{name: "weighted-round-robin", selector: &WeightedRoundRobinSelector{}},
		{name: "fill-first", selector: &FillFirstSelector{}},
		{name: "session-affinity", selector: NewSessionAffinitySelector(&RoundRobinSelector{})},
	}

	for _, tt := range selectors {
		t.Run(tt.name, func(t *testing.T) {
			freeID := "free-" + tt.name
			paidID := "paid-" + tt.name
			registerSchedulerModels(t, "codex", model, freeID, paidID)

			manager := NewManager(nil, tt.selector, nil)
			manager.executors["codex"] = schedulerTestExecutor{}
			manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{
				Routing: internalconfig.CodexRoutingConfig{PreferFreeForSharedModels: true},
			}})
			for _, candidate := range []*Auth{
				{ID: freeID, Provider: "codex", Attributes: map[string]string{"plan_type": "free", "priority": "0", AttributeWeight: "1"}},
				{ID: paidID, Provider: "codex", Attributes: map[string]string{"plan_type": "pro", "priority": "100", AttributeWeight: "100"}},
			} {
				if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
					t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
				}
			}

			selected, _, errPick := manager.pickNext(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickNext() error = %v", errPick)
			}
			if selected == nil || selected.ID != freeID {
				t.Fatalf("pickNext() auth = %#v, want %q", selected, freeID)
			}

			selected, _, errPick = manager.pickNext(context.Background(), "codex", model, cliproxyexecutor.Options{}, map[string]struct{}{freeID: {}})
			if errPick != nil {
				t.Fatalf("pickNext() fallback error = %v", errPick)
			}
			if selected == nil || selected.ID != paidID {
				t.Fatalf("pickNext() fallback auth = %#v, want %q", selected, paidID)
			}
		})
	}
}

func TestManagerCodexPreferFreeFallsBackWhenFreeUnavailable(t *testing.T) {
	model := "codex-free-routing-unavailable"
	freeID := "codex-free-routing-unavailable-free"
	paidID := "codex-free-routing-unavailable-paid"
	registerSchedulerModels(t, "codex", model, freeID, paidID)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{
		Routing: internalconfig.CodexRoutingConfig{PreferFreeForSharedModels: true},
	}})
	for _, candidate := range []*Auth{
		{
			ID:             freeID,
			Provider:       "codex",
			Attributes:     map[string]string{"plan_type": "free"},
			Unavailable:    true,
			NextRetryAfter: time.Now().Add(time.Hour),
		},
		{ID: paidID, Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
		}
	}

	selected, _, errPick := manager.pickNext(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if selected == nil || selected.ID != paidID {
		t.Fatalf("pickNext() auth = %#v, want available fallback %q", selected, paidID)
	}
}

func TestManagerCodexPreferFreeRespectsModelSupportAndExplicitPin(t *testing.T) {
	sharedModel := "codex-free-routing-shared-pin"
	paidModel := "codex-free-routing-paid-only"
	freeID := "codex-free-routing-free"
	paidID := "codex-free-routing-paid"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(freeID, "codex", []*registry.ModelInfo{{ID: sharedModel}})
	reg.RegisterClient(paidID, "codex", []*registry.ModelInfo{{ID: sharedModel}, {ID: paidModel}})
	t.Cleanup(func() {
		reg.UnregisterClient(freeID)
		reg.UnregisterClient(paidID)
	})

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{
		Routing: internalconfig.CodexRoutingConfig{PreferFreeForSharedModels: true},
	}})
	for _, candidate := range []*Auth{
		{ID: freeID, Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
		{ID: paidID, Provider: "codex", Attributes: map[string]string{"plan_type": "k12"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
		}
	}

	selected, _, errPick := manager.pickNext(context.Background(), "codex", paidModel, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext(paid-only) error = %v", errPick)
	}
	if selected == nil || selected.ID != paidID {
		t.Fatalf("pickNext(paid-only) auth = %#v, want %q", selected, paidID)
	}

	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: paidID}}
	selected, _, errPick = manager.pickNext(context.Background(), "codex", sharedModel, opts, nil)
	if errPick != nil {
		t.Fatalf("pickNext(pinned) error = %v", errPick)
	}
	if selected == nil || selected.ID != paidID {
		t.Fatalf("pickNext(pinned) auth = %#v, want %q", selected, paidID)
	}
}

func TestManagerCodexPreferFreeDefaultsOff(t *testing.T) {
	model := "codex-free-routing-default-off"
	freeID := "codex-free-routing-default-free"
	paidID := "codex-free-routing-default-paid"
	registerSchedulerModels(t, "codex", model, freeID, paidID)

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.executors["codex"] = schedulerTestExecutor{}
	manager.SetConfig(&internalconfig.Config{})
	for _, candidate := range []*Auth{
		{ID: freeID, Provider: "codex", Attributes: map[string]string{"plan_type": "free", "priority": "0"}},
		{ID: paidID, Provider: "codex", Attributes: map[string]string{"plan_type": "plus", "priority": "10"}},
	} {
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", candidate.ID, errRegister)
		}
	}

	selected, _, errPick := manager.pickNext(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if selected == nil || selected.ID != paidID {
		t.Fatalf("pickNext() auth = %#v, want existing priority winner %q", selected, paidID)
	}
}
