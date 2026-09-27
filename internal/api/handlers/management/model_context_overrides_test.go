package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func newContextOverrideHandler(t *testing.T, overrides []config.ModelContextOverride) *Handler {
	t.Helper()
	return &Handler{
		cfg:            &config.Config{ModelContextOverrides: overrides},
		configFilePath: writeTestConfigFile(t),
	}
}

func TestPutModelContextOverridesReplacesList(t *testing.T) {
	h := newContextOverrideHandler(t, []config.ModelContextOverride{{Model: "old", ContextLength: 1}})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	body := `[{"model":"custom-a","context-length":262144,"max-completion-tokens":8192}]`
	ctx.Request = httptest.NewRequest(http.MethodPut, "/model-context-overrides", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutModelContextOverrides(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ModelContextOverrides) != 1 {
		t.Fatalf("override count = %d, want 1", len(h.cfg.ModelContextOverrides))
	}
	got := h.cfg.ModelContextOverrides[0]
	if got.Model != "custom-a" || got.ContextLength != 262144 || got.MaxCompletionTokens != 8192 {
		t.Fatalf("override = %+v", got)
	}
}

func TestPutModelContextOverridesRejectsNegativeValues(t *testing.T) {
	h := newContextOverrideHandler(t, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	body := `[{"model":"custom-a","context-length":-5}]`
	ctx.Request = httptest.NewRequest(http.MethodPut, "/model-context-overrides", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.PutModelContextOverrides(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ModelContextOverrides) != 0 {
		t.Fatalf("config should be untouched, got %+v", h.cfg.ModelContextOverrides)
	}
}

func TestPatchModelContextOverrideUpsertsAndClears(t *testing.T) {
	h := newContextOverrideHandler(t, nil)

	patch := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/model-context-overrides", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchModelContextOverride(ctx)
		return rec
	}

	if rec := patch(`{"model":"Custom-B","context-length":131072}`); rec.Code != http.StatusOK {
		t.Fatalf("insert status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ModelContextOverrides) != 1 || h.cfg.ModelContextOverrides[0].ContextLength != 131072 {
		t.Fatalf("after insert = %+v", h.cfg.ModelContextOverrides)
	}

	// Case-insensitive match must update the existing entry, not append a new one.
	if rec := patch(`{"model":"custom-b","context-length":65536}`); rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ModelContextOverrides) != 1 || h.cfg.ModelContextOverrides[0].ContextLength != 65536 {
		t.Fatalf("after update = %+v", h.cfg.ModelContextOverrides)
	}

	// Clearing both values removes the entry.
	if rec := patch(`{"model":"custom-b","context-length":0,"max-completion-tokens":0}`); rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ModelContextOverrides) != 0 {
		t.Fatalf("after clear = %+v", h.cfg.ModelContextOverrides)
	}
}

func TestDeleteModelContextOverrideRemovesEntry(t *testing.T) {
	h := newContextOverrideHandler(t, []config.ModelContextOverride{
		{Model: "keep-me", ContextLength: 4096},
		{Model: "drop-me", ContextLength: 8192},
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/model-context-overrides?model=DROP-ME", nil)

	h.DeleteModelContextOverride(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ModelContextOverrides) != 1 || h.cfg.ModelContextOverrides[0].Model != "keep-me" {
		t.Fatalf("remaining = %+v", h.cfg.ModelContextOverrides)
	}
}

func TestDeleteModelContextOverrideRequiresModel(t *testing.T) {
	h := newContextOverrideHandler(t, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/model-context-overrides", nil)

	h.DeleteModelContextOverride(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestGetModelContextStatusReportsMissingCount(t *testing.T) {
	h := newContextOverrideHandler(t, []config.ModelContextOverride{{Model: "custom-a", ContextLength: 4096}})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/model-context-status", nil)

	h.GetModelContextStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Models              []modelContextEntry        `json:"models"`
		MissingContextCount int                        `json:"missing-context-count"`
		Overrides           []modelContextOverrideItem `json:"model-context-overrides"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(payload.Overrides) != 1 || payload.Overrides[0].Model != "custom-a" {
		t.Fatalf("overrides = %+v", payload.Overrides)
	}
}

// Regression: deleting the last override must be written to disk. With an
// omitempty yaml tag (or a nil slice) the emptied list is skipped entirely and
// SaveConfigPreserveComments leaves the stale entry in the file.
func TestDeleteLastModelContextOverridePersistsEmptyList(t *testing.T) {
	path := writeTestConfigFile(t)
	if errSeed := os.WriteFile(path, []byte("model-context-overrides:\n  - model: custom-a\n    context-length: 262144\n"), 0o600); errSeed != nil {
		t.Fatalf("seed config: %v", errSeed)
	}
	h := &Handler{
		cfg:            &config.Config{ModelContextOverrides: []config.ModelContextOverride{{Model: "custom-a", ContextLength: 262144}}},
		configFilePath: path,
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/model-context-overrides?model=custom-a", nil)

	h.DeleteModelContextOverride(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read config: %v", errRead)
	}
	if strings.Contains(string(saved), "custom-a") {
		t.Fatalf("deleted override still present on disk:\n%s", saved)
	}
}
