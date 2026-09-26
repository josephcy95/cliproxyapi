package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	codexAdaptiveUsageEndpoint        = "https://chatgpt.com/backend-api/wham/usage"
	codexAdaptiveResetCreditsEndpoint = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

func (m *Manager) codexAdaptiveEnabled() bool {
	if m == nil || m.scheduler == nil {
		return false
	}
	return m.scheduler.codexAdaptiveEnabled()
}

// refreshCodexAdaptiveQuota probes only one stale paid candidate. Normal
// response/websocket observations remain the primary source of truth.
func (m *Manager) refreshCodexAdaptiveQuota(ctx context.Context, model string, opts cliproxyexecutor.Options, tried map[string]struct{}) {
	if m == nil || !m.codexAdaptiveEnabled() {
		return
	}
	m.syncPersistedCodexQuotaSnapshots()
	if ctx == nil {
		ctx = context.Background()
	}
	eligibility := m.authSelectionEligibilityForManager(ctx, opts, model)
	now := time.Now()
	var candidate *Auth
	var candidateState codexAdaptiveAccount
	reg := registry.GetGlobalRegistry()

	m.mu.RLock()
	if eligibility.preferFreeCodexAuths {
		for _, auth := range m.auths {
			if auth == nil || auth.Disabled || !isFreeCodexAuth(auth) || !eligibility.allows(auth) {
				continue
			}
			if _, used := tried[auth.ID]; used {
				continue
			}
			if model != "" && !m.authSupportsRouteModel(reg, auth, model) {
				continue
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, m.selectionModelForAuth(auth, model), now); !blocked {
				state := m.schedulerAdaptiveAccount(auth.ID)
				if state.inFlight < state.limit {
					m.mu.RUnlock()
					return
				}
			}
		}
	}

	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || !isCodexCredential(auth) || isFreeCodexAuth(auth) || !eligibility.allows(auth) {
			continue
		}
		if _, used := tried[auth.ID]; used {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(reg, auth, model) {
			continue
		}
		state := m.schedulerAdaptiveAccount(auth.ID)
		if state.inFlight >= state.limit || !quotaProbeNeeded(auth, &state, now) {
			continue
		}
		if candidate == nil || adaptiveScoreFor(auth, state, now).betterThan(adaptiveScoreFor(candidate, candidateState, now)) {
			candidate = auth.Clone()
			candidateState = state
		}
	}
	m.mu.RUnlock()
	if candidate == nil {
		return
	}
	if !m.scheduler.beginAdaptiveQuotaProbe(candidate.ID, now) {
		return
	}
	probeCtx := context.WithoutCancel(ctx)
	go func(candidate *Auth) {
		_, errProbe := m.probeCodexQuota(probeCtx, candidate)
		m.scheduler.finishAdaptiveQuotaProbe(candidate.ID, errProbe == nil)
		if errProbe != nil {
			log.WithError(errProbe).WithField("auth_id", candidate.ID).Debug("codex adaptive quota probe failed")
		}
	}(candidate)
}

func (m *Manager) schedulerAdaptiveAccount(authID string) codexAdaptiveAccount {
	if m == nil || m.scheduler == nil {
		return codexAdaptiveAccount{limit: codexAdaptiveDefaultConcurrency}
	}
	return m.scheduler.adaptiveAccount(authID)
}

