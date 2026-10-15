package basispoints

import (
	"encoding/json"
	"strings"
	"testing"
)

func concurrencyListHost(files string) func(method string, payload any, out any) error {
	return func(method string, payload any, out any) error {
		if method != "host.auth.list" {
			return nil
		}
		return json.Unmarshal([]byte(files), out)
	}
}

// The page writes a cap back under the key it was found under, so editing an
// override set by file name replaces it instead of adding a second entry.
func TestConcurrencyRowsCarryTheKeyThePageEdits(t *testing.T) {
	service := NewService()
	service.cfg.CredentialConcurrency = map[string]int{"b@example.com": 2, "c-file": 4}
	service.SetHost(concurrencyListHost(`{"files":[
		{"name":"a.json","label":"A@Example.com","provider":"oai-basispoints"},
		{"name":"b.json","label":"b@example.com","provider":"oai-basispoints"},
		{"name":"c-file.json","label":"c@example.com","provider":"oai-basispoints"},
		{"name":"d.json","label":"","provider":"oai-basispoints"}]}`))

	byFile := map[string]concurrencyRow{}
	for _, row := range service.concurrencySnapshot().Rows {
		byFile[row.File] = row
	}
	for file, want := range map[string]struct {
		key      string
		limit    int
		override bool
	}{
		"a.json":      {"a@example.com", 5, false},
		"b.json":      {"b@example.com", 2, true},
		"c-file.json": {"c-file", 4, true},
		"d.json":      {"d", 5, false},
	} {
		got := byFile[file]
		if got.Key != want.key || got.Limit != want.limit || got.Override != want.override {
			t.Errorf("%s: key=%q limit=%d override=%v, want %q %d %v",
				file, got.Key, got.Limit, got.Override, want.key, want.limit, want.override)
		}
	}
}

// A key the page writes must be one concurrencyLimit reads back.
func TestOverrideKeyRoundTripsThroughTheLookup(t *testing.T) {
	for _, tc := range []struct{ email, file string }{
		{"Someone@Example.com", "x.json"},
		{"", "codex_accounts_cpa_01_x.json"},
	} {
		cfg := defaultConfig()
		cfg.MaxConcurrencyPerCredential = 3
		key := cfg.concurrencyOverrideKey(tc.email, tc.file)
		cfg.CredentialConcurrency = map[string]int{key: 9}
		if err := cfg.normalize(); err != nil {
			t.Fatal(err)
		}
		if limit, override := cfg.concurrencyLimit(tc.email, tc.file); limit != 9 || !override {
			t.Errorf("%+v: key %q gave limit %d override %v, want 9 true", tc, key, limit, override)
		}
	}
}

// The page edits through CPA's own authenticated management API and keeps the
// key only for the save it was typed for.
func TestConcurrencyPageSavesThroughTheManagementAPI(t *testing.T) {
	service := NewService()
	service.SetHost(concurrencyListHost(`{"files":[{"name":"a.json","label":"a@example.com","provider":"oai-basispoints"}]}`))
	page := service.renderConcurrencyPage()
	for _, want := range []string{
		`var CONFIG_API="/v0/management/plugins/` + PluginID + `/config"`,
		`id="save"`, `id="def"`, `id="bulk"`, `type="password"`,
		`method:"PATCH"`, `credential_concurrency`, `max_concurrency_per_credential`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %s", want)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", "document.cookie"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page touches %s; the key must not be stored", forbidden)
		}
	}
}
