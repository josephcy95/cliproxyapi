package registry

import (
	"testing"
	"time"
)

// A credential-wide 429 marks the model that received it "quota" and the credential's
// other models "credential_quota". With no other credential to serve them, all of them
// must stay listed like any quota cooldown instead of leaving only the model that
// received the 429 until a later request re-projects the credential.
func TestCredentialQuotaKeepsSoleCredentialModelsListed(t *testing.T) {
	r := newTestModelRegistry()
	models := []*ModelInfo{
		{ID: "claude-sonnet-5", OwnedBy: "anthropic", Type: "claude"},
		{ID: "claude-opus-5-5", OwnedBy: "anthropic", Type: "claude"},
		{ID: "claude-haiku-4-5", OwnedBy: "anthropic", Type: "claude"},
	}
	r.RegisterClient("claude-oauth", "claude", models)
	_, epoch := r.GetModelsAndEpochForClient("claude-oauth")
	if !r.ApplyClientModelProjections("claude-oauth", epoch, 1, []ClientModelProjection{
		{ModelID: "claude-sonnet-5", Suspended: true, SuspendReason: "quota", QuotaExceeded: true},
		{ModelID: "claude-opus-5-5", Suspended: true, SuspendReason: "credential_quota", QuotaExceeded: true},
		// A sibling without a recorded state of its own inherits the credential's suspension.
		{ModelID: "claude-haiku-4-5", Suspended: true, SuspendReason: "credential_quota"},
	}) {
		t.Fatal("failed to apply the credential-wide quota state")
	}
	if got := len(r.GetAvailableModels("claude")); got != len(models) {
		t.Errorf("/v1/models lists %d models during a credential-wide quota, want %d", got, len(models))
	}
	if got := len(r.GetAvailableModelsByProvider("claude")); got != len(models) {
		t.Errorf("provider catalog lists %d models during a credential-wide quota, want %d", got, len(models))
	}
	if got := len(r.GetAvailableModelInfos()); got != len(models) {
		t.Errorf("metadata catalog lists %d models during a credential-wide quota, want %d", got, len(models))
	}

	// After the quota window, the suspension alone still keeps each model listed.
	registration := r.models["claude-opus-5-5"]
	past := time.Now().Add(-2 * modelQuotaExceededWindow)
	registration.QuotaExceededClients["claude-oauth"] = &past
	if available, _ := modelRegistrationAvailability(registration, time.Now()); !available {
		t.Error("a credential-wide quota hid the model after the quota window")
	}
	if got := len(r.GetAvailableModelsByProvider("claude")); got != len(models) {
		t.Errorf("provider catalog lists %d models after the quota window, want %d", got, len(models))
	}

	// Non-quota suspensions block routing but the fork keeps registered models discoverable.
	_, epoch = r.GetModelsAndEpochForClient("claude-oauth")
	if !r.ApplyClientModelProjections("claude-oauth", epoch, 2, []ClientModelProjection{
		{ModelID: "claude-opus-5-5", Suspended: true, SuspendReason: "unauthorized"},
	}) {
		t.Fatal("failed to apply the unauthorized state")
	}
	if got := len(r.GetAvailableModelsByProvider("claude")); got != len(models) {
		t.Errorf("provider catalog hid a registered unauthorized model: got %d, want %d", got, len(models))
	}
	if available, _ := modelRegistrationAvailability(r.models["claude-opus-5-5"], time.Now()); available {
		t.Error("an unauthorized-only registration is available for routing")
	}
	if got := r.GetModelCount("claude-opus-5-5"); got != 0 {
		t.Errorf("unauthorized model has %d routable clients, want 0", got)
	}

	// When a second credential is suspended for a non-quota reason, a sibling model
	// with only credential_quota (no active 5-minute quota timestamp) remains discoverable.
	r.RegisterClient("claude-api-backup", "claude", []*ModelInfo{
		{ID: "claude-haiku-4-5", OwnedBy: "anthropic", Type: "claude"},
	})
	_, backupEpoch := r.GetModelsAndEpochForClient("claude-api-backup")
	if !r.ApplyClientModelProjections("claude-api-backup", backupEpoch, 1, []ClientModelProjection{
		{ModelID: "claude-haiku-4-5", Suspended: true, SuspendReason: "manual"},
	}) {
		t.Fatal("failed to apply the manual suspension to backup client")
	}
	if got := len(r.GetAvailableModelsByProvider("claude")); got != len(models) {
		t.Errorf("provider catalog hid a registered suspended model: got %d, want %d", got, len(models))
	}
	if got := len(r.GetAvailableModels("claude")); got != len(models) {
		t.Errorf("/v1/models hid a registered suspended model: got %d, want %d", got, len(models))
	}
	if available, _ := modelRegistrationAvailability(r.models["claude-haiku-4-5"], time.Now()); available {
		t.Error("a quota/manual-suspended registration is available for routing")
	}
	if got := r.GetModelCount("claude-haiku-4-5"); got != 0 {
		t.Errorf("quota/manual-suspended model has %d routable clients, want 0", got)
	}
}
