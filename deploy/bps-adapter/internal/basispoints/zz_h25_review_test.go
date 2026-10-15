package basispoints

// Fixes from the h25 verification round (wf2): EH-1 (the native tool
// catalogue is built once per stream), R1/EH-5 (replay pool budget and
// rewrites), F2/F3 (boolean switches accept YAML booleans only), F6 (E3 only
// converts numbers a client can hold), EH-3/EH-4 (the native and
// tool_search_output E3 repairs are counted) and R5 (replay scope counted on
// admission). Default behaviour stays h22's: the S1~S5 golden tests are the
// proof of that; these tests pin the fixes themselves.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- EH-1

// reviewLiteSource is a Lite request with history items, additional_tools and
// tool_search_output results. With dup, every search re-declares the same
// names with different wording, the case that made each fragment compare
// every declaration pair as JSON.
func reviewLiteSource(history, searches, perSearch int, dup bool) map[string]any {
	tools := []any{map[string]any{"type": "namespace", "name": "shell", "tools": []any{
		map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}},
	}}}
	for i := 0; i < 10; i++ {
		tools = append(tools, map[string]any{"type": "function", "name": fmt.Sprintf("extra_%d", i), "description": strings.Repeat("d", 120),
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}})
	}
	input := []any{map[string]any{"type": "additional_tools", "tools": tools}}
	for s := 0; s < searches; s++ {
		var found []any
		for i := 0; i < perSearch; i++ {
			name := fmt.Sprintf("s%d_tool_%d", s, i)
			if dup {
				name = fmt.Sprintf("tool_%d", i)
			}
			found = append(found, map[string]any{"type": "function", "name": name, "description": fmt.Sprintf("refresh %d %s", s, strings.Repeat("desc ", 30)),
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}, "n": map[string]any{"type": "integer"}}}})
		}
		input = append(input, toolSearchOutput(fmt.Sprintf("ts%d", s), map[string]any{"type": "namespace", "name": "mcp__x", "tools": found}))
	}
	for i := 0; i < history; i++ {
		switch i % 4 {
		case 0:
			input = append(input, messageItem("user", strings.Repeat("u", 300)))
		case 1:
			input = append(input, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": strings.Repeat("E", 400)})
		case 2:
			input = append(input, map[string]any{"type": "function_call", "call_id": fmt.Sprintf("h%d", i), "name": "exec_command", "namespace": "shell", "arguments": `{"cmd":"ls -la"}`})
		case 3:
			input = append(input, map[string]any{"type": "function_call_output", "call_id": fmt.Sprintf("h%d", i-1), "output": strings.Repeat("o", 500)})
		}
	}
	return map[string]any{"model": "gpt-6-astra", "input": input, "tool_choice": "auto", "parallel_tool_calls": true, "prompt_cache_key": "h25-review-eh1"}
}

// reviewNativeRelay is a relay whose native call has been announced, plus one
// argument fragment event for it.
func reviewNativeRelay(t *testing.T, source map[string]any) (*streamRelay, []byte) {
	t.Helper()
	parsed, err := rawObject(jsonBytes(source))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService()
	service.SetHost(func(method string, payload any, out any) error { return nil })
	relay := newStreamRelay(service, "h25-review-eh1", parsed, usageTarget{}, "gpt-6-astra")
	added := map[string]any{"type": "function_call", "id": "fc_review", "call_id": "call_review", "name": "exec_command", "namespace": "shell", "arguments": "", "status": "in_progress"}
	for _, event := range []string{
		s2Created(),
		sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added}),
	} {
		if err := relay.feed([]byte(event)); err != nil {
			t.Fatal(err)
		}
		if err := relay.flush(); err != nil {
			t.Fatal(err)
		}
	}
	delta := sseEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_review", "delta": "ab"})
	return relay, []byte(delta)
}

