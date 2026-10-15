package basispoints

// H25 S1: the default configuration against h22's own output.
//
// testdata/h22_s1_golden.json was recorded by running
// testdata/h22_s1_golden_gen.go.txt (as a _test.go file) together with
// zz_h25_s1_fixtures_test.go inside a checkout of the h22-live branch:
//
//	git archive h22-live | tar -x -C /tmp/h22
//	cp zz_h25_s1_fixtures_test.go /tmp/h22/internal/basispoints/
//	cp testdata/h22_s1_golden_gen.go.txt /tmp/h22/internal/basispoints/zz_s1_golden_gen_test.go
//	S1_GOLDEN_OUT=$PWD/testdata/h22_s1_golden.json go test -C /tmp/h22 ./internal/basispoints/ -run TestGenerateS1Golden
//
// Every value in it is h22's output. Where h22 itself was nondeterministic
// (the order of per-custom-tool reminder sentences) the generator kept the
// genuine h22 output whose order is sorted, which is the order h25 always
// produces.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type s1H22Prompt struct {
	Body            string `json:"body"`
	PlainBody       string `json:"plain_body"`
	Instructions    string `json:"instructions"`
	PlainInstr      string `json:"plain_instructions"`
	Reminder        string `json:"reminder"`
	ReminderPresent bool   `json:"reminder_present"`
}

type s1H22Decode struct {
	OK   bool   `json:"ok"`
	Call string `json:"call"`
}

type s1H22Golden struct {
	Generator          string                 `json:"generator"`
	Prompts            map[string]s1H22Prompt `json:"prompts"`
	TwoCustomReminders []string               `json:"two_custom_reminders"`
	Decodes            map[string]s1H22Decode `json:"decodes"`
}

func loadS1H22Golden(t *testing.T) s1H22Golden {
	t.Helper()
	data, err := os.ReadFile("testdata/h22_s1_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden s1H22Golden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(golden.Generator, "h22-live 0.1.2-h22") {
		t.Fatalf("golden was not recorded from h22: %q", golden.Generator)
	}
	if len(golden.Prompts) != len(s1PromptFixtures()) || len(golden.Decodes) != len(s1DecodeFixtures()) {
		t.Fatalf("golden is stale: %d prompts, %d decodes", len(golden.Prompts), len(golden.Decodes))
	}
	return golden
}

// I1 for the relay prologue: with no new key in config.yaml, and with every
// switch written out explicitly at its default, the whole upstream body --
// instructions, catalogue, reminder, history, metadata -- is h22's, byte for
// byte, in both prologue wordings.
func TestH25S1DefaultRelayPromptIsH22Bytes(t *testing.T) {
	golden := loadS1H22Golden(t)
	explicit := defaultConfig()
	explicit.RelayProtocol = relayProtocolLegacy
	explicit.RelayToolContracts = relayToolContractsSummary
	for _, fixture := range s1PromptFixtures() {
		want := golden.Prompts[fixture.Name]
		for round := 0; round < 8; round++ {
			for _, cfg := range []Config{defaultConfig(), explicit} {
				body, err := prepareResponsesBody(cloneObject(fixture.Source), cfg)
				if err != nil {
					t.Fatalf("%s: %v", fixture.Name, err)
				}
				if got := string(jsonBytes(body)); got != want.Body {
					t.Fatalf("%s: upstream body differs from h22\n got: %s\nwant: %s", fixture.Name, got, want.Body)
				}
				plainCfg := cfg
				plainCfg.ClientIdentity.PlainPrologue = true
				source := cloneObject(fixture.Source)
				plain, err := prepareResponsesBodyFor(source, plainCfg, conversationScope(source), persona{}, identityHygiene)
				if err != nil {
					t.Fatalf("%s: %v", fixture.Name, err)
				}
				if got := string(jsonBytes(plain)); got != want.PlainBody {
					t.Fatalf("%s: plain-prologue body differs from h22\n got: %s\nwant: %s", fixture.Name, got, want.PlainBody)
				}
			}
			if got := clientToolProtocolInstructionsFor(fixture.Source, false); got != want.Instructions {
				t.Fatalf("%s: relay instructions differ from h22\n got: %s\nwant: %s", fixture.Name, got, want.Instructions)
			}
			if got := clientToolProtocolInstructionsFor(fixture.Source, true); got != want.PlainInstr {
				t.Fatalf("%s: plain instructions differ from h22", fixture.Name)
			}
			if got := clientToolProtocolReminder(fixture.Source); got != want.Reminder {
				t.Fatalf("%s: reminder differs from h22\n got: %s\nwant: %s", fixture.Name, got, want.Reminder)
			}
		}
		for _, banned := range []string{"Tool choice", "At most one client", "The client also declares native tools", "Full tool key:"} {
			if strings.Contains(want.Body, banned) {
				t.Fatalf("%s: fixture golden unexpectedly contains rc2 text %q", fixture.Name, banned)
			}
		}
	}
}

// h22 returned one of several reminder orders at random once there were two
// custom tools; h25 always returns the sorted one, which is one of them.
func TestH25S1CustomToolReminderIsStableAndOneOfH22s(t *testing.T) {
	golden := loadS1H22Golden(t)
	if len(golden.TwoCustomReminders) != 2 {
		t.Fatalf("expected h22 to show both orders, got %d", len(golden.TwoCustomReminders))
	}
	first := clientToolProtocolReminder(s1TwoCustomSource())
	allowed := false
	for _, text := range golden.TwoCustomReminders {
		allowed = allowed || text == first
	}
	if !allowed {
		t.Fatalf("reminder is not one of h22's outputs: %s", first)
	}
	if strings.Index(first, "Custom tool alpha_patch") > strings.Index(first, "Custom tool zeta_patch") {
		t.Fatal("custom-tool sentences are not in sorted order")
	}
	for i := 0; i < 200; i++ {
		if clientToolProtocolReminder(s1TwoCustomSource()) != first {
			t.Fatal("reminder order is not deterministic")
		}
	}
}

