package basispoints

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Tests from the second GPT-6-astra review of h25 (6e2655b). The review wrote
// them as zz_review2_{e4,schema,replay}_test.go in its own copy; they are
// ported here with the probe that only logged (the E4 declaration-order
// boundary) turned into assertions of the fixed behaviour. The originals stay
// in the review copy and are not checked in.

// ---------------------------------------------------------------------------
// E4: collisions.

// The ordinary bare-name collision: the unqualified declaration counts even
// if its search definitions conflict or tool_choice excludes it.
func TestH25CodexReview2EncryptedCollisionCatalogMatrix(t *testing.T) {
	for _, typ := range []string{"function", "custom"} {
		for _, location := range []string{"top_before", "top_after", "additional", "search", "search_conflict"} {
			t.Run(typ+"/"+location, func(t *testing.T) {
				bare := map[string]any{"type": typ, "name": "send", "parameters": map[string]any{"type": "object"}}
				source := map[string]any{"tools": []any{codexReviewAgents()}}
				switch location {
				case "top_before":
					source["tools"] = []any{bare, codexReviewAgents()}
				case "top_after":
					source["tools"] = []any{codexReviewAgents(), bare}
				case "additional":
					source["input"] = []any{map[string]any{"type": "additional_tools", "tools": []any{bare}}}
				case "search":
					source["input"] = []any{toolSearchOutput("search_send", bare)}
				case "search_conflict":
					other := map[string]any{"type": typ, "name": "send", "parameters": map[string]any{"type": "string"}}
					source["input"] = []any{toolSearchOutput("search_a", bare), toolSearchOutput("search_b", other)}
				}
				source["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "namespace": "agents", "name": "send"}}}
				call, reason := decodeTransportItemWithReason(h21TransportItem("review2_collision", "agents.send", map[string]any{"message": "hello"}), source)
				if reason != "" || call["name"] != "agents.send" || call["namespace"] != nil {
					t.Fatalf("unsafe collision: reason=%q call=%s", reason, jsonBytes(call))
				}
			})
		}
	}
}

// codexReview2Unmarked is agents.send declared again without the encrypted
// mark: the same full key as codexReviewAgents' tool.
func codexReview2Unmarked() map[string]any {
	return map[string]any{"type": "namespace", "name": "agents", "tools": []any{codexReviewFlat("send")}}
}

// codexReview2Place puts the encrypted agents.send (enc) and another
// declaration under the same key (other) where the case says. Top-level
// declarations come first in "tools"; additional_tools and
// tool_search_output are input items before the user message.
func codexReview2Place(enc, other string, otherDecl map[string]any, bareSend bool) map[string]any {
	var tools, input []any
	if bareSend {
		tools = append(tools, codexReviewFlat("send"))
	}
	add := func(where string, decl map[string]any, id string) {
		switch where {
		case "top":
			tools = append(tools, decl)
		case "additional":
			input = append(input, map[string]any{"type": "additional_tools", "tools": []any{decl}})
		case "search":
			input = append(input, toolSearchOutput("search_"+id, decl))
		}
	}
	split := func(where string) (string, bool) {
		if strings.HasSuffix(where, "_before") {
			return strings.TrimSuffix(where, "_before"), true
		}
		return strings.TrimSuffix(where, "_after"), false
	}
	place, first := split(other)
	if first {
		add(place, otherDecl, "other")
		add(enc, codexReviewAgents(), "enc")
	} else {
		add(enc, codexReviewAgents(), "enc")
		add(place, otherDecl, "other")
	}
	source := map[string]any{"model": "gpt-6-astra", "input": append(input, s5User("go"))}
	if tools != nil {
		source["tools"] = tools
	}
	return source
}

// The review's probe (TestReview2EncryptedDoubleCollisionOrderProbe): bare
// send, encrypted agents.send and a flat tool literally named "agents.send".
// Before the fix the flat declaration replaced the encrypted one in the
// catalogue when it came later (in tools or additional_tools), and the call
// was handed back as the flat tool's name. Now all four orders fail alike.
func TestH25CodexReview2EncryptedDoubleCollisionAnyOrder(t *testing.T) {
	call := h21TransportItem("review2_double", "agents.send", map[string]any{"message": "hello"})
	for _, location := range []string{"before", "after", "additional", "search"} {
		t.Run(location, func(t *testing.T) {
			source := map[string]any{"model": "gpt-6-astra", "tools": []any{codexReviewFlat("send"), codexReviewAgents()}, "input": []any{s5User("go")}}
			switch location {
			case "before":
				source["tools"] = []any{codexReviewFlat("send"), codexReviewFlat("agents.send"), codexReviewAgents()}
			case "after":
				source["tools"] = []any{codexReviewFlat("send"), codexReviewAgents(), codexReviewFlat("agents.send")}
			case "additional":
				source["input"] = []any{map[string]any{"type": "additional_tools", "tools": []any{codexReviewFlat("agents.send")}}, s5User("go")}
			case "search":
				source["input"] = []any{toolSearchOutput("search_dot", codexReviewFlat("agents.send")), s5User("go")}
			}
			decoded, reason := decodeTransportItemWithReason(call, source)
			if reason != "encrypted_tool_blocked" || decoded != nil {
				t.Fatalf("declaration %s: reason=%q call=%s", location, reason, jsonBytes(decoded))
			}
			codexReview2AssertStreamRefuses(t, source, call)
			// The decoder now stops such a call before naming it; the fallback
			// name itself still never returns a declared flat name.
			encrypted := toolSpec{Key: "agents.send", Name: "send", Namespace: "agents", Type: "function", Spec: codexReviewAgents()["tools"].([]any)[0].(map[string]any)}
			if name, collision, ok := blockedRelayCallName(source, encrypted); ok || !collision {
				t.Fatalf("fallback name %q (collision %v) offered although send and agents.send are both declared", name, collision)
			}
		})
	}
}

// codexReview2AssertStreamRefuses runs call through a stream and checks that
// it fails with encrypted_tool_blocked, that no client tool call reaches the
// client, and that the failure is counted once.
func codexReview2AssertStreamRefuses(t *testing.T, source, call map[string]any) {
	t.Helper()
	before := relayCounterSnapshot()
	result := runStreamTest(t, source, []string{s2Created(), h21ItemEvent("response.output_item.done", 0, call), h21Terminal("response.completed", call)})
	delta := reviewCounterDelta(before, "relay_call:legacy:encrypted_tool_blocked", "encrypted_tool_blocked", "encrypted_tool_blocked_collision")
	joined := result.joined()
	if !strings.Contains(result.closeErr, "reason=encrypted_tool_blocked") || strings.Contains(joined, `"type":"function_call"`) || strings.Contains(joined, `"name":"agents.send"`) || strings.Contains(joined, `"name":"send"`) {
		t.Fatalf("stream: close %q, emitted %s", result.closeErr, joined)
	}
	// A failure is no delivery: the E4 notes are not recorded.
	if delta["relay_call:legacy:encrypted_tool_blocked"] != 1 || delta["encrypted_tool_blocked"] != 0 || delta["encrypted_tool_blocked_collision"] != 0 {
		t.Fatalf("counters %v", delta)
	}
}

// The general form: whenever the request declares agents.send both as the
// encrypted namespaced tool and as anything else -- a flat function or custom
// tool literally named "agents.send", or the agents namespace again without
// the mark -- in any place and any order, and with or without a bare send, a
// run_officejs call to agents.send fails as encrypted_tool_blocked, and E1
// hides agents.send from the relay catalogue. Off, the switch changes nothing
// (h22: the catalogue's last declaration is delivered), and a call that does
// not come through run_officejs is never touched.
func TestH25CodexReview2EncryptedKeyClaimedTwiceFailsInAnyOrder(t *testing.T) {
	others := map[string]map[string]any{
		"flat_function": codexReviewFlat("agents.send"),
		"flat_custom":   {"type": "custom", "name": "agents.send"},
		"unmarked_ns":   codexReview2Unmarked(),
	}
	placements := [][2]string{
		{"top", "top_before"}, {"top", "top_after"}, {"top", "additional_after"}, {"top", "search_after"},
		{"additional", "top_after"}, {"additional", "additional_before"}, {"additional", "additional_after"},
		{"search", "top_after"}, {"search", "additional_after"},
	}
	relayCall := h21TransportItem("review2_claims", "agents.send", map[string]any{"message": "hello"})
	direct := map[string]any{"type": "function_call", "id": "fc_review2_direct", "call_id": "call_review2_direct", "name": "agents.send", "status": "completed", "arguments": `{"message":"hello"}`}
	off := defaultRelayOptions()
	off.HideEncryptedTools = false
	prompts := map[string]string{}
	for otherName, otherDecl := range others {
		for _, place := range placements {
			for _, bareSend := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s+%s/bare=%v", otherName, place[0], place[1], bareSend)
				t.Run(name, func(t *testing.T) {
					source := codexReview2Place(place[0], place[1], otherDecl, bareSend)
					decoded, reason := decodeTransportItemWithReason(relayCall, source)
					if reason != "encrypted_tool_blocked" || decoded != nil {
						t.Fatalf("relay call: reason=%q call=%s", reason, jsonBytes(decoded))
					}
					codexReview2AssertStreamRefuses(t, source, relayCall)

					// E1: a relay catalogue (summary: h22's; full: rc2's) that
					// had agents.send before E1 no longer lists it and names it
					// as hidden, whichever declaration the catalogue kept.
					for catalogueName, c := range map[string]struct {
						build  func(map[string]any) (map[string]toolSpec, map[string]bool)
						before map[string]toolSpec
					}{
						"summary": {summaryRelayCatalogue, clientToolSpecs(source)},
						"full":    {fullRelayCatalogue, advertisedRelayToolSpecs(source)},
					} {
						specs, hidden := c.build(source)
						if _, listed := specs["agents.send"]; listed {
							t.Fatalf("%s catalogue still lists agents.send", catalogueName)
						}
						if _, had := c.before["agents.send"]; had && !hidden["agents.send"] {
							t.Fatalf("%s catalogue dropped agents.send without hiding it: %v", catalogueName, hidden)
						}
					}
					prompt := clientToolProtocolInstructions(source)
					if strings.Contains(prompt, "- agents.send (") {
						t.Fatal("prompt lists agents.send")
					}
					if _, top := clientToolSpecs(source)["agents.send"]; top && !strings.Contains(prompt, "so do not call them: agents.send.") {
						t.Fatalf("prompt does not name agents.send as unavailable")
					}
					if place[0] == "top" && (place[1] == "top_before" || place[1] == "top_after") {
						key := fmt.Sprintf("%s/bare=%v", otherName, bareSend)
						if previous, seen := prompts[key]; seen && previous != prompt {
							t.Fatalf("prompt depends on the declaration order")
						}
						prompts[key] = prompt
					}

					// A direct call (not through run_officejs) is never touched,
					// and with the switch off nothing is: both are h22's.
					offSource := withRelayOptions(source, off)
					onDirect, onDirectReason := decodeTransportItemWithReason(direct, source)
					offDirect, offDirectReason := decodeTransportItemWithReason(direct, offSource)
					if onDirectReason != offDirectReason || string(jsonBytes(onDirect)) != string(jsonBytes(offDirect)) {
						t.Fatalf("direct call changed: on %q %s, off %q %s", onDirectReason, jsonBytes(onDirect), offDirectReason, jsonBytes(offDirect))
					}
					offCall, offReason := decodeTransportItemWithReason(relayCall, offSource)
					spec, declared := relayOutputToolSpecs(offSource)["agents.send"]
					if !declared {
						if offReason != "tool_not_in_catalog" {
							t.Fatalf("off: reason %q for an undeclared key", offReason)
						}
						return
					}
					if offReason != "" || offCall["name"] != spec.Name || stringValue(offCall["namespace"]) != spec.Namespace {
						t.Fatalf("off: reason %q call %s, want the catalogue's %s", offReason, jsonBytes(offCall), spec.Key)
					}
					if strings.Contains(clientToolProtocolInstructions(offSource), "do not call them") {
						t.Fatal("off: E1 sentence present")
					}
				})
			}
		}
	}
}

