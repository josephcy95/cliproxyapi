package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRetiredExcelConfigPrunedWithoutChangingOtherSettings(t *testing.T) {
	for _, raw := range []string{
		"port: 9123\napi-keys: [frontend-key]\nexcel-api-key: [{enabled: true}]\ncodex-api-key: [{api-key: codex-key, base-url: https://example.invalid}]\nqoder: {queued-403-cooldown-minutes: 17}\ncustom-setting: {keep: value}\n",
		"config-version: 8\nserver: {port: 9123}\naccess: {api-keys: [frontend-key]}\napi-keys:\n  excel: [{enabled: true}]\n  codex: [{base-url: https://example.invalid, keys: [{api-key: codex-key}]}]\noauth: {providers: {qoder: {queued-403-cooldown-minutes: 17}}}\ncustom-setting: {keep: value}\n",
	} {
		for _, migrate := range []bool{false, true} {
			normalized, changed, err := NormalizeConfigLayout([]byte(raw), migrate)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("retired config not marked changed")
			}
			assertRetiredConfigAbsent(t, normalized)
			cfg, err := ParseConfigBytes(normalized)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Port != 9123 || !reflect.DeepEqual(cfg.APIKeys, []string{"frontend-key"}) || len(cfg.CodexKey) != 1 || cfg.CodexKey[0].APIKey != "codex-key" || cfg.Qoder.QueuedForbiddenCooldownMinutesValue() != 17 {
				t.Fatalf("unrelated settings changed: port=%d api=%v codex=%+v qoder=%+v normalized=%s", cfg.Port, cfg.APIKeys, cfg.CodexKey, cfg.Qoder, normalized)
			}
			if !strings.Contains(string(normalized), "custom-setting") || !strings.Contains(string(normalized), "keep: value") {
				t.Fatal("unrelated unknown YAML discarded")
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Port != cfg.Port || !reflect.DeepEqual(loaded.APIKeys, cfg.APIKeys) || !reflect.DeepEqual(loaded.CodexKey, cfg.CodexKey) || loaded.Qoder.QueuedForbiddenCooldownMinutesValue() != 17 {
				t.Fatal("disk load changed unrelated settings")
			}
			if err := SaveConfigPreserveComments(path, cfg, migrate); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			assertRetiredConfigAbsent(t, saved)
			savedCfg, err := ParseConfigBytes(saved)
			if err != nil {
				t.Fatal(err)
			}
			if savedCfg.Port != cfg.Port || !reflect.DeepEqual(savedCfg.APIKeys, cfg.APIKeys) || !reflect.DeepEqual(savedCfg.CodexKey, cfg.CodexKey) || savedCfg.Qoder.QueuedForbiddenCooldownMinutesValue() != 17 {
				t.Fatal("save changed unrelated settings")
			}
			if !strings.Contains(string(saved), "custom-setting") || !strings.Contains(string(saved), "keep: value") {
				t.Fatal("save discarded unrelated unknown YAML")
			}
		}
	}
	if err := ValidateV8Config([]byte("config-version: 8\nexcel-api-key: [{}]\napi-keys: {excel: [{}]}\n")); err != nil {
		t.Fatalf("retired config should be accepted and ignored: %v", err)
	}
}

func assertRetiredConfigAbsent(t *testing.T, data []byte) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	if yamlPath(root, "excel-api-key") != nil || yamlPath(root, "api-keys.excel") != nil {
		t.Fatalf("retired configuration remains: %s", data)
	}
	if strings.Contains(string(data), "excel-api-key") {
		t.Fatal("retired section retained as comment")
	}
}
