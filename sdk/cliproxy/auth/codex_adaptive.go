package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	codexAdaptiveStrategy                = "adaptive"
	codexAdaptiveDefaultConcurrency      = 2
	codexAdaptiveMaxConcurrency          = 4
	codexAdaptiveQuotaStaleAfter         = 15 * time.Minute
	codexAdaptiveQuotaProbeInterval      = 5 * time.Minute
	codexAdaptiveProbeFailureBackoff     = 30 * time.Second
	codexAdaptiveLeaseTTL                = 10 * time.Minute
	codexAdaptiveWaitInterval            = 100 * time.Millisecond
	codexAdaptiveMaxWait                 = 2 * time.Second
	codexAdaptiveSuccessesBeforeIncrease = 8
	adaptiveExecutionMetadataKey         = "codex_adaptive_execution"
	adaptiveLeaseMetadataKey             = "codex_adaptive_lease"
)

type codexAdaptiveAccount struct {
	inFlight      int
	limit         int
	successes     int
	lastProbeAt   time.Time
	probeFailedAt time.Time
	probeInFlight bool
}

type codexAdaptiveLease struct {
	authID    string
	expiresAt time.Time
	done      chan struct{}
}

type codexAdaptiveRouter struct {
	mu       sync.Mutex
	accounts map[string]*codexAdaptiveAccount
	leases   map[string]codexAdaptiveLease
	changed  chan struct{}
}

func newCodexAdaptiveRouter() *codexAdaptiveRouter {
	return &codexAdaptiveRouter{
		accounts: make(map[string]*codexAdaptiveAccount),
		leases:   make(map[string]codexAdaptiveLease),
		changed:  make(chan struct{}),
	}
}

func (r *codexAdaptiveRouter) accountLocked(id string) *codexAdaptiveAccount {
	state := r.accounts[id]
	if state == nil {
		state = &codexAdaptiveAccount{limit: codexAdaptiveDefaultConcurrency}
		r.accounts[id] = state
	}
	return state
}

func (r *codexAdaptiveRouter) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *codexAdaptiveRouter) pruneLeasesLocked(now time.Time) {
	changed := false
	for leaseID, lease := range r.leases {
		if lease.expiresAt.After(now) {
			continue
		}
		state := r.accountLocked(lease.authID)
		if state.inFlight > 0 {
			state.inFlight--
		}
		delete(r.leases, leaseID)
		close(lease.done)
		changed = true
	}
	if changed {
		r.notifyLocked()
	}
}

func (r *codexAdaptiveRouter) pick(ctx context.Context, candidates []*Auth, preferFree bool) (*Auth, string, error) {
	if r == nil || len(candidates) == 0 {
		return nil, "", &Error{Code: "auth_unavailable", Message: "no auth available"}
	}
	waitTimer := time.NewTimer(codexAdaptiveMaxWait)
	defer waitTimer.Stop()
	waitForChange := func() error {
		if errContext := contextError(ctx); errContext != nil {
			return errContext
		}
		r.mu.Lock()
		changed := r.changed
		r.mu.Unlock()
		timer := time.NewTimer(codexAdaptiveWaitInterval)
		defer timer.Stop()
		select {
		case <-changed:
			return nil
		case <-timer.C:
			return nil
		case <-waitTimer.C:
			return &Error{Code: "auth_unavailable", HTTPStatus: http.StatusServiceUnavailable, Message: "Codex adaptive capacity is busy"}
		case <-ctxDone(ctx):
			return ctx.Err()
		}
	}

	for {
		now := time.Now()
		selected := r.best(candidates, preferFree, now)
		if selected == nil {
			if errWait := waitForChange(); errWait != nil {
				return nil, "", errWait
			}
			continue
		}

		r.mu.Lock()
		r.pruneLeasesLocked(now)
		state := r.accountLocked(selected.ID)
		if state.inFlight >= state.limit {
			r.mu.Unlock()
			if errWait := waitForChange(); errWait != nil {
				return nil, "", errWait
			}
			continue
		}
		state.inFlight++
		leaseID := fmt.Sprintf("%s:%d", selected.ID, time.Now().UnixNano())
		done := make(chan struct{})
		r.leases[leaseID] = codexAdaptiveLease{authID: selected.ID, expiresAt: now.Add(codexAdaptiveLeaseTTL), done: done}
		r.mu.Unlock()
		r.renewLeaseUntilDone(ctx, leaseID, done)
		return selected, leaseID, nil
	}
}

