package basispoints

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func s1ReferencesSource(source map[string]any) map[string]any {
	return withRelayProtocolSelection(withRelayOptions(source, relayOptions{Protocol: relayProtocolReferences, CoerceScalarStrings: true}), relayProtocolReferences)
}

func s1Call(refs any, refsPresent bool, code any) map[string]any {
	outer := map[string]any{"summary": "s", "code": code}
	if refsPresent {
		outer["references"] = refs
	}
	return s1Transport("s1_ref_rules", outer)
}

// S1.1: references mode reads code first; exactly one declared key selects;
// a routing envelope is never a payload; failure reasons describe code first.
func TestH25S1ReferencesDecodeRules(t *testing.T) {
	source := s1ReferencesSource(s1DecodeSources()["multi"])
	weather := s1Envelope("get_weather", map[string]any{"city": "Paris"})
	brokenEnvelope := `{"tool":"apply_patch","args":"line "quoted" end"}`
	for _, tc := range []struct {
		name        string
		refs        any
		refsPresent bool
		code        any
		wantName    string
		wantPayload string
		wantReason  string
	}{
		{"envelope_any_refs_paths", []any{"src/a.go", "b.md"}, true, weather, "get_weather", `{"city":"Paris"}`, ""},
		{"envelope_refs_other_tool", []any{"workspace.exec_command"}, true, weather, "get_weather", `{"city":"Paris"}`, ""},
		{"envelope_refs_null", nil, true, weather, "get_weather", `{"city":"Paris"}`, ""},
		{"envelope_refs_string", "x", true, weather, "get_weather", `{"city":"Paris"}`, ""},
		{"envelope_undeclared_never_payload", []any{"get_weather"}, true, s1Envelope("not_declared", map[string]any{"city": "x"}), "", "", "tool_not_in_catalog"},
		{"envelope_into_custom_ref", []any{"apply_patch"}, true, s1Envelope("get_weather", map[string]any{"city": "P"}), "get_weather", `{"city":"P"}`, ""},
		{"payloadless_envelope_never_custom_input", []any{"apply_patch"}, true, `{"tool":"get_weather"}`, "", "", "invalid_arguments_json"},
		{"business_tool_field_undeclared_is_payload", []any{"get_weather"}, true, `{"tool":"hammer","city":"S"}`, "get_weather", `{"city":"S","tool":"hammer"}`, ""},
		{"broken_envelope_custom_ref", []any{"apply_patch"}, true, brokenEnvelope, "", "", "code_json"},
		{"broken_envelope_paths", []any{"a.txt", "b.txt"}, true, brokenEnvelope, "", "", "code_json"},
		{"broken_envelope_two_tools", []any{"get_weather", "apply_patch"}, true, brokenEnvelope, "", "", "code_json"},
		{"broken_envelope_no_refs", nil, false, brokenEnvelope, "", "", "code_json"},
		{"broken_fenced_envelope", []any{"apply_patch"}, true, "```json\n" + brokenEnvelope + "\n```", "", "", "code_json"},
		{"nested_wrapper_never_payload", []any{"apply_patch"}, true, `{"name":"run_officejs","arguments":"{broken"}`, "", "", "outer_json"},
		{"payload_one_ref", []any{"get_weather"}, true, `{"city":"S"}`, "get_weather", `{"city":"S"}`, ""},
		{"payload_ref_among_paths", []any{"src/a.go", "get_weather", "README.md"}, true, `{"city":"S"}`, "get_weather", `{"city":"S"}`, ""},
		{"payload_ref_repeated", []any{"get_weather", "get_weather"}, true, `{"city":"S"}`, "get_weather", `{"city":"S"}`, ""},
		{"payload_two_declared", []any{"get_weather", "workspace.exec_command"}, true, `{"city":"S"}`, "", "", "ambiguous_references"},
		{"payload_one_undeclared", []any{"src/a.go"}, true, `{"city":"S"}`, "", "", "tool_not_in_catalog"},
		{"payload_two_undeclared", []any{"a", "b"}, true, `{"city":"S"}`, "", "", "invalid_references"},
		{"payload_refs_null", nil, true, `{"city":"S"}`, "", "", "invalid_references"},
		{"payload_refs_absent", nil, false, `{"city":"S"}`, "", "", "missing_tool_name"},
		{"payload_refs_empty", []any{}, true, `{"city":"S"}`, "", "", "missing_tool_name"},
		{"broken_payload_no_ref", []any{"a.txt"}, true, `{"city":"S`, "", "", "code_json"},
		{"broken_payload_declared_ref", []any{"get_weather"}, true, `{"city":"S`, "", "", "invalid_arguments_json"},
		{"raw_custom", []any{"apply_patch"}, true, "*** Begin Patch\n+\"q\" \\x\n*** End Patch\n", "apply_patch", "*** Begin Patch\n+\"q\" \\x\n*** End Patch\n", ""},
		{"raw_custom_empty", []any{"workspace.write"}, true, "", "write", "", ""},
		{"raw_custom_no_ref", nil, false, "plain words", "", "", "missing_tool_name"},
		{"code_missing", []any{"get_weather"}, true, nil, "", "", "code_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := s1Call(tc.refs, tc.refsPresent, tc.code)
			if tc.code == nil {
				outer := parseArguments(native["arguments"])
				delete(outer, "code")
				native["arguments"] = string(jsonBytes(outer))
			}
			for round := 0; round < 2; round++ {
				call, reason := decodeTransportItemWithReason(cloneObject(native), source)
				if reason != tc.wantReason {
					t.Fatalf("reason=%q, want %q (call %v)", reason, tc.wantReason, call)
				}
				if tc.wantReason != "" {
					continue
				}
				// Exact bytes: stringValue would trim them.
				payload, _ := call["arguments"].(string)
				if call["type"] == "custom_tool_call" {
					payload, _ = call["input"].(string)
				}
				if call["name"] != tc.wantName || payload != tc.wantPayload {
					t.Fatalf("call=%#v payload=%q want=%q", call, payload, tc.wantPayload)
				}
			}
		})
	}
}

