package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type authParseRequest struct {
	Provider string `json:"Provider"`
	Path     string `json:"Path"`
	FileName string `json:"FileName"`
	RawJSON  []byte `json:"RawJSON"`
}

type authRefreshRequest struct {
	AuthID      string         `json:"AuthID"`
	StorageJSON []byte         `json:"StorageJSON"`
	Metadata    map[string]any `json:"Metadata"`
}

type credential struct {
	AccessToken string
	AccountID   string
	AuthMode    string
	Email       string
	ExpiresAt   time.Time
	// AccountUserID is what the real add-in sends as x-openai-account-user-id.
	// It is read from the token rather than assembled: the claim already holds
	// the "<user>__<account>" form the header carries, on all 25 credentials in
	// the pool, and rebuilding it from its two halves would be inventing a
	// value that merely looks right.
	AccountUserID string
	// SubscriptionStart is when this account's ChatGPT subscription began, as
	// the upstream's own id_token states it. It is the one date in a credential
	// that is both stable across token refreshes and already known to the other
	// side, which is what makes it usable as a floor for anything this relay
	// claims about the account's history.
	SubscriptionStart time.Time
}

func parseCredential(raw []byte) (credential, error) {
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil || root == nil {
		return credential{}, fail(400, "invalid_auth", "OAuth credential is not valid JSON")
	}
	token := findToken(root)
	if token == "" {
		return credential{}, fail(401, "invalid_auth", "ChatGPT OAuth credential has no access_token")
	}
	claims := jwtPayload(token)
	accountID := accountIDFromClaims(claims)
	if accountID == "" {
		accountID = findAccountID(root)
	}
	if accountID == "" {
		return credential{}, fail(401, "invalid_auth", "ChatGPT OAuth credential has no account ID")
	}
	authMode := firstString(root, "auth_mode", "authMode")
	if !strings.EqualFold(authMode, "chatgpt") {
		authMode = "chatgpt"
	}
	email := firstString(root, "email")
	if email == "" {
		email = stringValue(claims["email"])
	}
	expiresAt := jwtExpiry(claims)
	if rawExpiry := firstValue(root, "expires_at", "expired"); expiresAt.IsZero() {
		expiresAt = timeFromValue(rawExpiry)
	}
	return credential{
		AccessToken:       token,
		AccountID:         accountID,
		AuthMode:          authMode,
		Email:             email,
		ExpiresAt:         expiresAt,
		AccountUserID:     accountUserIDFromClaims(claims),
		SubscriptionStart: subscriptionStartFromRoot(root),
	}, nil
}

// subscriptionStartFromRoot reads chatgpt_subscription_active_start out of the
// credential's id_token.
//
// It lives in the id_token rather than the access token, and it survives a
// refresh because it describes the subscription rather than the session. A
// credential without an id_token, or from before this claim existed, yields the
// zero time and callers fall back to not using it.
func subscriptionStartFromRoot(root map[string]any) time.Time {
	token := strings.TrimSpace(stringValue(root["id_token"]))
	if token == "" {
		return time.Time{}
	}
	auth, ok := jwtPayload(token)["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		return time.Time{}
	}
	return timeFromValue(auth["chatgpt_subscription_active_start"])
}

// accountUserIDFromClaims reads the id the real add-in sends as
// x-openai-account-user-id. An absent claim yields an empty string and the
// header is then left off, which is what this relay does today anyway.
func accountUserIDFromClaims(claims map[string]any) string {
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if id := firstString(auth, "chatgpt_account_user_id"); id != "" {
			return id
		}
	}
	return firstString(claims, "chatgpt_account_user_id")
}

func findToken(root map[string]any) string {
	for _, key := range []string{"access_token", "accessToken"} {
		if token := stringValue(root[key]); token != "" {
			return strings.TrimPrefix(strings.TrimSpace(token), "Bearer ")
		}
	}
	for _, key := range []string{"token_data", "tokenData", "sessionInfo", "session_info", "oauth", "tokens"} {
		if nested, ok := root[key].(map[string]any); ok {
			if token := findToken(nested); token != "" {
				return token
			}
		}
	}
	return ""
}

