package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type aliasRoutingExecutor struct {
	id string

	mu             sync.Mutex
	executeModels  []string
	executeAliases []string
}

func (e *aliasRoutingExecutor) Identifier() string { return e.id }

func (e *aliasRoutingExecutor) Execute(ctx context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeModels = append(e.executeModels, req.Model)
	e.executeAliases = append(e.executeAliases, coreusage.RequestedModelAliasFromContext(ctx))
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(req.Model)}, nil
}

func (e *aliasRoutingExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "ExecuteStream not implemented"}
}

func (e *aliasRoutingExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *aliasRoutingExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "CountTokens not implemented"}
}

func (e *aliasRoutingExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

func (e *aliasRoutingExecutor) ExecuteModels() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeModels))
	copy(out, e.executeModels)
	return out
}

func (e *aliasRoutingExecutor) ExecuteAliases() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.executeAliases))
	copy(out, e.executeAliases)
	return out
}

func TestManagerExecute_OAuthAliasBypassesBlockedRouteModel(t *testing.T) {
	const (
		provider    = "antigravity"
		routeModel  = "claude-opus-4-6"
		targetModel = "claude-opus-4-6-thinking"
	)

	manager := NewManager(nil, nil, nil)
	executor := &aliasRoutingExecutor{id: provider}
	manager.RegisterExecutor(executor)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		provider: {{
			Name:  targetModel,
			Alias: routeModel,
			Fork:  true,
		}},
	})

	auth := &Auth{
		ID:       "oauth-alias-auth",
		Provider: provider,
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			routeModel: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: time.Now().Add(1 * time.Hour),
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: routeModel}, {ID: targetModel}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})
	manager.RefreshSchedulerEntry(auth.ID)

	resp, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: routeModel}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute error = %v, want success", errExecute)
	}
	if string(resp.Payload) != targetModel {
		t.Fatalf("execute payload = %q, want %q", string(resp.Payload), targetModel)
	}

	gotModels := executor.ExecuteModels()
	if len(gotModels) != 1 {
		t.Fatalf("execute models len = %d, want 1", len(gotModels))
	}
	if gotModels[0] != targetModel {
		t.Fatalf("execute model = %q, want %q", gotModels[0], targetModel)
	}

	gotAliases := executor.ExecuteAliases()
	if len(gotAliases) != 1 {
		t.Fatalf("execute aliases len = %d, want 1", len(gotAliases))
	}
	if gotAliases[0] != routeModel {
		t.Fatalf("execute alias = %q, want %q", gotAliases[0], routeModel)
	}
}

type stub429CooldownExecutor struct {
	id         string
	retryAfter time.Duration

	mu     sync.Mutex
	models []string
}

func (e *stub429CooldownExecutor) Identifier() string { return e.id }

func (e *stub429CooldownExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.models = append(e.models, req.Model)
	e.mu.Unlock()
	ra := e.retryAfter
	return cliproxyexecutor.Response{}, withTrustedRetryAfter(&Error{
		Code:       "rate_limit_exceeded",
		Message:    "upstream 429 cooldown",
		HTTPStatus: http.StatusTooManyRequests,
	}, &ra)
}

func (e *stub429CooldownExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "ExecuteStream not implemented"}
}

func (e *stub429CooldownExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *stub429CooldownExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{HTTPStatus: http.StatusNotImplemented, Message: "CountTokens not implemented"}
}

func (e *stub429CooldownExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

func (e *stub429CooldownExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.models))
	copy(out, e.models)
	return out
}

