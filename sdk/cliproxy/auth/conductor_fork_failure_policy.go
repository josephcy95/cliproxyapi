package auth

// Fork-only credential failure policy: xAI permission-denied auto-disable,
// Codex auth-death / usage-limit auto-disable, and the private Codex
// instructions auth policy. Kept in a dedicated file so upstream refactors of
// conductor.go do not silently drop them.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexinstructions"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func privateInstructionsModeFromMetadata(meta map[string]any) bool {
	return codexinstructions.RequestIsPrivate(meta)
}

func codexInstructionsSelectionConfig(m *Manager) (requireAllow bool, reserveMarked bool) {
	requireAllow = true
	if m == nil {
		return requireAllow, false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return requireAllow, false
	}
	if cfg.Codex.Instructions.RequireAuthAllow != nil {
		requireAllow = *cfg.Codex.Instructions.RequireAuthAllow
	}
	return requireAllow, cfg.Codex.Instructions.ReserveMarkedAuths
}

func codexPreferFreeForSharedModels(m *Manager) bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.Codex.Routing.PreferFreeForSharedModels
}

func isCodexCredential(auth *Auth) bool {
	if auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || executorKeyFromAuth(auth) == "codex"
}

// authMatchesPrivateInstructionsPolicy filters candidates for private/normal instruction mode.
// Private requests optionally require allow_private_instructions.
// When reserve-marked-auths is enabled, marked auths are excluded from normal traffic.
func authMatchesPrivateInstructionsPolicy(auth *Auth, privateRequest bool, requireAllow bool, reserveMarked bool) bool {
	if auth == nil {
		return false
	}
	// Only apply for codex credentials; other providers ignore this gate.
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && executorKeyFromAuth(auth) != "codex" {
		return !privateRequest || !requireAllow
	}
	allows := codexinstructions.AuthAllows(auth.Attributes, auth.Metadata)
	if privateRequest {
		if requireAllow {
			return allows
		}
		return true
	}
	if reserveMarked && allows {
		return false
	}
	return true
}

func recoverQoderQuotaProbeDisable(auth *Auth, now time.Time) bool {
	if auth == nil || !isQoderProvider(auth.Provider) || auth.Metadata == nil {
		return false
	}
	reason, _ := auth.Metadata["disabled_reason"].(string)
	reason = strings.TrimSpace(reason)
	if reason == "" || !strings.Contains(strings.ToLower(reason), "token is not active") {
		return false
	}
	// v7.2.109 incorrectly let management quota probes permanently disable an
	// auth. Execution failures are wrapped as Qoder API errors, so recover only
	// the unwrapped probe response and retain real request-path auto-disable.
	lowerReason := strings.ToLower(reason)
	if strings.Contains(lowerReason, "qoder") && strings.Contains(lowerReason, "api error 401") {
		return false
	}
	auth.Disabled = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.LastError = nil
	auth.UpdatedAt = now
	auth.Metadata["disabled"] = false
	delete(auth.Metadata, "disabled_reason")
	return true
}

func (m *Manager) shouldAutoDisableQoderAuth(result Result) bool {
	if m == nil || !isQoderProvider(result.Provider) || result.Error == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.Qoder.AutoDisableInactiveTokenEnabled() {
		return false
	}
	return result.Error.HTTPStatus == http.StatusUnauthorized && isQoderInactiveTokenError(result.Error)
}

func (m *Manager) disableQoderAuthForInactiveToken(auth *Auth, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	reason := "Qoder token is not active"
	if resultErr != nil && strings.TrimSpace(resultErr.Message) != "" {
		reason = resultErr.Message
	}
	auth.Disabled = true
	auth.Status = StatusDisabled
	auth.StatusMessage = reason
	auth.UpdatedAt = now
	auth.LastError = cloneError(resultErr)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = true
	auth.Metadata["disabled_reason"] = reason
}

func isQoderProvider(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return provider == "qoder" || provider == "qodercn"
}

func isQoderInactiveTokenError(resultErr *Error) bool {
	if resultErr == nil || resultErr.HTTPStatus != http.StatusUnauthorized {
		return false
	}
	return strings.Contains(strings.ToLower(resultErr.Message+" "+resultErr.Code), "token is not active")
}

