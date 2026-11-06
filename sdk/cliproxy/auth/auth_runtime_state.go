package auth

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	// metadataRuntimeKey is the auth-file JSON key for persisted runtime health.
	metadataRuntimeKey = "runtime"
	// metadataRuntimeModelsKey nests per-model cooldown / exhaustion counters.
	metadataRuntimeModelsKey = "models"
)

// authRuntimeModelState is the compact on-disk shape written into auth JSON:
//
//	"runtime": {
//	  "models": {
//	    "grok-4.5": {
//	      "cooldown_until": "...",
//	      "reason": "free_usage_exhausted",
//	      "http_status": 429,
//	      "last_response": "...",
//	      "free_usage_hits": 1,
//	      "other_403_hits": 0,
//	      "usage_limit_hits": 0,
//	      "auth_failure_hits": 0
//	    }
//	  }
//	}
type authRuntimeModelState struct {
	CooldownUntil   time.Time   `json:"cooldown_until,omitempty"`
	Reason          string      `json:"reason,omitempty"`
	HTTPStatus      int         `json:"http_status,omitempty"`
	ErrorCode       string      `json:"error_code,omitempty"`
	Quota           *QuotaState `json:"quota,omitempty"`
	LastResponse    string      `json:"last_response,omitempty"`
	FreeUsageHits   int         `json:"free_usage_hits,omitempty"`
	Other403Hits    int         `json:"other_403_hits,omitempty"`
	UsageLimitHits  int         `json:"usage_limit_hits,omitempty"`
	AuthFailureHits int         `json:"auth_failure_hits,omitempty"`
}

// hydrateAuthRuntimeFromMetadata rebuilds in-memory ModelStates from auth-file runtime.
// Called when loading/registering auths from disk. Expired cooldowns are dropped;
// exhaustion counters are kept until a successful request resets them.
func hydrateAuthRuntimeFromMetadata(auth *Auth, now time.Time) {
	if auth == nil || len(auth.Metadata) == 0 {
		return
	}
	runtimeRaw, ok := auth.Metadata[metadataRuntimeKey]
	if !ok || runtimeRaw == nil {
		return
	}
	runtimeMap, ok := runtimeRaw.(map[string]any)
	if !ok {
		// Drop unreadable runtime block.
		delete(auth.Metadata, metadataRuntimeKey)
		return
	}
	modelsRaw, ok := runtimeMap[metadataRuntimeModelsKey]
	if !ok || modelsRaw == nil {
		return
	}
	modelsMap, ok := modelsRaw.(map[string]any)
	if !ok {
		return
	}

	if now.IsZero() {
		now = time.Now()
	}
	for model, raw := range modelsMap {
		model = strings.TrimSpace(model)
		if model == "" || raw == nil {
			continue
		}
		entryMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		entry := parseAuthRuntimeModelState(entryMap)
		cooling := !entry.CooldownUntil.IsZero() && entry.CooldownUntil.After(now)
		if !cooling && entry.FreeUsageHits <= 0 && entry.Other403Hits <= 0 &&
			entry.UsageLimitHits <= 0 && entry.AuthFailureHits <= 0 {
			continue
		}
		hadLiveState := auth.ModelStates[model] != nil
		state := ensureModelState(auth, model)
		if cooling && !hadLiveState {
			state.Unavailable = true
			state.Status = StatusError
			state.NextRetryAfter = entry.CooldownUntil
			state.StatusMessage = entry.Reason
			if entry.Quota != nil {
				state.Quota = entry.Quota.Clone()
			} else if entry.HTTPStatus != 401 {
				// Legacy records lacked separate quota provenance. Preserve
				// their cooldowns, but never turn a 401 into account quota.
				state.Quota = QuotaState{Exceeded: true, Reason: entry.Reason, NextRecoverAt: entry.CooldownUntil}
			}
			if entry.LastResponse != "" || entry.HTTPStatus > 0 {
				state.LastError = &Error{
					Code:       entry.ErrorCode,
					Message:    entry.LastResponse,
					HTTPStatus: entry.HTTPStatus,
				}
				if auth.LastError == nil {
					auth.LastError = cloneError(state.LastError)
				}
				if auth.StatusMessage == "" {
					auth.StatusMessage = entry.LastResponse
					if auth.StatusMessage == "" {
						auth.StatusMessage = entry.Reason
					}
				}
			}
		}
		if entry.FreeUsageHits > state.FreeUsageExhaustionCount {
			state.FreeUsageExhaustionCount = entry.FreeUsageHits
		}
		if entry.Other403Hits > state.OtherForbiddenCount {
			state.OtherForbiddenCount = entry.Other403Hits
		}
		if entry.UsageLimitHits > state.UsageLimitCount {
			state.UsageLimitCount = entry.UsageLimitHits
		}
		if entry.AuthFailureHits > state.AuthFailureCount {
			state.AuthFailureCount = entry.AuthFailureHits
		}
		state.UpdatedAt = now
	}
	if len(auth.ModelStates) > 0 {
		updateAggregatedAvailability(auth, now)
		if auth.Status != StatusDisabled && !auth.Disabled {
			if hasModelError(auth, now) {
				auth.Status = StatusError
			}
		}
	}
}

