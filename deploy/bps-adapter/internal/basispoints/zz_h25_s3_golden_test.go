package basispoints

// H25 S3: the default configuration against h22's own outputs.
//
// testdata/h22_s3_golden.json was recorded in the h22-live tree by
// testdata/h22_s3_golden_gen.go.txt from the scenarios in
// zz_h25_s3_fixtures_test.go (compiled there unchanged): for every turn of
// every multi-turn conversation, the exact upstream body, the outcome and the
// tool calls the client received.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type s3Golden struct {
	Generator string                    `json:"generator"`
	Scenarios map[string][]s3TurnResult `json:"scenarios"`
}

func loadS3Golden(t *testing.T) s3Golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/h22_s3_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden s3Golden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(golden.Generator, "h22-live 0.1.2-h22") || len(golden.Scenarios) == 0 {
		t.Fatalf("golden is not h22's: %q", golden.Generator)
	}
	return golden
}

// s3ResetCaches empties every replay cache, so a scenario sees exactly the
// state its own turns build (the golden was recorded on fresh scopes).
func s3ResetCaches() {
	for _, pool := range []*replayPool{nativeCallCache, nativeOriginCache} {
		pool.Lock()
		pool.items = map[string]nativeCallEntry{}
		pool.order = nil
		pool.bytes = 0
		pool.Unlock()
	}
	namespaceCache.Lock()
	namespaceCache.items = map[string]map[string]string{}
	namespaceCache.order = nil
	namespaceCache.Unlock()
}

// s3Differences compares one scenario's turns with h22's.
func s3Differences(got, want []s3TurnResult) []string {
	var out []string
	if len(got) != len(want) {
		return []string{"turn count"}
	}
	for index := range want {
		g, w := got[index], want[index]
		if g.Upstream != w.Upstream {
			out = append(out, "upstream body of turn "+string(rune('1'+index)))
		}
		if g.Status != w.Status || g.Kind != w.Kind || g.CloseErr != w.CloseErr {
			out = append(out, "outcome of turn "+string(rune('1'+index)))
		}
		if string(jsonBytes(g.Delivered)) != string(jsonBytes(w.Delivered)) {
			out = append(out, "delivered calls of turn "+string(rune('1'+index)))
		}
	}
	return out
}

// I1 for history (S3): with the default configuration -- and with every
// switch written out at its default, including relay_protocol: legacy (I3) --
// every upstream body of every turn is h22's, byte for byte, and the client
// receives the same calls. The scenarios cover warm replay after the client
// rewrote its copy in nine ways, every scope source (prompt_cache_key,
// client_metadata session, first item, with and without caller_scope, a
// credential switch), custom and namespaced relay tools, compaction,
// tool-less and tool_choice:none turns, output ordering, empty/structured/
// opaque results, a call_id delivered twice, a leaked transport call, cold
// history in every unverifiable shape, Lite namespace memory and precedence,
// non-stream delivery and the production identity profiles.
func TestH25S3DefaultHistoryIsH22Bytes(t *testing.T) {
	golden := loadS3Golden(t)
	scenarios := s3Scenarios()
	if len(scenarios) != len(golden.Scenarios) {
		t.Fatalf("scenarios %d, golden %d", len(scenarios), len(golden.Scenarios))
	}
	turns := 0
	for _, configure := range []func(*Config){nil, s2ExplicitDefaults} {
		for _, scenario := range scenarios {
			want, ok := golden.Scenarios[scenario.Name]
			if !ok {
				t.Fatalf("%s: no golden", scenario.Name)
			}
			s3ResetCaches()
			got := s3RunScenario(t, scenario, configure)
			if diffs := s3Differences(got, want); len(diffs) > 0 {
				for index := range want {
					if got[index].Upstream != want[index].Upstream {
						t.Logf("%s turn %d\n got: %s\nwant: %s", scenario.Name, index+1, got[index].Upstream, want[index].Upstream)
					}
				}
				t.Errorf("%s: differs from h22: %v", scenario.Name, diffs)
			}
			turns += len(scenario.Turns)
		}
	}
	if turns < 150 {
		t.Fatalf("only %d turns compared", turns)
	}
}

// The golden is sensitive: rc2's history translation (history_conflict:
// reject) differs from h22 exactly where rc2 was found to differ.
func TestH25S3GoldenSeparatesRC2History(t *testing.T) {
	golden := loadS3Golden(t)
	differs := map[string]bool{}
	for _, scenario := range s3Scenarios() {
		s3ResetCaches()
		got := s3RunScenario(t, scenario, s3Reject)
		if len(s3Differences(got, golden.Scenarios[scenario.Name])) > 0 {
			differs[scenario.Name] = true
		}
	}
	for _, name := range []string{
		// A relay call the client rewrote is sent upstream as the client's copy
		// (cold wrapper) instead of the model's own transport call.
		"relay_fn_key_added_by_client", "relay_fn_user_edit", "relay_custom_trailing_newline_stripped",
		// Placeholders and output pairing follow rc2's rules.
		"relay_outputs_empty_structured_opaque",
		// rc2 does not wrap a namespaced call under a declared top-level name.
		"relay_cold_history_shapes",
	} {
		if !differs[name] {
			t.Errorf("%s: rc2 history matched h22; the golden would not catch a regression", name)
		}
	}
	for _, name := range []string{"relay_fn_exact", "scope_first_item", "scope_credential_switch", "relay_namespace_exact"} {
		if differs[name] {
			t.Errorf("%s: an unchanged echo replays the same under both modes", name)
		}
	}
}
