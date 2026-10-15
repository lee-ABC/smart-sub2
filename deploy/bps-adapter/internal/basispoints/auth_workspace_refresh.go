package basispoints

import (
	"encoding/json"
	"strings"
	"time"
)

type workspaceRefreshIdentity struct {
	accountID string
	clientID  string
	plan      string
	users     map[string]string
}

// These are identity claims from the authenticated token response. The
// composite chatgpt_account_user_id is deliberately excluded: it changes when
// the same person selects a different workspace.
func workspaceRefreshUserClaims(claims map[string]any) map[string]string {
	users := map[string]string{}
	if sub := firstString(claims, "sub"); sub != "" {
		users["sub"] = sub
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		for _, key := range []string{"user_id", "chatgpt_user_id"} {
			if value := firstString(auth, key); value != "" {
				users[key] = value
			}
		}
	}
	return users
}

func workspaceRefreshIdentityFromToken(token string) (workspaceRefreshIdentity, error) {
	claims := jwtPayload(token)
	identity := workspaceRefreshIdentity{
		accountID: accountIDFromClaims(claims),
		clientID:  firstString(claims, "client_id", "azp"),
		plan:      credentialPlan(credential{AccessToken: token}),
		users:     workspaceRefreshUserClaims(claims),
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" || identity.accountID == "" || identity.clientID == "" || len(identity.users) == 0 || jwtExpiry(claims).IsZero() {
		return workspaceRefreshIdentity{}, fail(502, "refresh_identity_invalid", "ChatGPT token refresh has incomplete identity claims")
	}
	return identity, nil
}

func workspaceRefreshBusinessPlan(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "team", "business", "self_serve_business", "self_serve_business_prolite", "chatgptteamplan", "chatgptbusinessplan":
		return true
	default:
		return false
	}
}

func workspaceRefreshSameUser(expected, observed map[string]string) bool {
	if len(expected) == 0 {
		return false
	}
	for key, value := range expected {
		if observed[key] != value {
			return false
		}
	}
	return true
}

func validateWorkspaceRefreshIDToken(token string, expected workspaceRefreshIdentity) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	claims := jwtPayload(token)
	users := workspaceRefreshUserClaims(claims)
	if len(claims) == 0 {
		return fail(502, "refresh_identity_invalid", "ChatGPT token refresh returned an invalid ID token")
	}
	for key, value := range expected.users {
		if observed, exists := users[key]; exists {
			if observed != value {
				return fail(502, "refresh_identity_mismatch", "ChatGPT token refresh returned an inconsistent ID token user")
			}
		}
	}
	clientID := firstString(claims, "client_id", "azp")
	clientMatches := clientID == expected.clientID
	clientPresent := clientID != ""
	if clientID == "" {
		switch audience := claims["aud"].(type) {
		case string:
			clientPresent = true
			clientMatches = audience == expected.clientID
		case []any:
			clientPresent = true
			for _, value := range audience {
				if stringValue(value) == expected.clientID {
					clientMatches = true
				}
			}
		}
	}
	if clientPresent && !clientMatches {
		return fail(502, "refresh_identity_mismatch", "ChatGPT token refresh returned an inconsistent ID token client")
	}
	if accountID := accountIDFromClaims(claims); accountID != "" && accountID != expected.accountID {
		return fail(502, "refresh_identity_mismatch", "ChatGPT token refresh returned inconsistent workspace claims")
	}
	return nil
}

func (s *Service) workspaceRefreshResult(request authRefreshRequest, root map[string]any, next time.Time) (map[string]any, error) {
	updated, err := json.Marshal(root)
	if err != nil {
		return nil, fail(500, "refresh_encode", "failed to encode refreshed ChatGPT credential")
	}
	c, err := parseCredential(updated)
	if err != nil {
		return nil, err
	}
	// Preserve the host's identity instead of applying the bp- prefix again.
	fileName := firstString(request.Metadata, "file_name", "fileName")
	if fileName == "" {
		fileName = strings.TrimSpace(request.AuthID)
	}
	if fileName != "" && !strings.HasSuffix(fileName, ".json") {
		fileName += ".json"
	}
	auth := authData(updated, fileName, c, s.authProviderID())
	if id := strings.TrimSpace(request.AuthID); id != "" {
		auth["ID"] = id
	}
	return map[string]any{"Auth": auth, "NextRefreshAfter": next.UTC()}, nil
}