// s1CoercionFixture names the decode fixtures whose only h22 failure is a
// number/boolean written as a string where list_files is callable (E3).
func s1CoercionFixture(name string) bool {
	return strings.HasPrefix(name, "multi/string_integer/") || strings.HasPrefix(name, "multi/string_bool_number/")
}

var s1CoercedArguments = map[string]string{
	"string_integer":     `{"limit":20,"path":"/"}`,
	"string_bool_number": `{"path":"/","ratio":0.5,"recursive":true,"tags":[1,2]}`,
}

// I2/I3 for decoding: under the default switches, an explicit legacy
// selection, and legacy with coerce_scalar_strings off, every one of the 1056
// transport calls -- 22 code shapes x 12 references shapes x 4 catalogues --
// decodes exactly as h22 decoded it. references is never read in legacy. The
// only difference allowed is E3: the 24 calls that failed in h22 only because
// "20"/"true" was written for a number/boolean, which now succeed with the
// converted arguments.
func TestH25S1LegacyDecodeIsH22(t *testing.T) {
	golden := loadS1H22Golden(t)
	sources := s1DecodeSources()
	noCoercion := defaultRelayOptions()
	noCoercion.CoerceScalarStrings = false
	explicitLegacy := defaultRelayOptions()
	explicitLegacy.Protocol = relayProtocolLegacy
	contexts := map[string]func(map[string]any) map[string]any{
		"default":         func(s map[string]any) map[string]any { return s },
		"frozen_legacy":   func(s map[string]any) map[string]any { return withRelayProtocolSelection(s, relayProtocolLegacy) },
		"explicit_legacy": func(s map[string]any) map[string]any { return withRelayOptions(s, explicitLegacy) },
		"no_coercion":     func(s map[string]any) map[string]any { return withRelayOptions(s, noCoercion) },
		// A prepared request: frozen switches plus the selection preparation made.
		"service_shape": func(s map[string]any) map[string]any {
			return withRelayProtocolSelection(withRelayOptions(s, defaultRelayOptions()), "")
		},
	}
	coerced := 0
	for _, fixture := range s1DecodeFixtures() {
		want := golden.Decodes[fixture.Name]
		for contextName, wrap := range contexts {
			call, reason := decodeTransportItemWithReason(cloneObject(fixture.Native), wrap(sources[fixture.Source]))
			got := s1H22Decode{OK: reason == ""}
			if got.OK {
				got.Call = string(jsonBytes(call))
			}
			if s1CoercionFixture(fixture.Name) && contextName != "no_coercion" {
				if want.OK || !got.OK {
					t.Fatalf("%s/%s: E3 coercion fixture: h22 ok=%v, h25 reason=%q", fixture.Name, contextName, want.OK, reason)
				}
				code := strings.Split(fixture.Name, "/")[1]
				if call["name"] != "list_files" || call["arguments"] != s1CoercedArguments[code] {
					t.Fatalf("%s/%s: coerced call = %s", fixture.Name, contextName, got.Call)
				}
				coerced++
				continue
			}
			if got != want {
				t.Fatalf("%s/%s: decode differs from h22\n got: %+v (reason %q)\nwant: %+v", fixture.Name, contextName, got, reason, want)
			}
		}
	}
	if coerced != 24*4 {
		t.Fatalf("E3 fixtures exercised %d times, want 96", coerced)
	}
}

// I3 under the opt-in protocols: whatever references holds, every call h22
// delivered is still delivered, identically, when the conversation is frozen
// to references or chosen by auto. (references mode reads code first.)
func TestH25S1ReferencesModeNeverLosesAnH22Delivery(t *testing.T) {
	golden := loadS1H22Golden(t)
	sources := s1DecodeSources()
	for _, fixture := range s1DecodeFixtures() {
		want := golden.Decodes[fixture.Name]
		if !want.OK {
			continue
		}
		for _, mode := range []string{relayProtocolReferences, relayProtocolAuto} {
			source := withRelayProtocolSelection(withRelayOptions(sources[fixture.Source], relayOptions{Protocol: mode, CoerceScalarStrings: true}), mode)
			call, reason := decodeTransportItemWithReason(cloneObject(fixture.Native), source)
			if reason != "" || string(jsonBytes(call)) != want.Call {
				t.Fatalf("%s/%s: h22 delivered %s, h25 gave reason=%q call=%s", fixture.Name, mode, want.Call, reason, jsonBytes(call))
			}
		}
	}
}

// No mode ever hands a routing envelope to a client tool as its payload.
func TestH25S1NoModeDeliversAnEnvelopeAsPayload(t *testing.T) {
	sources := s1DecodeSources()
	for _, fixture := range s1DecodeFixtures() {
		for _, mode := range []string{relayProtocolLegacy, relayProtocolReferences, relayProtocolAuto} {
			source := withRelayProtocolSelection(withRelayOptions(sources[fixture.Source], relayOptions{Protocol: mode, CoerceScalarStrings: true}), mode)
			call, reason := decodeTransportItemWithReason(cloneObject(fixture.Native), source)
			if reason != "" {
				continue
			}
			payload := stringValue(call["input"]) + stringValue(call["arguments"])
			for _, marker := range []string{`"tool":`, `"tool" :`, transportName} {
				if strings.Contains(payload, marker) {
					t.Fatalf("%s/%s delivered a routing envelope as payload: %s", fixture.Name, mode, jsonBytes(call))
				}
			}
		}
	}
}