func findAccountID(root map[string]any) string {
	for _, key := range []string{"userInfo", "user_info", "auth", "token_data", "tokenData", "sessionInfo", "session_info"} {
		if nested, ok := root[key].(map[string]any); ok {
			if id := firstString(nested, "chatgpt_account_id", "account_id", "accountId"); id != "" {
				return id
			}
		}
	}
	return firstString(root, "chatgpt_account_id", "account_id", "accountId")
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(object[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value
		}
	}
	return nil
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

func accountIDFromClaims(claims map[string]any) string {
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if accountID := firstString(auth, "chatgpt_account_id", "account_id"); accountID != "" {
			return accountID
		}
	}
	return firstString(claims, "chatgpt_account_id", "account_id")
}

func jwtExpiry(claims map[string]any) time.Time {
	if claims == nil {
		return time.Time{}
	}
	if value, ok := claims["exp"]; ok {
		return timeFromValue(value)
	}
	return time.Time{}
}

func timeFromValue(value any) time.Time {
	switch number := value.(type) {
	case json.Number:
		if seconds, err := number.Int64(); err == nil && seconds > 0 {
			return time.Unix(seconds, 0)
		}
	case float64:
		if number > 0 {
			return time.Unix(int64(number), 0)
		}
	case int64:
		if number > 0 {
			return time.Unix(number, 0)
		}
	case string:
		if timestamp, err := time.Parse(time.RFC3339, strings.TrimSpace(number)); err == nil {
			return timestamp
		}
	}
	return time.Time{}
}

func credentialID(fileName string) string {
	base := strings.TrimSpace(filepath.Base(fileName))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "chatgpt"
	}
	var builder strings.Builder
	builder.WriteString("bp-")
	for _, character := range base {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	return builder.String()
}

// credentialDisabled reports whether the credential file says the operator
// has disabled this credential. The host writes the flag with this exact key
// (setSourceAuthFileDisabled), and anything unparseable means not disabled, so
// a damaged file never silently takes a working credential out of service.
func credentialDisabled(raw []byte) bool {
	var root struct {
		Disabled *bool `json:"disabled"`
	}
	if json.Unmarshal(raw, &root) != nil || root.Disabled == nil {
		return false
	}
	return *root.Disabled
}

func authData(raw []byte, fileName string, c credential, providerID string) map[string]any {
	id := credentialID(fileName)
	label := c.Email
	if label == "" {
		label = fileName
	}
	return map[string]any{
		// Must equal the value auth.identifier reports, or the host cannot
		// resolve this plugin for auth.refresh and the credential stops
		// renewing silently.
		"Provider": providerID,
		"ID":       id,
		"FileName": fileName,
		"Label":    label,
		// Whether the operator has taken this credential out of service.
		//
		// The host turns this field into the record's status, and a field this
		// map leaves out decodes as false. Omitting it therefore re-enabled a
		// credential every time its record was rebuilt -- which a token refresh
		// does on its own schedule, so a credential disabled in the panel came
		// back by itself minutes later. The flag lives in the credential file,
		// where the host writes it, so it is read back from there.
		"Disabled":    credentialDisabled(raw),
		"StorageJSON": raw,
		"Metadata": withRefreshTimestamps(raw, c, withNote(raw, map[string]any{
			"type":       Provider,
			"auth_kind":  "oauth",
			"account_id": c.AccountID,
			"auth_mode":  c.AuthMode,
			"file_name":  fileName,
			// The host reads the bearer token for its own outbound calls from
			// this key (management.tokenValueFromMetadata), which is how the
			// native Codex records carry theirs. Without it /v0/management
			// /api-call cannot substitute $TOKEN$ and the control panel's quota
			// card fails with "auth token not found", even though the
			// credential itself is perfectly usable for serving requests.
			"access_token": c.AccessToken,
			// Read by the host before its own configuration: a credential
			// refusing a request for being at its concurrency cap, or one
			// whose model Basis Points kept re-routing to a variant it cannot
			// serve, is skipped for the next one instead of being cooled down.
			"request_scoped_errors": []any{concurrencyRequestScopedRule(), modelRerouteRequestScopedRule()},
			// CPA only schedules auto-refresh for providers registered in its
			// own refreshLeadFactories (codex/claude/...), which a plugin cannot
			// extend. Manager.shouldRefresh falls back to authPreferredInterval
			// first, so publishing an interval here is what actually gets this
			// credential into the refresh loop.
			"refresh_interval_seconds": int(refreshInterval / time.Second),
		})),
		"Attributes": map[string]string{
			"auth_kind":  "oauth",
			"account_id": c.AccountID,
			"auth_mode":  c.AuthMode,
		},
	}
}

