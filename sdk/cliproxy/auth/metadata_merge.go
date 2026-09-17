package auth

import (
	"strings"
	"time"
)

// IsAuthTokenPayloadKey returns true if key is a credential or token lifecycle field
// that should not overwrite newly acquired OAuth credentials during metadata merge.
func IsAuthTokenPayloadKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "access_token", "refresh_token", "id_token", "session_id",
		"expired", "last_refresh", "expires_in", "timestamp",
		"token_type", "user_code", "verification_uri", "verification_uri_complete":
		return true
	default:
		return false
	}
}

// IsEphemeralAuthMetadataKey reports keys that describe live quota, cooldown, or
// disablement. Re-logging in the same account must not keep these; they are
// observed again from upstream after the new tokens are in place.
func IsEphemeralAuthMetadataKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	switch normalized {
	case "disabled", "disabled_reason", "unavailable",
		"status", "status_message", "statusmessage",
		"next_retry_after", "nextretryafter",
		"last_error", "lasterror",
		"quota", "runtime",
		"codex_quota_observed_at", "codexquotaobservedat",
		"rate_limit_reset_credits",
		"rate_limit_reset_credits_available_count",
		"rate_limit_reset_credits_applicable_available_count",
		"rate_limit_reset_credits_checked_at",
		"chatgpt_subscription_active_until", "subscription_active_until",
		"xai_cooldown_until", "xai_last_error_status":
		return true
	default:
		return strings.HasPrefix(normalized, "x-codex-")
	}
}

// MergeExistingAuthMetadata merges user-configured metadata fields from existingMap
// into target.Metadata and target.Storage if target does not already define them.
func MergeExistingAuthMetadata(target *Auth, existingMap map[string]any) {
	if target == nil || len(existingMap) == 0 {
		return
	}
	if target.Metadata == nil {
		target.Metadata = make(map[string]any)
	}
	if _, explicitlySet := target.Metadata["disabled"]; !explicitlySet {
		if disabled, ok := existingMap["disabled"].(bool); ok {
			target.Disabled = disabled
		}
	}
	for k, v := range existingMap {
		if IsAuthTokenPayloadKey(k) || IsEphemeralAuthMetadataKey(k) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(target.Provider), "meta") {
			switch CanonicalCredentialMetadataKey(k) {
			case "api_key", "dca_token", "dca_expired", "dca_expires_at":
				continue
			}
		}
		if _, exists := target.Metadata[k]; !exists {
			target.Metadata[k] = v
		}
	}
	if setter, ok := target.Storage.(interface{ SetMetadata(map[string]any) }); ok {
		setter.SetMetadata(target.Metadata)
	}
}

// ResetAuthRuntimeForRelogin drops cooldown, quota snapshots, and disablement so a
// fresh OAuth login of an existing account starts from current upstream state.
func ResetAuthRuntimeForRelogin(target *Auth) {
	if target == nil {
		return
	}
	target.ReplaceRuntimeState = true
	target.Disabled = false
	target.Unavailable = false
	target.Status = StatusActive
	target.StatusMessage = ""
	target.LastError = nil
	target.NextRetryAfter = time.Time{}
	target.Quota = QuotaState{}
	target.ModelStates = nil
	if target.Metadata == nil {
		return
	}
	for key := range target.Metadata {
		if IsEphemeralAuthMetadataKey(key) {
			delete(target.Metadata, key)
		}
	}
	target.Metadata["disabled"] = false
	if setter, ok := target.Storage.(interface{ SetMetadata(map[string]any) }); ok {
		setter.SetMetadata(target.Metadata)
	}
}

// RestoreOperatorDisablementFromMetadata copies disabled:true from source metadata
// onto target after ResetAuthRuntimeForRelogin, so Claude legacy credential
// migration can preserve operator disablement.
func RestoreOperatorDisablementFromMetadata(target *Auth, source map[string]any) {
	if target == nil || source == nil {
		return
	}
	disabled, ok := source["disabled"].(bool)
	if !ok || !disabled {
		return
	}
	target.Disabled = true
	if target.Metadata == nil {
		target.Metadata = make(map[string]any)
	}
	target.Metadata["disabled"] = true
}