// S1.1 + S1.2 end to end: under the default configuration a legacy call whose
// references hold two file paths is delivered (rc2 failed it with
// ambiguous_references), and a references-style call fails exactly as in h22.
func TestH25S1DefaultStreamDecodesLikeH22(t *testing.T) {
	legacy := jsonBytes(map[string]any{"summary": "s", "references": []any{"src/app.go", "README.md"}, "code": s1Envelope("get_weather", map[string]any{"city": "Paris"})})
	result := runStreamTest(t, weatherRequest(), transportChunks(string(legacy)))
	if result.closeErr != "" || !strings.Contains(result.joined(), `"name":"get_weather"`) || strings.Contains(result.joined(), transportName) {
		t.Fatalf("legacy call with path references not delivered: err=%q", result.closeErr)
	}
	modern := jsonBytes(map[string]any{"summary": "s", "references": []any{"get_weather"}, "code": `{"city":"Paris"}`})
	result = runStreamTest(t, weatherRequest(), transportChunks(string(modern)))
	if !strings.Contains(result.closeErr, "reason=missing_tool_name") || strings.Contains(result.joined(), `"name":"get_weather"`) {
		t.Fatalf("references-style call was decoded under the default legacy protocol: err=%q", result.closeErr)
	}
}

// Why relay acceptance is h22 OR rc2: rc2's checker alone rejects these, and
// h22 delivered every one of them.
func TestH25S1RelaySchemaAcceptsWhatH22Accepted(t *testing.T) {
	for _, tc := range []struct{ value, schema string }{
		{`{"n":1.5}`, `{"type":"object","properties":{"n":{"type":"integer"}}}`},
		{`{}`, `{"required":["city"]}`},
		{`{"other":7}`, `{"type":"object","additionalProperties":{"type":"string"}}`},
		{`{"n":"1"}`, `{"properties":{"n":{"enum":[1]}}}`},
		{`{"blocked":true}`, `{"type":"object","properties":{"blocked":false}}`},
	} {
		value, schema := decodeToolJSONObject(tc.value), decodeToolJSONObject(tc.schema)
		if schemaMatches(value, schema) || !h22SchemaMatches(value, schema) || !relaySchemaAccepts(value, schema) {
			t.Fatalf("%s against %s: rc2=%v h22=%v relay=%v", tc.value, tc.schema, schemaMatches(value, schema), h22SchemaMatches(value, schema), relaySchemaAccepts(value, schema))
		}
	}
	// And rc2's corrections of h22's false rejections are kept (E3).
	value := decodeToolJSONObject(`{"S_name":"ok"}`)
	schema := decodeToolJSONObject(`{"type":"object","properties":{},"patternProperties":{"^S_":{"type":"string"}},"additionalProperties":false}`)
	if h22SchemaMatches(value, schema) || !relaySchemaAccepts(value, schema) {
		t.Fatal("patternProperties correction lost")
	}
}

