package usagestore

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Plugin persists usage records into a Store when enabled.
type Plugin struct {
	store   atomic.Pointer[Store]
	enabled atomic.Bool
}

// NewPlugin creates a usage store plugin. Store may be set later via SetStore.
func NewPlugin(store *Store) *Plugin {
	p := &Plugin{}
	if store != nil {
		p.store.Store(store)
	}
	p.enabled.Store(true)
	return p
}

// SetStore swaps the active store (e.g. after reconfigure).
func (p *Plugin) SetStore(store *Store) {
	if p == nil {
		return
	}
	p.store.Store(store)
}

// SetEnabled toggles persistence without unregistering the plugin.
func (p *Plugin) SetEnabled(enabled bool) {
	if p == nil {
		return
	}
	p.enabled.Store(enabled)
}

// Enabled reports whether the plugin will persist records.
func (p *Plugin) Enabled() bool {
	return p != nil && p.enabled.Load()
}

// HandleUsage implements coreusage.Plugin.
func (p *Plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !p.enabled.Load() {
		return
	}
	store := p.store.Load()
	if store == nil {
		return
	}

	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	modelName := strings.TrimSpace(record.Model)
	if modelName == "" {
		modelName = "unknown"
	}
	aliasName := strings.TrimSpace(record.Alias)
	if aliasName == "" {
		aliasName = modelName
	}
	provider := strings.TrimSpace(record.Provider)
	if provider == "" {
		provider = "unknown"
	}
	executorType := strings.TrimSpace(record.ExecutorType)
	if executorType == "" {
		executorType = "unknown"
	}
	authType := strings.TrimSpace(record.AuthType)
	if authType == "" {
		authType = "unknown"
	}
	apiKey := strings.TrimSpace(record.APIKey)
	requestID := strings.TrimSpace(internallogging.GetRequestID(ctx))
	reasoningEffort := strings.TrimSpace(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = coreusage.ReasoningEffortFromContext(ctx)
	}
	serviceTier := strings.TrimSpace(record.ServiceTier)
	if serviceTier == "" {
		serviceTier = strings.TrimSpace(record.RequestServiceTier)
	}
	if serviceTier == "" {
		serviceTier = coreusage.ServiceTierFromContext(ctx)
	}
	responseServiceTier := strings.TrimSpace(record.ResponseServiceTier)

	input := record.Detail.InputTokens
	output := record.Detail.OutputTokens
	reasoning := record.Detail.ReasoningTokens
	cached := record.Detail.CachedTokens
	cacheRead := record.Detail.CacheReadTokens
	cacheCreation := record.Detail.CacheCreationTokens
	total := record.Detail.TotalTokens
	if total == 0 {
		total = input + output + reasoning
	}
	if total == 0 {
		total = input + output + reasoning + cached
	}

	failed := record.Failed
	if !failed {
		status := internallogging.GetResponseStatus(ctx)
		if status >= http.StatusBadRequest {
			failed = true
		}
	}
	failStatus := record.Fail.StatusCode
	failSummary := ""
	if failed {
		if failStatus <= 0 {
			failStatus = internallogging.GetResponseStatus(ctx)
		}
		if failStatus <= 0 {
			failStatus = 500
		}
		failSummary = SummarizeFailBody(record.Fail.Body, 240)
	} else {
		failStatus = 200
	}

	sourceRaw := strings.TrimSpace(record.Source)
	var latencyPtr, ttftPtr *int64
	if record.Latency > 0 {
		v := record.Latency.Milliseconds()
		latencyPtr = &v
	}
	if record.TTFT > 0 {
		v := record.TTFT.Milliseconds()
		ttftPtr = &v
	}

	endpoint := strings.TrimSpace(internallogging.GetEndpoint(ctx))

	event := Event{
		TimestampMS:         timestamp.UnixMilli(),
		RequestID:           requestID,
		Provider:            provider,
		ExecutorType:        executorType,
		Model:               modelName,
		Alias:               aliasName,
		Endpoint:            endpoint,
		AuthType:            authType,
		AuthIndex:           strings.TrimSpace(record.AuthIndex),
		Source:              sourceRaw,
		SourceHash:          HashSecret(sourceRaw),
		APIKey:              apiKey,
		APIKeyHash:          HashSecret(apiKey),
		ReasoningEffort:     reasoningEffort,
		ServiceTier:         serviceTier,
		ResponseServiceTier: responseServiceTier,
		InputTokens:         input,
		OutputTokens:        output,
		ReasoningTokens:     reasoning,
		CachedTokens:        cached,
		CacheReadTokens:     cacheRead,
		CacheCreationTokens: cacheCreation,
		TotalTokens:         total,
		LatencyMS:           latencyPtr,
		TTFTMS:              ttftPtr,
		Failed:              failed,
		FailStatusCode:      failStatus,
		FailSummary:         failSummary,
		CreatedAtMS:         time.Now().UnixMilli(),
	}
	store.Enqueue(event)
}
