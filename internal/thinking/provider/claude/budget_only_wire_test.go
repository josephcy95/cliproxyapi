package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
)

// The provider appliers choose their wire format from ModelInfo.Thinking, so a
// capability entry that lists no reasoning levels must stay that way. Injecting
// catalog levels would make an older Claude model look adaptive-thinking capable.

func TestApplyLevelUsesBudgetTokensForNonAdaptiveModel(t *testing.T) {
	info := registry.LookupStaticModelInfo("claude-opus-4-5-20251101")
	if info == nil {
		t.Fatal("claude-opus-4-5-20251101 missing from the static catalog")
	}
	if info.Thinking == nil {
		t.Fatal("expected budget-shaped thinking support")
	}
	if len(info.Thinking.Levels) != 0 {
		t.Fatalf("levels = %v, want none: this model predates adaptive thinking", info.Thinking.Levels)
	}

	applier := NewApplier()
	out, err := applier.Apply([]byte(`{}`), thinking.ThinkingConfig{
		Mode:  thinking.ModeLevel,
		Level: thinking.LevelHigh,
	}, info)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if gjson.GetBytes(out, "thinking.type").String() == "adaptive" {
		t.Fatalf("adaptive thinking sent for a non-adaptive model: %s", out)
	}
	if effort := gjson.GetBytes(out, "output_config.effort"); effort.Exists() {
		t.Fatalf("output_config.effort is a Claude 4.6 feature, got %s", out)
	}
	if budget := gjson.GetBytes(out, "thinking.budget_tokens"); !budget.Exists() {
		t.Fatalf("expected budget_tokens fallback, got %s", out)
	}
}

func TestApplyLevelUsesAdaptiveForLevelCapableModel(t *testing.T) {
	info := registry.LookupStaticModelInfo("claude-opus-4-6")
	if info == nil {
		t.Fatal("claude-opus-4-6 missing from the static catalog")
	}
	if info.Thinking == nil || len(info.Thinking.Levels) == 0 {
		t.Fatalf("thinking = %+v, want discrete levels for an adaptive model", info.Thinking)
	}

	applier := NewApplier()
	out, err := applier.Apply([]byte(`{}`), thinking.ThinkingConfig{
		Mode:  thinking.ModeLevel,
		Level: thinking.LevelHigh,
	}, info)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive for a level-capable model: %s", got, out)
	}
}