func TestManager_Issue7360_Shape1_MixedCaseAliasAndLowercaseModelIDShareCooldown(t *testing.T) {
	const (
		provider       = "deepseek-plugin"
		mixedCaseModel = "DeepSeek-V4.1-flash-sg"
		lowerCaseModel = "deepseek-v4.1-flash-sg"
		aliasModel     = "DeepSeek V4.1 flash"
		authID         = "auth-issue7360-shape1"
	)

	manager := NewManager(nil, nil, nil)
	executor := &stub429CooldownExecutor{id: provider, retryAfter: time.Hour}
	manager.RegisterExecutor(executor)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		provider: {{
			Name:  mixedCaseModel,
			Alias: aliasModel,
			Fork:  true,
		}},
	})

	auth := &Auth{
		ID:       authID,
		Provider: provider,
		Status:   StatusActive,
	}
	if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
		t.Fatalf("register auth: %v", errReg)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{
		{ID: lowerCaseModel},
	})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})
	manager.RefreshSchedulerEntry(authID)

	_, errFirst := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
	if errFirst == nil {
		t.Fatal("expected first Execute to fail with 429")
	}
	if calls := executor.Calls(); len(calls) != 1 || calls[0] != mixedCaseModel {
		t.Fatalf("after first Execute, calls = %v, want [%q]", calls, mixedCaseModel)
	}

	_, errSecond := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: lowerCaseModel}, cliproxyexecutor.Options{})
	if errSecond == nil {
		t.Fatal("expected second Execute to fail due to live cooldown")
	}
	if calls := executor.Calls(); len(calls) != 1 {
		t.Fatalf("expected second Execute to be blocked by live cooldown without calling upstream again, got calls = %v", calls)
	}

	manager.mu.RLock()
	currentAuth := manager.auths[authID]
	manager.mu.RUnlock()
	if len(currentAuth.ModelStates) != 1 {
		t.Fatalf("expected exactly 1 ModelStates cell, got %d: %#v", len(currentAuth.ModelStates), currentAuth.ModelStates)
	}
	if !reg.IsModelSuspendedForClient(authID, lowerCaseModel) {
		t.Fatalf("expected %q to be suspended in registry for %s", lowerCaseModel, authID)
	}
}

