package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexAdaptivePrioritySpillsWhenBusy(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{
		Routing: internalconfig.CodexRoutingConfig{Strategy: "adaptive"},
	}})

	high := &Auth{ID: "adaptive-high", Provider: "codex", Attributes: map[string]string{"priority": "100"}}
	low := &Auth{ID: "adaptive-low", Provider: "codex", Attributes: map[string]string{"priority": "50"}}
	for _, auth := range []*Auth{high, low} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}

	selectedOpts := make([]cliproxyexecutor.Options, 0, 3)
	for i := 0; i < 2; i++ {
		opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
		selected, _, err := manager.pickNext(context.Background(), "codex", "", opts, nil)
		if err != nil || selected == nil {
			t.Fatalf("pick #%d: selected=%v err=%v", i+1, selected, err)
		}
		if selected.ID != high.ID {
			t.Fatalf("pick #%d = %q, want high-priority account", i+1, selected.ID)
		}
		selectedOpts = append(selectedOpts, opts)
	}

	third := cliproxyexecutor.Options{Metadata: map[string]any{}}
	selected, _, err := manager.pickNext(context.Background(), "codex", "", third, nil)
	if err != nil || selected == nil {
		t.Fatalf("spill pick: selected=%v err=%v", selected, err)
	}
	if selected.ID != low.ID {
		t.Fatalf("spill pick = %q, want low-priority fallback", selected.ID)
	}
	selectedOpts = append(selectedOpts, third)

	for i, opts := range selectedOpts {
		manager.MarkResult(context.Background(), Result{AuthID: []string{high.ID, high.ID, low.ID}[i], Provider: "codex", Success: true, Options: opts})
	}
}

func TestCodexAdaptive429ReducesCapacity(t *testing.T) {
	router := newCodexAdaptiveRouter()
	auth := &Auth{ID: "adaptive-429", Provider: "codex"}

	for i := 0; i < codexAdaptiveDefaultConcurrency; i++ {
		if _, lease, err := router.pick(context.Background(), []*Auth{auth}, false); err != nil {
			t.Fatalf("pick #%d: %v", i+1, err)
		} else {
			router.observe(Result{AuthID: auth.ID, Provider: "codex", Error: &Error{HTTPStatus: http.StatusTooManyRequests}, Options: cliproxyexecutor.Options{Metadata: map[string]any{adaptiveLeaseMetadataKey: lease, adaptiveExecutionMetadataKey: true}}})
			break
		}
	}

	if got := router.accountSnapshot(auth.ID).limit; got != 1 {
		t.Fatalf("capacity after 429 = %d, want 1", got)
	}
}

func TestCodexAdaptiveResultReleasesOnlyItsLease(t *testing.T) {
	router := newCodexAdaptiveRouter()
	auth := &Auth{ID: "adaptive-lease-match", Provider: "codex"}
	_, firstLease, err := router.pick(context.Background(), []*Auth{auth}, false)
	if err != nil {
		t.Fatalf("first pick: %v", err)
	}
	_, secondLease, err := router.pick(context.Background(), []*Auth{auth}, false)
	if err != nil {
		t.Fatalf("second pick: %v", err)
	}
	resultOptions := cliproxyexecutor.Options{Metadata: map[string]any{
		adaptiveExecutionMetadataKey: true,
		adaptiveLeaseMetadataKey:     secondLease,
	}}
	router.observe(Result{AuthID: auth.ID, Provider: "codex", Success: true, Options: resultOptions})
	if got := router.accountSnapshot(auth.ID).inFlight; got != 1 {
		t.Fatalf("in-flight after matching result = %d, want 1", got)
	}
	if _, ok := router.leases[firstLease]; !ok {
		t.Fatal("matching result released the wrong lease")
	}
	if _, ok := router.leases[secondLease]; ok {
		t.Fatal("matching result did not release its lease")
	}
	router.release(firstLease)
}

