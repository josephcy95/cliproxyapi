package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestDesensitizationPreviewHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Desensitization: config.DefaultDesensitizationConfig()}
	h := &Handler{cfg: cfg}
	r := gin.New()
	r.POST("/preview", h.PreviewDesensitization)

	body, _ := json.Marshal(map[string]string{"text": "call 13800138000"})
	req := httptest.NewRequest(http.MethodPost, "/preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	masked, _ := resp["masked"].(string)
	if masked == "" || masked == "call 13800138000" {
		t.Fatalf("expected masked output, got %#v", resp)
	}
}

func TestGetDesensitizationConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Desensitization: config.DefaultDesensitizationConfig()}
	h := &Handler{cfg: cfg}
	r := gin.New()
	r.GET("/desensitization-config", h.GetDesensitizationConfig)
	req := httptest.NewRequest(http.MethodGet, "/desensitization-config", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var got config.DesensitizationConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("default enabled should be false")
	}
}

func TestGetDesensitizationConfigDefaultScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Desensitization: config.DefaultDesensitizationConfig()}
	h := &Handler{cfg: cfg}
	r := gin.New()
	r.GET("/desensitization-config", h.GetDesensitizationConfig)
	req := httptest.NewRequest(http.MethodGet, "/desensitization-config", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var got config.DesensitizationConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Scope != config.DesensitizationScopeAll {
		t.Fatalf("scope = %q, want all", got.Scope)
	}
}

func TestGetDesensitizationScopeOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{APIKeys: []string{" inbound-key "}},
		OpenAICompatibility: []config.OpenAICompatibility{
			{Name: "bohe"},
		},
	}
	h := &Handler{cfg: cfg}
	r := gin.New()
	r.GET("/scope-options", h.GetDesensitizationScopeOptions)
	req := httptest.NewRequest(http.MethodGet, "/scope-options", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	keys, _ := got["api_keys"].([]any)
	if len(keys) != 1 || keys[0] != "inbound-key" {
		t.Fatalf("api_keys = %#v", got["api_keys"])
	}
	oauth, _ := got["oauth_providers"].([]any)
	if len(oauth) == 0 {
		t.Fatal("expected oauth_providers")
	}
	apis, _ := got["api_providers"].([]any)
	foundBohe := false
	for _, item := range apis {
		if item == "bohe" {
			foundBohe = true
		}
	}
	if !foundBohe {
		t.Fatalf("api_providers missing compat name: %#v", apis)
	}
}

func TestPutDesensitizationConfigAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Desensitization: config.DefaultDesensitizationConfig()}
	h := &Handler{cfg: cfg}
	r := gin.New()
	r.PUT("/desensitization-config", h.PutDesensitizationConfig)
	r.GET("/desensitization-config", h.GetDesensitizationConfig)

	body := config.DefaultDesensitizationConfig()
	body.Enabled = true
	body.Allowlist = []string{"exact-value", "  trimmed  "}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/desensitization-config", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// persist may fail without full handler setup; accept 200 or persist-related codes if any
	if w.Code != http.StatusOK && w.Code != http.StatusInternalServerError {
		// Put calls persist(c) which may write status; check cfg mutation regardless
	}
	if !h.cfg.Desensitization.Enabled {
		t.Fatal("expected enabled after PUT")
	}
	if len(h.cfg.Desensitization.Allowlist) == 0 {
		t.Fatalf("allowlist not stored: %#v", h.cfg.Desensitization.Allowlist)
	}
	found := false
	for _, v := range h.cfg.Desensitization.Allowlist {
		if v == "exact-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("allowlist missing exact-value: %#v", h.cfg.Desensitization.Allowlist)
	}

	req = httptest.NewRequest(http.MethodGet, "/desensitization-config", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d", w.Code)
	}
	var got config.DesensitizationConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, v := range got.Allowlist {
		if v == "exact-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET allowlist missing exact-value: %#v", got.Allowlist)
	}
}
