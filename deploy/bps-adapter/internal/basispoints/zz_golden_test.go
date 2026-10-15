package basispoints

// Golden test for the turnState fix.
//
// turnState's fingerprint keys the upstream prompt cache (~85% hit rate in
// production), so the fix must not change the value it returns for ANY input
// that does not panic today. This test records the fingerprint for a corpus of
// shapes into testdata/turnstate.golden and fails if a later run disagrees.
//
// Run before the patch with -args -record to write the golden file, then again
// after the patch to compare.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

var record = flag.Bool("record", false, "write the golden file instead of comparing")

const goldenPath = "testdata/turnstate.golden"

// corpus is every input shape we care about. Shapes that panic today are
// marked panics=true and are skipped while recording.
var corpus = []struct {
	name   string
	input  string // JSON for the `input` field
	panics bool
}{
	{"empty-array", `[]`, true},
	{"null", `null`, false},
	{"missing", ``, false}, // nil
	{"string", `"just text"`, false},
	{"number", `42`, false},
	{"object", `{"role":"user"}`, false},
	{"one-user", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`, false},
	{"user-then-assistant", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo"}]}]`, false},
	{"two-users", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"b"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"c"}]}]`, false},
	{"no-user-at-all", `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"b"}]}]`, false},
	{"no-user-two-items", `[{"type":"reasoning","encrypted_content":"x"},{"type":"message","role":"assistant","content":[]}]`, false},
	{"user-then-tool-outputs", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"r"},{"type":"custom_tool_call_output","call_id":"c2","output":"r2"}]`, false},
	{"role-uppercase", `[{"type":"message","ROLE":"user"},{"type":"message","role":"USER"}]`, false},
	{"role-not-string", `[{"type":"message","role":123}]`, false},
	{"items-with-null", `[null,{"type":"message","role":"user"},null]`, false},
	{"items-non-object", `["a",7,true,{"type":"message","role":"user"}]`, false},
	{"nested-empty-content", `[{"type":"message","role":"user","content":[]}]`, false},
	{"unicode", `[{"type":"message","role":"user","content":[{"type":"input_text","text":"你好 🌟 é"}]}]`, false},
	{"additional-tools-first", `[{"type":"additional_tools","tools":[{"type":"namespace","name":"web","tools":[{"name":"run"}]}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]}]`, false},
	{"compaction-only", `[{"type":"compaction_trigger"}]`, false},
}

func decodeInput(t *testing.T, raw string) any {
	t.Helper()
	if raw == "" {
		return nil
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("corpus entry is not valid JSON: %v", err)
	}
	return value
}

// callTurnState runs turnState and reports a panic instead of crashing.
func callTurnState(input any) (fingerprint, iteration, panicked string) {
	defer func() {
		if r := recover(); r != nil {
			panicked = fmt.Sprint(r)
		}
	}()
	fingerprint, iteration = turnState(input)
	return
}

func TestTurnStateGolden(t *testing.T) {
	got := map[string]string{}
	for _, c := range corpus {
		fp, iter, panicked := callTurnState(decodeInput(t, c.input))
		if panicked != "" {
			got[c.name] = "PANIC: " + panicked
			continue
		}
		got[c.name] = fp + " iter=" + iter
	}

	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s\t%s\n", n, got[n])
	}

	if *record {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("recorded %d entries to %s", len(got), goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden file missing; run with -record first: %v", err)
	}
	wantLines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(want)), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			wantLines[parts[0]] = parts[1]
		}
	}
	for _, n := range names {
		w, ok := wantLines[n]
		if !ok {
			t.Errorf("%s: no golden entry", n)
			continue
		}
		g := got[n]
		if strings.HasPrefix(w, "PANIC:") {
			// The whole point of the fix: what used to panic must now succeed.
			if strings.HasPrefix(g, "PANIC:") {
				t.Errorf("%s: still panics: %s", n, g)
			} else {
				t.Logf("%s: FIXED (was %q, now %q)", n, w, g)
			}
			continue
		}
		if g != w {
			t.Errorf("%s: fingerprint CHANGED\n  before: %s\n  after:  %s", n, w, g)
		}
	}
}

