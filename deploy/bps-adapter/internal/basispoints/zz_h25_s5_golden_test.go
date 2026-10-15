package basispoints

// H25 S5: the whole plugin under the production configuration, against
// h22's own outputs.
//
// testdata/h22_s5_golden.json was recorded in the h22-live tree by
// testdata/h22_s5_golden_gen.go.txt from the cases in
// zz_h25_s5_fixtures_test.go (compiled there unchanged): for every turn of
// every case, under the production config.yaml and under a minimal one, the
// exact upstream request (body bytes and headers), what the client received
// and any executor error. Here the same cases run through plugin.register
// and the executor of this tree.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type s5Golden struct {
	Generator string                           `json:"generator"`
	Profiles  map[string]map[string][]s5Result `json:"profiles"`
}

func loadS5Golden(t *testing.T) s5Golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/h22_s5_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden s5Golden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(golden.Generator, "h22-live 0.1.2-h22") || len(golden.Profiles) != len(s5Profiles()) {
		t.Fatalf("golden is not h22's: %q", golden.Generator)
	}
	return golden
}

// s5ResetCaches empties every process-wide cache a case can fill, so each
// case sees exactly the state its own turns build (as in the generator).
func s5ResetCaches() {
	s3ResetCaches()
	turnIDCache.Lock()
	turnIDCache.items = map[string]turnIDEntry{}
	turnIDCache.order = nil
	turnIDCache.Unlock()
}

// The configurations the cases run under: nothing written (the production
// file as it is), every switch written out at its default, and the three
// switches that carry an exception (E1/E4, E3's coercion, E5) turned off.
const (
	s5ExplicitDefaultsYAML = `relay_protocol: legacy
relay_tool_contracts: summary
relay_tool_delivery: immediate
tool_policy_checks: log
history_conflict: replay
relay_incomplete: error
relay_hide_encrypted_tools: true
coerce_scalar_strings: true
retryable_error_hint: true
schema_shadow: false
`
	s5ExceptionsOffYAML = `relay_hide_encrypted_tools: false
coerce_scalar_strings: false
retryable_error_hint: false
`
)

type s5Mode struct {
	name    string
	overlay string
	// The exceptions this configuration allows.
	hide, coerce, hint bool
}

func s5Modes() []s5Mode {
	return []s5Mode{
		{name: "default", hide: true, coerce: true, hint: true},
		{name: "explicit_defaults", overlay: s5ExplicitDefaultsYAML, hide: true, coerce: true, hint: true},
		{name: "exceptions_off", overlay: s5ExceptionsOffYAML},
	}
}

// s5EditDeveloperTexts applies edit to the text of every one-part developer
// message of an upstream body, on the body's own bytes: the production body
// is written in the add-in's key order, so it cannot be re-encoded, and the
// items inside it are plain JSON. edit returns false to drop the message.
func s5EditDeveloperTexts(t *testing.T, body string, edit func(string) (string, bool)) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	out := body
	items, _ := document["input"].([]any)
	for index, raw := range items {
		item := objectValue(raw)
		if item == nil || item["role"] != "developer" || item["type"] != "message" {
			continue
		}
		parts, _ := item["content"].([]any)
		if len(parts) != 1 {
			continue
		}
		text := stringValue(objectValue(parts[0])["text"])
		replacement, keep := edit(text)
		if !keep {
			encoded := "," + string(jsonBytes(item))
			if index == 0 || strings.Count(out, encoded) != 1 {
				t.Fatalf("developer message %d cannot be located in the body", index)
			}
			out = strings.Replace(out, encoded, "", 1)
			continue
		}
		if replacement == text {
			continue
		}
		from, to := `"text":`+string(jsonBytes(text)), `"text":`+string(jsonBytes(replacement))
		if strings.Count(out, from) != 1 {
			t.Fatalf("developer text %d cannot be located in the body", index)
		}
		out = strings.Replace(out, from, to, 1)
	}
	return out
}

// s5E1Body is h22's body with E1 applied and nothing else: the hidden tools'
// directory lines and reminder names are gone and one sentence names them;
// with nothing left, the prologue is h22's tool-less one plus the sentence
// and there is no reminder.
func s5E1Body(t *testing.T, body string, turn s5Turn, plain bool) string {
	t.Helper()
	prologues, reminders := 0, 0
	expected := s5EditDeveloperTexts(t, body, func(text string) (string, bool) {
		switch {
		case strings.Contains(text, "Available client tools:\n"):
			prologues++
			guard := strings.Index(text, " "+accountNameGuard)
			if guard < 0 {
				guard = len(text)
			}
			if turn.AllHidden {
				noTools := relayNoToolsHint
				if plain {
					noTools = plainNoToolsHint
				}
				return noTools + " " + s4Sentence(turn.Hidden, plain) + text[guard:], true
			}
			return s4E1Prologue(t, text[:guard], turn.Hidden, plain) + text[guard:], true
		case strings.HasPrefix(text, toolCatalogReminder+" The inner tool"):
			reminders++
			if turn.AllHidden {
				return "", false
			}
			return s4E1Reminder(t, text, turn.Hidden), true
		}
		return text, true
	})
	if prologues != 1 || reminders != 1 {
		t.Fatalf("h22 body has %d relay prologues and %d reminders", prologues, reminders)
	}
	return expected
}

