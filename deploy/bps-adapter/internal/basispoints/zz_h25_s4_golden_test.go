package basispoints

// H25 S4: the default configuration against h22's own outputs, for requests
// that declare tools whose parameters demand ciphertext.
//
// testdata/h22_s4_golden.json was recorded in the h22-live tree by
// testdata/h22_s4_golden_gen.go.txt (as a _test.go file) from the fixtures in
// zz_h25_s4_fixtures_test.go, with zz_h25_s2_fixtures_test.go and
// zz_h25_s3_fixtures_test.go next to it, all three copied unchanged:
//
//	git archive h22-live | tar -x -C /tmp/h22
//	cp zz_h25_s2_fixtures_test.go zz_h25_s3_fixtures_test.go zz_h25_s4_fixtures_test.go /tmp/h22/internal/basispoints/
//	cp testdata/h22_s4_golden_gen.go.txt /tmp/h22/internal/basispoints/zz_s4_golden_gen_test.go
//	S4_GOLDEN_OUT=$PWD/testdata/h22_s4_golden.json go test -C /tmp/h22 ./internal/basispoints/ -run TestGenerateS4Golden
//
// The tests derive what h25 must produce from h22's output and the spec's
// wording of E1/E4 alone -- lines removed, one sentence added, namespace
// dropped -- never from h25's own functions.

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

type s4H22Prompt struct {
	Body                     string `json:"body"`
	PlainBody                string `json:"plain_body"`
	Instructions             string `json:"instructions"`
	PlainInstructions        string `json:"plain_instructions"`
	Reminder                 string `json:"reminder"`
	NoRelayInstructions      string `json:"no_relay_instructions"`
	NoRelayPlainInstructions string `json:"no_relay_plain_instructions"`
}

type s4H22Decode struct {
	OK   bool   `json:"ok"`
	Call string `json:"call"`
}

type s4H22Stream struct {
	Events   []string `json:"events"`
	CloseErr string   `json:"close_err"`
}

type s4H22Transform struct {
	Body   string `json:"body"`
	Status int    `json:"status"`
	Kind   string `json:"kind"`
}

type s4H22Golden struct {
	Generator  string                    `json:"generator"`
	Prompts    map[string]s4H22Prompt    `json:"prompts"`
	Decodes    map[string]s4H22Decode    `json:"decodes"`
	Streams    map[string]s4H22Stream    `json:"streams"`
	Transforms map[string]s4H22Transform `json:"transforms"`
	Scenarios  map[string][]s3TurnResult `json:"scenarios"`
}

func loadS4Golden(t *testing.T) s4H22Golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/h22_s4_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden s4H22Golden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(golden.Generator, "h22-live 0.1.2-h22") {
		t.Fatalf("golden was not recorded from h22: %q", golden.Generator)
	}
	if len(golden.Prompts) != len(s4PromptFixtures()) || len(golden.Decodes) != len(s4DecodeFixtures()) ||
		len(golden.Streams) != len(s4StreamFixtures()) || len(golden.Transforms) != len(s4TransformFixtures()) ||
		len(golden.Scenarios) != len(s4Scenarios()) {
		t.Fatal("golden is stale: fixture counts differ")
	}
	return golden
}

// s4HideOff writes relay_hide_encrypted_tools: false, which must be h22.
func s4HideOff(cfg *Config) { cfg.RelayHideEncryptedTools = false }

// s4ExplicitDefaults writes every switch out at its default value.
func s4ExplicitDefaults(cfg *Config) {
	s2ExplicitDefaults(cfg)
	cfg.RelayHideEncryptedTools = true
	cfg.CoerceScalarStrings = true
	cfg.RetryableErrorHint = true
	cfg.SchemaShadow = false
}

