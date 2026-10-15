package basispoints

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestH23SchemaCompatibilityBaseline(t *testing.T) {
	for _, tc := range []struct {
		name, value, schema string
		accept              bool
	}{
		{"pattern_permits_field", `{"S_name":"ok"}`, `{"type":"object","properties":{},"patternProperties":{"^S_":{"type":"string"}},"additionalProperties":false}`, true},
		{"pattern_is_unanchored", `{"foo":"ok"}`, `{"type":"object","properties":{},"patternProperties":{"o":{"type":"string"}},"additionalProperties":false}`, true},
		{"property_and_pattern_both_apply", `{"x":"ok"}`, `{"type":"object","properties":{"x":{"type":"string"}},"patternProperties":{"^x":{"type":"number"}}}`, false},
		{"unknown_ecma_regex_is_not_extra", `{"foo":"ok"}`, `{"type":"object","properties":{},"patternProperties":{"(?=foo)foo":{"type":"string"}},"additionalProperties":false}`, true},
		{"genuine_extra_without_properties", `{"other":true}`, `{"type":"object","patternProperties":{"^ok":{"type":"boolean"}},"additionalProperties":false}`, false},
		{"required_without_explicit_type", `{}`, `{"required":["city"]}`, false},
		{"property_schema_without_explicit_type", `{"city":7}`, `{"properties":{"city":{"type":"string"}}}`, false},
		{"boolean_property_schema", `{"blocked":true}`, `{"type":"object","properties":{"blocked":false}}`, false},
		{"additional_schema", `{"other":7}`, `{"type":"object","additionalProperties":{"type":"string"}}`, false},
		{"enum_string_not_number", `{"n":"1"}`, `{"properties":{"n":{"enum":[1]}}}`, false},
		{"unknown_does_not_hide_required", `{}`, `{"type":"object","required":["city"],"some_future_keyword":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := schemaMatches(decodeToolJSONObject(tc.value), decodeToolJSONObject(tc.schema)); got != tc.accept {
				t.Fatalf("compatible=%v, want %v", got, tc.accept)
			}
		})
	}
}

func TestH23SchemaThreeStateAndIntersectingPatterns(t *testing.T) {
	for _, tc := range []struct {
		name, value, schema string
		want                schemaCompatibility
	}{
		{"two_matching_patterns", `{"abc":"ok"}`, `{"patternProperties":{"^a":{"type":"string"},"c$":{"enum":["ok"]}},"additionalProperties":false}`, schemaCompatible},
		{"conflicting_patterns", `{"abc":"ok"}`, `{"patternProperties":{"^a":{"type":"string"},"c$":{"type":"integer"}}}`, schemaMismatch},
		{"optional_empty_schema_is_declared", `{"anything":7}`, `{"properties":{"anything":{}},"additionalProperties":false}`, schemaCompatible},
		{"optional_property_absent", `{}`, `{"properties":{"anything":false},"additionalProperties":false}`, schemaCompatible},
		{"closed_empty_object", `{"x":7}`, `{"additionalProperties":false}`, schemaMismatch},
		{"known_match_wrong_type", `{"S_x":7}`, `{"patternProperties":{"^S_":{"type":"string"}}}`, schemaMismatch},
		{"lookahead_unknown", `{"foo":7}`, `{"patternProperties":{"(?=foo)foo":{"type":"string"}},"additionalProperties":false}`, schemaUnknown},
		{"invalid_regex_unknown", `{"foo":7}`, `{"patternProperties":{"[":{"type":"string"}},"additionalProperties":false}`, schemaUnknown},
		{"unicode_dialect_unknown", `{"é":7}`, `{"patternProperties":{"^.$":{"type":"string"}},"additionalProperties":false}`, schemaUnknown},
		{"end_anchor_dialect_unknown", "{\"foo\\n\":7}", `{"patternProperties":{"foo$":{"type":"string"}},"additionalProperties":false}`, schemaUnknown},
		{"unknown_pattern_preserves_known_error", `{"x":7}`, `{"properties":{"x":{"type":"string"}},"patternProperties":{"(?=x)":{"type":"number"}}}`, schemaMismatch},
		{"unknown_keyword", `{"x":7}`, `{"properties":{"x":{"type":"number"}},"futureAssertion":true}`, schemaUnknown},
		{"unknown_keyword_preserves_extra_error", `{"other":7}`, `{"properties":{"x":{}},"additionalProperties":false,"futureAssertion":true}`, schemaMismatch},
		{"ref_siblings_may_be_ignored", `{"x":"legal-for-referenced-schema"}`, `{"properties":{"x":{"$ref":"#/$defs/value","type":"number"}}}`, schemaUnknown},
		{"ref_does_not_erase_parent_required", `{}`, `{"properties":{"x":{"$ref":"#/$defs/value","type":"number"}},"required":["x"]}`, schemaMismatch},
		{"prefix_items_not_misapplied", `{"x":[1]}`, `{"properties":{"x":{"prefixItems":[{"type":"integer"}],"items":false}}}`, schemaUnknown},
		{"integer_integral_decimal", `{"n":3.000}`, `{"properties":{"n":{"type":"integer"}}}`, schemaCompatible},
		{"integer_fraction", `{"n":1e-1}`, `{"properties":{"n":{"type":"integer"}}}`, schemaMismatch},
		{"integer_large_exponent", `{"n":1e100000000}`, `{"properties":{"n":{"type":"integer"}}}`, schemaCompatible},
		{"boolean_items_schema", `{"x":[1]}`, `{"properties":{"x":{"type":"array","items":false}}}`, schemaMismatch},
		{"object_keywords_ignore_other_types", `{"x":7}`, `{"properties":{"x":{"required":["not_applicable"]}}}`, schemaCompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, schema := decodeToolJSONObject(tc.value), decodeToolJSONObject(tc.schema)
			for i := 0; i < 5; i++ {
				if got := checkSchemaCompatibility(value, schema); got != tc.want {
					t.Fatalf("state=%v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestH23SchemaBudgetsReturnUnknownWithoutOpeningKnownErrors(t *testing.T) {
	value := any(map[string]any{})
	schema := map[string]any{}
	for i := 0; i < 1000; i++ {
		value = map[string]any{"a": value}
		schema = map[string]any{"properties": map[string]any{"a": schema}}
	}
	if got := checkSchemaCompatibility(value, schema); got != schemaUnknown {
		t.Fatalf("deep schema state=%v", got)
	}
	large := map[string]any{"type": "string"}
	for i := 0; i < schemaMaxVisits+1; i++ {
		large[fmt.Sprintf("unknown_%d", i)] = true
	}
	if got := checkSchemaCompatibility(map[string]any{}, large); got != schemaMismatch {
		t.Fatalf("unknown keywords hid known type error: %v", got)
	}
	large["type"] = "object"
	large["required"] = []any{"missing"}
	if got := checkSchemaCompatibility(map[string]any{}, large); got != schemaMismatch {
		t.Fatalf("unknown keywords hid required error: %v", got)
	}
	patterns := map[string]any{}
	for i := 0; i < 1000; i++ {
		patterns[fmt.Sprintf("(?=a%d)", i)] = false
	}
	object := map[string]any{}
	for i := 0; i < 1000; i++ {
		object[fmt.Sprintf("key_%d", i)] = true
	}
	start := time.Now()
	if got := checkSchemaCompatibility(object, map[string]any{"patternProperties": patterns, "additionalProperties": false}); got != schemaUnknown {
		t.Fatalf("regex work state=%v", got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("bounded pattern work took over one second")
	}
	budget := newSchemaBudget()
	budget.visits = 1
	if got := schemaCheckWithin(map[string]any{"a": true}, map[string]any{"properties": map[string]any{"a": false}}, 0, budget); got != schemaUnknown {
		t.Fatalf("visit exhaustion=%v", got)
	}
	budget = newSchemaBudget()
	budget.visits = 2
	if got := equivalentJSONValuesWithin([]any{1, 1}, []any{1, 2}, 0, budget); got != schemaUnknown {
		t.Fatalf("an unexamined suffix was declared equal: %v", got)
	}
}

func TestH23EquivalentToolArguments(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		equal       bool
	}{
		{`{"a":1,"b":[true,null,"x"]}`, ` { "b": [true,null,"x"], "a":1.0 } `, true},
		{`{"n":1e3}`, `{"n":1000}`, true}, {`{"n":-0}`, `{"n":0.00}`, true},
		{`{"n":0.100}`, `{"n":1e-1}`, true},
		{`{"n":9007199254740993}`, `{"n":9007199254740993.0}`, true},
		{`{"n":9007199254740993}`, `{"n":9007199254740992}`, false},
		{`{"n":1e100000000}`, `{"n":10e99999999}`, true},
		{`{"n":"1"}`, `{"n":1}`, false}, {`{"n":true}`, `{"n":1}`, false},
		{`{"a":[1,2]}`, `{"a":[2,1]}`, false},
		{`{"a":" text "}`, `{"a":"text"}`, false},
		{`{} {}`, `{} {}`, false}, {`{} false`, `{}`, false},
		{`{"n":NaN}`, `{"n":NaN}`, false}, {` `, ` `, false},
		{`[1,{"b":2,"a":3}]`, `[1.0,{"a":3,"b":2e0}]`, true},
	} {
		if got := equivalentToolArguments(tc.left, tc.right); got != tc.equal {
			t.Fatalf("equivalence=%v, want %v for %q / %q", got, tc.equal, tc.left, tc.right)
		}
	}
	integer := strings.Repeat("9", 10000)
	if !equivalentToolArguments(`{"n":`+integer+`}`, `{"n":`+integer+`e0}`) {
		t.Fatal("large integer lost precision")
	}
	exp := strings.Repeat("9", 20000)
	if !equivalentToolArguments(`{"n":1e`+exp+`}`, `{"n":10e`+strings.Repeat("9", 19999)+`8}`) {
		t.Fatal("large exponent spelling lost precision")
	}
	if !equivalentToolArguments(`{"n":1e-`+exp+`}`, `{"n":10e-1`+strings.Repeat("0", 20000)+`}`) {
		t.Fatal("large negative exponent lost precision")
	}
	deep := strings.Repeat(`[`, 1000) + `1` + strings.Repeat(`]`, 1000)
	if !equivalentToolArguments(deep, " "+deep) {
		t.Fatal("valid deep JSON equality was mistaken for a change")
	}
	large := `{"text":"` + strings.Repeat("x", toolJSONMaxBytes) + `"}`
	if got := toolArgumentsCompatibility(large, large); got != schemaCompatible {
		t.Fatalf("identical valid large JSON state=%v", got)
	}
	if got := toolArgumentsCompatibility(large, " "+large); got != schemaUnknown {
		t.Fatalf("oversized comparison is %v, want unknown", got)
	}
}

func TestH23DecimalExponentArithmeticAgainstBigInt(t *testing.T) {
	for _, exponent := range []string{"0", "-0", "+0001", "-0001", "99", "-99", "99999999999999999999999999", "-99999999999999999999999999"} {
		for _, offset := range []int{-1000000, -100, -1, 0, 1, 100, 1000000} {
			base, ok := new(big.Int).SetString(exponent, 10)
			if !ok {
				t.Fatal("bad test fixture")
			}
			want := base.Add(base, big.NewInt(int64(offset))).String()
			if got := addDecimalExponent(exponent, offset); got != want {
				t.Fatalf("exponent %s %+d = %s, want %s", exponent, offset, got, want)
			}
		}
	}
	for _, invalid := range []json.Number{"true", "null", "NaN", "1e", "01", ""} {
		if _, ok := canonicalEnumNumber(invalid); ok {
			t.Fatalf("accepted invalid numeric value %q", invalid)
		}
	}
	if enumValuesEqual("1", json.Number("1")) {
		t.Fatal("enum string equaled number")
	}
	if !enumValuesEqual(decodeToolJSONObject(`{"x":1e3}`), decodeToolJSONObject(`{"x":1000}`)) {
		t.Fatal("nested enum numeric equality failed")
	}
}

func TestH23ExactVisitBudgetBoundaries(t *testing.T) {
	options := make([]any, schemaMaxVisits)
	for i := range options {
		options[i] = json.Number("0")
	}
	options[len(options)-1] = json.Number("1")
	if got := checkSchemaCompatibility(json.Number("1"), map[string]any{"enum": options}); got != schemaUnknown {
		t.Errorf("unvisited legal final enum option: state=%v, want unknown", got)
	}
	// One fewer option fits exactly: a real final match or a completely checked
	// miss is conclusive even though the last comparison used the last visit.
	if got := checkSchemaCompatibility(json.Number("0"), map[string]any{"enum": options[:len(options)-1]}); got != schemaCompatible {
		t.Errorf("checked enum match state=%v", got)
	}
	if got := checkSchemaCompatibility(json.Number("1"), map[string]any{"enum": options[:len(options)-1]}); got != schemaMismatch {
		t.Errorf("fully checked enum miss state=%v", got)
	}
	left, right := make([]any, schemaMaxVisits), make([]any, schemaMaxVisits)
	for i := range left {
		left[i], right[i] = json.Number("0"), json.Number("0")
	}
	right[len(right)-1] = json.Number("1")
	if got := equivalentJSONValuesWithin(left, right, 0, newSchemaBudget()); got != schemaUnknown {
		t.Errorf("unvisited differing array suffix: state=%v, want unknown", got)
	}
	if got := toolArgumentsCompatibility(string(jsonBytes(left)), string(jsonBytes(right))); got != schemaMismatch {
		t.Errorf("argument-specific input-size budget missed actual array change: %v", got)
	}
	leftMap, rightMap := map[string]any{}, map[string]any{}
	for i := 0; i < schemaMaxVisits; i++ {
		key := fmt.Sprintf("k%d", i)
		leftMap[key], rightMap[key] = true, true
	}
	if got := equivalentJSONValuesWithin(leftMap, rightMap, 0, newSchemaBudget()); got != schemaUnknown {
		t.Errorf("unvisited map members were asserted equal: %v", got)
	}
	rightMap["k19999"] = false
	for i := 0; i < 4; i++ {
		if got := equivalentJSONValuesWithin(leftMap, rightMap, 0, newSchemaBudget()); got == schemaCompatible {
			t.Errorf("large map with a differing member was declared equal")
		}
	}
	for _, value := range []any{[]any{true}, map[string]any{"a": true}} {
		budget := newSchemaBudget()
		budget.visits = 2
		if got := equivalentJSONValuesWithin(value, value, 0, budget); got != schemaCompatible {
			t.Errorf("fully examined final element used the last visit: %v", got)
		}
	}
}

func TestH23DecodeReasonCategories(t *testing.T) {
	for _, reason := range []string{"incomplete_item", "outer_json", "code_json", "missing_tool_name", "tool_not_in_catalog", "tool_not_allowed", "invalid_arguments_json", "schema_mismatch"} {
		t.Run(reason, func(t *testing.T) {
			source := weatherRequest()
			item := h21Native("h23_reason", "get_weather", map[string]any{"city": "Singapore"})
			switch reason {
			case "incomplete_item":
				item["status"] = "in_progress"
			case "outer_json":
				item["arguments"] = `{} {}`
			case "code_json":
				item["arguments"] = `{"code":"not JSON"}`
			case "missing_tool_name":
				item["arguments"] = `{"code":"{}"}`
			case "tool_not_in_catalog":
				item = h21Native("h23_reason", "unknown", map[string]any{})
			case "tool_not_allowed":
				source["tool_choice"] = "none"
			case "invalid_arguments_json":
				item = h21Native("h23_reason", "get_weather", `{} {}`)
			case "schema_mismatch":
				item = h21Native("h23_reason", "get_weather", map[string]any{"city": 7})
			}
			if _, got := decodeTransportItemWithReason(item, source); got != reason {
				t.Fatalf("reason=%q, want %q", got, reason)
			}
			if reason == "tool_not_allowed" && source["tool_choice"] != "none" {
				t.Fatal("choice filter was mutated")
			}
		})
	}
}
