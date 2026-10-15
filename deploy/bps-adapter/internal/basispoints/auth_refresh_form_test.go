package basispoints

import "testing"

// Stage-2 refresh retarget: the refresh grant must follow the credential's own
// client_id so add-in (app_fnr0...) tokens refresh under the add-in client, not
// the hard-coded Codex client.
func TestRefreshFormUsesCredentialClientID(t *testing.T) {
	form := refreshForm(map[string]any{"client_id": "app_fnr0pYvVwwFDocDumLG3H2Bp"}, "rt123")
	if got := form.Get("client_id"); got != "app_fnr0pYvVwwFDocDumLG3H2Bp" {
		t.Fatalf("client_id = %q, want add-in client", got)
	}
	// Scope must be omitted for a non-Codex client so the server reuses the
	// originally granted scope (incl. offline_access) and keeps rotating the RT.
	if form.Has("scope") {
		t.Fatalf("scope must be omitted for non-Codex client, got %q", form.Get("scope"))
	}
	if got := form.Get("refresh_token"); got != "rt123" {
		t.Fatalf("refresh_token = %q", got)
	}
	if got := form.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type = %q", got)
	}
}

func TestRefreshFormDefaultsToCodex(t *testing.T) {
	form := refreshForm(map[string]any{}, "rt")
	if got := form.Get("client_id"); got != openAIClientID {
		t.Fatalf("default client_id = %q, want Codex", got)
	}
	if got := form.Get("scope"); got != openAITokenScope {
		t.Fatalf("Codex scope = %q, want %q", got, openAITokenScope)
	}
}
