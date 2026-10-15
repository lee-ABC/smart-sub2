//go:build bpsexp

package basispoints

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the three independent audits of the stage-0 experiment
// build (2026-10-09). Each test is named after the finding it closes.

// ------------------------------------------------------------------- iso-11
// Stripping the experiment fields used to hang off `armed`. That only held
// because an arm override (identityoff) can itself only be applied while
// armed, so "disarmed" and "mode: off" were mutually exclusive by accident.
// Disarmed + client_identity.mode: off would have serialised
// metadata.bpsexp_arm straight to the upstream.
func TestAuditIso11StripIsUnconditional(t *testing.T) {
	expReset() // armed stays false
	for _, mode := range []string{"off", "thread", "hygiene"} {
		t.Run(mode, func(t *testing.T) {
			req := ExecutorRequest{
				HostCallbackID: "cb-strip-" + mode,
				Headers:        map[string][]string{expArmHeader: {"A1"}},
				Metadata:       map[string]any{expArmMetaKey: "A1", "caller_scope": "keep"},
			}
			source := map[string]any{
				"metadata": map[string]any{
					expArmMetaKey:  "A1",
					expTrialMetaID: "trial-strip",
					"task_id":      "keep-me",
				},
				"input": []any{s5User("x")},
			}
			cfg := defaultConfig()
			cfg.ClientIdentity.Mode = mode
			expApplyArm(nil, &req, source, &cfg)

			meta := objectValue(source["metadata"])
			for _, key := range []string{expArmMetaKey, expTrialMetaID} {
				if _, present := meta[key]; present {
					t.Fatalf("body metadata still carries %s while disarmed (mode=%s)", key, mode)
				}
			}
			if meta["task_id"] != "keep-me" {
				t.Fatal("the strip removed a non-experiment metadata key")
			}
			for name := range req.Headers {
				if strings.EqualFold(name, expArmHeader) {
					t.Fatal("the arm header survived while disarmed")
				}
			}
			if _, present := req.Metadata[expArmMetaKey]; present {
				t.Fatal("CPA metadata still carries bpsexp_arm (EXP-BUILD s.3 says it is removed)")
			}
			if req.Metadata["caller_scope"] != "keep" {
				t.Fatal("the strip removed an unrelated CPA metadata key")
			}
		})
	}
}

// ------------------------------------------------------------------- drv-F6-1
// A streaming request that fails BEFORE the stream opens returned without
// reaching the relay goroutine, so expRecordStreamEnd never ran: no fixture at
// all, and the captured request body leaked in the registry. Stage 1 exists to
// collect exactly this case (a promoted body refused with 400).
func TestAuditF6PreStreamFailureStillWritesFixture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	h := newExpHarness(t, "stage-f6a", "experiment_record_dir: "+filepath.ToSlash(dir)+"\n")

	req := a1Relay("exp-f6a", s5User("go"))
	req["metadata"] = map[string]any{"bpsexp_arm": "A0", "bpsexp_trial": "trial-f6a"}
	h.run(req, true, a1Always(a1Reply{
		status: 400,
		body:   `{"error":{"code":"invalid_request_error","message":"unsupported tool schema"}}`,
	}))

	entries, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(entries) != 1 {
		t.Fatalf("expected one fixture for the pre-stream failure, got %v", entries)
	}
	raw, _ := os.ReadFile(entries[0])
	var fx struct {
		Req    map[string]any `json:"req"`
		Bpsexp map[string]any `json:"bpsexp"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.Req == nil || fx.Req["input"] == nil {
		t.Fatal("fixture carries no request doc")
	}
	outcome, _ := fx.Bpsexp["outcome"].(string)
	if outcome == "ok" || outcome == "" {
		t.Fatalf("outcome = %q, want an error outcome", outcome)
	}
	// and nothing is left behind in the registry
	expCapture.Lock()
	left := len(expCapture.byKey)
	expCapture.Unlock()
	if left != 0 {
		t.Fatalf("%d capture(s) leaked after the failure", left)
	}
}

// ------------------------------------------------------------------- drv-F6-2
// expRecordNonStreamSSE used to finalise with a hard-coded nil error, so every
// failed NON-streaming request -- the A2 shadow requests above all -- was
// recorded with outcome "ok" and an empty event list, which would have
// inflated the shadow rescue rate.
func TestAuditF6NonStreamFailureRecordsTheError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	h := newExpHarness(t, "stage-f6b", "experiment_record_dir: "+filepath.ToSlash(dir)+"\n")

	req := a1Relay("exp-f6b", s5User("go"))
	req["metadata"] = map[string]any{"bpsexp_arm": "A0", "bpsexp_trial": "trial-f6b"}
	h.run(req, false, a1Always(a1Reply{
		status: 400,
		body:   `{"error":{"code":"invalid_request_error","message":"nope"}}`,
	}))

	entries, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(entries) != 1 {
		t.Fatalf("expected one fixture, got %v", entries)
	}
	raw, _ := os.ReadFile(entries[0])
	var fx struct {
		Bpsexp map[string]any `json:"bpsexp"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	outcome, _ := fx.Bpsexp["outcome"].(string)
	if !strings.HasPrefix(outcome, "error:") && !strings.HasPrefix(outcome, "tool_reject:") {
		t.Fatalf("outcome = %q, want the real upstream error (it used to be \"ok\")", outcome)
	}
}