// Over-blocking guard: the same encrypted tool declared twice (identically, or
// differing only in description) is not ambiguous; it is blocked as before,
// handed back under its bare name. Other tools of an ambiguous request, and an
// unmarked tool of the same namespace, are delivered exactly as before.
func TestH25CodexReview2EncryptedKeyClaimsDoNotOverBlock(t *testing.T) {
	call := h21TransportItem("review2_same", "agents.send", map[string]any{"message": "hello"})
	reworded := codexReviewAgents()
	member := cloneObject(reworded["tools"].([]any)[0].(map[string]any))
	member["description"] = "Send a message (found by tool search)."
	reworded["tools"] = []any{member}
	for name, source := range map[string]map[string]any{
		"top_and_additional": {"model": "gpt-6-astra", "tools": []any{codexReviewAgents()}, "input": []any{map[string]any{"type": "additional_tools", "tools": []any{codexReviewAgents()}}, s5User("go")}},
		"top_twice":          {"model": "gpt-6-astra", "tools": []any{codexReviewAgents(), codexReviewAgents()}, "input": []any{s5User("go")}},
		"top_and_search":     {"model": "gpt-6-astra", "tools": []any{codexReviewAgents()}, "input": []any{toolSearchOutput("search_same", reworded), s5User("go")}},
		"additional_twice":   {"model": "gpt-6-astra", "input": []any{map[string]any{"type": "additional_tools", "tools": []any{codexReviewAgents()}}, map[string]any{"type": "additional_tools", "tools": []any{reworded}}, s5User("go")}},
	} {
		t.Run(name, func(t *testing.T) {
			decoded, reason := decodeTransportItemWithReason(call, source)
			if reason != "" || decoded["name"] != "send" || decoded["namespace"] != nil {
				t.Fatalf("decode %q: %s", reason, jsonBytes(decoded))
			}
		})
	}
	// An ambiguous agents.send leaves the rest of the request alone.
	namespace := map[string]any{"type": "namespace", "name": "agents", "tools": []any{
		codexReviewAgents()["tools"].([]any)[0],
		map[string]any{"type": "function", "name": "list", "parameters": map[string]any{"type": "object"}},
	}}
	source := map[string]any{"model": "gpt-6-astra", "tools": []any{codexReviewFlat("send"), namespace, codexReviewFlat("agents.send")}, "input": []any{s5User("go")}}
	if _, reason := decodeTransportItemWithReason(call, source); reason != "encrypted_tool_blocked" {
		t.Fatalf("ambiguous agents.send: %q", reason)
	}
	for tool, want := range map[string][2]string{"send": {"send", ""}, "agents.list": {"list", "agents"}} {
		decoded, reason := decodeTransportItemWithReason(h21TransportItem("review2_other_"+want[0], tool, map[string]any{}), source)
		if reason != "" || decoded["name"] != want[0] || stringValue(decoded["namespace"]) != want[1] {
			t.Fatalf("%s: decode %q %s", tool, reason, jsonBytes(decoded))
		}
	}
	prompt := clientToolProtocolInstructions(source)
	if !strings.Contains(prompt, "- agents.list (function)") || !strings.Contains(prompt, "- send (function)") || !strings.Contains(prompt, "so do not call them: agents.send.") {
		t.Fatalf("prompt lost the unambiguous tools or the E1 sentence")
	}
}

