package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

func TestPersistCodexRefreshKeepsForkRuntimeReset(t *testing.T) {
	for _, staleCredential := range []bool{false, true} {
		name := "matching_credential"
		if staleCredential {
			name = "newer_disk_credential"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "codex.json")
			expiry := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			runtime := map[string]any{"models": map[string]any{"gpt-5": map[string]any{
				"cooldown_until": expiry, "reason": "unauthorized", "http_status": 401, "auth_failure_hits": 1,
			}}}
			before := map[string]any{"type": "codex", "access_token": "old", "refresh_token": "refresh-old", "runtime": runtime, "disabled": true, "disabled_reason": "auth_failure"}
			after := cloneMetadataMap(before)
			after["access_token"] = "new"
			after["refresh_token"] = "refresh-new"
			delete(after, "runtime")
			delete(after, "disabled_reason")
			after["disabled"] = false
			disk := cloneMetadataMap(before)
			disk["priority"] = float64(9)
			disk["allow_private_instructions"] = true
			if staleCredential {
				disk["access_token"] = "newer"
				disk["refresh_token"] = "refresh-newer"
			}
			raw, err := json.Marshal(disk)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			auth := &Auth{ID: "codex", Provider: "codex", Metadata: after, PersistCodexRuntimeReset: true, CodexRefreshPatch: codex.NewCredentialRefreshPatch(before, after, true, false)}
			handled, err := PersistCodexAuthFile(context.Background(), path, auth)
			if err != nil || !handled {
				t.Fatalf("persist handled=%v err=%v", handled, err)
			}
			raw, err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]any
			if err = json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if saved["priority"] != float64(9) || saved["allow_private_instructions"] != true {
				t.Fatal("lost operator metadata")
			}
			reloaded := &Auth{Provider: "codex", Metadata: saved}
			hydrateAuthRuntimeFromMetadata(reloaded, time.Now())
			if staleCredential {
				if saved["access_token"] != "newer" || saved["runtime"] == nil || saved["disabled"] != true {
					t.Fatalf("superseded refresh changed new credential/runtime: %#v", saved)
				}
			} else {
				if saved["access_token"] != "new" {
					t.Fatal("refresh token not persisted")
				}
				if saved["runtime"] != nil || saved["disabled"] != false || saved["disabled_reason"] != nil {
					t.Fatalf("reset did not survive persistence: %#v", saved)
				}
				if state := reloaded.ModelStates["gpt-5"]; state != nil && (state.Unavailable || state.AuthFailureCount != 0) {
					t.Fatal("reload resurrected old failure state")
				}
			}
		})
	}
}
