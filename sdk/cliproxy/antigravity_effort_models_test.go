package cliproxy

import (
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestAntigravityEffortPublicationHonorsExclusionsAliasesAndPrefixes(t *testing.T) {
	auth := &coreauth.Auth{ID: t.Name(), Provider: "antigravity", Prefix: "tenant", Attributes: map[string]string{}}
	hints := antigravityModelCapabilityHints{ModelIDs: map[string]struct{}{"gemini-3.8-flash-low": {}, "gemini-3.8-flash-medium": {}, "gemini-3.8-flash-high": {}}}
	cfg := &config.Config{SDKConfig: config.SDKConfig{ForceModelPrefix: true}, OAuthExcludedModels: map[string][]string{"antigravity": {"gemini-3.8-flash-medium"}}, OAuthModelAlias: map[string][]config.OAuthModelAlias{"antigravity": {{Name: "gemini-3.8-flash", Alias: "flash"}, {Name: "gemini-3.8-flash-high", Alias: "legacy-flash"}}}}
	svc := &Service{cfg: cfg}
	models := svc.antigravityModelsForHintsWithConfig(auth, hints, cfg)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "antigravity", models)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	family := reg.GetModelInfo("tenant/flash", "antigravity")
	if family == nil {
		t.Fatalf("canonical alias/prefix not published: %+v", models)
	}
	if family.HiddenFromList || !reflect.DeepEqual(family.Thinking.Levels, []string{"low", "high"}) {
		t.Fatalf("family lost exclusion: %+v", family)
	}
	if family.AntigravityEffortModels["high"] != "gemini-3.8-flash-high" {
		t.Fatal("wire map changed by alias/prefix")
	}
	legacy := reg.GetModelInfo("tenant/legacy-flash", "antigravity")
	if legacy == nil || legacy.HiddenFromList {
		t.Fatal("operator-configured legacy alias was hidden")
	}
	for _, m := range models {
		if m.ID == "tenant/gemini-3.8-flash-low" && !m.HiddenFromList {
			t.Fatal("legacy wire ID advertised")
		}
	}
	cfg.OAuthExcludedModels["antigravity"] = []string{"gemini-3.8-flash"}
	models = svc.antigravityModelsForHintsWithConfig(auth, hints, cfg)
	for _, m := range models {
		if len(m.AntigravityEffortModels) > 0 {
			t.Fatal("excluded family published")
		}
	}
}

func TestAntigravityModelListModeReloadWithoutPluginHost(t *testing.T) {
	resetAntigravityCapabilityCache()
	t.Cleanup(resetAntigravityCapabilityCache)
	svc := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth, err := svc.coreManager.Register(t.Context(), antigravityTestAuth(t.Name(), "http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.WaitAntigravityProbes(); registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	hints := antigravityModelCapabilityHints{ModelIDs: map[string]struct{}{"gemini-3.8-flash-high": {}, "gemini-3.8-flash-medium": {}}, revision: 1}
	key := svc.antigravityCapabilityKey(auth)
	antigravityCapabilityMu.Lock()
	antigravityCapabilityCache[key] = antigravityCapabilityCacheEntry{hints: hints, expiresAt: time.Now().Add(antigravityCapabilityCacheTTL)}
	antigravityCapabilityMu.Unlock()
	for _, mode := range []string{"canonical", "variants", "both", "canonical"} {
		cfg := &config.Config{}
		cfg.Antigravity.ModelListMode = mode
		if !svc.applyConfigUpdateWithAuthSynthesis(t.Context(), cfg, false) {
			t.Fatal("reload failed")
		}
		models := registry.GetGlobalRegistry().GetModelsForClient(auth.ID)
		visible := map[string]bool{}
		for _, m := range models {
			if !m.HiddenFromList {
				visible[m.ID] = true
			}
		}
		if visible["gemini-3.8-flash"] != (mode != "variants") || visible["gemini-3.8-flash-high"] != (mode != "canonical") {
			t.Fatalf("%s: %v", mode, visible)
		}
	}
}
