package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/desensitization"
)

// KnownOAuthProviderIDs are Auth.Provider values used by OAuth/file login in this fork.
var KnownOAuthProviderIDs = []string{
	"claude",
	"gemini",
	"aistudio",
	"antigravity",
	"codex",
	"kimi",
	"xai",
	"qoder",
	"qodercn",
	"vertex",
}

// KnownAPIProviderIDs are built-in API-key channel Auth.Provider values.
var KnownAPIProviderIDs = []string{
	"claude",
	"gemini",
	"codex",
	"commandcode",
	"xai",
	"gemini-interactions",
	"vertex",
}

// GetDesensitizationConfig returns the current desensitization settings.
func (h *Handler) GetDesensitizationConfig(c *gin.Context) {
	normalized := config.NormalizeDesensitizationConfig(h.cfg.Desensitization)
	c.JSON(http.StatusOK, normalized)
}

// PutDesensitizationConfig replaces desensitization settings and persists config.
func (h *Handler) PutDesensitizationConfig(c *gin.Context) {
	var body config.DesensitizationConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body", "message": err.Error()})
		return
	}
	h.cfg.Desensitization = config.NormalizeDesensitizationConfig(body)
	desensitization.Configure(h.cfg.Desensitization)
	h.persist(c)
}

// GetDesensitizationScopeOptions returns selectable inbound keys and provider ids.
func (h *Handler) GetDesensitizationScopeOptions(c *gin.Context) {
	apiKeys := make([]string, 0, len(h.cfg.APIKeys))
	for _, key := range h.cfg.APIKeys {
		key = strings.TrimSpace(key)
		if key != "" {
			apiKeys = append(apiKeys, key)
		}
	}
	apiProviders := append([]string(nil), KnownAPIProviderIDs...)
	seen := map[string]struct{}{}
	for _, id := range apiProviders {
		seen[strings.ToLower(id)] = struct{}{}
	}
	for _, compat := range h.cfg.OpenAICompatibility {
		name := strings.TrimSpace(compat.Name)
		if name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		apiProviders = append(apiProviders, name)
	}
	c.JSON(http.StatusOK, gin.H{
		"api_keys":        apiKeys,
		"oauth_providers": append([]string(nil), KnownOAuthProviderIDs...),
		"api_providers":   apiProviders,
	})
}

// PreviewDesensitization masks a sample string without mutating live session maps.
func (h *Handler) PreviewDesensitization(c *gin.Context) {
	var req struct {
		Text string `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body", "message": err.Error()})
		return
	}
	cfg := config.NormalizeDesensitizationConfig(h.cfg.Desensitization)
	cfg.Enabled = true
	eng := desensitization.NewEngine(cfg)
	masked, hits, err := eng.Preview(req.Text)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "preview failed", "message": err.Error()})
		return
	}
	if hits == nil {
		hits = []desensitization.Hit{}
	}
	c.JSON(http.StatusOK, gin.H{"masked": masked, "hits": hits})
}