func isQoderQueuedForbiddenError(resultErr *Error) bool {
	if resultErr == nil || resultErr.HTTPStatus != http.StatusForbidden {
		return false
	}
	// Qoder nests its queue details in JSON strings, which leaves backslash
	// escaping in the propagated error. Strip it before checking the stable
	// upstream code and queue marker.
	body := strings.ToLower(strings.ReplaceAll(resultErr.Message+" "+resultErr.Code, "\\", ""))
	return strings.Contains(body, "10605") && strings.Contains(body, `"isqueued":true`)
}

func (m *Manager) shouldAutoDisableXAIAuth(result Result) bool {
	if m == nil || !strings.EqualFold(strings.TrimSpace(result.Provider), "xai") || result.Error == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.XAI.AutoDisablePermissionDeniedEnabled() {
		return false
	}
	// 401: execute paths already try one OAuth refresh before MarkResult; a surviving
	// 401 (or bearer keys with nothing to refresh) is treated as permanent auth death.
	if result.Error.HTTPStatus == http.StatusUnauthorized {
		return true
	}
	if result.Error.HTTPStatus != http.StatusForbidden {
		return false
	}
	return isXAIPermissionDeniedError(result.Error)
}

func (m *Manager) disableXAIAuthForPermissionFailure(auth *Auth, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	reason := "xAI permission denied"
	if resultErr != nil && resultErr.HTTPStatus == http.StatusUnauthorized {
		reason = "xAI unauthorized"
	}
	if resultErr != nil && strings.TrimSpace(resultErr.Message) != "" {
		reason = resultErr.Message
	}
	m.disableXAIAuth(auth, reason, resultErr, now)
}

func (m *Manager) disableXAIAuth(auth *Auth, reason string, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "xAI auth disabled"
	}
	auth.Disabled = true
	auth.Status = StatusDisabled
	auth.StatusMessage = reason
	auth.UpdatedAt = now
	auth.LastError = cloneError(resultErr)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = true
	auth.Metadata["disabled_reason"] = reason
}

func (m *Manager) xaiExhaustionKindForResult(result Result) xaiExhaustionKind {
	if m == nil || !strings.EqualFold(strings.TrimSpace(result.Provider), "xai") || result.Error == nil {
		return ""
	}
	if isXAIFreeUsageExhaustedError(result.Error) {
		return xaiExhaustionFreeUsage
	}
	if result.Error.HTTPStatus == http.StatusForbidden && !isXAIPermissionDeniedError(result.Error) {
		return xaiExhaustionOtherForbidden
	}
	return ""
}

// claudeRateLimitFallbackCooldown is used when Anthropic returns 429 without any
// Retry-After header or resets_at body hint. Anthropic 5-hour windows are the
// common case, so a 5-minute recheck is frequent enough to pick the account back
// up quickly without retrying a genuinely exhausted credential every second.
const claudeRateLimitFallbackCooldown = 5 * time.Minute

// effectiveRetryAfterForResult returns the provider RetryAfter when present; otherwise, for
// known xAI free-usage / other-403 or Codex usage-limit failures, the configured fallback.
func (m *Manager) effectiveRetryAfterForResult(result Result) *time.Duration {
	if result.RetryAfter != nil {
		value := *result.RetryAfter
		return &value
	}
	if m == nil || result.Error == nil {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(result.Provider))
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	switch provider {
	case "qoder", "qodercn":
		if isQoderQueuedForbiddenError(result.Error) {
			policy := internalconfig.DefaultQoderConfig()
			if cfg != nil {
				policy = internalconfig.NormalizeQoderConfig(cfg.Qoder)
			}
			d := time.Duration(policy.QueuedForbiddenCooldownMinutesValue()) * time.Minute
			return &d
		}
	case "xai":
		policy := internalconfig.DefaultXAIConfig()
		if cfg != nil {
			policy = internalconfig.NormalizeXAIConfig(cfg.XAI)
		}
		if isXAIFreeUsageExhaustedError(result.Error) || isXAIFreeUsageExhaustedErrorMessage(result.Error) {
			d := time.Duration(policy.FreeUsageExhaustedCooldownHoursValue()) * time.Hour
			return &d
		}
		if result.Error.HTTPStatus == http.StatusForbidden && !isXAIPermissionDeniedError(result.Error) {
			d := time.Duration(policy.OtherForbiddenCooldownHoursValue()) * time.Hour
			return &d
		}
	case "claude":
		// Anthropic 429s carry their reset time in headers/body, which the executor
		// surfaces as result.RetryAfter (handled above). When it is absent, fall back
		// to a fixed cooldown instead of the 1s-and-doubling quota ladder, which would
		// hammer an account whose 5-hour or weekly window is genuinely exhausted.
		if result.Error.HTTPStatus == http.StatusTooManyRequests {
			d := claudeRateLimitFallbackCooldown
			return &d
		}
	case "codex":
		policy := internalconfig.DefaultCodexFailurePolicy()
		if cfg != nil {
			policy = internalconfig.NormalizeCodexConfig(cfg.Codex)
		}
		if isCodexUsageLimitError(result.Error) || isCodexUsageLimitErrorMessage(result.Error) {
			hours := policy.UsageLimitCooldownFallbackHoursValue()
			if hours <= 0 {
				return nil
			}
			d := time.Duration(hours) * time.Hour
			return &d
		}
	}
	return nil
}

