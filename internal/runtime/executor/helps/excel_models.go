package helps

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/excel"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// ExcelProvider is the ownership label used for the Excel-backed catalog
// entries. They are Codex models for routing, credential selection, usage and
// cooldown; this only marks which upstream conversation serves them.
const ExcelProvider = "openai"

// ExcelModels returns the static catalog for the Excel-backed aliases.
//
// The catalog is static because the backend exposes no model listing endpoint;
// the alias set is a fixed mapping onto backend slugs.
//
// Each alias is the same underlying model as a public Codex model, reached over
// a different endpoint, so it advertises that model's context window and token
// limits rather than a guess. Only the reasoning efforts stay alias-specific,
// because this backend rejects "max" and "ultra".
func ExcelModels() []*registry.ModelInfo {
	now := time.Now().Unix()
	out := make([]*registry.ModelInfo, 0, len(excel.DefaultModels))
	for _, model := range excel.DefaultModels {
		contextLength := model.ContextLengthValue()
		maxCompletionTokens := 0
		maxContextLength := 0
		if base, ok := excel.BaseModelFor(model.Alias); ok {
			if info := registry.LookupModelInfo(base); info != nil {
				if info.ContextLength > 0 {
					contextLength = info.ContextLength
				}
				if info.MaxCompletionTokens > 0 {
					maxCompletionTokens = info.MaxCompletionTokens
				}
				if info.MaxContextLength > 0 {
					maxContextLength = info.MaxContextLength
				}
			}
		}
		out = append(out, &registry.ModelInfo{
			ID:                  model.Alias,
			Object:              "model",
			Created:             now,
			OwnedBy:             ExcelProvider,
			Type:                "codex",
			DisplayName:         model.DisplayName,
			ContextLength:       contextLength,
			MaxContextLength:    maxContextLength,
			MaxCompletionTokens: maxCompletionTokens,
			Thinking:            &registry.ThinkingSupport{Levels: append([]string(nil), model.Efforts...)},
		})
	}
	return out
}