func TestManager_Issue7360_Shape2_CooldownRecordedBeforeAliasMappingMigratesToTarget(t *testing.T) {
	const (
		provider       = "deepseek-plugin"
		lowerCaseModel = "deepseek-v4.1-flash-sg"
		aliasModel     = "DeepSeek V4.1 flash"
	)

	t.Run("execute-before-alias-mapping", func(t *testing.T) {
		const authID = "auth-issue7360-shape2-exec"
		manager := NewManager(nil, nil, nil)
		executor := &stub429CooldownExecutor{id: provider, retryAfter: time.Hour}
		manager.RegisterExecutor(executor)

		auth := &Auth{
			ID:       authID,
			Provider: provider,
			Status:   StatusActive,
		}
		if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
			t.Fatalf("register auth: %v", errReg)
		}

		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{
			{ID: lowerCaseModel},
			{ID: aliasModel},
		})
		t.Cleanup(func() {
			reg.UnregisterClient(authID)
		})
		manager.RefreshSchedulerEntry(authID)

		_, errFirst := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
		if errFirst == nil {
			t.Fatal("expected first Execute to fail with 429")
		}
		if calls := executor.Calls(); len(calls) != 1 || calls[0] != aliasModel {
			t.Fatalf("after first Execute, calls = %v, want [%q]", calls, aliasModel)
		}

		manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
			provider: {{
				Name:  lowerCaseModel,
				Alias: aliasModel,
				Fork:  true,
			}},
		})

		_, errSecond := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
		if errSecond == nil {
			t.Fatal("expected second Execute to fail due to live cooldown")
		}
		if calls := executor.Calls(); len(calls) != 1 {
			t.Fatalf("expected second Execute to honour cooldown recorded before alias mapping without calling upstream again, got calls = %v", calls)
		}

		_, errThird := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: lowerCaseModel}, cliproxyexecutor.Options{})
		if errThird == nil {
			t.Fatal("expected third Execute on target model to fail due to live cooldown")
		}
		if calls := executor.Calls(); len(calls) != 1 {
			t.Fatalf("expected third Execute on target model to honour migrated cooldown without calling upstream again, got calls = %v", calls)
		}

		manager.mu.RLock()
		currentAuth := manager.auths[authID]
		manager.mu.RUnlock()
		if len(currentAuth.ModelStates) != 1 {
			t.Fatalf("expected exactly 1 ModelStates cell after alias migration, got %d: %#v", len(currentAuth.ModelStates), currentAuth.ModelStates)
		}
		if _, existsStale := currentAuth.ModelStates[aliasModel]; existsStale {
			t.Fatalf("expected stale route key %q to be migrated away from ModelStates, got %#v", aliasModel, currentAuth.ModelStates)
		}
		if state := currentAuth.ModelStates[lowerCaseModel]; state == nil || !state.Unavailable {
			t.Fatalf("expected target key %q to hold migrated active cooldown, got %#v", lowerCaseModel, state)
		}
	})

	t.Run("preexisting-alias-cooldown-with-only-lowercase-model-registered", func(t *testing.T) {
		const authID = "auth-issue7360-shape2-preexisting"
		manager := NewManager(nil, nil, nil)
		executor := &stub429CooldownExecutor{id: provider, retryAfter: time.Hour}
		manager.RegisterExecutor(executor)

		retryUntil := time.Now().Add(time.Hour)
		auth := &Auth{
			ID:       authID,
			Provider: provider,
			Status:   StatusActive,
			ModelStates: map[string]*ModelState{
				aliasModel: {
					Unavailable:    true,
					Status:         StatusError,
					StatusMessage:  "quota",
					NextRetryAfter: retryUntil,
					Quota: QuotaState{
						Exceeded:      true,
						Reason:        "quota",
						NextRecoverAt: retryUntil,
					},
				},
			},
		}
		if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
			t.Fatalf("register auth: %v", errReg)
		}

		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{
			{ID: lowerCaseModel},
		})
		t.Cleanup(func() {
			reg.UnregisterClient(authID)
		})
		manager.RefreshSchedulerEntry(authID)

		manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
			provider: {{
				Name:  lowerCaseModel,
				Alias: aliasModel,
				Fork:  true,
			}},
		})

		_, errSecond := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
		if errSecond == nil {
			t.Fatal("expected Execute via alias to fail due to live cooldown")
		}
		if calls := executor.Calls(); len(calls) != 0 {
			t.Fatalf("expected Execute via alias to honour pre-existing cooldown without calling upstream, got calls = %v", calls)
		}

		_, errThird := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: lowerCaseModel}, cliproxyexecutor.Options{})
		if errThird == nil {
			t.Fatal("expected Execute on target model to fail due to live cooldown")
		}
		if calls := executor.Calls(); len(calls) != 0 {
			t.Fatalf("expected Execute on target model to honour migrated cooldown without calling upstream, got calls = %v", calls)
		}

		manager.mu.RLock()
		currentAuth := manager.auths[authID]
		manager.mu.RUnlock()
		if len(currentAuth.ModelStates) != 1 {
			t.Fatalf("expected exactly 1 ModelStates cell after alias migration, got %d: %#v", len(currentAuth.ModelStates), currentAuth.ModelStates)
		}
		if _, existsStale := currentAuth.ModelStates[aliasModel]; existsStale {
			t.Fatalf("expected stale route key %q to be migrated away from ModelStates, got %#v", aliasModel, currentAuth.ModelStates)
		}
		if state := currentAuth.ModelStates[lowerCaseModel]; state == nil || !state.Unavailable {
			t.Fatalf("expected target key %q to hold migrated active cooldown, got %#v", lowerCaseModel, state)
		}
		if !reg.IsModelSuspendedForClient(authID, lowerCaseModel) {
			t.Fatalf("expected %q to be suspended in registry for %s", lowerCaseModel, authID)
		}
	})

	t.Run("inflight-alias-request-completes-after-alias-mapping", func(t *testing.T) {
		const authID = "auth-issue7360-shape2-inflight"
		manager := NewManager(nil, nil, nil)

		startedCh := make(chan struct{})
		releaseCh := make(chan struct{})
		var callsMu sync.Mutex
		var calls []string
		retryAfter := time.Hour

		manager.RegisterExecutor(&customStreamMockExecutor{
			identifier: provider,
			mockCustomErrorExecutor: mockCustomErrorExecutor{
				executeFn: func(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
					callsMu.Lock()
					callNum := len(calls) + 1
					calls = append(calls, req.Model)
					callsMu.Unlock()
					if callNum == 1 {
						close(startedCh)
						<-releaseCh
					}
					return cliproxyexecutor.Response{}, withTrustedRetryAfter(&Error{
						Code:       "rate_limit_exceeded",
						Message:    "upstream 429 cooldown",
						HTTPStatus: http.StatusTooManyRequests,
					}, &retryAfter)
				},
			},
		})

		auth := &Auth{
			ID:       authID,
			Provider: provider,
			Status:   StatusActive,
		}
		if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
			t.Fatalf("register auth: %v", errReg)
		}

		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{
			{ID: lowerCaseModel},
			{ID: aliasModel},
		})
		t.Cleanup(func() {
			reg.UnregisterClient(authID)
		})
		manager.RefreshSchedulerEntry(authID)

		doneCh := make(chan error, 1)
		go func() {
			_, errExec := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
			doneCh <- errExec
		}()

		<-startedCh
		manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
			provider: {{
				Name:  lowerCaseModel,
				Alias: aliasModel,
				Fork:  true,
			}},
		})
		close(releaseCh)

		if errFirst := <-doneCh; errFirst == nil {
			t.Fatal("expected in-flight Execute to fail with 429")
		}

		_, errSecond := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: lowerCaseModel}, cliproxyexecutor.Options{})
		if errSecond == nil {
			t.Fatal("expected Execute on target model to fail due to live cooldown")
		}
		_, errThird := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
		if errThird == nil {
			t.Fatal("expected Execute via alias to fail due to live cooldown")
		}

		callsMu.Lock()
		gotCalls := append([]string(nil), calls...)
		callsMu.Unlock()
		if len(gotCalls) != 1 {
			t.Fatalf("expected only 1 upstream call, got %v", gotCalls)
		}

		manager.mu.RLock()
		currentAuth := manager.auths[authID]
		manager.mu.RUnlock()
		if len(currentAuth.ModelStates) != 1 {
			t.Fatalf("expected exactly 1 ModelStates cell, got %d: %#v", len(currentAuth.ModelStates), currentAuth.ModelStates)
		}
		if _, existsStale := currentAuth.ModelStates[aliasModel]; existsStale {
			t.Fatalf("expected stale route key %q not to exist in ModelStates, got %#v", aliasModel, currentAuth.ModelStates)
		}
		if state := currentAuth.ModelStates[lowerCaseModel]; state == nil || !state.Unavailable {
			t.Fatalf("expected target key %q to hold active cooldown, got %#v", lowerCaseModel, state)
		}
	})

	t.Run("chained-alias-with-only-intermediate-target-registered-preserves-cooldown", func(t *testing.T) {
		const (
			authID  = "auth-issue7360-chained-intermediate"
			routeB  = "DeepSeek V4.1 flash"
			targetA = "deepseek-v4.1-flash-sg"
			targetC = "deepseek-v4.1-flash-global"
		)
		manager := NewManager(nil, nil, nil)
		executor := &stub429CooldownExecutor{id: provider, retryAfter: time.Hour}
		manager.RegisterExecutor(executor)

		aliases := map[string][]internalconfig.OAuthModelAlias{
			provider: {
				{Name: targetA, Alias: routeB, Fork: true},
				{Name: targetC, Alias: targetA, Fork: true},
			},
		}
		manager.SetOAuthModelAlias(aliases)

		auth := &Auth{
			ID:       authID,
			Provider: provider,
			Status:   StatusActive,
		}
		if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
			t.Fatalf("register auth: %v", errReg)
		}

		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{
			{ID: targetA},
		})
		t.Cleanup(func() {
			reg.UnregisterClient(authID)
		})
		manager.RefreshSchedulerEntry(authID)

		_, errFirst := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: routeB}, cliproxyexecutor.Options{})
		if errFirst == nil {
			t.Fatal("expected first Execute for routeB to fail with 429")
		}
		if calls := executor.Calls(); len(calls) != 1 || calls[0] != targetA {
			t.Fatalf("after first Execute, calls = %v, want [%q]", calls, targetA)
		}

		// Re-applying alias configuration or reconciling must not migrate targetA's cooldown to targetC
		manager.SetOAuthModelAlias(aliases)
		manager.ReconcileRegistryModelStates(context.Background(), authID)

		_, errSecond := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: routeB}, cliproxyexecutor.Options{})
		if errSecond == nil {
			t.Fatal("expected second Execute for routeB to be blocked by cooldown on targetA")
		}
		if calls := executor.Calls(); len(calls) != 1 {
			t.Fatalf("expected second Execute for routeB not to call upstream again, got calls = %v", calls)
		}
	})

	t.Run("atomic-multi-auth-migration-while-first-auth-save-is-blocked", func(t *testing.T) {
		const (
			authID1 = "auth-issue7360-atomic-1"
			authID2 = "auth-issue7360-atomic-2"
		)
		saveBlockedCh := make(chan struct{})
		saveReleaseCh := make(chan struct{})
		var releaseOnce sync.Once
		releaseSave := func() { releaseOnce.Do(func() { close(saveReleaseCh) }) }
		t.Cleanup(releaseSave)
		var blockSaves sync.Once
		var shouldBlockSaves sync.Mutex
		blockEnabled := false

		store := &blockingSelectiveAuthStore{
			onSave: func(_ *Auth) {
				shouldBlockSaves.Lock()
				enabled := blockEnabled
				shouldBlockSaves.Unlock()
				if !enabled {
					return
				}
				blockSaves.Do(func() {
					close(saveBlockedCh)
					<-saveReleaseCh
				})
			},
		}

		manager := NewManager(store, nil, nil)
		executor := &stub429CooldownExecutor{id: provider, retryAfter: time.Hour}
		manager.RegisterExecutor(executor)

		retryUntil := time.Now().Add(time.Hour)
		for _, id := range []string{authID1, authID2} {
			auth := &Auth{
				ID:       id,
				Provider: provider,
				Status:   StatusActive,
				Metadata: map[string]any{"type": provider},
				ModelStates: map[string]*ModelState{
					aliasModel: {
						Unavailable:    true,
						Status:         StatusError,
						StatusMessage:  "quota",
						NextRetryAfter: retryUntil,
						Quota: QuotaState{
							Exceeded:      true,
							Reason:        "quota",
							NextRecoverAt: retryUntil,
						},
					},
				},
			}
			if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
				t.Fatalf("register %s: %v", id, errReg)
			}
		}

		reg := registry.GetGlobalRegistry()
		for _, id := range []string{authID1, authID2} {
			reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: lowerCaseModel}})
		}
		t.Cleanup(func() {
			reg.UnregisterClient(authID1)
			reg.UnregisterClient(authID2)
		})
		manager.RefreshSchedulerEntry(authID1)
		manager.RefreshSchedulerEntry(authID2)

		shouldBlockSaves.Lock()
		blockEnabled = true
		shouldBlockSaves.Unlock()

		aliasDoneCh := make(chan struct{})
		go func() {
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
				provider: {{
					Name:  lowerCaseModel,
					Alias: aliasModel,
					Fork:  true,
				}},
			})
			close(aliasDoneCh)
		}()

		<-saveBlockedCh
		// While the first credential's store persistence is blocked, both credentials
		// must already have migrated their in-memory cooldowns atomically with the alias table.
		_, errExecDuringSave := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: aliasModel}, cliproxyexecutor.Options{})
		if errExecDuringSave == nil {
			t.Fatal("expected Execute during SetOAuthModelAlias persistence to be blocked by migrated cooldowns on both credentials")
		}
		if calls := executor.Calls(); len(calls) != 0 {
			t.Fatalf("expected 0 upstream calls during blocked persistence, got %v", calls)
		}

		releaseSave()
		<-aliasDoneCh
	})

	t.Run("concurrent-load-and-alias-publication-preserves-cooldown", func(t *testing.T) {
		const authID = "auth-issue7360-load-race"
		listBlockedCh := make(chan struct{})
		listReleaseCh := make(chan struct{})
		var releaseOnce sync.Once
		releaseList := func() { releaseOnce.Do(func() { close(listReleaseCh) }) }
		t.Cleanup(releaseList)

		retryUntil := time.Now().Add(time.Hour)
		diskSnapshot := &Auth{
			ID:       authID,
			Provider: provider,
			Status:   StatusActive,
			Metadata: map[string]any{"type": provider},
			ModelStates: map[string]*ModelState{
				aliasModel: {
					Unavailable:    true,
					Status:         StatusError,
					StatusMessage:  "quota",
					NextRetryAfter: retryUntil,
					Quota: QuotaState{
						Exceeded:      true,
						Reason:        "quota",
						NextRecoverAt: retryUntil,
					},
				},
			},
		}

		var listOnce sync.Once
		store := &blockingSelectiveAuthStore{
			onList: func() ([]*Auth, error) {
				listOnce.Do(func() {
					close(listBlockedCh)
					<-listReleaseCh
				})
				return []*Auth{diskSnapshot.Clone()}, nil
			},
		}

		manager := NewManager(store, nil, nil)
		executor := &stub429CooldownExecutor{id: provider, retryAfter: time.Hour}
		manager.RegisterExecutor(executor)
		if _, errReg := manager.Register(context.Background(), diskSnapshot.Clone()); errReg != nil {
			t.Fatalf("register auth: %v", errReg)
		}

		reg := registry.GetGlobalRegistry()
		reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: lowerCaseModel}})
		t.Cleanup(func() {
			reg.UnregisterClient(authID)
		})
		manager.RefreshSchedulerEntry(authID)

		loadDoneCh := make(chan error, 1)
		go func() {
			loadDoneCh <- manager.Load(context.Background())
		}()

		<-listBlockedCh
		aliasDoneCh := make(chan struct{})
		go func() {
			manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
				provider: {{
					Name:  lowerCaseModel,
					Alias: aliasModel,
					Fork:  true,
				}},
			})
			close(aliasDoneCh)
		}()

		releaseList()
		if errLoad := <-loadDoneCh; errLoad != nil {
			t.Fatalf("manager.Load failed: %v", errLoad)
		}
		<-aliasDoneCh

		for _, modelSpelling := range []string{aliasModel, lowerCaseModel} {
			_, errExec := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: modelSpelling}, cliproxyexecutor.Options{})
			if errExec == nil {
				t.Fatalf("expected Execute(%q) after concurrent Load and SetOAuthModelAlias to be blocked by cooldown", modelSpelling)
			}
		}
		if calls := executor.Calls(); len(calls) != 0 {
			t.Fatalf("expected 0 upstream calls after concurrent Load and SetOAuthModelAlias, got %v", calls)
		}
	})
}

type blockingSelectiveAuthStore struct {
	onSave func(auth *Auth)
	onList func() ([]*Auth, error)
}

func (s *blockingSelectiveAuthStore) List(context.Context) ([]*Auth, error) {
	if s.onList != nil {
		return s.onList()
	}
	return nil, nil
}

func (s *blockingSelectiveAuthStore) Save(_ context.Context, auth *Auth) (string, error) {
	if s.onSave != nil {
		s.onSave(auth)
	}
	if auth != nil {
		return auth.ID, nil
	}
	return "", nil
}

func (s *blockingSelectiveAuthStore) Delete(context.Context, string) error {
	return nil
}