// credentialNote returns the note the operator attached to this credential in
// the control panel, and whether the file carries one at all.
//
// The panel saves it into the credential file under this exact key
// (management PatchAuthFileFields), and saves "" when the operator clears it.
// Anything unparseable means no note, for the same reason credentialDisabled
// reads a damaged file as not disabled: a file this function cannot read
// should not make it invent state.
func credentialNote(raw []byte) (string, bool) {
	var root struct {
		Note *string `json:"note"`
	}
	if json.Unmarshal(raw, &root) != nil || root.Note == nil {
		return "", false
	}
	return strings.TrimSpace(*root.Note), true
}

// withNote puts the operator's note into the metadata handed back to the host.
//
// The panel draws the note on a credential's card from the management list,
// and the list reads it from the record. For a file the host parses itself,
// the host copies "note" out of the file into the record. For a file a plugin
// claims it does not: the record's metadata is exactly what the plugin
// returns. Left out here, a note was accepted with a 200, written to disk, and
// then never shown -- on every credential in the pool, since all of them are
// this plugin's.
//
// It goes in Metadata rather than Attributes, where the host puts it for its
// own files, because metadata is what the host lays over the file on its next
// save. A token refresh finishes with the file as it was when the refresh
// began, so a note edited in the panel while one was in flight was silently
// put back to what it had been; with the edited note in the metadata, the
// edit is what gets written.
//
// For the same reason a cleared note is passed on as "" and not left out.
// Only a key that is present can overrule the refresh's older copy of the
// file: left out, a note cleared during a refresh came straight back. A file
// that has never carried the key gets none, so every credential nobody has
// annotated keeps exactly the record it had before.
func withNote(raw []byte, metadata map[string]any) map[string]any {
	if note, ok := credentialNote(raw); ok {
		metadata["note"] = note
	}
	return metadata
}

