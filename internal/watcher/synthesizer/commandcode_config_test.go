package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func synthesizeCommandCodeAuths(t *testing.T, cfg *config.Config) []*coreauth.Auth {
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
		if auth != nil && auth.Provider == constant.CommandCode {
			out = append(out, auth)
		}
	}
	return out
}

func TestSynthesizeCommandCodeAttributes(t *testing.T) {
	cfg := &config.Config{
		CommandCodeKey: []config.CommandCodeKey{
			{
				APIKey:  "user_test",
				BaseURL: "https://api.commandcode.ai",
				Prefix:  "cc",
				Headers: map[string]string{"X-Custom-Header": "custom-value"},
				Models: []config.CommandCodeModel{
					{Name: "deepseek/deepseek-v4-flash", Alias: "cc-flash"},
				},
			},
		},
	}

	auths := synthesizeCommandCodeAuths(t, cfg)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d commandcode auths, want 1", len(auths))
	}
	auth := auths[0]

	if auth.Provider != constant.CommandCode {
		t.Errorf("Provider = %q, want %q", auth.Provider, constant.CommandCode)
	}
	if auth.Prefix != "cc" {
		t.Errorf("Prefix = %q, want cc", auth.Prefix)
	}
	if auth.Status != coreauth.StatusActive {
		t.Errorf("Status = %q, want active", auth.Status)
	}
	if got := auth.Attributes["api_key"]; got != "user_test" {
		t.Errorf("api_key = %q", got)
	}
	// The base URL is required by the executor, and recording it must not make
	// the auth look like an OpenAI-compat credential.
	if got := auth.Attributes["base_url"]; got != "https://api.commandcode.ai" {
		t.Errorf("base_url = %q", got)
	}
	if got := auth.Attributes["config_index"]; got != "0" {
		t.Errorf("config_index = %q", got)
	}
	if got := auth.Attributes["header:X-Custom-Header"]; got != "custom-value" {
		t.Errorf("header:X-Custom-Header = %q (custom headers must land in the header: prefix)", got)
	}
	if got := auth.Attributes["models_hash"]; got == "" {
		t.Error("models_hash should be present so config reloads detect alias changes")
	}
	if got := auth.Attributes["source"]; got == "" {
		t.Error("source should be recorded")
	}
}

// TestSynthesizeCommandCodeIsNotOpenAICompat is the regression pin for the
// registration order in registerExecutorForAuth: that function consults
// openAICompatInfoFromAuth BEFORE its provider switch and returns early when the
// auth looks compat-shaped. A "compat_name" attribute (or a
// "openai-compatibility" provider) would therefore bind these auths to the
// OpenAI-compat executor and the dedicated commandcode case would never run.
//
// The unexported helper lives in package cliproxy, so this test asserts the
// equivalent condition on the attributes the synthesizer produces.
func TestSynthesizeCommandCodeIsNotOpenAICompat(t *testing.T) {
	cfg := &config.Config{
		CommandCodeKey: []config.CommandCodeKey{
			{
				APIKey:  "user_test",
				BaseURL: "https://api.commandcode.ai",
				Models:  []config.CommandCodeModel{{Name: "deepseek/deepseek-v4-flash", Alias: "cc-flash"}},
			},
		},
	}

	auths := synthesizeCommandCodeAuths(t, cfg)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d commandcode auths, want 1", len(auths))
	}
	for _, auth := range auths {
		if got := auth.Attributes["compat_name"]; got != "" {
			t.Errorf("compat_name must be empty, got %q: it would route the auth to the OpenAI-compat executor", got)
		}
		if _, present := auth.Attributes["provider_key"]; present {
			// provider_key alone is harmless, but the synthesizer must not set it
			// either because it signals an OpenAI-compatible provider key.
			t.Errorf("provider_key must not be set: %q", auth.Attributes["provider_key"])
		}
		if auth.Provider == "openai-compatibility" {
			t.Error("Provider must not be openai-compatibility")
		}
		if auth.Provider != constant.CommandCode {
			t.Errorf("Provider = %q, want %q", auth.Provider, constant.CommandCode)
		}
	}
}

func TestSynthesizeCommandCodeSkipsEmptyAndDisabled(t *testing.T) {
	cfg := &config.Config{
		CommandCodeKey: []config.CommandCodeKey{
			{},
			{APIKey: "user_ok", BaseURL: "https://api.commandcode.ai"},
		},
	}
	auths := synthesizeCommandCodeAuths(t, cfg)
	if len(auths) != 1 {
		t.Fatalf("synthesized %d commandcode auths, want 1 (the entry with neither key nor base-url is skipped)", len(auths))
	}
	if got := auths[0].Attributes["config_index"]; got != "1" {
		t.Errorf("config_index = %q, want 1", got)
	}
}

func TestSynthesizeCommandCodeNilWhenUnconfigured(t *testing.T) {
	auths := synthesizeCommandCodeAuths(t, &config.Config{})
	if len(auths) != 0 {
		t.Fatalf("synthesized %d commandcode auths from an empty config, want 0", len(auths))
	}
}
