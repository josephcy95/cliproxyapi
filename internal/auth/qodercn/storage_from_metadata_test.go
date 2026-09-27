package qodercn_test

import (
	q "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qodercn"
	"testing"
)

func TestStorageFromMetadata(t *testing.T) {
	s := q.StorageFromMetadata(map[string]any{
		"type":       "qodercn",
		"token":      "dt-abc",
		"user_id":    "uid-1",
		"machine_id": "mid-1",
		"name":       "n",
		"email":      "e",
	})
	if s == nil || s.Token != "dt-abc" || s.UserID != "uid-1" || s.MachineID != "mid-1" {
		t.Fatalf("unexpected %#v", s)
	}
	if q.StorageFromMetadata(map[string]any{"token": "x"}) != nil {
		t.Fatal("expected nil without user id")
	}
}
