package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// testToken builds a JWT-shaped token. The signature is never verified for this
// claim extraction, so the parts only need to be well-formed.
func testToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, errHeader := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	if errHeader != nil {
		t.Fatalf("marshal header: %v", errHeader)
	}
	payload, errPayload := json.Marshal(claims)
	if errPayload != nil {
		t.Fatalf("marshal claims: %v", errPayload)
	}
	encode := func(raw []byte) string {
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(header) + "." + encode(payload) + ".signature"
}

// liveClaims renders the ChatGPT claim shape the provider reads.
func liveClaims(accountID string, expires time.Time) map[string]any {
	return map[string]any{
		"exp": expires.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	}
}

func TestTokenClaimsAndExpiry(t *testing.T) {
	valid := testToken(t, liveClaims("acct-1", time.Now().Add(time.Hour)))
	if got := accountIDFromToken(valid); got != "acct-1" {
		t.Fatalf("accountIDFromToken = %q, want acct-1", got)
	}
	if tokenExpired(valid) {
		t.Fatal("a future exp must not be treated as expired")
	}

	expired := testToken(t, liveClaims("acct-1", time.Now().Add(-time.Hour)))
	if !tokenExpired(expired) {
		t.Fatal("a past exp must be treated as expired")
	}

	// An opaque credential cannot be introspected and must not be discarded.
	if tokenExpired("opaque-token") {
		t.Fatal("a non-JWT token must not be reported as expired")
	}
	if got := accountIDFromToken("opaque-token"); got != "" {
		t.Fatalf("accountIDFromToken on a non-JWT = %q, want empty", got)
	}
	if tokenExpired("") {
		t.Fatal("an empty token must not be reported as expired")
	}
}

func TestCredentialFromAuth(t *testing.T) {
	// Attributes win, and an explicit account id is used as-is.
	fromAttrs := credentialFromAuth(&cliproxyauth.Auth{
		Attributes: map[string]string{"api_key": "token-a", "account_id": "acct-a"},
	})
	if fromAttrs.AccessToken != "token-a" || fromAttrs.AccountID != "acct-a" {
		t.Fatalf("attributes credential = %+v", fromAttrs)
	}

	// A metadata-only credential derives the account id from the token claim.
	claimed := testToken(t, liveClaims("acct-from-jwt", time.Now().Add(time.Hour)))
	fromMeta := credentialFromAuth(&cliproxyauth.Auth{
		Metadata: map[string]any{"access_token": claimed},
	})
	if fromMeta.AccessToken != claimed {
		t.Fatalf("metadata token not read: %+v", fromMeta)
	}
	if fromMeta.AccountID != "acct-from-jwt" {
		t.Fatalf("account id not derived from the token: %q", fromMeta.AccountID)
	}

	if empty := credentialFromAuth(nil); empty.AccessToken != "" || empty.AccountID != "" {
		t.Fatalf("nil auth must yield an empty credential: %+v", empty)
	}
}

// writeCodexAuth writes one Codex auth file into dir.
func writeCodexAuth(t *testing.T, dir, name, token, accountID string) {
	t.Helper()
	payload := map[string]any{
		"type":         "codex",
		"access_token": token,
		"account_id":   accountID,
		"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal auth: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, name), raw, 0o600); errWrite != nil {
		t.Fatalf("write auth: %v", errWrite)
	}
}

func TestLookupCodexCredentialFromAuthDir(t *testing.T) {
	dir := t.TempDir()
	token := testToken(t, liveClaims("acct-borrowed", time.Now().Add(time.Hour)))
	writeCodexAuth(t, dir, "codex-a.json", token, "acct-borrowed")

	previous := excelConfiguredAuthDir()
	SetExcelAuthDir(dir)
	invalidateCodexCredentialCache()
	t.Cleanup(func() {
		SetExcelAuthDir(previous)
		invalidateCodexCredentialCache()
	})

	credential, ok := lookupCodexCredential(context.Background(), "")
	if !ok {
		t.Fatal("a usable Codex token in the auth directory was not found")
	}
	if credential.AccessToken != token || credential.AccountID != "acct-borrowed" {
		t.Fatalf("borrowed credential = %+v", credential)
	}

	// Pinning to a different account must not silently fall back to this one.
	if _, okOther := lookupCodexCredential(context.Background(), "some-other-account"); okOther {
		t.Fatal("a pinned account must not fall back to an unrelated account")
	}
	// Pinning to the right account resolves.
	if _, okPinned := lookupCodexCredential(context.Background(), "acct-borrowed"); !okPinned {
		t.Fatal("a pinned matching account was not resolved")
	}
}

