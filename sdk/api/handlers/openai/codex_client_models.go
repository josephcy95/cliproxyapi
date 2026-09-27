package openai

import (
	"strings"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexinstructions"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func (h *OpenAIAPIHandler) codexClientModelsResponse(clientVersion ...string) map[string]any {
	version := ""
	if len(clientVersion) > 0 {
		version = clientVersion[0]
	}
	optimizeMultiAgentV2 := h != nil && h.Cfg != nil && h.Cfg.CodexOptimizeMultiAgentV2
	// Build from base registry models (without private virtual ids), then clone catalog
	// entries so private variants keep full template/capability metadata.
	baseModels := registry.GetGlobalRegistry().GetAvailableModels("openai")
	built := codexmodels.BuildResponseForClient(baseModels, codexClientProvidersForModel(h), optimizeMultiAgentV2, version)
	if raw, ok := built["models"].([]map[string]any); ok {
		built["models"] = handlers.ExpandPrivateCodexClientModels(h.AuthManager, raw)
	}
	return built
}

func codexClientProvidersForModel(h *OpenAIAPIHandler) codexmodels.ProvidersForModelFunc {
	markers := codexinstructions.DefaultMarkers()
	if h != nil && h.AuthManager != nil {
		markers = h.AuthManager.CodexInstructionMarkers()
	}
	markers = codexinstructions.NormalizeMarkers(markers)
	return func(id string) []string {
		base, private := codexinstructions.ParseModel(strings.TrimSpace(id), markers)
		if !private || base == "" {
			base = strings.TrimSpace(id)
		}
		return registry.GetGlobalRegistry().GetModelProviders(base)
	}
}

// CodexClientModelsResponse builds a Codex client model response.
func CodexClientModelsResponse(models []map[string]any) map[string]any {
	return codexmodels.BuildResponse(models, nil, false)
}

// CodexClientModelsResponseWithMultiAgentV2 builds a Codex client model response
// and advertises multi-agent v2 for synthesized models when enabled.
func CodexClientModelsResponseWithMultiAgentV2(models []map[string]any, enabled bool) map[string]any {
	return codexmodels.BuildResponse(models, nil, enabled)
}

// CodexClientModelsResponseForClient builds a Codex client model response
// tailored for a specific client version.
func CodexClientModelsResponseForClient(models []map[string]any, clientVersion string, enabled bool) map[string]any {
	return codexmodels.BuildResponseForClient(models, nil, enabled, clientVersion)
}
