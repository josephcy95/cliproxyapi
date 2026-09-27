package executor

// Fork-only Codex private/configured instructions injection. Kept in a dedicated
// file so upstream refactors of codex_executor.go do not silently drop it.

import (
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexinstructions"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func applyCodexConfiguredInstructions(cfg *config.Config, auth *cliproxyauth.Auth, model string, body []byte, opts cliproxyexecutor.Options) []byte {
	if cfg == nil || !cfg.Codex.Instructions.Enabled || len(body) == 0 {
		return body
	}
	if !codexinstructions.RequestIsPrivate(opts.Metadata) {
		return body
	}
	if codexInstructionsOAuthOnly(cfg.Codex.Instructions) && !codexInstructionsAuthIsOAuth(auth) {
		return body
	}
	if codexInstructionsRequireAuthAllow(cfg.Codex.Instructions) && !codexinstructions.AuthAllows(auth.Attributes, auth.Metadata) {
		return body
	}
	if !codexInstructionsModelMatches(cfg.Codex.Instructions.Models, model) {
		return body
	}
	private := strings.TrimSpace(cfg.Codex.Instructions.Content)
	if private == "" && strings.TrimSpace(cfg.Codex.Instructions.File) != "" {
		data, err := os.ReadFile(strings.TrimSpace(cfg.Codex.Instructions.File))
		if err == nil {
			private = strings.TrimSpace(string(data))
		} else {
			log.Warnf("codex instructions: failed to read file %q: %v", cfg.Codex.Instructions.File, err)
		}
	}
	if private == "" {
		return body
	}

	current := gjson.GetBytes(body, "instructions").String()
	mode := strings.ToLower(strings.TrimSpace(cfg.Codex.Instructions.Mode))
	if mode == "" {
		mode = "prepend"
	}
	var merged string
	switch mode {
	case "replace":
		merged = private
	case "append":
		merged = joinCodexInstructions(current, private)
	default:
		merged = joinCodexInstructions(private, current)
	}
	out, err := sjson.SetBytes(body, "instructions", merged)
	if err != nil {
		return body
	}
	return out
}

func codexInstructionsOAuthOnly(cfg config.CodexInstructionsConfig) bool {
	if cfg.OAuthOnly == nil {
		return true
	}
	return *cfg.OAuthOnly
}

func codexInstructionsAuthIsOAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if strings.TrimSpace(auth.Provider) == "codex" && len(auth.Metadata) > 0 {
		return true
	}
	return strings.TrimSpace(auth.Attributes["api_key"]) == "" && strings.TrimSpace(auth.Attributes["base_url"]) == ""
}

func codexInstructionsRequireAuthAllow(cfg config.CodexInstructionsConfig) bool {
	if cfg.RequireAuthAllow == nil {
		return true
	}
	return *cfg.RequireAuthAllow
}

func codexInstructionsModelMatches(patterns []string, model string) bool {
	return codexinstructions.ModelMatches(patterns, model)
}

func joinCodexInstructions(first, second string) string {
	first = strings.TrimSpace(first)
	second = strings.TrimSpace(second)
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "\n\n" + second
	}
}