// EH-1: the work per native argument fragment does not grow with the
// request's history or declarations (allocation counts, so not flaky), and a
// long call on the review's worst request shape stays far inside a time bound
// that the per-fragment rebuild missed by seconds.
func TestH25ReviewNativeDeltaCostIsIndependentOfRequestSize(t *testing.T) {
	allocs := func(source map[string]any) float64 {
		relay, delta := reviewNativeRelay(t, source)
		return testing.AllocsPerRun(100, func() {
			if err := relay.feed(delta); err != nil {
				t.Fatal(err)
			}
			if err := relay.flush(); err != nil {
				t.Fatal(err)
			}
		})
	}
	small := allocs(reviewLiteSource(4, 0, 0, false))
	large := allocs(reviewLiteSource(2000, 30, 40, true))
	t.Logf("allocations per native fragment: small request %.0f, large request %.0f", small, large)
	if large > small+5 {
		t.Fatalf("a native fragment costs %.0f allocations on a large request, %.0f on a small one", large, small)
	}

	// Time bound: 500 fragments. Before the per-stream catalogue this request
	// took about 5.7 ms per fragment (2.8 s for the call); now it is a few
	// microseconds, so one second leaves room for -race and a loaded machine.
	const fragments = 500
	relay, delta := reviewNativeRelay(t, reviewLiteSource(2000, 30, 40, true))
	start := time.Now()
	for i := 0; i < fragments; i++ {
		if err := relay.feed(delta); err != nil {
			t.Fatal(err)
		}
		if err := relay.flush(); err != nil {
			t.Fatal(err)
		}
	}
	done := map[string]any{"type": "function_call", "id": "fc_review", "call_id": "call_review", "name": "exec_command", "namespace": "shell",
		"arguments": `{"cmd":"` + strings.Repeat("ab", fragments) + `"}`, "status": "completed"}
	for _, event := range []string{
		sseEvent("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "fc_review", "arguments": done["arguments"]}),
		h21ItemEvent("response.output_item.done", 0, done),
		h21Terminal("response.completed", done),
	} {
		if err := relay.feed([]byte(event)); err != nil {
			t.Fatal(err)
		}
		if err := relay.flush(); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("%d native fragments + done + terminal on the large request: %v", fragments, elapsed)
	if elapsed > time.Second {
		t.Fatalf("%d native fragments took %v", fragments, elapsed)
	}
	if relay.terminal != "response.completed" {
		t.Fatalf("the call did not complete: %q", relay.terminal)
	}
}

// EH-1 changes no answer: the cached catalogue gives every policy check the
// result the uncached one gives, and a stream builds it at most once.
func TestH25ReviewCachedCatalogueGivesTheSameAnswers(t *testing.T) {
	base := reviewLiteSource(8, 2, 3, true)
	sources := map[string]map[string]any{"auto": base}
	for name, choice := range map[string]any{
		"none":          "none",
		"forced":        map[string]any{"type": "function", "namespace": "shell", "name": "exec_command"},
		"forced_other":  map[string]any{"type": "function", "name": "extra_1"},
		"allowed":       map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "name": "extra_2"}}},
		"allowed_none":  map[string]any{"type": "allowed_tools", "mode": "none", "tools": []any{}},
		"allowed_other": map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "mcp", "server_label": "x"}}},
	} {
		sources[name] = s5Set(base, "tool_choice", choice)
	}
	items := map[string]map[string]any{
		"declared":       {"type": "function_call", "id": "fc_1", "call_id": "c1", "name": "exec_command", "namespace": "shell", "arguments": ""},
		"no_namespace":   {"type": "function_call", "id": "fc_2", "call_id": "c2", "name": "exec_command", "arguments": ""},
		"extra":          {"type": "function_call", "id": "fc_3", "call_id": "c3", "name": "extra_2", "arguments": ""},
		"searched_dup":   {"type": "function_call", "id": "fc_4", "call_id": "c4", "name": "tool_1", "namespace": "mcp__x", "arguments": ""},
		"undeclared":     {"type": "function_call", "id": "fc_5", "call_id": "c5", "name": "nope", "arguments": ""},
		"custom_as_func": {"type": "custom_tool_call", "id": "ctc_6", "call_id": "c6", "name": "extra_2", "input": ""},
		"unnamed":        {"type": "function_call", "id": "fc_7", "call_id": "c7", "arguments": ""},
		"transport":      {"type": "function_call", "id": "fc_8", "call_id": "c8", "name": transportName, "arguments": ""},
	}
	events := []struct {
		kind    string
		payload map[string]any
	}{
		{"response.function_call_arguments.delta", map[string]any{"item_id": "fc_1"}},
		{"response.function_call_arguments.delta", map[string]any{"item_id": "fc_other"}},
		{"response.custom_tool_call_input.delta", map[string]any{"item_id": "ctc_6"}},
	}
	for sourceName, source := range sources {
		builds := 0
		var cached map[string]toolSpec
		catalog := func() map[string]toolSpec {
			if cached == nil {
				builds++
				cached = allDeclaredClientToolSpecs(source)
			}
			return cached
		}
		for itemName, item := range items {
			if got, want := nativeToolPolicyReasonIn(item, source, catalog), nativeToolPolicyReason(item, source); got != want {
				t.Errorf("%s/%s: policy %q cached, %q uncached", sourceName, itemName, got, want)
			}
			gotSpec, gotKnown := knownNativeToolSpecIn(item, source, catalog)
			wantSpec, wantKnown := knownNativeToolSpec(item, source)
			if gotKnown != wantKnown || !reflect.DeepEqual(gotSpec, wantSpec) {
				t.Errorf("%s/%s: known spec differs with the cached catalogue", sourceName, itemName)
			}
			for _, event := range events {
				if got, want := nativeToolDeltaPolicyReasonIn(event.kind, event.payload, item, source, catalog), nativeToolDeltaPolicyReason(event.kind, event.payload, item, source); got != want {
					t.Errorf("%s/%s/%s: delta policy %q cached, %q uncached", sourceName, itemName, event.kind, got, want)
				}
			}
		}
		if builds > 1 {
			t.Errorf("%s: catalogue built %d times", sourceName, builds)
		}
	}
	relay := newStreamRelay(NewService(), "h25-review-cache", base, usageTarget{}, "gpt-6-astra")
	first, second := relay.declaredTools(), relay.declaredTools()
	if reflect.ValueOf(first).Pointer() != reflect.ValueOf(second).Pointer() || !reflect.DeepEqual(first, allDeclaredClientToolSpecs(base)) {
		t.Fatal("the stream does not keep one catalogue")
	}
	empty := newStreamRelay(NewService(), "h25-review-empty", map[string]any{"model": "gpt-6-astra", "input": "x"}, usageTarget{}, "gpt-6-astra")
	if len(empty.declaredTools()) != 0 || !empty.declaredBuilt {
		t.Fatal("an empty catalogue is not cached")
	}
}

