package config

import (
	"os"
	"path/filepath"
	"testing"
)

// save-cooldown-status is a live setting mapped to routing.cooldown.save-cooldown-status.
// Load must preserve the operator's value in the file rather than pruning it.
func TestLoadConfigOptional_KeepsSaveCooldownStatusKey(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	raw := "port: 8317\nsave-cooldown-status: true\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional: %v", err)
	}
	if !cfg.SaveCooldownStatus {
		t.Fatal("expected save-cooldown-status:true to reach the runtime config")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(data) != raw {
		t.Fatalf("load rewrote the config file:\n%s", data)
	}
}

func TestParseConfigBytes_ReadsSaveCooldownStatus(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\nsave-cooldown-status: false\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if cfg.Port != 8317 {
		t.Fatalf("port = %d, want 8317", cfg.Port)
	}
	if cfg.SaveCooldownStatus {
		t.Fatal("explicit save-cooldown-status:false must be preserved")
	}
}