func (r *codexAdaptiveRouter) renewLeaseUntilDone(ctx context.Context, leaseID string, done <-chan struct{}) {
	if r == nil || leaseID == "" {
		return
	}
	interval := codexAdaptiveLeaseTTL / 2
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.mu.Lock()
				lease, ok := r.leases[leaseID]
				if ok {
					lease.expiresAt = time.Now().Add(codexAdaptiveLeaseTTL)
					r.leases[leaseID] = lease
				}
				r.mu.Unlock()
				if !ok {
					return
				}
			case <-ctxDone(ctx):
				r.release(leaseID)
				return
			case <-done:
				return
			}
		}
	}()
}

func adaptiveLeaseID(options cliproxyexecutor.Options) string {
	if options.Metadata == nil {
		return ""
	}
	if enabled, ok := options.Metadata[adaptiveExecutionMetadataKey].(bool); !ok || !enabled {
		return ""
	}
	leaseID, _ := options.Metadata[adaptiveLeaseMetadataKey].(string)
	return strings.TrimSpace(leaseID)
}

func markAdaptiveLease(options cliproxyexecutor.Options, leaseID string) bool {
	if leaseID == "" {
		return true
	}
	if options.Metadata == nil {
		return false
	}
	options.Metadata[adaptiveExecutionMetadataKey] = true
	options.Metadata[adaptiveLeaseMetadataKey] = leaseID
	return true
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

func (r *codexAdaptiveRouter) best(candidates []*Auth, preferFree bool, now time.Time) *Auth {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLeasesLocked(now)

	freeAvailable := false
	if preferFree {
		for _, candidate := range candidates {
			if candidate == nil || !isFreeCodexAuth(candidate) {
				continue
			}
			state := r.accountLocked(candidate.ID)
			if state.inFlight < state.limit && codexQuotaCandidateAvailable(candidate, now) {
				freeAvailable = true
				break
			}
		}
	}

	var selected *Auth
	var selectedScore adaptiveScore
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == "" {
			continue
		}
		if freeAvailable && !isFreeCodexAuth(candidate) {
			continue
		}
		if !codexQuotaCandidateAvailable(candidate, now) {
			continue
		}
		state := r.accountLocked(candidate.ID)
		if state.inFlight >= state.limit {
			continue
		}
		score := adaptiveScoreFor(candidate, *state, now)
		if selected == nil || score.betterThan(selectedScore) {
			selected = candidate
			selectedScore = score
		}
	}
	return selected
}

type adaptiveScore struct {
	expired  bool
	urgency  float64
	deadline time.Time
	load     int
	priority int
	id       string
}

func (s adaptiveScore) betterThan(other adaptiveScore) bool {
	if s.expired != other.expired {
		return s.expired
	}
	if !s.deadline.Equal(other.deadline) {
		if s.deadline.IsZero() {
			return false
		}
		if other.deadline.IsZero() {
			return true
		}
		return s.deadline.Before(other.deadline)
	}
	if math.Abs(s.urgency-other.urgency) > 0.000001 {
		return s.urgency > other.urgency
	}
	if s.priority != other.priority {
		return s.priority > other.priority
	}
	if s.load != other.load {
		return s.load < other.load
	}
	return s.id < other.id
}

func adaptiveScoreFor(auth *Auth, state codexAdaptiveAccount, now time.Time) adaptiveScore {
	weeklyPrefix := codexWeeklyQuotaPrefix(auth.Quota.Signals)
	return adaptiveScore{
		expired:  codexSubscriptionExpired(auth, now),
		urgency:  codexQuotaUrgency(auth, now),
		deadline: codexQuotaDeadline(auth, now, weeklyPrefix),
		load:     state.inFlight,
		priority: authPriority(auth),
		id:       auth.ID,
	}
}

// CodexAdaptiveCandidateInfo is the backend's current, request-independent
// candidate view exposed to the management UI. The request-time selector may
// still differ when model filters or leases change between list and request.
type CodexAdaptiveCandidateInfo struct {
	Candidate        bool       `json:"candidate"`
	Rank             int        `json:"rank,omitempty"`
	BlockedReason    string     `json:"blocked_reason,omitempty"`
	Deadline         *time.Time `json:"deadline,omitempty"`
	QuotaUrgency     float64    `json:"quota_urgency,omitempty"`
	Priority         int        `json:"priority"`
	InFlight         int        `json:"in_flight"`
	ConcurrencyLimit int        `json:"concurrency_limit"`
}

