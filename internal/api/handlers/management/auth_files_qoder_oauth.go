package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	qoderauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qoder"
	qodercnauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qodercn"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Fork-only Qoder / Qoder CN OAuth token handlers. Kept in a dedicated file so
// upstream refactors of auth_files.go do not silently drop them.

func (h *Handler) RequestQoderCNToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Qoder CN authentication...")

	state := fmt.Sprintf("qdcn-%d", time.Now().UnixNano())
	qoderAuth := qodercnauth.NewQoderAuth(h.cfg)

	deviceFlow, errStartDeviceFlow := qoderAuth.InitiateDeviceFlow(ctx)
	if errStartDeviceFlow != nil {
		log.Errorf("Failed to generate Qoder CN authorization URL: %v", errStartDeviceFlow)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}
	authURL := strings.TrimSpace(deviceFlow.VerificationURIComplete)
	if authURL == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "empty authorization url"})
		return
	}

	RegisterOAuthSession(state, "qodercn")

	go func() {
		pollCtx, cancelPoll := context.WithCancel(ctx)
		defer cancelPoll()
		go watchOAuthSessionCancel(pollCtx, cancelPoll, state, "qodercn")

		fmt.Println("Waiting for Qoder CN authentication...")
		tokenData, errPollForToken := qoderAuth.PollForToken(pollCtx, deviceFlow)
		if errPollForToken != nil {
			if !IsOAuthSessionPending(state, "qodercn") {
				return
			}
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Authentication failed", errPollForToken))
			fmt.Printf("Authentication failed: %v\n", errPollForToken)
			return
		}
		if !IsOAuthSessionPending(state, "qodercn") {
			return
		}

		tokenStorage := qoderAuth.CreateTokenStorage(tokenData, deviceFlow.MachineID)
		name, email := qoderAuth.SaveUserInfo(ctx, tokenData.AccessToken, tokenData.UserID, "", "")
		label := strings.TrimSpace(email)
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
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "qodercn",
			FileName: fileName,
			Label:    label,
			Storage:  tokenStorage,
			Metadata: metadata,
		}
		if errGuard := guardOAuthSessionPendingForSave(state, "qodercn"); errGuard != nil {
			return
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use Qoder CN services through this CLI")
		CompleteOAuthSession(state)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state, "flow": "device"})
}

// RequestQoderToken starts the Qoder (international) device-flow OAuth session for the management UI.
func (h *Handler) RequestQoderToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Qoder (international) authentication...")

	state := fmt.Sprintf("qd-%d", time.Now().UnixNano())
	qoderAuth := qoderauth.NewQoderAuth(h.cfg)

	deviceFlow, errStartDeviceFlow := qoderAuth.InitiateDeviceFlow(ctx)
	if errStartDeviceFlow != nil {
		log.Errorf("Failed to generate Qoder authorization URL: %v", errStartDeviceFlow)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}
	authURL := strings.TrimSpace(deviceFlow.VerificationURIComplete)
	if authURL == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "empty authorization url"})
		return
	}

	RegisterOAuthSession(state, "qoder")

	go func() {
		pollCtx, cancelPoll := context.WithCancel(ctx)
		defer cancelPoll()
		go watchOAuthSessionCancel(pollCtx, cancelPoll, state, "qoder")

		fmt.Println("Waiting for Qoder authentication...")
		tokenData, errPollForToken := qoderAuth.PollForToken(pollCtx, deviceFlow)
		if errPollForToken != nil {
			if !IsOAuthSessionPending(state, "qoder") {
				return
			}
			SetOAuthSessionError(state, oauthSessionErrorWithCause("Authentication failed", errPollForToken))
			fmt.Printf("Authentication failed: %v\n", errPollForToken)
			return
		}
		if !IsOAuthSessionPending(state, "qoder") {
			return
		}

		tokenStorage := qoderAuth.CreateTokenStorage(tokenData, deviceFlow.MachineID)
		name, email := qoderAuth.SaveUserInfo(ctx, tokenData.AccessToken, tokenData.UserID, "", "")
		label := strings.TrimSpace(email)
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
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "qoder",
			FileName: fileName,
			Label:    label,
			Storage:  tokenStorage,
			Metadata: metadata,
		}
		if errGuard := guardOAuthSessionPendingForSave(state, "qoder"); errGuard != nil {
			return
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use Qoder (international) services through this CLI")
		CompleteOAuthSession(state)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state, "flow": "device"})
}