// s4Sentence is the E1 sentence, as the spec words it ("these tools are not
// available through this relay"), in the two prologue wordings.
func s4Sentence(hidden []string, plain bool) string {
	names := append([]string(nil), hidden...)
	sort.Strings(names)
	if plain {
		return "These client tools cannot be dispatched through run_officejs, so do not call them: " + strings.Join(names, ", ") + "."
	}
	return "These client tools are declared by the client but are unavailable through this relay, so do not call them: " + strings.Join(names, ", ") + "."
}

// s4E1Prologue applies E1 to h22's relay prologue: each hidden tool's
// directory line goes, and the sentence follows the directory.
func s4E1Prologue(t *testing.T, text string, hidden []string, plain bool) string {
	t.Helper()
	for _, key := range hidden {
		start := strings.Index(text, "\n- "+key+" (")
		if start < 0 {
			t.Fatalf("h22 prologue has no line for %s", key)
		}
		end := start + 1 + strings.Index(text[start+1:], "\n")
		text = text[:start] + text[end:]
	}
	if strings.Count(text, "\n"+toolCatalogReminder) != 1 {
		t.Fatal("h22 prologue does not end its directory with the transport reminder")
	}
	return strings.Replace(text, "\n"+toolCatalogReminder, "\n"+s4Sentence(hidden, plain)+"\n"+toolCatalogReminder, 1)
}

// s4E1Reminder takes the hidden tools out of h22's reminder list.
func s4E1Reminder(t *testing.T, text string, hidden []string) string {
	t.Helper()
	const head, tail = "Client tools: ", ". Other native tools are unavailable."
	start := strings.Index(text, head)
	end := strings.Index(text, tail)
	if start < 0 || end < start {
		t.Fatalf("h22 reminder has no tool list: %q", text)
	}
	drop := map[string]bool{}
	for _, key := range hidden {
		drop[key] = true
	}
	var kept []string
	for _, name := range strings.Split(text[start+len(head):end], ", ") {
		if !drop[name] {
			kept = append(kept, name)
		}
	}
	return text[:start+len(head)] + strings.Join(kept, ", ") + text[end:]
}

// s4RewriteDeveloperTexts re-encodes an h22 body with edit applied to each
// developer message text; edit returns false to drop the message. Re-encoding
// an unedited h22 body reproduces it byte for byte (checked by the caller).
func s4RewriteDeveloperTexts(t *testing.T, body string, edit func(string) (string, bool)) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	items, _ := document["input"].([]any)
	kept := make([]any, 0, len(items))
	for _, raw := range items {
		item := objectValue(raw)
		if item != nil && item["role"] == "developer" && item["type"] == "message" {
			parts, _ := item["content"].([]any)
			if len(parts) == 1 {
				part := objectValue(parts[0])
				text, keep := edit(stringValue(part["text"]))
				if !keep {
					continue
				}
				part["text"] = text
			}
		}
		kept = append(kept, raw)
	}
	document["input"] = kept
	return string(jsonBytes(document))
}

// s4ExpectedBody is h22's body with E1 applied, and nothing else changed.
func s4ExpectedBody(t *testing.T, fixture s4PromptFixture, want s4H22Prompt, body string, plain bool) string {
	t.Helper()
	if identity := s4RewriteDeveloperTexts(t, body, func(text string) (string, bool) { return text, true }); identity != body {
		t.Fatalf("%s: h22 body does not survive re-encoding", fixture.Name)
	}
	if len(fixture.Hidden) == 0 {
		return body
	}
	instructions, noRelay := want.Instructions, want.NoRelayInstructions
	if plain {
		instructions, noRelay = want.PlainInstructions, want.NoRelayPlainInstructions
	}
	replaced, droppedReminder := 0, false
	expected := s4RewriteDeveloperTexts(t, body, func(text string) (string, bool) {
		switch {
		case strings.HasPrefix(text, instructions):
			replaced++
			if fixture.AllHidden {
				return noRelay + " " + s4Sentence(fixture.Hidden, plain) + text[len(instructions):], true
			}
			return s4E1Prologue(t, instructions, fixture.Hidden, plain) + text[len(instructions):], true
		case text == want.Reminder:
			if fixture.AllHidden {
				droppedReminder = true
				return "", false
			}
			return s4E1Reminder(t, text, fixture.Hidden), true
		}
		return text, true
	})
	if replaced != 1 || fixture.AllHidden != droppedReminder {
		t.Fatalf("%s: h22 body has %d prologues (reminder dropped %v)", fixture.Name, replaced, droppedReminder)
	}
	return expected
}