// ---------------------------------------------------------------- R1 / EH-5

func reviewPoolWeights(pool *replayPool) (int, []string) {
	pool.Lock()
	defer pool.Unlock()
	sum := 0
	for _, entry := range pool.items {
		sum += entry.weight
	}
	if sum != pool.bytes || len(pool.order) != len(pool.items) {
		return -1, nil
	}
	return sum, append([]string(nil), pool.order...)
}

// R1: at production record sizes the relay pool keeps h22's 4096 entries.
// The budget is 256 MiB: records averaging 60 KiB -- twice the largest of 676
// production run_officejs calls -- still all fit, and only records above
// 64 KiB on average make the budget, not the count, evict.
func TestH25ReviewRelayPoolKeepsH22CountAtProductionSizes(t *testing.T) {
	if nativeCallCacheEntries != 4096 || nativeCallCacheBytes != 256<<20 || nativeCallEntryBytes != 64<<20 {
		t.Fatalf("relay pool limits %d / %d / %d", nativeCallCacheEntries, nativeCallCacheBytes, nativeCallEntryBytes)
	}
	if nativeOriginCacheEntries != 4096 || nativeOriginCacheBytes != 16<<20 || nativeOriginEntryBytes != 1<<20 {
		t.Fatalf("native pool limits changed")
	}
	s3ResetCaches()
	defer s3ResetCaches()
	fill := func(size int) (uint64, bool) {
		s3ResetCaches()
		before := relayCounterSnapshot()["replay_byte_eviction:relay"]
		// One string shared by every record: the weight counts its length,
		// the test does not hold 4096 copies.
		arguments := strings.Repeat("x", size)
		for i := 0; i < nativeCallCacheEntries; i++ {
			rememberRelayCall("h25-review-budget", map[string]any{"type": "function_call", "name": transportName, "call_id": fmt.Sprintf("call_%d", i), "arguments": arguments}, nil)
		}
		_, oldestKept := nativeCallCache.lookup("h25-review-budget", "call_0")
		return relayCounterSnapshot()["replay_byte_eviction:relay"] - before, oldestKept
	}
	for _, size := range []int{2 << 10, 20 << 10, 60 << 10} {
		evicted, oldestKept := fill(size)
		nativeCallCache.Lock()
		kept := len(nativeCallCache.items)
		nativeCallCache.Unlock()
		if kept != nativeCallCacheEntries || evicted != 0 || !oldestKept {
			t.Errorf("records of %d bytes: kept %d, byte evictions %d, oldest kept %v", size, kept, evicted, oldestKept)
		}
	}
	// Above 64 KiB on average the budget binds, visibly.
	evicted, oldestKept := fill(70 << 10)
	if evicted == 0 || oldestKept {
		t.Errorf("records of 70 KiB: byte evictions %d, oldest kept %v", evicted, oldestKept)
	}
	if sum, _ := reviewPoolWeights(nativeCallCache); sum < 0 || sum > nativeCallCacheBytes {
		t.Fatalf("relay pool accounting broken: %d", sum)
	}
}

