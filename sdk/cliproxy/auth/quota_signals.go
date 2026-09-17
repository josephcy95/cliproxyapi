package auth

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxQuotaSignalHeaders      = 64
	quotaObservedAtMetadataKey = "codex_quota_observed_at"
	maxQuotaSignalValue        = 512
)

// ProviderSupportsQuotaObservation reports whether the named provider emits a
// passive credential-level quota snapshot understood by collectQuotaSignals.
func ProviderSupportsQuotaObservation(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "codex", "devin":
		return true
	default:
		return false
	}
}

// ObserveResponseHeadersForProvider records the passive quota signals carried by
// the current upstream response. Codex rate-limit windows are merged by window
// namespace because some responses expose only one window; transient watermarks
// such as Retry-After are still replaced on every observation. Responses that
// carry no quota signal at all leave the previous snapshot untouched.
//
// This function only ever touches ObservedAt and Signals. Cooldown and
// scheduling fields are never read or written here.
func (q *QuotaState) ObserveResponseHeadersForProvider(provider string, headers http.Header, observedAt time.Time) bool {
	if q == nil {
		return false
	}
	if !ProviderSupportsQuotaObservation(provider) {
		return q.ClearObservationSignals()
	}
	next := collectQuotaSignals(provider, headers)
	if len(next) == 0 {
		return false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if strings.EqualFold(strings.TrimSpace(provider), "codex") && len(q.Signals) > 0 {
		// Codex responses can expose only part of a rate-limit window. Preserve
		// omitted fields (especially reset metadata) and replace only values that
		// the current response actually observed. Retry-After is transient and is
		// intentionally removed before applying the new response.
		merged := make(map[string]string, len(q.Signals)+len(next))
		for key, value := range q.Signals {
			if !strings.EqualFold(key, "Retry-After") {
				merged[key] = value
			}
		}
		for key, value := range next {
			merged[key] = value
		}
		next = merged
	}

	q.Signals = next
	q.ObservedAt = observedAt
	return true
}

// ClearObservationSignals removes only passive observation data. It leaves
// cooldown and scheduler state untouched.
func (q *QuotaState) ClearObservationSignals() bool {
	if q == nil || (len(q.Signals) == 0 && q.ObservedAt.IsZero()) {
		return false
	}
	q.Signals = nil
	q.ObservedAt = time.Time{}
	return true
}

// cooldownFieldsOf copies only scheduler cooldown fields. Observation data is
// omitted so cooldown persistence and restore cannot replace a live snapshot.
func cooldownFieldsOf(q QuotaState) QuotaState {
	return QuotaState{
		Exceeded:      q.Exceeded,
		Reason:        q.Reason,
		NextRecoverAt: q.NextRecoverAt,
		BackoffLevel:  q.BackoffLevel,
	}
}

// applyCooldownFields writes only scheduler cooldown fields. ObservedAt and
// Signals are left untouched so a cooldown transition cannot erase the last
// upstream watermark.
func applyCooldownFields(dst *QuotaState, cooldown QuotaState) {
	if dst == nil {
		return
	}
	dst.Exceeded = cooldown.Exceeded
	dst.Reason = cooldown.Reason
	dst.NextRecoverAt = cooldown.NextRecoverAt
	dst.BackoffLevel = cooldown.BackoffLevel
}

// collectQuotaSignals builds the bounded snapshot for a single response.
func isCodexQuotaWindowSignal(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	return strings.HasPrefix(lower, "x-codex-") &&
		(strings.Contains(lower, "-primary-") || strings.Contains(lower, "-secondary-"))
}

func quotaSignalNamespace(key string) string {
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{"-primary-", "-secondary-"} {
		if index := strings.Index(lower, marker); index >= 0 {
			return lower[:index+len(marker)-1]
		}
	}
	return lower
}

func collectQuotaSignals(provider string, headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	values := make(map[string]string, len(headers))
	for key, headerValues := range headers {
		canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if !isQuotaSignalHeaderForProvider(provider, canonicalKey) || len(headerValues) == 0 {
			continue
		}
		value := strings.TrimSpace(headerValues[len(headerValues)-1])
		if !validQuotaSignalValue(value) {
			continue
		}
		if _, exists := values[canonicalKey]; !exists {
			names = append(names, canonicalKey)
		}
		values[canonicalKey] = value
	}
	if len(names) == 0 {
		return nil
	}
	// Rank then sort so truncation is deterministic and keeps credential-level
	// watermarks (plan, credits, primary) ahead of additional-limit namespaces.
	sort.Slice(names, func(i, j int) bool {
		ri, rj := quotaSignalRetentionRank(names[i]), quotaSignalRetentionRank(names[j])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	if len(names) > maxQuotaSignalHeaders {
		names = names[:maxQuotaSignalHeaders]
	}
	signals := make(map[string]string, len(names))
	for _, name := range names {
		signals[name] = values[name]
	}
	return signals
}

// validQuotaSignalValue rejects empty, oversized, and control-character values.
// Observed values reach the plain-text upstream request log, so a value
// containing CR/LF could otherwise forge a header line there.
func validQuotaSignalValue(value string) bool {
	if value == "" || len(value) > maxQuotaSignalValue {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func quotaSignalRetentionRank(name string) int {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case lower == "retry-after", strings.HasPrefix(lower, "anthropic-ratelimit-unified-"):
		return 0
	case lower == "x-codex-plan-type", lower == "x-codex-active-limit", strings.HasPrefix(lower, "x-codex-credits-"):
		return 1
	case lower == "x-codex-allowed", lower == "x-codex-limit-reached",
		strings.HasPrefix(lower, "x-codex-primary-"), strings.HasPrefix(lower, "x-codex-secondary-"):
		return 2
	case strings.HasPrefix(lower, "x-codex-code-review-"):
		return 3
	case strings.HasPrefix(lower, "x-codex-additional-"):
		return 5
	case strings.HasPrefix(lower, "x-codex-"):
		return 4
	default:
		return 6
	}
}

func isQuotaSignalHeaderForProvider(provider, name string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "retry-after" {
		return provider == "claude" || provider == "codex"
	}
	if strings.HasPrefix(name, "anthropic-ratelimit-unified-") {
		return provider == "claude"
	}
	if strings.HasPrefix(name, "x-ratelimit-") {
		// Observed Codex responses do not carry x-ratelimit-* headers; the only
		// upstream seen emitting them is Grok, which is excluded from quota
		// observation. The rule is kept so a future Codex rollout is captured
		// without another change, but it is expected to be inert today.
		return provider == "codex"
	}
	if !strings.HasPrefix(name, "x-codex-") {
		return false
	}
	if provider != "codex" {
		return false
	}
	if name == "x-codex-active-limit" || name == "x-codex-plan-type" ||
		strings.HasPrefix(name, "x-codex-credits-") {
		return true
	}
	// Codex namespaces each additional limit by a short name on the HTTP path
	// (x-codex-bengalfox-primary-used-percent) and by limit name on the
	// websocket path (x-codex-additional-<limit>-primary-used-percent), so the
	// suffix is matched instead of an exhaustive header list.
	for _, marker := range []string{
		"-allowed",
		"-limit-reached",
		"-limit-name",
		"-used-percent",
		"-window-minutes",
		"-reset-after-seconds",
		"-reset-at",
		"-over-secondary-limit-percent",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

var persistedCodexPlanMetadataKeys = []string{
	"plan_type",
	"chatgpt_plan_type",
	"plan_checked_at",
	"chatgpt_subscription_active_until",
	"subscription_active_until",
	"expired",
	"expires_at",
	"expires",
}

var persistedCodexResetCreditMetadataKeys = []string{
	"rate_limit_reset_credits_available_count",
	"rate_limit_reset_credits_applicable_available_count",
	"rate_limit_reset_credits",
	"rate_limit_reset_credits_checked_at",
}

// FlattenPersistedCodexQuotaMetadata normalizes the supported auth-file layouts
// into one flat snapshot. Older files store these fields at the top level,
// newer records may place them in metadata or quota.signals.
func FlattenPersistedCodexQuotaMetadata(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}
	type persistedSource struct {
		values map[string]any
		order  int
	}
	sources := make([]persistedSource, 0, 5)
	addSource := func(values map[string]any) {
		if len(values) == 0 {
			return
		}
		normalized := make(map[string]any, len(values))
		for key, value := range values {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(key), "x-codex-") {
				key = http.CanonicalHeaderKey(key)
			}
			normalized[key] = value
		}
		if len(normalized) > 0 {
			sources = append(sources, persistedSource{values: normalized, order: len(sources)})
		}
	}
	flattenQuota := func(quota map[string]any) map[string]any {
		values := make(map[string]any, len(quota))
		for key, value := range quota {
			switch key {
			case "signals":
				if signals, ok := value.(map[string]any); ok {
					for signalKey, signalValue := range signals {
						values[http.CanonicalHeaderKey(strings.TrimSpace(signalKey))] = signalValue
					}
				}
			case "observed_at":
				values[quotaObservedAtMetadataKey] = value
			default:
				values[key] = value
			}
		}
		return values
	}
	if quota, ok := raw["quota"].(map[string]any); ok {
		addSource(flattenQuota(quota))
	}
	if metadata, ok := raw["metadata"].(map[string]any); ok {
		values := make(map[string]any, len(metadata))
		for key, value := range metadata {
			if key != "quota" {
				values[key] = value
			}
		}
		addSource(values)
		if quota, ok := metadata["quota"].(map[string]any); ok {
			addSource(flattenQuota(quota))
		}
	}
	values := make(map[string]any, len(raw))
	for key, value := range raw {
		if key != "metadata" && key != "quota" {
			values[key] = value
		}
	}
	addSource(values)

	flat := make(map[string]any)
	for _, source := range sources {
		for key, value := range source.values {
			flat[key] = value
		}
	}

	mergeLatestCategory := func(keys []string, timestampKey string, fallbackTimestampKeys ...string) {
		keySet := make(map[string]struct{}, len(keys))
		for _, key := range keys {
			keySet[key] = struct{}{}
			delete(flat, key)
		}
		var selected *persistedSource
		var selectedAt time.Time
		for index := range sources {
			source := &sources[index]
			hasCategory := false
			for key := range keySet {
				if _, ok := source.values[key]; ok {
					hasCategory = true
					break
				}
			}
			if !hasCategory {
				continue
			}
			at := persistedMetadataTime(source.values, timestampKey, fallbackTimestampKeys...)
			preferSource := selected == nil
			if selected != nil && !at.IsZero() {
				preferSource = selectedAt.IsZero() || at.After(selectedAt) || (at.Equal(selectedAt) && source.order > selected.order)
			}
			// Older auth files did not write a per-category timestamp. If the
			// reset-credit data is split across layouts, prefer the source that
			// still contains usable credits instead of an empty stale list.
			if selected != nil && timestampKey == "rate_limit_reset_credits_checked_at" && at.IsZero() && selectedAt.IsZero() {
				sourceScore := persistedResetCreditSourceScore(source.values)
				selectedScore := persistedResetCreditSourceScore(selected.values)
				preferSource = sourceScore > selectedScore || (sourceScore == selectedScore && source.order > selected.order)
			}
			if preferSource {
				selected = source
				selectedAt = at
			}
		}
		if selected == nil {
			return
		}
		for key := range keySet {
			if value, ok := selected.values[key]; ok {
				flat[key] = value
			}
		}
	}

	quotaKeys := []string{quotaObservedAtMetadataKey}
	seenQuotaKeys := map[string]struct{}{quotaObservedAtMetadataKey: {}}
	for _, source := range sources {
		for key := range source.values {
			if strings.HasPrefix(strings.ToLower(key), "x-codex-") && isQuotaSignalHeaderForProvider("codex", key) {
				if _, seen := seenQuotaKeys[key]; !seen {
					seenQuotaKeys[key] = struct{}{}
					quotaKeys = append(quotaKeys, key)
				}
			}
		}
	}
	mergeLatestCategory(quotaKeys, quotaObservedAtMetadataKey)
	mergeLatestCategory(persistedCodexResetCreditMetadataKeys, "rate_limit_reset_credits_checked_at")
	mergeLatestCategory(persistedCodexPlanMetadataKeys, "plan_checked_at", "rate_limit_reset_credits_checked_at", quotaObservedAtMetadataKey)
	return flat
}

func readPersistedCodexQuotaMetadata(auth *Auth) map[string]any {
	if auth == nil || auth.Attributes == nil {
		return nil
	}
	path := strings.TrimSpace(auth.Attributes[AttributePath])
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
	flat := FlattenPersistedCodexQuotaMetadata(raw)
	if len(flat) == 0 {
		return flat
	}
	// Older auth files predate the explicit observation timestamps. Use the
	// file mtime as a local sync watermark so another CPA instance's persisted
	// quota is noticed without making a remote quota request.
	if info, errStat := os.Stat(path); errStat == nil && !info.ModTime().IsZero() {
		fileTime := info.ModTime().UTC().Format(time.RFC3339Nano)
		for _, key := range []string{quotaObservedAtMetadataKey, "rate_limit_reset_credits_checked_at", "plan_checked_at"} {
			if _, exists := persistedMetadataValue(flat, key); !exists {
				flat[key] = fileTime
			}
		}
	}
	return flat
}

func persistedResetCreditSourceScore(values map[string]any) int {
	if values == nil {
		return 0
	}
	score := 0
	for _, key := range []string{
		"rate_limit_reset_credits_available_count",
		"rate_limit_reset_credits_applicable_available_count",
	} {
		if count := quotaMetadataInt(values[key]); count > 0 {
			score += count * 2
		}
	}
	if raw := values["rate_limit_reset_credits"]; raw != nil {
		score += len(codexResetCreditItems(raw)) * 2
		score += codexResetCreditSummaryCount(raw)
	}
	return score
}

func persistedMetadataValue(metadata map[string]any, key string) (any, bool) {
	if metadata == nil {
		return nil, false
	}
	if value, ok := metadata[key]; ok {
		return value, true
	}
	for candidate, value := range metadata {
		if strings.EqualFold(strings.TrimSpace(candidate), key) {
			return value, true
		}
	}
	return nil, false
}

func persistedMetadataHasAny(metadata map[string]any, keys []string) bool {
	for _, key := range keys {
		if _, ok := persistedMetadataValue(metadata, key); ok {
			return true
		}
	}
	return false
}

func persistedMetadataTime(metadata map[string]any, primary string, fallbacks ...string) time.Time {
	if value, ok := persistedMetadataValue(metadata, primary); ok {
		if parsed, ok := parseTimeValue(value); ok {
			return parsed
		}
	}
	for _, key := range fallbacks {
		if value, ok := persistedMetadataValue(metadata, key); ok {
			if parsed, ok := parseTimeValue(value); ok {
				return parsed
			}
		}
	}
	return time.Time{}
}

func mergePersistedMetadataCategory(dst, persisted map[string]any, keys []string, timestampKey string, fallbackTimestampKeys ...string) bool {
	if dst == nil || !persistedMetadataHasAny(persisted, keys) {
		return false
	}
	persistedAt := persistedMetadataTime(persisted, timestampKey, fallbackTimestampKeys...)
	currentAt := persistedMetadataTime(dst, timestampKey, fallbackTimestampKeys...)
	if !persistedAt.IsZero() {
		if !currentAt.IsZero() && !persistedAt.After(currentAt) {
			return false
		}
	} else if !currentAt.IsZero() {
		return false
	}

	changed := false
	for _, key := range keys {
		value, ok := persistedMetadataValue(persisted, key)
		if !ok {
			continue
		}
		current, exists := persistedMetadataValue(dst, key)
		if !exists || !reflect.DeepEqual(current, value) {
			changed = true
		}
		dst[key] = value
	}
	return changed
}

func persistedCodexQuotaSignals(metadata map[string]any) map[string]any {
	signals := make(map[string]any)
	for key, value := range metadata {
		canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if strings.HasPrefix(canonicalKey, "X-Codex-") && isQuotaSignalHeaderForProvider("codex", canonicalKey) {
			signals[canonicalKey] = value
		}
	}
	return signals
}

func shouldApplyPersistedCodexQuota(auth *Auth, persisted map[string]any, signals map[string]any) bool {
	if len(signals) == 0 {
		return false
	}
	persistedAt := persistedCodexQuotaObservedAt(persisted)
	currentAt := auth.Quota.ObservedAt
	if currentAt.IsZero() {
		currentAt = persistedCodexQuotaObservedAt(auth.Metadata)
	}
	if persistedAt.IsZero() {
		return currentAt.IsZero() && len(auth.Quota.Signals) == 0
	}
	return currentAt.IsZero() || persistedAt.After(currentAt)
}

func applyPersistedCodexQuotaMetadata(auth *Auth, persisted map[string]any) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || persisted == nil {
		return false
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}

	changed := false
	quotaSignals := persistedCodexQuotaSignals(persisted)
	quotaChanged := shouldApplyPersistedCodexQuota(auth, persisted, quotaSignals)
	if quotaChanged {
		for key := range auth.Metadata {
			canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
			if strings.HasPrefix(canonicalKey, "X-Codex-") && isQuotaSignalHeaderForProvider("codex", canonicalKey) {
				delete(auth.Metadata, key)
			}
		}
		for key, value := range quotaSignals {
			auth.Metadata[key] = value
		}
		if persistedAt := persistedCodexQuotaObservedAt(persisted); !persistedAt.IsZero() {
			auth.Metadata[quotaObservedAtMetadataKey] = persistedAt.UTC().Format(time.RFC3339Nano)
		}
		auth.Quota.ObservedAt = time.Time{}
		auth.Quota.Signals = nil
		hydrateQuotaObservationFromMetadata(auth)
		changed = true
	}
	if mergePersistedMetadataCategory(auth.Metadata, persisted, persistedCodexResetCreditMetadataKeys, "rate_limit_reset_credits_checked_at") {
		changed = true
	}
	if mergePersistedMetadataCategory(auth.Metadata, persisted, persistedCodexPlanMetadataKeys, "plan_checked_at", "rate_limit_reset_credits_checked_at", quotaObservedAtMetadataKey) {
		changed = true
	}
	if changed {
		if planType := metadataString(auth, "plan_type", "chatgpt_plan_type"); planType != "" {
			if auth.Attributes == nil {
				auth.Attributes = make(map[string]string)
			}
			auth.Attributes["plan_type"] = planType
		}
	}
	return changed
}

func persistedCodexQuotaObservedAt(metadata map[string]any) time.Time {
	return persistedMetadataTime(metadata, quotaObservedAtMetadataKey)
}

// syncPersistedCodexQuotaSnapshots refreshes only local JSON metadata; it does
// not call ChatGPT. The short gate coalesces concurrent requests while still
// noticing another CPA instance's persisted quota changes quickly.
func (m *Manager) syncPersistedCodexQuotaSnapshots() {
	if m == nil {
		return
	}
	m.codexSnapshotSyncMu.Lock()
	defer m.codexSnapshotSyncMu.Unlock()

	m.mu.RLock()
	candidates := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			candidates = append(candidates, auth.Clone())
		}
	}
	m.mu.RUnlock()

	for _, candidate := range candidates {
		persisted := readPersistedCodexQuotaMetadata(candidate)
		if persisted == nil {
			continue
		}
		m.mu.Lock()
		current := m.auths[candidate.ID]
		updated := current != nil && applyPersistedCodexQuotaMetadata(current, persisted)
		if updated {
			candidate = current.Clone()
		}
		m.mu.Unlock()
		if updated && m.scheduler != nil {
			m.scheduler.upsertAuth(candidate)
		}
	}
}

// hydrateQuotaObservationFromMetadata restores the last persisted Codex quota
// snapshot from the auth JSON. Runtime cooldown state remains separate.
func hydrateQuotaObservationFromMetadata(auth *Auth) {
	if auth == nil || auth.Metadata == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	observedAt, okObserved := parseTimeValue(auth.Metadata[quotaObservedAtMetadataKey])
	if !okObserved || observedAt.IsZero() {
		observedAt = auth.UpdatedAt
	}
	if observedAt.IsZero() {
		return
	}

	signals := make(map[string]string)
	for key, raw := range auth.Metadata {
		canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if !strings.HasPrefix(canonicalKey, "X-Codex-") || !isQuotaSignalHeaderForProvider("codex", canonicalKey) {
			continue
		}
		value := quotaMetadataValueString(raw)
		if !validQuotaSignalValue(value) {
			continue
		}
		signals[canonicalKey] = value
	}
	if len(signals) == 0 {
		return
	}
	auth.Quota = mergeQuotaObservation(auth.Quota, QuotaState{ObservedAt: observedAt, Signals: signals})
}

// syncQuotaObservationMetadata makes the auth JSON the durable source for the
// latest Codex quota snapshot used by both adaptive routing and management UI.
func syncQuotaObservationMetadata(auth *Auth) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	for key := range auth.Metadata {
		if strings.HasPrefix(http.CanonicalHeaderKey(strings.TrimSpace(key)), "X-Codex-") {
			delete(auth.Metadata, key)
		}
	}
	for key, value := range auth.Quota.Signals {
		canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if strings.HasPrefix(canonicalKey, "X-Codex-") && isQuotaSignalHeaderForProvider("codex", canonicalKey) && validQuotaSignalValue(value) {
			auth.Metadata[canonicalKey] = value
		}
	}
	if auth.Quota.ObservedAt.IsZero() || len(auth.Quota.Signals) == 0 {
		delete(auth.Metadata, quotaObservedAtMetadataKey)
		return
	}
	auth.Metadata[quotaObservedAtMetadataKey] = auth.Quota.ObservedAt.UTC().Format(time.RFC3339Nano)
}

func quotaMetadataValueString(raw any) string {
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return strings.TrimSpace(value.String())
	case int:
		return strconv.Itoa(value)
	case int32:
		return strconv.FormatInt(int64(value), 10)
	case int64:
		return strconv.FormatInt(value, 10)
	case uint:
		return strconv.FormatUint(uint64(value), 10)
	case uint32:
		return strconv.FormatUint(uint64(value), 10)
	case uint64:
		return strconv.FormatUint(value, 10)
	case float32:
		return strconv.FormatFloat(float64(value), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	default:
		return ""
	}
}

// mergeQuotaObservation keeps the newest observation snapshot instead of
// unioning signals captured at different times, so merging an older snapshot
// can never resurrect a stale watermark.
func mergeQuotaObservation(target, source QuotaState) QuotaState {
	if source.ObservedAt.IsZero() || source.ObservedAt.Before(target.ObservedAt) {
		return target
	}
	target.ObservedAt = source.ObservedAt
	target.Signals = source.Clone().Signals
	return target
}