func TestCodexAdaptiveCancellationReleasesLease(t *testing.T) {
	router := newCodexAdaptiveRouter()
	ctx, cancel := context.WithCancel(context.Background())
	auth := &Auth{ID: "adaptive-cancel", Provider: "codex"}
	_, lease, err := router.pick(ctx, []*Auth{auth}, false)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got := router.accountSnapshot(auth.ID).inFlight; got != 1 {
		t.Fatalf("in-flight after pick = %d, want 1", got)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := router.accountSnapshot(auth.ID).inFlight; got == 0 {
			if _, ok := router.leases[lease]; ok {
				t.Fatal("canceled lease remained in lease map")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("canceled lease was not released")
}

func TestCodexAdaptiveProbeFailureCanRetry(t *testing.T) {
	router := newCodexAdaptiveRouter()
	auth := &Auth{ID: "adaptive-probe-retry", Provider: "codex"}
	now := time.Now()
	if !router.beginProbe(auth.ID, now) {
		t.Fatal("beginProbe() = false, want true")
	}
	router.finishProbe(auth.ID, false)
	state := router.accountSnapshot(auth.ID)
	if quotaProbeNeeded(auth, &state, time.Now()) {
		t.Fatal("failed probe should briefly back off the next probe")
	}
	if !quotaProbeNeeded(auth, &state, time.Now().Add(codexAdaptiveProbeFailureBackoff+time.Second)) {
		t.Fatal("failed probe backoff did not expire")
	}
}

func TestCodexAdaptiveSuccessRestoresCapacity(t *testing.T) {
	router := newCodexAdaptiveRouter()
	auth := &Auth{ID: "adaptive-success", Provider: "codex"}
	router.mu.Lock()
	router.accountLocked(auth.ID).limit = 1
	router.mu.Unlock()

	for i := 0; i < codexAdaptiveSuccessesBeforeIncrease; i++ {
		selected, lease, err := router.pick(context.Background(), []*Auth{auth}, false)
		if err != nil || selected == nil {
			t.Fatalf("pick #%d: selected=%v err=%v", i+1, selected, err)
		}
		router.observe(Result{AuthID: auth.ID, Provider: "codex", Success: true, Options: cliproxyexecutor.Options{Metadata: map[string]any{adaptiveLeaseMetadataKey: lease, adaptiveExecutionMetadataKey: true}}})
	}
	if got := router.accountSnapshot(auth.ID).limit; got != 2 {
		t.Fatalf("capacity after successes = %d, want 2", got)
	}
}

func TestCodexAdaptiveQuotaUrgencyUsesExpiryAndWeeklyWindow(t *testing.T) {
	now := time.Now()
	soon := &Auth{
		ID:       "adaptive-soon",
		Provider: "codex",
		Metadata: map[string]any{"chatgpt_subscription_active_until": now.Add(48 * time.Hour).Format(time.RFC3339)},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Secondary-Used-Percent":        "50",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "86400",
		}},
	}
	later := soon.Clone()
	later.ID = "adaptive-later"
	later.Metadata["chatgpt_subscription_active_until"] = now.Add(7 * 24 * time.Hour).Format(time.RFC3339)
	later.Quota.Signals["X-Codex-Secondary-Reset-After-Seconds"] = "604800"

	if got, want := codexQuotaUrgency(soon, now) > codexQuotaUrgency(later, now), true; got != want {
		t.Fatalf("soon urgency comparison = %v, want %v", got, want)
	}
}

func TestCodexAdaptiveQuotaUrgencyAccountsForAvailableReset(t *testing.T) {
	now := time.Now()
	base := &Auth{ID: "adaptive-base", Provider: "codex", Metadata: map[string]any{
		"chatgpt_subscription_active_until": now.Add(48 * time.Hour).Format(time.RFC3339),
	}, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "50", "X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-After-Seconds": "86400",
	}}}
	withReset := base.Clone()
	withReset.ID = "adaptive-reset"
	withReset.Metadata["rate_limit_reset_credits_applicable_available_count"] = 1
	if codexQuotaUrgency(withReset, now) <= codexQuotaUrgency(base, now) {
		t.Fatal("available reset credit did not increase burn urgency")
	}
}

func TestCodexAdaptiveQuotaUrgencyUsesResetCreditExpiry(t *testing.T) {
	now := time.Now()
	base := &Auth{ID: "adaptive-credit-base", Provider: "codex", Metadata: map[string]any{
		"chatgpt_subscription_active_until": now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
	}, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Primary-Used-Percent": "50", "X-Codex-Primary-Window-Minutes": "10080", "X-Codex-Primary-Reset-After-Seconds": "604800",
	}}}
	withCredit := base.Clone()
	withCredit.ID = "adaptive-credit-soon"
	withCredit.Metadata["rate_limit_reset_credits"] = []any{map[string]any{"status": "available", "expires_at": now.Add(24 * time.Hour).Format(time.RFC3339)}}
	if codexQuotaUrgency(withCredit, now) <= codexQuotaUrgency(base, now) {
		t.Fatal("expiring reset credit did not increase burn urgency")
	}
}

type adaptiveProbeExecutor struct {
	schedulerTestExecutor
	calls chan struct{}
}

func (e *adaptiveProbeExecutor) Identifier() string { return "codex" }

