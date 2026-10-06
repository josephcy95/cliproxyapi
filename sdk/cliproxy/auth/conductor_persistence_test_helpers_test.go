package auth

import (
	"context"
	"sync"
)

type memoryAuthTestStore struct {
	mu    sync.Mutex
	auths map[string]*Auth
}

func newMemoryAuthTestStore() *memoryAuthTestStore {
	return &memoryAuthTestStore{auths: make(map[string]*Auth)}
}

func (s *memoryAuthTestStore) List(ctx context.Context) ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := make([]*Auth, 0, len(s.auths))
	for _, a := range s.auths {
		res = append(res, a.Clone())
	}
	return res, nil
}

func (s *memoryAuthTestStore) Save(ctx context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auths[auth.ID] = auth.Clone()
	return auth.ID, nil
}

func (s *memoryAuthTestStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.auths, id)
	return nil
}
