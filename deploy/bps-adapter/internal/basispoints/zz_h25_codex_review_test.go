package basispoints

// Regression tests for the independent GPT-6-astra (ultra) review of h25.
//
// Each test started as one of the reviewer's zz_independent_* probes, which
// recorded a defect (the probes are kept, unchanged, in the review's own copy
// of the repository). Here every assertion states the corrected behaviour, so
// each test fails again if its fix is taken out.
//
//	1  E3: an exhausted or unknown rc2 check is not an acceptance; decoding
//	   is deterministic (sorted traversal in all three walkers)
//	6  E4: the bare name of a blocked tool never addresses another declared
//	   tool
//	7  zero-output detection: a response.created / response.in_progress
//	   snapshot that already carries output is output
//	8  E1/E4: draft-7 schema-valued dependencies carry the encrypted mark
//	9  relay_stats: an upstream failure terminal is not "succeeded"
//	   (cache)  a relay record too large to keep leaves a provenance marker;
//	   the per-entry limit is 64 MiB
//	   (heartbeat) a batch heartbeat that stops says so

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 1. E3 under an exhausted validation budget.

// codexReviewBudgetFixture: an integer array of n elements and one integer
// written as the string "2". At n = 10000 the arguments are inside h22's
// visit budget but past rc2's (two visits per array element), which is the
// reviewer's fixture.
func codexReviewBudgetFixture(n int) (map[string]any, map[string]any) {
	bulk := make([]any, n)
	for i := range bulk {
		bulk[i] = 1
	}
	props := map[string]any{"bulk": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "target": map[string]any{"type": "integer"}}
	args := map[string]any{"bulk": bulk, "target": "2"}
	source := map[string]any{"tools": []any{map[string]any{"type": "function", "name": "f", "parameters": map[string]any{"type": "object", "properties": props}}}}
	native := map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "c", "arguments": string(jsonBytes(map[string]any{"code": string(jsonBytes(map[string]any{"tool": "f", "args": args}))}))}
	return source, native
}

// codexReviewTarget names what a decoded call carries at "target".
func codexReviewTarget(call map[string]any) string {
	switch value := parseArguments(call["arguments"])["target"].(type) {
	case json.Number:
		return "number:" + value.String()
	case float64:
		return fmt.Sprintf("number:%v", value)
	case string:
		return "string:" + value
	default:
		return fmt.Sprintf("%T", value)
	}
}

// Same immutable relay call, decoded 50 times: one answer, and that answer is
// the converted number -- never the schema-invalid string.
func TestH25CodexReviewCoercionBudgetDeterminism(t *testing.T) {
	source, native := codexReviewBudgetFixture(10000)
	first, reason := decodeTransportItemWithReason(native, source)
	if reason != "" {
		t.Fatalf("decode rejected: %s", reason)
	}
	if got := codexReviewTarget(first); got != "number:2" {
		t.Fatalf("delivered target %s, want number:2", got)
	}
	want := string(jsonBytes(first))
	for i := 0; i < 50; i++ {
		call, reason := decodeTransportItemWithReason(native, source)
		if reason != "" || string(jsonBytes(call)) != want {
			t.Fatalf("decode %d differs: reason=%q target=%s", i, reason, codexReviewTarget(call))
		}
	}
}

// The production streaming path with byte-identical done and terminal
// snapshots: never terminal_args_changed, never a string delivered, and the
// same client bytes on every run.
func TestH25CodexReviewCoercionBudgetStream(t *testing.T) {
	source, native := codexReviewBudgetFixture(10000)
	source["model"], source["input"] = "gpt-6-astra", "go"
	native["id"], native["status"] = "fc_c", "completed"
	chunks := []string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}
	first := ""
	for i := 0; i < 50; i++ {
		result := runStreamTest(t, source, chunks)
		joined := result.joined()
		if result.closeErr != "" {
			t.Fatalf("stream %d failed: %q", i, result.closeErr)
		}
		if strings.Contains(joined, `\"target\":\"2\"`) || !strings.Contains(joined, `\"target\":2}`) {
			t.Fatalf("stream %d did not deliver the converted number", i)
		}
		if i == 0 {
			first = joined
		} else if joined != first {
			t.Fatalf("stream %d differs from stream 0", i)
		}
	}
}

