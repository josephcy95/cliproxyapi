package handlers

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/excel"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// applyModelAuthPolicy records auth-selection constraints implied by the
// requested model.
//
// Excel aliases require paid Codex OAuth, not just the absence of a free plan.
func applyModelAuthPolicy(meta map[string]any, modelName string) {
	// Guard on nil, not length: a caller may legitimately pass an empty map and
	// the policy still has to be recorded.
	if meta == nil {
		return
	}
	if excel.IsRoute(modelName) {
		meta[coreexecutor.DisallowFreeAuthMetadataKey] = true
		meta[coreexecutor.RequireExcelOAuthMetadataKey] = true
	}
}
