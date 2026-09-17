package auth

import (
	"testing"
	"time"
)

func TestMergeExistingAuthMetadataKeepsUserSettingsAndDropsQuota(t *testing.T) {
	target := &Auth{
		Provider: "codex",
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "acct-new",
		},
	}
	MergeExistingAuthMetadata(target, map[string]any{
		"access_token":                      "old-access",
		"prefix":                            "keep-me",
		"note":                              "important",
		"weight":                            float64(5),
		"disabled":                          true,
		"disabled_reason":                   "usage_limit_reached",
		"X-Codex-Primary-Used-Percent":      "100",
		"codex_quota_observed_at":           time.Now().UTC().Format(time.RFC3339Nano),
		"rate_limit_reset_credits":          []any{map[string]any{"status": "available"}},
		"runtime":                           map[string]any{"models": map[string]any{}},
		"next_retry_after":                  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
		"chatgpt_subscription_active_until": "2026-01-01T00:00:00Z",
	})

	if target.Metadata["prefix"] != "keep-me" {
		t.Fatalf("prefix = %#v, want keep-me", target.Metadata["prefix"])
	}
	if target.Metadata["note"] != "important" {
		t.Fatalf("note = %#v, want important", target.Metadata["note"])
	}
	if target.Metadata["weight"] != float64(5) {
		t.Fatalf("weight = %#v, want 5", target.Metadata["weight"])
	}
	if _, ok := target.Metadata["access_token"]; ok {
		t.Fatal("access_token was preserved")
	}
	for _, key := range []string{
		"disabled",
		"disabled_reason",
		"X-Codex-Primary-Used-Percent",
		"codex_quota_observed_at",
		"rate_limit_reset_credits",
		"runtime",
		"next_retry_after",
		"chatgpt_subscription_active_until",
	} {
		if _, ok := target.Metadata[key]; ok {
			t.Fatalf("%s was preserved: %#v", key, target.Metadata[key])
		}
	}
}

func TestMergeExistingAuthMetadataPreservesDisabledState(t *testing.T) {
	target := &Auth{Metadata: map[string]any{"type": "claude"}}
	MergeExistingAuthMetadata(target, map[string]any{"disabled": true, "prefix": "team"})

	if !target.Disabled {
		t.Fatal("Disabled = false, want true")
	}
	// Fork treats disabled as ephemeral metadata during generic merges (re-login
	// drops quota/disablement chrome). Claude legacy migration restores it via
	// RestoreOperatorDisablementFromMetadata after ResetAuthRuntimeForRelogin.
	if _, ok := target.Metadata["disabled"]; ok {
		t.Fatalf("metadata disabled preserved = %#v, want omitted (ephemeral)", target.Metadata["disabled"])
	}
	if target.Metadata["prefix"] != "team" {
		t.Fatalf("prefix = %v, want team", target.Metadata["prefix"])
	}
}

func TestMergeExistingAuthMetadataKeepsExplicitDisabledState(t *testing.T) {
	target := &Auth{Metadata: map[string]any{"disabled": false}}
	MergeExistingAuthMetadata(target, map[string]any{"disabled": true})

	if target.Disabled {
		t.Fatal("Disabled = true, want explicit false state")
	}
	if disabled, _ := target.Metadata["disabled"].(bool); disabled {
		t.Fatalf("metadata disabled = %v, want false", target.Metadata["disabled"])
	}
}

type dummyMetadataStorage struct {
	meta map[string]any
}

func TestResetAuthRuntimeForReloginClearsCooldownAndQuota(t *testing.T) {
	until := time.Now().Add(2 * time.Hour)
	auth := &Auth{
		Disabled:       true,
		Unavailable:    true,
		Status:         StatusError,
		StatusMessage:  "usage_limit_reached",
		NextRetryAfter: until,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "credential_quota",
			NextRecoverAt: until,
			Signals:       map[string]string{"X-Codex-Primary-Used-Percent": "100"},
		},
		ModelStates: map[string]*ModelState{
			"gpt-5": {Unavailable: true, NextRetryAfter: until},
		},
		Metadata: map[string]any{
			"prefix":                       "keep-me",
			"disabled":                     true,
			"disabled_reason":              "auth failed",
			"X-Codex-Primary-Used-Percent": "100",
			"runtime":                      map[string]any{"models": map[string]any{}},
		},
	}

	ResetAuthRuntimeForRelogin(auth)

	if !auth.ReplaceRuntimeState {
		t.Fatal("ReplaceRuntimeState = false")
	}
	if auth.Disabled || auth.Unavailable || auth.Status != StatusActive || auth.StatusMessage != "" {
		t.Fatalf("runtime status was not reset: disabled=%v unavailable=%v status=%q message=%q", auth.Disabled, auth.Unavailable, auth.Status, auth.StatusMessage)
	}
	if !auth.NextRetryAfter.IsZero() || auth.Quota.Exceeded || len(auth.Quota.Signals) != 0 || auth.ModelStates != nil {
		t.Fatalf("quota/cooldown was not reset: retry=%v quota=%+v models=%v", auth.NextRetryAfter, auth.Quota, auth.ModelStates)
	}
	if auth.Metadata["prefix"] != "keep-me" {
		t.Fatalf("prefix = %#v, want keep-me", auth.Metadata["prefix"])
	}
	if auth.Metadata["disabled"] != false {
		t.Fatalf("disabled metadata = %#v, want false", auth.Metadata["disabled"])
	}
	if _, ok := auth.Metadata["disabled_reason"]; ok {
		t.Fatal("disabled_reason was preserved")
	}
	if _, ok := auth.Metadata["X-Codex-Primary-Used-Percent"]; ok {
		t.Fatal("quota signal was preserved")
	}
}

func TestMergeExistingAuthMetadataMetaDoesNotRestoreOldKey(t *testing.T) {
	// A new device login can succeed while API key minting fails.
	// Its DCA credential must not inherit the previous login's API key.
	auth := &Auth{Provider: "meta", Metadata: map[string]any{"access_token": "dca:new", "dca_token": "dca:new"}}
	MergeExistingAuthMetadata(auth, map[string]any{"api_key": "LLM|old", "dca_expired": "old expiry", "dca_expires_at": 42, "priority": 3})
	for _, key := range []string{"api_key", "dca_expired", "dca_expires_at"} {
		if _, exists := auth.Metadata[key]; exists {
			t.Fatalf("restored old Meta credential field %s", key)
		}
	}
	if auth.Metadata["priority"] != 3 || auth.Metadata["dca_token"] != "dca:new" {
		t.Fatalf("incorrect merged metadata: %#v", auth.Metadata)
	}
}
