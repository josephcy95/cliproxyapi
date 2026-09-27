package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	apiHandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	playgroundMaxMessages      = 50
	playgroundMaxMessageLength = 1 << 20
)

var playgroundReasoningEfforts = map[string]struct{}{
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
}

type playgroundMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type playgroundChatRequest struct {
	Model           string              `json:"model"`
	Provider        string              `json:"provider"`
	AuthIndex       string              `json:"auth_index"`
	AuthID          string              `json:"auth_id"`
	ReasoningEffort string              `json:"reasoning_effort"`
	Messages        []playgroundMessage `json:"messages"`
}

type playgroundRoute struct {
	Model           string `json:"model"`
	Provider        string `json:"provider"`
	AuthIndex       string `json:"auth_index"`
	CredentialLabel string `json:"credential_label"`
}

type playgroundChatResponse struct {
	Message    playgroundMessage `json:"message"`
	Route      playgroundRoute   `json:"route"`
	Usage      json.RawMessage   `json:"usage,omitempty"`
	DurationMS int64             `json:"duration_ms"`
}

type openAIChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message playgroundMessage `json:"message"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

func (s *Server) playgroundChat(c *gin.Context) {
	var request playgroundChatRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	request.Provider = strings.TrimSpace(request.Provider)
	request.AuthIndex = strings.TrimSpace(request.AuthIndex)
	request.AuthID = strings.TrimSpace(request.AuthID)
	request.ReasoningEffort = strings.ToLower(strings.TrimSpace(request.ReasoningEffort))
	if request.Model == "" || request.Provider == "" || request.AuthIndex == "" || request.AuthID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model, provider, auth_index, and auth_id are required"})
		return
	}
	if errMessage := validatePlaygroundMessages(request.Messages); errMessage != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMessage})
		return
	}
	if request.ReasoningEffort != "" {
		if _, ok := playgroundReasoningEfforts[request.ReasoningEffort]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported reasoning_effort"})
			return
		}
	}

	auth := playgroundAuthByIdentity(s.handlers.AuthManager, request.AuthID, request.AuthIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential not found"})
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), request.Provider) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credential does not belong to the selected provider"})
		return
	}

	payloadBody := gin.H{
		"model":    request.Model,
		"messages": request.Messages,
		"stream":   false,
	}
	if request.ReasoningEffort != "" {
		payloadBody["reasoning_effort"] = request.ReasoningEffort
	}
	payload, errMarshal := json.Marshal(payloadBody)
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build request"})
		return
	}

	ctx := apiHandlers.WithPinnedAuthID(c.Request.Context(), auth.ID)
	startedAt := time.Now()
	result, errMessage := s.handlers.ExecuteProtocolWithAuthManager(ctx, apiHandlers.ProtocolExecutionRequest{
		EntryProtocol:  OpenAI,
		ExitProtocol:   OpenAI,
		ForcedProvider: request.Provider,
		Model:          request.Model,
		Body:           payload,
	})
	durationMS := time.Since(startedAt).Milliseconds()
	if errMessage != nil {
		status := playgroundErrorStatus(errMessage.StatusCode)
		message := "playground request failed"
		if errMessage.Error != nil && strings.TrimSpace(errMessage.Error.Error()) != "" {
			message = errMessage.Error.Error()
		}
		c.JSON(status, gin.H{"error": message})
		return
	}

	var response openAIChatResponse
	if errUnmarshal := json.Unmarshal(result.Body, &response); errUnmarshal != nil || len(response.Choices) == 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "provider returned an invalid chat response"})
		return
	}
	message := response.Choices[0].Message
	if strings.TrimSpace(message.Role) == "" {
		message.Role = "assistant"
	}
	resolvedModel := strings.TrimSpace(response.Model)
	if resolvedModel == "" {
		resolvedModel = request.Model
	}
	c.JSON(http.StatusOK, playgroundChatResponse{
		Message: message,
		Route: playgroundRoute{
			Model:           resolvedModel,
			Provider:        auth.Provider,
			AuthIndex:       request.AuthIndex,
			CredentialLabel: playgroundCredentialLabel(auth),
		},
		Usage:      response.Usage,
		DurationMS: durationMS,
	})
}

func validatePlaygroundMessages(messages []playgroundMessage) string {
	if len(messages) == 0 {
		return "at least one message is required"
	}
	if len(messages) > playgroundMaxMessages {
		return "too many messages"
	}
	for _, message := range messages {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "user", "assistant":
		default:
			return "message role must be system, user, or assistant"
		}
		if strings.TrimSpace(message.Content) == "" {
			return "message content cannot be empty"
		}
		if len(message.Content) > playgroundMaxMessageLength {
			return "message content is too large"
		}
	}
	return ""
}

func playgroundAuthByIdentity(manager *coreauth.Manager, authID, authIndex string) *coreauth.Auth {
	if manager == nil {
		return nil
	}
	auth, ok := manager.GetByID(strings.TrimSpace(authID))
	if !ok || auth == nil || strings.TrimSpace(auth.EnsureIndex()) != strings.TrimSpace(authIndex) {
		return nil
	}
	return auth
}

func playgroundErrorStatus(status int) int {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return http.StatusBadGateway
	}
	if status < 400 || status > 599 {
		return http.StatusBadGateway
	}
	return status
}

func playgroundCredentialLabel(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	if name := strings.TrimSpace(filepath.Base(auth.FileName)); name != "" && name != "." {
		return name
	}
	return strings.TrimSpace(auth.Provider)
}
