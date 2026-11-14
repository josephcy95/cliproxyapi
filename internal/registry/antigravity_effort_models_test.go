package registry

import (
	"reflect"
	"testing"
)

func TestAntigravityEffortFamilies(t *testing.T) {
	template := &ModelInfo{ID: "gemini-3.8-flash-high", Type: "antigravity", DisplayName: "Gemini 3.8 Flash (High)", Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high"}}}
	ids := map[string]struct{}{"gemini-3.8-flash-low": {}, "gemini-3.8-flash-medium": {}, "gemini-3.8-flash-high": {}}
	expanded := ExpandAntigravityEffortModels([]*ModelInfo{template}, []*ModelInfo{template}, ids)
	models := GroupAntigravityEffortModels(expanded)
	r := newTestModelRegistry()
	r.RegisterClient("effort-account", "antigravity", models)
	for _, format := range []string{"openai", "claude", "gemini"} {
		listed := r.GetAvailableModels(format)
		if len(listed) != 1 {
			t.Fatalf("%s: listed %d models, want one family", format, len(listed))
		}
	}
	info := r.GetModelInfo("gemini-3.8-flash", "antigravity")
	if info == nil || !reflect.DeepEqual(info.Thinking.Levels, []string{"low", "medium", "high"}) {
		t.Fatalf("family capabilities: %+v", info)
	}
	if info.AntigravityEffortModels["medium"] != "gemini-3.8-flash-medium" {
		t.Fatal("missing medium route")
	}
	if !r.ClientSupportsModel("effort-account", template.ID) {
		t.Fatal("legacy route lost")
	}
	info.AntigravityEffortModels["medium"] = "mutated"
	if r.GetModelInfo("gemini-3.8-flash", "antigravity").AntigravityEffortModels["medium"] == "mutated" {
		t.Fatal("shared mutable routes")
	}
	if template.HiddenFromList || template.ID != "gemini-3.8-flash-high" {
		t.Fatal("mutated source catalog")
	}
}

func TestAntigravityEffortEntitlements(t *testing.T) {
	source := []*ModelInfo{{ID: "gemini-4-argon-high", Thinking: &ThinkingSupport{Levels: []string{"low", "medium", "high"}}}, {ID: "gemini-3.8-flash-tiered"}, {ID: "gemini-3-flash"}}
	models := GroupAntigravityEffortModels(source)
	for _, m := range models {
		if m.ID == "gemini-4-argon" && !reflect.DeepEqual(m.Thinking.Levels, []string{"high"}) {
			t.Fatalf("advertised unentitled efforts: %v", m.Thinking.Levels)
		}
		if (m.ID == "gemini-3.8-flash-tiered" || m.ID == "gemini-3-flash") && m.HiddenFromList {
			t.Fatal("hid distinct route")
		}
	}
}

func TestAntigravityEffortDiscoveryModes(t *testing.T) {
	for _, mode := range []string{"canonical", "variants", "both", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			models := GroupAntigravityEffortModelsWithMode([]*ModelInfo{
				{ID: "gemini-future-medium", Thinking: &ThinkingSupport{}},
				{ID: "gemini-future-xhigh", Thinking: &ThinkingSupport{}},
			}, mode)
			if len(models) != 3 {
				t.Fatalf("lost callable routes: %v", models)
			}
			visible := map[string]bool{}
			for _, m := range models {
				if !m.HiddenFromList {
					visible[m.ID] = true
				}
			}
			if visible["gemini-future"] != (mode != "variants") {
				t.Fatal(visible)
			}
			for _, effort := range []string{"medium", "xhigh"} {
				if visible["gemini-future-"+effort] != (mode == "variants" || mode == "both") {
					t.Fatal(visible)
				}
			}
			r := newTestModelRegistry()
			r.RegisterClient("account", "antigravity", models)
			for _, format := range []string{"openai", "claude", "gemini", "codex"} {
				listed := r.GetAvailableModels(format)
				if len(listed) != len(visible) {
					t.Fatalf("%s: got %d, want %d", format, len(listed), len(visible))
				}
			}
			if visible["gemini-future-high"] {
				t.Fatal("invented unsupported variant")
			}
		})
	}
}
