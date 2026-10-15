package basispoints

// H25 S5: release checks across the stages -- the version, the switch table
// of H25-SPEC section 2 against everything that has to know about a switch,
// and the opt-in switches never losing an h22 success on the S5 cases.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestH25S5Version(t *testing.T) {
	if Version != "0.1.2-h28-workspace1-a1wide2" {
		t.Fatalf("Version = %q", Version)
	}
	if registration(defaultConfig())["metadata"].(map[string]any)["Version"] != "0.1.2-h28-workspace1-a1wide2" {
		t.Fatal("registration reports another version")
	}
}

// s5Switch is one row of the H25-SPEC section 2 table.
type s5Switch struct {
	key          string
	defaultValue any
	others       []any
	invalid      string // a config.yaml value that must fall back
}

func s5SwitchTable() []s5Switch {
	return []s5Switch{
		{"relay_protocol", "legacy", []any{"auto", "references"}, "nonsense"},
		{"relay_tool_contracts", "summary", []any{"full"}, "[full]"},
		{"relay_tool_delivery", "immediate", []any{"batch"}, "later"},
		{"tool_policy_checks", "log", []any{"enforce"}, "maybe"},
		{"history_conflict", "replay", []any{"reject"}, "{mode: reject}"},
		{"relay_incomplete", "error", []any{"passthrough"}, "drop"},
		{"relay_hide_encrypted_tools", true, []any{false}, "sometimes"},
		{"coerce_scalar_strings", true, []any{false}, `"false"`},
		{"retryable_error_hint", true, []any{false}, "[true]"},
		{"schema_shadow", false, []any{true}, "42"},
		// A1 native promotion (a1_promotion.go): default off = h25.
		{"relay_native_promotion", "off", []any{"shadow", "on"}, "maybe"},
		{"relay_native_promotion_history", "h1", []any{"h2"}, "h3"},
		{"relay_native_promotion_block", a1DefaultBlock, []any{"none", "enum,ref"}, "bogus_feature"},
		// a1wide2: run_officejs calls no client tool can stand for (default h22).
		{"relay_no_catalog_transport", "error", []any{"continue"}, "maybe"},
	}
}

func s5YAMLValue(value any) string {
	if flag, ok := value.(bool); ok {
		if flag {
			return "true"
		}
		return "false"
	}
	return value.(string)
}

// s5NoConfig registers without any config_yaml (no plugin section at all).
const s5NoConfig = "\x00none"

// s5Register registers a fresh plugin with config. The data directory is the
// default one, relative to the working directory the caller chose.
func s5Register(t *testing.T, config string, hostLog *[]map[string]any) *Service {
	t.Helper()
	service := NewService()
	service.SetHost(func(method string, payload, out any) error {
		if method == "host.log" && hostLog != nil {
			*hostLog = append(*hostLog, payload.(map[string]any))
		}
		return nil
	})
	request := map[string]any{}
	if config != s5NoConfig {
		request["config_yaml"] = []byte(config)
	}
	raw, _ := json.Marshal(request)
	if _, err := service.Handle("plugin.register", raw); err != nil {
		t.Fatalf("plugin.register failed: %v", err)
	}
	return service
}

// s5Status reads the relay-status route.
func s5Status(t *testing.T, service *Service) (map[string]any, []string) {
	t.Helper()
	page, err := service.Handle("management.handle", jsonBytes(map[string]any{"Method": "GET", "Path": "/v0/resource/plugins/oai-basispoints/" + relayStatusPath}))
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Version   string         `json:"plugin_version"`
		Effective map[string]any `json:"effective"`
		Warnings  []string       `json:"config_warnings"`
	}
	if err := json.Unmarshal(page.(map[string]any)["Body"].([]byte), &status); err != nil || status.Version != Version {
		t.Fatalf("relay-status unreadable: %v", err)
	}
	return status.Effective, status.Warnings
}