// syncAuthRuntimeMetadata writes the compact runtime block into auth.Metadata so
// FileTokenStore.Save persists it with the rest of the auth JSON.
func syncAuthRuntimeMetadata(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if now.IsZero() {
		now = time.Now()
	}

	models := make(map[string]any)
	for model, state := range auth.ModelStates {
		model = strings.TrimSpace(model)
		if model == "" || state == nil {
			continue
		}
		entry := modelStateToRuntimeEntry(state, now)
		if entry == nil {
			continue
		}
		models[model] = runtimeEntryToMap(entry)
	}

	if len(models) == 0 {
		delete(auth.Metadata, metadataRuntimeKey)
		return
	}
	auth.Metadata[metadataRuntimeKey] = map[string]any{
		metadataRuntimeModelsKey: models,
	}
}

func modelStateToRuntimeEntry(state *ModelState, now time.Time) *authRuntimeModelState {
	if state == nil {
		return nil
	}
	cooling := state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now)
	if !cooling && state.FreeUsageExhaustionCount <= 0 && state.OtherForbiddenCount <= 0 &&
		state.UsageLimitCount <= 0 && state.AuthFailureCount <= 0 {
		return nil
	}
	quota := state.Quota.Clone()
	entry := &authRuntimeModelState{
		Quota:           &quota,
		FreeUsageHits:   state.FreeUsageExhaustionCount,
		Other403Hits:    state.OtherForbiddenCount,
		UsageLimitHits:  state.UsageLimitCount,
		AuthFailureHits: state.AuthFailureCount,
	}
	if cooling {
		entry.CooldownUntil = state.NextRetryAfter
		entry.Reason = strings.TrimSpace(state.StatusMessage)
		if entry.Reason == "" {
			entry.Reason = strings.TrimSpace(state.Quota.Reason)
		}
		if entry.Reason == "" {
			entry.Reason = "quota"
		}
		if state.LastError != nil {
			entry.ErrorCode = state.LastError.Code
			entry.HTTPStatus = state.LastError.HTTPStatus
			entry.LastResponse = truncateRuntimeLastResponse(state.LastError.Message)
		}
	}
	return entry
}

func runtimeEntryToMap(entry *authRuntimeModelState) map[string]any {
	out := make(map[string]any)
	if entry == nil {
		return out
	}
	if !entry.CooldownUntil.IsZero() {
		out["cooldown_until"] = entry.CooldownUntil.Format(time.RFC3339Nano)
	}
	if entry.Quota != nil {
		out["quota"] = entry.Quota
	}
	if entry.ErrorCode != "" {
		out["error_code"] = entry.ErrorCode
	}
	if entry.Reason != "" {
		out["reason"] = entry.Reason
	}
	if entry.HTTPStatus > 0 {
		out["http_status"] = entry.HTTPStatus
	}
	if entry.LastResponse != "" {
		out["last_response"] = entry.LastResponse
	}
	if entry.FreeUsageHits > 0 {
		out["free_usage_hits"] = entry.FreeUsageHits
	}
	if entry.Other403Hits > 0 {
		out["other_403_hits"] = entry.Other403Hits
	}
	if entry.UsageLimitHits > 0 {
		out["usage_limit_hits"] = entry.UsageLimitHits
	}
	if entry.AuthFailureHits > 0 {
		out["auth_failure_hits"] = entry.AuthFailureHits
	}
	return out
}

func parseAuthRuntimeModelState(m map[string]any) authRuntimeModelState {
	var entry authRuntimeModelState
	if m == nil {
		return entry
	}
	entry.CooldownUntil = parseRuntimeTime(m["cooldown_until"])
	entry.Reason = strings.TrimSpace(anyToString(m["reason"]))
	if raw, ok := m["quota"]; ok && raw != nil {
		encoded, err := json.Marshal(raw)
		var quota QuotaState
		if err == nil && json.Unmarshal(encoded, &quota) == nil {
			entry.Quota = &quota
		}
	}
	entry.ErrorCode = strings.TrimSpace(anyToString(m["error_code"]))
	entry.HTTPStatus = anyToInt(m["http_status"])
	entry.LastResponse = strings.TrimSpace(anyToString(m["last_response"]))
	entry.FreeUsageHits = anyToInt(m["free_usage_hits"])
	entry.Other403Hits = anyToInt(m["other_403_hits"])
	entry.UsageLimitHits = anyToInt(m["usage_limit_hits"])
	entry.AuthFailureHits = anyToInt(m["auth_failure_hits"])
	return entry
}

func parseRuntimeTime(raw any) time.Time {
	switch v := raw.(type) {
	case time.Time:
		return v
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func anyToString(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	default:
		return ""
	}
}

func anyToInt(raw any) int {
	switch v := raw.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		return 0
	}
}

const maxRuntimeLastResponseLen = 512

func truncateRuntimeLastResponse(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) <= maxRuntimeLastResponseLen {
		return msg
	}
	return msg[:maxRuntimeLastResponseLen]
}
