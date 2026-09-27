package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

// GetUsageRetentionDays reports the configured and active cleanup policy.
func (h *Handler) GetUsageRetentionDays(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	configured := h.cfg.UsageRetentionDays
	effective := configured
	if effective <= 0 {
		effective = usagestore.DefaultRetentionDays
	}
	if h.usageStore != nil {
		effective = h.usageStore.RetentionDays()
	}
	c.JSON(http.StatusOK, gin.H{"value": effective, "configured_days": configured, "default_days": usagestore.DefaultRetentionDays, "restart_required": false})
}

// PutUsageRetentionDays persists before updating the cleaner; a failed save must
// never shorten live retention. Cleanup remains on the existing hourly schedule.
func (h *Handler) PutUsageRetentionDays(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil || *body.Value < 1 || *body.Value > 36500 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "retention must be a whole number between 1 and 36500 days"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.cfg.UsageRetentionDays
	h.cfg.UsageRetentionDays = *body.Value
	if !h.persistLocked(c) {
		h.cfg.UsageRetentionDays = previous
		return
	}
	if h.usageStore != nil {
		h.usageStore.SetRetentionDays(*body.Value)
	}
}