// I4 and the switch table, as one inventory: the switches Config reads from
// raw YAML nodes are exactly the ten of H25-SPEC section 2, each with the
// table's default; each is in the frozen per-request snapshot, in
// relay-status, in ConfigFields and in config.example.yaml; every listed
// value applies without a warning; deleting the line -- or the whole plugin
// section, so that only settings.json still holds a value -- restores the
// default; and an unreadable value falls back to the default with exactly one
// warning and never fails registration.
func TestH25S5SwitchTableIsCompleteAndSafe(t *testing.T) {
	table := s5SwitchTable()
	byKey := map[string]s5Switch{}
	for _, row := range table {
		byKey[row.key] = row
	}
	// Config: the yaml:"-" fields are the switches, all of them in the table.
	configType := reflect.TypeOf(Config{})
	var raw []string
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		if field.Tag.Get("yaml") != "-" {
			continue
		}
		key := strings.Split(field.Tag.Get("json"), ",")[0]
		raw = append(raw, key)
		row, ok := byKey[key]
		if !ok {
			t.Errorf("Config.%s (%s) is read from raw YAML but is not in the switch table", field.Name, key)
			continue
		}
		if got := reflect.ValueOf(defaultConfig()).Field(index).Interface(); got != row.defaultValue {
			t.Errorf("%s: default %v, table %v", key, got, row.defaultValue)
		}
	}
	if len(raw) != len(table) {
		t.Fatalf("switches in Config %v, table has %d", raw, len(table))
	}
	// The per-request snapshot and its description carry every switch.
	if fields := reflect.TypeOf(relayOptions{}).NumField(); fields != len(table) {
		t.Fatalf("relayOptions has %d fields", fields)
	}
	defaults := defaultRelayOptions().describe()
	if len(defaults) != len(table) {
		t.Fatalf("describe() has %d keys", len(defaults))
	}
	for _, row := range table {
		if defaults[row.key] != row.defaultValue {
			t.Errorf("%s: described default %v", row.key, defaults[row.key])
		}
	}
	// ConfigFields and the example file document every switch.
	fields := map[string]string{}
	for _, field := range registration(defaultConfig())["metadata"].(map[string]any)["ConfigFields"].([]map[string]any) {
		fields[field["Name"].(string)] = field["Type"].(string)
	}
	example, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range table {
		wantType := "string"
		if _, ok := row.defaultValue.(bool); ok {
			wantType = "boolean"
		}
		if fields[row.key] != wantType {
			t.Errorf("%s: ConfigFields type %q", row.key, fields[row.key])
		}
		if !strings.Contains(string(example), "# "+row.key+": "+s5YAMLValue(row.defaultValue)) {
			t.Errorf("%s: config.example.yaml does not document the default", row.key)
		}
	}

	t.Chdir(t.TempDir())
	settings := filepath.Join(defaultConfig().DataDir, "settings.json")
	for _, row := range table {
		for _, value := range row.others {
			line := row.key + ": " + s5YAMLValue(value) + "\n"
			service := s5Register(t, line, nil)
			effective, warnings := s5Status(t, service)
			if effective[row.key] != value || len(warnings) != 0 {
				t.Errorf("%s: %q gave %v (warnings %v)", row.key, line, effective[row.key], warnings)
			}
			// The rest of the table keeps its defaults.
			for _, other := range table {
				if other.key != row.key && effective[other.key] != other.defaultValue {
					t.Errorf("%s: setting %s moved %s to %v", row.key, row.key, other.key, effective[other.key])
				}
			}
			cached, err := os.ReadFile(settings)
			if err != nil || !strings.Contains(string(cached), `"`+row.key+`": `+string(jsonBytes(value))) {
				t.Fatalf("%s: settings.json does not hold the value (fixture): %v", row.key, err)
			}
			// Line deleted, live: the default.
			s1Reconfigure(t, service, "timeout_seconds: 300\n")
			if got := relayOptionsFromConfig(service.config()).describe()[row.key]; got != row.defaultValue {
				t.Errorf("%s: deleted line kept %v", row.key, got)
			}
			// Restart with the whole plugin section gone: settings.json is
			// all there is, and it does not bring the switch back.
			s5Register(t, line, nil)
			if cached, err := os.ReadFile(settings); err != nil || !strings.Contains(string(cached), `"`+row.key+`": `+string(jsonBytes(value))) {
				t.Fatalf("%s: settings.json does not hold the value before the restart (fixture): %v", row.key, err)
			}
			restarted := s5Register(t, s5NoConfig, nil)
			if got := relayOptionsFromConfig(restarted.config()).describe()[row.key]; got != row.defaultValue {
				t.Errorf("%s: restart without config.yaml restored %v from settings.json", row.key, got)
			}
		}
		// An unreadable value: the default, one warning naming the key, the
		// rest of the file applied, and no host call during registration
		// (the warning is logged on the next request; TestH25S1Invalid...).
		var logs []map[string]any
		service := s5Register(t, row.key+": "+row.invalid+"\nmax_concurrency_per_credential: 3\n", &logs)
		effective, warnings := s5Status(t, service)
		if effective[row.key] != row.defaultValue || len(warnings) != 1 || !strings.HasPrefix(warnings[0], row.key+":") {
			t.Errorf("%s: invalid %q gave %v (warnings %v)", row.key, row.invalid, effective[row.key], warnings)
		}
		if service.config().MaxConcurrencyPerCredential != 3 {
			t.Errorf("%s: an invalid switch cost the rest of the file", row.key)
		}
		if len(logs) != 0 {
			t.Errorf("%s: registration called host.log", row.key)
		}
	}
	_ = sort.Strings
}