// withRefreshTimestamps publishes, in the record's metadata, when this
// credential's tokens were last renewed and when its access token expires.
//
// The host schedules renewals from the record, not from the file. It takes the
// time of the last renewal from the record's own LastRefreshedAt and falls back
// to metadata "last_refresh"; the file's last_refresh is never consulted.
// LastRefreshedAt is in-memory state: zero after a restart, and dropped whenever
// the host rebuilds a record from a changed file while the credential is
// disabled (prepareCoreAuthForModelRegistration keeps it only when both the old
// and the new record are enabled). With neither source the host treats the
// credential as never renewed and renews it at once. For a disabled credential
// that renewal rewrote the file, the rebuild dropped the timestamp again, and
// the next check renewed it again -- about once a second per disabled
// credential, each time rotating the refresh token at the token endpoint:
// roughly 2,700 file rewrites a minute on the 2026-10-06 pool, until
// auth.openai.com answered 429 to everyone for a while. After a restart the
// same gap made every credential in the pool renew at once.
//
// Publishing the file's last_refresh closes both gaps: a rebuilt record keeps
// its place in the renewal cadence whether or not it is disabled. "expired" is
// the access token's own expiry (parseCredential already prefers the JWT
// claim), under the key the host's ExpirationTime looks for, so the schedule
// no longer depends on the host decoding the token itself.
//
// A credential that has never been renewed carries no last_refresh and gets no
// key: the host then renews it once, writes last_refresh into the file, and
// from there on the cadence is anchored.
func withRefreshTimestamps(raw []byte, c credential, metadata map[string]any) map[string]any {
	if lastRefresh := credentialLastRefresh(raw); !lastRefresh.IsZero() {
		metadata["last_refresh"] = lastRefresh.UTC().Format(time.RFC3339)
	}
	if !c.ExpiresAt.IsZero() {
		metadata["expired"] = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return metadata
}

// credentialLastRefresh reads the time of the last renewal out of the
// credential file: the RFC 3339 string authRefresh writes, or a Unix timestamp
// from a credential imported from elsewhere. Anything else means never renewed.
func credentialLastRefresh(raw []byte) time.Time {
	var root struct {
		LastRefresh any `json:"last_refresh"`
	}
	if json.Unmarshal(raw, &root) != nil {
		return time.Time{}
	}
	return timeFromValue(root.LastRefresh)
}

// refreshFailure turns a non-2xx answer from the token endpoint into the error
// the host sees, with a status that says what actually happened.
//
// Before h19 every refusal came back as 401 "refresh_rejected". To the host a
// 401 from a refresh means the credential itself is bad: v7 stops retrying once
// the access token has also expired, and v8 treats the credential as
// terminally unauthorized as soon as the 401 arrives. But on 2026-10-06 three
// out of four refusals were 429s -- the endpoint rate-limiting this address
// after the disabled-credential refresh storm -- with nothing wrong with the
// credentials. Under v8 one such refusal during a credential's normal renewal
// would have taken it out of service for good.
//
// Only 400 and 401 are the credential's own fault (invalid_grant for a revoked
// or rotated-away refresh token, invalid_client ...), and those keep the 401,
// with the endpoint's error code in the message so the host can tell
// invalid_grant apart from the rest. A 429 stays a 429, with the endpoint's
// Retry-After when it sends one. Everything else is the endpoint's trouble,
// not the credential's, and is reported as the gateway failure it is (503
// kept as 503, anything else as 502), so the host retries on its usual backoff.
func refreshFailure(response upstreamResponse) error {
	status := response.StatusCode
	switch {
	case status == http.StatusTooManyRequests:
		message := fmt.Sprintf("ChatGPT token refresh rate limited with status %d", status)
		if retryAfter := shortToken(response.Headers.Get("Retry-After")); retryAfter != "" {
			message += "; Retry-After " + retryAfter
		}
		return fail(http.StatusTooManyRequests, "refresh_rate_limited", message)
	case status == http.StatusBadRequest || status == http.StatusUnauthorized:
		message := fmt.Sprintf("ChatGPT token refresh rejected with status %d", status)
		if code := oauthErrorCode(response.Body); code != "" {
			message += " (" + code + ")"
		}
		return fail(http.StatusUnauthorized, "refresh_rejected", message)
	case status == http.StatusServiceUnavailable:
		return fail(http.StatusServiceUnavailable, "refresh_upstream_error", fmt.Sprintf("ChatGPT token refresh failed upstream with status %d", status))
	default:
		return fail(http.StatusBadGateway, "refresh_upstream_error", fmt.Sprintf("ChatGPT token refresh failed upstream with status %d", status))
	}
}

// oauthErrorCode returns the OAuth error code ("invalid_grant", ...) from a
// token endpoint refusal, or "" when the body carries none. Only the short code
// is passed on: the body is written by the other side and ends up in the host's
// log, so nothing free-form from it is repeated.
func oauthErrorCode(body []byte) string {
	var payload struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	code := stringValue(payload.Error)
	if object, ok := payload.Error.(map[string]any); ok {
		code = firstString(object, "code", "type")
	}
	return shortToken(code)
}

// shortToken keeps a short value made only of the characters a Retry-After
// header or an OAuth error code can contain, so a surprising header or body
// cannot carry anything else into the log line.
func shortToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 40 {
		return ""
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '_', character == '-', character == ':', character == ',', character == ' ':
		default:
			return ""
		}
	}
	return value
}