func (m *Manager) codexExhaustionKindForResult(result Result) codexExhaustionKind {
	if m == nil || !strings.EqualFold(strings.TrimSpace(result.Provider), "codex") || result.Error == nil {
		return ""
	}
	if isCodexUsageLimitError(result.Error) || isCodexUsageLimitErrorMessage(result.Error) {
		return codexExhaustionUsageLimit
	}
	if isCodexHardAuthFailureError(result.Error) {
		return codexExhaustionAuthFailure
	}
	return ""
}

// trackCodexExhaustionCounter increments usage-limit / auth-failure counters and may disable
// the auth file. Returns true when the auth was disabled.
func (m *Manager) trackCodexExhaustionCounter(auth *Auth, result Result, kind codexExhaustionKind, now time.Time) bool {
	if m == nil || auth == nil || kind == "" {
		return false
	}
	// Auth-failure disable can run without a model name (file-level death).
	if kind == codexExhaustionUsageLimit && strings.TrimSpace(result.Model) == "" {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return false
	}

	threshold := 0
	switch kind {
	case codexExhaustionUsageLimit:
		threshold = cfg.Codex.UsageLimitDisableAfterValue()
	case codexExhaustionAuthFailure:
		threshold = cfg.Codex.AuthFailureDisableAfterValue()
	default:
		return false
	}
	if threshold <= 0 {
		return false
	}

	model := strings.TrimSpace(result.Model)
	if model == "" {
		// File-level auth death: use a synthetic model key only for counter storage.
		model = "_auth"
	}
	state := ensureModelState(auth, model)
	// Do not double-count while the model is still inside an active cooldown window.
	if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
		return false
	}

	switch kind {
	case codexExhaustionUsageLimit:
		state.UsageLimitCount++
		if state.UsageLimitCount >= threshold {
			reason := fmt.Sprintf(
				"Codex usage limit always exhausted (counter=%d, threshold=%d)",
				state.UsageLimitCount,
				threshold,
			)
			m.disableCodexAuth(auth, reason, result.Error, now)
			return true
		}
	case codexExhaustionAuthFailure:
		state.AuthFailureCount++
		if state.AuthFailureCount >= threshold {
			reason := fmt.Sprintf(
				"Codex auth failure (counter=%d, threshold=%d)",
				state.AuthFailureCount,
				threshold,
			)
			if result.Error != nil && strings.TrimSpace(result.Error.Message) != "" {
				// Prefer the upstream body as disabled_reason when present.
				reason = result.Error.Message
			}
			m.disableCodexAuth(auth, reason, result.Error, now)
			return true
		}
	}
	return false
}

// clearDisabledReasonOnSuccessfulAuth removes an automatic authentication diagnostic after
// a confirmed successful request or provider health check. Explicitly disabled auths retain
// their reason until the user re-enables or replaces them.
func clearDisabledReasonOnSuccessfulAuth(auth *Auth, now time.Time) {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || auth.Metadata == nil {
		return
	}
	if hasModelError(auth, now) {
		return
	}
	delete(auth.Metadata, "disabled_reason")
}

func (m *Manager) disableCodexAuth(auth *Auth, reason string, resultErr *Error, now time.Time) {
	if auth == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Codex auth disabled"
	}
	auth.Disabled = true
	auth.Status = StatusDisabled
	auth.StatusMessage = reason
	auth.UpdatedAt = now
	auth.LastError = cloneError(resultErr)
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = true
	auth.Metadata["disabled_reason"] = reason
}

