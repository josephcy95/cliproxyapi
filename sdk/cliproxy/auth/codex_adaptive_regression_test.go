package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func formatUnixSeconds(value time.Time) string {
	return strconv.FormatInt(value.Unix(), 10)
}

type adaptiveSnapshotStore struct {
	saved *Auth
}

func (s *adaptiveSnapshotStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *adaptiveSnapshotStore) Delete(context.Context, string) error  { return nil }
func (s *adaptiveSnapshotStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.saved = auth.Clone()
	return "", nil
}

func TestFlattenPersistedCodexQuotaMetadataPrefersNewerNestedSnapshot(t *testing.T) {
	older := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	flat := FlattenPersistedCodexQuotaMetadata(map[string]any{
		"rate_limit_reset_credits_checked_at":      older.Format(time.RFC3339Nano),
		"rate_limit_reset_credits_available_count": 0,
		"metadata": map[string]any{
			"rate_limit_reset_credits_checked_at":      newer.Format(time.RFC3339Nano),
			"rate_limit_reset_credits_available_count": 1,
		},
	})
	if got := quotaMetadataInt(flat["rate_limit_reset_credits_available_count"]); got != 1 {
		t.Fatalf("flattened reset-credit count = %d, want newer nested count 1", got)
	}
	if got := persistedMetadataTime(flat, "rate_limit_reset_credits_checked_at"); !got.Equal(newer) {
		t.Fatalf("flattened reset-credit timestamp = %v, want %v", got, newer)
	}
}

func TestFlattenPersistedCodexQuotaMetadataKeepsNestedResetCreditsWhenTopLevelIsEmpty(t *testing.T) {
	flat := FlattenPersistedCodexQuotaMetadata(map[string]any{
		"rate_limit_reset_credits_available_count": 0,
		"rate_limit_reset_credits":                 []any{},
		"metadata": map[string]any{
			"rate_limit_reset_credits_available_count": 1,
			"rate_limit_reset_credits": map[string]any{
				"credits": []any{map[string]any{
					"status":     "available",
					"expires_at": "2026-09-02T04:00:00Z",
				}},
			},
		},
	})
	if got := quotaMetadataInt(flat["rate_limit_reset_credits_available_count"]); got != 1 {
		t.Fatalf("flattened reset-credit count = %d, want 1", got)
	}
	items := codexResetCreditItems(flat["rate_limit_reset_credits"])
	if len(items) != 1 {
		t.Fatalf("flattened reset-credit list length = %d, want 1", len(items))
	}
}

func TestCodexAdaptiveDerivesResetCreditsFromSummaryObject(t *testing.T) {
	auth := &Auth{Provider: "codex", Metadata: map[string]any{
		"rate_limit_reset_credits": map[string]any{
			"available_count": 1,
		},
	}}
	if got := codexAvailableResetCredits(auth); got != 1 {
		t.Fatalf("summary reset-credit count = %d, want 1", got)
	}
}

func TestCodexAdaptiveDeadlineUsesNestedResetCreditExpiry(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	auth := &Auth{
		ID:       "nested-credit",
		Provider: "codex",
		Metadata: map[string]any{
			"rate_limit_reset_credits": map[string]any{
				"credits": []any{map[string]any{
					"status":     "available",
					"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano),
				}},
			},
		},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":        "50",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "604800",
		}},
	}
	deadline := codexQuotaDeadline(auth, now, "X-Codex-Secondary-")
	want := now.Add(24 * time.Hour)
	if !deadline.Equal(want) {
		t.Fatalf("adaptive deadline = %v, want nested reset-credit expiry %v", deadline, want)
	}
}

func TestCodexAdaptiveDeadlineIgnoresFiveHourResetWhenWeeklyWindowIsMissing(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	expires := now.Add(72 * time.Hour)
	auth := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"chatgpt_subscription_active_until": expires.Format(time.RFC3339Nano),
		},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":        "10",
			"X-Codex-Primary-Window-Minutes":      "300",
			"X-Codex-Primary-Reset-After-Seconds": "3600",
		}},
	}
	deadline := codexQuotaDeadline(auth, now, codexWeeklyQuotaPrefix(auth.Quota.Signals))
	if !deadline.Equal(expires) {
		t.Fatalf("adaptive deadline = %v, want subscription expiry %v", deadline, expires)
	}
}

