package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// QoderAuthenticator implements the device flow login for Qoder (international) accounts.
type QoderAuthenticator struct{}

// NewQoderAuthenticator constructs a Qoder (international) authenticator.
func NewQoderAuthenticator() *QoderAuthenticator {
	return &QoderAuthenticator{}
}

// Provider returns the provider key for Qoder (international).
func (a *QoderAuthenticator) Provider() string {
	return "qoder"
}

// RefreshLead returns a nominal lead for scheduled revisit. Device tokens are
// long-lived and the upstream refresh path is not used for this flow.
func (a *QoderAuthenticator) RefreshLead() *time.Duration {
	d := 24 * time.Hour
	return &d
}

// Login initiates Qoder (international) device-flow authentication and returns a saved auth record.
func (a *QoderAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := qoder.NewQoderAuth(cfg)

	deviceFlow, err := authSvc.InitiateDeviceFlow(ctx)
	if err != nil {
		return nil, fmt.Errorf("qoder device flow initiation failed: %w", err)
	}

	authURL := deviceFlow.VerificationURIComplete

	if !opts.NoBrowser {
		fmt.Println("Opening browser for Qoder authentication")
		if !browser.IsAvailable() {
			log.Warn("No browser available; please open the URL manually")
			fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
		} else if err = browser.OpenURL(authURL); err != nil {
			log.Warnf("Failed to open browser automatically: %v", err)
			fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
		}
	} else {
		fmt.Printf("Visit the following URL to continue authentication:\n%s\n", authURL)
	}

	fmt.Println("Waiting for Qoder authentication...")

	tokenData, err := authSvc.PollForToken(ctx, deviceFlow)
	if err != nil {
		return nil, fmt.Errorf("qoder authentication failed: %w", err)
	}

	tokenStorage := authSvc.CreateTokenStorage(tokenData, deviceFlow.MachineID)
	name, email := authSvc.SaveUserInfo(ctx, tokenData.AccessToken, tokenData.UserID, "", "")

	label := strings.TrimSpace(email)
	if label == "" && opts.Metadata != nil {
		label = strings.TrimSpace(opts.Metadata["email"])
		if label == "" {
			label = strings.TrimSpace(opts.Metadata["alias"])
		}
	}
	if label == "" {
		label = strings.TrimSpace(tokenData.UserID)
	}
	if label == "" {
		label = fmt.Sprintf("user-%d", time.Now().UnixMilli())
	}

	tokenStorage.Email = label
	tokenStorage.Name = name

	fileName := fmt.Sprintf("qoder-%s.json", label)
	metadata := map[string]any{
		"type":         "qoder",
		"email":        label,
		"name":         name,
		"user_id":      tokenData.UserID,
		"token":        tokenData.AccessToken,
		"access_token": tokenData.AccessToken,
		"machine_id":   deviceFlow.MachineID,
	}
	if tokenData.RefreshToken != "" {
		metadata["refresh_token"] = tokenData.RefreshToken
	}
	if tokenData.ExpireTime > 0 {
		metadata["expire_time"] = tokenData.ExpireTime
	}

	fmt.Println("Qoder authentication successful")
	if name != "" {
		fmt.Printf("Logged in as %s <%s>\n", name, label)
	}

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    label,
		Storage:  tokenStorage,
		Metadata: metadata,
	}, nil
}
