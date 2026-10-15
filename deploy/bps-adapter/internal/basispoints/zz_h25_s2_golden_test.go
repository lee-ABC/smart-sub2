package basispoints

// H25 S2: the default configuration against h22's own outputs.
//
// testdata/h22_s2_golden.json was recorded in the h22-live tree by
// testdata/h22_s2_golden_gen.go.txt from the fixtures in
// zz_h25_s2_fixtures_test.go (compiled there unchanged): upstream bodies of
// Lite requests, what the stream relay sent the client and how it closed,
// and whole-response conversions.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type s2GoldenPrompt struct {
	Body              string `json:"body"`
	PlainBody         string `json:"plain_body"`
	Instructions      string `json:"instructions"`
	PlainInstructions string `json:"plain_instructions"`
}

type s2GoldenStream struct {
	Events   []string `json:"events"`
	CloseErr string   `json:"close_err"`
}

type s2GoldenTransform struct {
	Body    string `json:"body"`
	Status  int    `json:"status"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type s2Golden struct {
	Generator  string                       `json:"generator"`
	Prompts    map[string]s2GoldenPrompt    `json:"prompts"`
	Streams    map[string]s2GoldenStream    `json:"streams"`
	Transforms map[string]s2GoldenTransform `json:"transforms"`
}

func loadS2Golden(t *testing.T) s2Golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/h22_s2_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden s2Golden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(golden.Generator, "h22-live 0.1.2-h22") || len(golden.Prompts) == 0 || len(golden.Streams) == 0 || len(golden.Transforms) == 0 {
		t.Fatalf("golden is not h22's: %q", golden.Generator)
	}
	return golden
}

// s2ExplicitDefaults writes every switch out at its default value, which must
// behave exactly as leaving the lines out (I3 also holds for an explicit
// relay_protocol: legacy).
func s2ExplicitDefaults(cfg *Config) {
	cfg.RelayProtocol = relayProtocolLegacy
	cfg.RelayToolContracts = relayToolContractsSummary
	cfg.RelayToolDelivery = relayToolDeliveryImmediate
	cfg.ToolPolicyChecks = toolPolicyChecksLog
	cfg.HistoryConflict = historyConflictReplay
	cfg.RelayIncomplete = relayIncompleteError
}

// s2JSONText is text as it appears inside a JSON body.
func s2JSONText(text string) string {
	encoded := string(jsonBytes(text))
	return encoded[1 : len(encoded)-1]
}

func TestH25S2DefaultLitePrologueIsH22Bytes(t *testing.T) {
	golden := loadS2Golden(t)
	fixtures := s2PromptFixtures()
	if len(fixtures) != len(golden.Prompts) {
		t.Fatalf("fixtures %d, golden %d", len(fixtures), len(golden.Prompts))
	}
	for _, configure := range []func(*Config){nil, s2ExplicitDefaults} {
		for _, fixture := range fixtures {
			want, ok := golden.Prompts[fixture.Name]
			if !ok {
				t.Fatalf("%s: no golden", fixture.Name)
			}
			cfg := defaultConfig()
			if configure != nil {
				configure(&cfg)
			}
			body, err := prepareResponsesBody(cloneObject(fixture.Source), cfg)
			if err != nil {
				t.Fatal(fixture.Name, err)
			}
			plainCfg := cfg
			plainCfg.ClientIdentity.PlainPrologue = true
			source := cloneObject(fixture.Source)
			plainBody, err := prepareResponsesBodyFor(source, plainCfg, conversationScope(source), persona{}, identityHygiene)
			if err != nil {
				t.Fatal(fixture.Name, err)
			}
			wantBody, wantPlain := want.Body, want.PlainBody
			wantInstr, wantPlainInstr := want.Instructions, want.PlainInstructions
			if s2E2Fixtures[fixture.Name] {
				// E2, and nothing else: h22's no-tools sentence becomes the
				// tool_search_output one, in the same place.
				if wantInstr != relayNoToolsHint || wantPlainInstr != plainNoToolsHint ||
					strings.Count(wantBody, s2JSONText(relayNoToolsHint)) != 1 || strings.Count(wantPlain, s2JSONText(plainNoToolsHint)) != 1 {
					t.Fatalf("%s: h22 did not answer this with its no-tools text", fixture.Name)
				}
				wantBody = strings.Replace(wantBody, s2JSONText(relayNoToolsHint), s2JSONText(relaySearchedTools), 1)
				wantPlain = strings.Replace(wantPlain, s2JSONText(plainNoToolsHint), s2JSONText(plainSearchedTools), 1)
				wantInstr, wantPlainInstr = relaySearchedTools, plainSearchedTools
			}
			if got := string(jsonBytes(body)); got != wantBody {
				t.Errorf("%s: upstream body differs from h22\n got: %s\nwant: %s", fixture.Name, got, wantBody)
			}
			if got := string(jsonBytes(plainBody)); got != wantPlain {
				t.Errorf("%s: plain_prologue body differs from h22\n got: %s\nwant: %s", fixture.Name, got, wantPlain)
			}
			frozen := withRelayOptions(cloneObject(fixture.Source), relayOptionsFromConfig(cfg))
			if got := clientToolProtocolInstructionsFor(frozen, false); got != wantInstr {
				t.Errorf("%s: instructions %q, want %q", fixture.Name, got, wantInstr)
			}
			if got := clientToolProtocolInstructionsFor(frozen, true); got != wantPlainInstr {
				t.Errorf("%s: plain instructions %q, want %q", fixture.Name, got, wantPlainInstr)
			}
		}
	}
}

// s2Failure classifies a close message: "" (success), "invalid_tool_call",
// or the whole message for any other failure. Wording may differ (I2), the
// class may not; relay_incomplete: error keeps h22's invalid_tool_call.
func s2Failure(closeErr string) string {
	closeErr = strings.TrimSuffix(closeErr, retryableErrorHint)
	switch {
	case closeErr == "":
		return ""
	case strings.Contains(closeErr, "invalid_tool_call"):
		return "invalid_tool_call"
	default:
		return closeErr
	}
}

// s2ZeroOutput: what h22 sent the client before failing was at most
// response.created / response.in_progress.
func s2ZeroOutput(events []string) bool {
	for _, event := range events {
		name := strings.SplitN(event, "\x00", 2)[0]
		if name != "response.created" && name != "response.in_progress" {
			return false
		}
	}
	return true
}

// s2E3Streams are the fixtures h22 rejected and h25 converts (E3). Each lists
// what the client must now receive.
var s2E3Streams = map[string]struct{ arguments, id string }{
	"e3_equivalent_number":   {arguments: `{"n":1e3}`, id: "fc_num"},
	"e3_terminal_missing_id": {arguments: `{"city":"Paris"}`, id: "fc_paris"},
	"e3_scalar_string":       {arguments: `{"limit":20,"path":"/tmp"}`, id: "fc_coerce"},
}

func TestH25S2DefaultStreamIsH22(t *testing.T) {
	golden := loadS2Golden(t)
	fixtures := s2StreamFixtures()
	if len(fixtures) != len(golden.Streams) {
		t.Fatalf("fixtures %d, golden %d", len(fixtures), len(golden.Streams))
	}
	for _, mode := range []struct {
		name      string
		configure func(*Config)
	}{{"default", nil}, {"explicit", s2ExplicitDefaults}} {
		for _, fixture := range fixtures {
			fixture := fixture
			want, ok := golden.Streams[fixture.Name]
			if !ok {
				t.Fatalf("%s: no golden", fixture.Name)
			}
			t.Run(mode.name+"/"+fixture.Name, func(t *testing.T) {
				result := runStreamTestWith(t, mode.configure, cloneObject(fixture.Request), fixture.Chunks)
				got := s2NormalizeStream(result.joined())
				if e3, ok := s2E3Streams[fixture.Name]; ok {
					if s2Failure(want.CloseErr) != "invalid_tool_call" {
						t.Fatalf("not an E3 case: h22 closed with %q", want.CloseErr)
					}
					if result.closeErr != "" {
						t.Fatalf("E3 conversion failed: %q", result.closeErr)
					}
					s2AssertOneDeliveredCall(t, result.joined(), e3.arguments, e3.id)
					return
				}
				if strings.Join(got, "\n") != strings.Join(want.Events, "\n") {
					t.Fatalf("client events differ from h22\n got: %q\nwant: %q", got, want.Events)
				}
				if s2Failure(result.closeErr) != s2Failure(want.CloseErr) {
					t.Fatalf("close %q, h22 closed with %q", result.closeErr, want.CloseErr)
				}
				// E5: the retry hint exactly on failures before any output.
				hinted := strings.HasSuffix(result.closeErr, retryableErrorHint)
				if wantHint := want.CloseErr != "" && s2ZeroOutput(want.Events); hinted != wantHint {
					t.Fatalf("retry hint %v, want %v (close %q)", hinted, wantHint, result.closeErr)
				}
				if strings.Contains(result.closeErr, "[request_error]") {
					t.Fatalf("tool failure still reads as a client error: %q", result.closeErr)
				}
			})
		}
	}
}

// s2AssertOneDeliveredCall: exactly one client call, with these argument
// bytes and item id, in its lifecycle and in the terminal snapshot.
func s2AssertOneDeliveredCall(t *testing.T, text, arguments, id string) {
	t.Helper()
	done, terminal := 0, 0
	for _, event := range h23Events(t, text) {
		switch event["type"] {
		case "response.output_item.done":
			item := objectValue(event["item"])
			if item["arguments"] != arguments || item["id"] != id || item["name"] == transportName {
				t.Fatalf("delivered call %v", item)
			}
			done++
		case "response.completed":
			output, _ := objectValue(event["response"])["output"].([]any)
			for _, value := range output {
				item := objectValue(value)
				if isStreamToolItem(item) {
					if item["arguments"] != arguments || item["id"] != id {
						t.Fatalf("terminal call %v", item)
					}
					terminal++
				}
			}
		}
	}
	if done != 1 || terminal != 1 || strings.Contains(text, "run_officejs") {
		t.Fatalf("done=%d terminal=%d", done, terminal)
	}
}

func TestH25S2DefaultTransformIsH22(t *testing.T) {
	golden := loadS2Golden(t)
	fixtures := s2TransformFixtures()
	if len(fixtures) != len(golden.Transforms) {
		t.Fatalf("fixtures %d, golden %d", len(fixtures), len(golden.Transforms))
	}
	for _, configure := range []func(*Config){nil, s2ExplicitDefaults} {
		for _, fixture := range fixtures {
			want := golden.Transforms[fixture.Name]
			source := s2Source(cloneObject(fixture.Source), configure)
			body, _, _, err := transformResponseBody(jsonBytes(fixture.Response), source)
			if want.Kind == "" {
				if err != nil || string(body) != want.Body {
					t.Fatalf("%s: body differs from h22 (%v)\n got: %s\nwant: %s", fixture.Name, err, body, want.Body)
				}
				continue
			}
			api, ok := err.(interface {
				StatusCode() int
				Code() string
			})
			if !ok || api.StatusCode() != want.Status || api.Code() != want.Kind || body != nil {
				t.Fatalf("%s: error %v, h22 %d %s", fixture.Name, err, want.Status, want.Kind)
			}
		}
	}
}