// Around both budgets. Within h22's budget the answer is always the
// converted number; past it h22 itself accepts by exhaustion (in map order:
// randomly), and h25 gives that acceptance deterministically. A string is
// only ever delivered where h22's own check accepted the call.
func TestH25CodexReviewCoercionBudgetBoundaries(t *testing.T) {
	for _, n := range []int{9990, 9997, 9998, 9999, 10000, 10001, 15000, 19990, 19996, 19997, 19998, 19999, 20000, 25000} {
		source, native := codexReviewBudgetFixture(n)
		first, reason := decodeTransportItemWithReason(native, source)
		for i := 0; i < 10; i++ {
			call, again := decodeTransportItemWithReason(native, source)
			if again != reason || string(jsonBytes(call)) != string(jsonBytes(first)) {
				t.Fatalf("n=%d: decode %d differs", n, i)
			}
		}
		inner := parseArguments(transportEnvelope(native)["args"])
		schema := firstMap(objectValue(source["tools"].([]any)[0]), "parameters")
		h22Accepts := h22SchemaMatches(inner, schema)
		got := "rejected:" + reason
		if reason == "" {
			got = codexReviewTarget(first)
		}
		switch {
		case n <= 19990 && got != "number:2":
			t.Errorf("n=%d (inside h22's budget): %s, want number:2", n, got)
		case strings.HasPrefix(got, "string:") && !h22Accepts:
			t.Errorf("n=%d: the string was delivered although h22's check rejects it", n)
		case reason != "" && h22Accepts:
			t.Errorf("n=%d: rejected (%s) although h22's check accepts it", n, reason)
		}
		t.Logf("n=%d: %s (h22 check accepts the original: %v, rc2: %v)", n, got, h22Accepts, checkSchemaCompatibility(inner, schema))
	}
}

// The three walkers themselves give one verdict per input: rc2's check (here
// past its budget, with a mismatch it may or may not reach), the JSON value
// comparison (past a small budget, with one differing key), and the repair.
// In map order the first two answered mismatch or unknown at random.
func TestH25CodexReviewWalkersAreDeterministic(t *testing.T) {
	source, native := codexReviewBudgetFixture(10000)
	args := parseArguments(transportEnvelope(native)["args"])
	schema := firstMap(objectValue(source["tools"].([]any)[0]), "parameters")
	rc2 := checkSchemaCompatibility(args, schema)
	left, right := map[string]any{}, map[string]any{}
	for i := 0; i < 64; i++ {
		key := fmt.Sprintf("k%02d", i)
		left[key], right[key] = json.Number("1"), json.Number("1")
	}
	right["k60"] = json.Number("2")
	compare := func() schemaCompatibility {
		budget := newSchemaBudget()
		budget.visits = 50
		return equivalentJSONValuesWithin(left, right, 0, budget)
	}
	equal := compare()
	coerced, _ := coerceScalarStrings(args, schema)
	repaired := string(jsonBytes(coerced))
	for i := 0; i < 50; i++ {
		if got := checkSchemaCompatibility(args, schema); got != rc2 {
			t.Fatalf("rc2 check: %v then %v", rc2, got)
		}
		if got := compare(); got != equal {
			t.Fatalf("value comparison: %v then %v", equal, got)
		}
		if again, _ := coerceScalarStrings(args, schema); string(jsonBytes(again)) != repaired {
			t.Fatal("repair differs")
		}
	}
	if rc2 != schemaUnknown || equal != schemaUnknown {
		t.Fatalf("sorted walks: rc2 %v, comparison %v; want unknown, unknown", rc2, equal)
	}
}