func TestCodexAdaptiveSkipsPersistedCooldownAndLetsUrgencyBeatPriority(t *testing.T) {
	now := time.Now()
	cooldown := &Auth{
		ID:       "cooldown",
		Provider: "codex",
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":        "100",
			"X-Codex-Primary-Window-Minutes":      "300",
			"X-Codex-Primary-Reset-After-Seconds": "7200",
		}},
	}
	urgent := &Auth{
		ID:       "urgent",
		Provider: "codex",
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":   "80",
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       formatUnixSeconds(now.Add(2*time.Hour + 11*time.Hour)),
		}},
	}
	later := &Auth{
		ID:       "later",
		Provider: "codex",
		Attributes: map[string]string{
			"priority": "100",
		},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":   "80",
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       formatUnixSeconds(now.Add(2*time.Hour + 23*time.Hour)),
		}},
	}

	router := newCodexAdaptiveRouter()
	selected := router.best([]*Auth{cooldown, later, urgent}, false, now)
	if selected == nil || selected.ID != urgent.ID {
		t.Fatalf("adaptive selected %v, want urgent account", selected)
	}
	if codexQuotaCandidateAvailable(cooldown, now) {
		t.Fatal("account at a live five-hour limit remained a candidate")
	}
	if !codexQuotaCandidateAvailable(urgent, now) {
		t.Fatal("available urgent account was rejected")
	}
}

func TestCodexAdaptiveIgnoresExpiredQuotaDeadlines(t *testing.T) {
	now := time.Now()
	auth := &Auth{
		ID:       "expired-deadlines",
		Provider: "codex",
		Metadata: map[string]any{
			"chatgpt_subscription_active_until": now.Add(-time.Hour).Format(time.RFC3339),
			"rate_limit_reset_credits": []any{map[string]any{
				"status":     "available",
				"expires_at": now.Add(-30 * time.Minute).Format(time.RFC3339),
			}},
		},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":   "50",
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       formatUnixSeconds(now.Add(-time.Hour)),
		}},
	}
	if deadline := codexQuotaDeadline(auth, now, "X-Codex-Secondary-"); !deadline.IsZero() {
		t.Fatalf("expired quota deadline = %v, want zero", deadline)
	}
}

func TestCodexAdaptivePersistsResponseQuotaSnapshot(t *testing.T) {
	store := &adaptiveSnapshotStore{}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "persisted-quota",
		Provider: "codex",
		Status:   StatusActive,
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}

	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"X-Codex-Primary-Used-Percent":        []string{"41"},
		"X-Codex-Primary-Window-Minutes":      []string{"300"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"3600"},
	})
	manager.MarkResult(ctx, Result{AuthID: auth.ID, Provider: "codex", Success: true})
	if store.saved == nil {
		t.Fatal("MarkResult did not persist the auth")
	}
	if store.saved.Metadata["X-Codex-Primary-Used-Percent"] != "41" {
		t.Fatalf("persisted quota metadata = %#v", store.saved.Metadata)
	}
	if _, ok := store.saved.Metadata[quotaObservedAtMetadataKey].(string); !ok {
		t.Fatalf("missing persisted quota timestamp: %#v", store.saved.Metadata)
	}
}

func TestCodexAdaptiveHydratesPersistedQuotaSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "hydrated-quota",
		Provider: "codex",
		Metadata: map[string]any{
			"type":                                  "codex",
			"codex_quota_observed_at":               now.Format(time.RFC3339),
			"X-Codex-Secondary-Used-Percent":        "73",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "3600",
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	loaded, ok := manager.GetByID(auth.ID)
	if !ok || loaded == nil {
		t.Fatal("hydrated auth was not registered")
	}
	if loaded.Quota.Signals["X-Codex-Secondary-Used-Percent"] != "73" || !loaded.Quota.ObservedAt.Equal(now) {
		t.Fatalf("hydrated quota = %#v", loaded.Quota)
	}
}

func TestCodexAdaptiveBusyPoolReturnsBoundedUnavailableError(t *testing.T) {
	router := newCodexAdaptiveRouter()
	auth := &Auth{ID: "busy", Provider: "codex"}
	leases := make([]string, 0, codexAdaptiveDefaultConcurrency)
	for i := 0; i < codexAdaptiveDefaultConcurrency; i++ {
		_, lease, errPick := router.pick(context.Background(), []*Auth{auth}, false)
		if errPick != nil {
			t.Fatalf("pick %d: %v", i+1, errPick)
		}
		leases = append(leases, lease)
	}
	defer func() {
		for _, lease := range leases {
			router.release(lease)
		}
	}()

	started := time.Now()
	_, _, errPick := router.pick(context.Background(), []*Auth{auth}, false)
	if errPick == nil {
		t.Fatal("busy adaptive pool unexpectedly selected an account")
	}
	adaptiveErr, ok := errPick.(*Error)
	if !ok || adaptiveErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("busy pool error = %#v, want HTTP 503", errPick)
	}
	if elapsed := time.Since(started); elapsed > codexAdaptiveMaxWait+500*time.Millisecond {
		t.Fatalf("busy pool waited %v, max expected %v", elapsed, codexAdaptiveMaxWait)
	}
}

