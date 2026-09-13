package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func newCommandCodeHandler(t *testing.T, entries ...config.CommandCodeKey) *Handler {
	t.Helper()
	cfg := &config.Config{CommandCodeKey: entries}
	return &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}
}

func runCommandCodeHandler(t *testing.T, h *Handler, method, target, body string, fn func(*Handler, *gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	ctx.Request = httptest.NewRequest(method, target, reader)
	ctx.Request.Header.Set("Content-Type", "application/json")
	if target != "" {
		// Populate query values for handlers that read them via c.Query.
		ctx.Request.URL.RawQuery = strings.TrimPrefix(strings.SplitN(target, "?", 2)[0:1][0], "")
		if idx := strings.Index(target, "?"); idx >= 0 {
			ctx.Request.URL.RawQuery = target[idx+1:]
		}
	}
	fn(h, ctx)
	return rec
}

// TestSanitizeCommandCodeKeysKeepsEntryWithoutBaseURL is the regression guard for
// the sharpest trap in this provider: sanitizeCodexKeyEntries (shared by codex
// and xai) DROPS entries with an empty BaseURL. Command Code's base-url is
// optional and documented as defaulting to the public host, so reusing that
// sanitiser would silently delete the credential of every user who omits it —
// once at config load, on every reload.
func TestSanitizeCommandCodeKeysKeepsEntryWithoutBaseURL(t *testing.T) {
	cfg := &config.Config{CommandCodeKey: []config.CommandCodeKey{
		{APIKey: "user_default_base"},
	}}

	cfg.SanitizeCommandCodeKeys()

	if len(cfg.CommandCodeKey) != 1 {
		t.Fatalf("entry without base-url was dropped: got %d entries, want 1", len(cfg.CommandCodeKey))
	}
	if cfg.CommandCodeKey[0].APIKey != "user_default_base" {
		t.Fatalf("api key = %q, want it preserved", cfg.CommandCodeKey[0].APIKey)
	}
}

// TestSanitizeCommandCodeKeysClearsCodexOnlyFields guards against the shared
// CodexKey shape leaking codex-only transport switches into this provider.
func TestSanitizeCommandCodeKeysClearsCodexOnlyFields(t *testing.T) {
	cfg := &config.Config{CommandCodeKey: []config.CommandCodeKey{
		{APIKey: "user_x", BaseURL: " https://api.commandcode.ai ", Websockets: true, AlphaSearch: true},
	}}

	cfg.SanitizeCommandCodeKeys()

	entry := cfg.CommandCodeKey[0]
	if entry.Websockets || entry.AlphaSearch {
		t.Errorf("codex-only fields survived: websockets=%v alpha-search=%v", entry.Websockets, entry.AlphaSearch)
	}
	if entry.BaseURL != "https://api.commandcode.ai" {
		t.Errorf("base-url = %q, want it trimmed", entry.BaseURL)
	}
}

// TestCommandCodeKeysRoundTrip covers PUT -> GET through the management API and
// asserts an entry with no base-url survives the round trip.
func TestCommandCodeKeysRoundTrip(t *testing.T) {
	h := newCommandCodeHandler(t)

	payload := `[{"api-key":"user_aaaa","weight":3,"models":[{"name":"deepseek/deepseek-v4-flash","alias":"cc-flash"}]},
	             {"api-key":"user_bbbb","base-url":"https://alt.example.com"}]`
	if rec := runCommandCodeHandler(t, h, http.MethodPut, "/v0/management/commandcode-api-key", payload, (*Handler).PutCommandCodeKeys); rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if len(h.cfg.CommandCodeKey) != 2 {
		t.Fatalf("stored %d entries, want 2", len(h.cfg.CommandCodeKey))
	}
	if h.cfg.CommandCodeKey[0].BaseURL != "" {
		t.Errorf("entry without base-url had one injected: %q", h.cfg.CommandCodeKey[0].BaseURL)
	}

	rec := runCommandCodeHandler(t, h, http.MethodGet, "/v0/management/commandcode-api-key", "", (*Handler).GetCommandCodeKeys)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rec.Code)
	}
	var body map[string][]config.CommandCodeKey
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode GET body: %v; body=%s", err, rec.Body.String())
	}
	entries := body["commandcode-api-key"]
	if len(entries) != 2 {
		t.Fatalf("GET returned %d entries, want 2; body=%s", len(entries), rec.Body.String())
	}
	if entries[0].APIKey != "user_aaaa" || entries[1].APIKey != "user_bbbb" {
		t.Errorf("GET returned wrong entries: %+v", entries)
	}
	if len(entries[0].Models) != 1 || entries[0].Models[0].Alias != "cc-flash" {
		t.Errorf("models lost in round trip: %+v", entries[0].Models)
	}
}

