package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LoadConfig must not materialize missing defaults into the on-disk file. The v8
// configuration API treats GET and failed writes as read-only, so a plain load may
// only clean conflicting legacy fields, never inject keys the operator did not set.
func TestLoadConfigOptional_DoesNotWriteUnsetDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	legacy := "" +
		"port: 9000\n" +
		"debug: true\n" +
		"api-keys:\n" +
		"  - \"my-real-key\"\n" +
		"request-retry: 1\n"
	if err := os.WriteFile(configPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("expected config")
	}

	if cfg.Port != 9000 {
		t.Fatalf("port = %d, want 9000", cfg.Port)
	}
	if !cfg.Debug {
		t.Fatal("debug should remain true")
	}
	if cfg.RequestRetry != 1 {
		t.Fatalf("request-retry = %d, want 1", cfg.RequestRetry)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "my-real-key" {
		t.Fatalf("api-keys = %#v, want [my-real-key]", cfg.APIKeys)
	}

	// Non-zero in-memory defaults still apply when a key is absent.
	if cfg.ErrorLogsMaxFiles != 10 {
		t.Fatalf("error-logs-max-files = %d, want 10", cfg.ErrorLogsMaxFiles)
	}
	if !cfg.WebsocketAuth {
		t.Fatal("expected ws-auth=true default")
	}
	if cfg.XAI.FreeUsageExhaustedCooldownHoursValue() != 24 {
		t.Fatalf("xai free-usage cooldown = %d, want 24", cfg.XAI.FreeUsageExhaustedCooldownHoursValue())
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) != legacy {
		t.Fatalf("load rewrote an unchanged config file:\n%s", data)
	}
}

func TestLoadConfigOptional_ExplicitFalsePreserved(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := "" +
		"port: 8317\n" +
		"ws-auth: false\n" +
		"request-retry: 0\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.WebsocketAuth {
		t.Fatal("explicit ws-auth:false must be preserved")
	}
	if cfg.RequestRetry != 0 {
		t.Fatalf("explicit request-retry:0 must be preserved, got %d", cfg.RequestRetry)
	}
}

// Fork-owned product defaults (xAI/Qoder failure policy, panel repository) are still
// applied in memory when the corresponding block is absent, unlike the global retry,
// routing, and quota values that presence alone decides.
func TestParseConfigBytes_UsesForkDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.RequestRetry != 0 {
		t.Fatalf("request-retry = %d, want 0 (presence-decided)", cfg.RequestRetry)
	}
	if cfg.MaxRetryInterval != 0 {
		t.Fatalf("max-retry-interval = %d, want 0 (presence-decided)", cfg.MaxRetryInterval)
	}
	if !cfg.WebsocketAuth {
		t.Fatal("expected ws-auth=true default")
	}
	if cfg.XAI.OtherForbiddenCooldownHoursValue() != 6 {
		t.Fatalf("xai other-403 cooldown = %d, want 6", cfg.XAI.OtherForbiddenCooldownHoursValue())
	}
	if cfg.Qoder.QueuedForbiddenCooldownMinutesValue() != 5 {
		t.Fatalf("qoder queued-403 cooldown = %d, want 5", cfg.Qoder.QueuedForbiddenCooldownMinutesValue())
	}
	if cfg.RemoteManagement.PanelGitHubRepository != DefaultPanelGitHubRepository {
		t.Fatalf("panel repository = %q, want %q", cfg.RemoteManagement.PanelGitHubRepository, DefaultPanelGitHubRepository)
	}
	if !strings.Contains(cfg.RemoteManagement.PanelGitHubRepository, "github.com/josephcy95/") {
		t.Fatalf("panel repository must default to the fork, got %q", cfg.RemoteManagement.PanelGitHubRepository)
	}
	// Fork single data root: an omitted auth-dir resolves under CLIPROXY_DATA_DIR.
	if cfg.AuthDir != DefaultForkAuthDir {
		t.Fatalf("auth-dir = %q, want %q", cfg.AuthDir, DefaultForkAuthDir)
	}
}

func TestLoadConfigOptional_OptionalCloudDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	legacy := "port: 8317\n"
	if err := os.WriteFile(configPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, _ := os.ReadFile(configPath)
	cfg, err := LoadConfigOptional(configPath, true)
	if err != nil {
		t.Fatalf("LoadConfigOptional optional: %v", err)
	}
	after, _ := os.ReadFile(configPath)
	if string(before) != string(after) {
		t.Fatalf("optional/cloud load must not rewrite config file")
	}
	// In-memory defaults still apply.
	if cfg == nil {
		t.Fatal("expected config")
	}
	if cfg.Port != 8317 {
		t.Fatalf("port = %d, want 8317", cfg.Port)
	}
	if cfg.RemoteManagement.PanelGitHubRepository != DefaultPanelGitHubRepository {
		t.Fatalf("panel repository = %q, want %q", cfg.RemoteManagement.PanelGitHubRepository, DefaultPanelGitHubRepository)
	}
}
