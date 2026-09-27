package modelconfig

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// ResolveModelInfo returns a private capability snapshot for a configured model.
// Static capabilities come from the suffix-free upstream name, while explicit
// configuration takes precedence.
func ResolveModelInfo(name, modelType string, support *registry.ThinkingSupport) *registry.ModelInfo {
	return ResolveModelInfoWithAlias(name, "", modelType, support)
}

// ResolveModelInfoWithAlias resolves a configured model the same way as
// ResolveModelInfo and additionally consults the catalog fallback.
//
// The fallback covers models no static catalog knows about, such as models
// reached through a custom OpenAI-compatible endpoint. It is looked up by the
// upstream name first, because that is what the request actually carries, then
// by the client-facing alias, then by both with trailing reasoning-level
// segments removed. Every field it provides is only a gap filler: static
// catalog values and explicit configuration always win, and a model that
// resolves nowhere keeps whatever it already had so nothing is guessed.
func ResolveModelInfoWithAlias(name, alias, modelType string, support *registry.ThinkingSupport) *registry.ModelInfo {
	trimmedName := strings.TrimSpace(name)
	baseName := strings.TrimSpace(thinking.ParseSuffix(trimmedName).ModelName)
	baseAlias := strings.TrimSpace(thinking.ParseSuffix(strings.TrimSpace(alias)).ModelName)

	info := registry.LookupStaticModelInfo(baseName)
	if info == nil {
		info = &registry.ModelInfo{}
	}
	applyCatalogFallback(info, baseName, baseAlias)

	info.ID = trimmedName
	info.Type = strings.TrimSpace(modelType)
	if support != nil {
		info.Thinking = NormalizeThinkingSupport(support)
	}
	info.UserDefined = false
	return info
}

// applyCatalogFallback fills fields the static catalogs could not provide.
func applyCatalogFallback(info *registry.ModelInfo, candidates ...string) {
	if info == nil {
		return
	}
	entry, ok := lookupCatalogFallbackEntry(candidates)
	if !ok {
		return
	}
	if info.ContextLength <= 0 && entry.Context > 0 {
		info.ContextLength = entry.Context
	}
	if info.MaxCompletionTokens <= 0 && entry.Output > 0 {
		info.MaxCompletionTokens = entry.Output
	}
	mergeCatalogFallbackLevels(info, entry.Efforts)
}

// mergeCatalogFallbackLevels adds reasoning levels only to a model that carries
// no thinking metadata at all.
//
// A model whose capability entry already exists but lists no levels is
// deliberately budget-shaped: the provider appliers switch their output format
// on this exact condition (Gemini 2.5 uses thinkingBudget while Gemini 3.x uses
// thinkingLevel, and Claude adaptive effort requires levels). Injecting catalog
// levels there would flip the wire format and send an unsupported field, so
// such a model is left untouched. Explicit configuration is applied afterwards
// and still wins.
func mergeCatalogFallbackLevels(info *registry.ModelInfo, efforts []string) {
	if info == nil || info.Thinking != nil || len(efforts) == 0 {
		return
	}
	fromCatalog := NormalizeThinkingSupport(&registry.ThinkingSupport{Levels: efforts})
	if fromCatalog == nil || len(fromCatalog.Levels) == 0 {
		return
	}
	info.Thinking = fromCatalog
}

// lookupCatalogFallbackEntry returns the first fallback entry matching any
// candidate name.
func lookupCatalogFallbackEntry(candidates []string) (*registry.CatalogFallbackEntry, bool) {
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if entry, ok := registry.LookupCatalogFallback(candidate); ok {
			return entry, true
		}
	}
	return nil, false
}

// NormalizeThinkingSupport clones and normalizes configured reasoning levels.
func NormalizeThinkingSupport(raw *registry.ThinkingSupport) *registry.ThinkingSupport {
	if raw == nil {
		return nil
	}
	normalized := *raw
	normalized.Levels = nil
	seen := make(map[string]struct{}, len(raw.Levels))
	for _, value := range raw.Levels {
		level := strings.ToLower(strings.TrimSpace(value))
		if level == "" {
			continue
		}
		switch level {
		case "none":
			normalized.ZeroAllowed = true
		case "auto":
			normalized.DynamicAllowed = true
		}
		if _, exists := seen[level]; exists {
			continue
		}
		seen[level] = struct{}{}
		normalized.Levels = append(normalized.Levels, level)
	}
	return &normalized
}