// TestCommandCodeDeleteAndPatch covers the two paths that differ from codex:
// clearing base-url must NOT delete the credential, and delete works by
// api-key and by index.
func TestCommandCodeDeleteAndPatch(t *testing.T) {
	t.Run("clearing base-url keeps the credential", func(t *testing.T) {
		h := newCommandCodeHandler(t, config.CommandCodeKey{APIKey: "user_keep", BaseURL: "https://alt.example.com"})

		body := `{"index":0,"value":{"base-url":""}}`
		if rec := runCommandCodeHandler(t, h, http.MethodPatch, "/v0/management/commandcode-api-key", body, (*Handler).PatchCommandCodeKey); rec.Code != http.StatusOK {
			t.Fatalf("PATCH status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if len(h.cfg.CommandCodeKey) != 1 {
			t.Fatalf("clearing base-url removed the credential: %d entries, want 1", len(h.cfg.CommandCodeKey))
		}
		if h.cfg.CommandCodeKey[0].BaseURL != "" {
			t.Errorf("base-url = %q, want it cleared so the default applies", h.cfg.CommandCodeKey[0].BaseURL)
		}
	})

	t.Run("patch by match", func(t *testing.T) {
		h := newCommandCodeHandler(t, config.CommandCodeKey{APIKey: "user_match"})

		body := `{"match":"user_match","value":{"priority":7}}`
		if rec := runCommandCodeHandler(t, h, http.MethodPatch, "/v0/management/commandcode-api-key", body, (*Handler).PatchCommandCodeKey); rec.Code != http.StatusOK {
			t.Fatalf("PATCH status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if h.cfg.CommandCodeKey[0].Priority != 7 {
			t.Errorf("priority = %d, want 7", h.cfg.CommandCodeKey[0].Priority)
		}
	})

	t.Run("patch missing target is 404", func(t *testing.T) {
		h := newCommandCodeHandler(t, config.CommandCodeKey{APIKey: "user_a"})
		body := `{"index":9,"value":{"priority":1}}`
		if rec := runCommandCodeHandler(t, h, http.MethodPatch, "/v0/management/commandcode-api-key", body, (*Handler).PatchCommandCodeKey); rec.Code != http.StatusNotFound {
			t.Fatalf("PATCH status = %d, want 404", rec.Code)
		}
	})

	t.Run("delete by api-key", func(t *testing.T) {
		h := newCommandCodeHandler(t,
			config.CommandCodeKey{APIKey: "user_a"},
			config.CommandCodeKey{APIKey: "user_b"},
		)
		if rec := runCommandCodeHandler(t, h, http.MethodDelete, "/v0/management/commandcode-api-key?api-key=user_a", "", (*Handler).DeleteCommandCodeKey); rec.Code != http.StatusOK {
			t.Fatalf("DELETE status = %d, want 200", rec.Code)
		}
		if len(h.cfg.CommandCodeKey) != 1 || h.cfg.CommandCodeKey[0].APIKey != "user_b" {
			t.Fatalf("after delete: %+v", h.cfg.CommandCodeKey)
		}
	})

	t.Run("delete by index", func(t *testing.T) {
		h := newCommandCodeHandler(t,
			config.CommandCodeKey{APIKey: "user_a"},
			config.CommandCodeKey{APIKey: "user_b"},
		)
		if rec := runCommandCodeHandler(t, h, http.MethodDelete, "/v0/management/commandcode-api-key?index=1", "", (*Handler).DeleteCommandCodeKey); rec.Code != http.StatusOK {
			t.Fatalf("DELETE status = %d, want 200", rec.Code)
		}
		if len(h.cfg.CommandCodeKey) != 1 || h.cfg.CommandCodeKey[0].APIKey != "user_a" {
			t.Fatalf("after delete: %+v", h.cfg.CommandCodeKey)
		}
	})

	t.Run("delete missing is 400", func(t *testing.T) {
		h := newCommandCodeHandler(t, config.CommandCodeKey{APIKey: "user_a"})
		if rec := runCommandCodeHandler(t, h, http.MethodDelete, "/v0/management/commandcode-api-key", "", (*Handler).DeleteCommandCodeKey); rec.Code != http.StatusBadRequest {
			t.Fatalf("DELETE status = %d, want 400", rec.Code)
		}
	})
}

// TestCommandCodePatchRejectsInvalidWeight confirms the weight guard is wired for
// this family, which also proves commandcode-api-key was added to the weight
// validation list rather than silently skipping it.
//
// Note: a non-positive weight is VALID here (it excludes the credential from
// weighted round-robin), so the rejection case must exceed the documented max.
func TestCommandCodePatchRejectsInvalidWeight(t *testing.T) {
	h := newCommandCodeHandler(t, config.CommandCodeKey{APIKey: "user_w"})

	body := `{"index":0,"value":{"weight":2000000}}`
	rec := runCommandCodeHandler(t, h, http.MethodPatch, "/v0/management/commandcode-api-key", body, (*Handler).PatchCommandCodeKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH invalid weight status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