// rc2's "unknown" (here: a $ref it does not resolve) is not an acceptance.
// h22 rejects "abc" for an integer; before the review this was delivered as
// schema_corrected.
func TestH25CodexReviewUnknownIsNotAcceptance(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"$ref": "#/$defs/n", "type": "integer"}},
		"$defs": map[string]any{"n": map[string]any{"type": "integer"}}}
	source := map[string]any{"model": "gpt-6-astra", "input": "go", "tools": []any{map[string]any{"type": "function", "name": "f", "parameters": schema}}}
	native := h21TransportItem("unknown", "f", map[string]any{"n": "abc"})
	if checkSchemaCompatibility(map[string]any{"n": "abc"}, schema) != schemaUnknown || h22SchemaMatches(map[string]any{"n": "abc"}, schema) {
		t.Fatal("fixture: want rc2 unknown and h22 reject")
	}
	if relaySchemaAccepts(map[string]any{"n": "abc"}, schema) {
		t.Fatal("relaySchemaAccepts took rc2's unknown as acceptance")
	}
	if _, reason := decodeTransportItemWithReason(native, source); reason != "schema_mismatch" {
		t.Fatalf("decode reason %q, want schema_mismatch (h22's verdict)", reason)
	}
	result := runStreamTest(t, source, []string{h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)})
	if !strings.Contains(result.closeErr, "reason=schema_mismatch") || strings.Contains(result.joined(), `\"n\":\"abc\"`) {
		t.Fatalf("stream: close %q", result.closeErr)
	}
	// rc2's definite verdicts still count: its correction of h22's
	// patternProperties rejection stays (E3).
	value := decodeToolJSONObject(`{"S_name":"ok"}`)
	corrected := decodeToolJSONObject(`{"type":"object","properties":{},"patternProperties":{"^S_":{"type":"string"}},"additionalProperties":false}`)
	if h22SchemaMatches(value, corrected) || checkSchemaCompatibility(value, corrected) != schemaCompatible || !relaySchemaAccepts(value, corrected) {
		t.Fatal("patternProperties correction lost")
	}
}

