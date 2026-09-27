package desensitization

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const tokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// Hit summarizes mask matches for preview.
type Hit struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// Engine is the process-wide desensitization engine (in-memory session maps only).
type Engine struct {
	mu          sync.RWMutex
	cfg         config.DesensitizationConfig
	sessions    map[string]*sessionState
	streams     map[string]*streamBuffer // key: sessionID + "\x00" + streamID
	customs     []*regexp.Regexp
	customCat   []string
	prefixRes   []*regexp.Regexp
	allowExact  map[string]struct{} // exact allowlist values (O(1))
	testMaskErr error               // test-only: simulate mask/store failure
}

type sessionState struct {
	mu      sync.Mutex
	fwd     map[string]string // original -> token
	rev     map[string]string // token -> original
	labels  map[string]string // token -> category
	expires time.Time
}

type streamBuffer struct {
	mu      sync.Mutex
	pending string
	expires time.Time
}

var (
	runtimeMu sync.RWMutex
	runtime   *Engine
)

// Configure replaces the process-wide engine configuration. Sessions are preserved
// when only knobs change; a nil/disabled config still installs a no-op engine.
func Configure(cfg config.DesensitizationConfig) *Engine {
	cfg = config.NormalizeDesensitizationConfig(cfg)
	eng := newEngine(cfg)

	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if runtime != nil {
		// Keep existing session mappings across soft reloads.
		eng.sessions = runtime.sessions
		eng.streams = runtime.streams
	}
	runtime = eng
	return eng
}

// Current returns the active engine (never nil after first Configure; may be nil before).
func Current() *Engine {
	runtimeMu.RLock()
	defer runtimeMu.RUnlock()
	return runtime
}

// Enabled reports whether the master switch is on (not whether a given request applies).
func Enabled() bool {
	e := Current()
	return e != nil && e.cfg.Enabled
}

// ShouldMask reports whether this request should be masked given inbound key and selected credential.
func (e *Engine) ShouldMask(clientAPIKey, provider, authKind string) bool {
	if e == nil {
		return false
	}
	return e.cfg.Applies(clientAPIKey, provider, authKind)
}

// NewEngine builds an engine from config without installing it process-wide.
func NewEngine(cfg config.DesensitizationConfig) *Engine {
	cfg = config.NormalizeDesensitizationConfig(cfg)
	return newEngine(cfg)
}

func newEngine(cfg config.DesensitizationConfig) *Engine {
	e := &Engine{
		cfg:        cfg,
		sessions:   make(map[string]*sessionState),
		streams:    make(map[string]*streamBuffer),
		allowExact: make(map[string]struct{}, len(cfg.Allowlist)),
	}
	for _, v := range cfg.Allowlist {
		if v == "" {
			continue
		}
		e.allowExact[v] = struct{}{}
	}
	for _, r := range cfg.CustomRegex {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			continue
		}
		e.customs = append(e.customs, re)
		cat := strings.ToUpper(strings.TrimSpace(r.Category))
		if cat == "" {
			cat = "CUSTOM"
		}
		e.customCat = append(e.customCat, cat)
	}
	for _, p := range cfg.SecretPrefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		escaped := regexp.QuoteMeta(p)
		// RE2 has no lookaround; match prefix+body and trim word boundaries in findSpans.
		re, err := regexp.Compile(`(?i)` + escaped + `[A-Za-z0-9_\-]{16,}`)
		if err != nil {
			continue
		}
		e.prefixRes = append(e.prefixRes, re)
	}
	go e.sweeper()
	return e
}

func (e *Engine) sweeper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		e.prune()
	}
}

func (e *Engine) prune() {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, s := range e.sessions {
		s.mu.Lock()
		exp := s.expires
		s.mu.Unlock()
		if now.After(exp) {
			delete(e.sessions, id)
		}
	}
	for id, s := range e.streams {
		s.mu.Lock()
		exp := s.expires
		s.mu.Unlock()
		if now.After(exp) {
			delete(e.streams, id)
		}
	}
}

