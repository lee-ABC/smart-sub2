package basispoints

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func shadowJSON(t *testing.T, raw string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestH24ShadowLibraryObservesWithoutChangingLegacyDecision(t *testing.T) {
	schema := shadowJSON(t, `{"type":"object","properties":{"name":{"type":"string","minLength":4}},"required":["name"]}`).(map[string]any)
	instance := shadowJSON(t, `{"name":"abc"}`)
	beforeSchema, beforeInstance := string(jsonBytes(schema)), string(jsonBytes(instance))
	before := schemaMatches(instance, schema)
	if !before {
		t.Fatal("fixture should be accepted by the existing compatibility checker")
	}
	observation := observeSchemaShadow(schema, instance)
	if observation.Code != "mismatch" || observation.Draft != "2020-12" {
		t.Fatalf("observation = %+v, want mature-validator mismatch", observation)
	}
	if schemaMatches(instance, schema) != before || string(jsonBytes(schema)) != beforeSchema || string(jsonBytes(instance)) != beforeInstance {
		t.Fatal("shadow observation changed the legacy decision or request data")
	}
}

func TestH24ShadowSupportedDraftsAndAssertions(t *testing.T) {
	for _, tc := range []struct {
		name, schema, instance, want, draft string
	}{
		{"local_ref", `{"$defs":{"city":{"type":"string"}},"type":"object","properties":{"city":{"$ref":"#/$defs/city"}},"required":["city"]}`, `{"city":"Paris"}`, "match", "2020-12"},
		{"local_ref_mismatch", `{"$defs":{"city":{"type":"string"}},"properties":{"city":{"$ref":"#/$defs/city"}}}`, `{"city":7}`, "mismatch", "2020-12"},
		{"draft7", `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","definitions":{"value":{"enum":[1,2]}},"properties":{"n":{"$ref":"#/definitions/value"}}}`, `{"n":2}`, "match", "draft-07"},
		{"pattern", `{"type":"object","patternProperties":{"^S_":{"type":"string"}},"additionalProperties":false}`, `{"S_city":"Paris"}`, "match", "2020-12"},
		{"closed_object", `{"type":"object","properties":{"known":{}},"additionalProperties":false}`, `{"other":1}`, "mismatch", "2020-12"},
		{"numeric_exact", `{"properties":{"n":{"const":9007199254740993}}}`, `{"n":9007199254740992}`, "mismatch", "2020-12"},
		{"allof", `{"allOf":[{"type":"integer"},{"minimum":5}]}`, `4`, "mismatch", "2020-12"},
		{"true_schema", `true`, `{}`, "match", "2020-12"},
		{"false_schema", `false`, `{}`, "mismatch", "2020-12"},
		{"invalid_schema", `{"type":7}`, `{}`, "compile_error", "2020-12"},
		{"unknown_draft", `{"$schema":"https://example.invalid/custom-schema"}`, `{}`, "unsupported", "unknown"},
		{"unknown_keyword", `{"futureAssertion":true}`, `{}`, "unsupported", "2020-12"},
		{"recursive", `{"$ref":"#"}`, `{}`, "unsupported", "2020-12"},
		{"nested_draft", `{"properties":{"x":{"$schema":"http://json-schema.org/draft-07/schema#"}}}`, `{}`, "unsupported", "2020-12"},
		{"complex_regex", `{"pattern":"(?=x)x"}`, `"x"`, "unsupported", "2020-12"},
		{"unicode_regex", `{"pattern":"^.$"}`, `"😀"`, "unsupported", "2020-12"},
		{"format", `{"format":"regex"}`, `"(a+)+"`, "unsupported", "2020-12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := observeSchemaShadow(shadowJSON(t, tc.schema), shadowJSON(t, tc.instance))
			if got.Code != tc.want || got.Draft != tc.draft {
				t.Fatalf("observation = %+v, want %s/%s", got, tc.want, tc.draft)
			}
		})
	}
}