func (e *adaptiveProbeExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	e.calls <- struct{}{}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"plan_type":"plus","rate_limit":{"secondary":{"used_percent":63,"window_minutes":10080,"reset_after_seconds":3600}}}`)),
	}, nil
}

func TestCodexAdaptiveQuotaProbeIsLazyAndPaidOnly(t *testing.T) {
	executor := &adaptiveProbeExecutor{calls: make(chan struct{}, 4)}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Routing: internalconfig.CodexRoutingConfig{Strategy: "adaptive"}}})
	paid := &Auth{ID: "adaptive-probe-paid", Provider: "codex"}
	free := &Auth{ID: "adaptive-probe-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}}
	manager.Register(context.Background(), paid)
	manager.Register(context.Background(), free)
	if !manager.codexAdaptiveEnabled() {
		t.Fatal("adaptive strategy is not enabled")
	}
	if !quotaProbeNeeded(paid, &codexAdaptiveAccount{}, time.Now()) {
		t.Fatal("fresh paid auth should need a first probe")
	}

	manager.refreshCodexAdaptiveQuota(context.Background(), "", cliproxyexecutor.Options{}, map[string]struct{}{})
	select {
	case <-executor.calls:
	case <-time.After(time.Second):
		t.Fatal("paid quota probe did not start")
	}
	manager.refreshCodexAdaptiveQuota(context.Background(), "", cliproxyexecutor.Options{}, map[string]struct{}{})
	select {
	case <-executor.calls:
		t.Fatal("paid quota probe was repeated inside the probe interval")
	case <-time.After(20 * time.Millisecond):
	}
	if got, ok := manager.GetByID(paid.ID); !ok || got == nil || got.Quota.Signals["X-Codex-Secondary-Used-Percent"] != "63" {
		t.Fatalf("paid quota was not observed: %#v", got)
	}

	freeManager := NewManager(nil, &RoundRobinSelector{}, nil)
	freeExecutor := &adaptiveProbeExecutor{calls: make(chan struct{}, 1)}
	freeManager.RegisterExecutor(freeExecutor)
	freeManager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{Routing: internalconfig.CodexRoutingConfig{Strategy: "adaptive"}}})
	freeManager.Register(context.Background(), &Auth{ID: "adaptive-only-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}})
	freeManager.refreshCodexAdaptiveQuota(context.Background(), "", cliproxyexecutor.Options{}, map[string]struct{}{})
	select {
	case <-freeExecutor.calls:
		t.Fatal("free account was proactively probed")
	case <-time.After(20 * time.Millisecond):
	}
}

type adaptiveResetCreditProbeExecutor struct {
	schedulerTestExecutor
	requests chan *http.Request
}

func (adaptiveResetCreditProbeExecutor) Identifier() string { return "codex" }

func (e adaptiveResetCreditProbeExecutor) HttpRequest(_ context.Context, _ *Auth, req *http.Request) (*http.Response, error) {
	if e.requests != nil {
		e.requests <- req.Clone(req.Context())
	}
	body := `{"rate_limits":{"secondary":{"used_percent":63,"window_minutes":10080,"reset_after_seconds":3600}}}`
	if strings.HasSuffix(req.URL.Path, "/rate-limit-reset-credits") {
		body = `{"credits":[{"status":"available","expires_at":"2099-01-01T00:00:00Z"}],"available_count":1}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestCodexQuotaProbeInitializesMetadataBeforeMergingResetCredits(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	requests := make(chan *http.Request, 2)
	manager.RegisterExecutor(adaptiveResetCreditProbeExecutor{
		schedulerTestExecutor: schedulerTestExecutor{provider: "codex"},
		requests:              requests,
	})
	auth := &Auth{
		ID:       "adaptive-reset-credit-map",
		Provider: "codex",
		Metadata: map[string]any{
			"account_id": "acct-adaptive",
			"rate_limit_reset_credits_applicable_available_count": 1,
			"rate_limit_reset_credits":                            []any{map[string]any{"status": "available"}},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register: %v", err)
	}

	updated, err := manager.probeCodexQuota(context.Background(), auth)
	if err != nil {
		t.Fatalf("probeCodexQuota: %v", err)
	}
	if updated == nil || updated.Metadata["rate_limit_reset_credits_available_count"] != int64(1) {
		t.Fatalf("reset-credit metadata was not merged: %#v", updated)
	}
	usageRequest := <-requests
	if got := usageRequest.Header.Get("ChatGPT-Account-ID"); got != "acct-adaptive" {
		t.Fatalf("usage probe account header = %q, want acct-adaptive", got)
	}
	if usageRequest.Header.Get("OpenAI-Beta") != "" || usageRequest.Header.Get("Originator") != "" {
		t.Fatal("usage probe unexpectedly sent reset-credit headers")
	}
	resetRequest := <-requests
	if got := resetRequest.Header.Get("ChatGPT-Account-ID"); got != "acct-adaptive" {
		t.Fatalf("reset-credit probe account header = %q, want acct-adaptive", got)
	}
	if resetRequest.Header.Get("OpenAI-Beta") != "codex-1" || resetRequest.Header.Get("Originator") != "Codex Desktop" {
		t.Fatalf("reset-credit probe headers = %#v", resetRequest.Header)
	}
}

func TestCodexQuotaHeadersFromUsage(t *testing.T) {
	headers := codexQuotaHeadersFromUsage([]byte(`{
		"plan_type":"plus",
		"rate_limits":{"secondary":{"used_percent":63,"window_minutes":10080,"reset_after_seconds":3600}}
	}`))
	if headers.Get("X-Codex-Plan-Type") != "plus" ||
		headers.Get("X-Codex-Secondary-Used-Percent") != "63" ||
		headers.Get("X-Codex-Secondary-Reset-After-Seconds") != "3600" {
		t.Fatalf("unexpected usage headers: %#v", headers)
	}
}

func TestCodexAdaptiveIgnoresUnmarkedCodexResults(t *testing.T) {
	router := newCodexAdaptiveRouter()
	auth := &Auth{ID: "adaptive-unmarked", Provider: "codex"}
	router.observe(Result{AuthID: auth.ID, Provider: "codex", Success: true})
	state := router.accountSnapshot(auth.ID)
	if state.inFlight != 0 || state.successes != 0 || state.limit != codexAdaptiveDefaultConcurrency {
		t.Fatalf("unmarked result changed adaptive state: %#v", state)
	}
}

func TestCodexAdaptiveDisabledUsesGlobalScheduler(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	manager.SetConfig(&internalconfig.Config{})
	for _, auth := range []*Auth{
		{ID: "global-first", Provider: "codex"},
		{ID: "global-second", Provider: "codex"},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	first, _, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if err != nil || first == nil {
		t.Fatalf("first pick: auth=%v err=%v", first, err)
	}
	second, _, err := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, map[string]struct{}{first.ID: {}})
	if err != nil || second == nil {
		t.Fatalf("second pick: auth=%v err=%v", second, err)
	}
	if first.ID != "global-first" || second.ID != "global-second" {
		t.Fatalf("global fill-first changed: %q then %q", first.ID, second.ID)
	}
}

func TestCodexAdaptiveDoesNotMarkMixedProviderRouting(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	manager.RegisterExecutor(schedulerTestExecutor{provider: "gemini"})
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{
		Routing: internalconfig.CodexRoutingConfig{Strategy: "adaptive"},
	}})
	for _, auth := range []*Auth{
		{ID: "adaptive-mixed-codex", Provider: "codex"},
		{ID: "adaptive-mixed-gemini", Provider: "gemini"},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
	selected, _, _, err := manager.pickNextMixed(context.Background(), []string{"codex", "gemini"}, "", opts, nil)
	if err != nil || selected == nil {
		t.Fatalf("mixed pick: auth=%v err=%v", selected, err)
	}
	if adaptiveLeaseID(opts) != "" {
		t.Fatalf("mixed-provider global routing acquired adaptive lease: %#v", opts.Metadata)
	}
}