// TestPrepareRequestEmptyInput drives the real entry point with the exact body
// shape that took the pool down on 2026-09-28.
func TestPrepareRequestEmptyInput(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5.6-sol","input":[]}`,
		`{"model":"gpt-5.6-sol","input":[],"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}],"tool_choice":"auto"}`,
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("prepareRequest panicked on %s: %v", body, r)
				}
			}()
			s := NewService()
			_, _, err := s.prepareRequest(ExecutorRequest{
				OriginalRequest: []byte(body),
				StorageJSON:     fakeCredential(t),
				AuthMetadata:    map[string]any{"file_name": "probe.json"},
			})
			status := 0
			if err != nil {
				if coded, ok := err.(interface{ StatusCode() int }); ok {
					status = coded.StatusCode()
				}
			}
			t.Logf("body=%s -> err=%v status=%d", body, err, status)
			if err == nil {
				t.Logf("  (no error: the request would be forwarded upstream)")
			} else if status < 400 || status >= 500 {
				t.Errorf("  empty input must be reported as a 4xx request fault, got status %d", status)
			}
		}()
	}
}

// fakeCredential builds a syntactically valid, entirely invented credential.
func fakeCredential(t *testing.T) []byte {
	t.Helper()
	header := b64(`{"alg":"none","typ":"JWT"}`)
	payload := b64(`{"https://api.openai.com/auth":{"chatgpt_account_id":"00000000-0000-0000-0000-000000000000","chatgpt_plan_type":"pro"},"exp":4102444800}`)
	token := header + "." + payload + ".sig"
	out, _ := json.Marshal(map[string]any{
		"access_token": token,
		"account_id":   "00000000-0000-0000-0000-000000000000",
		"email":        "probe@example.invalid",
		"type":         "oai-basispoints",
	})
	return out
}

func b64(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	data := []byte(s)
	var out strings.Builder
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], data[i:])
		out.WriteByte(alphabet[chunk[0]>>2])
		out.WriteByte(alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			out.WriteByte(alphabet[(chunk[1]&0x0f)<<2|chunk[2]>>6])
		}
		if n > 2 {
			out.WriteByte(alphabet[chunk[2]&0x3f])
		}
	}
	return out.String()
}

// TestPrepareRequestInputShapes checks that only genuinely empty turns are
// refused, and that ordinary requests still go through untouched.
func TestPrepareRequestInputShapes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int // 0 = must be accepted
	}{
		{"empty-array", `{"model":"gpt-5.6-sol","input":[]}`, 400},
		{"empty-array-with-tools", `{"model":"gpt-5.6-sol","input":[],"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}],"tool_choice":"auto"}`, 400},
		{"missing-input", `{"model":"gpt-5.6-sol"}`, 400},
		{"null-input", `{"model":"gpt-5.6-sol","input":null}`, 400},
		{"blank-string", `{"model":"gpt-5.6-sol","input":"   "}`, 400},
		{"string-input", `{"model":"gpt-5.6-sol","input":"hello"}`, 0},
		{"one-message", `{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, 0},
		{"reasoning-only", `{"model":"gpt-5.6-sol","input":[{"type":"reasoning","encrypted_content":"x"}]}`, 0},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panicked: %v", c.name, r)
				}
			}()
			s := NewService()
			_, _, err := s.prepareRequest(ExecutorRequest{
				OriginalRequest: []byte(c.body),
				StorageJSON:     fakeCredential(t),
				AuthMetadata:    map[string]any{"file_name": "probe.json"},
			})
			status := 0
			if err != nil {
				if coded, ok := err.(interface{ StatusCode() int }); ok {
					status = coded.StatusCode()
				} else {
					status = -1
				}
			}
			if status != c.wantCode {
				t.Errorf("%s: want status %d, got %d (err=%v)", c.name, c.wantCode, status, err)
			}
		}()
	}
}
