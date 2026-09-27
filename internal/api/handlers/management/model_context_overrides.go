package management

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// modelContextOverrideItem is the management API shape for one override entry.
type modelContextOverrideItem struct {
	Model               string `json:"model"`
	ContextLength       int    `json:"context-length,omitempty"`
	MaxCompletionTokens int    `json:"max-completion-tokens,omitempty"`
}

// modelContextEntry describes a registered model and its effective context window.
type modelContextEntry struct {
	Model               string   `json:"model"`
	DisplayName         string   `json:"display-name,omitempty"`
	Type                string   `json:"type,omitempty"`
	OwnedBy             string   `json:"owned-by,omitempty"`
	Providers           []string `json:"providers,omitempty"`
	ContextLength       int      `json:"context-length,omitempty"`
	MaxCompletionTokens int      `json:"max-completion-tokens,omitempty"`
	Overridden          bool     `json:"overridden"`
	Resolved            bool     `json:"resolved"`
}

// GetModelContextOverrides returns the configured manual context windows.
func (h *Handler) GetModelContextOverrides(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"model-context-overrides": h.modelContextOverrideItems()})
}

// GetModelContextStatus returns every registered model with its effective context
// window, so the management UI can surface the models that still lack one.
func (h *Handler) GetModelContextStatus(c *gin.Context) {
	statuses := registry.GetGlobalRegistry().GetModelContextStatuses()
	entries := make([]modelContextEntry, 0, len(statuses))
	missing := 0
	for _, status := range statuses {
		if !status.Resolved {
			missing++
		}
		entries = append(entries, modelContextEntry{
			Model:               status.ID,
			DisplayName:         status.DisplayName,
			Type:                status.Type,
			OwnedBy:             status.OwnedBy,
			Providers:           status.Providers,
			ContextLength:       status.ContextLength,
			MaxCompletionTokens: status.MaxCompletionTokens,
			Overridden:          status.Overridden,
			Resolved:            status.Resolved,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"models":                  entries,
		"missing-context-count":   missing,
		"model-context-overrides": h.modelContextOverrideItems(),
	})
}

// PutModelContextOverrides replaces the whole override list.
func (h *Handler) PutModelContextOverrides(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	var arr []modelContextOverrideItem
	if errUnmarshal := json.Unmarshal(data, &arr); errUnmarshal != nil {
		var obj struct {
			Items []modelContextOverrideItem `json:"items"`
		}
		if errItems := json.Unmarshal(data, &obj); errItems != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}

	overrides := make([]config.ModelContextOverride, 0, len(arr))
	for _, item := range arr {
		model := strings.TrimSpace(item.Model)
		if model == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
			return
		}
		if item.ContextLength < 0 || item.MaxCompletionTokens < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "context window values must not be negative"})
			return
		}
		overrides = append(overrides, config.ModelContextOverride{
			Model:               model,
			ContextLength:       item.ContextLength,
			MaxCompletionTokens: item.MaxCompletionTokens,
		})
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ModelContextOverrides = overrides
	h.cfg.SanitizeModelContextOverrides()
	h.persistLocked(c)
}

// PatchModelContextOverride upserts a single override, keyed by model ID.
// Sending both values as 0 removes the entry.
func (h *Handler) PatchModelContextOverride(c *gin.Context) {
	var body struct {
		Model               *string `json:"model"`
		ContextLength       *int    `json:"context-length"`
		MaxCompletionTokens *int    `json:"max-completion-tokens"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Model == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	model := strings.TrimSpace(*body.Model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}
	if (body.ContextLength != nil && *body.ContextLength < 0) ||
		(body.MaxCompletionTokens != nil && *body.MaxCompletionTokens < 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "context window values must not be negative"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	key := strings.ToLower(model)
	targetIndex := -1
	for i := range h.cfg.ModelContextOverrides {
		if strings.ToLower(strings.TrimSpace(h.cfg.ModelContextOverrides[i].Model)) == key {
			targetIndex = i
			break
		}
	}

	entry := config.ModelContextOverride{Model: model}
	if targetIndex != -1 {
		entry = h.cfg.ModelContextOverrides[targetIndex]
		entry.Model = model
	}
	if body.ContextLength != nil {
		entry.ContextLength = *body.ContextLength
	}
	if body.MaxCompletionTokens != nil {
		entry.MaxCompletionTokens = *body.MaxCompletionTokens
	}

	switch {
	case entry.ContextLength <= 0 && entry.MaxCompletionTokens <= 0:
		// Clearing both values removes the override entirely.
		if targetIndex != -1 {
			h.cfg.ModelContextOverrides = append(
				h.cfg.ModelContextOverrides[:targetIndex],
				h.cfg.ModelContextOverrides[targetIndex+1:]...,
			)
		}
	case targetIndex != -1:
		h.cfg.ModelContextOverrides[targetIndex] = entry
	default:
		h.cfg.ModelContextOverrides = append(h.cfg.ModelContextOverrides, entry)
	}

	h.cfg.SanitizeModelContextOverrides()
	h.persistLocked(c)
}

// DeleteModelContextOverride removes one override by model ID.
func (h *Handler) DeleteModelContextOverride(c *gin.Context) {
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing model"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	key := strings.ToLower(model)
	out := make([]config.ModelContextOverride, 0, len(h.cfg.ModelContextOverrides))
	for _, override := range h.cfg.ModelContextOverrides {
		if strings.ToLower(strings.TrimSpace(override.Model)) == key {
			continue
		}
		out = append(out, override)
	}
	h.cfg.ModelContextOverrides = out
	h.cfg.SanitizeModelContextOverrides()
	h.persistLocked(c)
}

// modelContextOverrideItems renders the configured overrides for API responses.
func (h *Handler) modelContextOverrideItems() []modelContextOverrideItem {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.cfg == nil {
		return []modelContextOverrideItem{}
	}
	items := make([]modelContextOverrideItem, 0, len(h.cfg.ModelContextOverrides))
	for _, override := range h.cfg.ModelContextOverrides {
		items = append(items, modelContextOverrideItem{
			Model:               override.Model,
			ContextLength:       override.ContextLength,
			MaxCompletionTokens: override.MaxCompletionTokens,
		})
	}
	return items
}