// I1 with exception E1: with the switch at its default (left out or written
// out), every upstream body is h22's except that the hidden tools' directory
// lines and reminder names are gone and one sentence names them. Requests that
// hide nothing -- flat names, look-alike marks, Lite, tool_choice none,
// additional_tools or tool_search_output declarations -- are h22's bytes.
func TestH25S4DefaultPromptIsH22ExceptE1(t *testing.T) {
	golden := loadS4Golden(t)
	hidingFixtures := 0
	for _, configure := range []func(*Config){nil, s4ExplicitDefaults} {
		for _, fixture := range s4PromptFixtures() {
			want := golden.Prompts[fixture.Name]
			cfg := defaultConfig()
			if configure != nil {
				configure(&cfg)
			}
			body, err := prepareResponsesBody(cloneObject(fixture.Source), cfg)
			if err != nil {
				t.Fatal(fixture.Name, err)
			}
			if got, expected := string(jsonBytes(body)), s4ExpectedBody(t, fixture, want, want.Body, false); got != expected {
				t.Errorf("%s: upstream body is not h22's + E1\n got: %s\nwant: %s", fixture.Name, got, expected)
			}
			plainCfg := cfg
			plainCfg.ClientIdentity.PlainPrologue = true
			source := cloneObject(fixture.Source)
			plainBody, err := prepareResponsesBodyFor(source, plainCfg, conversationScope(source), persona{}, identityHygiene)
			if err != nil {
				t.Fatal(fixture.Name, err)
			}
			if got, expected := string(jsonBytes(plainBody)), s4ExpectedBody(t, fixture, want, want.PlainBody, true); got != expected {
				t.Errorf("%s: plain_prologue body is not h22's + E1\n got: %s\nwant: %s", fixture.Name, got, expected)
			}
			// The plain wording must not bring back the relay self-description
			// plain_prologue exists to remove.
			if len(fixture.Hidden) > 0 {
				hidingFixtures++
				sentence := s4Sentence(fixture.Hidden, true)
				if strings.Contains(sentence, "relay") || strings.Contains(sentence, "proxy") || !strings.Contains(string(jsonBytes(plainBody)), s2JSONText(sentence)) {
					t.Errorf("%s: plain E1 sentence missing or self-describing", fixture.Name)
				}
				for _, key := range fixture.Hidden {
					if strings.Contains(string(jsonBytes(body)), "- "+key+" (") {
						t.Errorf("%s: %s still listed", fixture.Name, key)
					}
				}
			}
		}
	}
	if hidingFixtures != 2*8 {
		t.Fatalf("expected 8 hiding fixtures per configuration, got %d", hidingFixtures)
	}
}

// relay_hide_encrypted_tools: false is h22, byte for byte.
func TestH25S4SwitchOffPromptIsH22Bytes(t *testing.T) {
	golden := loadS4Golden(t)
	for _, fixture := range s4PromptFixtures() {
		want := golden.Prompts[fixture.Name]
		cfg := defaultConfig()
		s4HideOff(&cfg)
		body, err := prepareResponsesBody(cloneObject(fixture.Source), cfg)
		if err != nil {
			t.Fatal(fixture.Name, err)
		}
		if got := string(jsonBytes(body)); got != want.Body {
			t.Errorf("%s: switched off, body differs from h22\n got: %s\nwant: %s", fixture.Name, got, want.Body)
		}
		plainCfg := cfg
		plainCfg.ClientIdentity.PlainPrologue = true
		source := cloneObject(fixture.Source)
		plainBody, err := prepareResponsesBodyFor(source, plainCfg, conversationScope(source), persona{}, identityHygiene)
		if err != nil {
			t.Fatal(fixture.Name, err)
		}
		if got := string(jsonBytes(plainBody)); got != want.PlainBody {
			t.Errorf("%s: switched off, plain body differs from h22", fixture.Name)
		}
		frozen := withRelayOptions(cloneObject(fixture.Source), relayOptionsFromConfig(cfg))
		if clientToolProtocolInstructionsFor(frozen, false) != want.Instructions || clientToolProtocolInstructionsFor(frozen, true) != want.PlainInstructions ||
			clientToolProtocolReminder(frozen) != want.Reminder {
			t.Errorf("%s: switched off, prologue texts differ from h22", fixture.Name)
		}
	}
}