// ------------------------------------------------------------------- drv-F6-3
// The second attempt on one trial (the A1 fallback's relay body) used to
// overwrite the first (the promoted body the upstream refused), so the fixture
// said arm A1 while holding the relay body and any reconciliation against
// capture.req drew the wrong conclusion.
func TestAuditF6FallbackKeepsTheRefusedBody(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	expReset()
	trial := &expTrial{id: "trial-f6c", arm: "A1", model: "gpt-6-sol"}
	expState.Lock()
	expState.armed, expState.recordDir = true, dir
	expState.Unlock()
	req := ExecutorRequest{StreamID: "s-f6c"}
	expPutTrial(expTrialKey(req), trial)

	first := []byte(`{"model":"gpt-6-sol","additional_tools":{"functions":[{"name":"exec_command"}]}}`)
	second := []byte(`{"model":"gpt-6-sol","tools":[{"name":"run_officejs"}]}`)
	expRecordRequestBody(req, first, nil, true)
	expRecordRequestBody(req, second, nil, true)
	expRecordStreamEnd(req, nil, nil)

	entries, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(entries) != 1 {
		t.Fatalf("expected one fixture, got %v", entries)
	}
	raw, _ := os.ReadFile(entries[0])
	var fx struct {
		Req      map[string]any   `json:"req"`
		Attempts []map[string]any `json:"req_attempts"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if _, present := fx.Req["tools"]; !present {
		t.Fatal("req should be the LAST body sent (the relay fallback)")
	}
	if len(fx.Attempts) != 1 {
		t.Fatalf("req_attempts = %d, want the one refused promoted body", len(fx.Attempts))
	}
	if _, present := fx.Attempts[0]["additional_tools"]; !present {
		t.Fatalf("the refused promoted body was not kept: %v", fx.Attempts[0])
	}
}

// ------------------------------------------------------------------- drv-F6-4
// An unfinalised capture used to sit in the registry for the life of the
// process, holding a whole parsed request body.
func TestAuditF6CaptureRegistryIsSwept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	expReset()
	expState.Lock()
	expState.armed, expState.recordDir = true, dir
	expState.Unlock()

	// a stale partial that will never be finalised
	expCapture.Lock()
	expCapture.byKey["s:abandoned"] = &expPartial{
		id: "trial-stale", startedAt: time.Now().Add(-2 * expCaptureTTL),
	}
	expCapture.Unlock()

	// any subsequent request sweeps it
	req := ExecutorRequest{StreamID: "s-live"}
	expPutTrial(expTrialKey(req), &expTrial{id: "trial-live", arm: "A0"})
	expRecordRequestBody(req, []byte(`{"model":"gpt-6-sol"}`), nil, true)

	expCapture.Lock()
	_, stale := expCapture.byKey["s:abandoned"]
	_, live := expCapture.byKey["s:s-live"]
	expCapture.Unlock()
	if stale {
		t.Fatal("the abandoned capture was not swept")
	}
	if !live {
		t.Fatal("the sweep removed the live capture")
	}
}