func (e *Engine) ttl() time.Duration {
	mins := e.cfg.SessionTTLMinutesValue()
	return time.Duration(mins) * time.Minute
}

func (e *Engine) Config() config.DesensitizationConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

func (e *Engine) isAllowlisted(value string) bool {
	if e == nil || len(e.allowExact) == 0 || value == "" {
		return false
	}
	_, ok := e.allowExact[value]
	return ok
}

// SetTestMaskFailure injects a mask/store error for tests only.
func (e *Engine) SetTestMaskFailure(err error) {
	if e == nil {
		return
	}
	e.testMaskErr = err
}

func (e *Engine) shouldSkip(model, requestedModel, format string) bool {
	for _, m := range e.cfg.SkipModels {
		if strings.EqualFold(m, model) || strings.EqualFold(m, requestedModel) {
			return true
		}
	}
	for _, f := range e.cfg.SkipFormats {
		if strings.EqualFold(f, format) {
			return true
		}
	}
	return false
}

func (e *Engine) getSession(sessionID string) *sessionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[sessionID]
	if !ok {
		s = &sessionState{
			fwd:     make(map[string]string),
			rev:     make(map[string]string),
			labels:  make(map[string]string),
			expires: time.Now().Add(e.ttl()),
		}
		e.sessions[sessionID] = s
		return s
	}
	s.mu.Lock()
	s.expires = time.Now().Add(e.ttl())
	s.mu.Unlock()
	return s
}

func (e *Engine) tokenFor(sessionID, category, original string) string {
	s := e.getSession(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := s.fwd[original]; ok {
		s.expires = time.Now().Add(e.ttl())
		return tok
	}
	tok := fmt.Sprintf("{{%s_%s}}", category, randomID(6))
	for {
		if _, exists := s.rev[tok]; !exists {
			break
		}
		tok = fmt.Sprintf("{{%s_%s}}", category, randomID(8))
	}
	s.fwd[original] = tok
	s.rev[tok] = original
	s.labels[tok] = category
	s.expires = time.Now().Add(e.ttl())
	return tok
}

func (e *Engine) lookup(sessionID, token string) (string, string, bool) {
	e.mu.RLock()
	s, ok := e.sessions[sessionID]
	e.mu.RUnlock()
	if !ok {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	orig, ok := s.rev[token]
	if !ok {
		return "", "", false
	}
	cat := s.labels[token]
	s.expires = time.Now().Add(e.ttl())
	return orig, cat, true
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = tokenAlphabet[int(b[i])%len(tokenAlphabet)]
	}
	return string(b)
}

// SessionIDFromMetadata picks CanonicalSessionID or falls back to requestID.
func SessionIDFromMetadata(metadata map[string]any, requestID string) string {
	if metadata != nil {
		if v, ok := metadata["canonical_session_id"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return v
			}
		}
	}
	if strings.TrimSpace(requestID) != "" {
		return "req:" + requestID
	}
	return "req:" + randomID(8)
}

// Preview masks text in a throwaway session and returns hits.
func (e *Engine) Preview(text string) (masked string, hits []Hit, err error) {
	if e == nil {
		return text, nil, nil
	}
	sid := "preview:" + randomID(8)
	counts := map[string]int{}
	masked = e.maskText(sid, text, counts)
	for cat, n := range counts {
		hits = append(hits, Hit{Category: cat, Count: n})
	}
	return masked, hits, nil
}