// CodexAdaptiveSnapshot returns the same deadline/load ordering used by the
// adaptive router, based on persisted local state and current lease state.
func (m *Manager) CodexAdaptiveSnapshot() map[string]CodexAdaptiveCandidateInfo {
	if m == nil || !m.codexAdaptiveEnabled() {
		return nil
	}
	m.syncPersistedCodexQuotaSnapshots()

	m.mu.RLock()
	auths := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth != nil && isCodexCredential(auth) {
			auths = append(auths, auth.Clone())
		}
	}
	m.mu.RUnlock()

	now := time.Now()
	info := make(map[string]CodexAdaptiveCandidateInfo, len(auths))
	available := make([]*Auth, 0, len(auths))
	preferFree := codexPreferFreeForSharedModels(m)
	freeAvailable := false
	for _, auth := range auths {
		if !preferFree || !isFreeCodexAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		state := m.schedulerAdaptiveAccount(auth.ID)
		blocked, _, _ := isAuthBlockedForModel(auth, "", now)
		if state.inFlight < state.limit && !blocked && codexQuotaCandidateAvailable(auth, now) {
			freeAvailable = true
			break
		}
	}
	for _, auth := range auths {
		state := m.schedulerAdaptiveAccount(auth.ID)
		entry := CodexAdaptiveCandidateInfo{
			ConcurrencyLimit: state.limit,
			InFlight:         state.inFlight,
			Priority:         authPriority(auth),
		}
		if entry.ConcurrencyLimit <= 0 {
			entry.ConcurrencyLimit = codexAdaptiveDefaultConcurrency
		}
		if auth.Disabled || auth.Status == StatusDisabled {
			entry.BlockedReason = "disabled"
		} else if blocked, _, _ := isAuthBlockedForModel(auth, "", now); blocked {
			entry.BlockedReason = "runtime_cooldown"
		} else if reason := codexQuotaBlockedReason(auth, now); reason != "" {
			entry.BlockedReason = reason
		} else if freeAvailable && !isFreeCodexAuth(auth) {
			entry.BlockedReason = "free_preferred"
		} else if state.inFlight >= entry.ConcurrencyLimit {
			entry.BlockedReason = "concurrency_limit"
		} else {
			entry.Candidate = true
			available = append(available, auth)
		}
		info[auth.ID] = entry
	}
	sort.Slice(available, func(i, j int) bool {
		left := available[i]
		right := available[j]
		return adaptiveScoreFor(left, m.schedulerAdaptiveAccount(left.ID), now).betterThan(
			adaptiveScoreFor(right, m.schedulerAdaptiveAccount(right.ID), now),
		)
	})
	for index, auth := range available {
		entry := info[auth.ID]
		entry.Rank = index + 1
		score := adaptiveScoreFor(auth, m.schedulerAdaptiveAccount(auth.ID), now)
		entry.QuotaUrgency = score.urgency
		if !score.deadline.IsZero() {
			deadline := score.deadline
			entry.Deadline = &deadline
		}
		info[auth.ID] = entry
	}
	return info
}

func codexQuotaBlockedReason(auth *Auth, now time.Time) string {
	if auth == nil {
		return "runtime_cooldown"
	}
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		if !quotaWindowLimited(auth.Quota.Signals, prefix, auth.Quota.ObservedAt, now) {
			continue
		}
		minutes, _ := strconv.Atoi(strings.TrimSpace(auth.Quota.Signals[prefix+"Window-Minutes"]))
		if minutes >= 10080 {
			return "weekly_limit"
		}
		return "five_hour_limit"
	}
	return ""
}

func quotaProbeNeeded(auth *Auth, state *codexAdaptiveAccount, now time.Time) bool {
	if auth == nil || isFreeCodexAuth(auth) {
		return false
	}
	if !state.lastProbeAt.IsZero() && now.Sub(state.lastProbeAt) < codexAdaptiveQuotaProbeInterval {
		return false
	}
	if !state.probeFailedAt.IsZero() && now.Sub(state.probeFailedAt) < codexAdaptiveProbeFailureBackoff {
		return false
	}
	return auth.Quota.ObservedAt.IsZero() || now.Sub(auth.Quota.ObservedAt) >= codexAdaptiveQuotaStaleAfter
}

