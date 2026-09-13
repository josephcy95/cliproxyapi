package helps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// commandCodeLiveCatalogBody is the real response shape captured from
// GET https://api.commandcode.ai/provider/v1/models.
const commandCodeLiveCatalogBody = `{
  "object": "list",
  "data": [
    {"id":"claude-sonnet-5","object":"model","created":1789321639,"owned_by":"command-code","name":"Claude Sonnet 5","context_length":1000000},
    {"id":"deepseek/deepseek-v4-flash","object":"model","created":1789321639,"owned_by":"command-code","name":"DeepSeek V4 Flash","context_length":262144},
    {"id":"","object":"model","created":1,"owned_by":"command-code","name":"broken","context_length":1000},
    {"id":"no-context-window","object":"model","created":1,"owned_by":"command-code","name":"broken","context_length":0}
  ]
}`

func TestParseCommandCodeCatalog(t *testing.T) {
	models := ParseCommandCodeCatalog([]byte(commandCodeLiveCatalogBody))
	if len(models) != 2 {
		t.Fatalf("parsed %d models, want 2 (entries without id or context window are skipped)", len(models))
	}

	first := models[0]
	if first.ID != "claude-sonnet-5" {
		t.Errorf("ID = %q", first.ID)
	}
	if first.Object != "model" {
		t.Errorf("Object = %q", first.Object)
	}
	if first.Created != 1789321639 {
		t.Errorf("Created = %d", first.Created)
	}
	if first.OwnedBy != "command-code" {
		t.Errorf("OwnedBy = %q", first.OwnedBy)
	}
	if first.DisplayName != "Claude Sonnet 5" {
		t.Errorf("DisplayName = %q", first.DisplayName)
	}
	// Clients size requests from the advertised window, so ContextLength must
	// survive parsing.
	if first.ContextLength != 1000000 {
		t.Errorf("ContextLength = %d, want 1000000", first.ContextLength)
	}

	second := models[1]
	if second.ID != "deepseek/deepseek-v4-flash" {
		t.Errorf("ID = %q", second.ID)
	}
	if second.ContextLength != 262144 {
		t.Errorf("ContextLength = %d, want 262144", second.ContextLength)
	}
}

func TestParseCommandCodeCatalogRejectsMalformed(t *testing.T) {
	for _, body := range []string{"", "not json", `{"data":{}}`, `{"object":"list"}`} {
		if models := ParseCommandCodeCatalog([]byte(body)); len(models) != 0 {
			t.Errorf("ParseCommandCodeCatalog(%q) = %d models, want 0", body, len(models))
		}
	}
}

func TestParseCommandCodeCatalogFillsDefaults(t *testing.T) {
	models := ParseCommandCodeCatalog([]byte(`{"data":[{"id":"m","context_length":10}]}`))
	if len(models) != 1 {
		t.Fatalf("parsed %d models, want 1", len(models))
	}
	if models[0].Object != "model" {
		t.Errorf("Object = %q, want the model default", models[0].Object)
	}
	if models[0].OwnedBy != CommandCodeProvider {
		t.Errorf("OwnedBy = %q, want %q", models[0].OwnedBy, CommandCodeProvider)
	}
	if models[0].DisplayName != "m" {
		t.Errorf("DisplayName = %q, want the id as fallback", models[0].DisplayName)
	}
}

func TestFetchCommandCodeModelsUsesLiveCatalog(t *testing.T) {
	ResetCommandCodeCatalogCache()
	t.Cleanup(ResetCommandCodeCatalogCache)

	requests := 0
	var sawAuthz string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		sawAuthz = r.Header.Get("Authorization")
		if got := r.URL.Path; got != commandCodeCatalogPath {
			t.Errorf("path = %q, want %q", got, commandCodeCatalogPath)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(commandCodeLiveCatalogBody))
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{ID: "cc-auth", Provider: CommandCodeProvider, Attributes: map[string]string{"base_url": server.URL}}
	models := FetchCommandCodeModels(context.Background(), auth, &config.Config{})
	if len(models) != 2 {
		t.Fatalf("fetched %d models, want 2", len(models))
	}
	// The catalog is anonymous, so no credentials may be attached.
	if sawAuthz != "" {
		t.Errorf("Authorization = %q, want it to be absent for the anonymous catalog", sawAuthz)
	}
	if requests != 1 {
		t.Fatalf("server saw %d requests, want 1", requests)
	}

	// A second call is served from the TTL cache, not a new request.
	models = FetchCommandCodeModels(context.Background(), auth, &config.Config{})
	if len(models) != 2 {
		t.Fatalf("cached fetch returned %d models, want 2", len(models))
	}
	if requests != 1 {
		t.Errorf("cached call made %d requests, want the catalog to be reused", requests)
	}
}

