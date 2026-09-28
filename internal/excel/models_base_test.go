package excel

import "testing"

// TestDefaultModelsDeclareBaseModel locks the rule that every Excel alias maps
// onto a public Codex model, so the alias can inherit that model's advertised
// context window and token limits instead of looking like a narrower model.
func TestDefaultModelsDeclareBaseModel(t *testing.T) {
	want := map[string]string{
		"gpt-6-astra-excel":   "gpt-6-astra",
		"gpt-6-sol-excel":     "gpt-6-sol",
		"gpt-6-luna-excel":    "gpt-6-luna",
		"gpt-5.6-luna-excel":  "gpt-5.6-luna",
		"gpt-5.6-terra-excel": "gpt-5.6-terra",
		"gpt-5.6-sol-excel":   "gpt-5.6-sol",
	}
	if len(DefaultModels) != len(want) {
		t.Fatalf("DefaultModels has %d entries, want %d", len(DefaultModels), len(want))
	}
	for _, model := range DefaultModels {
		expected, ok := want[model.Alias]
		if !ok {
			t.Fatalf("unexpected alias %q", model.Alias)
		}
		if model.BaseModel != expected {
			t.Errorf("%s BaseModel = %q, want %q", model.Alias, model.BaseModel, expected)
		}
		if model.BaseModel != model.Slug {
			t.Errorf("%s BaseModel %q should equal upstream slug %q", model.Alias, model.BaseModel, model.Slug)
		}
		// The public route accepts "max"/"ultra"; this backend rejects them, so
		// no alias may advertise them.
		for _, effort := range model.Efforts {
			if effort == "max" || effort == "ultra" {
				t.Errorf("%s advertises unsupported effort %q", model.Alias, effort)
			}
		}
	}
}

func TestBaseModelFor(t *testing.T) {
	for _, model := range []string{"gpt-6-sol-excel", "GPT-6-SOL-EXCEL", "kiro/gpt-6-sol-excel"} {
		if base, ok := BaseModelFor(model); !ok || base != "gpt-6-sol" {
			t.Errorf("BaseModelFor(%q) = %q, %v; want gpt-6-sol, true", model, base, ok)
		}
	}
	// A bare upstream slug is not an alias and must not resolve as one.
	for _, model := range []string{"", "gpt-6-sol", "unknown-excel"} {
		if base, ok := BaseModelFor(model); ok {
			t.Errorf("BaseModelFor(%q) = %q, want false", model, base)
		}
	}
}
