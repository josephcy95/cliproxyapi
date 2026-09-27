package managementasset

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAutoUpdateSkipReason(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.Config
		wantReason string
		wantSkip   bool
	}{
		{
			name:       "nil config",
			cfg:        nil,
			wantReason: "config not yet available",
			wantSkip:   true,
		},
		{
			name: "cluster mode",
			cfg: &config.Config{
				Home: config.HomeConfig{Enabled: true},
			},
			wantReason: "cluster mode enabled",
			wantSkip:   true,
		},
		{
			name: "control panel disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{DisableControlPanel: true},
			},
			wantReason: "control panel disabled",
			wantSkip:   true,
		},
		{
			name: "auto update disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{DisableAutoUpdatePanel: true},
			},
			wantReason: "disable-auto-update-panel is enabled",
			wantSkip:   true,
		},
		{
			name:       "enabled",
			cfg:        &config.Config{},
			wantReason: "",
			wantSkip:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReason, gotSkip := autoUpdateSkipReason(tt.cfg)
			if gotReason != tt.wantReason || gotSkip != tt.wantSkip {
				t.Fatalf("autoUpdateSkipReason() = (%q, %t), want (%q, %t)", gotReason, gotSkip, tt.wantReason, tt.wantSkip)
			}
		})
	}
}

func TestManagementUpdateSkipReasonRefreshesAtStartup(t *testing.T) {
	cfg := &config.Config{
		RemoteManagement: config.RemoteManagement{DisableAutoUpdatePanel: true},
	}
	if reason, skip := managementUpdateSkipReason(cfg, false); skip {
		t.Fatalf("startup refresh skipped: %s", reason)
	}
	if reason, skip := managementUpdateSkipReason(cfg, true); !skip || reason != "disable-auto-update-panel is enabled" {
		t.Fatalf("periodic refresh = (%q, %t), want disabled", reason, skip)
	}
}

func TestResolveReleaseURLKeepsManagementPanelOnFork(t *testing.T) {
	tests := []struct {
		name string
		repo string
	}{
		{name: "empty", repo: ""},
		{name: "fork default", repo: config.DefaultPanelGitHubRepository},
		{name: "legacy upstream default", repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"},
		{name: "legacy upstream default with slash", repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center/"},
		{name: "legacy upstream git URL", repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git"},
		{name: "legacy upstream git URL with slash", repo: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git/"},
		{name: "legacy upstream API URL", repo: "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveReleaseURL(tt.repo); got != defaultManagementReleaseURL {
				t.Fatalf("resolveReleaseURL(%q) = %q, want %q", tt.repo, got, defaultManagementReleaseURL)
			}
		})
	}
}

func TestManagementFallbackURLIsForkOwned(t *testing.T) {
	if !strings.Contains(defaultManagementFallbackURL, "github.com/josephcy95/Cli-Proxy-API-Management-Center/") {
		t.Fatalf("management fallback must be fork-owned, got %q", defaultManagementFallbackURL)
	}
}