// ---------------------------------------------------------------------------
// E3: the acceptance boundary. No schema gains acceptance merely because an
// unsupported assertion or an exhausted budget hid h22's contradiction. ($ref
// is TestH25CodexReviewUnknownIsNotAcceptance.)

func TestH25CodexReview2SchemaUnknownCannotRescueH22Rejection(t *testing.T) {
	for _, tc := range []struct{ name, value, schema string }{
		{"unsupported_keyword", `{"p":1}`, `{"type":"object","properties":{},"patternProperties":{"^p$":{"type":"integer"}},"additionalProperties":false,"futureAssertion":true}`},
		{"unsupported_pattern", `{"p":1}`, `{"type":"object","properties":{},"patternProperties":{"(?=p)p":{"type":"integer"}},"additionalProperties":false}`},
		{"malformed_pattern_catalog", `{"p":1}`, `{"type":"object","properties":{},"patternProperties":[],"additionalProperties":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, schema := parseArguments(tc.value), parseArguments(tc.schema)
			if h22SchemaMatches(args, schema) || checkSchemaCompatibility(args, schema) != schemaUnknown {
				t.Fatal("fixture must be an h22 rejection and rc2 unknown")
			}
			if rc2SchemaProves(args, schema) || relaySchemaAccepts(args, schema) {
				t.Fatal("unknown counted as proof")
			}
			source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "f", "parameters": schema}}}
			if _, reason := decodeTransportItemWithReason(h21TransportItem("review2", "f", args), source); reason != "schema_mismatch" {
				t.Fatalf("decode reason = %q", reason)
			}
		})
	}
}

func TestH25CodexReview2SchemaUnknownAfterCoercionCannotRescue(t *testing.T) {
	// a is repairable, but z remains a genuine undeclared property that rc2
	// cannot settle (unsupported pattern). The converted object must not be
	// accepted through the old `schemaMatches != mismatch` shortcut.
	schema := parseArguments(`{"type":"object","properties":{"a":{"type":"integer"}},"patternProperties":{"(?=z)z":{"type":"integer"}},"additionalProperties":false}`)
	args := parseArguments(`{"a":"2","z":1}`)
	coerced, changed := coerceScalarStrings(args, schema)
	if !changed || h22SchemaMatches(coerced, schema) || checkSchemaCompatibility(coerced, schema) != schemaUnknown {
		t.Fatal("fixture did not reach the post-coercion unknown boundary")
	}
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "f", "parameters": schema}}}
	if _, reason := decodeTransportItemWithReason(h21TransportItem("review2", "f", args), source); reason != "schema_mismatch" {
		t.Fatalf("post-coercion reason = %q", reason)
	}
	if got := args["a"]; got != "2" {
		t.Fatalf("coercion mutated original: %v", got)
	}
}

func TestH25CodexReview2SchemaProofKeepsKnownCorrections(t *testing.T) {
	args := parseArguments(`{"n":1e3}`)
	schema := parseArguments(`{"type":"object","properties":{},"patternProperties":{"^n$":{"type":"integer","enum":[1000]}},"additionalProperties":false}`)
	if h22SchemaMatches(args, schema) || !rc2SchemaProves(args, schema) {
		t.Fatal("fixture must be a known rc2-only correction")
	}
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "f", "parameters": schema}}}
	call, reason := decodeTransportItemWithReason(h21TransportItem("review2", "f", args), source)
	if reason != "" || parseArguments(call["arguments"])["n"] != json.Number("1e3") {
		t.Fatalf("known correction lost: %s / %s", reason, jsonBytes(call))
	}
}

func TestH25CodexReview2SchemaSortedPatternBudgetAndComparison(t *testing.T) {
	// The first lexicographic pattern consumes a tiny artificial budget; the
	// incompatible pattern sorts last. Building the maps in opposite insertion
	// order must not change either the schema or the JSON-comparison result.
	for i := 0; i < 100; i++ {
		patterns, lhs, rhs := map[string]any{}, map[string]any{}, map[string]any{}
		for j := 0; j < 24; j++ {
			k := j
			if i%2 != 0 {
				k = 23 - j
			}
			name := fmt.Sprintf("k%02d", k)
			lhs[name], rhs[name] = true, true
			patterns[fmt.Sprintf("(?:%02d)?", k)] = true
		}
		patterns["z?"] = false
		rhs["k23"] = false
		budget := newSchemaBudget()
		budget.visits = 8
		if got := schemaCheckWithin(map[string]any{"x": true}, map[string]any{"patternProperties": patterns}, 0, budget); got != schemaUnknown {
			t.Fatalf("pattern budget run %d = %v", i, got)
		}
		budget = newSchemaBudget()
		budget.visits = 8
		if got := equivalentJSONValuesWithin(lhs, rhs, 0, budget); got != schemaUnknown {
			t.Fatalf("comparison budget run %d = %v", i, got)
		}
	}
}

func TestH25CodexReview2SchemaH22KnownAcceptancePreserved(t *testing.T) {
	// The proof requirement must not replace the h22 OR branch.
	for _, tc := range []struct{ value, schema string }{
		{`{"n":1.5}`, `{"type":"object","properties":{"n":{"type":"integer"}}}`},
		{`{"n":"1"}`, `{"type":"object","properties":{"n":{"enum":[1]}}}`},
		{`{}`, `{"required":["missing"]}`},
		{`{"n":1}`, `{"type":"object","properties":{"n":false}}`},
		{`{"n":1}`, `{"type":"object","additionalProperties":{"type":"string"}}`},
	} {
		args, schema := parseArguments(tc.value), parseArguments(tc.schema)
		if !h22SchemaMatches(args, schema) || !relaySchemaAccepts(args, schema) {
			t.Fatalf("h22 acceptance lost: %s / %s", tc.value, tc.schema)
		}
		source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "f", "parameters": schema}}}
		call, reason := decodeTransportItemWithReason(h21TransportItem("review2", "f", args), source)
		if reason != "" || stringValue(call["arguments"]) != tc.value {
			t.Fatalf("h22 result altered: %s / %s", reason, jsonBytes(call))
		}
	}
}

// ---------------------------------------------------------------------------
// Relay source markers (records over the per-entry cap).

// Marker reads through actual request preparation, in both history policies
// and every relay protocol. Shrinking only the entry cap exercises the
// production branch without generating a 64 MiB SSE response.
func TestH25CodexReview2ReplayMarkerModeMatrix(t *testing.T) {
	s3ResetCaches()
	defer s3ResetCaches()
	codexReviewLowerEntryLimit(t, 1<<10)
	for _, history := range []string{historyConflictReplay, historyConflictReject} {
		for _, protocol := range []string{relayProtocolLegacy, relayProtocolAuto, relayProtocolReferences} {
			for _, plain := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/plain=%v", history, protocol, plain), func(t *testing.T) {
					cfg := defaultConfig()
					cfg.HistoryConflict, cfg.RelayProtocol, cfg.ClientIdentity.PlainPrologue = history, protocol, plain
					source := weatherRequest()
					source["prompt_cache_key"] = t.Name()
					scope := conversationScope(source)
					source = withReplayScopes(source, scope, "review2-strict")
					source = withRelayOptions(source, relayOptionsFromConfig(cfg))
					client := map[string]any{"type": "function_call", "name": "get_weather", "call_id": "review2_marker", "arguments": `{"city":"Paris"}`}
					native := h24ReferencedCall("review2_marker", []any{"get_weather"}, `{"city":"Paris"}`)
					args := parseArguments(native["arguments"])
					args["summary"] = strings.Repeat("x", 2<<10)
					native["arguments"] = string(jsonBytes(args))
					rememberRelayCallFor(source, native, client)
					if !relayCallMarked(scope, "review2_marker") {
						t.Fatal("oversized delivery did not leave marker")
					}
					source["input"] = []any{client, map[string]any{"type": "custom_tool_call_output", "call_id": "review2_marker", "output": "sunny"}}
					if got := selectRelayProtocol(source, relayProtocolAuto, scope); got != relayProtocolLegacy {
						t.Fatalf("auto trusted a marker as references evidence: %s", got)
					}
					delete(source, "tools")
					prepared, err := prepareResponsesBodyFor(source, cfg, scope, persona{}, identityHygiene)
					if err != nil {
						t.Fatal(err)
					}
					var call, output map[string]any
					for _, raw := range prepared["input"].([]any) {
						item := objectValue(raw)
						if item["call_id"] != "review2_marker" {
							continue
						}
						switch item["type"] {
						case "function_call":
							call = item
						case "function_call_output":
							output = item
						}
					}
					if !reflect.DeepEqual(call, fallbackTransportCallFor(client, plain)) {
						t.Fatalf("history not wrapped as h22 cold fallback: %s", jsonBytes(call))
					}
					if output == nil || output["id"] != functionItemID("review2_marker") || output["output"] != "sunny" {
						t.Fatalf("output not normalized consistently: %s", jsonBytes(output))
					}
				})
			}
		}
	}
}

// Markers follow the replay scope (caller scope in, credential out), are
// replaced by a later comparable delivery, never stand in for a reject-mode
// tombstone, and an oversized conflicting delivery still leaves the tombstone.
func TestH25CodexReview2ReplayMarkerScopeAndRejectReplacement(t *testing.T) {
	s3ResetCaches()
	defer s3ResetCaches()
	codexReviewLowerEntryLimit(t, 1<<10)
	base := weatherRequest()
	base["prompt_cache_key"] = "review2-marked-scope"
	request := ExecutorRequest{AuthID: "credential-A", AuthProvider: Provider, Metadata: map[string]any{"caller_scope": "caller-A"}}
	scopeA, _ := deriveReplayScope(request, base)
	request.AuthID = "credential-B"
	scopeB, _ := deriveReplayScope(request, base)
	request.Metadata["caller_scope"] = "caller-B"
	otherScope, _ := deriveReplayScope(request, base)
	client := replayClientCall("review2_scope", "Paris")
	native := replayNativeCall(client)
	big := cloneObject(native)
	outer := parseArguments(big["arguments"])
	outer["summary"] = strings.Repeat("x", 2<<10)
	big["arguments"] = string(jsonBytes(outer))
	target := replayCacheTarget{scope: scopeA, strict: "strict-A", mode: historyConflictReject}
	storeRelayRecord(target, big, client, "relay")
	if !relayCallMarked(scopeB, "review2_scope") || relayCallMarked(otherScope, "review2_scope") {
		t.Fatal("marker did not respect caller scope / credential independence")
	}
	if _, state := cachedCallForHistory(scopeA, client); state != "miss" {
		t.Fatalf("marker falsely proved historical arguments: %s", state)
	}
	// A marker may be replaced by a comparable delivery; it is not a false
	// reject-mode tombstone, because no first fingerprint was retained.
	storeRelayRecord(target, native, client, "relay")
	if relayCallMarked(scopeA, "review2_scope") || rememberedNativeCall(scopeA, "review2_scope") == nil {
		t.Fatal("small comparable delivery did not replace marker")
	}
	// Conversely, a proven contradiction must tombstone even if the new
	// transport would exceed the entry cap.
	changed := replayClientCall("review2_scope", "Tokyo")
	storeRelayRecord(target, big, changed, "relay")
	entry, found := nativeCallCache.lookup(scopeA, "review2_scope")
	if !found || !entry.conflict || entry.marker || entry.item != nil || entry.client != nil {
		t.Fatalf("oversized conflicting delivery lost tombstone: %#v", entry)
	}
	storeRelayRecord(target, big, client, "relay")
	entry, _ = nativeCallCache.lookup(scopeA, "review2_scope")
	if !entry.conflict || entry.marker {
		t.Fatal("same-scope overwrite erased tombstone")
	}
	target.mode = historyConflictReplay
	storeRelayRecord(target, big, client, "relay")
	if !relayCallMarked(scopeA, "review2_scope") {
		t.Fatal("replay overwrite failed to replace tombstone")
	}
}

// A marker obeys h22's FIFO order and the TTL like any record: lookups do not
// renew it, writes do, a rewrite keeps its queue position, and an expired
// marker is gone and leaves the byte accounting consistent. This is the I1
// boundary of CHANGES-h25 section 2: a marker, too, is forgotten.
func TestH25CodexReview2ReplayMarkerFIFOAndTTL(t *testing.T) {
	p := &replayPool{items: map[string]nativeCallEntry{}, name: "review2", maxEntries: 2, maxBytes: 1024, maxEntry: 128, markOversized: true}
	put := func(id string) {
		p.put(nativeCallCacheKey("review2", id), nativeCallEntry{origin: "relay", item: map[string]any{"arguments": strings.Repeat("a", 256)}}, false)
	}
	put("a")
	put("b")
	entryA, _ := p.lookup("review2", "a")
	if !entryA.marker {
		t.Fatal("oversized record did not become a marker")
	}
	entryA.storedAt = time.Now().Add(-time.Hour)
	p.items[nativeCallCacheKey("review2", "a")] = entryA
	if got, ok := p.lookup("review2", "a"); !ok || !got.storedAt.Equal(entryA.storedAt) {
		t.Fatal("lookup renewed TTL")
	}
	put("a")
	if got, _ := p.lookup("review2", "a"); time.Since(got.storedAt) > time.Minute {
		t.Fatal("write did not renew TTL")
	}
	put("c")
	if _, ok := p.lookup("review2", "a"); ok {
		t.Fatal("rewrite moved marker out of h22 FIFO order")
	}
	if got, ok := p.lookup("review2", "b"); !ok || !got.marker {
		t.Fatal("evicted wrong marker")
	}
	entry, _ := p.lookup("review2", "c")
	entry.storedAt = time.Now().Add(-nativeCallCacheTTL - time.Second)
	p.items[nativeCallCacheKey("review2", "c")] = entry
	if _, ok := p.lookup("review2", "c"); ok {
		t.Fatal("expired marker still visible")
	}
	if sum, order := reviewPoolWeights(p); sum <= 0 || sum > p.maxBytes || len(order) != 1 {
		t.Fatalf("bad accounting %d %v", sum, order)
	}
}
