package modelconfig

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestResolveModelInfoUsesSuffixFreeStaticCapabilities(t *testing.T) {
	info := ResolveModelInfo("claude-opus-4-6(high)", "claude", nil)
	if info == nil || info.Thinking == nil {
		t.Fatalf("ResolveModelInfo() = %+v, want inherited thinking support", info)
	}
	if info.ID != "claude-opus-4-6(high)" {
		t.Fatalf("model ID = %q, want configured upstream name", info.ID)
	}
	if info.UserDefined {
		t.Fatal("resolved capability snapshot must not be user-defined")
	}
}

func TestResolveModelInfoExplicitThinkingOverridesAndClones(t *testing.T) {
	support := &registry.ThinkingSupport{Levels: []string{" XHIGH ", "xhigh", " High "}}
	info := ResolveModelInfo("custom-model", "codex", support)
	if info == nil || info.Thinking == nil {
		t.Fatalf("ResolveModelInfo() = %+v, want explicit thinking support", info)
	}
	if got := info.Thinking.Levels; len(got) != 2 || got[0] != "xhigh" || got[1] != "high" {
		t.Fatalf("normalized levels = %v, want [xhigh high]", got)
	}
	support.Levels[0] = "low"
	if info.Thinking.Levels[0] != "xhigh" {
		t.Fatal("resolved thinking support shares mutable config storage")
	}
}

func TestNormalizeThinkingSupportDerivesSpecialLevelFlags(t *testing.T) {
	support := NormalizeThinkingSupport(&registry.ThinkingSupport{Levels: []string{"low", "none", "auto"}})
	if support == nil {
		t.Fatal("NormalizeThinkingSupport() = nil")
	}
	if !support.ZeroAllowed {
		t.Fatal("none level did not enable ZeroAllowed")
	}
	if !support.DynamicAllowed {
		t.Fatal("auto level did not enable DynamicAllowed")
	}
}

func TestResolveModelInfoUnknownModelKeepsMissingCapability(t *testing.T) {
	info := ResolveModelInfo("unknown-configured-model", "claude", nil)
	if info == nil {
		t.Fatal("ResolveModelInfo() = nil")
	}
	if info.Thinking != nil {
		t.Fatalf("unknown model thinking = %+v, want nil", info.Thinking)
	}
	if info.UserDefined {
		t.Fatal("unknown configured model must use its exact bound capability")
	}
}

func TestResolveModelInfoUsesCatalogFallbackForCustomEndpointModel(t *testing.T) {
	// A model reachable only through a custom endpoint has no static catalog
	// entry, which previously left it without any context window.
	if registry.LookupStaticModelInfo("deepseek-v4.1-flash") != nil {
		t.Skip("deepseek-v4.1-flash gained a static entry; pick another custom-only model")
	}

	info := ResolveModelInfo("deepseek-v4.1-flash", "openai-compatibility", nil)
	if info == nil {
		t.Fatal("ResolveModelInfo() = nil")
	}
	if info.ContextLength <= 0 {
		t.Errorf("context length = %d, want a catalog fallback value", info.ContextLength)
	}
	if info.MaxCompletionTokens <= 0 {
		t.Errorf("max completion tokens = %d, want a catalog fallback value", info.MaxCompletionTokens)
	}
	if info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		t.Fatalf("thinking = %+v, want catalog fallback levels", info.Thinking)
	}
}

func TestResolveModelInfoCatalogFallbackLeavesUnknownModelUnset(t *testing.T) {
	info := ResolveModelInfo("totally-unknown-model-xyz", "openai-compatibility", nil)
	if info == nil {
		t.Fatal("ResolveModelInfo() = nil")
	}
	if info.ContextLength != 0 || info.MaxCompletionTokens != 0 {
		t.Fatalf("capabilities = %d/%d, want nothing guessed", info.ContextLength, info.MaxCompletionTokens)
	}
	if info.Thinking != nil {
		t.Fatalf("thinking = %+v, want nil", info.Thinking)
	}
}

func TestResolveModelInfoWithAliasResolvesThroughAlias(t *testing.T) {
	info := ResolveModelInfoWithAlias("my-favourite-model", "muse-spark-1.3", "openai-compatibility", nil)
	if info == nil {
		t.Fatal("ResolveModelInfoWithAlias() = nil")
	}
	if info.ID != "my-favourite-model" {
		t.Fatalf("model ID = %q, want the upstream name", info.ID)
	}
	if info.ContextLength <= 0 {
		t.Errorf("context length = %d, want the aliased model's catalog value", info.ContextLength)
	}
}

func TestResolveModelInfoExplicitThinkingWinsOverCatalogFallback(t *testing.T) {
	support := &registry.ThinkingSupport{Levels: []string{"low"}}
	info := ResolveModelInfo("deepseek-v4.1-flash", "openai-compatibility", support)
	if info == nil || info.Thinking == nil {
		t.Fatalf("ResolveModelInfo() = %+v, want explicit thinking support", info)
	}
	if len(info.Thinking.Levels) != 1 || info.Thinking.Levels[0] != "low" {
		t.Fatalf("levels = %v, want only the configured level", info.Thinking.Levels)
	}
}

func TestResolveModelInfoLeavesBudgetOnlyStaticModelAlone(t *testing.T) {
	// claude-opus-4-6-thinking describes thinking as a budget range with no
	// levels. That empty list is deliberate: the provider appliers switch their
	// output format on it, so injecting catalog levels would flip the wire
	// format and bypass the Claude budget clamping.
	info := ResolveModelInfo("claude-opus-4-6-thinking", "openai", nil)
	if info == nil {
		t.Fatal("ResolveModelInfo() = nil")
	}
	if info.ContextLength != 200000 {
		t.Errorf("context length = %d, want the static catalog value to win", info.ContextLength)
	}
	if info.Thinking == nil {
		t.Fatal("budget-only thinking support must survive resolution")
	}
	if len(info.Thinking.Levels) != 0 {
		t.Fatalf("levels = %v, want none so the model keeps its budget format", info.Thinking.Levels)
	}
}

func TestResolveModelInfoLeavesBudgetOnlyGeminiModelAlone(t *testing.T) {
	// The same rule protects Gemini 2.5, which only accepts thinkingBudget.
	info := ResolveModelInfo("gemini-2.5-flash", "gemini", nil)
	if info == nil {
		t.Fatal("ResolveModelInfo() = nil")
	}
	if info.Thinking != nil && len(info.Thinking.Levels) > 0 {
		t.Fatalf("levels = %v, want none so Gemini 2.5 keeps thinkingBudget", info.Thinking.Levels)
	}
}
