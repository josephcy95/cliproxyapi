package cliproxy

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestRetiredConfigDoesNotAdvertiseModelsOnCodexOAuth(t *testing.T) {
	cfg, err := config.ParseConfigBytes([]byte("excel-api-key: [{}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: cfg}
	for _, plan := range []string{"plus", "free", ""} {
		a := &coreauth.Auth{ID: t.Name() + plan, Provider: "codex", Metadata: map[string]any{"access_token": "test-only", "plan_type": plan}}
		t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(a.ID) })
		s.registerModelsForAuth(context.Background(), a)
		models := GlobalModelRegistry().GetModelsForClient(a.ID)
		if len(models) == 0 {
			t.Fatalf("ordinary Codex models missing for %q", plan)
		}
		for _, m := range models {
			if strings.HasSuffix(m.ID, "-excel") {
				t.Fatalf("retired alias advertised: %s", m.ID)
			}
		}
	}
}
