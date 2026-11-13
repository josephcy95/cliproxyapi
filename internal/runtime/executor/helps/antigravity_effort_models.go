package helps

import (
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// ResolveAntigravityEffortModel uses the selected account's routes, never another
// account's union of entitlements. Legacy suffixed IDs remain literal wire IDs.
func ResolveAntigravityEffortModel(authID, model string, doc *ojson.Doc) (string, error) {
	for _, info := range registry.GetGlobalRegistry().GetModelsForClient(authID) {
		if (info.ID != model && info.MetadataModelID != model) || len(info.AntigravityEffortModels) == 0 {
			continue
		}
		effort := strings.ToLower(doc.Get("request.generationConfig.thinkingConfig.thinkingLevel").String())
		if effort == "" {
			effort = info.AntigravityDefaultEffort
		}
		wire, ok := info.AntigravityEffortModels[effort]
		if !ok {
			return "", fmt.Errorf("Antigravity model %s does not support effort %s on this account", model, effort)
		}
		return wire, nil
	}
	return model, nil
}
