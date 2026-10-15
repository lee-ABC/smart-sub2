package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func workspaceGuardClaims(accountID, plan string) map[string]any {
	return map[string]any{
		"client_id": "test-client", "sub": "subject-a", "exp": time.Now().Add(24 * time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID, "chatgpt_plan_type": plan,
			"chatgpt_user_id": "user-a", "user_id": "user-a",
			"chatgpt_account_user_id": "user-a__" + accountID,
		},
	}
}

func workspaceGuardJWT(claims map[string]any) string {
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(jsonBytes(claims)) + ".synthetic-signature"
}

func workspaceGuardRoot() map[string]any {
	return map[string]any{
		"access_token": workspaceGuardJWT(workspaceGuardClaims("business-a", "team")),
		"id_token":     "original-id-token", "refresh_token": "original-refresh-token",
		"account_id": "business-a", "client_id": "test-client", "type": "oai-basispoints",
		"email": "example@example.test", "note": "Original note", "priority": 8,
		"disabled": true, "custom_field": map[string]any{"keep": true}, "expired": "original-expiry",
	}
}

func workspaceGuardCall(t *testing.T, root, grant map[string]any) (map[string]any, int, error) {
	t.Helper()
	service := NewService()
	calls := 0
	service.SetHost(func(method string, payload any, out any) error {
		if method == "host.http.do" {
			calls++
			response := out.(*upstreamResponse)
			response.StatusCode = 200
			response.Body = jsonBytes(grant)
		}
		return nil
	})
	raw := jsonBytes(authRefreshRequest{
		AuthID: "bp-original", StorageJSON: jsonBytes(root),
		Metadata: map[string]any{"file_name": "original.json"},
	})
	result, err := service.authRefresh(raw)
	return result, calls, err
}

func workspaceGuardStored(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	auth := objectValue(result["Auth"])
	var stored map[string]any
	if err := json.Unmarshal(auth["StorageJSON"].([]byte), &stored); err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestRefreshWorkspaceDriftPreservesBusinessAndRotatedToken(t *testing.T) {
	for _, tc := range []struct{ name, accountID, plan, reason string }{
		{"personal", "personal-a", "free", "workspace_changed_during_refresh"},
		{"other_business", "business-b", "team", "workspace_changed_during_refresh"},
		{"same_workspace_free", "business-a", "free", "plan_mismatch"},
		{"same_workspace_missing_plan", "business-a", "", "plan_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := workspaceGuardRoot()
			originalJSON := string(jsonBytes(root))
			claims := workspaceGuardClaims(tc.accountID, tc.plan)
			grant := map[string]any{"access_token": workspaceGuardJWT(claims), "id_token": workspaceGuardJWT(claims), "refresh_token": "rotated-refresh-token"}
			before := time.Now()
			result, calls, err := workspaceGuardCall(t, root, grant)
			if err != nil || calls != 1 {
				t.Fatalf("refresh = %v, calls = %d", err, calls)
			}
			stored := workspaceGuardStored(t, result)
			for key, value := range root {
				if key != "refresh_token" && !reflect.DeepEqual(stored[key], value) {
					// JSON numbers decode to float64.
					if string(jsonBytes(stored[key])) != string(jsonBytes(value)) {
						t.Fatalf("original %s changed", key)
					}
				}
			}
			if stored["refresh_token"] != "rotated-refresh-token" {
				t.Fatal("rotated refresh token was lost")
			}
			pending := objectValue(stored["workspace_refresh_pending"])
			if pending["expected_account_id"] != "business-a" || pending["observed_account_id"] != tc.accountID || pending["reason"] != tc.reason {
				t.Fatalf("incorrect pending marker: %v", pending)
			}
			if _, err := time.Parse(time.RFC3339, stringValue(pending["detected_at"])); err != nil {
				t.Fatal("missing detection time")
			}
			for _, value := range pending {
				if strings.Contains(stringValue(value), "token") || strings.Contains(stringValue(value), ".synthetic-signature") {
					t.Fatal("pending marker contains token material")
				}
			}
			if next := result["NextRefreshAfter"].(time.Time); next.Before(before.Add(9*time.Minute)) || next.After(time.Now().Add(11*time.Minute)) {
				t.Fatalf("unexpected next refresh time: %v", next)
			}
			auth := objectValue(result["Auth"])
			if auth["ID"] != "bp-original" || auth["FileName"] != "original.json" || objectValue(auth["Metadata"])["account_id"] != "business-a" {
				t.Fatal("credential or workspace identity changed")
			}
			if string(jsonBytes(root)) != originalJSON {
				t.Fatal("input root was mutated")
			}
			again, againCalls, err := workspaceGuardCall(t, stored, grant)
			if err != nil || againCalls != 0 {
				t.Fatalf("pending credential sent another refresh grant: error=%v calls=%d", err, againCalls)
			}
			if string(jsonBytes(workspaceGuardStored(t, again))) != string(jsonBytes(stored)) {
				t.Fatal("pending credential changed without recovery")
			}
		})
	}
}