// trackXAIExhaustionCounter increments post-cooldown exhaustion counters on the auth file
// runtime block. Returns true when the auth was disabled after reaching the configured threshold.
func (m *Manager) trackXAIExhaustionCounter(auth *Auth, result Result, kind xaiExhaustionKind, now time.Time) bool {
	if m == nil || auth == nil || kind == "" || strings.TrimSpace(result.Model) == "" {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return false
	}

	threshold := 0
	switch kind {
	case xaiExhaustionFreeUsage:
		threshold = cfg.XAI.FreeUsageExhaustedDisableAfterValue()
	case xaiExhaustionOtherForbidden:
		threshold = cfg.XAI.OtherForbiddenDisableAfterValue()
	default:
		return false
	}
	if threshold <= 0 {
		return false
	}

	state := ensureModelState(auth, result.Model)
	// Do not double-count while the model is still inside an active cooldown window.
	if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
		return false
	}

	switch kind {
	case xaiExhaustionFreeUsage:
		state.FreeUsageExhaustionCount++
		if state.FreeUsageExhaustionCount >= threshold {
			reason := fmt.Sprintf(
				"xAI free usage always exhausted (counter=%d, threshold=%d)",
				state.FreeUsageExhaustionCount,
				threshold,
			)
			m.disableXAIAuth(auth, reason, result.Error, now)
			return true
		}
	case xaiExhaustionOtherForbidden:
		state.OtherForbiddenCount++
		if state.OtherForbiddenCount >= threshold {
			reason := fmt.Sprintf(
				"xAI other 403 always exhausted (counter=%d, threshold=%d)",
				state.OtherForbiddenCount,
				threshold,
			)
			m.disableXAIAuth(auth, reason, result.Error, now)
			return true
		}
	}
	return false
}

// xaiExhaustionKind identifies repeated xAI failures that can disable an auth after N hits.
type xaiExhaustionKind string

const (
	xaiExhaustionFreeUsage      xaiExhaustionKind = "free_usage"
	xaiExhaustionOtherForbidden xaiExhaustionKind = "other_403"
)

func isXAIFreeUsageExhaustedError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	// Prefer an explicit 429, but also accept free-usage body text when status was lost
	// on the stream path so cooldown + exhaustion counters still apply.
	if resultErr.HTTPStatus != 0 && resultErr.HTTPStatus != http.StatusTooManyRequests {
		return false
	}
	return isXAIFreeUsageExhaustedErrorMessage(resultErr)
}

func isXAIFreeUsageExhaustedErrorMessage(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	body := strings.ToLower(resultErr.Message)
	return strings.Contains(body, "free-usage-exhausted") ||
		strings.Contains(body, "included free usage")
}

func isXAIPermissionDeniedError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	var payload struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(resultErr.Message), &payload); err != nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(payload.Code), "permission-denied") {
		return true
	}
	var errorMessage string
	if err := json.Unmarshal(payload.Error, &errorMessage); err == nil {
		return strings.EqualFold(strings.TrimSpace(errorMessage), "access denied.")
	}
	var nestedError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload.Error, &nestedError); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(nestedError.Code), "permission-denied") ||
		strings.EqualFold(strings.TrimSpace(nestedError.Message), "access denied.")
}

// codexExhaustionKind identifies Codex failures that can disable an auth after N hits.
type codexExhaustionKind string

const (
	codexExhaustionUsageLimit  codexExhaustionKind = "usage_limit"
	codexExhaustionAuthFailure codexExhaustionKind = "auth_failure"
)

func isCodexUsageLimitError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	// usage_limit_reached is often remapped to 429; accept body match when status is 0/429/400.
	status := resultErr.HTTPStatus
	if status != 0 && status != http.StatusTooManyRequests && status != http.StatusBadRequest {
		return false
	}
	return isCodexUsageLimitErrorMessage(resultErr)
}

func isCodexUsageLimitErrorMessage(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	body := strings.ToLower(resultErr.Message + " " + resultErr.Code)
	return strings.Contains(body, "usage_limit_reached") ||
		strings.Contains(body, "you've hit your usage limit") ||
		strings.Contains(body, "hit your usage limit")
}