// The opt-in values are not held to I1/I2, but on these cases -- whose
// upstream always answers in the legacy envelope -- none of the protocol,
// catalogue, delivery, terminal or observation switches loses a turn h22
// completed: the client still gets a successful stream (or response) with
// the same tool calls. tool_policy_checks: enforce and history_conflict:
// reject are left out on purpose: refusing what h22 accepted is what they
// are for.
func TestH25S5OptInSwitchesKeepEveryH22Success(t *testing.T) {
	golden := loadS5Golden(t)
	overlays := map[string]string{
		"auto":                   "relay_protocol: auto\n",
		"references":             "relay_protocol: references\n",
		"contracts_full":         "relay_tool_contracts: full\n",
		"delivery_batch":         "relay_tool_delivery: batch\n",
		"incomplete_passthrough": "relay_incomplete: passthrough\n",
		"schema_shadow":          "schema_shadow: true\n",
	}
	names := make([]string, 0, len(overlays))
	for name := range overlays {
		names = append(names, name)
	}
	sort.Strings(names)
	profile := s5Profiles()[0]
	want := golden.Profiles[profile.Name]
	for _, name := range names {
		kept := 0
		for _, c := range s5Cases() {
			s5ResetCaches()
			got := s5RunCase(t, profile, c, overlays[name])
			for index, turn := range c.Turns {
				recorded := want[c.Name][index]
				if recorded.Status != 0 || recorded.CloseErr != "" || !s5Completed(recorded) {
					continue
				}
				label := name + "/" + c.Name + " turn " + string(rune('1'+index))
				if got[index].Status != 0 || got[index].CloseErr != "" || !s5Completed(got[index]) {
					t.Errorf("%s: h22 completed this turn, h25 did not: %d %s %q", label, got[index].Status, got[index].Kind, got[index].CloseErr)
					continue
				}
				if gotCalls, wantCalls := s5DeliveredCalls(t, got[index], turn.Blocked), s5DeliveredCalls(t, recorded, turn.Blocked); gotCalls != wantCalls {
					t.Errorf("%s: delivered calls differ\n got: %s\nwant: %s", label, gotCalls, wantCalls)
				}
				kept++
			}
		}
		if kept < 60 {
			t.Errorf("%s: only %d successful turns compared", name, kept)
		}
	}
}

