package gemini

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
)

// Output format follows len(Thinking.Levels): Gemini 3.x uses thinkingLevel
// while Gemini 2.5 uses thinkingBudget. A budget-shaped entry must therefore
// keep its empty level list and its budget range intact, because injecting
// catalog levels would reroute these paths to the Gemini 3.x format.

func TestBudgetShapedModelKeepsItsCapabilityEntry(t *testing.T) {
	info := registry.LookupStaticModelInfo("gemini-2.5-flash")
	if info == nil {
		t.Fatal("gemini-2.5-flash missing from the static catalog")
	}
	if info.Thinking == nil {
		t.Fatal("a nil entry makes the applier drop thinking entirely, losing the budget range")
	}
	if len(info.Thinking.Levels) != 0 {
		t.Fatalf("levels = %v, want none so Gemini 2.5 keeps thinkingBudget", info.Thinking.Levels)
	}
	if info.Thinking.Max <= 0 {
		t.Fatalf("max budget = %d, want the catalog range preserved", info.Thinking.Max)
	}
}

func TestModeNoneUsesBudgetFormatForBudgetShapedModel(t *testing.T) {
	info := registry.LookupStaticModelInfo("gemini-2.5-flash")
	if info == nil || info.Thinking == nil {
		t.Fatal("gemini-2.5-flash missing its static capability entry")
	}

	// ModeNone routes on len(Levels). With the empty list this is the
	// thinkingBudget path; injected levels would send it to thinkingLevel.
	out, err := NewApplier().Apply([]byte(`{}`), thinking.ThinkingConfig{
		Mode:  thinking.ModeNone,
		Level: thinking.LevelNone,
	}, info)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if level := gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel"); level.Exists() {
		t.Fatalf("thinkingLevel is a Gemini 3.x field, got %s", out)
	}
	if !gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingBudget").Exists() {
		t.Fatalf("expected the thinkingBudget path, got %s", out)
	}
}