// s4WithoutNamespace removes the namespace from h22's call JSON (E4).
func s4WithoutNamespace(t *testing.T, call string) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(call))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		t.Fatal(err)
	}
	if stringValue(object["namespace"]) == "" {
		t.Fatalf("h22 delivered this call without a namespace: %s", call)
	}
	delete(object, "namespace")
	return string(jsonBytes(object))
}

// I2 with exception E4: every run_officejs call decodes exactly as in h22,
// success or failure, except that a call to a namespaced tool whose
// parameters carry the mark -- declared in tools, additional_tools or
// tool_search_output -- comes back without its namespace. Off, it is h22.
func TestH25S4DecodeIsH22ExceptE4(t *testing.T) {
	golden := loadS4Golden(t)
	sources := s4DecodeSources()
	blocked := 0
	for _, fixture := range s4DecodeFixtures() {
		want := golden.Decodes[fixture.Name]
		for _, mode := range []struct {
			name      string
			configure func(*Config)
		}{{"default", nil}, {"explicit", s4ExplicitDefaults}, {"off", s4HideOff}} {
			source := s2Source(cloneObject(sources[fixture.Source]), mode.configure)
			for round := 0; round < 2; round++ {
				call, reason := decodeTransportItemWithReason(cloneObject(fixture.Native), source)
				got := s4H22Decode{OK: reason == ""}
				if got.OK {
					got.Call = string(jsonBytes(call))
				}
				expected := want
				if fixture.Blocked && mode.name != "off" {
					if !want.OK {
						t.Fatalf("%s: a blocked fixture must be one h22 delivered", fixture.Name)
					}
					expected.Call = s4WithoutNamespace(t, want.Call)
				}
				if got != expected {
					t.Fatalf("%s/%s: decode differs\n got: %+v (reason %q)\nwant: %+v", fixture.Name, mode.name, got, reason, expected)
				}
			}
		}
		if fixture.Blocked {
			blocked++
		}
	}
	if blocked != 4*3+6 {
		t.Fatalf("blocked fixtures = %d", blocked)
	}
	// Only a run_officejs call is ever blocked. The decoder also resolves a
	// bare call named by its full key (as h22 did); that is not a relay call
	// written in plaintext, and it keeps its namespace.
	bare := map[string]any{"type": "function_call", "id": "fc_s4_bare", "call_id": "s4_bare", "name": "collaboration.spawn_agent", "status": "completed",
		"arguments": `{"message":"m","task_name":"t"}`}
	call, reason := decodeTransportItemWithReason(bare, s2Source(cloneObject(sources["relay"]), nil))
	if reason != "" || call["namespace"] != "collaboration" {
		t.Fatalf("a bare call lost its namespace: %v %q", call, reason)
	}
}