func TestCodexAdaptiveUsesMetadataPlanTypeWhenAttributesAreMissing(t *testing.T) {
	auth := &Auth{
		ID:       "metadata-free",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "free"},
	}
	if !isFreeCodexAuth(auth) {
		t.Fatal("metadata-only free Codex auth was not recognized")
	}
}

func TestCodexAdaptiveDerivesResetCreditsFromAvailableCreditList(t *testing.T) {
	auth := &Auth{
		ID:       "listed-reset-credit",
		Provider: "codex",
		Metadata: map[string]any{
			"rate_limit_reset_credits_applicable_available_count": 0,
			"rate_limit_reset_credits_available_count":            0,
			"rate_limit_reset_credits": []any{map[string]any{
				"status":     "available",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			}},
		},
	}
	if got := codexAvailableResetCredits(auth); got != 1 {
		t.Fatalf("available reset credits = %d, want 1", got)
	}
}

func TestCodexAdaptiveDeadlineUsesJWTExpiryFallback(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	expires := now.Add(48 * time.Hour)
	payload, errMarshal := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_subscription_active_until": expires.Format(time.RFC3339Nano),
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal JWT payload: %v", errMarshal)
	}
	idToken := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	auth := &Auth{
		ID:       "jwt-expiry",
		Provider: "codex",
		Metadata: map[string]any{"id_token": idToken},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":        "50",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "604800",
		}},
	}
	deadline := codexQuotaDeadline(auth, now, "X-Codex-Secondary-")
	if !deadline.Equal(expires) {
		t.Fatalf("JWT expiry deadline = %v, want %v", deadline, expires)
	}
}

func TestCodexAdaptiveDeadlineUsesPersistedExpiryFallback(t *testing.T) {
	now := time.Now()
	auth := &Auth{
		ID:       "persisted-expiry",
		Provider: "codex",
		Metadata: map[string]any{
			"subscription_active_until": now.Add(2 * time.Hour).Format(time.RFC3339),
		},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":   "50",
			"X-Codex-Secondary-Window-Minutes": "10080",
		}},
	}
	deadline := codexQuotaDeadline(auth, now, "X-Codex-Secondary-")
	if deadline.IsZero() || deadline.Sub(now) > 2*time.Hour+time.Minute {
		t.Fatalf("deadline = %v, want persisted expiry near %v", deadline, now.Add(2*time.Hour))
	}
}

func TestCodexAdaptiveSyncsQuotaFromBackingFile(t *testing.T) {
	path := t.TempDir() + "/codex.json"
	raw, errMarshal := json.Marshal(map[string]any{
		"type":                                     "codex",
		"plan_type":                                "plus",
		"codex_quota_observed_at":                  time.Now().UTC().Format(time.RFC3339Nano),
		"X-Codex-Secondary-Used-Percent":           "73",
		"X-Codex-Secondary-Window-Minutes":         "10080",
		"X-Codex-Secondary-Reset-After-Seconds":    "3600",
		"rate_limit_reset_credits_available_count": 1,
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "backing-file",
		Provider: "codex",
		Attributes: map[string]string{
			AttributePath: path,
		},
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}
	manager.syncPersistedCodexQuotaSnapshots()
	loaded, ok := manager.GetByID(auth.ID)
	if !ok || loaded == nil {
		t.Fatal("backing-file auth was not registered")
	}
	if loaded.Quota.Signals["X-Codex-Secondary-Used-Percent"] != "73" {
		t.Fatalf("backing-file quota = %#v", loaded.Quota)
	}
	if loaded.Metadata["rate_limit_reset_credits_available_count"] != float64(1) {
		t.Fatalf("backing-file reset metadata = %#v", loaded.Metadata)
	}
}

func TestCodexAdaptiveDoesNotReplaceNewerMemoryQuotaWithStaleBackingFile(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := t.TempDir() + "/codex.json"
	stale := now.Add(-time.Hour)
	raw, errMarshal := json.Marshal(map[string]any{
		"type":                                     "codex",
		"codex_quota_observed_at":                  stale.Format(time.RFC3339Nano),
		"X-Codex-Secondary-Used-Percent":           "10",
		"X-Codex-Secondary-Window-Minutes":         "10080",
		"X-Codex-Secondary-Reset-After-Seconds":    "3600",
		"rate_limit_reset_credits_checked_at":      stale.Format(time.RFC3339Nano),
		"rate_limit_reset_credits_available_count": 0,
		"rate_limit_reset_credits":                 []any{},
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "newer-memory",
		Provider: "codex",
		Attributes: map[string]string{
			AttributePath: path,
		},
		Metadata: map[string]any{
			"type":                                "codex",
			"rate_limit_reset_credits_checked_at": now.Format(time.RFC3339Nano),
			"rate_limit_reset_credits_available_count": 1,
			"rate_limit_reset_credits": []any{map[string]any{
				"status":     "available",
				"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339),
			}},
		},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":        "80",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "7200",
		}},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register: %v", errRegister)
	}

	manager.syncPersistedCodexQuotaSnapshots()
	loaded, ok := manager.GetByID(auth.ID)
	if !ok || loaded == nil {
		t.Fatal("auth was not registered")
	}
	if got := loaded.Quota.Signals["X-Codex-Secondary-Used-Percent"]; got != "80" {
		t.Fatalf("stale backing file replaced newer memory quota: got %q", got)
	}
	if got := codexAvailableResetCredits(loaded); got != 1 {
		t.Fatalf("stale backing file replaced newer reset credits: got %d", got)
	}
}

