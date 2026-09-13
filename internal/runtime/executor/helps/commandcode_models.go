package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// commandCodeCatalogPath is the anonymous model catalog. Command Code serves
	// it without authentication, so no credentials are attached.
	commandCodeCatalogPath = "/provider/v1/models"
	// commandCodeCatalogTTL bounds how long a fetched catalog is reused.
	commandCodeCatalogTTL = 3 * time.Hour
	// commandCodeCatalogTimeout bounds the catalog request itself.
	commandCodeCatalogTimeout = 15 * time.Second
	// commandCodeCatalogMaxBytes caps the catalog response body.
	commandCodeCatalogMaxBytes int64 = 2 << 20
	// CommandCodeProvider is the provider identifier used for catalog entries and
	// routing. It matches the executor's Identifier().
	CommandCodeProvider = "commandcode"
)

// commandCodeBootstrapModels is the last-resort cold-start list used when the
// live catalog cannot be fetched and the config declares no models. It exists so
// the provider is not dead offline; the live catalog supersedes it whenever the
// fetch succeeds.
var commandCodeBootstrapModels = []string{
	"deepseek/deepseek-v4-flash",
	"deepseek/deepseek-v4-pro",
	"claude-sonnet-5",
	"claude-opus-4.6",
	"gpt-5.3-codex",
	"kimi-k2.5",
	"glm-5",
}

// commandCodeCatalogEntry mirrors one entry of the catalog response.
type commandCodeCatalogEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
}

// commandCodeCatalogResponse mirrors the catalog envelope.
type commandCodeCatalogResponse struct {
	Object string                    `json:"object"`
	Data   []commandCodeCatalogEntry `json:"data"`
}

// commandCodeCatalogCache caches the anonymous catalog per base URL. A mutex
// guards the map because config reloads can register models for several auths
// concurrently.
var commandCodeCatalogCache = struct {
	mu      sync.Mutex
	entries map[string]commandCodeCatalogCacheEntry
}{entries: make(map[string]commandCodeCatalogCacheEntry)}

type commandCodeCatalogCacheEntry struct {
	models    []*registry.ModelInfo
	expiresAt time.Time
}

// NormalizeCommandCodeStatusError maps a Command Code upstream failure status to the
// semantically correct billing status. commandcode.ai returns billing exhaustion as
// "400 bad_request" with an "insufficient credits" message instead of 402; the auth
// lifecycle classifies every 400 as a request-scoped fault and skips credential
// handling for it, so the billing evidence must be promoted to 402 up-front.
func NormalizeCommandCodeStatusError(httpStatus int, body []byte) int {
	if httpStatus == http.StatusBadRequest && strings.Contains(strings.ToLower(string(body)), "insufficient credits") {
		return http.StatusPaymentRequired
	}
	return httpStatus
}

// FetchCommandCodeModels returns the model list for a Command Code auth. The
// configured models are handled by the caller (they win over the catalog); this
// path serves the live anonymous catalog, falling back to a short embedded
// bootstrap list when the catalog cannot be fetched. A fetch failure is logged
// and never fails registration.
func FetchCommandCodeModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	baseURL := resolveCommandCodeCatalogBaseURL(auth)
	if models := fetchCommandCodeCatalog(ctx, auth, cfg, baseURL); len(models) > 0 {
		return models
	}
	return commandCodeBootstrapModelInfos()
}

// resolveCommandCodeCatalogBaseURL picks the host for the catalog request. The
// auth's base_url attribute wins; an absent or blank value means the vendor host.
// Split out so the default is unit-testable without issuing a request.
func resolveCommandCodeCatalogBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if trimmed := strings.TrimSpace(auth.Attributes["base_url"]); trimmed != "" {
			return trimmed
		}
	}
	return CommandCodeDefaultBaseURL
}

// commandCodeBootstrapModelInfos renders the cold-start fallback list.
func commandCodeBootstrapModelInfos() []*registry.ModelInfo {
	now := time.Now().Unix()
	out := make([]*registry.ModelInfo, 0, len(commandCodeBootstrapModels))
	for _, id := range commandCodeBootstrapModels {
		out = append(out, &registry.ModelInfo{
			ID:          id,
			Object:      "model",
			Created:     now,
			OwnedBy:     CommandCodeProvider,
			Type:        CommandCodeProvider,
			DisplayName: id,
		})
	}
	return out
}

