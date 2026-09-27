package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestListAuthFiles_IncludesStoredCodexQuotaSnapshot(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "codex-reset.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Quota: coreauth.QuotaState{
			ObservedAt: time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC),
			Signals: map[string]string{
				"X-Codex-Secondary-Used-Percent":   "63",
				"X-Codex-Secondary-Window-Minutes": "10080",
			},
		},
		Metadata: map[string]any{
			"type":                              "codex",
			"chatgpt_subscription_active_until": "2026-09-12T08:00:00Z",
			"rate_limit_reset_credits_available_count":            json.Number("1"),
			"rate_limit_reset_credits_applicable_available_count": 1,
			"rate_limit_reset_credits_checked_at":                 "2026-08-29T12:00:00Z",
			"rate_limit_reset_credits": []any{
				map[string]any{
					"id":         "credit-1",
					"status":     "available",
					"granted_at": "2026-08-01T00:00:00Z",
					"expires_at": "2026-09-01T04:00:00Z",
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	entry := firstAuthFileEntry(t, h)
	if got := entry["X-Codex-Secondary-Used-Percent"]; got != "63" {
		t.Fatalf("unexpected quota used percent %#v", got)
	}
	if _, ok := entry["codex_quota_observed_at"]; !ok {
		t.Fatal("expected codex quota observed timestamp")
	}
	if got := entry["chatgpt_subscription_active_until"]; got != "2026-09-12T08:00:00Z" {
		t.Fatalf("unexpected chatgpt_subscription_active_until %#v", got)
	}
	if got := jsonNumberValue(t, entry["rate_limit_reset_credits_available_count"]); got != 1 {
		t.Fatalf("unexpected available count %#v", entry["rate_limit_reset_credits_available_count"])
	}
	if got := jsonNumberValue(t, entry["rate_limit_reset_credits_applicable_available_count"]); got != 1 {
		t.Fatalf("unexpected applicable count %#v", entry["rate_limit_reset_credits_applicable_available_count"])
	}
	if got := entry["rate_limit_reset_credits_checked_at"]; got != "2026-08-29T12:00:00Z" {
		t.Fatalf("unexpected checked_at %#v", got)
	}
	credits, ok := entry["rate_limit_reset_credits"].([]any)
	if !ok || len(credits) != 1 {
		t.Fatalf("unexpected credits %#v", entry["rate_limit_reset_credits"])
	}
	credit, ok := credits[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected credit entry %#v", credits[0])
	}
	if credit["expires_at"] != "2026-09-01T04:00:00Z" {
		t.Fatalf("unexpected credit expiry %#v", credit["expires_at"])
	}
}

func TestListAuthFiles_StoredZeroResetCreditsAreProjected(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "codex-zero.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{
			"type": "codex",
			"rate_limit_reset_credits_available_count": 0,
			"rate_limit_reset_credits":                 []any{},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	entry := firstAuthFileEntry(t, h)
	if got := jsonNumberValue(t, entry["rate_limit_reset_credits_available_count"]); got != 0 {
		t.Fatalf("expected persisted zero count, got %#v", entry["rate_limit_reset_credits_available_count"])
	}
	credits, ok := entry["rate_limit_reset_credits"].([]any)
	if !ok {
		t.Fatalf("expected empty credits slice, got %#v", entry["rate_limit_reset_credits"])
	}
	if len(credits) != 0 {
		t.Fatalf("expected no credits, got %#v", credits)
	}
}

func jsonNumberValue(t *testing.T, value any) float64 {
	t.Helper()
	switch parsed := value.(type) {
	case float64:
		return parsed
	case int:
		return float64(parsed)
	case int64:
		return float64(parsed)
	case json.Number:
		number, errParse := parsed.Float64()
		if errParse != nil {
			t.Fatalf("invalid json number %#v: %v", value, errParse)
		}
		return number
	default:
		t.Fatalf("expected number, got %#v", value)
		return 0
	}
}

func TestListAuthFiles_UsesBackingCodexSnapshotAsDurableSource(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	now := time.Now().UTC().Truncate(time.Second)
	stale := now.Add(-time.Hour)
	authDir := t.TempDir()
	fileName := "codex-newer-memory.json"
	filePath := filepath.Join(authDir, fileName)
	disk, errMarshal := json.Marshal(map[string]any{
		"type":                                     "codex",
		"codex_quota_observed_at":                  stale.Format(time.RFC3339Nano),
		"X-Codex-Secondary-Used-Percent":           "10",
		"X-Codex-Secondary-Window-Minutes":         "10080",
		"rate_limit_reset_credits_checked_at":      stale.Format(time.RFC3339Nano),
		"rate_limit_reset_credits_available_count": 0,
		"rate_limit_reset_credits":                 []any{},
	})
	if errMarshal != nil {
		t.Fatalf("marshal disk auth: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filePath, disk, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Quota: coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":   "80",
			"X-Codex-Secondary-Window-Minutes": "10080",
		}},
		Metadata: map[string]any{
			"type":                                "codex",
			"rate_limit_reset_credits_checked_at": now.Format(time.RFC3339Nano),
			"rate_limit_reset_credits_available_count": 1,
			"rate_limit_reset_credits": []any{map[string]any{
				"status":     "available",
				"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano),
			}},
		},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}
	entry := firstAuthFileEntry(t, h)

	if got := entry["X-Codex-Secondary-Used-Percent"]; got != "10" {
		t.Fatalf("management view did not read backing quota: %#v", got)
	}
	if got := jsonNumberValue(t, entry["rate_limit_reset_credits_available_count"]); got != 0 {
		t.Fatalf("management view did not read backing reset count: %#v", got)
	}
	credits, ok := entry["rate_limit_reset_credits"].([]any)
	if !ok || len(credits) != 0 {
		t.Fatalf("management view did not read backing reset list: %#v", entry["rate_limit_reset_credits"])
	}
}

func TestListAuthFiles_ReadsNestedCodexSnapshotFromBackingFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	now := time.Now().UTC().Truncate(time.Second)
	authDir := t.TempDir()
	fileName := "codex-nested.json"
	filePath := filepath.Join(authDir, fileName)
	disk, errMarshal := json.Marshal(map[string]any{
		"type": "codex",
		"quota": map[string]any{
			"observed_at": now.Format(time.RFC3339Nano),
			"signals": map[string]any{
				"X-Codex-Primary-Used-Percent":        "100",
				"X-Codex-Primary-Window-Minutes":      "300",
				"X-Codex-Primary-Reset-After-Seconds": "3600",
			},
		},
		"metadata": map[string]any{
			"rate_limit_reset_credits_checked_at":      now.Format(time.RFC3339Nano),
			"rate_limit_reset_credits_available_count": 1,
			"rate_limit_reset_credits": []any{map[string]any{
				"status":     "available",
				"expires_at": now.Add(24 * time.Hour).Format(time.RFC3339Nano),
			}},
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal disk auth: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filePath, disk, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}
	entry := firstAuthFileEntry(t, h)
	if got := entry["X-Codex-Primary-Used-Percent"]; got != "100" {
		t.Fatalf("nested quota signal = %#v", got)
	}
	if got := jsonNumberValue(t, entry["rate_limit_reset_credits_available_count"]); got != 1 {
		t.Fatalf("nested reset count = %#v", got)
	}
	credits, ok := entry["rate_limit_reset_credits"].([]any)
	if !ok || len(credits) != 1 {
		t.Fatalf("nested reset credits = %#v", entry["rate_limit_reset_credits"])
	}
}

func TestListAuthFiles_ReadsCodexResetCreditSummaryObject(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	fileName := "codex-summary.json"
	filePath := filepath.Join(authDir, fileName)
	disk, errMarshal := json.Marshal(map[string]any{
		"type": "codex",
		"rate_limit_reset_credits": map[string]any{
			"available_count":            1,
			"applicable_available_count": 1,
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal disk auth: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filePath, disk, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filePath,
		},
	}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), record); errRegister != nil {
		t.Fatalf("register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}
	entry := firstAuthFileEntry(t, h)
	if got := jsonNumberValue(t, entry["rate_limit_reset_credits_available_count"]); got != 1 {
		t.Fatalf("summary available count = %#v", got)
	}
	if got := jsonNumberValue(t, entry["rate_limit_reset_credits_applicable_available_count"]); got != 1 {
		t.Fatalf("summary applicable count = %#v", got)
	}
	if _, ok := entry["rate_limit_reset_credits"].(map[string]any); !ok {
		t.Fatalf("summary credits = %#v", entry["rate_limit_reset_credits"])
	}
}