// MaskJSONBody walks JSON text leaves and replaces secrets/PII.
func (e *Engine) MaskJSONBody(sessionID, model, requestedModel, format string, body []byte) ([]byte, error) {
	if e == nil || !e.cfg.Enabled || len(body) == 0 {
		return body, nil
	}
	if e.shouldSkip(model, requestedModel, format) {
		return body, nil
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		// Empty/non-JSON/unrelated formats are never fail-closed — pass through.
		return body, nil
	}
	if e.testMaskErr != nil {
		if e.cfg.FailClosed {
			return nil, e.testMaskErr
		}
		return body, nil
	}
	counts := map[string]int{}
	changed := false
	walkMask(payload, func(s string) string {
		out := e.maskText(sessionID, s, counts)
		if out != s {
			changed = true
		}
		return out
	})
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		if e.cfg.FailClosed {
			return nil, fmt.Errorf("desensitization: remashal failed: %w", err)
		}
		return body, nil
	}
	return out, nil
}

// RestoreJSONBody restores placeholders in a non-stream JSON body.
func (e *Engine) RestoreJSONBody(sessionID string, body []byte) []byte {
	if e == nil || !e.cfg.Enabled || !e.cfg.RestoreEnabled() || len(body) == 0 {
		return body
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		// Not JSON — restore as plain text.
		return []byte(e.restoreText(sessionID, string(body), "", true))
	}
	changed := false
	walkMask(payload, func(s string) string {
		out := e.restoreText(sessionID, s, "", true)
		if out != s {
			changed = true
		}
		return out
	})
	if !changed {
		return body
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// RestoreStreamChunk restores placeholders across SSE/stream chunks with a pending buffer.
func (e *Engine) RestoreStreamChunk(sessionID, streamID string, chunk []byte) []byte {
	if e == nil || !e.cfg.Enabled || !e.cfg.RestoreEnabled() || len(chunk) == 0 {
		return chunk
	}
	key := sessionID + "\x00" + streamID
	buf := e.getStream(key)
	buf.mu.Lock()
	defer buf.mu.Unlock()
	combined := buf.pending + string(chunk)
	confirmed, pending := splitPartialPlaceholder(combined)
	buf.pending = pending
	buf.expires = time.Now().Add(e.ttl())
	if confirmed == "" {
		return nil
	}
	return []byte(e.restoreText(sessionID, confirmed, "", false))
}

// FlushStream emits any remaining pending buffer for a stream.
func (e *Engine) FlushStream(sessionID, streamID string) []byte {
	if e == nil || !e.cfg.Enabled || !e.cfg.RestoreEnabled() {
		return nil
	}
	key := sessionID + "\x00" + streamID
	e.mu.Lock()
	buf, ok := e.streams[key]
	if ok {
		delete(e.streams, key)
	}
	e.mu.Unlock()
	if !ok {
		return nil
	}
	buf.mu.Lock()
	pending := buf.pending
	buf.pending = ""
	buf.mu.Unlock()
	if pending == "" {
		return nil
	}
	return []byte(e.restoreText(sessionID, pending, "", true))
}

func (e *Engine) getStream(key string) *streamBuffer {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.streams[key]
	if !ok {
		s = &streamBuffer{expires: time.Now().Add(e.ttl())}
		e.streams[key] = s
	}
	return s
}

func splitPartialPlaceholder(s string) (confirmed, pending string) {
	if m := rePartialTok.FindStringIndex(s); m != nil && m[1] == len(s) {
		// Hold back only a short incomplete {{... suffix.
		if len(s)-m[0] <= 40 {
			return s[:m[0]], s[m[0]:]
		}
	}
	// Also hold a trailing single '{' that might start a token.
	if strings.HasSuffix(s, "{") && !strings.HasSuffix(s, "{{") {
		return s[:len(s)-1], "{"
	}
	if strings.HasSuffix(s, "{{") {
		return s[:len(s)-2], "{{"
	}
	return s, ""
}

func isSecretCategory(cat string) bool {
	switch strings.ToUpper(cat) {
	case "API_KEY", "TOKEN", "PRIVATE_KEY", "ACCESS_KEY", "JWT", "CONNSTR", "SECRET_ASSIGNMENT", "SECRET":
		return true
	default:
		return false
	}
}
