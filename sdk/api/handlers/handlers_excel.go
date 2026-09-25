package handlers

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/excel"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// applyModelAuthPolicy records auth-selection constraints implied by the
// requested model.
//
// The Excel-backed aliases are not available to free-tier accounts, so the
// scheduler must not consider one. That is enforced through the same
// disallow-free-auth metadata the image endpoints use, which keeps the decision
// in the request rather than in each selection path.
func applyModelAuthPolicy(meta map[string]any, modelName string) {
	// Guard on nil, not length: a caller may legitimately pass an empty map and
	// the policy still has to be recorded.
	if meta == nil {
		return
	}
	if excel.IsAlias(modelName) {
		meta[coreexecutor.DisallowFreeAuthMetadataKey] = true
	}
}