// s4StripNamespaces drops the namespace of the blocked calls from h22's
// normalized client events: the call in added/done and in the terminal output.
func s4StripNamespaces(t *testing.T, events []string, blocked []string) []string {
	t.Helper()
	ids := map[string]bool{}
	for _, id := range blocked {
		ids[id] = true
	}
	stripped := 0
	strip := func(item map[string]any) {
		if item != nil && ids[stringValue(item["call_id"])] {
			if _, ok := item["namespace"]; ok {
				delete(item, "namespace")
				stripped++
			}
		}
	}
	out := make([]string, 0, len(events))
	for _, event := range events {
		parts := strings.SplitN(event, "\x00", 2)
		if len(parts) != 2 || strings.HasPrefix(parts[1], "RAW ") {
			out = append(out, event)
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(parts[1]))
		decoder.UseNumber()
		var value map[string]any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		strip(objectValue(value["item"]))
		if response := objectValue(value["response"]); response != nil {
			output, _ := response["output"].([]any)
			for _, raw := range output {
				strip(objectValue(raw))
			}
		}
		canonical, _ := json.Marshal(value)
		out = append(out, parts[0]+"\x00"+string(canonical))
	}
	if len(blocked) > 0 && stripped == 0 {
		t.Fatal("h22 events carry no namespace to drop")
	}
	return out
}

// The client's events for a blocked call are h22's, minus the namespace; a
// Lite native call to the very same tool is h22's exactly. Each blocked call
// is noted once although a stream decodes it twice.
func TestH25S4StreamIsH22ExceptE4(t *testing.T) {
	golden := loadS4Golden(t)
	for _, fixture := range s4StreamFixtures() {
		fixture := fixture
		want := golden.Streams[fixture.Name]
		for _, mode := range []struct {
			name      string
			configure func(*Config)
		}{{"default", nil}, {"explicit", s4ExplicitDefaults}, {"off", s4HideOff}} {
			t.Run(mode.name+"/"+fixture.Name, func(t *testing.T) {
				result := runStreamTestWith(t, mode.configure, cloneObject(fixture.Request), fixture.Chunks)
				expected := want.Events
				blocked := fixture.Blocked
				if mode.name == "off" {
					blocked = nil
				}
				expected = s4StripNamespaces(t, expected, blocked)
				if got := s2NormalizeStream(result.joined()); strings.Join(got, "\n") != strings.Join(expected, "\n") {
					t.Fatalf("client events differ\n got: %q\nwant: %q", got, expected)
				}
				if result.closeErr != want.CloseErr {
					t.Fatalf("close %q, h22 %q", result.closeErr, want.CloseErr)
				}
				notes := 0
				for _, entry := range result.logs {
					fields := objectValue(entry["fields"])
					if fields["reason"] == "encrypted_tool_blocked" {
						notes++
						if fields["event"] != "relay_decode_note" || entry["level"] != "info" {
							t.Fatalf("blocked-call log %v", entry)
						}
					}
					text := string(jsonBytes(entry))
					for _, secret := range []string{"PLAINTEXT", "spawn_agent", "send_message", "collaboration", "s4_st_"} {
						if strings.Contains(text, secret) {
							t.Fatalf("log line carries request content %q: %s", secret, text)
						}
					}
				}
				if notes != len(blocked) {
					t.Fatalf("encrypted_tool_blocked logged %d times, want %d", notes, len(blocked))
				}
			})
		}
	}
}

func TestH25S4TransformIsH22ExceptE4(t *testing.T) {
	golden := loadS4Golden(t)
	for _, fixture := range s4TransformFixtures() {
		want := golden.Transforms[fixture.Name]
		for _, mode := range []struct {
			name      string
			configure func(*Config)
		}{{"default", nil}, {"off", s4HideOff}} {
			body, _, _, err := transformResponseBody(jsonBytes(fixture.Response), s2Source(cloneObject(fixture.Source), mode.configure))
			if err != nil || want.Kind != "" {
				t.Fatalf("%s/%s: %v (h22 %d %s)", fixture.Name, mode.name, err, want.Status, want.Kind)
			}
			expected := want.Body
			if mode.name == "default" {
				var document map[string]any
				decoder := json.NewDecoder(strings.NewReader(expected))
				decoder.UseNumber()
				if err := decoder.Decode(&document); err != nil {
					t.Fatal(err)
				}
				ids := map[string]bool{}
				for _, id := range fixture.Blocked {
					ids[id] = true
				}
				output, _ := document["output"].([]any)
				for _, raw := range output {
					if item := objectValue(raw); ids[stringValue(item["call_id"])] {
						delete(item, "namespace")
					}
				}
				expected = string(jsonBytes(document))
			}
			if string(body) != expected {
				t.Fatalf("%s/%s: body\n got: %s\nwant: %s", fixture.Name, mode.name, body, expected)
			}
		}
	}
}