func (m *Manager) probeCodexQuota(ctx context.Context, auth *Auth) (*Auth, error) {
	if m == nil || auth == nil {
		return nil, fmt.Errorf("codex quota probe: auth is nil")
	}
	executor, ok := m.Executor("codex")
	if !ok || executor == nil {
		return nil, fmt.Errorf("codex quota probe: executor not registered")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexAdaptiveUsageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	setCodexAdaptiveQuotaHeaders(req, auth, false)
	resp, err := executor.HttpRequest(ctx, auth, req)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("codex quota probe: empty response")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("codex quota probe: upstream status %d", resp.StatusCode)
	}

	headers := resp.Header.Clone()
	for key, value := range codexQuotaHeadersFromUsage(body) {
		headers[key] = value
	}
	if len(headers) == 0 {
		return nil, fmt.Errorf("codex quota probe: response contained no quota data")
	}

	updated := auth.Clone()
	if !updated.Quota.ObserveResponseHeadersForProvider("codex", headers, time.Now()) {
		return nil, fmt.Errorf("codex quota probe: response contained no recognized quota data")
	}
	metadataUpdates := codexQuotaMetadataFromUsage(body)
	if codexMetadataResetCreditCount(metadataUpdates) > 0 || codexAvailableResetCredits(auth) > 0 {
		if metadataUpdates == nil {
			metadataUpdates = make(map[string]any)
		}
		if resetCredits, errCredits := m.probeCodexResetCredits(ctx, auth); errCredits == nil {
			for key, value := range resetCredits {
				metadataUpdates[key] = value
			}
		}
	}

	m.mu.Lock()
	current := m.auths[auth.ID]
	if current == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("codex quota probe: auth no longer exists")
	}
	current.Quota = mergeQuotaObservation(current.Quota, updated.Quota)
	if len(metadataUpdates) > 0 {
		if current.Metadata == nil {
			current.Metadata = make(map[string]any)
		}
		for key, value := range metadataUpdates {
			current.Metadata[key] = value
		}
	}
	current.UpdatedAt = time.Now()
	result := current.Clone()
	m.mu.Unlock()

	if m.scheduler != nil {
		m.scheduler.upsertAuth(result)
	}
	if errPersist := m.persist(ctx, result); errPersist != nil {
		return result, errPersist
	}
	return result, nil
}

func codexMetadataResetCreditCount(metadata map[string]any) int {
	return codexAvailableResetCreditsFromMetadata(metadata, time.Now())
}

func setCodexAdaptiveQuotaHeaders(req *http.Request, auth *Auth, resetCredits bool) {
	if req == nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	if auth != nil && auth.Metadata != nil {
		if accountID, ok := auth.Metadata["account_id"].(string); ok {
			if accountID = strings.TrimSpace(accountID); accountID != "" {
				req.Header.Set("ChatGPT-Account-ID", accountID)
			}
		}
	}
	if resetCredits {
		req.Header.Set("OpenAI-Beta", "codex-1")
		req.Header.Set("Originator", "Codex Desktop")
	}
}

func (m *Manager) probeCodexResetCredits(ctx context.Context, auth *Auth) (map[string]any, error) {
	executor, ok := m.Executor("codex")
	if !ok || executor == nil {
		return nil, fmt.Errorf("codex reset-credit probe: executor not registered")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexAdaptiveResetCreditsEndpoint, nil)
	if err != nil {
		return nil, err
	}
	setCodexAdaptiveQuotaHeaders(req, auth, true)
	resp, err := executor.HttpRequest(ctx, auth, req)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("codex reset-credit probe: empty response")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("codex reset-credit probe: upstream status %d", resp.StatusCode)
	}
	root := gjson.ParseBytes(body)
	credits := firstUsageResult(root, "credits")
	if credits.Exists() && !credits.IsArray() {
		return nil, fmt.Errorf("codex reset-credit probe: response contained no credits")
	}
	creditValue := any([]any{})
	if credits.IsArray() {
		creditValue = credits.Value()
	} else if !firstUsageResult(root, "available_count", "availableCount").Exists() {
		return nil, fmt.Errorf("codex reset-credit probe: response contained no credits")
	}
	updates := map[string]any{
		"rate_limit_reset_credits":            creditValue,
		"rate_limit_reset_credits_checked_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	for key, paths := range map[string][]string{
		"rate_limit_reset_credits_available_count":            {"available_count", "availableCount"},
		"rate_limit_reset_credits_applicable_available_count": {"applicable_available_count", "applicableAvailableCount"},
	} {
		if value := firstUsageResult(root, paths...); value.Exists() {
			updates[key] = value.Int()
		}
	}
	return updates, nil
}