// R1: a later write too large to keep does not leave the earlier record for
// its call_id to be replayed in its place (h22: the later write wins). Since
// the h25 review a relay-pool marker takes its place (no arguments, never
// replayed; see TestH25CodexReviewTooLargeRecordLeavesARelayMarker). The
// per-entry limit is lowered for the test; the real one is 64 MiB.
func TestH25ReviewOversizedRewriteForgetsTheEarlierRecord(t *testing.T) {
	s3ResetCaches()
	defer s3ResetCaches()
	codexReviewLowerEntryLimit(t, 1<<20)
	scope := "h25-review-oversized"
	small := map[string]any{"type": "function_call", "name": transportName, "call_id": "call_big", "arguments": `{"code":"small"}`}
	huge := cloneObject(small)
	huge["arguments"] = `{"code":"` + strings.Repeat("y", 1<<20) + `"}`
	before := relayCounterSnapshot()["replay_record_too_large:relay"]
	rememberRelayCall(scope, small, nil)
	if rememberedNativeCall(scope, "call_big") == nil {
		t.Fatal("fixture: small record not kept")
	}
	rememberRelayCall(scope, huge, nil)
	if rememberedNativeCall(scope, "call_big") != nil {
		t.Fatal("an oversized rewrite left the earlier record to be replayed")
	}
	if got := relayCounterSnapshot()["replay_record_too_large:relay"] - before; got != 1 {
		t.Fatalf("replay_record_too_large:relay +%d", got)
	}
	// Only the marker is left, and it holds no argument bytes.
	if sum, order := reviewPoolWeights(nativeCallCache); sum <= 0 || sum > 1024 || len(order) != 1 || !relayCallMarked(scope, "call_big") {
		t.Fatalf("pool after the oversized rewrite: %d bytes, order %v", sum, order)
	}
	// A new key that is too large is not kept either, only marked.
	rememberRelayCall(scope, cloneObject(map[string]any{"type": "function_call", "name": transportName, "call_id": "call_new", "arguments": huge["arguments"]}), nil)
	if rememberedNativeCall(scope, "call_new") != nil || !relayCallMarked(scope, "call_new") {
		t.Fatal("an oversized record was kept, or not marked")
	}

	// The native pool shares put: a native call whose record exceeds its
	// 1 MiB limit forgets the earlier delivery under its call_id too.
	native := map[string]any{"type": "function_call", "name": "exec_command", "namespace": "shell", "call_id": "call_native_big", "arguments": `{"cmd":"ls"}`}
	rememberDeliveredNativeCall(scope, native)
	if _, found := nativeOriginCache.lookup(scope, "call_native_big"); !found {
		t.Fatal("fixture: native record not kept")
	}
	bigNative := cloneObject(native)
	bigNative["arguments"] = `{"cmd":"` + strings.Repeat("z", nativeOriginEntryBytes-len(`{"cmd":""}`)) + `"}`
	if _, valid := clientCallFingerprint(bigNative); !valid {
		t.Fatal("fixture: the large native call must still be fingerprintable")
	}
	rememberDeliveredNativeCall(scope, bigNative)
	if _, found := nativeOriginCache.lookup(scope, "call_native_big"); found {
		t.Fatal("an oversized native rewrite left the earlier record")
	}

	// reject (rc2): a tombstone holds no argument data and is what a
	// successful conflicting write leaves, so it is kept; a record made under
	// another strict scope is not.
	pool := &replayPool{items: map[string]nativeCallEntry{}, name: "review", maxEntries: 16, maxBytes: 64 << 10, maxEntry: 4 << 10}
	big := func(fingerprint, strict string) nativeCallEntry {
		return nativeCallEntry{item: map[string]any{"name": transportName, "arguments": strings.Repeat("q", 8<<10)}, fingerprint: fingerprint, origin: "relay", strict: strict}
	}
	pool.put("same", nativeCallEntry{item: map[string]any{"name": transportName}, fingerprint: "a", origin: "relay", strict: "s"}, true)
	pool.put("same", big("b", "s"), true)
	if entry, found := pool.items["same"]; !found || !entry.conflict {
		t.Fatal("reject mode lost its tombstone to an oversized write")
	}
	pool.put("other", nativeCallEntry{item: map[string]any{"name": transportName}, fingerprint: "a", origin: "relay", strict: "s1"}, true)
	pool.put("other", big("b", "s2"), true)
	if _, found := pool.items["other"]; found {
		t.Fatal("reject mode kept a superseded record under another strict scope")
	}
	if sum, _ := reviewPoolWeights(pool); sum < 0 {
		t.Fatal("pool accounting broken")
	}
}

