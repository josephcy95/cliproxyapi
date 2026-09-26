package auth

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executionregistry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestExcelEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, provider, kind, source, plan, token, key string
		want                                           bool
	}{
		{"paid", "codex", "oauth", "file", "plus", "oauth-token", "", true},
		{"free", "codex", "oauth", "file", "free", "oauth-token", "", false},
		{"unknown", "codex", "oauth", "file", "", "oauth-token", "", false},
		{"invalid plan", "codex", "oauth", "file", "unknown", "oauth-token", "", false},
		{"api key", "codex", "apikey", "config", "plus", "", "key", false},
		{"mixed key", "codex", "oauth", "file", "plus", "oauth-token", "key", false},
		{"configured", "codex", "oauth", "config", "plus", "oauth-token", "", false},
		{"kiro", "kiro", "oauth", "file", "plus", "oauth-token", "", false},
		{"missing token", "codex", "oauth", "file", "plus", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Auth{Provider: tc.provider, Attributes: map[string]string{"auth_kind": tc.kind, "source": tc.source, "plan_type": tc.plan, "api_key": tc.key}, Metadata: map[string]any{"access_token": tc.token}}
			if got := a.ExcelEligible(); got != tc.want {
				t.Fatalf("ExcelEligible()=%v want %v", got, tc.want)
			}
			for _, model := range []string{"gpt-6-astra-excel", "kiro/gpt-6-astra-excel(high)"} {
				e := authSelectionEligibilityForRequest(context.Background(), coreexecutor.Options{}, model)
				if got := e.allows(a); got != tc.want {
					t.Errorf("selection %s=%v want %v", model, got, tc.want)
				}
			}
		})
	}
}

func TestExcelEligibilityMetadataPlan(t *testing.T) {
	a := &Auth{Provider: "codex", Metadata: map[string]any{"access_token": "token", "chatgpt_plan_type": "pro"}}
	if !a.ExcelEligible() {
		t.Fatal("paid OAuth metadata should qualify")
	}
	a.Metadata["chatgpt_plan_type"] = "free"
	if a.ExcelEligible() {
		t.Fatal("free metadata must not qualify")
	}
}

func TestExcelManagerSelectionPaths(t *testing.T) {
	const model = "gpt-6-astra-excel"
	for _, path := range []string{"single", "legacy", "mixed", "mixed-legacy"} {
		t.Run(path, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			manager.RegisterExecutor(schedulerTestExecutor{provider: "kiro"})
			paid := &Auth{ID: t.Name() + "-paid", Provider: "codex", Metadata: map[string]any{"access_token": "token", "plan_type": "plus"}}
			key := &Auth{ID: t.Name() + "-key", Provider: "codex", Attributes: map[string]string{"api_key": "key", "priority": "100"}}
			free := &Auth{ID: t.Name() + "-free", Provider: "codex", Metadata: map[string]any{"access_token": "token", "plan_type": "free"}}
			kiro := &Auth{ID: t.Name() + "-kiro", Provider: "kiro", Metadata: map[string]any{"access_token": "token", "plan_type": "plus"}}
			for _, a := range []*Auth{paid, key, free, kiro} {
				registerSchedulerModels(t, a.Provider, model, a.ID)
				if _, err := manager.Register(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			pick := func(opts coreexecutor.Options, tried map[string]struct{}) (*Auth, error) {
				switch path {
				case "single":
					a, _, err := manager.pickNext(context.Background(), "codex", model, opts, tried)
					return a, err
				case "legacy":
					a, _, err := manager.pickNextLegacy(context.Background(), "codex", model, opts, tried)
					return a, err
				case "mixed":
					a, _, _, err := manager.pickNextMixed(context.Background(), []string{"codex", "kiro"}, model, opts, tried)
					return a, err
				default:
					a, _, _, err := manager.pickNextMixedLegacy(context.Background(), []string{"codex", "kiro"}, model, opts, tried)
					return a, err
				}
			}
			a, err := pick(coreexecutor.Options{}, nil)
			if err != nil || a == nil || a.ID != paid.ID {
				t.Fatalf("selection=%v err=%v", a, err)
			}
			for _, bad := range []*Auth{key, free, kiro} {
				a, err = pick(coreexecutor.Options{Metadata: map[string]any{coreexecutor.PinnedAuthMetadataKey: bad.ID}}, nil)
				if err == nil || a != nil {
					t.Fatalf("ineligible pin %s accepted", bad.ID)
				}
			}
			a, err = pick(coreexecutor.Options{}, map[string]struct{}{paid.ID: {}})
			if err == nil || a != nil {
				t.Fatal("retry fell back to ineligible credential")
			}
		})
	}
}

func TestExcelEligibilityPreservesRequestedModelPolicy(t *testing.T) {
	a := &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "key"}}
	for _, meta := range []map[string]any{
		{coreexecutor.RequestedModelMetadataKey: "paid/gpt-6-astra-excel(high)"},
		{coreexecutor.RequireExcelOAuthMetadataKey: true},
	} {
		if authSelectionEligibilityForRequest(context.Background(), coreexecutor.Options{Metadata: meta}, "rewritten-model").allows(a) {
			t.Fatal("request policy lost after model rewrite")
		}
	}
	if !authSelectionEligibilityForRequest(context.Background(), coreexecutor.Options{}, "gpt-6-astra").allows(a) {
		t.Fatal("ordinary Codex API-key routing changed")
	}
}

func TestExcelEligibilityPlanPrecedence(t *testing.T) {
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`)) + ".signature"
	a := &Auth{Provider: "codex", Metadata: map[string]any{"access_token": "token", "id_token": token}}
	if !a.ExcelEligible() {
		t.Fatal("paid ID-token plan should qualify")
	}
	a.Metadata["plan_type"] = "free"
	a.Attributes = map[string]string{"plan_type": "plus"}
	if a.ExcelEligible() {
		t.Fatal("fresh free plan must override stale paid attribute and JWT")
	}
	a.Metadata["plan_type"] = ""
	a.Metadata["chatgpt_plan_type"] = "free"
	if a.ExcelEligible() {
		t.Fatal("empty plan_type must not shadow chatgpt_plan_type")
	}
}

func TestExcelHomeRejectsIneligibleAuthAndReleasesDispatch(t *testing.T) {
	dispatcher := &authKindHomeDispatcher{auths: []Auth{{ID: "key", Provider: "codex", Attributes: map[string]string{"api_key": "key"}}}}
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	reg := executionregistry.New()
	manager.PublishHomeDispatch(dispatcher, reg, 1)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	opts := coreexecutor.Options{Metadata: map[string]any{}}
	opts.Metadata[homeAuthCountMetadataKey] = 1
	a, _, _, err := manager.pickNextViaHome(context.Background(), "gpt-6-astra-excel", opts, nil)
	if err == nil || a != nil {
		t.Fatal("Home accepted API key for Excel")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reg.Drain(ctx); err != nil {
		t.Fatalf("dispatch leaked: %v", err)
	}
}