// fetchCommandCodeCatalog returns the cached anonymous catalog, refreshing it
// when the TTL has elapsed. It logs and returns nil on any failure.
func fetchCommandCodeCatalog(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config, baseURL string) []*registry.ModelInfo {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	if models, ok := commandCodeCatalogCacheGet(baseURL); ok {
		return models
	}

	models, errFetch := requestCommandCodeCatalog(ctx, auth, cfg, baseURL)
	if errFetch != nil {
		log.Debugf("commandcode: model catalog fetch failed for %s: %v", baseURL, errFetch)
		// A short negative cache keeps a broken endpoint from being retried on
		// every reload without letting a transient failure stick around.
		commandCodeCatalogCachePut(baseURL, nil, time.Now().Add(time.Minute))
		return nil
	}
	commandCodeCatalogCachePut(baseURL, models, time.Now().Add(commandCodeCatalogTTL))
	return models
}

func commandCodeCatalogCacheGet(baseURL string) ([]*registry.ModelInfo, bool) {
	commandCodeCatalogCache.mu.Lock()
	defer commandCodeCatalogCache.mu.Unlock()
	entry, ok := commandCodeCatalogCache.entries[baseURL]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	if len(entry.models) == 0 {
		// Negative cache hit: the endpoint is known-bad for now.
		return nil, true
	}
	return entry.models, true
}

func commandCodeCatalogCachePut(baseURL string, models []*registry.ModelInfo, expiresAt time.Time) {
	commandCodeCatalogCache.mu.Lock()
	defer commandCodeCatalogCache.mu.Unlock()
	if commandCodeCatalogCache.entries == nil {
		commandCodeCatalogCache.entries = make(map[string]commandCodeCatalogCacheEntry)
	}
	commandCodeCatalogCache.entries[baseURL] = commandCodeCatalogCacheEntry{models: models, expiresAt: expiresAt}
}

// requestCommandCodeCatalog fetches and parses the anonymous catalog. The
// catalog needs no credentials, so no Authorization header is sent.
func requestCommandCodeCatalog(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config, baseURL string) ([]*registry.ModelInfo, error) {
	reqCtx, cancel := context.WithTimeout(ctx, commandCodeCatalogTimeout)
	defer cancel()

	req, errRequest := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+commandCodeCatalogPath, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", CommandCodeUserAgent)

	httpClient := NewProxyAwareHTTPClient(reqCtx, cfg, auth, commandCodeCatalogTimeout)
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("commandcode: close model catalog body: %v", errClose)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("commandcode: model catalog request failed with status %d", resp.StatusCode)
	}

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, commandCodeCatalogMaxBytes))
	if errRead != nil {
		return nil, errRead
	}
	return ParseCommandCodeCatalog(body), nil
}

// ParseCommandCodeCatalog maps the catalog response into registry entries. An
// entry without an id or with a non-positive context window is skipped rather
// than failing the whole list.
func ParseCommandCodeCatalog(body []byte) []*registry.ModelInfo {
	if len(body) == 0 || !json.Valid(body) {
		return nil
	}
	var response commandCodeCatalogResponse
	if errUnmarshal := json.Unmarshal(body, &response); errUnmarshal != nil {
		return nil
	}
	out := make([]*registry.ModelInfo, 0, len(response.Data))
	for _, entry := range response.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || entry.ContextLength <= 0 {
			continue
		}
		displayName := strings.TrimSpace(entry.Name)
		if displayName == "" {
			displayName = id
		}
		object := strings.TrimSpace(entry.Object)
		if object == "" {
			object = "model"
		}
		ownedBy := strings.TrimSpace(entry.OwnedBy)
		if ownedBy == "" {
			ownedBy = CommandCodeProvider
		}
		out = append(out, &registry.ModelInfo{
			ID:            id,
			Object:        object,
			Created:       entry.Created,
			OwnedBy:       ownedBy,
			Type:          CommandCodeProvider,
			DisplayName:   displayName,
			ContextLength: entry.ContextLength,
		})
	}
	return out
}

// ResetCommandCodeCatalogCache clears the catalog cache. It exists for tests.
func ResetCommandCodeCatalogCache() {
	commandCodeCatalogCache.mu.Lock()
	defer commandCodeCatalogCache.mu.Unlock()
	commandCodeCatalogCache.entries = make(map[string]commandCodeCatalogCacheEntry)
}
