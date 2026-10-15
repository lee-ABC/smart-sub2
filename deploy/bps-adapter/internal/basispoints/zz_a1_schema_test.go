package basispoints

// A1: the schema lowering is a port of Codex's parse_tool_input_schema
// (openai/codex @ea27864, codex-rs/tools/src/json_schema.rs and
// json_schema/compaction.rs). These cases are Codex's own unit tests
// (json_schema_tests.rs), input and expected output as Codex asserts them,
// plus the refusals serde would make.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func a1MustJSON(t *testing.T, text string) any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("bad test JSON %q: %v", text, err)
	}
	return value
}

func a1Canonical(t *testing.T, value any) string {
	t.Helper()
	return string(a1CompactJSON(a1MustJSON(t, string(a1CompactJSON(value)))))
}

func TestA1SchemaLoweringMatchesCodex(t *testing.T) {
	x5500 := strings.Repeat("x", 5500)
	many := func(count int) string {
		parts := make([]string, 0, count)
		for index := 0; index < count; index++ {
			parts = append(parts, fmt.Sprintf(`"field_%03d":{"type":"string"}`, index))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	cases := []struct{ name, in, want string }{
		{"coerces_boolean_schemas", `true`, `{"type":"string"}`},
		{"infers_object_shape_and_defaults_properties", `{"properties":{"query":{"description":"search query"}}}`,
			`{"type":"object","properties":{"query":{}}}`},
		{"coerces_unrecognized_object_schema_to_empty_schema", `{"description":"Ticket identifier","title":"Ticket ID"}`, `{}`},
		{"preserves_integer_and_defaults_array_items", `{"type":"object","properties":{"page":{"type":"integer"},"tags":{"type":"array"}}}`,
			`{"type":"object","properties":{"page":{"type":"integer"},"tags":{"type":"array","items":{"type":"string"}}}}`},
		{"sanitizes_additional_properties_schema",
			`{"type":"object","additionalProperties":{"required":["value"],"properties":{"value":{"anyOf":[{"type":"string"},{"type":"number"}]}}}}`,
			`{"type":"object","properties":{},"additionalProperties":{"type":"object","properties":{"value":{"anyOf":[{"type":"string"},{"type":"number"}]}},"required":["value"]}}`},
		{"infers_object_shape_from_boolean_additional_properties_only", `{"additionalProperties":false}`,
			`{"type":"object","properties":{},"additionalProperties":false}`},
		{"infers_number_from_numeric_keywords", `{"minimum":1}`, `{"type":"number"}`},
		{"infers_number_from_multiple_of", `{"multipleOf":5}`, `{"type":"number"}`},
		{"infers_string_from_enum", `{"enum":["fast","safe"]}`, `{"type":"string","enum":["fast","safe"]}`},
		{"infers_string_from_const", `{"const":"file"}`, `{"type":"string","enum":["file"]}`},
		{"infers_string_from_format", `{"format":"date-time"}`, `{"type":"string"}`},
		{"preserves_empty_schema", `{}`, `{}`},
		{"preserves_nested_empty_schema", `{"type":"object","properties":{"metadata":{"properties":{"extra":{}}}}}`,
			`{"type":"object","properties":{"metadata":{"type":"object","properties":{"extra":{}}}}}`},
		{"infers_array_from_prefix_items", `{"prefixItems":[{"type":"string"}]}`, `{"type":"array","items":{"type":"string"}}`},
		{"preserves_boolean_additional_properties_on_inferred_object",
			`{"type":"object","properties":{"metadata":{"additionalProperties":true}}}`,
			`{"type":"object","properties":{"metadata":{"type":"object","properties":{},"additionalProperties":true}}}`},
		{"infers_object_shape_from_schema_additional_properties_only", `{"additionalProperties":{"type":"string"}}`,
			`{"type":"object","properties":{},"additionalProperties":{"type":"string"}}`},
		{"rewrites_const_to_single_value_enum", `{"const":"tagged"}`, `{"type":"string","enum":["tagged"]}`},
		{"fills_default_properties_for_nullable_object_union", `{"type":["object","null"]}`, `{"type":["object","null"],"properties":{}}`},
		{"fills_default_items_for_nullable_array_union", `{"type":["array","null"]}`, `{"type":["array","null"],"items":{"type":"string"}}`},
		{"preserves_nested_nullable_any_of_shape",
			`{"type":"object","properties":{"open":{"anyOf":[{"type":"array","items":{"type":"object","properties":{"ref_id":{"type":"string"},"lineno":{"anyOf":[{"type":"integer"},{"type":"null"}]}},"required":["ref_id"],"additionalProperties":false}},{"type":"null"}]}}}`,
			`{"type":"object","properties":{"open":{"anyOf":[{"type":"array","items":{"type":"object","properties":{"lineno":{"anyOf":[{"type":"integer"},{"type":"null"}]},"ref_id":{"type":"string"}},"required":["ref_id"],"additionalProperties":false}},{"type":"null"}]}}}`},
		{"preserves_nested_nullable_type_union",
			`{"type":"object","properties":{"nickname":{"type":["string","null"],"description":"Optional nickname"}},"required":["nickname"],"additionalProperties":false}`,
			`{"type":"object","properties":{"nickname":{"type":["string","null"],"description":"Optional nickname"}},"required":["nickname"],"additionalProperties":false}`},
		{"preserves_nested_any_of_property", `{"type":"object","properties":{"query":{"anyOf":[{"type":"string"},{"type":"number"}]}}}`,
			`{"type":"object","properties":{"query":{"anyOf":[{"type":"string"},{"type":"number"}]}}}`},
		{"preserves_nested_one_of_property", `{"type":"object","properties":{"query":{"oneOf":[{"const":"exact"},{"type":"number"}]}}}`,
			`{"type":"object","properties":{"query":{"oneOf":[{"type":"string","enum":["exact"]},{"type":"number"}]}}}`},
		{"preserves_nested_all_of_property", `{"type":"object","properties":{"query":{"allOf":[{"type":"string"},{"description":"unrecognized by itself"}]}}}`,
			`{"type":"object","properties":{"query":{"allOf":[{"type":"string"},{}]}}}`},
		{"preserves_type_unions_without_rewriting_to_any_of", `{"type":["string","null"],"description":"optional string"}`,
			`{"type":["string","null"],"description":"optional string"}`},
		{"preserves_explicit_enum_type_union", `{"type":["string","null"],"enum":["short","medium","long"],"description":"optional response length"}`,
			`{"type":["string","null"],"enum":["short","medium","long"],"description":"optional response length"}`},
		{"preserves_string_enum_constraints",
			`{"type":"object","properties":{"response_length":{"type":"enum","enum":["short","medium","long"]},"kind":{"type":"const","const":"tagged"},"scope":{"type":"enum","enum":["one","two"]}}}`,
			`{"type":"object","properties":{"kind":{"type":"string","enum":["tagged"]},"response_length":{"type":"string","enum":["short","medium","long"]},"scope":{"type":"string","enum":["one","two"]}}}`},
		{"preserves_refs_and_prunes_unreachable_defs",
			`{"type":"object","properties":{"user":{"$ref":"#/$defs/User"}},"$defs":{"User":{"type":"object","properties":{"name":{"type":"string"}}},"Unused":{"type":"string"}}}`,
			`{"type":"object","properties":{"user":{"$ref":"#/$defs/User"}},"$defs":{"User":{"type":"object","properties":{"name":{"type":"string"}}}}}`},
		{"preserves_refs_from_properties_named_def_tables",
			`{"type":"object","properties":{"$defs":{"$ref":"#/$defs/User"}},"$defs":{"User":{"type":"string"},"Unused":{"type":"boolean"}}}`,
			`{"type":"object","properties":{"$defs":{"$ref":"#/$defs/User"}},"$defs":{"User":{"type":"string"}}}`},
		{"collects_refs_from_schema_child_keywords",
			`{"type":"object","properties":{"items_holder":{"type":"array","items":{"$ref":"#/$defs/Item"}},"map_holder":{"type":"object","additionalProperties":{"$ref":"#/$defs/Extra"}},"choice":{"anyOf":[{"$ref":"#/$defs/Choice"},{"type":"string"}]},"exclusive_choice":{"oneOf":[{"$ref":"#/$defs/ExclusiveChoice"},{"type":"integer"}]},"combined":{"allOf":[{"$ref":"#/$defs/Combined"},{"type":"object"}]}},"$defs":{"Combined":{"type":"object"},"Choice":{"type":"boolean"},"ExclusiveChoice":{"type":"null"},"Extra":{"type":"number"},"Item":{"type":"string"},"Unused":{"type":"null"}}}`,
			`{"type":"object","properties":{"choice":{"anyOf":[{"$ref":"#/$defs/Choice"},{"type":"string"}]},"combined":{"allOf":[{"$ref":"#/$defs/Combined"},{"type":"object","properties":{}}]},"exclusive_choice":{"oneOf":[{"$ref":"#/$defs/ExclusiveChoice"},{"type":"integer"}]},"items_holder":{"type":"array","items":{"$ref":"#/$defs/Item"}},"map_holder":{"type":"object","properties":{},"additionalProperties":{"$ref":"#/$defs/Extra"}}},"$defs":{"Combined":{"type":"object","properties":{}},"Choice":{"type":"boolean"},"ExclusiveChoice":{"type":"null"},"Extra":{"type":"number"},"Item":{"type":"string"}}}`},
		{"handles_cyclic_local_refs",
			`{"type":"object","properties":{"node":{"$ref":"#/$defs/Node"}},"$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}}}`,
			`{"type":"object","properties":{"node":{"$ref":"#/$defs/Node"}},"$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}}}`},
		{"preserves_legacy_definitions",
			`{"type":"object","properties":{"user":{"$ref":"#/definitions/User"}},"definitions":{"User":{"type":"object","properties":{"profile":{"$ref":"#/definitions/Profile"}}},"Profile":{"type":"object","properties":{"name":{"type":"string"}}},"Unused":{"type":"string"}}}`,
			`{"type":"object","properties":{"user":{"$ref":"#/definitions/User"}},"definitions":{"Profile":{"type":"object","properties":{"name":{"type":"string"}}},"User":{"type":"object","properties":{"profile":{"$ref":"#/definitions/Profile"}}}}}`},
		{"preserves_unresolved_and_external_refs",
			`{"type":"object","properties":{"missing":{"$ref":"#/$defs/Missing"},"remote":{"$ref":"https://example.com/schema.json"}},"$defs":{"Unused":{"type":"string"}}}`,
			`{"type":"object","properties":{"missing":{"$ref":"#/$defs/Missing"},"remote":{"$ref":"https://example.com/schema.json"}}}`},
		{"preserves_nested_defs_ref_parent",
			`{"type":"object","properties":{"name":{"$ref":"#/$defs/User/properties/name"}},"$defs":{"User":{"type":"object","properties":{"name":{"type":"string"}}},"name":{"type":"string"},"Unused":{"type":"boolean"}}}`,
			`{"type":"object","properties":{"name":{"$ref":"#/$defs/User/properties/name"}},"$defs":{"User":{"type":"object","properties":{"name":{"type":"string"}}}}}`},
		{"preserves_percent_encoded_definition_refs",
			`{"type":"object","properties":{"user":{"$ref":"#/$defs/User%20Name"},"profile":{"$ref":"#/%24defs/Profile%7E0Name"}},"$defs":{"User Name":{"type":"string"},"Profile~Name":{"type":"string"},"Unused":{"type":"boolean"}}}`,
			`{"type":"object","properties":{"profile":{"$ref":"#/%24defs/Profile%7E0Name"},"user":{"$ref":"#/$defs/User%20Name"}},"$defs":{"Profile~Name":{"type":"string"},"User Name":{"type":"string"}}}`},
		{"drops_malformed_definition_tables",
			`{"type":"object","properties":{"user":{"$ref":"#/$defs/User"}},"$defs":["not","an","object"]}`,
			`{"type":"object","properties":{"user":{"$ref":"#/$defs/User"}}}`},
		// json_schema/compaction.rs, through the size policy (5,000 bytes).
		{"compacts_descriptions_only_on_default_path",
			`{"type":"object","description":"` + x5500 + `","properties":{"metadata":{"$ref":"#/$defs/metadata"}},"$defs":{"metadata":{"type":"string","description":"Metadata value"}}}`,
			`{"type":"object","properties":{"metadata":{"$ref":"#/$defs/metadata"}},"$defs":{"metadata":{"type":"string"}}}`},
		{"ignores_dropped_metadata_for_budget",
			`{"type":"object","properties":{"event":{"type":"object","title":"Calendar event","properties":{"recurrence":{"type":"object","examples":[{"payload":"` + x5500 + `"}],"properties":{"pattern":{"type":"string","title":"Recurrence pattern"}}}}}}}`,
			`{"type":"object","properties":{"event":{"type":"object","properties":{"recurrence":{"type":"object","properties":{"pattern":{"type":"string"}}}}}}}`},
		{"stops_after_dropping_root_definitions_when_under_budget",
			`{"type":"object","description":"` + x5500 + `","properties":{"event":{"type":"object","description":"Calendar event","properties":{"recurrence":{"type":"object","description":"Recurrence settings","properties":{"pattern":{"type":"string","description":"Recurrence pattern"}}}}},"metadata":{"$ref":"#/$defs/metadata"}},"$defs":{"metadata":{"type":"object","description":"metadata object","properties":` + many(300) + `}}}`,
			`{"type":"object","properties":{"event":{"type":"object","properties":{"recurrence":{"type":"object","properties":{"pattern":{"type":"string"}}}}},"metadata":{}}}`},
		{"strips_descriptions_without_removing_description_property",
			`{"type":"object","description":"` + x5500 + `","properties":{"description":{"type":"string","description":"User-facing description value"},"metadata":{"type":"object","description":"Metadata object","properties":{"label":{"type":"string","description":"Metadata label"}}},"tags":{"type":"array","description":"Tag list","items":{"type":"string","description":"Tag value"}},"extras":{"type":"object","additionalProperties":{"type":"string","description":"Extra value"}},"choice":{"description":"Choice value","anyOf":[{"type":"string","description":"String choice"},{"type":"number","description":"Number choice"}]}}}`,
			`{"type":"object","properties":{"choice":{"anyOf":[{"type":"string"},{"type":"number"}]},"description":{"type":"string"},"extras":{"type":"object","properties":{},"additionalProperties":{"type":"string"}},"metadata":{"type":"object","properties":{"label":{"type":"string"}}},"tags":{"type":"array","items":{"type":"string"}}}}`},
		{"prunes_single_composition_variant_if_still_over_budget",
			`{"type":"object","properties":{"choice":{"anyOf":[{"type":"string","enum":["` + x5500 + `"]}]}}}`,
			`{"type":"object","properties":{"choice":{}}}`},
		{"preserves_object_enum_literal_descriptions",
			`{"type":"object","description":"` + x5500 + `","properties":{"choice":{"enum":[{"description":"first literal","id":1},{"description":"second literal","id":2}]}}}`,
			`{"type":"object","properties":{"choice":{"type":"string","enum":[{"description":"first literal","id":1},{"description":"second literal","id":2}]}}}`},
	}
	for _, key := range a1CompositionKeys {
		first, second, third := strings.Repeat("first ", 400), strings.Repeat("second ", 400), strings.Repeat("third ", 400)
		cases = append(cases, struct{ name, in, want string }{"prunes_compositions_as_last_resort_" + key,
			`{"type":"object","properties":{"choice":{"` + key + `":[{"type":"string","enum":["` + first + `"]},{"type":"string","enum":["` + second + `"]},{"type":"string","enum":["` + third + `"]}]}}}`,
			`{"type":"object","properties":{"choice":{}}}`})
	}
	for _, c := range cases {
		got, _, err := a1LowerSchema(a1MustJSON(t, c.in))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if a1Canonical(t, got) != a1Canonical(t, a1MustJSON(t, c.want)) {
			t.Errorf("%s:\n got %s\nwant %s", c.name, a1CompactJSON(got), c.want)
		}
		// Lowering is idempotent: a lowered schema lowers to itself.
		again, _, err := a1LowerSchema(a1MustJSON(t, string(a1CompactJSON(got))))
		if err != nil || a1Canonical(t, again) != a1Canonical(t, got) {
			t.Errorf("%s: not idempotent: %s (%v)", c.name, a1CompactJSON(again), err)
		}
	}
}

// serde refuses these, so Codex would not register the tool; A1 keeps the
// request on the relay instead (schema_invalid).
func TestA1SchemaLoweringRefusesWhatCodexRefuses(t *testing.T) {
	for _, in := range []string{
		`{"type":"null"}`, // rejects_singleton_null_type
		`{"type":"object","required":"a"}`,
		`{"type":"object","required":["a",1]}`,
		`{"type":"string","enum":"a"}`,
		`{"type":"array","items":[{"type":"string"}]}`,
		`{"type":"array","minItems":1.5}`,
		`{"type":"array","minItems":-1}`,
		`{"type":"string","description":5}`,
		`{"type":"object","properties":"x"}`,
		`{"$ref":7}`,
		`{"$ref":"#/$defs/A","type":"any"}`,
		`{"type":"object","properties":{"a":"text"}}`,
		`{"type":"object","additionalProperties":"x"}`,
		`{"anyOf":{"type":"string"}}`,
		`{"type":"string","encrypted":"yes"}`,
	} {
		if got, _, err := a1LowerSchema(a1MustJSON(t, in)); err == nil {
			t.Errorf("%s lowered to %s, Codex refuses it", in, a1CompactJSON(got))
		}
	}
	// Nulls are absent, as serde reads Option<T>.
	got, _, err := a1LowerSchema(a1MustJSON(t, `{"type":"object","properties":{"a":{"type":"string","description":null,"enum":null}},"required":null}`))
	if err != nil || string(a1CompactJSON(got)) != `{"properties":{"a":{"type":"string"}},"type":"object"}` {
		t.Fatalf("nulls: %s %v", a1CompactJSON(got), err)
	}
}

// A tool's parameters also get mcp_tool.rs's rule: a missing or null
// top-level properties becomes {}; no parameters at all is an empty object.
func TestA1ToolParametersFollowTheMCPRule(t *testing.T) {
	for in, want := range map[string]string{
		`null`:                                `{"properties":{},"type":"object"}`,
		`{}`:                                  `{"properties":{},"type":"object"}`,
		`{"type":"object"}`:                   `{"properties":{},"type":"object"}`,
		`{"type":"object","properties":null}`: `{"properties":{},"type":"object"}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"a":{"type":"string","minLength":1,"default":"x","title":"A"}},"required":["a"],"additionalProperties":false}`: `{"additionalProperties":false,"properties":{"a":{"type":"string"}},"required":["a"],"type":"object"}`,
	} {
		got, _, err := a1ToolParameters(a1MustJSON(t, in))
		if err != nil || string(a1CompactJSON(got)) != want {
			t.Errorf("%s -> %s (%v), want %s", in, a1CompactJSON(got), err, want)
		}
	}
	if _, _, err := a1ToolParameters("not a schema"); err == nil {
		t.Fatal("a string as parameters was accepted")
	}
	// The input is never modified.
	raw := a1MustJSON(t, `{"type":"object","properties":{"a":{"const":"x","title":"t"}}}`)
	before := string(a1CompactJSON(raw))
	if _, _, err := a1ToolParameters(raw); err != nil || string(a1CompactJSON(raw)) != before {
		t.Fatalf("parameters changed in place: %s", a1CompactJSON(raw))
	}
}

// The features the not-promotable list reads are the keywords that survive
// lowering, looked for on the lowered schema.
func TestA1SchemaFeatures(t *testing.T) {
	for in, want := range map[string]a1SchemaInfo{
		`{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}},"$defs":{"A":{"type":"string"}}}`: {ref: true},
		`{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}`:     {composition: true},
		`{"type":"object","properties":{"a":{"const":"x"}}}`:                                        {enum: true},
		`{"type":"object","properties":{"a":{"type":["string","null"]}}}`:                           {typeList: true},
		`{"type":"object","additionalProperties":true}`:                                             {additionalTrue: true},
		`{"type":"object","additionalProperties":{"type":"string"}}`:                                {additionalSchema: true},
		`{"type":"object","properties":{"m":{"type":"string","encrypted":true}}}`:                   {encrypted: true},
		`{"type":"object","properties":{"a":{"type":"string","pattern":"^x","default":"x"}}}`:       {},
		`{"type":"object","description":"` + strings.Repeat("d", 6000) + `"}`:                       {compacted: true},
	} {
		_, info, err := a1ToolParameters(a1MustJSON(t, in))
		if err != nil || info != want {
			t.Errorf("%.80s: %+v (%v), want %+v", in, info, err, want)
		}
	}
}

// json_schema/compaction_tests.rs: the depth pass counts every schema child
// (properties, items, additionalProperties, a composition's variants).
func TestA1CollapseDeepSchemaObjectsMatchesCodex(t *testing.T) {
	leaf := `{"type":"object","properties":{"leaf":{"type":"string"}}}`
	in := `{"type":"object","properties":{` +
		`"object_parent":{"type":"object","properties":{"complex":{"type":"object","properties":{"nested":` + leaf + `}},"scalar":{"type":"string"}}},` +
		`"array_parent":{"type":"array","items":{"type":"object","properties":{"nested":` + leaf + `}}},` +
		`"map_parent":{"type":"object","additionalProperties":{"type":"object","properties":{"nested":` + leaf + `}}},` +
		`"union_parent":{"anyOf":[{"type":"object","properties":{"nested":` + leaf + `}},{"type":"string"}]}}}`
	want := `{"type":"object","properties":{` +
		`"object_parent":{"type":"object","properties":{"complex":{"type":"object","properties":{"nested":{}}},"scalar":{"type":"string"}}},` +
		`"array_parent":{"type":"array","items":{"type":"object","properties":{"nested":{}}}},` +
		`"map_parent":{"type":"object","additionalProperties":{"type":"object","properties":{"nested":{}}}},` +
		`"union_parent":{"anyOf":[{"type":"object","properties":{"nested":{}}},{"type":"string"}]}}}`
	got := a1CollapseDeepSchemaObjects(a1MustJSON(t, in), 0)
	if a1Canonical(t, got) != a1Canonical(t, a1MustJSON(t, want)) {
		t.Fatalf("got %s", a1CompactJSON(got))
	}
}