func TestCodexAdaptiveDoesNotChangeGeminiRouting(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "gemini"})
	manager.SetConfig(&internalconfig.Config{Codex: internalconfig.CodexConfig{
		Routing: internalconfig.CodexRoutingConfig{Strategy: "adaptive"},
	}})
	for _, auth := range []*Auth{
		{ID: "adaptive-gemini-a", Provider: "gemini"},
		{ID: "adaptive-gemini-b", Provider: "gemini"},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	first, _, err := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if err != nil || first == nil {
		t.Fatalf("first Gemini pick: auth=%v err=%v", first, err)
	}
	second, _, err := manager.pickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if err != nil || second == nil {
		t.Fatalf("second Gemini pick: auth=%v err=%v", second, err)
	}
	if first.ID == second.ID {
		t.Fatalf("Gemini round-robin was replaced by adaptive routing: %q then %q", first.ID, second.ID)
	}
}

func TestCodexAdaptiveExpiredSubscriptionIsMostUrgent(t *testing.T) {
	now := time.Now()
	due := &Auth{ID: "due", Provider: "codex", Metadata: map[string]any{"expires": now.Add(-24 * time.Hour).Format(time.RFC3339)}}
	soon := &Auth{ID: "soon", Provider: "codex", Metadata: map[string]any{"expires": now.Add(3 * 24 * time.Hour).Format(time.RFC3339)}}
	far := &Auth{ID: "far", Provider: "codex", Metadata: map[string]any{"expires": now.Add(10 * 24 * time.Hour).Format(time.RFC3339)}}
	got := newCodexAdaptiveRouter().best([]*Auth{soon, far, due}, false, now)
	if got != due {
		t.Fatalf("selected %q, want due account", got.ID)
	}
}
