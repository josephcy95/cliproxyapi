package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// excelProviderKey mirrors the executor provider key.
const excelProviderKey = "excel"

func synthesizeExcelAuths(t *testing.T, cfg *config.Config) []*coreauth.Auth {
	t.Helper()
	synth := NewConfigSynthesizer()
	auths, errSynthesize := synth.Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize returned an error: %v", errSynthesize)
	}
	out := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if auth != nil && auth.Provider == excelProviderKey {
			out = append(out, auth)
		}
	}
	return out
}

func TestSynthesizeExcelDirectCredential(t *testing.T) {
	cfg := &config.Config{
		ExcelKey: []config.ExcelKey{
			{
				AccessToken: "token-abc",
				AccountID:   "acct-1",
				BaseURL:     "https://example.invalid",
				Headers:     map[string]string{"X-Custom": "value"},
			},
		},
	}

	auths := synthesizeExcelAuths(t, cfg)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d excel auths, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != excelProviderKey {
		t.Errorf("Provider = %q, want %q", auth.Provider, excelProviderKey)
	}
	if auth.Status != coreauth.StatusActive {
		t.Errorf("Status = %q, want active", auth.Status)
	}
	// The executor reads the credential from these attributes.
	if got := auth.Attributes["api_key"]; got != "token-abc" {
		t.Errorf("api_key attribute = %q, want token-abc", got)
	}
	if got := auth.Attributes["account_id"]; got != "acct-1" {
		t.Errorf("account_id attribute = %q, want acct-1", got)
	}
	if got := auth.Attributes["base_url"]; got != "https://example.invalid" {
		t.Errorf("base_url attribute = %q", got)
	}
	if got := auth.Attributes["header:X-Custom"]; got != "value" {
		t.Errorf("custom header attribute = %q, want value", got)
	}
	// A direct credential must not opt into borrowing.
	if _, ok := auth.Metadata["use_codex_auths"]; ok {
		t.Error("a direct credential must not set use_codex_auths")
	}
}

func TestSynthesizeExcelBorrowsCodexAuths(t *testing.T) {
	cfg := &config.Config{
		ExcelKey: []config.ExcelKey{
			{UseCodexAuths: true},
		},
	}

	auths := synthesizeExcelAuths(t, cfg)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d excel auths, want 1", len(auths))
	}
	auth := auths[0]
	if useCodex, _ := auth.Metadata["use_codex_auths"].(bool); !useCodex {
		t.Fatalf("use_codex_auths not propagated: %+v", auth.Metadata)
	}
	// No token is stored in configuration in this mode.
	if _, ok := auth.Attributes["api_key"]; ok {
		t.Error("borrow mode must not carry an api_key attribute")
	}
}

func TestSynthesizeExcelBorrowModePin(t *testing.T) {
	cfg := &config.Config{
		ExcelKey: []config.ExcelKey{
			{UseCodexAuths: true, AccountID: "acct-pinned"},
		},
	}

	auths := synthesizeExcelAuths(t, cfg)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d excel auths, want 1", len(auths))
	}
	if got := auths[0].Metadata["preferred_account_id"]; got != "acct-pinned" {
		t.Fatalf("preferred_account_id = %v, want acct-pinned", got)
	}
}

func TestSynthesizeExcelSkipsUnusableEntries(t *testing.T) {
	cases := []struct {
		name  string
		entry config.ExcelKey
	}{
		{
			// A token without an account id is rejected by the backend, so it must
			// not produce a credential that fails at request time.
			name:  "token without account id",
			entry: config.ExcelKey{AccessToken: "token-abc"},
		},
		{
			// No token and no opt-in leaves nothing to authenticate with.
			name:  "nothing to authenticate with",
			entry: config.ExcelKey{},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := &config.Config{ExcelKey: []config.ExcelKey{testCase.entry}}
			if auths := synthesizeExcelAuths(t, cfg); len(auths) != 0 {
				t.Fatalf("expected no auth, got %d", len(auths))
			}
		})
	}
}

func TestSynthesizeExcelDisabled(t *testing.T) {
	cfg := &config.Config{
		ExcelKey: []config.ExcelKey{
			{UseCodexAuths: true, Disabled: true},
		},
	}
	if auths := synthesizeExcelAuths(t, cfg); len(auths) != 0 {
		t.Fatalf("a disabled entry must not be synthesized, got %d", len(auths))
	}
}

func TestSynthesizeExcelIDsAreStable(t *testing.T) {
	cfg := &config.Config{
		ExcelKey: []config.ExcelKey{
			{AccessToken: "token-abc", AccountID: "acct-1"},
		},
	}
	first := synthesizeExcelAuths(t, cfg)
	second := synthesizeExcelAuths(t, cfg)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("expected one auth per run, got %d and %d", len(first), len(second))
	}
	// A stable id keeps model registrations and cooldown state attached across
	// config reloads instead of orphaning them.
	if first[0].ID != second[0].ID {
		t.Fatalf("auth id is not stable: %q vs %q", first[0].ID, second[0].ID)
	}
}
