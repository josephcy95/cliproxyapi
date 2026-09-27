package handlers

// Fork-only private Codex instructions support. Kept in a dedicated file so
// upstream refactors of handlers.go do not silently drop it.

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexinstructions"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func applyPrivateCodexInstructionModel(manager *coreauth.Manager, modelName string, meta map[string]any) (string, map[string]any) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return modelName, meta
	}
	parsed := thinking.ParseSuffix(modelName)
	base := strings.TrimSpace(parsed.ModelName)
	stripped := base
	private := manager.CodexInstructionsApplyWithoutPrefixSuffix(base)
	if !private {
		stripped, private = codexinstructions.ParseModel(base, codexInstructionMarkersFromManager(manager))
	}
	if !private {
		return modelName, meta
	}
	if meta == nil {
		meta = make(map[string]any)
	}
	meta[coreexecutor.CodexPrivateInstructionsMetadataKey] = true
	if parsed.HasSuffix {
		return fmt.Sprintf("%s(%s)", stripped, parsed.RawSuffix), meta
	}
	return stripped, meta
}

// stripPrivateCodexMarkersForProviderLookup removes configured private prefix/suffix markers so
// registry provider resolution can find the real model (e.g. kiro/gpt-5.6-luna-jb → kiro/gpt-5.6-luna).
// Thinking suffixes such as "(high)" should already be stripped by the caller.
// The original model id must still be passed to applyPrivateCodexInstructionModel later so private
// mode metadata is set.
func stripPrivateCodexMarkersForProviderLookup(manager *coreauth.Manager, modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || manager == nil {
		return modelName
	}
	// Without prefix/suffix mode every eligible request is private; model ids are not marked.
	if manager.CodexInstructionsApplyWithoutPrefixSuffix(modelName) {
		return modelName
	}
	stripped, private := codexinstructions.ParseModel(modelName, codexInstructionMarkersFromManager(manager))
	if private && strings.TrimSpace(stripped) != "" {
		return stripped
	}
	return modelName
}

func codexInstructionMarkersFromManager(manager *coreauth.Manager) codexinstructions.MarkerConfig {
	if manager == nil {
		return codexinstructions.DefaultMarkers()
	}
	return manager.CodexInstructionMarkers()
}

// ExpandPrivateCodexInstructionModels clones eligible models with private prefix/suffix ids
// for /v1/models when instructions are enabled with markers.
func ExpandPrivateCodexInstructionModels(manager *coreauth.Manager, models []map[string]any) []map[string]any {
	return expandPrivateCodexInstructionModels(manager, models)
}

// ExpandPrivateCodexClientModels clones Codex client catalog entries for private markers.
func ExpandPrivateCodexClientModels(manager *coreauth.Manager, models []map[string]any) []map[string]any {
	return expandPrivateCodexClientModels(manager, models)
}

func expandPrivateCodexInstructionModels(manager *coreauth.Manager, models []map[string]any) []map[string]any {
	cfg := codexInstructionsListingConfig(manager)
	if cfg == nil || !cfg.Enabled || !codexInstructionsUsePrefixSuffix(*cfg) {
		return models
	}
	markers := codexinstructions.NormalizeMarkers(codexinstructions.MarkerConfig{
		Prefixes: cfg.RequestMarkers.Prefixes,
		Suffixes: cfg.RequestMarkers.Suffixes,
	})
	if len(markers.Prefixes) == 0 && len(markers.Suffixes) == 0 {
		return models
	}

	out := make([]map[string]any, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(fmt.Sprint(model["id"]))
		if id == "" || id == "<nil>" {
			out = append(out, model)
			continue
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, model)
		}
		if !codexinstructions.ModelMatches(cfg.Models, id) {
			continue
		}
		for _, virtualID := range codexinstructions.VirtualModelIDs(id, markers) {
			if _, ok := seen[virtualID]; ok {
				continue
			}
			seen[virtualID] = struct{}{}
			clone := cloneModelMap(model)
			clone["id"] = virtualID
			if _, ok := clone["display_name"]; ok {
				clone["display_name"] = virtualID
			}
			if _, ok := clone["description"]; ok {
				clone["description"] = virtualID
			}
			out = append(out, clone)
		}
	}
	return out
}

// expandPrivateCodexClientModels clones Codex client catalog entries (slug-based) for private markers.
func expandPrivateCodexClientModels(manager *coreauth.Manager, models []map[string]any) []map[string]any {
	cfg := codexInstructionsListingConfig(manager)
	if cfg == nil || !cfg.Enabled || !codexInstructionsUsePrefixSuffix(*cfg) {
		return models
	}
	markers := codexinstructions.NormalizeMarkers(codexinstructions.MarkerConfig{
		Prefixes: cfg.RequestMarkers.Prefixes,
		Suffixes: cfg.RequestMarkers.Suffixes,
	})
	if len(markers.Prefixes) == 0 && len(markers.Suffixes) == 0 {
		return models
	}

	out := make([]map[string]any, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)
	for _, model := range models {
		if model == nil {
			continue
		}
		slug := strings.TrimSpace(fmt.Sprint(model["slug"]))
		if slug == "" || slug == "<nil>" {
			if id := strings.TrimSpace(fmt.Sprint(model["id"])); id != "" && id != "<nil>" {
				slug = id
			}
		}
		if slug == "" {
			out = append(out, model)
			continue
		}
		if _, ok := seen[slug]; !ok {
			seen[slug] = struct{}{}
			out = append(out, model)
		}
		if !codexinstructions.ModelMatches(cfg.Models, slug) {
			continue
		}
		for _, virtualID := range codexinstructions.VirtualModelIDs(slug, markers) {
			if _, ok := seen[virtualID]; ok {
				continue
			}
			seen[virtualID] = struct{}{}
			clone := cloneModelMap(model)
			clone["slug"] = virtualID
			if _, hasID := clone["id"]; hasID {
				clone["id"] = virtualID
			}
			if _, ok := clone["display_name"]; ok {
				clone["display_name"] = virtualID
			}
			if _, ok := clone["description"]; ok {
				clone["description"] = virtualID
			}
			out = append(out, clone)
		}
	}
	return out
}

func codexInstructionsListingConfig(manager *coreauth.Manager) *internalconfig.CodexInstructionsConfig {
	if manager == nil {
		return nil
	}
	return manager.CodexInstructionsConfig()
}

func codexInstructionsUsePrefixSuffix(cfg internalconfig.CodexInstructionsConfig) bool {
	if cfg.UsePrefixSuffix == nil {
		return true
	}
	return *cfg.UsePrefixSuffix
}

func cloneModelMap(model map[string]any) map[string]any {
	if model == nil {
		return nil
	}
	out := make(map[string]any, len(model))
	for key, value := range model {
		out[key] = cloneModelMapValue(value)
	}
	return out
}

func cloneModelMapValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneModelMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, entry := range typed {
			out[i] = cloneModelMapValue(entry)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}
