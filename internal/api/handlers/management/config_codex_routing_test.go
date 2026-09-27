package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPutCodexRoutingConfigPreservesOtherCodexSettings(t *testing.T) {
	autoDisable := true
	cfg := &config.Config{Codex: config.CodexConfig{
		IdentityConfuse:         true,
		AutoDisableAuthFailures: &autoDisable,
	}}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(
		http.MethodPut,
		"/v0/management/codex-routing-config",
		strings.NewReader(`{"strategy":"adaptive","prefer-free-for-shared-models":true}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutCodexRoutingConfig(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cfg.Codex.Routing.Strategy != "adaptive" {
		t.Fatalf("strategy = %q, want adaptive", cfg.Codex.Routing.Strategy)
	}
	if !cfg.Codex.Routing.PreferFreeForSharedModels {
		t.Fatal("prefer-free-for-shared-models = false, want true")
	}
	if !cfg.Codex.IdentityConfuse || cfg.Codex.AutoDisableAuthFailures == nil || !*cfg.Codex.AutoDisableAuthFailures {
		t.Fatal("updating Codex routing replaced unrelated Codex settings")
	}
}

func TestPutCodexRoutingConfigRejectsUnknownStrategy(t *testing.T) {
	cfg := &config.Config{}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/codex-routing-config", strings.NewReader(`{"strategy":"round-robin"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutCodexRoutingConfig(ctx)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