func codexQuotaUrgency(auth *Auth, now time.Time) float64 {
	if auth == nil || isFreeCodexAuth(auth) {
		return 0
	}
	weeklyPrefix := codexWeeklyQuotaPrefix(auth.Quota.Signals)
	if weeklyPrefix == "" {
		return 0
	}
	remaining := 100 - quotaPercent(auth.Quota.Signals, weeklyPrefix+"Used-Percent")
	if remaining < 0 {
		remaining = 0
	}
	resetCredits := codexAvailableResetCredits(auth)
	if remaining == 0 && resetCredits > 0 {
		// A reset credit is only useful if this account reaches the reset point
		// before the credit expires, so exhausted accounts with credits remain urgent.
		remaining = 1
	}
	remaining *= 1 + resetCredits
	deadline := codexQuotaDeadline(auth, now, weeklyPrefix)
	if deadline.IsZero() {
		return float64(remaining) / 168
	}
	hours := deadline.Sub(now).Hours()
	if hours < 1.0/24.0 {
		hours = 1.0 / 24.0
	}
	return float64(remaining) / hours
}

func codexWeeklyQuotaPrefix(signals map[string]string) string {
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		if minutes, err := strconv.Atoi(strings.TrimSpace(signals[prefix+"Window-Minutes"])); err == nil && minutes >= 10080 {
			return prefix
		}
	}
	// A partial response may omit Window-Minutes, but the secondary Codex
	// window is still the only safe fallback for weekly quota. Never treat a
	// known five-hour primary window as the weekly deadline.
	if signals["X-Codex-Secondary-Used-Percent"] != "" {
		return "X-Codex-Secondary-"
	}
	return ""
}

func codexSubscriptionExpired(auth *Auth, now time.Time) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	for _, key := range []string{"chatgpt_subscription_active_until", "subscription_active_until", "expired", "expires_at", "expires"} {
		if expiry, ok := parseTimeValue(auth.Metadata[key]); ok && !expiry.After(now) {
			return true
		}
	}
	return !codexJWTSubscriptionExpiry(auth.Metadata).After(now) && !codexJWTSubscriptionExpiry(auth.Metadata).IsZero()
}

func codexQuotaDeadline(auth *Auth, now time.Time, weeklyPrefix string) time.Time {
	var deadline time.Time
	if auth == nil {
		return deadline
	}
	addFutureDeadline := func(candidate time.Time) {
		if candidate.After(now) && (deadline.IsZero() || candidate.Before(deadline)) {
			deadline = candidate
		}
	}
	if auth.Metadata != nil {
		for _, key := range []string{
			"chatgpt_subscription_active_until",
			"subscription_active_until",
			"expired",
			"expires_at",
			"expires",
		} {
			if parsed, ok := parseTimeValue(auth.Metadata[key]); ok {
				addFutureDeadline(parsed)
			}
		}
		if parsed := codexJWTSubscriptionExpiry(auth.Metadata); !parsed.IsZero() {
			addFutureDeadline(parsed)
		}
	}
	if resetCreditExpiry := codexResetCreditExpiry(auth, now); !resetCreditExpiry.IsZero() {
		addFutureDeadline(resetCreditExpiry)
	}
	if weeklyPrefix != "" {
		if resetAt := quotaResetAt(auth.Quota.Signals, weeklyPrefix+"Reset-At", weeklyPrefix+"Reset-After-Seconds", auth.Quota.ObservedAt, now); !resetAt.IsZero() {
			addFutureDeadline(resetAt)
		}
	}
	return deadline
}

func codexJWTSubscriptionExpiry(metadata map[string]any) time.Time {
	if len(metadata) == 0 {
		return time.Time{}
	}
	var earliest time.Time
	visit := func(raw any) {}
	visit = func(raw any) {
		switch value := raw.(type) {
		case string:
			claims, err := codexauth.ParseJWTToken(strings.TrimSpace(value))
			if err != nil || claims == nil {
				return
			}
			if expiry, ok := parseTimeValue(claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil); ok &&
				expiry.After(time.Time{}) && (earliest.IsZero() || expiry.Before(earliest)) {
				earliest = expiry
			}
		case map[string]any:
			for _, key := range []string{"id_token", "idToken"} {
				if nested, ok := value[key]; ok {
					visit(nested)
				}
			}
		}
	}
	visit(metadata["id_token"])
	visit(metadata["token"])
	return earliest
}

func codexResetCreditExpiry(auth *Auth, now time.Time) time.Time {
	if auth == nil || auth.Metadata == nil {
		return time.Time{}
	}
	var earliest time.Time
	for _, item := range codexResetCreditItems(auth.Metadata["rate_limit_reset_credits"]) {
		values, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if status, _ := values["status"].(string); status != "" && !strings.EqualFold(strings.TrimSpace(status), "available") {
			continue
		}
		for _, key := range []string{"expires_at", "expiresAt"} {
			if expiry, ok := parseTimeValue(values[key]); ok && expiry.After(now) && (earliest.IsZero() || expiry.Before(earliest)) {
				earliest = expiry
			}
		}
	}
	return earliest
}

