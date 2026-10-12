package handlers

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

func TestRetiredModelAliasesAreUnavailableWithoutRerouting(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	const authID = "retired-model-resolution"
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-6-astra"}, {ID: "gpt-5.6-sol"}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	for _, model := range []string{"gpt-6-astra-excel", "gpt-6-sol-excel", "gpt-6-luna-excel", "gpt-5.6-luna-excel", "gpt-5.6-terra-excel", "gpt-5.6-sol-excel", "gpt-6-astra-excel(high)", "codex/gpt-6-astra-excel"} {
		providers, _, errMsg := h.getRequestDetails(model)
		if len(providers) != 0 || errMsg == nil || errMsg.StatusCode != http.StatusBadRequest || gjson.Get(errMsg.Error.Error(), "error.code").String() != "model_not_found" {
			t.Fatalf("retired alias %s routed: providers=%v error=%+v", model, providers, errMsg)
		}
	}
	providers, model, errMsg := h.getRequestDetails("gpt-6-astra")
	if errMsg != nil || len(providers) != 1 || providers[0] != "codex" || model != "gpt-6-astra" {
		t.Fatalf("ordinary model changed: %v %s %+v", providers, model, errMsg)
	}
}
