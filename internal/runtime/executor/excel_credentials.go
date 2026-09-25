package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	// excelCredentialCacheTTL bounds how long a Codex credential borrowed for
	// the Excel provider is reused before the auth store is re-read. It is
	// deliberately short: the token expires roughly every ten days, but the
	// Codex provider refreshes it without telling this one, so a stale copy is
	// picked up quickly rather than at the next restart.
	excelCredentialCacheTTL = 30 * time.Second
)

// excelCredential is a ChatGPT access token borrowed from a Codex account.
type excelCredential struct {
	AccessToken string
	AccountID   string
}

// excelAuthDir holds the configured auth directory, published by the service
// when configuration is applied.
var excelAuthDir = struct {
	sync.RWMutex
	dir string
}{}

// SetExcelAuthDir publishes the auth directory the provider should read Codex
// tokens from.
func SetExcelAuthDir(dir string) {
	excelAuthDir.Lock()
	excelAuthDir.dir = strings.TrimSpace(dir)
	excelAuthDir.Unlock()
}

// excelConfiguredAuthDir returns the published auth directory.
func excelConfiguredAuthDir() string {
	excelAuthDir.RLock()
	defer excelAuthDir.RUnlock()
	return excelAuthDir.dir
}

// readAuthDirCredentials reads Codex token files from the configured auth
// directory.
func readAuthDirCredentials() []*cliproxyauth.Auth {
	dir := excelConfiguredAuthDir()
	if dir == "" {
		return nil
	}
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, errList := store.List(context.Background())
	if errList != nil {
		return nil
	}
	return auths
}

// excelCredentialCache memoises Codex tokens for the Excel provider.
//
// Codex access tokens expire roughly every ten days and are refreshed by the
// Codex provider itself. Reading them from the shared auth store on demand means
// the Excel provider has no token store of its own and therefore no second
// refresh lifecycle to keep in sync.
var excelCredentialCache = struct {
	sync.Mutex
	fetchedAt time.Time
	entries   []excelCredential
}{}

// resolveExcelCredential returns the credential an Excel auth entry should use.
//
// An entry with an explicit token uses it directly. Otherwise the token is
// borrowed from the loaded Codex accounts, preferring the account named in the
// entry's metadata when one is set.
func resolveExcelCredential(ctx context.Context, auth *cliproxyauth.Auth) (excelCredential, bool) {
	if credential := credentialFromAuth(auth); credential.AccessToken != "" {
		return credential, true
	}
	if auth == nil || auth.Metadata == nil {
		return excelCredential{}, false
	}
	useCodexAuths, _ := auth.Metadata["use_codex_auths"].(bool)
	if !useCodexAuths {
		return excelCredential{}, false
	}
	preferred := strings.TrimSpace(stringFromAny(auth.Metadata["preferred_account_id"]))
	return lookupCodexCredential(ctx, preferred)
}

// credentialFromAuth reads a credential stored directly on the auth entry.
func credentialFromAuth(auth *cliproxyauth.Auth) excelCredential {
	if auth == nil {
		return excelCredential{}
	}
	var credential excelCredential
	if auth.Attributes != nil {
		credential.AccessToken = strings.TrimSpace(auth.Attributes["api_key"])
		credential.AccountID = strings.TrimSpace(auth.Attributes["account_id"])
	}
	if auth.Metadata != nil {
		if credential.AccessToken == "" {
			credential.AccessToken = strings.TrimSpace(stringFromAny(auth.Metadata["access_token"]))
		}
		if credential.AccountID == "" {
			credential.AccountID = strings.TrimSpace(stringFromAny(auth.Metadata["account_id"]))
		}
	}
	// A configured account id wins over one derived from the token, because it is
	// what the operator explicitly asked for.
	if credential.AccountID == "" {
		credential.AccountID = accountIDFromToken(credential.AccessToken)
	}
	return credential
}

