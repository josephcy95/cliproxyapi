package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/excel"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestExcelModelRegistrationEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, provider, plan, key string
		want                      bool
	}{
		{"paid", "codex", "plus", "", true},
		{"free", "codex", "free", "", false},
		{"unknown", "codex", "", "", false},
		{"apikey", "codex", "plus", "key", false},
		{"kiro", "kiro", "plus", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{cfg: &config.Config{ExcelKey: []internalconfig.ExcelKey{{}}}}
			a := &coreauth.Auth{ID: t.Name(), Provider: tc.provider, Attributes: map[string]string{"api_key": tc.key}, Metadata: map[string]any{"access_token": "token", "plan_type": tc.plan}}
			t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(a.ID) })
			models := []*ModelInfo{{ID: "ordinary"}, {ID: "gpt-6-astra-excel"}, {ID: "kiro/gpt-6-astra-excel"}, {ID: "renamed-excel-route", MetadataModelID: "gpt-6-astra-excel"}}
			s.registerResolvedModelsForAuth(a, tc.provider, models)
			got := GlobalModelRegistry().GetModelsForClient(a.ID)
			count := 0
			for _, m := range got {
				if excel.IsRoute(m.ID) || excel.IsRoute(m.MetadataModelID) {
					count++
				}
			}
			if (count > 0) != tc.want {
				t.Fatalf("Excel registrations=%d want eligibility %v", count, tc.want)
			}
			if len(got) == 0 {
				t.Fatal("ordinary model lost")
			}
		})
	}
}

func TestExcelOAuthRegistrationAndExclusions(t *testing.T) {
	for _, excluded := range []string{"", "gpt-6-astra-excel"} {
		t.Run("excluded="+excluded, func(t *testing.T) {
			s := &Service{cfg: &config.Config{ExcelKey: []internalconfig.ExcelKey{{}}}}
			a := &coreauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token", "plan_type": "plus"}, Attributes: map[string]string{"excluded_models": excluded}}
			t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(a.ID) })
			s.registerModelsForAuth(context.Background(), a)
			found := false
			for _, m := range GlobalModelRegistry().GetModelsForClient(a.ID) {
				if m.ID == "gpt-6-astra-excel" {
					found = true
				}
			}
			if found != (excluded == "") {
				t.Fatalf("alias present=%v excluded=%q", found, excluded)
			}
		})
	}
}

func TestExcelConfiguredProvidersCannotAdvertiseRoutes(t *testing.T) {
	for _, provider := range []string{"codex", "kiro"} {
		t.Run(provider, func(t *testing.T) {
			cfg := &config.Config{ExcelKey: []internalconfig.ExcelKey{{}}}
			a := &coreauth.Auth{ID: t.Name(), Provider: provider, Prefix: provider, Attributes: map[string]string{"api_key": "test-key", "source": "config:" + provider + ":test", "config_index": "0"}}
			if provider == "codex" {
				cfg.CodexKey = []config.CodexKey{{APIKey: "test-key", Models: []internalconfig.CodexModel{{Name: "gpt-6-astra-excel", Alias: "renamed"}, {Name: "ordinary"}}}}
			} else {
				cfg.OpenAICompatibility = []config.OpenAICompatibility{{Name: "kiro", Models: []config.OpenAICompatibilityModel{{Name: "gpt-6-astra-excel", Alias: "renamed"}, {Name: "gpt-6-astra-excel"}, {Name: "ordinary"}}}}
				a.Attributes["compat_name"] = "kiro"
				a.Attributes["provider_key"] = "kiro"
			}
			s := &Service{cfg: cfg}
			t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(a.ID) })
			s.registerModelsForAuth(context.Background(), a)
			models := GlobalModelRegistry().GetModelsForClient(a.ID)
			if len(models) == 0 {
				t.Fatal("ordinary models were removed")
			}
			for _, m := range models {
				if excel.IsRoute(m.ID) || excel.IsRoute(m.MetadataModelID) || m.ID == "renamed" || m.ID == provider+"/renamed" {
					t.Errorf("ineligible provider advertises %q", m.ID)
				}
			}
		})
	}
}

func TestExcelRegistrationRemovesAliasesAfterDowngradeOrDisable(t *testing.T) {
	s := &Service{cfg: &config.Config{ExcelKey: []internalconfig.ExcelKey{{}}}}
	a := &coreauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token", "plan_type": "plus"}}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(a.ID) })
	for _, state := range []struct {
		plan          string
		enabled, want bool
	}{{"plus", true, true}, {"free", true, false}, {"plus", false, false}, {"plus", true, true}} {
		a.Metadata["plan_type"] = state.plan
		s.cfg.ExcelKey[0].Disabled = !state.enabled
		s.registerModelsForAuth(context.Background(), a)
		found := false
		for _, m := range GlobalModelRegistry().GetModelsForClient(a.ID) {
			found = found || excel.IsRoute(m.ID)
		}
		if found != state.want {
			t.Fatalf("plan=%s enabled=%v: Excel present=%v", state.plan, state.enabled, found)
		}
	}
}