func TestH25S1CoerceScalarStringsRules(t *testing.T) {
	for _, tc := range []struct {
		name, args, schema, want string
	}{
		{"integer", `{"n":"20"}`, `{"properties":{"n":{"type":"integer"}}}`, `{"n":20}`},
		{"negative_integer", `{"n":"-3"}`, `{"properties":{"n":{"type":"integer"}}}`, `{"n":-3}`},
		{"big_integer_exact", `{"n":"9007199254740993"}`, `{"properties":{"n":{"type":"integer"}}}`, `{"n":9007199254740993}`},
		{"integer_fraction_stays", `{"n":"1.0"}`, `{"properties":{"n":{"type":"integer"}}}`, ""},
		{"integer_exponent_stays", `{"n":"1e3"}`, `{"properties":{"n":{"type":"integer"}}}`, ""},
		{"number", `{"n":"0.5"}`, `{"properties":{"n":{"type":"number"}}}`, `{"n":0.5}`},
		{"number_exponent", `{"n":"1e3"}`, `{"properties":{"n":{"type":"number"}}}`, `{"n":1e3}`},
		{"leading_space_stays", `{"n":" 5"}`, `{"properties":{"n":{"type":"number"}}}`, ""},
		{"leading_zero_stays", `{"n":"05"}`, `{"properties":{"n":{"type":"integer"}}}`, ""},
		{"plus_stays", `{"n":"+5"}`, `{"properties":{"n":{"type":"integer"}}}`, ""},
		{"hex_stays", `{"n":"0x10"}`, `{"properties":{"n":{"type":"integer"}}}`, ""},
		{"boolean_true", `{"b":"true"}`, `{"properties":{"b":{"type":"boolean"}}}`, `{"b":true}`},
		{"boolean_false", `{"b":"false"}`, `{"properties":{"b":{"type":"boolean"}}}`, `{"b":false}`},
		{"boolean_case_stays", `{"b":"True"}`, `{"properties":{"b":{"type":"boolean"}}}`, ""},
		{"boolean_digit_stays", `{"b":"1"}`, `{"properties":{"b":{"type":"boolean"}}}`, ""},
		{"nullable_integer", `{"n":"7"}`, `{"properties":{"n":{"type":["integer","null"]}}}`, `{"n":7}`},
		{"string_allowed_stays", `{"n":"7"}`, `{"properties":{"n":{"type":["string","integer"]}}}`, ""},
		{"nested_object", `{"o":{"n":"7","s":"7"}}`, `{"properties":{"o":{"type":"object","properties":{"n":{"type":"integer"},"s":{"type":"string"}}}}}`, `{"o":{"n":7,"s":"7"}}`},
		{"array_items", `{"a":["1",2,"x"]}`, `{"properties":{"a":{"type":"array","items":{"type":"integer"}}}}`, `{"a":[1,2,"x"]}`},
		{"additional_properties", `{"k":"3"}`, `{"type":"object","additionalProperties":{"type":"integer"}}`, `{"k":3}`},
		{"pattern_properties_untouched", `{"k":"3"}`, `{"type":"object","patternProperties":{"^k":{"type":"integer"}},"additionalProperties":{"type":"integer"}}`, ""},
		{"ref_untouched", `{"n":"3"}`, `{"properties":{"n":{"$ref":"#/$defs/n"}}}`, ""},
		{"any_of_untouched", `{"n":"3"}`, `{"properties":{"n":{"anyOf":[{"type":"integer"}]}}}`, ""},
		{"prefix_items_untouched", `{"a":["1"]}`, `{"properties":{"a":{"prefixItems":[{"type":"integer"}],"items":{"type":"integer"}}}}`, ""},
		{"undeclared_untouched", `{"x":"3"}`, `{"properties":{"n":{"type":"integer"}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, schema := decodeToolJSONObject(tc.args), decodeToolJSONObject(tc.schema)
			before := string(jsonBytes(args))
			got, changed := coerceScalarStrings(args, schema)
			if string(jsonBytes(args)) != before {
				t.Fatal("input mutated")
			}
			if tc.want == "" {
				if changed {
					t.Fatalf("converted: %s", jsonBytes(got))
				}
				return
			}
			if !changed || string(jsonBytes(got)) != tc.want {
				t.Fatalf("got %s changed=%v, want %s", jsonBytes(got), changed, tc.want)
			}
		})
	}
}

func s1ListFilesCall(args map[string]any) map[string]any {
	return s1Transport("s1_coerce", map[string]any{"code": s1Envelope("list_files", args)})
}

// S1.4 through the decoder: only failing calls are repaired, only when the
// repair makes them pass, never for custom tools, never with the switch off,
// identically on every decode, and noted once per call.
func TestH25S1CoercionInTheDecoder(t *testing.T) {
	multi := s1DecodeSources()["multi"]
	on := withRelayOptions(multi, defaultRelayOptions())
	offOptions := defaultRelayOptions()
	offOptions.CoerceScalarStrings = false
	off := withRelayOptions(multi, offOptions)

	native := s1ListFilesCall(map[string]any{"path": "/", "limit": "20"})
	logged := 0
	on[relayDecodeNotesKey] = &relayDecodeNotes{seen: map[string]bool{}, log: func(reason string) {
		if reason != "scalar_coerced" {
			t.Fatalf("note %q", reason)
		}
		logged++
	}}
	before := relayCounterSnapshot()["scalar_coerced"]
	var first string
	for round := 0; round < 3; round++ {
		call, reason := decodeTransportItemWithReason(cloneObject(native), on)
		if reason != "" || call["arguments"] != `{"limit":20,"path":"/"}` {
			t.Fatalf("not coerced: %q %v", reason, call)
		}
		if round == 0 {
			first = string(jsonBytes(call))
		} else if string(jsonBytes(call)) != first {
			t.Fatal("second decode of the same call differs")
		}
	}
	if logged != 1 || relayCounterSnapshot()["scalar_coerced"] != before+1 {
		t.Fatalf("note logged %d times, counter +%d", logged, relayCounterSnapshot()["scalar_coerced"]-before)
	}
	if _, reason := decodeTransportItemWithReason(cloneObject(native), off); reason != "schema_mismatch" {
		t.Fatalf("switch off: %q", reason)
	}
	// Another error stays an error, and nothing is half-converted.
	for _, args := range []map[string]any{{"path": 7, "limit": "20"}, {"path": "/", "limit": "abc"}, {"limit": "20"}} {
		if _, reason := decodeTransportItemWithReason(s1ListFilesCall(args), on); reason != "schema_mismatch" {
			t.Fatalf("%v: %q", args, reason)
		}
	}
	// A call that already passes is never touched, even with strings in it.
	call, reason := decodeTransportItemWithReason(s1Transport("s1_loose", map[string]any{"code": s1Envelope("loose", map[string]any{"n": "2"})}), on)
	if reason != "" || call["arguments"] != `{"n":"2"}` {
		t.Fatalf("passing call changed: %q %v", reason, call)
	}
	// Custom tools receive raw text, unchanged.
	call, reason = decodeTransportItemWithReason(s1Transport("s1_custom", map[string]any{"code": s1Envelope("apply_patch", "20")}), on)
	if reason != "" || call["input"] != "20" {
		t.Fatalf("custom input changed: %q %v", reason, call)
	}
}

// S1.3: full mode is rc2's complete contract (without HTML escaping) plus its
// hints; summary mode has none of rc2's text, in legacy and references alike.
func TestH25S1ToolContractsSwitch(t *testing.T) {
	source := cloneObject(s1PromptFixtures()[2].Source) // multi
	source["tool_choice"] = "required"
	source["parallel_tool_calls"] = false
	source["input"] = []any{map[string]any{"type": "additional_tools", "tools": []any{s1Function("native_lookup", "", map[string]any{"type": "object"})}}, messageItem("user", "go")}
	rc2Only := []string{"Tool choice for this request is required", "At most one client function/custom tool call", "The client also declares native tools", "Full tool key:"}
	for _, protocol := range []string{relayProtocolLegacy, relayProtocolReferences} {
		summary := defaultConfig()
		summary.RelayProtocol = protocol
		full := summary
		full.RelayToolContracts = relayToolContractsFull
		summaryBody, err := prepareResponsesBody(cloneObject(source), summary)
		if err != nil {
			t.Fatal(err)
		}
		fullBody, err := prepareResponsesBody(cloneObject(source), full)
		if err != nil {
			t.Fatal(err)
		}
		summaryText, fullText := preparedDeveloperText(summaryBody), preparedDeveloperText(fullBody)
		for _, text := range rc2Only {
			if strings.Contains(summaryText, text) {
				t.Fatalf("%s summary carries rc2 text %q", protocol, text)
			}
			if !strings.Contains(fullText, text) {
				t.Fatalf("%s full lacks %q", protocol, text)
			}
		}
		if !strings.Contains(summaryText, "- list_files (function): List files <b>&amp;</b> folders 天气 \"quoted\". Its arguments are an object with") {
			t.Fatalf("%s summary is not h22's directory", protocol)
		}
		if !strings.Contains(fullText, `"description":"List files <b>&amp;</b> folders 天气 \"quoted\""`) || strings.Contains(fullText, `\u003c`) {
			t.Fatalf("%s full contract is HTML-escaped or missing", protocol)
		}
	}
}

// S1.2: the shipped default is legacy, and no relay switch default departs
// from h22.
func TestH25S1SwitchDefaults(t *testing.T) {
	want := map[string]any{
		"relay_protocol": "legacy", "relay_tool_contracts": "summary", "relay_tool_delivery": "immediate",
		"tool_policy_checks": "log", "history_conflict": "replay", "relay_incomplete": "error",
		"relay_hide_encrypted_tools": true, "coerce_scalar_strings": true, "retryable_error_hint": true,
		"schema_shadow": false,
		// A1 native promotion: off by default (a1_promotion.go).
		"relay_native_promotion": "off", "relay_native_promotion_history": "h1",
		"relay_native_promotion_block": a1DefaultBlock,
		"relay_no_catalog_transport":   "error",
	}
	if got := defaultRelayOptions().describe(); !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %v", got)
	}
	if got := relayOptionsFrom(map[string]any{}).describe(); !reflect.DeepEqual(got, want) {
		t.Fatalf("unprepared source defaults = %v", got)
	}
	// A JSON body cannot plant switches: the key needs the private type.
	forged := map[string]any{relayOptionsKey: map[string]any{"options": map[string]any{"Protocol": "references"}}}
	if relayOptionsFrom(forged).Protocol != relayProtocolLegacy {
		t.Fatal("forged options honoured")
	}
	if strings.Contains(string(jsonBytes(withRelayOptions(map[string]any{}, defaultRelayOptions()))), "legacy") {
		t.Fatal("frozen options serialised")
	}
}

func s1Reconfigure(t *testing.T, service *Service, yaml string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
	if _, err := service.Handle("plugin.reconfigure", raw); err != nil {
		t.Fatalf("reconfigure failed: %v", err)
	}
}

const s1AllSwitchesYAML = `relay_protocol: references
relay_tool_contracts: full
relay_tool_delivery: batch
tool_policy_checks: enforce
history_conflict: reject
relay_incomplete: passthrough
relay_hide_encrypted_tools: false
coerce_scalar_strings: false
retryable_error_hint: false
schema_shadow: true
`

// S1.5 / I4: every switch applies, and deleting its line restores the default
// even though settings.json still holds the old value -- live and across a
// restart.
func TestH25S1DeletedSwitchLinesRestoreDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	service := NewService()
	s1Reconfigure(t, service, s1AllSwitchesYAML)
	set := map[string]any{
		"relay_protocol": "references", "relay_tool_contracts": "full", "relay_tool_delivery": "batch",
		"tool_policy_checks": "enforce", "history_conflict": "reject", "relay_incomplete": "passthrough",
		"relay_hide_encrypted_tools": false, "coerce_scalar_strings": false, "retryable_error_hint": false,
		"schema_shadow": true,
		// A1 switches are not in s1AllSwitchesYAML: their defaults
		// (TestA1Switches* cover writing and deleting them).
		"relay_native_promotion": "off", "relay_native_promotion_history": "h1",
		"relay_native_promotion_block": a1DefaultBlock,
		"relay_no_catalog_transport":   "error",
	}
	if got := relayOptionsFromConfig(service.config()).describe(); !reflect.DeepEqual(got, set) {
		t.Fatalf("switches not applied: %v", got)
	}
	cached, err := os.ReadFile(filepath.Join("plugins", "oai-basispoints-data", "settings.json"))
	if err != nil || !strings.Contains(string(cached), `"relay_protocol": "references"`) {
		t.Fatalf("fixture: settings.json does not hold the old values: %v", err)
	}
	defaults := defaultRelayOptions().describe()
	s1Reconfigure(t, service, "timeout_seconds: 300\n")
	if got := relayOptionsFromConfig(service.config()).describe(); !reflect.DeepEqual(got, defaults) {
		t.Fatalf("deleted lines resurrected live: %v", got)
	}
	s1Reconfigure(t, service, s1AllSwitchesYAML)
	restarted := NewService()
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("timeout_seconds: 300\nrelay_protocol:\n")})
	if _, err := restarted.Handle("plugin.register", raw); err != nil {
		t.Fatal(err)
	}
	if got := relayOptionsFromConfig(restarted.config()).describe(); !reflect.DeepEqual(got, defaults) {
		t.Fatalf("deleted lines resurrected after restart: %v", got)
	}
}

// I4: no value of any switch can fail registration. Each bad value falls back
// to its default, says so once in a warning, and the rest of the file applies.
func TestH25S1InvalidSwitchValuesNeverFailRegistration(t *testing.T) {
	t.Chdir(t.TempDir())
	service := NewService()
	var logs []map[string]any
	var mu sync.Mutex
	service.SetHost(func(method string, payload, out any) error {
		if method == "host.log" {
			mu.Lock()
			logs = append(logs, payload.(map[string]any))
			mu.Unlock()
		}
		return nil
	})
	yaml := `relay_protocol: typo
relay_tool_contracts: [full]
relay_tool_delivery: {mode: batch}
tool_policy_checks: 42
history_conflict: REJECT
relay_incomplete: ""
relay_hide_encrypted_tools: maybe
coerce_scalar_strings: "false"
retryable_error_hint: [true]
schema_shadow: yes please
max_concurrency_per_credential: 3
`
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
	result, err := service.Handle("plugin.register", raw)
	if err != nil {
		t.Fatalf("invalid switch values failed registration: %v", err)
	}
	cfg := service.config()
	if cfg.MaxConcurrencyPerCredential != 3 {
		t.Fatal("valid keys next to invalid switches were not applied")
	}
	want := defaultRelayOptions().describe()
	want["history_conflict"] = "reject" // case-insensitive, valid
	if got := relayOptionsFromConfig(cfg).describe(); !reflect.DeepEqual(got, want) {
		t.Fatalf("effective switches = %v", got)
	}
	registered := result.(map[string]any)["config"].(Config)
	if registered.RelayProtocol != relayProtocolLegacy || registered.HistoryConflict != "reject" {
		t.Fatal("registration does not report the effective values")
	}
	service.mu.RLock()
	warnings := append([]string(nil), service.configWarnings...)
	service.mu.RUnlock()
	if len(warnings) != 8 {
		t.Fatalf("warnings = %d: %v", len(warnings), warnings)
	}
	if len(logs) != 0 {
		t.Fatal("logged from inside plugin.register")
	}
	// Logged once, as a warning, on the next request.
	for i := 0; i < 2; i++ {
		_, _ = service.Handle("executor.execute", jsonBytes(map[string]any{"host_callback_id": "cb-1"}))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || logs[0]["level"] != "warn" || logs[0]["host_callback_id"] != "cb-1" || !strings.Contains(stringValue(logs[0]["message"]), "relay_protocol: unsupported value") {
		t.Fatalf("warning log = %v", logs)
	}
}

// S1.5: the effective switches, the last warnings and the counters can be
// read back, and ConfigFields documents every switch.
func TestH25S1StatusRouteAndConfigFields(t *testing.T) {
	t.Chdir(t.TempDir())
	service := NewService()
	s1Reconfigure(t, service, "relay_protocol: auto\ncoerce_scalar_strings: nope\n")
	page, err := service.Handle("management.handle", jsonBytes(map[string]any{"Method": "GET", "Path": "/v0/resource/plugins/oai-basispoints/" + relayStatusPath}))
	if err != nil {
		t.Fatal(err)
	}
	response := page.(map[string]any)
	var status struct {
		Version   string            `json:"plugin_version"`
		Effective map[string]any    `json:"effective"`
		Warnings  []string          `json:"config_warnings"`
		Counters  map[string]uint64 `json:"counters"`
	}
	if response["StatusCode"] != 200 || json.Unmarshal(response["Body"].([]byte), &status) != nil {
		t.Fatalf("status route: %v", response)
	}
	if status.Version != Version || status.Effective["relay_protocol"] != "auto" || status.Effective["coerce_scalar_strings"] != true || len(status.Warnings) != 1 || status.Counters == nil {
		t.Fatalf("status = %+v", status)
	}
	listed := false
	for _, resource := range managementRegistration()["Resources"].([]map[string]any) {
		listed = listed || resource["Path"] == relayStatusPath
	}
	if !listed {
		t.Fatal("status route not registered")
	}
	fields := map[string]bool{}
	for _, field := range registration(defaultConfig())["metadata"].(map[string]any)["ConfigFields"].([]map[string]any) {
		fields[stringValue(field["Name"])] = true
	}
	for key := range defaultRelayOptions().describe() {
		if !fields[key] {
			t.Fatalf("ConfigFields lacks %s", key)
		}
	}
}

// I4: switches are frozen when the request is prepared. A reconfigure while
// the request is in flight changes neither its decoding nor its observation.
func TestH25S1SwitchesFrozenAtRequestEntry(t *testing.T) {
	s := NewService()
	source := s1DecodeSources()["multi"]
	request := ExecutorRequest{OriginalRequest: jsonBytes(source), StorageJSON: fakeCredential(t)}
	_, _, selected, err := s.prepareRequestWithRelayContext(&request)
	if err != nil {
		t.Fatal(err)
	}
	request.selectedRelayProtocol = selected // as execute() does
	s.cfg.CoerceScalarStrings = false
	s.cfg.RelayProtocol = relayProtocolReferences
	s.cfg.SchemaShadow = true
	decodeSource := withRelayProtocolSelection(withRelayOptions(cloneObject(source), s.relayOptionsFor(request)), request.selectedRelayProtocol)
	s.attachSchemaObservation(decodeSource, "cb")
	if decodeSource[schemaObserverKey] != nil {
		t.Fatal("schema_shadow switched on mid-request")
	}
	if relayProtocolForDecode(decodeSource) != relayProtocolLegacy {
		t.Fatal("protocol changed mid-request")
	}
	if call, reason := decodeTransportItemWithReason(s1ListFilesCall(map[string]any{"path": "/", "limit": "20"}), decodeSource); reason != "" || call["arguments"] != `{"limit":20,"path":"/"}` {
		t.Fatalf("coercion changed mid-request: %q", reason)
	}
	raw, _ := json.Marshal(request)
	if strings.Contains(string(raw), "CoerceScalarStrings") || strings.Contains(string(raw), "relayOptions") {
		t.Fatal("frozen switches leaked into the host ABI")
	}
}

// S1.4 end to end in a stream: the client receives the repaired call once,
// the terminal check agrees with it, and one decode note is logged.
func TestH25S1CoercedCallStreamsAndPassesTerminalCheck(t *testing.T) {
	service := NewService()
	var mu sync.Mutex
	var emits, notes []string
	closeErr := ""
	done := make(chan struct{})
	native := s1ListFilesCall(map[string]any{"path": "/", "limit": "20"})
	chunks := []string{
		sseEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_s1", "status": "in_progress", "output": []any{}}}),
		sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": native["id"], "call_id": native["call_id"], "name": transportName, "arguments": ""}}),
		sseEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": native}),
		sseEvent("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_s1", "status": "completed", "output": []any{native}}}) + "data: [DONE]\n\n",
	}
	next := 0
	service.SetHost(func(method string, payload any, out any) error {
		request, _ := payload.(map[string]any)
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "host.http.do_stream":
			stream := out.(*upstreamStream)
			stream.StatusCode, stream.StreamID = 200, "upstream-s1"
		case "host.http.stream_read":
			chunk := out.(*streamChunk)
			if next < len(chunks) {
				chunk.Payload = []byte(chunks[next])
				next++
			} else {
				chunk.Done = true
			}
		case "host.stream.emit":
			emits = append(emits, string(request["payload"].([]byte)))
		case "host.log":
			notes = append(notes, stringValue(request["message"]))
		case "host.stream.close":
			closeErr, _ = request["error"].(string)
			close(done)
		}
		return nil
	})
	body := s1DecodeSources()["multi"]
	raw, _ := json.Marshal(map[string]any{"OriginalRequest": jsonBytes(body), "Payload": jsonBytes(body), "StorageJSON": streamTestCredential(),
		"Model": "gpt-6-astra", "Stream": true, "stream_id": "client-s1", "host_callback_id": "cb-s1"})
	if _, err := service.Handle("executor.execute_stream", raw); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(emits, "")
	if closeErr != "" || !strings.Contains(joined, `\"limit\":20`) || strings.Contains(joined, `\"limit\":\"20\"`) || strings.Contains(joined, transportName) {
		t.Fatalf("err=%q emits=%s", closeErr, joined)
	}
	count := 0
	for _, note := range notes {
		if strings.Contains(note, "scalar_coerced") {
			count++
			if strings.Contains(note, "list_files") || strings.Contains(note, "limit") {
				t.Fatalf("note leaked call content: %s", note)
			}
		}
	}
	if count != 1 {
		t.Fatalf("scalar_coerced noted %d times", count)
	}
}