// nativeCodexProvider is the provider CPA assigns to a `type: codex` file.
const nativeCodexProvider = "codex"

// authProviderID is the credential type this plugin owns.
//
// CPA stores one provider per auth record, and that single value decides both
// which plugin parses the file AND which executor serves requests made with it.
// A record labelled "codex" is therefore handed to the host's native Codex
// executor no matter which models this plugin registered -- verified on a live
// gateway, where adopting codex files made every gpt-6-astra request fail with
// "upstream execution failed: provider=codex ... 401".
//
// So a plugin can only ever serve credentials whose file type already equals
// its own provider. Adopting native `type: codex` files is not possible without
// giving up the plugin's own executor, and labelling the record
// "oai-basispoints" while claiming codex files instead breaks auth.refresh,
// because the host resolves the refreshing plugin by that same value.
func (s *Service) authProviderID() string {
	return AuthProviderID
}

func (s *Service) authParse(raw []byte) (map[string]any, error) {
	var request authParseRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	// Which credential files this plugin claims.
	//
	// Its own type is always claimed. A native `type: codex` file is claimed
	// only when the operator asked for it with claim_codex_auths, because CPA
	// gives each file to exactly one provider: adopting one removes it from the
	// native Codex channel. 0.1.1 claimed "codex", "openai" and the empty
	// provider unconditionally, so CPA handed it every Codex OAuth file on the
	// gateway; once a plugin answers Handled=true the host skips its own
	// parsing, and those credentials were replaced by records holding no
	// tokens, which broke every existing Codex model.
	//
	// Anything not claimed is handed straight back, so the host parses it
	// exactly as it would if this plugin were not installed.
	//
	// Exactly one auth is returned so the host never stamps plugin_virtual
	// (it only does that when len(auths) > 1), which would disable persistence
	// and token renewal.
	if strings.ToLower(strings.TrimSpace(request.Provider)) != s.authProviderID() {
		return map[string]any{"Handled": false}, nil
	}
	fileName := strings.TrimSpace(request.FileName)
	if fileName == "" {
		fileName = filepath.Base(strings.TrimSpace(request.Path))
	}
	c, err := parseCredential(request.RawJSON)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Handled": true, "Auth": authData(request.RawJSON, fileName, c, s.authProviderID())}, nil
}

// OAuth endpoint used to renew the ChatGPT access token. These values mirror
// CPA's own native Codex refresh (internal/auth/codex/openai_auth.go) so the
// renewed credential is byte-compatible with what the host would have minted.
const (
	openAITokenURL   = "https://auth.openai.com/oauth/token"
	openAIClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAITokenScope = "openid profile email"

	// ChatGPT access tokens live about ten days; refresh well inside that so a
	// restart never starts from an already-dead credential.
	refreshInterval = 12 * time.Hour
)

// refreshForm builds the refresh_token grant. The client_id follows the
// credential: add-in credentials carry client_id=app_fnr0... and must refresh
// under that client -- refreshing an add-in refresh_token under the Codex
// client returns 401 invalid_client (verified 2026-10-03). Scope is sent only
// for the default Codex client; for other clients it is omitted so the server
// reuses the originally granted scope (which includes offline_access, so the
// refresh_token keeps rotating) -- verified against a real add-in token.
func refreshForm(root map[string]any, refreshToken string) url.Values {
	clientID := firstString(root, "client_id", "clientID")
	if clientID == "" {
		clientID = openAIClientID
	}
	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if clientID == openAIClientID {
		form.Set("scope", openAITokenScope)
	}
	return form
}