func isCodexHardAuthFailureError(resultErr *Error) bool {
	if resultErr == nil {
		return false
	}
	if isInvalidGrantResultError(resultErr) {
		return true
	}
	body := strings.ToLower(resultErr.Message + " " + resultErr.Code)
	// Workspace/account death codes even when status is missing or remapped.
	if strings.Contains(body, "deactivated_workspace") ||
		strings.Contains(body, "auth_unavailable") ||
		strings.Contains(body, "authentication_error") ||
		strings.Contains(body, "invalid_api_key") ||
		strings.Contains(body, "invalid or expired token") ||
		strings.Contains(body, "refresh_token_reused") ||
		strings.Contains(body, "token has been expired or revoked") ||
		strings.Contains(body, "access token invalidated") ||
		strings.Contains(body, "needs re-auth") ||
		strings.Contains(body, "reauthorize") ||
		strings.Contains(body, "re-authenticate") {
		return true
	}
	status := resultErr.HTTPStatus
	switch status {
	case http.StatusUnauthorized:
		// 401 without request-scoped noise is treated as auth death for Codex OAuth pools.
		if isRequestScopedNotFoundResultError(resultErr) || isModelSupportResultError(resultErr) {
			return false
		}
		return true
	case http.StatusPaymentRequired:
		// Codex 402 (e.g. deactivated_workspace) means the account/workspace is unusable.
		// Treat as file-level auth death so the credential leaves the pool instead of a 30m cool loop.
		return true
	default:
		return false
	}
}

// CodexInstructionMarkers returns configured private instruction model markers.
func (m *Manager) CodexInstructionMarkers() codexinstructions.MarkerConfig {
	if m == nil {
		return codexinstructions.DefaultMarkers()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return codexinstructions.DefaultMarkers()
	}
	return codexinstructions.MarkerConfig{
		Prefixes: cfg.Codex.Instructions.RequestMarkers.Prefixes,
		Suffixes: cfg.Codex.Instructions.RequestMarkers.Suffixes,
	}
}

// CodexInstructionsConfig returns a copy of the runtime private-instructions config, or nil.
func (m *Manager) CodexInstructionsConfig() *internalconfig.CodexInstructionsConfig {
	if m == nil {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return nil
	}
	copyCfg := cfg.Codex.Instructions
	return &copyCfg
}

// CodexInstructionsApplyWithoutPrefixSuffix reports whether a model should use private mode without markers.
func (m *Manager) CodexInstructionsApplyWithoutPrefixSuffix(model string) bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.Codex.Instructions.Enabled || cfg.Codex.Instructions.UsePrefixSuffix == nil || *cfg.Codex.Instructions.UsePrefixSuffix {
		return false
	}
	return codexinstructions.ModelMatches(cfg.Codex.Instructions.Models, model)
}

func mergeModelStatesConservatively(existing, incoming map[string]*ModelState, now time.Time) map[string]*ModelState {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	merged := make(map[string]*ModelState, len(existing)+len(incoming))
	for model, state := range incoming {
		merged[model] = state.Clone()
	}
	for model, existingState := range existing {
		if existingState == nil {
			continue
		}
		incomingState := merged[model]
		if incomingState == nil {
			merged[model] = existingState.Clone()
			continue
		}

		existingCooling := existingState.Unavailable && !existingState.NextRetryAfter.IsZero() && existingState.NextRetryAfter.After(now)
		incomingCooling := incomingState.Unavailable && !incomingState.NextRetryAfter.IsZero() && incomingState.NextRetryAfter.After(now)
		if existingCooling && (!incomingCooling || existingState.NextRetryAfter.After(incomingState.NextRetryAfter)) {
			incomingState = existingState.Clone()
			merged[model] = incomingState
		}
		if existingState.FreeUsageExhaustionCount > incomingState.FreeUsageExhaustionCount {
			incomingState.FreeUsageExhaustionCount = existingState.FreeUsageExhaustionCount
		}
		if existingState.OtherForbiddenCount > incomingState.OtherForbiddenCount {
			incomingState.OtherForbiddenCount = existingState.OtherForbiddenCount
		}
		if existingState.UsageLimitCount > incomingState.UsageLimitCount {
			incomingState.UsageLimitCount = existingState.UsageLimitCount
		}
		if existingState.AuthFailureCount > incomingState.AuthFailureCount {
			incomingState.AuthFailureCount = existingState.AuthFailureCount
		}
	}
	return merged
}