// s5E2Body is h22's body with E2 applied: the tool-less sentence that opens
// the prologue points at tool_search_output instead.
func s5E2Body(t *testing.T, body string, plain bool) string {
	t.Helper()
	from, to := relayNoToolsHint, relaySearchedTools
	if plain {
		from, to = plainNoToolsHint, plainSearchedTools
	}
	replaced := 0
	expected := s5EditDeveloperTexts(t, body, func(text string) (string, bool) {
		if strings.HasPrefix(text, from) {
			replaced++
			return to + text[len(from):], true
		}
		return text, true
	})
	if replaced != 1 {
		t.Fatalf("h22 body has %d tool-less prologues", replaced)
	}
	return expected
}

// s5StripPayloadNamespaces is h22's non-stream response with E4 applied: the
// blocked calls lose their namespace.
func s5StripPayloadNamespaces(t *testing.T, payload string, blocked []string) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if string(jsonBytes(document)) != payload {
		t.Fatal("h22 response does not survive re-encoding")
	}
	ids := map[string]bool{}
	for _, id := range blocked {
		ids[id] = true
	}
	stripped := 0
	output, _ := document["output"].([]any)
	for _, raw := range output {
		if item := objectValue(raw); item != nil && ids[stringValue(item["call_id"])] && item["namespace"] != nil {
			delete(item, "namespace")
			stripped++
		}
	}
	if stripped != len(blocked) {
		t.Fatalf("h22 response carries %d of %d namespaces to drop", stripped, len(blocked))
	}
	return string(jsonBytes(document))
}

// s5Applies counts which exceptions the comparison actually exercised.
type s5Applies struct{ e1, e2, e3, e3coerce, e4, e5, equal, failures int }

// s5Compare holds one turn to h22's result: equal, except for the
// exceptions (H25-SPEC section 1) this turn carries and this mode allows.
func s5Compare(t *testing.T, label string, profile s5Profile, mode s5Mode, turn s5Turn, got, want s5Result, applies *s5Applies) {
	t.Helper()
	exception := false
	// I1: the upstream request.
	body := want.Upstream
	if mode.hide && len(turn.Hidden) > 0 {
		body = s5E1Body(t, body, turn, profile.Plain)
		applies.e1++
		exception = true
	}
	if turn.Searched {
		body = s5E2Body(t, body, profile.Plain)
		applies.e2++
		exception = true
	}
	if got.Upstream != body {
		t.Errorf("%s: upstream body differs from h22\n got: %s\nwant: %s", label, got.Upstream, body)
	}
	if got.Headers != want.Headers || got.UpstreamCalls != want.UpstreamCalls {
		t.Errorf("%s: upstream headers/calls differ from h22\n got: %d %s\nwant: %d %s", label, got.UpstreamCalls, got.Headers, want.UpstreamCalls, want.Headers)
	}
	// An executor error: same status and kind. A refused tool call may be
	// worded differently (E5); every other message is h22's.
	if got.Status != want.Status || got.Kind != want.Kind {
		t.Errorf("%s: executor error %d %s, h22 %d %s (%q)", label, got.Status, got.Kind, want.Status, want.Kind, got.Message)
	}
	if want.Kind != "invalid_tool_call" && got.Message != want.Message {
		t.Errorf("%s: executor error message %q, h22 %q", label, got.Message, want.Message)
	}
	if want.Status != 0 {
		applies.failures++
	}
	// E3: h22 refused the call; h25 delivers exactly this one.
	if turn.Converted != nil && (mode.coerce || !turn.Coerced) {
		if s2Failure(want.CloseErr) != "invalid_tool_call" {
			t.Fatalf("%s: not an E3 case, h22 closed with %q", label, want.CloseErr)
		}
		if got.CloseErr != "" {
			t.Errorf("%s: E3 conversion failed: %q", label, got.CloseErr)
			return
		}
		s2AssertOneDeliveredCall(t, got.raw, turn.Converted.Arguments, turn.Converted.ID)
		applies.e3++
		if turn.Coerced {
			applies.e3coerce++
		}
		return
	}
	// I2: what the client received.
	events := want.Events
	payload := want.Payload
	if mode.hide && len(turn.Blocked) > 0 {
		if turn.Stream {
			events = s4StripNamespaces(t, events, turn.Blocked)
		} else {
			payload = s5StripPayloadNamespaces(t, payload, turn.Blocked)
		}
		applies.e4++
		exception = true
	}
	if strings.Join(got.Events, "\n") != strings.Join(events, "\n") {
		t.Errorf("%s: client events differ from h22\n got: %q\nwant: %q", label, got.Events, events)
	}
	if got.Payload != payload {
		t.Errorf("%s: non-stream response differs from h22\n got: %s\nwant: %s", label, got.Payload, payload)
	}
	if s2Failure(got.CloseErr) != s2Failure(want.CloseErr) {
		t.Errorf("%s: stream closed with %q, h22 with %q", label, got.CloseErr, want.CloseErr)
	}
	if want.CloseErr != "" {
		applies.failures++
	}
	// E5: the retry hint exactly on a failure before any output.
	hinted := strings.HasSuffix(got.CloseErr, retryableErrorHint)
	wantHint := mode.hint && want.CloseErr != "" && s2ZeroOutput(want.Events)
	if hinted != wantHint {
		t.Errorf("%s: retry hint %v, want %v (close %q)", label, hinted, wantHint, got.CloseErr)
	}
	if hinted {
		applies.e5++
		exception = true
	}
	// A refused tool call says why, and no longer reads as the client's error.
	if s2Failure(got.CloseErr) == "invalid_tool_call" {
		if !strings.HasPrefix(got.CloseErr, "[relay_tool_error] invalid_tool_call") || !strings.Contains(got.CloseErr, "[reason=") || !strings.Contains(got.CloseErr, " ref=bpdiag_") {
			t.Errorf("%s: tool failure lost its reason code: %q", label, got.CloseErr)
		}
	}
	if !exception {
		applies.equal++
	}
}

