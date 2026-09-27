package desensitization

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func enabledCfg() config.DesensitizationConfig {
	cfg := config.DefaultDesensitizationConfig()
	cfg.Enabled = true
	return config.NormalizeDesensitizationConfig(cfg)
}

func TestMaskRestorePhoneEmailSK(t *testing.T) {
	eng := NewEngine(enabledCfg())
	sid := "sess-1"
	in := `联系我 13800138000 或 user@example.com，密钥 sk-abcdefghijklmnopqrstuvwxyz012345`
	out := eng.maskText(sid, in, nil)
	if strings.Contains(out, "13800138000") || strings.Contains(out, "user@example.com") || strings.Contains(out, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("expected secrets masked, got %q", out)
	}
	if !strings.Contains(out, "{{PHONE_") || !strings.Contains(out, "{{EMAIL_") || !strings.Contains(out, "{{API_KEY_") {
		t.Fatalf("expected placeholders, got %q", out)
	}
	restored := eng.restoreText(sid, out, "", true)
	if !strings.Contains(restored, "13800138000") || !strings.Contains(restored, "user@example.com") {
		t.Fatalf("expected PII restored, got %q", restored)
	}
	// restore_secrets default false — API key stays masked
	if strings.Contains(restored, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("API key should remain placeholder, got %q", restored)
	}
	if !strings.Contains(restored, "{{API_KEY_") {
		t.Fatalf("expected API_KEY placeholder kept, got %q", restored)
	}
}

func TestCustomTermAndSessionReuse(t *testing.T) {
	cfg := enabledCfg()
	cfg.CustomTerms = []config.DesensitizationTerm{{Value: "AcmeSecretCorp", Category: "TERM", WholeWord: true}}
	eng := NewEngine(cfg)
	sid := "sess-reuse"
	a := eng.maskText(sid, "call AcmeSecretCorp now", nil)
	b := eng.maskText(sid, "again AcmeSecretCorp please", nil)
	tokA := extractToken(a, "TERM")
	tokB := extractToken(b, "TERM")
	if tokA == "" || tokA != tokB {
		t.Fatalf("session reuse failed: %q vs %q", tokA, tokB)
	}
	phone1 := eng.maskText(sid, "13800138000", nil)
	phone2 := eng.maskText(sid, "tel 13800138000", nil)
	if extractToken(phone1, "PHONE") != extractToken(phone2, "PHONE") {
		t.Fatalf("phone token not stable: %q / %q", phone1, phone2)
	}
}

func TestChunkSplitRestore(t *testing.T) {
	eng := NewEngine(enabledCfg())
	sid := "sess-stream"
	masked := eng.maskText(sid, "hello 13800138000", nil)
	tok := extractToken(masked, "PHONE")
	if tok == "" {
		t.Fatal("no phone token")
	}
	// Split across {{PHO and NE_xxx}}
	mid := strings.Index(tok, "_")
	part1 := "hello " + tok[:mid]
	part2 := tok[mid:]
	out1 := eng.RestoreStreamChunk(sid, "s1", []byte(part1))
	out2 := eng.RestoreStreamChunk(sid, "s1", []byte(part2))
	flush := eng.FlushStream(sid, "s1")
	combined := string(out1) + string(out2) + string(flush)
	if combined != "hello 13800138000" {
		t.Fatalf("chunk restore = %q, want hello 13800138000 (parts %q + %q)", combined, part1, part2)
	}
}

func TestRestoreSecretsFalse(t *testing.T) {
	cfg := enabledCfg()
	falseVal := false
	cfg.RestoreSecrets = &falseVal
	eng := NewEngine(cfg)
	sid := "sess-sec"
	masked := eng.maskText(sid, "sk-abcdefghijklmnopqrstuvwxyz012345", nil)
	restored := eng.restoreText(sid, masked, "", true)
	if restored != masked {
		t.Fatalf("expected API key placeholder kept, got %q from %q", restored, masked)
	}
}

func TestWalkerNestedToolArgs(t *testing.T) {
	eng := NewEngine(enabledCfg())
	sid := "sess-walk"
	payload := map[string]any{
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": "hi",
			},
		},
		"tools": []any{
			map[string]any{
				"function": map[string]any{
					"arguments": `{"phone":"13800138000","note":"x"}`,
				},
			},
		},
	}
	raw, _ := json.Marshal(payload)
	out, err := eng.MaskJSONBody(sid, "gpt", "gpt", "openai", raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "13800138000") {
		t.Fatalf("nested tool args not masked: %s", out)
	}
	if !strings.Contains(string(out), "{{PHONE_") {
		t.Fatalf("expected phone token in nested args: %s", out)
	}
}