// authRefresh renews the access token with the stored refresh token.
//
// 0.1.1 only re-parsed the same StorageJSON and returned 401 once it expired,
// so the credential died permanently after roughly ten days. It also derived a
// new FileName/ID on every call (credentialID re-applied its "bp-" prefix),
// which broke the host's identity contract. This version performs the real
// refresh_token grant, preserves the record identity handed in by the host, and
// writes the renewed tokens back into StorageJSON so CPA can persist them.
func (s *Service) authRefresh(raw []byte) (map[string]any, error) {
	var request authRefreshRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(request.StorageJSON, &root); err != nil || root == nil {
		return nil, fail(400, "invalid_auth", "ChatGPT OAuth credential is not a JSON object")
	}
	original, err := workspaceRefreshIdentityFromToken(findToken(root))
	if err != nil {
		return nil, err
	}
	if clientID := firstString(root, "client_id", "clientID"); clientID != "" && clientID != original.clientID {
		return nil, fail(502, "refresh_identity_mismatch", "ChatGPT credential client does not match its access token")
	}
	if pending, exists := root["workspace_refresh_pending"]; exists {
		marker, ok := pending.(map[string]any)
		if !ok || firstString(marker, "expected_account_id") != original.accountID {
			return nil, fail(502, "refresh_pending_invalid", "ChatGPT workspace recovery marker does not match its access token")
		}
		// Recovery reauthorizes the expected workspace separately. Never spend
		// another rotated refresh token while that work is pending.
		return s.workspaceRefreshResult(request, root, time.Now().Add(10*time.Minute))
	}
	refreshToken := firstString(root, "refresh_token", "refreshToken")
	if refreshToken == "" {
		return nil, fail(401, "invalid_auth", "ChatGPT OAuth credential has no refresh_token")
	}

	form := refreshForm(root, refreshToken)
	form.Set("client_id", original.clientID)
	if original.clientID != openAIClientID {
		form.Del("scope")
	}
	var response upstreamResponse
	callErr := s.call("host.http.do", map[string]any{
		"method": http.MethodPost,
		"url":    openAITokenURL,
		"headers": map[string][]string{
			"Content-Type": {"application/x-www-form-urlencoded"},
			"Accept":       {"application/json"},
			"User-Agent":   {chromeUserAgent},
		},
		"body": []byte(form.Encode()),
	}, &response)
	if callErr != nil {
		return nil, fail(502, "refresh_transport", "ChatGPT token refresh failed: "+safeError(callErr))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, refreshFailure(response)
	}

	var grant struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(response.Body, &grant); err != nil || strings.TrimSpace(grant.AccessToken) == "" {
		return nil, fail(502, "refresh_invalid", "ChatGPT token refresh returned no access_token")
	}
	refreshed, err := workspaceRefreshIdentityFromToken(grant.AccessToken)
	if err != nil {
		return nil, err
	}
	if refreshed.clientID != original.clientID || !workspaceRefreshSameUser(original.users, refreshed.users) {
		return nil, fail(502, "refresh_identity_mismatch", "ChatGPT token refresh changed the credential user or client")
	}
	if !jwtExpiry(jwtPayload(grant.AccessToken)).After(time.Now()) {
		return nil, fail(502, "refresh_identity_invalid", "ChatGPT token refresh returned an expired access token")
	}
	if err := validateWorkspaceRefreshIDToken(grant.IDToken, refreshed); err != nil {
		return nil, err
	}
	pendingReason := ""
	if refreshed.accountID != original.accountID {
		pendingReason = "workspace_changed_during_refresh"
	} else if workspaceRefreshBusinessPlan(original.plan) && !workspaceRefreshBusinessPlan(refreshed.plan) {
		pendingReason = "plan_mismatch"
	}
	if pendingReason != "" {
		if strings.TrimSpace(grant.RefreshToken) != "" {
			root["refresh_token"] = grant.RefreshToken
		}
		now := time.Now()
		root["last_refresh"] = now.UTC().Format(time.RFC3339)
		root["workspace_refresh_pending"] = map[string]any{
			"expected_account_id": original.accountID,
			"observed_account_id": refreshed.accountID,
			"detected_at":         now.UTC().Format(time.RFC3339),
			"reason":              pendingReason,
		}
		return s.workspaceRefreshResult(request, root, now.Add(10*time.Minute))
	}

	root["access_token"] = grant.AccessToken
	if strings.TrimSpace(grant.RefreshToken) != "" {
		root["refresh_token"] = grant.RefreshToken
	}
	if strings.TrimSpace(grant.IDToken) != "" {
		root["id_token"] = grant.IDToken
	}
	now := time.Now()
	expiresAt := jwtExpiry(jwtPayload(grant.AccessToken))
	if expiresAt.IsZero() && grant.ExpiresIn > 0 {
		expiresAt = now.Add(time.Duration(grant.ExpiresIn) * time.Second)
	}
	if !expiresAt.IsZero() {
		root["expired"] = expiresAt.UTC().Format(time.RFC3339)
	}
	root["last_refresh"] = now.UTC().Format(time.RFC3339)
	delete(root, "workspace_refresh_pending")

	updated, err := json.Marshal(root)
	if err != nil {
		return nil, fail(500, "refresh_encode", "failed to encode refreshed ChatGPT credential")
	}
	c, err := parseCredential(updated)
	if err != nil {
		return nil, err
	}

	// Keep the identity the host gave us. Re-deriving it through credentialID
	// would prepend another "bp-" on every refresh and orphan the record.
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

	next := now.Add(10 * time.Minute)
	if !c.ExpiresAt.IsZero() {
		next = c.ExpiresAt.Add(-5 * time.Minute)
		if next.Before(now.Add(time.Minute)) {
			next = now.Add(time.Minute)
		}
	}
	return map[string]any{
		"Auth":             auth,
		"NextRefreshAfter": next.UTC(),
	}, nil
}