func TestCodexCredentialFiltering(t *testing.T) {
	dir := t.TempDir()
	valid := testToken(t, liveClaims("acct-valid", time.Now().Add(time.Hour)))
	expired := testToken(t, liveClaims("acct-expired", time.Now().Add(-time.Hour)))

	writeCodexAuth(t, dir, "a-valid.json", valid, "acct-valid")
	writeCodexAuth(t, dir, "b-expired.json", expired, "acct-expired")

	// A disabled entry must be ignored even though its token is valid.
	disabled := map[string]any{
		"type":         "codex",
		"access_token": testToken(t, liveClaims("acct-disabled", time.Now().Add(time.Hour))),
		"account_id":   "acct-disabled",
		"disabled":     true,
	}
	raw, _ := json.Marshal(disabled)
	if errWrite := os.WriteFile(filepath.Join(dir, "c-disabled.json"), raw, 0o600); errWrite != nil {
		t.Fatalf("write disabled auth: %v", errWrite)
	}

	// A different provider must be ignored.
	other := map[string]any{"type": "claude", "access_token": "not-a-codex-token", "account_id": "acct-other"}
	rawOther, _ := json.Marshal(other)
	if errWrite := os.WriteFile(filepath.Join(dir, "d-claude.json"), rawOther, 0o600); errWrite != nil {
		t.Fatalf("write other auth: %v", errWrite)
	}

	previous := excelConfiguredAuthDir()
	SetExcelAuthDir(dir)
	invalidateCodexCredentialCache()
	t.Cleanup(func() {
		SetExcelAuthDir(previous)
		invalidateCodexCredentialCache()
	})

	entries := cachedCodexCredentials(context.Background(), true)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one usable Codex credential, got %d: %+v", len(entries), entries)
	}
	if entries[0].AccountID != "acct-valid" {
		t.Fatalf("wrong credential survived filtering: %+v", entries[0])
	}
}

func TestResolveExcelCredentialPrefersExplicitToken(t *testing.T) {
	// An explicit token must be used without consulting the auth directory, even
	// when use-codex-auths is also set.
	dir := t.TempDir()
	writeCodexAuth(t, dir, "codex.json", testToken(t, liveClaims("acct-other", time.Now().Add(time.Hour))), "acct-other")
	previous := excelConfiguredAuthDir()
	SetExcelAuthDir(dir)
	invalidateCodexCredentialCache()
	t.Cleanup(func() {
		SetExcelAuthDir(previous)
		invalidateCodexCredentialCache()
	})

	explicit := testToken(t, liveClaims("acct-explicit", time.Now().Add(time.Hour)))
	credential, ok := resolveExcelCredential(context.Background(), &cliproxyauth.Auth{
		Attributes: map[string]string{"api_key": explicit, "account_id": "acct-explicit"},
		Metadata:   map[string]any{"use_codex_auths": true},
	})
	if !ok {
		t.Fatal("an explicit credential must always resolve")
	}
	if credential.AccessToken != explicit || credential.AccountID != "acct-explicit" {
		t.Fatalf("explicit credential ignored: %+v", credential)
	}
}

func TestResolveExcelCredentialRequiresOptIn(t *testing.T) {
	dir := t.TempDir()
	writeCodexAuth(t, dir, "codex.json", testToken(t, liveClaims("acct-1", time.Now().Add(time.Hour))), "acct-1")
	previous := excelConfiguredAuthDir()
	SetExcelAuthDir(dir)
	invalidateCodexCredentialCache()
	t.Cleanup(func() {
		SetExcelAuthDir(previous)
		invalidateCodexCredentialCache()
	})

	// Without use-codex-auths the provider must not borrow an unrelated account.
	if _, ok := resolveExcelCredential(context.Background(), &cliproxyauth.Auth{}); ok {
		t.Fatal("borrowing must require explicit opt-in")
	}
	if _, ok := resolveExcelCredential(context.Background(), nil); ok {
		t.Fatal("a nil auth must not borrow a credential")
	}
	// With the opt-in it resolves.
	if _, ok := resolveExcelCredential(context.Background(), &cliproxyauth.Auth{
		Metadata: map[string]any{"use_codex_auths": true},
	}); !ok {
		t.Fatal("use-codex-auths did not borrow a credential")
	}
}
