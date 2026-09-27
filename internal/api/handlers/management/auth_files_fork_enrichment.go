package management

// Fork-only auth-file enrichment helpers: Codex plan type resolution and xAI
// failure status. Kept in a dedicated file so upstream refactors of
// auth_files.go do not silently drop them.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func authCodexPlanType(auth *coreauth.Auth) string {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return ""
	}
	// Prefer stored plan fields (updated by quota refresh) over JWT claims.
	if planType := authMetadataString(auth, "plan_type"); planType != "" {
		return planType
	}
	if planType := authMetadataString(auth, "chatgpt_plan_type"); planType != "" {
		return planType
	}
	if planType := strings.TrimSpace(authAttribute(auth, "plan_type")); planType != "" {
		return planType
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		if planType, _ := claims["plan_type"].(string); strings.TrimSpace(planType) != "" {
			return strings.TrimSpace(planType)
		}
	}
	return ""
}

func authMetadataString(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}

func authMetadataValue(auth *coreauth.Auth, key string) any {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	value, ok := auth.Metadata[key]
	if !ok || value == nil {
		return nil
	}
	if text, okString := value.(string); okString && strings.TrimSpace(text) == "" {
		return nil
	}
	return value
}

func authMetadataNumber(auth *coreauth.Auth, key string) (float64, bool) {
	switch value := authMetadataValue(auth, key).(type) {
	case nil:
		return 0, false
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	case json.Number:
		parsed, errParse := value.Float64()
		return parsed, errParse == nil
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed, errParse == nil
	default:
		return 0, false
	}
}

func jsonWholeNumber(value float64) any {
	if value == float64(int64(value)) {
		return int64(value)
	}
	return value
}

// authCodexQuotaSnapshot copies persisted Codex subscription / reset-credit
// fields onto the management list DTO. Latest upstream values are stored on
// the auth file via PATCH and always replace the previous snapshot.
func authCodexQuotaSnapshot(auth *coreauth.Auth) gin.H {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	out := authCodexQuotaSnapshotFromMetadata(coreauth.FlattenPersistedCodexQuotaMetadata(auth.Metadata))
	for key, value := range auth.Quota.Signals {
		if strings.HasPrefix(key, "X-Codex-") && strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	if !auth.Quota.ObservedAt.IsZero() {
		out["codex_quota_observed_at"] = auth.Quota.ObservedAt
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func authCodexQuotaSnapshotFromMetadata(metadata map[string]any) gin.H {
	out := gin.H{}
	if metadata == nil {
		return out
	}
	view := &coreauth.Auth{Metadata: metadata}
	if count, ok := authMetadataNumber(view, "rate_limit_reset_credits_available_count"); ok {
		out["rate_limit_reset_credits_available_count"] = jsonWholeNumber(count)
	}
	if count, ok := authMetadataNumber(view, "rate_limit_reset_credits_applicable_available_count"); ok {
		out["rate_limit_reset_credits_applicable_available_count"] = jsonWholeNumber(count)
	}
	if credits := authMetadataValue(view, "rate_limit_reset_credits"); credits != nil {
		switch value := credits.(type) {
		case []any, []map[string]any, map[string]any:
			out["rate_limit_reset_credits"] = value
			if summary, ok := value.(map[string]any); ok {
				if _, exists := out["rate_limit_reset_credits_available_count"]; !exists {
					if count, ok := authMetadataNumber(&coreauth.Auth{Metadata: summary}, "available_count"); ok {
						out["rate_limit_reset_credits_available_count"] = jsonWholeNumber(count)
					}
				}
				if _, exists := out["rate_limit_reset_credits_applicable_available_count"]; !exists {
					if count, ok := authMetadataNumber(&coreauth.Auth{Metadata: summary}, "applicable_available_count"); ok {
						out["rate_limit_reset_credits_applicable_available_count"] = jsonWholeNumber(count)
					}
				}
			}
		}
	}
	if checkedAt := authMetadataString(view, "rate_limit_reset_credits_checked_at"); checkedAt != "" {
		out["rate_limit_reset_credits_checked_at"] = checkedAt
	}
	for _, key := range []string{
		"plan_type",
		"chatgpt_plan_type",
		"plan_checked_at",
		"chatgpt_subscription_active_until",
		"subscription_active_until",
		"expired",
		"expires_at",
		"expires",
	} {
		if value := authMetadataValue(view, key); value != nil {
			out[key] = value
		}
	}
	for key, value := range metadata {
		if strings.HasPrefix(key, "X-Codex-") {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
				out[key] = text
			}
		}
	}
	if observedAt := authMetadataValue(view, "codex_quota_observed_at"); observedAt != nil {
		out["codex_quota_observed_at"] = observedAt
	}
	return out
}

func authCodexQuotaSnapshotFromFile(path string) gin.H {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil
	}
	var raw map[string]any
	if errDecode := json.Unmarshal(data, &raw); errDecode != nil {
		return nil
	}
	return authCodexQuotaSnapshotFromMetadata(coreauth.FlattenPersistedCodexQuotaMetadata(raw))
}

func xaiAuthStatus(auth *coreauth.Auth, now time.Time) (int, time.Time) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "xai") {
		return 0, time.Time{}
	}

	statusCode := 0
	if auth.LastError != nil {
		statusCode = auth.LastError.StatusCode()
	}

	cooldownUntil := auth.NextRetryAfter
	if !cooldownUntil.After(now) {
		cooldownUntil = time.Time{}
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if statusCode == 0 && state.LastError != nil {
			statusCode = state.LastError.StatusCode()
		}
		if state.NextRetryAfter.After(now) &&
			(cooldownUntil.IsZero() || state.NextRetryAfter.Before(cooldownUntil)) {
			cooldownUntil = state.NextRetryAfter
		}
	}

	return statusCode, cooldownUntil
}

