package management

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// modelSourceCandidate describes one auth that can serve a model alias/id.
// Order matches runtime selection preference: higher priority, then higher key_priority.
type modelSourceCandidate struct {
	Provider    string `json:"provider"`
	Label       string `json:"label,omitempty"`
	Priority    int    `json:"priority"`
	KeyPriority int    `json:"key_priority,omitempty"`
	AuthIndex   string `json:"auth_index,omitempty"`
	AuthID      string `json:"auth_id,omitempty"`
	Status      string `json:"status,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	Preferred   bool   `json:"preferred,omitempty"`
}

// GetModelSources returns the candidate auth pool for every registered model id,
// ordered the way the scheduler prefers them (provider priority, then key priority).
// Used by the management UI to preview routing on model hover.
func (h *Handler) GetModelSources(c *gin.Context) {
	out := make(map[string][]modelSourceCandidate)
	if h == nil || h.authManager == nil {
		c.JSON(200, gin.H{"models": out})
		return
	}

	reg := registry.GetGlobalRegistry()
	auths := h.authManager.List()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		models := reg.GetModelsForClient(auth.ID)
		if len(models) == 0 {
			continue
		}
		for _, model := range models {
			if model == nil {
				continue
			}
			id := strings.TrimSpace(model.ID)
			if id == "" {
				continue
			}
			out[id] = append(out[id], buildModelSourceCandidateForModel(auth, id))
		}
	}

	for modelID, sources := range out {
		sort.SliceStable(sources, func(i, j int) bool {
			if sources[i].Priority != sources[j].Priority {
				return sources[i].Priority > sources[j].Priority
			}
			if sources[i].KeyPriority != sources[j].KeyPriority {
				return sources[i].KeyPriority > sources[j].KeyPriority
			}
			if sources[i].Label != sources[j].Label {
				return sources[i].Label < sources[j].Label
			}
			return sources[i].AuthID < sources[j].AuthID
		})
		// Mark the first ready candidate as preferred (matches primary pick under normal conditions).
		for i := range sources {
			if sources[i].Disabled || sources[i].Unavailable {
				continue
			}
			sources[i].Preferred = true
			break
		}
		out[modelID] = sources
	}

	c.JSON(200, gin.H{"models": out})
}

func buildModelSourceCandidate(auth *coreauth.Auth) modelSourceCandidate {
	return buildModelSourceCandidateForModel(auth, "")
}

func buildModelSourceCandidateForModel(auth *coreauth.Auth, model string) modelSourceCandidate {
	candidate := modelSourceCandidate{
		Provider:    strings.TrimSpace(auth.Provider),
		Label:       strings.TrimSpace(auth.Label),
		Disabled:    auth.Disabled,
		Unavailable: auth.Unavailable,
		Status:      string(auth.Status),
		Reason:      strings.TrimSpace(auth.StatusMessage),
		AuthID:      strings.TrimSpace(auth.ID),
	}
	applyModelSourceState(&candidate, auth, model, time.Now())
	if candidate.Label == "" {
		if name := strings.TrimSpace(authAttribute(auth, "compat_name")); name != "" {
			candidate.Label = name
		} else if auth.FileName != "" {
			candidate.Label = auth.FileName
		} else {
			candidate.Label = candidate.Provider
		}
	}
	if idx := strings.TrimSpace(auth.EnsureIndex()); idx != "" {
		candidate.AuthIndex = idx
	}
	if p := strings.TrimSpace(authAttribute(auth, "priority")); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			candidate.Priority = parsed
		}
	}
	if kp := strings.TrimSpace(authAttribute(auth, "key_priority")); kp != "" {
		if parsed, err := strconv.Atoi(kp); err == nil {
			candidate.KeyPriority = parsed
		}
	}
	if base := strings.TrimSpace(authAttribute(auth, "base_url")); base != "" {
		candidate.BaseURL = base
	}
	return candidate
}

func applyModelSourceState(candidate *modelSourceCandidate, auth *coreauth.Auth, model string, now time.Time) {
	if candidate == nil || auth == nil || strings.TrimSpace(model) == "" || len(auth.ModelStates) == 0 {
		return
	}
	modelKey := strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	state := auth.ModelStates[model]
	if state == nil && modelKey != "" && modelKey != model {
		state = auth.ModelStates[modelKey]
	}
	if state == nil {
		return
	}
	if state.Status == coreauth.StatusDisabled {
		candidate.Disabled = true
		candidate.Status = string(state.Status)
	}
	if reason := strings.TrimSpace(state.StatusMessage); reason != "" {
		candidate.Reason = reason
	} else if reason := strings.TrimSpace(state.Quota.Reason); reason != "" {
		candidate.Reason = reason
	} else if state.LastError != nil && strings.TrimSpace(state.LastError.Message) != "" {
		candidate.Reason = strings.TrimSpace(state.LastError.Message)
	}
	if state.Unavailable && state.NextRetryAfter.After(now) {
		candidate.Unavailable = true
	}
}