// R1: a re-written key keeps its FIFO place. When it is the oldest and its new
// size is what overflows the budget, the next oldest record is evicted -- not
// the record just written.
func TestH25ReviewRewriteNeverEvictsItself(t *testing.T) {
	pool := &replayPool{items: map[string]nativeCallEntry{}, name: "review", maxEntries: 4096, maxBytes: 10 << 10, maxEntry: 8 << 10}
	entry := func(size int) nativeCallEntry {
		return nativeCallEntry{item: map[string]any{"name": transportName, "arguments": strings.Repeat("x", size)}, origin: "relay"}
	}
	pool.put("a", entry(1000), false)
	pool.put("b", entry(4000), false)
	pool.put("c", entry(4000), false)
	pool.put("a", entry(5000), false)
	sum, order := reviewPoolWeights(pool)
	if _, kept := pool.items["a"]; !kept || !reflect.DeepEqual(order, []string{"a", "c"}) || sum > pool.maxBytes {
		t.Fatalf("after re-writing the oldest key: order %v, %d bytes", order, sum)
	}
	// Re-written again past everything else: only it is left.
	pool.put("a", entry(7900), false)
	if sum, order := reviewPoolWeights(pool); !reflect.DeepEqual(order, []string{"a"}) || sum > pool.maxBytes {
		t.Fatalf("after a second re-write: order %v, %d bytes", order, sum)
	}
	// A new key still evicts from the front, as in h22.
	pool.put("d", entry(4000), false)
	if _, order := reviewPoolWeights(pool); !reflect.DeepEqual(order, []string{"d"}) {
		t.Fatalf("a new key did not evict the oldest: %v", order)
	}
}

// ---------------------------------------------------------------- F2 / F3

// The four boolean switches accept YAML booleans only (the YAML 1.2 core
// schema spellings). yes/no/on/off, quoted words, numbers and anything else
// fall back to the default with one warning, and registration never fails.
func TestH25ReviewBooleanSwitchesAcceptOnlyYAMLBooleans(t *testing.T) {
	t.Chdir(t.TempDir())
	defaults := defaultRelayOptions().describe()
	accepted := map[string]bool{"true": true, "True": true, "TRUE": true, "false": false, "False": false, "FALSE": false, "!!bool true": true, "!!bool false": false}
	rejected := []string{"yes", "no", "Yes", "NO", "on", "off", "On", "OFF", "y", "n", "Y", "N", `"no"`, `"yes"`, `'off'`, `"true"`, `'false'`, "1", "0", "1.0", "tRue", "!!bool yes", "!!str true", "maybe", "[true]", "{a: true}"}
	for _, key := range []string{"relay_hide_encrypted_tools", "coerce_scalar_strings", "retryable_error_hint", "schema_shadow"} {
		for spelling, want := range accepted {
			effective, warnings := s5Status(t, s5Register(t, key+": "+spelling+"\n", nil))
			if effective[key] != want || len(warnings) != 0 {
				t.Errorf("%s: %s gave %v (warnings %v)", key, spelling, effective[key], warnings)
			}
		}
		for _, spelling := range rejected {
			var logs []map[string]any
			service := s5Register(t, key+": "+spelling+"\nmax_concurrency_per_credential: 3\n", &logs)
			effective, warnings := s5Status(t, service)
			if effective[key] != defaults[key] || len(warnings) != 1 || !strings.HasPrefix(warnings[0], key+": value is not true or false") {
				t.Errorf("%s: %s gave %v (warnings %v)", key, spelling, effective[key], warnings)
			}
			if service.config().MaxConcurrencyPerCredential != 3 || len(logs) != 0 {
				t.Errorf("%s: %s cost the rest of the file or logged during registration", key, spelling)
			}
		}
	}
	// The verification's case: "no" used to switch E3 off without a word.
	effective, warnings := s5Status(t, s5Register(t, "coerce_scalar_strings: \"no\"\nrelay_hide_encrypted_tools: \"yes\"\n", nil))
	if effective["coerce_scalar_strings"] != true || effective["relay_hide_encrypted_tools"] != true || len(warnings) != 2 {
		t.Fatalf("quoted yes/no: %v (warnings %v)", effective, warnings)
	}
}