func codexQuotaHeadersFromUsage(body []byte) http.Header {
	if len(body) == 0 {
		return nil
	}
	root := gjson.ParseBytes(body)
	headers := make(http.Header)
	rate := firstUsageResult(root, "rate_limits", "rateLimit", "rate_limit")
	if !rate.Exists() {
		rate = root
	}
	addUsageRateHeaders(headers, "X-Codex-", rate)

	additional := firstUsageResult(root, "additional_rate_limits", "additionalRateLimits")
	if additional.Exists() {
		additional.ForEach(func(key, value gjson.Result) bool {
			name := strings.TrimSpace(key.String())
			if additional.IsArray() {
				name = firstUsageString(value, "limit_name", "limitName", "name")
			}
			if name == "" {
				return true
			}
			identifier := quotaHeaderIdentifier(name)
			info := firstUsageResult(value, "rate_limit", "rateLimit")
			if !info.Exists() {
				info = value
			}
			addUsageRateHeaders(headers, "X-Codex-Additional-"+identifier+"-", info)
			return true
		})
	}
	if plan := firstUsageString(root, "plan_type", "planType"); plan != "" {
		headers.Set("X-Codex-Plan-Type", plan)
	}
	if active := firstUsageString(root, "metered_limit_name", "meteredLimitName", "limit_name", "limitName"); active != "" {
		headers.Set("X-Codex-Active-Limit", active)
	}
	return headers
}

func codexQuotaMetadataFromUsage(body []byte) map[string]any {
	root := gjson.ParseBytes(body)
	updates := make(map[string]any)
	credits := firstUsageResult(root, "rate_limit_reset_credits", "rateLimitResetCredits")
	for key, paths := range map[string][]string{
		"rate_limit_reset_credits_available_count":            {"available_count", "availableCount"},
		"rate_limit_reset_credits_applicable_available_count": {"applicable_available_count", "applicableAvailableCount"},
	} {
		if value := firstUsageResult(credits, paths...); value.Exists() {
			updates[key] = value.Int()
		}
	}
	if value := firstUsageResult(root, "chatgpt_subscription_active_until", "subscription_active_until", "subscriptionActiveUntil"); value.Exists() {
		updates["chatgpt_subscription_active_until"] = usageScalar(value)
	}
	if len(updates) == 0 {
		return nil
	}
	updates["rate_limit_reset_credits_checked_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	return updates
}

func quotaHeaderIdentifier(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else if b.Len() == 0 || !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-_.")
}

func addUsageRateHeaders(headers http.Header, prefix string, rate gjson.Result) {
	if !rate.Exists() || !rate.IsObject() {
		return
	}
	for _, windowName := range []string{"primary", "secondary"} {
		window := firstUsageResult(rate, windowName, windowName+"_window", windowName+"Window")
		if !window.Exists() || !window.IsObject() {
			continue
		}
		used := firstUsageResult(window, "used_percent", "usedPercent")
		minutes := firstUsageResult(window, "window_minutes", "windowMinutes")
		resetAfter := firstUsageResult(window, "reset_after_seconds", "resetAfterSeconds")
		resetAt := firstUsageResult(window, "reset_at", "resetAt")
		if !used.Exists() || !minutes.Exists() || !resetAfter.Exists() && !resetAt.Exists() {
			continue
		}
		windowPrefix := prefix + strings.ToUpper(windowName[:1]) + windowName[1:] + "-"
		if raw := usageScalar(used); raw != "" {
			headers.Set(windowPrefix+"Used-Percent", raw)
		}
		if raw := usageScalar(minutes); raw != "" {
			headers.Set(windowPrefix+"Window-Minutes", raw)
		}
		if raw := usageScalar(resetAfter); raw != "" {
			headers.Set(windowPrefix+"Reset-After-Seconds", raw)
		}
		if raw := usageScalar(resetAt); raw != "" {
			headers.Set(windowPrefix+"Reset-At", raw)
		}
	}
}

func firstUsageResult(object gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		value := object.Get(path)
		if value.Exists() && value.Type != gjson.Null {
			return value
		}
	}
	return gjson.Result{}
}

func firstUsageString(object gjson.Result, paths ...string) string {
	return usageScalar(firstUsageResult(object, paths...))
}

func usageScalar(value gjson.Result) string {
	if !value.Exists() {
		return ""
	}
	if value.Type == gjson.String {
		return strings.TrimSpace(value.String())
	}
	if value.Type == gjson.Number || value.Type == gjson.True || value.Type == gjson.False {
		return strings.TrimSpace(value.Raw)
	}
	return ""
}

func (m *Manager) observeCodexAdaptiveResult(result Result) {
	if m == nil || m.scheduler == nil || adaptiveLeaseID(result.Options) == "" {
		return
	}
	m.scheduler.observeAdaptiveResult(result)
}

func (m *Manager) releaseCodexAdaptiveLease(options cliproxyexecutor.Options) {
	if m != nil && m.scheduler != nil {
		m.scheduler.releaseAdaptiveLease(options)
	}
}