func credentialFromExecutor(request ExecutorRequest) (credential, error) {
	if len(request.StorageJSON) > 0 {
		return parseCredential(request.StorageJSON)
	}
	metadata := map[string]any{}
	for key, value := range request.AuthMetadata {
		metadata[key] = value
	}
	for key, value := range request.AuthAttributes {
		metadata[key] = value
	}
	if token := stringValue(metadata["access_token"]); token != "" {
		data := map[string]any{"access_token": token}
		if accountID := stringValue(metadata["account_id"]); accountID != "" {
			data["account_id"] = accountID
		}
		return parseCredential(jsonBytes(data))
	}
	return credential{}, fail(401, "missing_auth", "CPA did not provide a ChatGPT OAuth credential")
}

func redactTokenMessage(message string) string {
	for _, prefix := range []string{"Bearer ", "bearer "} {
		if index := strings.Index(message, prefix); index >= 0 {
			end := index + len(prefix)
			for end < len(message) && message[end] != ' ' && message[end] != '"' && message[end] != '\n' {
				end++
			}
			message = message[:index] + prefix + "[REDACTED]" + message[end:]
		}
	}
	return fmt.Sprintf("%s", message)
}

// credentialPlan reports the ChatGPT plan tier carried in the access token, for
// display alongside quota. It is best-effort: an unparsable token yields "".
func credentialPlan(c credential) string {
	claims := jwtPayload(c.AccessToken)
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if plan := firstString(auth, "chatgpt_plan_type", "plan_type"); plan != "" {
			return plan
		}
	}
	return firstString(claims, "chatgpt_plan_type", "plan_type")
}
