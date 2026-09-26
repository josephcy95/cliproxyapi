package registry

import "testing"

func TestAvailableModelsKeepsHealthyProviderDuringCredentialQuota(t *testing.T) {
	r := newTestModelRegistry()
	model := &ModelInfo{ID: "gpt-5.6-luna", OwnedBy: "openai", Type: "openai"}
	r.RegisterClient("healthy-api-provider", "codex", []*ModelInfo{model})
	r.RegisterClient("exhausted-oauth-account", "codex", []*ModelInfo{model})
	r.SetModelQuotaExceeded("exhausted-oauth-account", model.ID)
	r.SuspendClientModel("exhausted-oauth-account", model.ID, "credential_quota")
	if got := r.GetAvailableModels("openai"); len(got) != 1 {
		t.Errorf("/v1/models hid a model with a healthy API provider: got %v", got)
	}
	if got := r.GetAvailableModelsByProvider("codex"); len(got) != 1 {
		t.Errorf("provider catalog hid a model with a healthy API provider: got %v", got)
	}
	if got := r.GetAvailableModelInfos(); len(got) != 1 {
		t.Errorf("metadata catalog hid a model with a healthy API provider: got %v", got)
	}
	if got := r.GetModelCount(model.ID); got != 1 {
		t.Errorf("healthy API provider count = %d, want 1", got)
	}
	r.SuspendClientModel("healthy-api-provider", model.ID, "manual")
	if got := r.GetAvailableModels("openai"); len(got) != 1 {
		t.Errorf("fork discovery must remain stable while routing excludes unavailable providers: %v", got)
	}
	if got := r.GetAvailableModelsByProvider("codex"); len(got) != 1 {
		t.Errorf("fork provider catalog must preserve discovery while runtime routing blocks unavailable providers: %v", got)
	}
	if got := r.GetModelCount(model.ID); got != 0 {
		t.Errorf("unavailable provider count = %d, want 0", got)
	}
}
