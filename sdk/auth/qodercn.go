package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qodercn"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// QoderCNAuthenticator implements the device flow login for Qoder CN accounts.
type QoderCNAuthenticator struct{}

// NewQoderCNAuthenticator constructs a Qoder CN authenticator.
func NewQoderCNAuthenticator() *QoderCNAuthenticator {
	return &QoderCNAuthenticator{}
}

// Provider returns the provider key for Qoder CN.
func (a *QoderCNAuthenticator) Provider() string {
	return "qodercn"
}

// RefreshLead returns a nominal lead for scheduled revisit. Device tokens are
// long-lived and the upstream refresh path is not used for this flow.
func (a *QoderCNAuthenticator) RefreshLead() *time.Duration {
	d := 24 * time.Hour
	return &d
}

// Login initiates Qoder CN device-flow authentication and returns a saved auth record.
func (a *QoderCNAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := qodercn.NewQoderAuth(cfg)

	deviceFlow, err := authSvc.InitiateDeviceFlow(ctx)
	if err != nil {
		return nil, fmt.Errorf("qodercn device flow initiation failed: %w", err)
	}

	authURL := deviceFlow.VerificationURIComplete

	if !opts.NoBrowser {
		fmt.Println("Opening browser for Qoder CN authentication")
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

	fmt.Println("Waiting for Qoder CN authentication...")

	tokenData, err := authSvc.PollForToken(ctx, deviceFlow)
	if err != nil {
		return nil, fmt.Errorf("qodercn authentication failed: %w", err)
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

	fileName := fmt.Sprintf("qodercn-%s.json", label)
	metadata := map[string]any{
		"type":         "qodercn",
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

	fmt.Println("Qoder CN authentication successful")
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