func TestH24ShadowNeverLoadsExternalSchema(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`false`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "private-schema.json")
	if err := os.WriteFile(path, []byte(`false`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{server.URL, "file:///" + filepath.ToSlash(path), "../other.json", "https://schema-shadow.invalid/selected-tool.json#/$defs/x"} {
		got := observeSchemaShadow(map[string]any{"$ref": ref}, map[string]any{})
		if got.Code != "unsupported" || got.Reason != "external_reference" {
			t.Fatalf("external reference was not rejected before loading: %+v", got)
		}
		if _, err := (shadowNoExternalLoader{}).Load(ref); err == nil || strings.Contains(err.Error(), ref) {
			t.Fatal("deny loader accepted or disclosed a requested resource")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("shadow validator accessed the network")
	}
}

func TestH24ShadowResourceAdmissionAndPrivacy(t *testing.T) {
	const secret = "PRIVATE_TOOL_ARGUMENT_CIPHERTEXT_MUST_NOT_APPEAR"
	for _, tc := range []struct {
		schema, instance any
		want             string
	}{
		{map[string]any{"type": secret}, map[string]any{"argument": secret}, "compile_error"},
		{map[string]any{"$ref": "https://" + secret + ".invalid/secret"}, nil, "unsupported"},
		{map[string]any{"description": strings.Repeat("s", shadowSchemaBytes+1)}, nil, "unknown"},
		{map[string]any{}, strings.Repeat("a", shadowInstanceBytes+1), "unknown"},
		{map[string]any{}, json.Number("1e1000000000"), "unknown"},
		{map[string]any{}, json.Number(strings.Repeat("9", shadowNumberDigits+1)), "unknown"},
		{nil, nil, "unsupported"},
	} {
		got := observeSchemaShadow(tc.schema, tc.instance)
		if got.Code != tc.want || strings.Contains(string(jsonBytes(got)), secret) {
			t.Fatalf("unsafe/inaccurate diagnostic: %+v", got)
		}
	}
	cyclic := map[string]any{}
	cyclic["properties"] = map[string]any{"self": cyclic}
	if got := observeSchemaShadow(cyclic, nil); got.Code != "unknown" {
		t.Fatalf("cyclic Go tree bypassed pre-marshalling budget: %+v", got)
	}
	instance := make([]any, shadowDocumentNodes+1)
	if got := observeSchemaShadow(map[string]any{}, instance); got.Code != "unknown" {
		t.Fatalf("large instance bypassed node budget: %+v", got)
	}

	// An acyclic reference graph can still expand exponentially. Inspect the
	// expanded graph with its own budget before the library compiles it.
	definitions := map[string]any{"leaf": map[string]any{"type": "string"}}
	previous := "leaf"
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("node%d", i)
		definitions[name] = map[string]any{"allOf": []any{map[string]any{"$ref": "#/$defs/" + previous}, map[string]any{"$ref": "#/$defs/" + previous}}}
		previous = name
	}
	got := observeSchemaShadow(map[string]any{"$defs": definitions, "$ref": "#/$defs/" + previous}, "x")
	if got.Code != "unknown" || got.Reason != "schema_expansion_budget" {
		t.Fatalf("expanded reference budget was not enforced: %+v", got)
	}
}

func TestH24ShadowBusyDoesNotStartAnotherWorker(t *testing.T) {
	schemaShadowSlot <- struct{}{}
	defer func() { <-schemaShadowSlot }()
	got := observeSchemaShadow(map[string]any{}, map[string]any{})
	if got.Code != "unknown" || got.Reason != "busy" {
		t.Fatalf("busy observer did not skip work: %+v", got)
	}
}

func TestH24ShadowPointerEscapesAndCycles(t *testing.T) {
	schema := shadowJSON(t, `{"$defs":{"a/b~c":{"type":"integer"}},"$ref":"#/$defs/a~1b~0c"}`)
	if got := observeSchemaShadow(schema, json.Number("3")); got.Code != "match" {
		t.Fatalf("local pointer escape was not resolved: %+v", got)
	}
	for _, raw := range []string{
		`{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`,
		`{"$defs":{"a":{"$ref":"#/%24defs/a"}},"$ref":"#/$defs/a"}`,
	} {
		if got := observeSchemaShadow(shadowJSON(t, raw), nil); got.Code != "unsupported" || got.Reason != "recursive_reference" {
			t.Fatalf("recursive local reference was not skipped: %+v", got)
		}
	}
}