func TestCodexAdaptiveMatchesExpectedUrgentCandidateOrder(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	paid := func(id string, expiresIn time.Duration, used int, resetCredits int) *Auth {
		metadata := map[string]any{
			"chatgpt_subscription_active_until": now.Add(expiresIn).Format(time.RFC3339Nano),
		}
		if resetCredits > 0 {
			metadata["rate_limit_reset_credits_available_count"] = resetCredits
			metadata["rate_limit_reset_credits"] = []any{map[string]any{
				"status":     "available",
				"expires_at": now.Add(expiresIn).Format(time.RFC3339Nano),
			}}
		}
		return &Auth{
			ID:       id,
			Provider: "codex",
			Metadata: metadata,
			Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":        strconv.Itoa(used),
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "604800",
			}},
		}
	}

	cooldown := paid("socialwisp", 58*time.Hour, 20, 1)
	cooldown.Quota.Signals["X-Codex-Primary-Used-Percent"] = "100"
	cooldown.Quota.Signals["X-Codex-Primary-Window-Minutes"] = "300"
	cooldown.Quota.Signals["X-Codex-Primary-Reset-After-Seconds"] = "7200"
	sounder := paid("sounder", 59*time.Hour, 20, 0)
	lido := paid("lido", 71*time.Hour, 20, 0)
	threeDays := paid("three-days", 72*time.Hour, 20, 0)
	fiveDaysWithReset := paid("five-days-reset", 120*time.Hour, 0, 1)

	router := newCodexAdaptiveRouter()
	candidates := []*Auth{fiveDaysWithReset, lido, cooldown, threeDays, sounder}
	selected := router.best(candidates, false, now)
	if selected == nil || selected.ID != sounder.ID {
		t.Fatalf("first adaptive candidate = %v, want %s", selected, sounder.ID)
	}
	selected = router.best([]*Auth{fiveDaysWithReset, lido, cooldown, threeDays}, false, now)
	if selected == nil || selected.ID != lido.ID {
		t.Fatalf("second adaptive candidate = %v, want %s", selected, lido.ID)
	}
	if codexQuotaCandidateAvailable(cooldown, now) {
		t.Fatal("five-hour-limited account remained an adaptive candidate")
	}
}