// I1 + I2 end to end, under the production configuration: every case,
// every turn, upstream request bytes and headers, the client's events, the
// non-stream response and every executor error are h22's -- with nothing in
// config.yaml, and with every switch written out at its default -- except
// exactly E1..E5, each asserted on its own. With the three switches that
// carry an exception turned off, E1, E4, E5's hint and E3's coercion are gone
// and those turns are h22's too.
func TestH25S5DefaultConfigurationIsH22EndToEnd(t *testing.T) {
	golden := loadS5Golden(t)
	cases := s5Cases()
	for _, profile := range s5Profiles() {
		want := golden.Profiles[profile.Name]
		if len(want) != len(cases) {
			t.Fatalf("%s: %d cases, golden %d", profile.Name, len(cases), len(want))
		}
		for _, mode := range s5Modes() {
			applies := &s5Applies{}
			turns := 0
			for _, c := range cases {
				recorded, ok := want[c.Name]
				if !ok || len(recorded) != len(c.Turns) {
					t.Fatalf("%s/%s: no golden", profile.Name, c.Name)
				}
				s5ResetCaches()
				got := s5RunCase(t, profile, c, mode.overlay)
				for index, turn := range c.Turns {
					label := profile.Name + "/" + mode.name + "/" + c.Name + " turn " + string(rune('1'+index))
					s5Compare(t, label, profile, mode, turn, got[index], recorded[index], applies)
					turns++
				}
			}
			t.Logf("%s/%s: %d turns, %d equal to h22, E1=%d E2=%d E3=%d (coerced %d) E4=%d E5=%d, failures compared=%d",
				profile.Name, mode.name, turns, applies.equal, applies.e1, applies.e2, applies.e3, applies.e3coerce, applies.e4, applies.e5, applies.failures)
			// The case list keeps covering every exception it was built for.
			if mode.hide && (applies.e1 < 6 || applies.e4 < 3) || !mode.hide && (applies.e1 != 0 || applies.e4 != 0) {
				t.Errorf("%s/%s: E1/E4 coverage %d/%d", profile.Name, mode.name, applies.e1, applies.e4)
			}
			if applies.e2 < 2 || applies.e3 < 2 || mode.coerce && applies.e3coerce < 1 || !mode.coerce && applies.e3coerce != 0 {
				t.Errorf("%s/%s: E2/E3 coverage %d/%d/%d", profile.Name, mode.name, applies.e2, applies.e3, applies.e3coerce)
			}
			if mode.hint && applies.e5 < 4 || !mode.hint && applies.e5 != 0 {
				t.Errorf("%s/%s: E5 coverage %d", profile.Name, mode.name, applies.e5)
			}
			if applies.equal < 70 || applies.failures < 15 {
				t.Errorf("%s/%s: only %d turns equal and %d failures compared", profile.Name, mode.name, applies.equal, applies.failures)
			}
		}
	}
}
