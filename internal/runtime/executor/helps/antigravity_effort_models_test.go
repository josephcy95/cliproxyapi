package helps

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"testing"
)

func TestResolveAntigravityEffortModel(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	models := registry.GroupAntigravityEffortModels([]*registry.ModelInfo{
		{ID: "gemini-4-hydrogen-low", Thinking: &registry.ThinkingSupport{}},
		{ID: "gemini-4-hydrogen-medium", Thinking: &registry.ThinkingSupport{}},
		{ID: "gemini-4-hydrogen-high", Thinking: &registry.ThinkingSupport{}},
		{ID: "gemini-4-hydrogen-xhigh", Thinking: &registry.ThinkingSupport{}},
	})
	reg.RegisterClient(t.Name(), "antigravity", models)
	t.Cleanup(func() { reg.UnregisterClient(t.Name()) })
	for _, tc := range []struct {
		model, level, want string
		fail               bool
	}{
		{"gemini-4-hydrogen", "low", "gemini-4-hydrogen-low", false},
		{"gemini-4-hydrogen", "medium", "gemini-4-hydrogen-medium", false},
		{"gemini-4-hydrogen", "xhigh", "gemini-4-hydrogen-xhigh", false},
		{"gemini-4-hydrogen", "", "gemini-4-hydrogen-xhigh", false},
		{"gemini-4-hydrogen", "max", "", true},
		{"gemini-4-hydrogen-xhigh", "low", "gemini-4-hydrogen-xhigh", false},
		{"gemini-3.8-flash-tiered", "medium", "gemini-3.8-flash-tiered", false},
	} {
		doc := ojson.NewDoc([]byte(`{}`))
		if tc.level != "" {
			doc.Set("request.generationConfig.thinkingConfig.thinkingLevel", tc.level)
		}
		got, err := ResolveAntigravityEffortModel(t.Name(), tc.model, doc)
		if (err != nil) != tc.fail || got != tc.want {
			t.Fatalf("%s/%s = %q, %v; want %q, fail=%v", tc.model, tc.level, got, err, tc.want, tc.fail)
		}
	}
	// Account-specific routing must not borrow another account's effort variants.
	reg.RegisterClient(t.Name()+"-restricted", "antigravity", registry.GroupAntigravityEffortModels(models[1:2]))
	t.Cleanup(func() { reg.UnregisterClient(t.Name() + "-restricted") })
	doc := ojson.NewDoc([]byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"medium"}}}}`))
	if _, err := ResolveAntigravityEffortModel(t.Name()+"-restricted", "gemini-4-hydrogen", doc); err == nil {
		t.Fatal("borrowed medium from another account")
	}
}