// ---------------------------------------------------------------- F6

// E3 converts a string only into a number the client can hold: an integer
// within int64, a number that parses to a finite float64. Anything else stays
// a string and the call fails as it did in h22.
func TestH25ReviewCoercionOnlyToClientSafeNumbers(t *testing.T) {
	for _, c := range []struct {
		text     string
		declared any
		want     any // nil: not converted
	}{
		{"9223372036854775807", "integer", json.Number("9223372036854775807")},
		{"-9223372036854775808", "integer", json.Number("-9223372036854775808")},
		{"9223372036854775808", "integer", nil},
		{"-9223372036854775809", "integer", nil},
		{"123456789012345678901234567890", "integer", nil},
		{"-0", "integer", json.Number("-0")},
		{"1e3", "integer", nil},
		{"1.0", "integer", nil},
		{"9223372036854775808", []any{"integer", "number"}, json.Number("9223372036854775808")},
		{"1e308", "number", json.Number("1e308")},
		{"1.7976931348623157e308", "number", json.Number("1.7976931348623157e308")},
		{"1.8e308", "number", nil},
		{"1e309", "number", nil},
		{"1E400", "number", nil},
		{"-1e400", "number", nil},
		{"1e999999999", "number", nil},
		{"1e-400", "number", json.Number("1e-400")},
		{"9007199254740993", "number", json.Number("9007199254740993")},
		{"123456789012345678901234567890", "number", json.Number("123456789012345678901234567890")},
		{"0.50", "number", json.Number("0.50")},
		{"true", "boolean", true},
		{"00123", "integer", nil},
		{"+5", "number", nil},
	} {
		got, changed := coerceScalarString(c.text, c.declared)
		if c.want == nil {
			if changed {
				t.Errorf("%q as %v converted to %v", c.text, c.declared, got)
			}
			continue
		}
		if !changed || got != c.want {
			t.Errorf("%q as %v: got %v (%v), want %v", c.text, c.declared, got, changed, c.want)
		}
	}
	// Through the decoder: an out-of-range value fails exactly as in h22.
	source := withRelayOptions(s1DecodeSources()["multi"], defaultRelayOptions())
	for _, c := range []struct {
		args   map[string]any
		reason string
		want   string
	}{
		{map[string]any{"path": "/", "limit": "9223372036854775807"}, "", `{"limit":9223372036854775807,"path":"/"}`},
		{map[string]any{"path": "/", "limit": "9223372036854775808"}, "schema_mismatch", ""},
		{map[string]any{"path": "/", "ratio": "1e308"}, "", `{"path":"/","ratio":1e308}`},
		{map[string]any{"path": "/", "ratio": "1e400"}, "schema_mismatch", ""},
		{map[string]any{"path": "/", "ratio": "1e-400"}, "", `{"path":"/","ratio":1e-400}`},
	} {
		call, reason := decodeTransportItemWithReason(s1ListFilesCall(c.args), source)
		if reason != c.reason || (c.want != "" && call["arguments"] != c.want) {
			t.Errorf("%v: reason %q arguments %v", c.args, reason, call["arguments"])
		}
	}
}

// ---------------------------------------------------------------- EH-3 / EH-4

func reviewCounterDelta(before map[string]uint64, keys ...string) map[string]uint64 {
	after := relayCounterSnapshot()
	delta := map[string]uint64{}
	for _, key := range keys {
		delta[key] = after[key] - before[key]
	}
	return delta
}