func codexAvailableResetCredits(auth *Auth) int {
	if auth == nil {
		return 0
	}
	return codexAvailableResetCreditsFromMetadata(auth.Metadata, time.Now())
}

func codexAvailableResetCreditsFromMetadata(metadata map[string]any, now time.Time) int {
	if len(metadata) == 0 {
		return 0
	}
	count := 0
	for _, key := range []string{"rate_limit_reset_credits_applicable_available_count", "rate_limit_reset_credits_available_count"} {
		if value := quotaMetadataInt(metadata[key]); value > count {
			count = value
		}
	}
	if listed := codexListedAvailableResetCredits(metadata["rate_limit_reset_credits"], now); listed > count {
		count = listed
	}
	if summary := codexResetCreditSummaryCount(metadata["rate_limit_reset_credits"]); summary > count {
		count = summary
	}
	return min(count, 2)
}

func quotaMetadataInt(raw any) int {
	switch value := raw.(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float64:
		return int(value)
	case json.Number:
		parsed, err := strconv.Atoi(value.String())
		if err == nil {
			return parsed
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil {
			return parsed
		}
	}
	return 0
}

func codexResetCreditSummaryCount(raw any) int {
	values, ok := raw.(map[string]any)
	if !ok {
		return 0
	}
	count := 0
	for _, key := range []string{"applicable_available_count", "applicableAvailableCount", "available_count", "availableCount"} {
		if value := quotaMetadataInt(values[key]); value > count {
			count = value
		}
	}
	for _, key := range []string{"credits", "items", "data"} {
		if value := codexResetCreditSummaryCount(values[key]); value > count {
			count = value
		}
	}
	return count
}

func codexListedAvailableResetCredits(raw any, now time.Time) int {
	count := 0
	for _, item := range codexResetCreditItems(raw) {
		values, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if status, _ := values["status"].(string); status != "" && !strings.EqualFold(strings.TrimSpace(status), "available") {
			continue
		}
		valid := true
		for _, key := range []string{"expires_at", "expiresAt"} {
			if expiry, ok := parseTimeValue(values[key]); ok && !expiry.After(now) {
				valid = false
				break
			}
		}
		if valid {
			count++
		}
	}
	return count
}

func codexResetCreditItems(raw any) []any {
	switch value := raw.(type) {
	case []any:
		return value
	case []map[string]any:
		items := make([]any, 0, len(value))
		for _, item := range value {
			items = append(items, item)
		}
		return items
	case map[string]any:
		if _, hasExpiry := value["expires_at"]; hasExpiry {
			return []any{value}
		}
		if _, hasExpiry := value["expiresAt"]; hasExpiry {
			return []any{value}
		}
		for _, key := range []string{"credits", "items", "data"} {
			if items := codexResetCreditItems(value[key]); len(items) > 0 {
				return items
			}
		}
	}
	return nil
}

func metadataString(auth *Auth, keys ...string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	for _, key := range keys {
		value, ok := auth.Metadata[key]
		if !ok || value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func quotaPercent(signals map[string]string, key string) int {
	value, err := strconv.Atoi(strings.TrimSpace(signals[key]))
	if err != nil || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func quotaResetAt(signals map[string]string, atKey, afterKey string, observedAt, now time.Time) time.Time {
	if raw, err := strconv.ParseInt(strings.TrimSpace(signals[atKey]), 10, 64); err == nil && raw > 0 {
		reset := time.Unix(raw, 0)
		if raw >= 1e11 {
			reset = time.UnixMilli(raw)
		}
		if reset.After(now.Add(-14*24*time.Hour)) && reset.Before(now.Add(14*24*time.Hour)) {
			return reset
		}
	}
	if seconds, err := strconv.ParseInt(strings.TrimSpace(signals[afterKey]), 10, 64); err == nil && seconds > 0 && !observedAt.IsZero() {
		reset := observedAt.Add(time.Duration(seconds) * time.Second)
		if reset.After(now.Add(-14*24*time.Hour)) && reset.Before(now.Add(14*24*time.Hour)) {
			return reset
		}
	}
	return time.Time{}
}

func codexQuotaCandidateAvailable(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		if quotaWindowLimited(auth.Quota.Signals, prefix, auth.Quota.ObservedAt, now) {
			return false
		}
	}
	return true
}

func quotaWindowLimited(signals map[string]string, prefix string, observedAt, now time.Time) bool {
	usedRaw, hasUsed := signals[prefix+"Used-Percent"]
	used, errUsed := strconv.Atoi(strings.TrimSpace(usedRaw))
	if hasUsed && errUsed == nil && used >= 100 {
		resetAt := quotaResetAt(signals, prefix+"Reset-At", prefix+"Reset-After-Seconds", observedAt, now)
		if resetAt.IsZero() || resetAt.After(now) {
			return true
		}
	}
	if reached, ok := parseQuotaBool(signals[prefix+"Limit-Reached"]); ok && reached {
		resetAt := quotaResetAt(signals, prefix+"Reset-At", prefix+"Reset-After-Seconds", observedAt, now)
		return resetAt.IsZero() || resetAt.After(now)
	}
	if allowed, ok := parseQuotaBool(signals[prefix+"Allowed"]); ok && !allowed {
		resetAt := quotaResetAt(signals, prefix+"Reset-At", prefix+"Reset-After-Seconds", observedAt, now)
		return resetAt.IsZero() || resetAt.After(now)
	}
	return false
}

func parseQuotaBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes":
		return true, true
	case "false", "0", "no":
		return false, true
	default:
		return false, false
	}
}

func (r *codexAdaptiveRouter) beginProbe(authID string, now time.Time) bool {
	if r == nil || authID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.accountLocked(authID)
	if state.probeInFlight || (!state.lastProbeAt.IsZero() && now.Sub(state.lastProbeAt) < codexAdaptiveQuotaProbeInterval) ||
		(!state.probeFailedAt.IsZero() && now.Sub(state.probeFailedAt) < codexAdaptiveProbeFailureBackoff) {
		return false
	}
	state.probeInFlight = true
	state.lastProbeAt = now
	return true
}

func (r *codexAdaptiveRouter) finishProbe(authID string, success bool) {
	if r == nil || authID == "" {
		return
	}
	r.mu.Lock()
	state := r.accountLocked(authID)
	state.probeInFlight = false
	if success {
		state.lastProbeAt = time.Now()
		state.probeFailedAt = time.Time{}
	} else {
		state.lastProbeAt = time.Time{}
		state.probeFailedAt = time.Now()
	}
	r.notifyLocked()
	r.mu.Unlock()
}

func (r *codexAdaptiveRouter) accountSnapshot(authID string) codexAdaptiveAccount {
	if r == nil {
		return codexAdaptiveAccount{limit: codexAdaptiveDefaultConcurrency}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLeasesLocked(time.Now())
	state := *r.accountLocked(authID)
	return state
}

func (r *codexAdaptiveRouter) release(leaseID string) {
	if r == nil || leaseID == "" {
		return
	}
	r.mu.Lock()
	lease, ok := r.leases[leaseID]
	if ok {
		state := r.accountLocked(lease.authID)
		if state.inFlight > 0 {
			state.inFlight--
		}
		delete(r.leases, leaseID)
		close(lease.done)
		r.notifyLocked()
	}
	r.mu.Unlock()
}

func (r *codexAdaptiveRouter) releaseOptions(options cliproxyexecutor.Options) {
	r.release(adaptiveLeaseID(options))
}

func (r *codexAdaptiveRouter) observe(result Result) {
	if r == nil || result.AuthID == "" || !strings.EqualFold(strings.TrimSpace(result.Provider), "codex") {
		return
	}
	leaseID := adaptiveLeaseID(result.Options)
	if leaseID == "" {
		return
	}
	r.mu.Lock()
	lease, ok := r.leases[leaseID]
	if !ok || lease.authID != result.AuthID {
		r.mu.Unlock()
		return
	}
	state := r.accountLocked(result.AuthID)
	if state.inFlight > 0 {
		state.inFlight--
	}
	delete(r.leases, leaseID)
	close(lease.done)
	status := 0
	if result.Error != nil {
		status = result.Error.HTTPStatus
	}
	if status == http.StatusTooManyRequests {
		state.limit = maxInt(1, state.limit/2)
		state.successes = 0
	} else if result.Success {
		state.successes++
		if state.successes >= codexAdaptiveSuccessesBeforeIncrease && state.limit < codexAdaptiveMaxConcurrency {
			state.limit++
			state.successes = 0
		}
	}
	r.notifyLocked()
	r.mu.Unlock()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
