package cliproxy

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// synthesizeCommandCodeConfigAuth builds the auth entry the config synthesizer
// produces for one commandcode-api-key entry.
func synthesizeCommandCodeConfigAuth(t *testing.T, cfg *config.Config) *coreauth.Auth {
	t.Helper()
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if errSynthesize != nil {
		t.Fatalf("Synthesize returned an error: %v", errSynthesize)
	}
	for _, auth := range auths {
		if auth != nil && auth.Provider == constant.CommandCode {
			return auth
		}
	}
	t.Fatal("the synthesizer produced no commandcode auth")
	return nil
}

func commandCodeRegistrationConfig() *config.Config {
	return &config.Config{
		CommandCodeKey: []config.CommandCodeKey{
			{
				APIKey:  "user_test",
				BaseURL: "https://api.commandcode.ai",
				Models:  []config.CommandCodeModel{{Name: "deepseek/deepseek-v4-flash", Alias: "cc-flash"}},
			},
		},
	}
}

// TestCommandCodeAuthIsNotOpenAICompatAuth is the regression pin for the branch
// order in registerExecutorForAuth: openAICompatInfoFromAuth is consulted BEFORE
// the provider switch and returns early when the auth looks compat-shaped.
// A "compat_name" attribute would therefore bind these auths to the OpenAI-compat
// executor and the "commandcode" case would never run.
func TestCommandCodeAuthIsNotOpenAICompatAuth(t *testing.T) {
	auth := synthesizeCommandCodeConfigAuth(t, commandCodeRegistrationConfig())

	if providerKey, compatName, isCompat := openAICompatInfoFromAuth(auth); isCompat {
		t.Fatalf("commandcode auth was treated as an OpenAI-compat auth (providerKey=%q compatName=%q); "+
			"registerExecutorForAuth would never reach the commandcode case", providerKey, compatName)
	}
}

// TestCommandCodeAuthBindsCommandCodeExecutor proves the end-to-end wiring: the
// synthesized auth must bind the dedicated executor, not an OpenAI-compat one.
func TestCommandCodeAuthBindsCommandCodeExecutor(t *testing.T) {
	service := &Service{cfg: commandCodeRegistrationConfig()}
	service.coreManager = coreauth.NewManager(nil, nil, nil)

	auth := synthesizeCommandCodeConfigAuth(t, service.cfg)
	service.ensureExecutorsForAuth(auth)

	bound, okExecutor := service.coreManager.Executor(constant.CommandCode)
	if !okExecutor || bound == nil {
		t.Fatal("no executor was bound for the commandcode provider")
	}
	if _, isCommandCode := bound.(*executor.CommandCodeExecutor); !isCommandCode {
		t.Fatalf("bound executor type = %T, want *executor.CommandCodeExecutor", bound)
	}
	if got := bound.Identifier(); got != constant.CommandCode {
		t.Errorf("Identifier() = %q, want %q", got, constant.CommandCode)
	}
}

// TestBaselineExecutorAuthsIncludesCommandCode keeps the provider bound even when
// the config declares no commandcode-api-key entry yet.
func TestBaselineExecutorAuthsIncludesCommandCode(t *testing.T) {
	for _, auth := range baselineExecutorAuths() {
		if auth != nil && auth.Provider == constant.CommandCode {
			return
		}
	}
	t.Fatalf("baselineExecutorAuths does not include %q", constant.CommandCode)
}

// TestCommandCodeConfigModelsAreRegistered proves the configured models reach the
// registry, which is what makes the models routable.
func TestCommandCodeConfigModelsAreRegistered(t *testing.T) {
	service := &Service{cfg: commandCodeRegistrationConfig()}
	service.coreManager = coreauth.NewManager(nil, nil, nil)

	auth := synthesizeCommandCodeConfigAuth(t, service.cfg)
	if _, errRegister := service.coreManager.Register(t.Context(), auth); errRegister != nil {
		t.Fatalf("Register commandcode auth: %v", errRegister)
	}
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
	service.completeModelRegistrationForAuth(t.Context(), auth)

	registered := false
	for _, model := range GlobalModelRegistry().GetAvailableModelsByProvider(constant.CommandCode) {
		if model != nil && model.ID == "cc-flash" {
			registered = true
			if model.OwnedBy != constant.CommandCode {
				t.Errorf("model.OwnedBy = %q, want %q", model.OwnedBy, constant.CommandCode)
			}
		}
	}
	if !registered {
		t.Fatalf("cc-flash was not registered under provider %q; got %v",
			constant.CommandCode, GlobalModelRegistry().GetAvailableModelsByProvider(constant.CommandCode))
	}
	if !GlobalModelRegistry().ClientSupportsModel(auth.ID, "cc-flash") {
		t.Errorf("auth %q does not support cc-flash, so the model cannot be routed", auth.ID)
	}
}