func authDisabledReason(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	reason, _ := auth.Metadata["disabled_reason"].(string)
	return strings.TrimSpace(reason)
}

func authAllowPrivateInstructionsValue(auth *coreauth.Auth) (bool, bool) {
	if auth == nil {
		return false, false
	}
	if auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["allow_private_instructions"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed, true
			}
		}
	}
	if auth.Metadata == nil {
		return false, false
	}
	raw, ok := auth.Metadata["allow_private_instructions"]
	if !ok || raw == nil {
		return false, false
	}
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

// syncAuthFileAllowPrivateInstructionsAttribute mirrors metadata onto the
// runtime Attributes map so the scheduler private-instructions policy sees
// the flag after a management PATCH. Dropped once by the upstream auth_files
// split — keep it next to the other fork enrichment helpers.
func syncAuthFileAllowPrivateInstructionsAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	allow, ok := authFileBoolValue(auth.Metadata["allow_private_instructions"])
	if !ok || !allow {
		delete(auth.Attributes, "allow_private_instructions")
		if auth.Metadata != nil {
			if !ok {
				delete(auth.Metadata, "allow_private_instructions")
			} else {
				auth.Metadata["allow_private_instructions"] = false
			}
		}
		return
	}
	auth.Attributes["allow_private_instructions"] = "true"
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["allow_private_instructions"] = true
}

func syncAuthFileCodexPlanTypeAttribute(auth *coreauth.Auth) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	// Prefer stored plan fields (quota refresh) over JWT; do not read Attributes
	// as a source or a clear of metadata.plan_type cannot drop the attribute.
	planType := authMetadataString(auth, "plan_type")
	if planType == "" {
		planType = authMetadataString(auth, "chatgpt_plan_type")
	}
	if planType == "" {
		if claims := extractCodexIDTokenClaims(auth); claims != nil {
			if pt, _ := claims["plan_type"].(string); strings.TrimSpace(pt) != "" {
				planType = strings.TrimSpace(pt)
			} else if pt, _ := claims["chatgpt_plan_type"].(string); strings.TrimSpace(pt) != "" {
				planType = strings.TrimSpace(pt)
			}
		}
	}
	if planType == "" {
		delete(auth.Attributes, "plan_type")
		return
	}
	auth.Attributes["plan_type"] = planType
}