func TestRefreshSameWorkspaceAcceptsNewTokens(t *testing.T) {
	root := workspaceGuardRoot()
	claims := workspaceGuardClaims("business-a", "team")
	claims["version"] = "renewed"
	grant := map[string]any{"access_token": workspaceGuardJWT(claims), "id_token": workspaceGuardJWT(claims), "refresh_token": "rotated-refresh-token"}
	result, calls, err := workspaceGuardCall(t, root, grant)
	if err != nil || calls != 1 {
		t.Fatalf("refresh=%v calls=%d", err, calls)
	}
	stored := workspaceGuardStored(t, result)
	if stored["access_token"] != grant["access_token"] || stored["id_token"] != grant["id_token"] || stored["refresh_token"] != grant["refresh_token"] {
		t.Fatal("same-workspace tokens were not accepted")
	}
	if _, pending := stored["workspace_refresh_pending"]; pending {
		t.Fatal("same-workspace refresh created a pending marker")
	}
	for _, key := range []string{"note", "priority", "disabled", "account_id", "custom_field"} {
		if string(jsonBytes(stored[key])) != string(jsonBytes(root[key])) {
			t.Fatalf("%s changed", key)
		}
	}
}

func TestRefreshRejectsChangedOrMissingIdentity(t *testing.T) {
	cases := []struct {
		name string
		edit func(map[string]any)
	}{
		{"client", func(c map[string]any) { c["client_id"] = "other-client" }},
		{"subject", func(c map[string]any) { c["sub"] = "other-subject" }},
		{"user_id", func(c map[string]any) { objectValue(c["https://api.openai.com/auth"])["user_id"] = "other-user" }},
		{"chatgpt_user_id", func(c map[string]any) {
			objectValue(c["https://api.openai.com/auth"])["chatgpt_user_id"] = "other-user"
		}},
		{"missing_subject", func(c map[string]any) { delete(c, "sub") }},
		{"missing_client", func(c map[string]any) { delete(c, "client_id") }},
		{"missing_workspace", func(c map[string]any) { delete(objectValue(c["https://api.openai.com/auth"]), "chatgpt_account_id") }},
		{"missing_user", func(c map[string]any) { delete(objectValue(c["https://api.openai.com/auth"]), "user_id") }},
		{"missing_expiry", func(c map[string]any) { delete(c, "exp") }},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := workspaceGuardRoot()
			before := string(jsonBytes(root))
			claims := workspaceGuardClaims("business-a", "team")
			tc.edit(claims)
			grant := map[string]any{"access_token": workspaceGuardJWT(claims), "refresh_token": "rejected-refresh-secret"}
			result, calls, err := workspaceGuardCall(t, root, grant)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 502 || result != nil || calls != 1 {
				t.Fatalf("expected safe 502 rejection; result=%v error=%v calls=%d", result, err, calls)
			}
			if strings.Contains(err.Error(), "rejected-refresh-secret") || strings.Contains(err.Error(), ".synthetic-signature") {
				t.Fatal("error leaks token material")
			}
			if string(jsonBytes(root)) != before {
				t.Fatal("rejected grant mutated original root")
			}
		})
	}
}

func TestRefreshRejectsMalformedTokenAndBadPendingMarker(t *testing.T) {
	for _, token := range []string{"opaque-access-token", "a.e30.signature", workspaceGuardJWT(map[string]any{"exp": 4102444800})} {
		root := workspaceGuardRoot()
		root["access_token"] = token
		result, calls, err := workspaceGuardCall(t, root, nil)
		if err == nil || result != nil || calls != 0 {
			t.Fatalf("invalid original token used for refresh; calls=%d error=%v", calls, err)
		}
	}
	for _, marker := range []any{true, map[string]any{"expected_account_id": "other-workspace"}} {
		root := workspaceGuardRoot()
		root["workspace_refresh_pending"] = marker
		result, calls, err := workspaceGuardCall(t, root, nil)
		if err == nil || result != nil || calls != 0 {
			t.Fatalf("bad pending marker used for refresh; calls=%d error=%v", calls, err)
		}
	}
	root := workspaceGuardRoot()
	root["client_id"] = "other-client"
	if result, calls, err := workspaceGuardCall(t, root, nil); err == nil || result != nil || calls != 0 {
		t.Fatal("source/token client mismatch was not rejected before refresh")
	}
}

func TestRefreshIDTokenCannotChangeIdentity(t *testing.T) {
	for _, key := range []string{"sub", "client_id", "workspace"} {
		t.Run(key, func(t *testing.T) {
			root := workspaceGuardRoot()
			claims := workspaceGuardClaims("business-a", "team")
			idClaims := workspaceGuardClaims("business-a", "team")
			if key == "workspace" {
				objectValue(idClaims["https://api.openai.com/auth"])["chatgpt_account_id"] = "personal-a"
			} else {
				idClaims[key] = "other-value"
			}
			grant := map[string]any{"access_token": workspaceGuardJWT(claims), "id_token": workspaceGuardJWT(idClaims), "refresh_token": "rejected-refresh-secret"}
			if result, _, err := workspaceGuardCall(t, root, grant); err == nil || result != nil {
				t.Fatal("inconsistent ID token was accepted")
			}
		})
	}
	root := workspaceGuardRoot()
	claims := workspaceGuardClaims("business-a", "team")
	idClaims := map[string]any{"sub": "subject-a", "aud": []any{"test-client"}, "exp": time.Now().Add(time.Hour).Unix()}
	grant := map[string]any{"access_token": workspaceGuardJWT(claims), "id_token": workspaceGuardJWT(idClaims), "refresh_token": "rotated-refresh-token"}
	if _, _, err := workspaceGuardCall(t, root, grant); err != nil {
		t.Fatalf("valid ID token without optional workspace was rejected: %v", err)
	}
}
