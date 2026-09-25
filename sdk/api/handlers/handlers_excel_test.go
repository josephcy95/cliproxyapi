package handlers

import (
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestApplyModelAuthPolicyExcludesFreeAccountsForExcel locks the rule that the
// Excel-backed aliases are not available to free-tier accounts. Without it the
// scheduler would consider a free credential and the request would fail
// upstream after consuming an attempt.
func TestApplyModelAuthPolicyExcludesFreeAccountsForExcel(t *testing.T) {
	for _, model := range []string{
		"gpt-6-astra-excel",
		"gpt-5.6-luna-excel",
		"gpt-5.6-terra-excel",
		"gpt-5.6-sol-excel",
	} {
		meta := map[string]any{}
		applyModelAuthPolicy(meta, model)
		if meta[coreexecutor.DisallowFreeAuthMetadataKey] != true {
			t.Errorf("model %q did not exclude free credentials: %v", model, meta)
		}
	}
}

func TestApplyModelAuthPolicyLeavesOtherModelsAlone(t *testing.T) {
	for _, model := range []string{
		"gpt-5.6-luna",
		"gpt-5.5-codex",
		"gpt-6-astra",
		"gpt-image-2",
		"",
	} {
		meta := map[string]any{}
		applyModelAuthPolicy(meta, model)
		if _, set := meta[coreexecutor.DisallowFreeAuthMetadataKey]; set {
			t.Errorf("model %q must keep its normal credential pool", model)
		}
	}
	// A nil map must not panic.
	applyModelAuthPolicy(nil, "gpt-5.6-sol-excel")
}
