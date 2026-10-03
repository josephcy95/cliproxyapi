package auth

import (
	"context"
	"testing"
)

type removedDuringRefreshExecutor struct {
	unauthorizedRefreshExecutor
	started, release chan struct{}
}

func (e *removedDuringRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	close(e.started)
	<-e.release
	return auth.Clone(), nil
}

func TestRefreshCannotReturnRemovedCredential(t *testing.T) {
	m := NewManager(nil, nil, nil)
	exec := &removedDuringRefreshExecutor{unauthorizedRefreshExecutor: unauthorizedRefreshExecutor{id: "codex"}, started: make(chan struct{}), release: make(chan struct{})}
	m.RegisterExecutor(exec)
	a, err := m.Register(context.Background(), &Auth{ID: "removed-refresh", Provider: "codex", Metadata: map[string]any{"access_token": "old"}})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	var updated *Auth
	var refreshErr error
	go func() { updated, refreshErr = m.ForceRefreshAuth(context.Background(), a.ID); close(finished) }()
	<-exec.started
	m.Remove(context.Background(), a.ID)
	close(exec.release)
	<-finished
	if refreshErr == nil || updated != nil {
		t.Fatalf("removed credential returned: auth=%v err=%v", updated, refreshErr)
	}
	if _, ok := m.GetByID(a.ID); ok {
		t.Fatal("removed credential resurrected")
	}
}