// EH-3: the native (Lite) terminal repairs -- a terminal item missing the id
// its done had, and terminal arguments written differently but meaning the
// same -- are kept (they only turn h22 failures into successes, or make the
// terminal agree with the done the client already has) and are counted apart
// from the relay ones.
func TestH25ReviewNativeTerminalRepairsAreCounted(t *testing.T) {
	keys := []string{"terminal_id_restored", "terminal_id_restored_native", "terminal_args_equivalent", "terminal_args_equivalent_native"}
	done := s5Native("rv_native_id", "shell", "exec_command", `{"cmd":"ls"}`)
	missing := cloneObject(done)
	delete(missing, "id")
	nullID := cloneObject(done)
	nullID["id"] = nil
	for name, terminal := range map[string]map[string]any{"missing": missing, "null": nullID} {
		before := relayCounterSnapshot()
		result := runStreamTest(t, s5Lite("h25-review-native-id-"+name, s5LiteDecl(), s5User("x")), s5Stream("r_rv_id_"+name).call(done).replaceOutput(0, terminal).completed())
		if result.closeErr != "" || !strings.Contains(result.joined(), `"id":"fc_rv_native_id"`) {
			t.Fatalf("%s terminal id: %q", name, result.closeErr)
		}
		if delta := reviewCounterDelta(before, keys...); !reflect.DeepEqual(delta, map[string]uint64{"terminal_id_restored": 0, "terminal_id_restored_native": 1, "terminal_args_equivalent": 0, "terminal_args_equivalent_native": 0}) {
			t.Errorf("%s terminal id: counters %v", name, delta)
		}
	}
	spaced := s5Native("rv_native_args", "shell", "exec_command", `{"cmd": "ls"}`)
	compact := s5Native("rv_native_args", "shell", "exec_command", `{"cmd":"ls"}`)
	before := relayCounterSnapshot()
	result := runStreamTest(t, s5Lite("h25-review-native-args", s5LiteDecl(), s5User("x")), s5Stream("r_rv_args").call(spaced).replaceOutput(0, compact).completed())
	terminal := result.emits[len(result.emits)-1]
	for _, emit := range result.emits {
		if strings.Contains(emit, "response.completed") {
			terminal = emit
		}
	}
	if result.closeErr != "" || !strings.Contains(terminal, `{\"cmd\": \"ls\"}`) {
		t.Fatalf("terminal arguments: %q %s", result.closeErr, terminal)
	}
	if delta := reviewCounterDelta(before, keys...); !reflect.DeepEqual(delta, map[string]uint64{"terminal_id_restored": 0, "terminal_id_restored_native": 0, "terminal_args_equivalent": 0, "terminal_args_equivalent_native": 1}) {
		t.Errorf("terminal arguments: counters %v", delta)
	}
	// The same bytes in both: nothing to count.
	before = relayCounterSnapshot()
	if result := runStreamTest(t, s5Lite("h25-review-native-same", s5LiteDecl(), s5User("x")), s5Stream("r_rv_same").call(compact).completed()); result.closeErr != "" {
		t.Fatal(result.closeErr)
	}
	if delta := reviewCounterDelta(before, keys...); delta["terminal_args_equivalent_native"] != 0 || delta["terminal_id_restored_native"] != 0 {
		t.Errorf("identical terminal counted: %v", delta)
	}
	// relay-status lists them with the other E3 counters.
	status := s4ReadStatus(t, NewService())
	exceptions := objectValue(objectValue(status["summary"])["exceptions"])
	for _, name := range []string{"E3_terminal_id_restored_native", "E3_terminal_args_equivalent_native", "E3_search_duplicate_merged"} {
		if _, listed := exceptions[name]; !listed {
			t.Errorf("relay-status summary lacks %s", name)
		}
	}
}

