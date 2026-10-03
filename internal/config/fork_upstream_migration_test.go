package config

import (
	"strings"
	"testing"
)

func TestHistoricalV8XAISettingsKeepForkFailurePolicy(t *testing.T) {
	raw := []byte(`config-version: 8
oauth:
  providers:
    xai:
      inject-x-search: true
      auto-disable-permission-denied: false
      other-403-cooldown-hours: 9
      free-usage-exhausted-cooldown-hours: 30
      free-usage-exhausted-disable-after: 8
      other-403-disable-after: 7
`)
	before, err := ParseConfigBytes(raw)
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
	if strings.Contains(string(migrated), "# oauth.providers.xai.") {
		t.Fatalf("fork policy archived as unknown: %s", migrated)
	}
	after, err := ParseConfigBytes(migrated)
	if err != nil {
		t.Fatal(err)
	}
	if !after.XAI.InjectXSearch || *after.XAI.AutoDisablePermissionDenied || *after.XAI.OtherForbiddenCooldownHours != 9 || *after.XAI.FreeUsageExhaustedCooldownHours != 30 || *after.XAI.FreeUsageExhaustedDisableAfter != 8 || *after.XAI.OtherForbiddenDisableAfter != 7 {
		t.Fatalf("lost historical failure policy: %+v", after.XAI)
	}
	if *before.XAI.OtherForbiddenDisableAfter != *after.XAI.OtherForbiddenDisableAfter {
		t.Fatal("runtime differs across migration")
	}
}