// s4E1Body applies E1 to a whole upstream body: the relay prologue and the
// reminder (the conversation turns have no instructions and no plain mode).
func s4E1Body(t *testing.T, body string, hidden []string) string {
	t.Helper()
	prologues := 0
	expected := s4RewriteDeveloperTexts(t, body, func(text string) (string, bool) {
		switch {
		case strings.Contains(text, "Available client tools:\n"):
			prologues++
			guard := strings.Index(text, " "+accountNameGuard)
			if guard < 0 {
				guard = len(text)
			}
			return s4E1Prologue(t, text[:guard], hidden, false) + text[guard:], true
		case strings.HasPrefix(text, toolCatalogReminder+" The inner tool"):
			return s4E1Reminder(t, text, hidden), true
		}
		return text, true
	})
	if prologues != 1 {
		t.Fatalf("upstream body has %d relay prologues", prologues)
	}
	return expected
}

// A conversation around a blocked call, end to end: what goes upstream on
// every turn is h22's + E1, whichever way the client echoes the call (h25's
// delivery without namespace, h22's with it, non-streaming, or cold), and
// the cached run_officejs replays exactly as in h22. A Lite native
// conversation with the same tool is h22's throughout.
func TestH25S4ConversationIsH22ExceptE1E4(t *testing.T) {
	golden := loadS4Golden(t)
	for _, scenario := range s4Scenarios() {
		want := golden.Scenarios[scenario.Name]
		for _, mode := range []struct {
			name      string
			configure func(*Config)
		}{{"default", nil}, {"off", s4HideOff}} {
			// The replay caches are process-wide; each run is its own caller
			// (part of h25's replay scope, never part of the body), so the two
			// runs cannot share a replay record.
			got := s3RunScenario(t, s4WithCaller(scenario, "caller-s4-"+mode.name), mode.configure)
			if len(got) != len(want) {
				t.Fatalf("%s/%s: %d turns", scenario.Name, mode.name, len(got))
			}
			for index := range want {
				expected := want[index]
				expected.Delivered = append([]string(nil), expected.Delivered...)
				lite := strings.Contains(scenario.Name, "lite")
				if mode.name == "default" && !lite {
					expected.Upstream = s4E1Body(t, expected.Upstream, s4CollaborationHidden)
					for i, call := range expected.Delivered {
						expected.Delivered[i] = s4WithoutNamespace(t, call)
					}
				}
				if got[index].Upstream != expected.Upstream {
					t.Fatalf("%s/%s turn %d: upstream differs\n got: %s\nwant: %s", scenario.Name, mode.name, index, got[index].Upstream, expected.Upstream)
				}
				if strings.Join(got[index].Delivered, "\n") != strings.Join(expected.Delivered, "\n") ||
					got[index].Status != expected.Status || got[index].Kind != expected.Kind || got[index].CloseErr != expected.CloseErr {
					t.Fatalf("%s/%s turn %d: result %+v, want %+v", scenario.Name, mode.name, index, got[index], expected)
				}
			}
		}
	}
}

// s4WithCaller runs a scenario as another authenticated caller.
func s4WithCaller(scenario s3Scenario, caller string) s3Scenario {
	turns := make([]s3Turn, len(scenario.Turns))
	for index, turn := range scenario.Turns {
		turn.Metadata = s3Meta(caller)
		turns[index] = turn
	}
	return s3Scenario{Name: scenario.Name, Identity: scenario.Identity, Turns: turns}
}
