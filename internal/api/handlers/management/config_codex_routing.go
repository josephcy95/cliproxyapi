package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// GetCodexRoutingConfig returns the Codex account-tier routing policy.
func (h *Handler) GetCodexRoutingConfig(c *gin.Context) {
	c.JSON(http.StatusOK, h.cfg.Codex.Routing)
}

// PutCodexRoutingConfig replaces the Codex account-tier routing policy.
func (h *Handler) PutCodexRoutingConfig(c *gin.Context) {
	var body config.CodexRoutingConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body", "message": err.Error()})
		return
	}
	strategy, ok := normalizeCodexRoutingStrategy(body.Strategy)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy", "message": "strategy must be empty or adaptive"})
		return
	}
	body.Strategy = strategy
	h.cfg.Codex.Routing = body
	h.persist(c)
}

func normalizeCodexRoutingStrategy(strategy string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "":
		return "", true
	case "adaptive":
		return "adaptive", true
	default:
		return "", false
	}
}