// lookupCodexCredential scans the shared auth store for a usable Codex token.
func lookupCodexCredential(ctx context.Context, preferredAccountID string) (excelCredential, bool) {
	entries := cachedCodexCredentials(ctx, false)
	if len(entries) == 0 {
		return excelCredential{}, false
	}
	if preferredAccountID != "" {
		for _, entry := range entries {
			if strings.EqualFold(entry.AccountID, preferredAccountID) {
				return entry, true
			}
		}
		return excelCredential{}, false
	}
	return entries[0], true
}

// invalidateCodexCredentialCache forces the next lookup to re-read the store.
func invalidateCodexCredentialCache() {
	excelCredentialCache.Lock()
	excelCredentialCache.fetchedAt = time.Time{}
	excelCredentialCache.entries = nil
	excelCredentialCache.Unlock()
}

// cachedCodexCredentials returns the cached Codex credentials, refreshing them
// when the TTL has elapsed.
func cachedCodexCredentials(ctx context.Context, force bool) []excelCredential {
	excelCredentialCache.Lock()
	defer excelCredentialCache.Unlock()
	if !force && !excelCredentialCache.fetchedAt.IsZero() && time.Since(excelCredentialCache.fetchedAt) < excelCredentialCacheTTL {
		return excelCredentialCache.entries
	}
	entries := readCodexCredentials(ctx)
	excelCredentialCache.entries = entries
	excelCredentialCache.fetchedAt = time.Now()
	return entries
}

// readCodexCredentials loads unexpired Codex access tokens.
//
// The configured auth directory is read directly, because that is where the
// running service loads auth files from; the shared token store is consulted as
// a fallback for builds that configure it instead.
func readCodexCredentials(ctx context.Context) []excelCredential {
	auths := readAuthDirCredentials()
	if len(auths) == 0 {
		if store := sdkauth.GetTokenStore(); store != nil {
			listed, errList := store.List(ctx)
			if errList == nil {
				auths = listed
			}
		}
	}
	entries := make([]excelCredential, 0, len(auths))
	for _, candidate := range auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "codex") {
			continue
		}
		token := ""
		if candidate.Attributes != nil {
			token = strings.TrimSpace(candidate.Attributes["api_key"])
		}
		if token == "" && candidate.Metadata != nil {
			token = strings.TrimSpace(stringFromAny(candidate.Metadata["access_token"]))
		}
		if token == "" {
			continue
		}
		if tokenExpired(token) {
			continue
		}
		accountID := ""
		if candidate.Metadata != nil {
			accountID = strings.TrimSpace(stringFromAny(candidate.Metadata["account_id"]))
		}
		if accountID == "" {
			accountID = accountIDFromToken(token)
		}
		entries = append(entries, excelCredential{AccessToken: token, AccountID: accountID})
	}
	return entries
}

// accountIDFromToken reads the ChatGPT account id claim from an access token.
//
// The token is a ChatGPT OAuth access token, not a security boundary: it is
// already trusted because it was authenticated when it was stored, so the claim
// is read without verifying the signature (matching how the Codex provider
// itself derives the account id).
func accountIDFromToken(token string) string {
	claims := tokenClaims(token)
	if claims == nil {
		return ""
	}
	authClaim, okClaim := claims["https://api.openai.com/auth"].(map[string]any)
	if !okClaim {
		return ""
	}
	return strings.TrimSpace(stringFromAny(authClaim["chatgpt_account_id"]))
}

// tokenExpired reports whether a JWT-shaped token's exp claim is in the past.
func tokenExpired(token string) bool {
	claims := tokenClaims(token)
	if claims == nil {
		// A non-JWT token cannot be checked; treat it as usable so a custom
		// opaque credential still works.
		return false
	}
	expires, okExpires := claims["exp"].(float64)
	if !okExpires {
		return false
	}
	return time.Now().After(time.Unix(int64(expires), 0))
}

// tokenClaims decodes a JWT payload without verifying its signature.
func tokenClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil
	}
	padding := strings.Repeat("=", (4-len(parts[1])%4)%4)
	decoded, errDecode := base64.URLEncoding.DecodeString(parts[1] + padding)
	if errDecode != nil {
		return nil
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(decoded, &claims); errUnmarshal != nil {
		return nil
	}
	return claims
}

func stringFromAny(value any) string {
	text, _ := value.(string)
	return text
}
