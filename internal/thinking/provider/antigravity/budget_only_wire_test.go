package antigravity

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
)

// This applier chooses its output format from len(Thinking.Levels): a model with
// levels sends thinkingLevel while a budget-shaped model sends thinkingBudget
// and keeps the Claude budget clamping. Injecting catalog levels into a
// budget-shaped entry would therefore flip the wire format.

func TestApplyLevelKeepsBudgetFormatForBudgetShapedModel(t *testing.T) {
	info := registry.LookupStaticModelInfo("claude-opus-4-6-thinking")
	if info == nil || info.Thinking == nil {
		t.Fatal("claude-opus-4-6-thinking missing its static capability entry")
	}
	if len(info.Thinking.Levels) != 0 {
		t.Fatalf("levels = %v, want none so this model keeps thinkingBudget", info.Thinking.Levels)
	}

	out, err := NewApplier().Apply([]byte(`{}`), thinking.ThinkingConfig{
		Mode:   thinking.ModeLevel,
		Level:  thinking.LevelHigh,
		Budget: 8192,
	}, info)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if level := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel"); level.Exists() {
		t.Fatalf("thinkingLevel sent for a budget-shaped model: %s", out)
	}
	if budget := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget"); !budget.Exists() {
		t.Fatalf("expected thinkingBudget, got %s", out)
	}
}

func TestApplyLevelUsesLevelFormatForLevelCapableModel(t *testing.T) {
	info := registry.LookupStaticModelInfo("claude-sonnet-4-6")
	if info == nil || info.Thinking == nil {
		t.Fatal("claude-sonnet-4-6 missing its static capability entry")
	}
	if len(info.Thinking.Levels) == 0 {
		t.Fatal("expected discrete levels on this model")
	}

	out, err := NewApplier().Apply([]byte(`{}`), thinking.ThinkingConfig{
		Mode:  thinking.ModeLevel,
		Level: thinking.LevelHigh,
	}, info)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "high" {
		t.Fatalf("thinkingLevel = %q, want high for a level-capable model: %s", got, out)
	}
}
