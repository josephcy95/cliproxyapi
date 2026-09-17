package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	var toReschedule []string
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	for id, auth := range m.auths {
		if auth != nil && strings.EqualFold(executorKeyFromAuth(auth), provider) {
			toReschedule = append(toReschedule, id)
		}
	}
	m.mu.Unlock()

	for _, id := range toReschedule {
		m.queueRefreshReschedule(id)
	}

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	NormalizeCredentialMetadata(auth.Metadata)
	hydrateQuotaObservationFromMetadata(auth)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, fmt.Errorf("register auth: %w", errWeight)
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	now := time.Now()
	// Restore credentials that v7.2.109 disabled only because the management
	// quota probe received an inactive-token response. Real execution failures
	// retain their wrapped Qoder API error and stay disabled.
	recoverQoderQuotaProbeDisable(auth, now)
	// Rebuild in-memory cooldown/counters from auth-file runtime block.
	hydrateAuthRuntimeFromMetadata(auth, now)
	cooldownStateChanged := normalizeModelStates(auth)
	if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		cooldownStateChanged = clearCooldownStateForAuth(auth, now) || cooldownStateChanged
	}
	auth.EnsureIndex()
	m.mu.Lock()
	if existing, ok := m.auths[auth.ID]; ok && existing != nil {
		if auth.RegistrationEpoch <= existing.RegistrationEpoch {
			auth.RegistrationEpoch = existing.RegistrationEpoch + 1
		}
	} else if auth.RegistrationEpoch == 0 {
		auth.RegistrationEpoch = 1
	}
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	m.queueRefreshReschedule(auth.ID)
	_ = m.persist(ctx, auth)
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	if cooldownStateChanged {
		m.persistCooldownStates(ctx)
	}
	return auth.Clone(), nil
}

func (m *Manager) rejectStaleAuthUpdate(base, updated *Auth) error {
	if m == nil || updated == nil || updated.ID == "" {
		return nil
	}
	m.mu.RLock()
	existing, ok := m.auths[updated.ID]
	m.mu.RUnlock()
	if !ok || existing == nil {
		return fmt.Errorf("update auth %s: credential removed", updated.ID)
	}
	if base != nil && existing.RegistrationEpoch != base.RegistrationEpoch {
		return fmt.Errorf("update auth %s: stale registration epoch %d != %d", updated.ID, base.RegistrationEpoch, existing.RegistrationEpoch)
	}
	if updated.RegistrationEpoch != 0 && updated.RegistrationEpoch < existing.RegistrationEpoch {
		return fmt.Errorf("update auth %s: stale registration epoch %d < %d", updated.ID, updated.RegistrationEpoch, existing.RegistrationEpoch)
	}
	return nil
}

// UpdatePreparedAuth persists a request-time credential mint using the same
// update path as a normal in-memory update, rejecting obsolete mints.
func (m *Manager) UpdatePreparedAuth(ctx context.Context, base, updated *Auth) (*Auth, error) {
	if err := m.rejectStaleAuthUpdate(base, updated); err != nil {
		return nil, err
	}
	return m.Update(ctx, updated)
}