func TestDisabledNoop(t *testing.T) {
	cfg := enabledCfg()
	cfg.Enabled = false
	eng := NewEngine(cfg)
	raw := []byte(`{"text":"13800138000"}`)
	out, err := eng.MaskJSONBody("s", "m", "m", "openai", raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(raw) {
		t.Fatalf("disabled engine should no-op, got %s", out)
	}
}

func TestPhoneRejectAllSame(t *testing.T) {
	if validatePhone("11111111111") {
		t.Fatal("all-same digits should reject")
	}
	if !validatePhone("13800138000") {
		t.Fatal("valid phone rejected")
	}
}

func extractToken(s, cat string) string {
	start := strings.Index(s, "{{"+cat+"_")
	if start < 0 {
		return ""
	}
	end := strings.Index(s[start:], "}}")
	if end < 0 {
		return ""
	}
	return s[start : start+end+2]
}

func TestShouldMaskScopes(t *testing.T) {
	disabled := enabledCfg()
	disabled.Enabled = false
	if NewEngine(disabled).ShouldMask("k", "claude", "oauth") {
		t.Fatal("disabled engine should not mask")
	}

	all := enabledCfg()
	if !NewEngine(all).ShouldMask("", "", "") {
		t.Fatal("scope all should mask")
	}

	empty := enabledCfg()
	empty.Scope = config.DesensitizationScopeTargeted
	if NewEngine(empty).ShouldMask("k", "claude", "oauth") {
		t.Fatal("targeted empty should not mask")
	}

	keyHit := enabledCfg()
	keyHit.Scope = config.DesensitizationScopeTargeted
	keyHit.APIKeys = []string{"client-a"}
	if !NewEngine(keyHit).ShouldMask("client-a", "", "") {
		t.Fatal("targeted key hit should mask")
	}
	if NewEngine(keyHit).ShouldMask("client-b", "", "") {
		t.Fatal("targeted key miss should not mask")
	}

	oauth := enabledCfg()
	oauth.Scope = config.DesensitizationScopeTargeted
	oauth.OAuthProviders = []string{"claude"}
	if !NewEngine(oauth).ShouldMask("", "CLAUDE", "oauth") {
		t.Fatal("targeted oauth hit should mask")
	}
	if NewEngine(oauth).ShouldMask("", "codex", "oauth") {
		t.Fatal("targeted oauth miss should not mask")
	}

	api := enabledCfg()
	api.Scope = config.DesensitizationScopeTargeted
	api.APIProviders = []string{"gemini"}
	if !NewEngine(api).ShouldMask("", "gemini", "apikey") {
		t.Fatal("targeted api-provider hit should mask")
	}
	if NewEngine(api).ShouldMask("", "claude", "apikey") {
		t.Fatal("targeted api-provider miss should not mask")
	}

	mix := enabledCfg()
	mix.Scope = config.DesensitizationScopeTargeted
	mix.APIKeys = []string{"priv"}
	mix.APIProviders = []string{"xai"}
	if !NewEngine(mix).ShouldMask("priv", "", "") || !NewEngine(mix).ShouldMask("nope", "xai", "apikey") {
		t.Fatal("mixture should be OR")
	}
}

func TestFailClosedRejectsMaskFailureOnJSON(t *testing.T) {
	cfg := enabledCfg()
	cfg.FailClosed = true
	eng := NewEngine(cfg)
	eng.SetTestMaskFailure(fmt.Errorf("store boom"))
	raw := []byte(`{"text":"hello 13800138000"}`)
	out, err := eng.MaskJSONBody("s", "m", "m", "openai", raw)
	if err == nil {
		t.Fatal("expected fail_closed error")
	}
	if out != nil {
		t.Fatalf("expected nil body on fail_closed, got %s", out)
	}
}

func TestFailClosedPassesNonJSON(t *testing.T) {
	cfg := enabledCfg()
	cfg.FailClosed = true
	eng := NewEngine(cfg)
	eng.SetTestMaskFailure(fmt.Errorf("should not matter"))
	raw := []byte("not-json-at-all 13800138000")
	out, err := eng.MaskJSONBody("s", "m", "m", "openai", raw)
	if err != nil {
		t.Fatalf("non-JSON must not fail closed: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("expected passthrough, got %s", out)
	}
}

func TestFailClosedOpenPassesOnMaskFailure(t *testing.T) {
	cfg := enabledCfg()
	cfg.FailClosed = false
	eng := NewEngine(cfg)
	eng.SetTestMaskFailure(fmt.Errorf("store boom"))
	raw := []byte(`{"text":"hello"}`)
	out, err := eng.MaskJSONBody("s", "m", "m", "openai", raw)
	if err != nil {
		t.Fatalf("fail_closed off should not error: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("expected original body, got %s", out)
	}
}

func TestAllowlistExactHitVsNearMiss(t *testing.T) {
	cfg := enabledCfg()
	cfg.Allowlist = []string{"user@example.com", "sk-abcdefghijklmnopqrstuvwxyz012345"}
	eng := NewEngine(cfg)
	sid := "allow-1"
	kept := eng.maskText(sid, "mail user@example.com please", nil)
	if !strings.Contains(kept, "user@example.com") {
		t.Fatalf("exact allowlist email should be kept: %q", kept)
	}
	// Near-miss: matched value differs by even one character → must mask.
	miss := eng.maskText(sid, "mail user@example.org please", nil)
	if strings.Contains(miss, "user@example.org") {
		t.Fatalf("near-miss email should be masked: %q", miss)
	}
	keyKept := eng.maskText(sid, "key sk-abcdefghijklmnopqrstuvwxyz012345", nil)
	if !strings.Contains(keyKept, "sk-abcdefghijklmnopqrstuvwxyz012345") {
		t.Fatalf("exact allowlisted API key should be kept: %q", keyKept)
	}
	keyNear := eng.maskText(sid, "key sk-abcdefghijklmnopqrstuvwxyz012345X", nil)
	if strings.Contains(keyNear, "sk-abcdefghijklmnopqrstuvwxyz012345X") {
		t.Fatalf("near-miss API key should be masked: %q", keyNear)
	}
}

func TestSecretPrefixExtensionsAndBoundaries(t *testing.T) {
	eng := NewEngine(enabledCfg())
	sid := "pref"
	cases := []struct {
		in      string
		wantTok bool
		label   string
	}{
		{"sk-ant-abcdefghijklmnopqrstuvwxyz0123", true, "sk-ant-"},
		{"glpat-abcdefghijklmnopqrstuvwxyz0123", true, "glpat-"},
		{"npm_abcdefghijklmnopqrstuvwxyz012345", true, "npm_"},
		{"xoxb-abcdefghijklmnopqrstuvwxyz0123", true, "xoxb-"},
		{"xoxp-abcdefghijklmnopqrstuvwxyz0123", true, "xoxp-"},
		{"pk-live-abcdefghijklmnopqrstuvwxyz", true, "pk-live-"},
		{"pk-test-abcdefghijklmnopqrstuvwxyz", true, "pk-test-"},
		{"rk-abcdefghijklmnopqrstuvwxyz012345", true, "rk-"},
		{"task-abcdefghijklmnopqrstuvwxyz0123", false, "task- false positive"},
		{"mysk-abcdefghijklmnopqrstuvwxyz0123", false, "mysk false positive"},
		{"flask-sk-abcdefghijklmnopqrstuvwxyz0123", false, "embedded sk after -"},
	}
	for _, tc := range cases {
		out := eng.maskText(sid, tc.in, nil)
		has := strings.Contains(out, "{{API_KEY_")
		if has != tc.wantTok {
			t.Fatalf("%s: wantTok=%v got %q", tc.label, tc.wantTok, out)
		}
	}
}

func TestCNSecretAssignmentKeywords(t *testing.T) {
	cfg := enabledCfg()
	on := true
	cfg.Categories.SecretAssignment = &on
	eng := NewEngine(cfg)
	sid := "cn"
	for _, in := range []string{"口令: hunter2xx", "登录密码： hunter2xx"} {
		out := eng.maskText(sid, in, nil)
		if strings.Contains(out, "hunter2xx") || !strings.Contains(out, "{{SECRET_ASSIGNMENT_") {
			t.Fatalf("expected CN secret assignment masked, got %q from %q", out, in)
		}
	}
}

func TestJWTValidationStillRequired(t *testing.T) {
	cfg := enabledCfg()
	on := true
	cfg.Categories.JWT = &on
	eng := NewEngine(cfg)
	sid := "jwt"
	// Header decodes to JSON without "alg" → must not mask.
	fake := "eyJzdWIiOiIxMjM0NTY3ODkwIn0.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signaturepartxxxxx"
	if validateJWT(fake) {
		t.Fatal("test setup: fake without alg must not validate")
	}
	out := eng.maskText(sid, "token "+fake, nil)
	if strings.Contains(out, "{{JWT_") {
		t.Fatalf("invalid JWT must not be masked: %q", out)
	}
	// Real-shaped header with alg
	header := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" // {"alg":"HS256","typ":"JWT"}
	validish := header + ".eyJzdWIiOiIxMjM0NTY3ODkwIn0.signaturepartxxxxx"
	if !validateJWT(validish) {
		t.Fatal("expected validateJWT to accept alg header")
	}
	out = eng.maskText(sid, "token "+validish, nil)
	if !strings.Contains(out, "{{JWT_") {
		t.Fatalf("valid JWT shape should mask: %q", out)
	}
}
