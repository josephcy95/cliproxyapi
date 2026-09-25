package helps

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/excel"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// ExcelProvider is the ownership label used for the Excel-backed catalog
// entries. They are Codex models for routing, credential selection, usage and
// cooldown; this only marks which upstream conversation serves them.
const ExcelProvider = "openai"

// ExcelModels returns the static catalog for the Excel-backed aliases.
//
// The catalog is static because the backend exposes no model listing endpoint;
// the alias set is a fixed mapping onto backend slugs.
func ExcelModels() []*registry.ModelInfo {
	now := time.Now().Unix()
	out := make([]*registry.ModelInfo, 0, len(excel.DefaultModels))
	for _, model := range excel.DefaultModels {
		out = append(out, &registry.ModelInfo{
			ID:                  model.Alias,
			Object:              "model",
			Created:             now,
			OwnedBy:             ExcelProvider,
			Type:                "codex",
			DisplayName:         model.DisplayName,
			ContextLength:       model.ContextLengthValue(),
			MaxCompletionTokens: model.ContextLengthValue(),
			Thinking:            &registry.ThinkingSupport{Levels: append([]string(nil), model.Efforts...)},
		})
	}
	return out
}