// codexReviewH22Original is h22-live's schemaMatchesWithin verbatim, map
// order and all (protocol.go:1173 at d45804d), kept only to compare h25's
// sorted copy against.
func codexReviewH22Original(value any, schema map[string]any, depth int, budget *int) bool {
	if len(schema) == 0 {
		return true
	}
	if depth > schemaMaxDepth || *budget <= 0 {
		return true
	}
	*budget--
	if alternatives, ok := schema["type"].([]any); ok {
		for _, alternative := range alternatives {
			copy := make(map[string]any, len(schema))
			for key, nested := range schema {
				copy[key] = nested
			}
			copy["type"] = alternative
			if codexReviewH22Original(value, copy, depth+1, budget) {
				return true
			}
			if *budget <= 0 {
				return true
			}
		}
		return false
	}
	switch stringValue(schema["type"]) {
	case "object":
		object := objectValue(value)
		if object == nil {
			return false
		}
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				if _, exists := object[stringValue(name)]; !exists {
					return false
				}
			}
		}
		properties := objectValue(schema["properties"])
		for key, nested := range object {
			if properties == nil {
				continue
			}
			nestedSchema := objectValue(properties[key])
			if nestedSchema == nil {
				if schema["additionalProperties"] == false {
					return false
				}
				continue
			}
			if !codexReviewH22Original(nested, nestedSchema, depth+1, budget) {
				return false
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		if itemSchema := objectValue(schema["items"]); itemSchema != nil {
			for _, item := range items {
				if !codexReviewH22Original(item, itemSchema, depth+1, budget) {
					return false
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return false
		}
	case "integer", "number":
		switch value.(type) {
		case json.Number, float64, int, int64:
		default:
			return false
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	case "null":
		if value != nil {
			return false
		}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		matched := false
		for _, option := range enum {
			if h22EnumValuesEqual(option, value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// h22's own check is only order-dependent once its budget runs out. Where h22
// gives one answer, h25's sorted copy gives the same one; past the budget h22
// answers at random (logged, not asserted: map order is the runtime's) and
// h25 answers one of h22's answers, every time.
func TestH25CodexReviewH22CheckIsH22WhereH22IsDeterministic(t *testing.T) {
	type sample struct {
		name          string
		value, schema map[string]any
	}
	var samples []sample
	for _, n := range []int{10, 10000, 19990, 19999, 20000, 25000} {
		source, native := codexReviewBudgetFixture(n)
		samples = append(samples, sample{fmt.Sprintf("budget_%d", n), parseArguments(transportEnvelope(native)["args"]),
			firstMap(objectValue(source["tools"].([]any)[0]), "parameters")})
	}
	wide := map[string]any{}
	wideProps := map[string]any{}
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("k%02d", i)
		wide[key] = i
		wideProps[key] = map[string]any{"type": "integer"}
	}
	wide["k17"] = "x"
	samples = append(samples, sample{"wide_mismatch", wide, map[string]any{"type": "object", "properties": wideProps}})
	for _, tc := range []struct{ name, value, schema string }{
		{"integer_fraction", `{"n":1.5}`, `{"type":"object","properties":{"n":{"type":"integer"}}}`},
		{"no_type_required", `{}`, `{"required":["city"]}`},
		{"additional_false", `{"a":1,"b":2}`, `{"type":"object","properties":{"a":{"type":"integer"}},"additionalProperties":false}`},
		{"type_union", `{"n":null,"m":"x"}`, `{"type":"object","properties":{"n":{"type":["integer","null"]},"m":{"type":["integer","null"]}}}`},
		{"enum", `{"e":"b","f":2}`, `{"type":"object","properties":{"e":{"enum":["a","b"]},"f":{"enum":[1]}}}`},
	} {
		samples = append(samples, sample{tc.name, decodeToolJSONObject(tc.value), decodeToolJSONObject(tc.schema)})
	}
	for _, s := range samples {
		seen := map[bool]int{}
		for i := 0; i < 200; i++ {
			budget := schemaMaxVisits
			seen[codexReviewH22Original(s.value, s.schema, 0, &budget)]++
		}
		h25 := h22SchemaMatches(s.value, s.schema)
		for i := 0; i < 20; i++ {
			if h22SchemaMatches(s.value, s.schema) != h25 {
				t.Fatalf("%s: h25's copy of h22's check is not deterministic", s.name)
			}
		}
		if len(seen) == 1 {
			for answer := range seen {
				if answer != h25 {
					t.Errorf("%s: h22 always answers %v, h25 %v", s.name, answer, h25)
				}
			}
		} else if seen[h25] == 0 {
			t.Errorf("%s: h25 answers %v, which h22 never gave (%v)", s.name, h25, seen)
		}
		t.Logf("%s: h22 over 200 runs %v; h25 %v", s.name, seen, h25)
	}
}

// ---------------------------------------------------------------------------
// 6. E4: a blocked tool's bare name must not address another declared tool.

func codexReviewAgents() map[string]any {
	return map[string]any{"type": "namespace", "name": "agents", "tools": []any{
		map[string]any{"type": "function", "name": "send", "parameters": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string", "encrypted": true}}}},
	}}
}

func codexReviewFlat(name string) map[string]any {
	return map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object"}}
}

func TestH25CodexReviewEncryptedBareNameCollision(t *testing.T) {
	call := h21TransportItem("collide", "agents.send", map[string]any{"message": "hello"})
	for _, c := range []struct {
		name      string
		tools     []any
		input     []any
		want      string // delivered name; "" = refused
		collision bool
	}{
		{name: "top_level", tools: []any{codexReviewFlat("send"), codexReviewAgents()}, want: "agents.send", collision: true},
		{name: "additional_tools", tools: []any{codexReviewAgents()}, input: []any{map[string]any{"type": "additional_tools", "tools": []any{codexReviewFlat("send")}}}, want: "agents.send", collision: true},
		{name: "tool_search_output", tools: []any{codexReviewAgents()}, input: []any{toolSearchOutput("ts_collide", codexReviewFlat("send"))}, want: "agents.send", collision: true},
		{name: "excluded_by_tool_choice", tools: []any{codexReviewFlat("send"), codexReviewAgents()}, want: "agents.send", collision: true},
		{name: "none", tools: []any{codexReviewAgents()}, want: "send"},
		{name: "other_namespace", tools: []any{codexReviewAgents(), map[string]any{"type": "namespace", "name": "other", "tools": []any{codexReviewFlat("send")}}}, want: "send"},
		{name: "qualified_also_declared", tools: []any{codexReviewFlat("send"), codexReviewFlat("agents.send"), codexReviewAgents()}, want: ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			source := map[string]any{"model": "gpt-6-astra", "tools": c.tools, "input": append(append([]any{}, c.input...), s5User("go"))}
			if c.name == "excluded_by_tool_choice" {
				// A tool the request does not allow is still a tool the
				// client knows and could dispatch.
				source["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "name": "send", "namespace": "agents"}}}
			}
			decoded, reason := decodeTransportItemWithReason(call, source)
			if c.want == "" {
				if reason != "encrypted_tool_blocked" {
					t.Fatalf("decode %q, want encrypted_tool_blocked", reason)
				}
			} else if reason != "" || decoded["name"] != c.want || decoded["namespace"] != nil {
				t.Fatalf("decode %q: %s", reason, jsonBytes(decoded))
			}
			before := relayCounterSnapshot()
			result := runStreamTest(t, source, []string{s2Created(), h21ItemEvent("response.output_item.done", 0, call), h21Terminal("response.completed", call)})
			delta := reviewCounterDelta(before, "encrypted_tool_blocked", "encrypted_tool_blocked_collision")
			if c.want == "" {
				if !strings.Contains(result.closeErr, "reason=encrypted_tool_blocked") || strings.Contains(result.joined(), `"name":"send"`) {
					t.Fatalf("stream: close %q", result.closeErr)
				}
				return
			}
			if result.closeErr != "" || !strings.Contains(result.joined(), `"name":"`+c.want+`"`) || strings.Contains(result.joined(), `"namespace":"agents"`) {
				t.Fatalf("stream: close %q", result.closeErr)
			}
			if want := map[bool]uint64{true: 1}[c.collision]; delta["encrypted_tool_blocked"] != 1 || delta["encrypted_tool_blocked_collision"] != want {
				t.Fatalf("counters %v", delta)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 7. A response.created / response.in_progress snapshot with output is output.

func TestH25CodexReviewCreatedOutputSuppressesRetryHint(t *testing.T) {
	item := map[string]any{"type": "message", "id": "msg_review", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "already delivered"}}}
	for _, kind := range []string{"response.created", "response.in_progress"} {
		chunk := sseEvent(kind, map[string]any{"type": kind, "response": map[string]any{"id": "resp_review", "status": "in_progress", "output": []any{item}}})
		result := runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hello"}, []string{chunk})
		if !strings.Contains(result.joined(), "already delivered") || result.closeErr == "" {
			t.Fatalf("%s: probe did not emit the item then fail: %q", kind, result.closeErr)
		}
		if strings.Contains(result.closeErr, retryableErrorHint) {
			t.Fatalf("%s: E5 retry hint follows an emitted output item: %q", kind, result.closeErr)
		}
	}
	// Metadata alone is still no output: empty or missing output keeps the hint.
	for name, response := range map[string]map[string]any{
		"empty":   {"id": "resp_review", "status": "in_progress", "output": []any{}},
		"missing": {"id": "resp_review", "status": "in_progress"},
		"null":    {"id": "resp_review", "status": "in_progress", "output": nil},
	} {
		chunk := sseEvent("response.created", map[string]any{"type": "response.created", "response": response})
		result := runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hello"}, []string{chunk})
		if !strings.HasSuffix(result.closeErr, retryableErrorHint) {
			t.Fatalf("%s output: zero-output failure lost its hint: %q", name, result.closeErr)
		}
	}
}

// ---------------------------------------------------------------------------
// 8. Draft-7 schema-valued dependencies.

func TestH25CodexReviewEncryptedSchemaDependencies(t *testing.T) {
	tools := func(dependencies any) []any {
		return []any{map[string]any{"type": "namespace", "name": "agents", "tools": []any{
			map[string]any{"type": "function", "name": "send", "parameters": map[string]any{
				"$schema": "http://json-schema.org/draft-07/schema#", "type": "object",
				"dependencies": dependencies,
			}},
		}}}
	}
	native := map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "c", "arguments": `{"code":"{\"tool\":\"agents.send\",\"args\":{\"message\":\"hello\"}}"}`}
	marked := map[string]any{"message": map[string]any{"properties": map[string]any{"message": map[string]any{"type": "string", "encrypted": true}}}}
	source := map[string]any{"tools": tools(marked)}
	call, reason := decodeTransportItemWithReason(native, source)
	if strings.Contains(clientToolProtocolInstructions(source), "- agents.send (function)") {
		t.Fatal("tool with an encrypted mark under dependencies is still advertised")
	}
	if reason != "" || call["namespace"] != nil || call["name"] != "send" {
		t.Fatalf("decode %q: %s", reason, jsonBytes(call))
	}
	// dependentSchemas (2019-09+) likewise; an array-valued dependency (a
	// list of property names) is no schema and carries no mark.
	if !encryptedNamespacedTool(toolSpec{Namespace: "agents", Spec: map[string]any{"parameters": map[string]any{"dependentSchemas": marked}}}) {
		t.Fatal("dependentSchemas mark missed")
	}
	for _, dependencies := range []any{
		map[string]any{"message": []any{"encrypted"}},
		map[string]any{"message": map[string]any{"properties": map[string]any{"message": map[string]any{"type": "string"}}}},
		[]any{map[string]any{"encrypted": true}},
	} {
		source := map[string]any{"tools": tools(dependencies)}
		call, reason := decodeTransportItemWithReason(native, source)
		if !strings.Contains(clientToolProtocolInstructions(source), "- agents.send (function)") || reason != "" || call["namespace"] != "agents" {
			t.Fatalf("unmarked dependencies %s: hidden or blocked (%q)", jsonBytes(dependencies), reason)
		}
	}
}

// ---------------------------------------------------------------------------
// 9. An upstream failure terminal is not a relay success.

func TestH25CodexReviewFailedTerminalNotCountedAsSucceeded(t *testing.T) {
	for _, terminal := range []string{"error", "response.failed", "response.incomplete", "response.completed"} {
		t.Run(terminal, func(t *testing.T) {
			payload := map[string]any{"type": terminal}
			if terminal == "error" {
				payload["error"] = map[string]any{"message": "upstream execution failed", "code": "server_error"}
			} else {
				payload["response"] = map[string]any{"id": "resp_failed", "status": strings.TrimPrefix(terminal, "response."), "output": []any{}}
			}
			body := map[string]any{"model": "gpt-6-astra", "input": "show counter behavior"}
			succeeded := "relay_outcome:legacy:none:succeeded"
			upstream := "relay_outcome:legacy:none:upstream_terminal:" + terminal
			before := relayCounterSnapshot()
			run := s2RunStream(t, s2HostScript{}, body, []string{s2Created(), sseEvent(terminal, payload)})
			delta := reviewCounterDelta(before, succeeded, upstream)
			// The client sees the same close as before: the terminal already
			// told it how the response ended.
			if run.closeErr != "" {
				t.Fatalf("close %q", run.closeErr)
			}
			if terminal == "response.completed" {
				if delta[succeeded] != 1 || delta[upstream] != 0 {
					t.Fatalf("completed: %v", delta)
				}
				return
			}
			if delta[succeeded] != 0 || delta[upstream] != 1 {
				t.Fatalf("upstream %s counted as %v", terminal, delta)
			}
			summary := relayStatusSummary(relayCounterSnapshot())
			outcomes := summary["requests_by_protocol"].(map[string]map[string]map[string]any)["legacy"]["none"]["outcomes"].(map[string]uint64)
			if outcomes["upstream_terminal:"+terminal] == 0 {
				t.Fatalf("summary outcomes %v", outcomes)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Replay cache boundary (review closing paragraph).

// The reviewer's probe with its 8 MiB record: inside the 64 MiB per-entry
// limit now, so the next turn replays the model's own run_officejs, byte for
// byte, as h22 did.
func TestH25CodexReviewOversizedDeliveredCallReplaysExactly(t *testing.T) {
	s3ResetCaches()
	defer s3ResetCaches()
	if nativeCallEntryBytes != 64<<20 || nativeCallCacheBytes != 256<<20 {
		t.Fatalf("relay pool limits %d / %d", nativeCallEntryBytes, nativeCallCacheBytes)
	}
	source := weatherRequest()
	source["prompt_cache_key"] = "h25-codex-review-oversized"
	native := h21Native("codex_review_big", "get_weather", map[string]any{"city": "Singapore"})
	outer := parseArguments(native["arguments"])
	outer["summary"] = strings.Repeat("x", 8<<20)
	native["arguments"] = string(jsonBytes(outer))
	_, response, _, err := transformResponseBody(h21Response(native), source)
	if err != nil {
		t.Fatalf("first turn failed before delivery: %v", err)
	}
	client := objectValue(response["output"].([]any)[0])
	second := cloneObject(source)
	delete(second, "tools")
	second["input"] = []any{client, map[string]any{"type": "function_call_output", "call_id": client["call_id"], "output": "sunny"}}
	prepared, err := prepareResponsesBody(second, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range prepared["input"].([]any) {
		item := objectValue(raw)
		if stringValue(item["type"]) == "function_call" {
			if item["name"] != transportName || item["arguments"] != native["arguments"] {
				t.Fatalf("replayed %v (%d argument bytes), want the model's own run_officejs", item["name"], len(stringValue(item["arguments"])))
			}
			return
		}
	}
	t.Fatal("replayed call disappeared")
}

// codexReviewLowerEntryLimit makes the relay pool's per-entry limit small for
// one test, so a record past it need not be 64 MiB.
func codexReviewLowerEntryLimit(t *testing.T, limit int) {
	nativeCallCache.Lock()
	previous := nativeCallCache.maxEntry
	nativeCallCache.maxEntry = limit
	nativeCallCache.Unlock()
	t.Cleanup(func() {
		nativeCallCache.Lock()
		nativeCallCache.maxEntry = previous
		nativeCallCache.Unlock()
	})
}

// A relay record too large to keep leaves a marker: the next turn's cold path
// wraps the client's call as run_officejs (h22's fallback) even though the
// turn declares no tools, instead of sending the bare get_weather upstream.
func TestH25CodexReviewTooLargeRecordLeavesARelayMarker(t *testing.T) {
	s3ResetCaches()
	defer s3ResetCaches()
	codexReviewLowerEntryLimit(t, 64<<10)
	for _, plain := range []bool{false, true} {
		source := weatherRequest()
		source["prompt_cache_key"] = fmt.Sprintf("h25-codex-review-marker-%v", plain)
		native := h21Native(fmt.Sprintf("codex_review_marker_%v", plain), "get_weather", map[string]any{"city": "Singapore"})
		outer := parseArguments(native["arguments"])
		outer["summary"] = strings.Repeat("x", 100<<10)
		native["arguments"] = string(jsonBytes(outer))
		before := relayCounterSnapshot()
		_, response, _, err := transformResponseBody(h21Response(native), source)
		if err != nil {
			t.Fatalf("first turn failed before delivery: %v", err)
		}
		client := objectValue(response["output"].([]any)[0])
		callID := stringValue(client["call_id"])
		scope := conversationScope(source)
		if rememberedNativeCall(scope, callID) != nil {
			t.Fatal("an oversized record was kept for replay")
		}
		if !relayCallMarked(scope, callID) {
			t.Fatal("no provenance marker for the oversized relay call")
		}
		if _, state := cachedCallForHistory(scope, client); state != "miss" {
			t.Fatalf("a marker answered a history comparison: %s", state)
		}
		cfg := defaultConfig()
		cfg.ClientIdentity.PlainPrologue = plain
		for _, withCall := range []bool{true, false} {
			second := cloneObject(source)
			delete(second, "tools")
			output := map[string]any{"type": "custom_tool_call_output", "call_id": callID, "output": "sunny"}
			second["input"] = []any{output}
			if withCall {
				second["input"] = []any{client, output}
			}
			prepared, err := prepareResponsesBodyFor(second, cfg, conversationScope(second), persona{}, identityHygiene)
			if err != nil {
				t.Fatal(err)
			}
			var call, answer map[string]any
			for _, raw := range prepared["input"].([]any) {
				item := objectValue(raw)
				switch stringValue(item["type"]) {
				case "function_call", "custom_tool_call":
					call = item
				case "function_call_output", "custom_tool_call_output":
					answer = item
				}
			}
			if withCall {
				want := fallbackTransportCallFor(client, plain)
				if call == nil || string(jsonBytes(call)) != string(jsonBytes(want)) {
					t.Fatalf("plain=%v: second turn sent %s, want h22's cold wrapping", plain, jsonBytes(call))
				}
			} else if call != nil {
				t.Fatalf("plain=%v: a call appeared from nowhere", plain)
			}
			if answer == nil || answer["type"] != "function_call_output" || answer["id"] != functionItemID(callID) {
				t.Fatalf("plain=%v withCall=%v: output not paired with run_officejs: %s", plain, withCall, jsonBytes(answer))
			}
		}
		delta := reviewCounterDelta(before, "replay_record_too_large:relay", "history_marker_wrapped")
		if delta["replay_record_too_large:relay"] != 1 || delta["history_marker_wrapped"] == 0 {
			t.Fatalf("plain=%v: counters %v", plain, delta)
		}
	}
}

// The marker obeys the pool: it replaces the earlier record under its key
// (h22: the later write wins), a later record that fits replaces it, it
// expires with the TTL, and it holds no argument bytes.
func TestH25CodexReviewMarkerLifecycle(t *testing.T) {
	s3ResetCaches()
	defer s3ResetCaches()
	codexReviewLowerEntryLimit(t, 4<<10)
	scope := "h25-codex-review-marker-life"
	small := map[string]any{"type": "function_call", "name": transportName, "call_id": "call_m", "arguments": `{"code":"small"}`}
	huge := cloneObject(small)
	huge["arguments"] = `{"code":"` + strings.Repeat("y", 8<<10) + `"}`
	rememberRelayCall(scope, small, nil)
	rememberRelayCall(scope, huge, nil)
	if rememberedNativeCall(scope, "call_m") != nil || !relayCallMarked(scope, "call_m") {
		t.Fatal("oversized rewrite: earlier record kept or no marker")
	}
	if sum, order := reviewPoolWeights(nativeCallCache); sum <= 0 || sum > 1024 || len(order) != 1 {
		t.Fatalf("marker weighs %d bytes, order %v", sum, order)
	}
	rememberRelayCall(scope, small, nil)
	if rememberedNativeCall(scope, "call_m") == nil || relayCallMarked(scope, "call_m") {
		t.Fatal("a record that fits did not replace the marker")
	}
	rememberRelayCall(scope, huge, nil)
	nativeCallCache.Lock()
	key := nativeCallCacheKey(scope, "call_m")
	entry := nativeCallCache.items[key]
	entry.storedAt = time.Now().Add(-nativeCallCacheTTL - time.Minute)
	nativeCallCache.items[key] = entry
	nativeCallCache.Unlock()
	if relayCallMarked(scope, "call_m") {
		t.Fatal("an expired marker still counts")
	}
	// The native (Lite) pool keeps no markers: under replay it never changes
	// a byte sent upstream.
	native := map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call_n", "arguments": `{"cmd":"` + strings.Repeat("z", nativeOriginEntryBytes-len(`{"cmd":""}`)) + `"}`}
	if _, valid := clientCallFingerprint(native); !valid {
		t.Fatal("fixture: the large native call must still be fingerprintable")
	}
	rememberDeliveredNativeCall(scope, native)
	if _, found := nativeOriginCache.lookup(scope, "call_n"); found {
		t.Fatal("the native pool kept an oversized record or a marker")
	}
}

// ---------------------------------------------------------------------------
// batch heartbeat: a heartbeat that stops is counted and logged.

func TestH25CodexReviewBatchHeartbeatStopIsVisible(t *testing.T) {
	interval, poll := relayHeartbeatInterval, relayHeartbeatPoll
	relayHeartbeatInterval, relayHeartbeatPoll = 20*time.Millisecond, 2*time.Millisecond
	defer func() { relayHeartbeatInterval, relayHeartbeatPoll = interval, poll }()
	paris := h21TransportItem("heartbeat_stop", "get_weather", map[string]any{"city": "Paris"})
	added := cloneObject(paris)
	added["status"], added["arguments"] = "in_progress", ""
	chunks := []string{s2Created() + h21ItemEvent("response.output_item.added", 0, added), h21ItemEvent("response.output_item.done", 0, paris), h21Terminal("response.completed", paris)}
	for _, c := range []struct {
		reason string
		emit   func(string) bool
	}{
		{"emit_failed", func(payload string) bool { return payload == relayHeartbeatFrame }},
		{"panic", func(payload string) bool {
			if payload == relayHeartbeatFrame {
				panic("synthetic host panic on the heartbeat")
			}
			return false
		}},
	} {
		before := relayCounterSnapshot()
		run := s2RunStream(t, s2HostScript{configure: s2Batch, readDelay: 150 * time.Millisecond, failEmit: c.emit}, weatherRequest(), chunks)
		// The relay's own frames are unaffected: the call is delivered.
		if run.closeErr != "" || s2DoneCount(run.joined()) != 1 {
			t.Fatalf("%s: close %q", c.reason, run.closeErr)
		}
		if got := reviewCounterDelta(before, "batch_heartbeat_stopped")["batch_heartbeat_stopped"]; got != 1 {
			t.Fatalf("%s: batch_heartbeat_stopped +%d, want +1", c.reason, got)
		}
		logged := false
		for _, entry := range run.logs {
			fields := objectValue(entry["fields"])
			if fields["event"] == "relay_batch_heartbeat_stopped" && fields["reason"] == c.reason && entry["level"] == "warn" {
				logged = true
			}
		}
		if !logged {
			t.Fatalf("%s: no relay_batch_heartbeat_stopped log", c.reason)
		}
	}
	// A heartbeat that is simply not needed (immediate) never reports a stop.
	before := relayCounterSnapshot()
	if run := s2RunStream(t, s2HostScript{readDelay: 50 * time.Millisecond}, weatherRequest(), chunks); run.closeErr != "" {
		t.Fatal(run.closeErr)
	}
	if got := reviewCounterDelta(before, "batch_heartbeat_stopped")["batch_heartbeat_stopped"]; got != 0 {
		t.Fatalf("immediate delivery reported a heartbeat stop: +%d", got)
	}
}
