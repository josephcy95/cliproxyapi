package auth

import (
	"strings"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
)

// ExcelEligible authorizes the temporary Excel routes, not ordinary Codex models.
// Only known paid Codex OAuth accounts qualify. A config/API-key entry must
// never become eligible merely by claiming an OAuth kind or a paid plan.
func (a *Auth) ExcelEligible() bool {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") ||
		a.AuthKind() != AuthKindOAuth || a.AuthSourceKind() == AuthSourceConfig ||
		authAttribute(a, AttributeAPIKey) != "" || authMetadataString(a, AttributeAPIKey) != "" ||
		authMetadataString(a, "access_token") == "" {
		return false
	}
	plan := authMetadataString(a, "plan_type")
	if plan == "" {
		plan = authMetadataString(a, "chatgpt_plan_type")
	}
	if plan == "" {
		plan = authAttribute(a, "plan_type")
	}
	if plan == "" {
		if claims, err := codexauth.ParseJWTToken(authMetadataString(a, "id_token")); err == nil && claims != nil {
			plan = claims.CodexAuthInfo.ChatgptPlanType
		}
	}
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "plus", "pro", "team", "business", "enterprise", "edu", "education", "k12", "go":
		return true
	default:
		return false
	}
}
