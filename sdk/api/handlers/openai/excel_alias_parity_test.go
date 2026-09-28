package openai

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/excel"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

// TestExcelAliasesMirrorBaseModelWindows locks the contract that an Excel alias
// advertises the same context window, maximum context window and token limits as
// the public Codex model it serves.
//
// The Excel backend is an alternative endpoint for the same model, so a client
// that switches to the alias must not see a narrower window. The only intended
// difference is that this backend rejects the "max" and "ultra" reasoning
// efforts.
func TestExcelAliasesMirrorBaseModelWindows(t *testing.T) {
	clientID := "excel-alias-window-parity-test"
	modelRegistry := registry.GetGlobalRegistry()

	var registered []*registry.ModelInfo
	for _, model := range excel.DefaultModels {
		base, ok := excel.BaseModelFor(model.Alias)
		if !ok {
			t.Fatalf("%s has no base model", model.Alias)
		}
		baseInfo := registry.LookupModelInfo(base)
		if baseInfo == nil {
			t.Fatalf("base model %q not resolvable", base)
		}
		// Register both the public model and the alias, mirroring how the
		// service exposes them to a Codex client.
		registered = append(registered,
			&registry.ModelInfo{
				ID:                  base,
				Object:              "model",
				Type:                "codex",
				ContextLength:       baseInfo.ContextLength,
				MaxCompletionTokens: baseInfo.MaxCompletionTokens,
			},
		)
	}
	registered = append(registered, helps.ExcelModels()...)
	modelRegistry.RegisterClient(clientID, "codex", registered)
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
	handler := NewOpenAIAPIHandler(base)
	resp := handler.codexClientModelsResponse("0.155.0")
	models, ok := resp["models"].([]map[string]any)
	if !ok {
		t.Fatalf("models type = %T, want []map[string]any", resp["models"])
	}
	entries := make(map[string]map[string]any, len(models))
	for _, entry := range models {
		if slug, okSlug := entry["slug"].(string); okSlug {
			entries[slug] = entry
		}
	}

	for _, model := range excel.DefaultModels {
		aliasEntry := entries[model.Alias]
		if aliasEntry == nil {
			t.Errorf("alias %q missing from catalog", model.Alias)
			continue
		}
		baseEntry := entries[model.BaseModel]
		if baseEntry == nil {
			t.Errorf("base model %q missing from catalog", model.BaseModel)
			continue
		}

		for _, field := range []string{"context_window", "max_context_window", "max_tokens"} {
			aliasValue := testIntModelValue(aliasEntry, field)
			baseValue := testIntModelValue(baseEntry, field)
			if aliasValue != baseValue {
				t.Errorf("%s %s = %d, want %d (same as %s)",
					model.Alias, field, aliasValue, baseValue, model.BaseModel)
			}
		}
		if aliasContext := testIntModelValue(aliasEntry, "context_window"); aliasContext <= 0 {
			t.Errorf("%s advertises no context_window", model.Alias)
		}

		// Reasoning levels are the one intended difference: this backend rejects
		// "max" and "ultra".
		for _, level := range testReasoningEfforts(aliasEntry) {
			if level == "max" || level == "ultra" {
				t.Errorf("%s advertises unsupported reasoning level %q", model.Alias, level)
			}
		}
	}
}

func testReasoningEfforts(entry map[string]any) []string {
	raw, ok := entry["supported_reasoning_levels"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		level, okLevel := item.(map[string]any)
		if !okLevel {
			continue
		}
		if effort, okEffort := level["effort"].(string); okEffort {
			out = append(out, effort)
		}
	}
	return out
}
