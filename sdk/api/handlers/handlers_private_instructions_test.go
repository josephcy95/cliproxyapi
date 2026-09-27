package handlers

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestApplyPrivateCodexInstructionModelWithoutPrefixSuffix(t *testing.T) {
	usePrefixSuffix := false
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Instructions: internalconfig.CodexInstructionsConfig{
		Enabled:         true,
		UsePrefixSuffix: &usePrefixSuffix,
	}}})

	model, metadata := applyPrivateCodexInstructionModel(manager, "gpt-5.5", nil)
	if model != "gpt-5.5" {
		t.Fatalf("model = %q, want %q", model, "gpt-5.5")
	}
	if got := metadata[coreexecutor.CodexPrivateInstructionsMetadataKey]; got != true {
		t.Fatalf("private instruction metadata = %#v, want true", got)
	}
}

func TestApplyPrivateCodexInstructionModelKeepsPrefixSuffixEnabledByDefault(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{})

	model, metadata := applyPrivateCodexInstructionModel(manager, "gpt-5.5", nil)
	if model != "gpt-5.5" {
		t.Fatalf("model = %q, want %q", model, "gpt-5.5")
	}
	if metadata != nil {
		t.Fatalf("metadata = %#v, want nil", metadata)
	}
}

func TestApplyPrivateCodexInstructionModelWithoutPrefixSuffixSkipsUnmatchedModel(t *testing.T) {
	usePrefixSuffix := false
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Instructions: internalconfig.CodexInstructionsConfig{
		Enabled:         true,
		UsePrefixSuffix: &usePrefixSuffix,
		Models:          []string{"gpt-5*"},
	}}})

	model, metadata := applyPrivateCodexInstructionModel(manager, "grok-4.5", nil)
	if model != "grok-4.5" {
		t.Fatalf("model = %q, want %q", model, "grok-4.5")
	}
	if metadata != nil {
		t.Fatalf("metadata = %#v, want nil", metadata)
	}
}

func TestStripPrivateCodexMarkersForProviderLookup(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Instructions: internalconfig.CodexInstructionsConfig{
		Enabled: true,
		RequestMarkers: internalconfig.CodexInstructionMarkersConfig{
			Prefixes: []string{"private/"},
			Suffixes: []string{"-jb", "-private"},
		},
	}}})

	cases := []struct {
		in   string
		want string
	}{
		{"kiro/gpt-5.6-luna-jb", "kiro/gpt-5.6-luna"},
		{"private/kiro/gpt-5.6-luna", "kiro/gpt-5.6-luna"},
		{"kiro/gpt-5.6-luna-private", "kiro/gpt-5.6-luna"},
		{"kiro/gpt-5.6-luna", "kiro/gpt-5.6-luna"},
	}
	for _, tc := range cases {
		if got := stripPrivateCodexMarkersForProviderLookup(manager, tc.in); got != tc.want {
			t.Fatalf("strip(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestApplyPrivateCodexInstructionModelCustomSuffix(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Instructions: internalconfig.CodexInstructionsConfig{
		Enabled: true,
		RequestMarkers: internalconfig.CodexInstructionMarkersConfig{
			Suffixes: []string{"-jb"},
		},
	}}})

	model, metadata := applyPrivateCodexInstructionModel(manager, "kiro/gpt-5.6-luna-jb(high)", nil)
	if model != "kiro/gpt-5.6-luna(high)" {
		t.Fatalf("model = %q, want kiro/gpt-5.6-luna(high)", model)
	}
	if got := metadata[coreexecutor.CodexPrivateInstructionsMetadataKey]; got != true {
		t.Fatalf("private metadata = %#v, want true", got)
	}
}