// EH-4: a run_officejs call to a tool that tool_search_output declared twice,
// differing only in description or defer_loading, is delivered (h22: the two
// declarations conflicted and the call was tool_not_in_catalog) and counted
// as search_duplicate_merged. A real contract difference stays a conflict.
func TestH25ReviewSearchDuplicateMergeIsCounted(t *testing.T) {
	find := func(description string, extra map[string]any) map[string]any {
		tool := map[string]any{"type": "function", "name": "find", "description": description,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}
		for key, value := range extra {
			tool[key] = value
		}
		return map[string]any{"type": "namespace", "name": "mcp__docs", "tools": []any{tool}}
	}
	call := h21TransportItem("rv_search", "mcp__docs.find", map[string]any{"q": "x"})
	chunks := []string{s2Created(), h21ItemEvent("response.output_item.done", 0, call), h21Terminal("response.completed", call)}
	for _, c := range []struct {
		name     string
		first    map[string]any
		second   map[string]any
		failure  string
		counted  uint64
		explicit bool
	}{
		{name: "description", first: find("Find v1.", nil), second: find("Find v2.", nil), counted: 1},
		{name: "defer_loading", first: find("Find.", nil), second: find("Find.", map[string]any{"defer_loading": true}), counted: 1},
		{name: "identical", first: find("Find.", nil), second: find("Find.", nil), counted: 0},
		{name: "contract", first: find("Find.", nil), second: find("Find.", map[string]any{"strict": true}), failure: "tool_not_in_catalog"},
		{name: "explicit", first: find("Find v1.", nil), second: find("Find v2.", nil), explicit: true, counted: 0},
	} {
		input := []any{toolSearchOutput("ts_rv_1", c.first), toolSearchOutput("ts_rv_2", c.second), s5User("x")}
		if c.explicit {
			input = append([]any{map[string]any{"type": "additional_tools", "tools": []any{find("Find.", nil)}}}, input...)
		}
		request := s5Relay("h25-review-search-"+c.name, input...)
		before := relayCounterSnapshot()
		result := runStreamTest(t, request, chunks)
		if c.failure != "" {
			if !strings.Contains(result.closeErr, c.failure) {
				t.Errorf("%s: close %q, want %s", c.name, result.closeErr, c.failure)
			}
		} else if result.closeErr != "" || !strings.Contains(result.joined(), `"namespace":"mcp__docs"`) {
			t.Errorf("%s: close %q", c.name, result.closeErr)
		}
		if got := reviewCounterDelta(before, "search_duplicate_merged")["search_duplicate_merged"]; got != c.counted {
			t.Errorf("%s: search_duplicate_merged +%d, want +%d", c.name, got, c.counted)
		}
		if merged := searchDuplicateMerged(request, "mcp__docs.find"); merged != (c.counted == 1) {
			t.Errorf("%s: searchDuplicateMerged = %v", c.name, merged)
		}
	}
}

// ---------------------------------------------------------------- R5

// replay_scope:* is counted with relay_requests, once the request is
// admitted: a request turned away at the concurrency gate counts neither.
func TestH25ReviewReplayScopeCountedOnAdmission(t *testing.T) {
	service := NewService()
	cfg := service.config()
	cfg.MaxConcurrencyPerCredential = 1
	cfg.DataDir = ""
	service.mu.Lock()
	service.cfg = cfg
	service.mu.Unlock()
	service.SetHost(func(method string, payload, out any) error { return nil })
	release, admitted := service.gate.acquire("h25-review-gate", 1)
	if !admitted {
		t.Fatal("fixture: gate")
	}
	body := s5Relay("h25-review-r5", s5User("x"))
	raw, _ := json.Marshal(map[string]any{"OriginalRequest": jsonBytes(body), "Payload": jsonBytes(body), "StorageJSON": streamTestCredential(),
		"Model": "gpt-6-astra", "AuthID": "h25-review-gate", "AuthProvider": Provider, "Metadata": map[string]any{"caller_scope": "caller"}})
	keys := []string{"replay_scope:prompt_cache_key", "replay_scope_caller:present", "relay_requests:legacy:relay"}
	before := relayCounterSnapshot()
	if _, err := service.Handle("executor.execute", raw); err == nil {
		t.Fatal("fixture: the request was admitted")
	}
	if delta := reviewCounterDelta(before, keys...); delta["replay_scope:prompt_cache_key"] != 0 || delta["replay_scope_caller:present"] != 0 || delta["relay_requests:legacy:relay"] != 0 {
		t.Fatalf("refused request counted: %v", delta)
	}
	release()
	// Admitted (streaming, through the stream harness): counted once each.
	before = relayCounterSnapshot()
	if result := runStreamTest(t, body, s5Stream("r_rv_r5").text("ok").completed()); result.closeErr != "" {
		t.Fatal(result.closeErr)
	}
	if delta := reviewCounterDelta(before, keys...); delta["replay_scope:prompt_cache_key"] != 1 || delta["replay_scope_caller:present"] != 1 || delta["relay_requests:legacy:relay"] != 1 {
		t.Fatalf("admitted request: %v", delta)
	}
}