func TestFetchCommandCodeModelsCachesPerBaseURL(t *testing.T) {
	ResetCommandCodeCatalogCache()
	t.Cleanup(ResetCommandCodeCatalogCache)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(commandCodeLiveCatalogBody))
	}))
	defer server.Close()

	config := &config.Config{}
	for i := 0; i < 8; i++ {
		auth := &cliproxyauth.Auth{ID: "cc-" + string(rune('a'+i)), Provider: CommandCodeProvider,
			Attributes: map[string]string{"base_url": server.URL}}
		if models := FetchCommandCodeModels(context.Background(), auth, config); len(models) != 2 {
			t.Fatalf("concurrent-path fetch %d returned %d models", i, len(models))
		}
	}
}

func TestFetchCommandCodeModelsConcurrentAccess(t *testing.T) {
	// Config reloads can register models for several auths at once; the catalog
	// cache is mutex-guarded and must stay race-free under -race.
	ResetCommandCodeCatalogCache()
	t.Cleanup(ResetCommandCodeCatalogCache)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(commandCodeLiveCatalogBody))
	}))
	defer server.Close()

	cfg := &config.Config{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			auth := &cliproxyauth.Auth{ID: "shared-auth", Provider: CommandCodeProvider,
				Attributes: map[string]string{"base_url": server.URL}}
			FetchCommandCodeModels(context.Background(), auth, cfg)
		}()
	}
	wg.Wait()
}

func TestFetchCommandCodeModelsFallsBackToBootstrap(t *testing.T) {
	ResetCommandCodeCatalogCache()
	t.Cleanup(ResetCommandCodeCatalogCache)

	// A dead endpoint must not fail registration and must still yield a usable
	// model list.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{ID: "cc-auth", Provider: CommandCodeProvider,
		Attributes: map[string]string{"base_url": server.URL}}
	models := FetchCommandCodeModels(context.Background(), auth, &config.Config{})
	if len(models) != len(commandCodeBootstrapModels) {
		t.Fatalf("fallback returned %d models, want the %d-entry bootstrap list", len(models), len(commandCodeBootstrapModels))
	}
	for i, id := range commandCodeBootstrapModels {
		if models[i].ID != id {
			t.Errorf("bootstrap[%d].ID = %q, want %q", i, models[i].ID, id)
		}
	}
}

func TestFetchCommandCodeModelsFallsBackOnUnreachableHost(t *testing.T) {
	ResetCommandCodeCatalogCache()
	t.Cleanup(ResetCommandCodeCatalogCache)

	// Reserve a port, then close it so the connection is refused.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := server.URL
	server.Close()

	auth := &cliproxyauth.Auth{ID: "cc-auth", Provider: CommandCodeProvider,
		Attributes: map[string]string{"base_url": baseURL}}
	models := FetchCommandCodeModels(context.Background(), auth, &config.Config{})
	if len(models) != len(commandCodeBootstrapModels) {
		t.Fatalf("fallback returned %d models, want the bootstrap list", len(models))
	}
}

func TestFetchCommandCodeModelsCapsResponseBody(t *testing.T) {
	ResetCommandCodeCatalogCache()
	t.Cleanup(ResetCommandCodeCatalogCache)

	// A hostile endpoint streaming far more than the cap must not exhaust memory;
	// the truncated body simply fails parsing and falls back.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"`))
		chunk := make([]byte, 64*1024)
		for i := range chunk {
			chunk[i] = 'a'
		}
		for i := 0; i < 64; i++ {
			if _, errWrite := w.Write(chunk); errWrite != nil {
				return
			}
		}
		_, _ = w.Write([]byte(`","context_length":1000}]}`))
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{ID: "cc-auth", Provider: CommandCodeProvider,
		Attributes: map[string]string{"base_url": server.URL}}
	models := FetchCommandCodeModels(context.Background(), auth, &config.Config{})
	if len(models) != len(commandCodeBootstrapModels) {
		t.Fatalf("oversized catalog returned %d models, want the bootstrap fallback", len(models))
	}
}

func TestFetchCommandCodeModelsUsesDefaultBaseURL(t *testing.T) {
	// With no base_url attribute the vendor host is the default. This asserts the
	// resolution itself rather than letting a request go out: the earlier version
	// issued real outbound traffic and then asserted only that the result was
	// non-empty, which the bootstrap fallback guarantees unconditionally — so it
	// passed regardless of what the resolver returned.
	if got := resolveCommandCodeCatalogBaseURL(nil); got != CommandCodeDefaultBaseURL {
		t.Errorf("base URL for a nil auth = %q, want %q", got, CommandCodeDefaultBaseURL)
	}
	auth := &cliproxyauth.Auth{ID: "cc-auth", Provider: CommandCodeProvider}
	if got := resolveCommandCodeCatalogBaseURL(auth); got != CommandCodeDefaultBaseURL {
		t.Errorf("base URL for an auth without base_url = %q, want %q", got, CommandCodeDefaultBaseURL)
	}
	withBase := &cliproxyauth.Auth{
		ID:         "cc-auth-2",
		Provider:   CommandCodeProvider,
		Attributes: map[string]string{"base_url": "https://alt.example.com"},
	}
	if got := resolveCommandCodeCatalogBaseURL(withBase); got != "https://alt.example.com" {
		t.Errorf("base URL = %q, want the configured override", got)
	}
}
