package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestV8ForkSettingsSurviveMigrationAndSave(t *testing.T) {
	raw := []byte(`port: 9123
usage-store-path: /data/custom-usage.db
usage-retention-days: 37
model-context-overrides:
  - model: custom
    context-length: 456789
qoder:
  auto-disable-inactive-token: false
  queued-403-cooldown-minutes: 17
codex:
  instructions:
    enabled: true
    mode: replace
    content: fork-test
    oauth-only: false
    require-auth-allow: false
    use-prefix-suffix: false
    request-markers: {prefixes: [custom/], suffixes: [/private]}
commandcode-api-key:
  - api-key: test-commandcode
    models: [{name: custom, alias: custom-alias}]
excel-api-key:
  - enabled: true
remote-management:
  panel-github-repository: https://github.com/josephcy95/Cli-Proxy-API-Management-Center
`)
	legacyPath := filepath.Join(t.TempDir(), "legacy.yaml")
	if err := os.WriteFile(legacyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := LoadConfig(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	migrated, _, err := NormalizeConfigLayout(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateV8Config(migrated); err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfigBytes(migrated)
	if err != nil {
		t.Fatal(err)
	}
	assert := func(got *Config) {
		t.Helper()
		if got.UsageStorePath != before.UsageStorePath || got.UsageRetentionDays != before.UsageRetentionDays || !reflect.DeepEqual(got.ModelContextOverrides, before.ModelContextOverrides) || !reflect.DeepEqual(got.Codex.Instructions, before.Codex.Instructions) || !reflect.DeepEqual(got.Qoder, before.Qoder) || !reflect.DeepEqual(got.CommandCodeKey, before.CommandCodeKey) || !reflect.DeepEqual(got.ExcelKey, before.ExcelKey) || got.RemoteManagement.PanelGitHubRepository != before.RemoteManagement.PanelGitHubRepository {
			t.Fatalf("fork settings changed during migration/save: before usage=%q/%d overrides=%+v codex=%+v qoder=%+v command=%+v excel=%+v; after usage=%q/%d overrides=%+v codex=%+v qoder=%+v command=%+v excel=%+v", before.UsageStorePath, before.UsageRetentionDays, before.ModelContextOverrides, before.Codex.Instructions, before.Qoder, before.CommandCodeKey, before.ExcelKey, got.UsageStorePath, got.UsageRetentionDays, got.ModelContextOverrides, got.Codex.Instructions, got.Qoder, got.CommandCodeKey, got.ExcelKey)
		}
	}
	assert(after)
	if !reflect.DeepEqual(after.ForAPIKey().Codex.Instructions, before.Codex.Instructions) {
		t.Fatal("migration discarded explicit API-key private instructions")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, migrated, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		assert(got)
		if err = SaveConfigPreserveComments(path, got); err != nil {
			t.Fatal(err)
		}
	}
}

func TestV8LoadPreservesExplicitValuesAndKeys(t *testing.T) {
	raw := []byte(`config-version: 8
server: {port: 9001}
management: {secret-key: plain-test-secret}
access: {api-keys: [client-key]}
routing: {retry: {request-retry: 0}, cooldown: {disable-cooling: false}}
api-keys:
  codex:
    - name: custom
      base-url: https://example.invalid
      keys: [{api-key: upstream-key}]
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != 9001 || cfg.RequestRetry != 0 || cfg.DisableCooling || !reflect.DeepEqual(cfg.APIKeys, []string{"client-key"}) || len(cfg.CodexKey) != 1 || cfg.CodexKey[0].APIKey != "upstream-key" || !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
			t.Fatal("load changed v8 values")
		}
	}
}