// The upstream prompt is a pure function of the request in every mode, so a
// conversation keeps its cached prefix. h22 (and rc2's full mode) appended
// the per-custom-tool reminder sentences in map order, a different prefix on
// almost every request with two or more custom tools; summary has written
// them sorted since S1, full since S5.
func TestH25S5UpstreamPromptIsDeterministicInEveryMode(t *testing.T) {
	custom := func(name string) map[string]any {
		return map[string]any{"type": "custom", "name": name, "description": "Raw " + name + "."}
	}
	threeCustom := s5RelayWith("", []any{custom("z_patch"), s5ExecTool(), custom("apply_patch"), s5MCP(), custom("m_patch")}, s5User("three custom tools"))
	sources := []map[string]any{threeCustom}
	for _, c := range s5Cases() {
		sources = append(sources, c.Turns[0].Request)
	}
	modes := []struct {
		name      string
		configure func(*Config)
	}{
		{"default", nil},
		{"full", func(cfg *Config) { cfg.RelayToolContracts = relayToolContractsFull }},
		{"references", func(cfg *Config) { cfg.RelayProtocol = relayProtocolReferences }},
		{"references_full", func(cfg *Config) {
			cfg.RelayProtocol, cfg.RelayToolContracts = relayProtocolReferences, relayToolContractsFull
		}},
		{"auto", func(cfg *Config) { cfg.RelayProtocol = relayProtocolAuto }},
		{"hide_off", func(cfg *Config) { cfg.RelayHideEncryptedTools = false }},
	}
	for _, mode := range modes {
		cfg := defaultConfig()
		if mode.configure != nil {
			mode.configure(&cfg)
		}
		for index, source := range sources {
			first, err := prepareResponsesBody(cloneObject(source), cfg)
			if err != nil {
				continue // a request this mode refuses is refused every time
			}
			want := string(jsonBytes(first))
			for round := 0; round < 20; round++ {
				again, err := prepareResponsesBody(cloneObject(source), cfg)
				if err != nil || string(jsonBytes(again)) != want {
					t.Fatalf("%s: source %d prepared differently on round %d", mode.name, index, round)
				}
			}
			if index == 0 && (mode.name == "default" || mode.name == "full") {
				text := string(jsonBytes(first))
				a := strings.Index(text, "Custom tool apply_patch receives")
				m := strings.Index(text, "Custom tool m_patch receives")
				z := strings.Index(text, "Custom tool z_patch receives")
				if a < 0 || !(a < m && m < z) {
					t.Fatalf("%s: custom tool sentences not in sorted order", mode.name)
				}
			}
		}
	}
}

// s5Completed: the client saw a completed response.
func s5Completed(result s5Result) bool {
	if result.Payload != "" {
		return true
	}
	for _, event := range result.Events {
		if strings.HasPrefix(event, "response.completed\x00") {
			return true
		}
	}
	return false
}

// s5DeliveredCalls lists the tool calls of the final response (stream
// terminal or non-stream body), canonically, without the namespace of the
// calls E4 blocks.
func s5DeliveredCalls(t *testing.T, result s5Result, blocked []string) string {
	t.Helper()
	var response map[string]any
	if result.Payload != "" {
		response = map[string]any{}
		if err := json.Unmarshal([]byte(result.Payload), &response); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range result.Events {
		parts := strings.SplitN(event, "\x00", 2)
		if parts[0] == "response.completed" {
			var value map[string]any
			if err := json.Unmarshal([]byte(parts[1]), &value); err != nil {
				t.Fatal(err)
			}
			response = objectValue(value["response"])
		}
	}
	skip := map[string]bool{}
	for _, id := range blocked {
		skip[id] = true
	}
	var calls []string
	output, _ := response["output"].([]any)
	for _, raw := range output {
		item := objectValue(raw)
		if kind := stringValue(item["type"]); kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		kept := map[string]any{}
		for _, key := range []string{"type", "call_id", "name", "namespace", "input"} {
			if value, ok := item[key]; ok && !(key == "namespace" && skip[stringValue(item["call_id"])]) {
				kept[key] = value
			}
		}
		if arguments, ok := item["arguments"].(string); ok {
			var parsed any
			if json.Unmarshal([]byte(arguments), &parsed) == nil {
				kept["arguments"] = parsed
			} else {
				kept["arguments"] = arguments
			}
		}
		calls = append(calls, string(jsonBytes(kept)))
	}
	return strings.Join(calls, "\n")
}