func TestCodexAdaptivePersistDoesNotOverwriteNewerBackingSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := t.TempDir() + "/codex.json"
	disk, errMarshal := json.Marshal(map[string]any{
		"type":                                     "codex",
		"codex_quota_observed_at":                  now.Format(time.RFC3339Nano),
		"X-Codex-Secondary-Used-Percent":           "80",
		"X-Codex-Secondary-Window-Minutes":         "10080",
		"X-Codex-Secondary-Reset-After-Seconds":    "7200",
		"rate_limit_reset_credits_checked_at":      now.Format(time.RFC3339Nano),
		"rate_limit_reset_credits_available_count": 1,
		"rate_limit_reset_credits": []any{map[string]any{
			"status":     "available",
			"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano),
		}},
	})
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if errWrite := os.WriteFile(path, disk, 0o600); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}

	store := &adaptiveSnapshotStore{}
	manager := NewManager(store, nil, nil)
	stale := now.Add(-time.Hour)
	auth := &Auth{
		ID:       "stale-writer",
		Provider: "codex",
		Attributes: map[string]string{
			AttributePath: path,
		},
		Metadata: map[string]any{"type": "codex"},
		Quota: QuotaState{ObservedAt: stale, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":        "10",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "3600",
		}},
	}
	if errPersist := manager.persist(context.Background(), auth); errPersist != nil {
		t.Fatalf("persist: %v", errPersist)
	}
	if store.saved == nil {
		t.Fatal("persist did not save the auth")
	}
	if got := store.saved.Quota.Signals["X-Codex-Secondary-Used-Percent"]; got != "80" {
		t.Fatalf("persist overwrote newer backing quota: got %q", got)
	}
	if got := codexAvailableResetCredits(store.saved); got != 1 {
		t.Fatalf("persist overwrote newer backing reset credits: got %d", got)
	}
}

func TestApplyPersistedCodexMetadataMergesResetCreditsIndependently(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	auth := &Auth{
		ID:       "independent-reset-merge",
		Provider: "codex",
		Metadata: map[string]any{
			"rate_limit_reset_credits_checked_at":      now.Add(-time.Hour).Format(time.RFC3339Nano),
			"rate_limit_reset_credits_available_count": 0,
			"rate_limit_reset_credits":                 []any{},
		},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":   "80",
			"X-Codex-Secondary-Window-Minutes": "10080",
		}},
	}
	persisted := map[string]any{
		"codex_quota_observed_at":                  now.Format(time.RFC3339Nano),
		"X-Codex-Secondary-Used-Percent":           "10",
		"X-Codex-Secondary-Window-Minutes":         "10080",
		"rate_limit_reset_credits_checked_at":      now.Format(time.RFC3339Nano),
		"rate_limit_reset_credits_available_count": 1,
		"rate_limit_reset_credits": []any{map[string]any{
			"status":     "available",
			"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano),
		}},
	}
	if !applyPersistedCodexQuotaMetadata(auth, persisted) {
		t.Fatal("newer reset-credit snapshot was not merged")
	}
	if got := auth.Quota.Signals["X-Codex-Secondary-Used-Percent"]; got != "80" {
		t.Fatalf("same-timestamp quota snapshot replaced memory value: %q", got)
	}
	if got := codexAvailableResetCredits(auth); got != 1 {
		t.Fatalf("reset-credit count = %d, want 1", got)
	}
}

func TestCodexAdaptiveSnapshotMatchesBackendCandidateOrdering(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	paid := func(id string, expiresIn time.Duration, used int) *Auth {
		return &Auth{
			ID:       id,
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{
				"chatgpt_subscription_active_until": now.Add(expiresIn).Format(time.RFC3339Nano),
			},
			Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":        strconv.Itoa(used),
				"X-Codex-Secondary-Window-Minutes":      "10080",
				"X-Codex-Secondary-Reset-After-Seconds": "604800",
			}},
		}
	}
	cooldown := paid("socialwisp", 58*time.Hour, 10)
	cooldown.Quota.Signals["X-Codex-Primary-Used-Percent"] = "100"
	cooldown.Quota.Signals["X-Codex-Primary-Window-Minutes"] = "300"
	cooldown.Quota.Signals["X-Codex-Primary-Reset-After-Seconds"] = "7200"
	sounder := paid("sounder", 59*time.Hour, 20)
	lido := paid("lido", 71*time.Hour, 20)

	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{
		Routing: internalconfig.CodexRoutingConfig{Strategy: "adaptive"},
	}})
	for _, auth := range []*Auth{cooldown, lido, sounder} {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	snapshot := manager.CodexAdaptiveSnapshot()
	if snapshot[sounder.ID].Rank != 1 || !snapshot[sounder.ID].Candidate {
		t.Fatalf("sounder snapshot = %#v, want rank 1 candidate", snapshot[sounder.ID])
	}
	if snapshot[lido.ID].Rank != 2 || !snapshot[lido.ID].Candidate {
		t.Fatalf("lido snapshot = %#v, want rank 2 candidate", snapshot[lido.ID])
	}
	if snapshot[cooldown.ID].Candidate || snapshot[cooldown.ID].BlockedReason != "five_hour_limit" {
		t.Fatalf("cooldown snapshot = %#v", snapshot[cooldown.ID])
	}
}