// UpdateRefreshedAuth persists a refreshed credential using the same update path
// as a normal in-memory update, rejecting obsolete mints.
func (m *Manager) UpdateRefreshedAuth(ctx context.Context, base, updated *Auth) (*Auth, error) {
	if err := m.rejectStaleAuthUpdate(base, updated); err != nil {
		return nil, err
	}
	return m.Update(ctx, updated)
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	NormalizeCredentialMetadata(auth.Metadata)
	hydrateQuotaObservationFromMetadata(auth)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, fmt.Errorf("update auth: %w", errWeight)
	}
	now := time.Now()
	// Watcher updates carry persisted runtime state in metadata. Hydrate it before
	// merging with the current in-memory snapshot so a future cooldown cannot be
	// dropped merely because the file was re-read.
	hydrateAuthRuntimeFromMetadata(auth, now)
	recoverQoderQuotaProbeDisable(auth, now)
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	if !ok || existing == nil {
		m.mu.Unlock()
		return nil, nil
	}
	if !auth.indexAssigned && auth.Index == "" {
		auth.Index = existing.Index
		auth.indexAssigned = existing.indexAssigned
	}
	auth.Success = existing.Success
	auth.Failed = existing.Failed
	auth.recentRequests = existing.recentRequests
	if auth.Generation <= existing.Generation {
		auth.Generation = existing.Generation + 1
	} else {
		auth.Generation++
	}
	replaceRuntime := auth.ReplaceRuntimeState
	cooldownStateChanged := false
	if replaceRuntime {
		ResetAuthRuntimeForRelogin(auth)
	} else {
		credChanged := CredentialsChanged(existing, auth)
		bothActive := !existing.Disabled && existing.Status != StatusDisabled && !auth.Disabled && auth.Status != StatusDisabled
		if bothActive || credChanged {
			// Merge states when both sides are active, or when credentials changed so
			// unauthorized/exhaustion clears can resume models even if previously disabled.
			auth.ModelStates = mergeModelStatesConservatively(existing.ModelStates, auth.ModelStates, now)
		}
		if credChanged {
			// Fresh tokens invalidate prior unauthorized / exhaustion disablement.
			if hasUnauthorizedAuthFailure(existing) || existing.Disabled || existing.Status == StatusDisabled || (auth.LastError != nil && isUnauthorizedError(auth.LastError)) {
				auth.Disabled = false
				auth.Unavailable = false
				auth.LastError = nil
				auth.StatusMessage = ""
				auth.Status = StatusActive
				if auth.Metadata != nil {
					delete(auth.Metadata, "disabled")
					delete(auth.Metadata, "disabled_reason")
				}
			}
			resumed := clearUnauthorizedModelStates(auth, now)
			if len(resumed) > 0 {
				cooldownStateChanged = true
			}
		}
		// Preserve credential_quota across reload/token refresh even when CredentialsChanged
		// (upstream behavior). Rate limits are account-scoped, not token-string-scoped.
		if bothActive {
			if existing.Quota.Exceeded && existing.Quota.Reason == "credential_quota" && existing.Quota.NextRecoverAt.After(now) {
				auth.Unavailable = existing.Unavailable
				auth.NextRetryAfter = existing.NextRetryAfter
				auth.Quota = existing.Quota
				if auth.Status == StatusActive {
					auth.Status = existing.Status
				}
			}
		}
	}
	auth.UpdatedAt = now
	cooldownStateChanged = normalizeModelStates(auth) || cooldownStateChanged
	if replaceRuntime || m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
		cooldownStateChanged = clearCooldownStateForAuth(auth, now) || cooldownStateChanged || replaceRuntime
	}
	auth.EnsureIndex()
	// A minted Meta key must reach the configured store before requests can use it.
	// Keep the epoch check, save and installation together so a concurrent reload
	// or removal cannot let an obsolete mint overwrite the credential on disk.
	persistMetaMint := strings.EqualFold(strings.TrimSpace(auth.Provider), "meta")
	if persistMetaMint {
		if errPersist := m.persist(ctx, auth); errPersist != nil {
			m.mu.Unlock()
			return nil, fmt.Errorf("persist meta auth: %w", errPersist)
		}
	}
	authClone := auth.Clone()
	m.auths[auth.ID] = authClone
	m.mu.Unlock()
	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(authClone)
	}
	m.queueRefreshReschedule(auth.ID)
	if !persistMetaMint {
		_ = m.persist(ctx, auth)
	}
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	if cooldownStateChanged {
		m.persistCooldownStates(ctx)
	}
	return auth.Clone(), nil
}

// Remove deletes an auth from runtime state without persisting.
// Disk and token-store deletion must be handled by the caller.
func (m *Manager) Remove(ctx context.Context, id string) {
	if m == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	_ = ctx

	m.mu.Lock()
	existing := m.auths[id]
	if existing == nil {
		m.mu.Unlock()
		return
	}
	provider := strings.TrimSpace(existing.Provider)
	delete(m.auths, id)
	if m.modelPoolOffsets != nil {
		delete(m.modelPoolOffsets, id)
	}
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		if sessionAuths == nil {
			continue
		}
		delete(sessionAuths, id)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
	m.mu.Unlock()

	if !shouldDeferAPIKeyModelAliasRebuild(ctx) {
		m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	}
	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	m.queueRefreshUnschedule(id)
	m.invalidateSessionAffinity(id)

	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	m.persistCooldownStates(ctx)
}

func (m *Manager) invalidateSessionAffinity(authID string) {
	if m == nil || authID == "" {
		return
	}
	if invalidator, ok := m.selector.(interface{ InvalidateAuth(string) }); ok && invalidator != nil {
		invalidator.InvalidateAuth(authID)
	}
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	if m.store == nil {
		m.mu.Unlock()
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	now := time.Now()
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		NormalizeCredentialMetadata(auth.Metadata)
		hydrateQuotaObservationFromMetadata(auth)
		if errWeight := ValidateAuthWeight(auth); errWeight != nil {
			continue
		}
		hydrateAuthRuntimeFromMetadata(auth, now)
		recoverQoderQuotaProbeDisable(auth, now)
		if m.cooldownDisabledForAuth(auth) || auth.Disabled || auth.Status == StatusDisabled {
			_ = clearCooldownStateForAuth(auth, now)
		}
		auth.EnsureIndex()
		m.auths[auth.ID] = auth.Clone()
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	m.mu.Unlock()
	m.syncScheduler()
	return nil
}

func (m *Manager) persist(ctx context.Context, auth *Auth) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if persisted := readPersistedCodexQuotaMetadata(auth); persisted != nil {
		applyPersistedCodexQuotaMetadata(auth, persisted)
	}
	syncQuotaObservationMetadata(auth)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return fmt.Errorf("persist auth: %w", errWeight)
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if IsConfigAPIKeyAuth(auth) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	if IsPluginVirtualAuth(auth) {
		return nil
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}
