//go:build bpsexp

package basispoints

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Tests for the experiment build (-tags bpsexp). They run only under that tag;
// the production build's behaviour is covered by the whole untagged suite,
// which also runs under this tag with the experiment disarmed (that is the
// "disarmed == production" property, exercised by every other test file here).

// expReset clears the process-wide experiment state between tests.
func expReset() {
	expState.Lock()
	expState.armed, expState.fault, expState.faultPending = false, "", false
	expState.stageID, expState.recordDir, expState.defaultArm = "", "", ""
	expState.authID, expState.seenAuth, expState.shadow, expState.scrub = "", "", false, nil
	expState.Unlock()
	expTrials.Lock()
	expTrials.byKey = map[string]*expTrial{}
	expTrials.Unlock()
	expCapture.Lock()
	expCapture.byKey = map[string]*expPartial{}
	expCapture.Unlock()
	expShadowQueue.Lock()
	expShadowQueue.tasks = nil
	expShadowQueue.Unlock()
}

// newExpHarness is newA1Harness with the experiment configured and armed.
func newExpHarness(t *testing.T, stage, extraYAML string) *a1Harness {
	t.Helper()
	t.Setenv(expStageEnv, stage)
	expReset()
	h := &a1Harness{t: t, service: NewService()}
	h.service.SetHost(h.host)
	config := "data_dir: \"" + filepath.ToSlash(t.TempDir()) + "\"\nquota_sample_seconds: 0\n" +
		"client_identity:\n  pepper: \"" + s5Pepper + "\"\n" +
		"experiment_stage_id: " + stage + "\n" + extraYAML
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(config)})
	if _, err := h.service.Handle("plugin.register", raw); err != nil {
		t.Fatal(err)
	}
	if !expIsArmed() {
		t.Fatalf("experiment did not arm: %q", expStatus()["fault"])
	}
	return h
}

// ---------------------------------------------------------------- self-check

func TestExpSelfCheckArmsOnlyWhenSafe(t *testing.T) {
	// register returns whether the experiment armed and, when it did not, the
	// fault reason relay-status reports (also pending to be logged).
	register := func(t *testing.T, env string, yaml string) (bool, string, bool) {
		t.Helper()
		t.Setenv(expStageEnv, env)
		expReset()
		service := NewService()
		service.SetHost(func(string, any, any) error { return nil })
		cfg := "data_dir: \"" + filepath.ToSlash(t.TempDir()) + "\"\nquota_sample_seconds: 0\n" + yaml
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(cfg)})
		if _, err := service.Handle("plugin.register", raw); err != nil {
			t.Fatal(err)
		}
		expState.RLock()
		armed, fault, pending := expState.armed, expState.fault, expState.faultPending
		expState.RUnlock()
		return armed, fault, pending
	}

	t.Run("matches", func(t *testing.T) {
		armed, fault, pending := register(t, "stage-ok", "experiment_stage_id: stage-ok\n")
		if !armed || fault != "" || pending {
			t.Fatalf("armed=%v fault=%q pending=%v", armed, fault, pending)
		}
	})
	t.Run("mismatch is loud", func(t *testing.T) {
		armed, fault, pending := register(t, "stage-ok", "experiment_stage_id: stage-wrong\n")
		if armed || !strings.Contains(fault, "does not match") || !pending {
			t.Fatalf("armed=%v fault=%q pending=%v", armed, fault, pending)
		}
	})
	t.Run("public_base_url is refused", func(t *testing.T) {
		armed, fault, pending := register(t, "stage-ok", "experiment_stage_id: stage-ok\npublic_base_url: https://x.example/bp-img\n")
		if armed || !strings.Contains(fault, "public_base_url") || !pending {
			t.Fatalf("armed=%v fault=%q pending=%v", armed, fault, pending)
		}
	})
	t.Run("no experiment config is silent production", func(t *testing.T) {
		armed, fault, pending := register(t, "", "")
		if armed || fault != "" || pending {
			t.Fatalf("a plain build should be disarmed and silent: armed=%v fault=%q pending=%v", armed, fault, pending)
		}
	})
	t.Run("env missing is loud", func(t *testing.T) {
		armed, fault, pending := register(t, "", "experiment_stage_id: stage-ok\n")
		if armed || !strings.Contains(fault, expStageEnv) || !pending {
			t.Fatalf("armed=%v fault=%q pending=%v", armed, fault, pending)
		}
	})
}

// TestExpFaultLoggedOnRequest proves the disarm is loud: a refused self-check
// logs an error on the next request that actually reaches the arm hook.
func TestExpFaultLoggedOnRequest(t *testing.T) {
	t.Setenv(expStageEnv, "stage-ok")
	expReset()
	h := &a1Harness{t: t, service: NewService()}
	h.service.SetHost(h.host)
	cfg := "data_dir: \"" + filepath.ToSlash(t.TempDir()) + "\"\nquota_sample_seconds: 0\n" +
		"client_identity:\n  pepper: \"" + s5Pepper + "\"\nexperiment_stage_id: stage-wrong\n"
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(cfg)})
	if _, err := h.service.Handle("plugin.register", raw); err != nil {
		t.Fatal(err)
	}
	if expIsArmed() {
		t.Fatal("experiment armed with a mismatched stage id")
	}
	h.run(a1Relay("exp-fault", s5User("x")), false, a1Always(a1Text("resp-fault", "ok")))
	logged := false
	h.mu.Lock()
	for _, entry := range h.logs {
		if entry["level"] == "error" && strings.Contains(stringValue(entry["message"]), "BPSEXP experiment disarmed") {
			logged = true
		}
	}
	h.mu.Unlock()
	if !logged {
		t.Fatal("a disarmed experiment did not log a loud fault on a request")
	}
}

func TestExpSecondCredentialTrips(t *testing.T) {
	h := newExpHarness(t, "stage-c", "experiment_record_dir: \"\"\n")
	// First credential: fine.
	req := &ExecutorRequest{AuthID: "cred-a1", HostCallbackID: "cb-1"}
	cfg := h.service.config()
	source := map[string]any{"model": "gpt-6-astra", "input": []any{s5User("x")}}
	expApplyArm(h.service, req, source, &cfg)
	if !expIsArmed() {
		t.Fatal("first credential disarmed the experiment")
	}
	// A second distinct credential trips the whole run.
	req2 := &ExecutorRequest{AuthID: "cred-b2", HostCallbackID: "cb-2"}
	expApplyArm(h.service, req2, source, &cfg)
	if expIsArmed() {
		t.Fatal("a second credential did not disarm the experiment")
	}
	if fault := stringValue(expStatus()["fault"]); !strings.Contains(fault, "more than one credential") {
		t.Fatalf("fault=%q", fault)
	}
}

// ---------------------------------------------------------------- arm parsing

func TestExpArmOverrides(t *testing.T) {
	cases := []struct {
		arm   string
		check func(*testing.T, Config, expArmSpec)
	}{
		{"A0", func(t *testing.T, c Config, _ expArmSpec) {
			if c.RelayNativePromotion != a1ModeOff || c.RelayToolContracts != relayToolContractsSummary {
				t.Fatal("A0 changed a switch")
			}
		}},
		{"A1", func(t *testing.T, c Config, _ expArmSpec) {
			if c.RelayNativePromotion != a1ModeOn {
				t.Fatal("A1 did not turn promotion on")
			}
		}},
		{"A1+H2", func(t *testing.T, c Config, _ expArmSpec) {
			if c.RelayNativePromotion != a1ModeOn || c.RelayNativePromotionHistory != a1HistoryH2 {
				t.Fatal("A1+H2 wrong")
			}
		}},
		{"B3", func(t *testing.T, c Config, _ expArmSpec) {
			if c.RelayToolContracts != relayToolContractsFull {
				t.Fatal("B3 did not set full contracts")
			}
		}},
		{"A1+blocknone", func(t *testing.T, c Config, _ expArmSpec) {
			if c.RelayNativePromotionBlock != a1BlockNone {
				t.Fatal("blocknone not applied")
			}
		}},
		{"A1+hygiene+S", func(t *testing.T, _ Config, spec expArmSpec) {
			if !spec.modeSet || spec.mode != identityHygiene || !spec.switchS {
				t.Fatalf("hygiene/S not resolved: %+v", spec)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.arm, func(t *testing.T) {
			spec, ok := expArmOverrides(tc.arm)
			if !ok {
				t.Fatalf("arm %q not recognised", tc.arm)
			}
			cfg := defaultConfig()
			spec.applyTo(&cfg)
			tc.check(t, cfg, spec)
		})
	}
	if _, ok := expArmOverrides("A1+notathing"); ok {
		t.Fatal("an unknown token was accepted")
	}
}

func TestExpReadArmStripsEverywhere(t *testing.T) {
	expReset()
	expState.Lock()
	expState.armed = true
	expState.Unlock()
	req := &ExecutorRequest{
		Headers:  http.Header{"X-Bpsexp-Arm": []string{"B3"}, "Authorization": []string{"Bearer secret"}},
		Metadata: map[string]any{},
	}
	source := map[string]any{"metadata": map[string]any{"bpsexp_arm": "A1", "task_id": "keep", "bpsexp_trial": "t-7"}}
	// The header wins over the body field; both are stripped.
	if arm := expReadArm(req, source); arm != "B3" {
		t.Fatalf("arm = %q, want B3", arm)
	}
	if _, present := req.Headers["X-Bpsexp-Arm"]; present {
		t.Fatal("arm header was not stripped")
	}
	if req.Headers.Get("Authorization") == "" {
		t.Fatal("stripping removed an unrelated header")
	}
	meta := objectValue(source["metadata"])
	if _, present := meta["bpsexp_arm"]; present {
		t.Fatal("arm body field was not stripped")
	}
	if _, present := meta["bpsexp_trial"]; present {
		t.Fatal("trial body field was not stripped")
	}
	if meta["task_id"] != "keep" {
		t.Fatal("stripping removed an unrelated metadata key")
	}
}

func TestExpArmAppliesEndToEndAndNeverLeaks(t *testing.T) {
	h := newExpHarness(t, "stage-arm", "")

	// A1: the request goes upstream promoted (an additional_tools item, no
	// top-level tools) and the arm field never appears in the upstream body.
	req := a1Relay("exp-arm-a1", s5User("hello"))
	req["metadata"] = map[string]any{"bpsexp_arm": "A1"}
	result := h.run(req, false, a1Always(a1Text("resp-arm-a1", "ok")))
	if result.status != 0 && result.status != 200 {
		t.Fatalf("A1 request failed: %d %s", result.status, result.message)
	}
	if len(result.bodies) == 0 {
		t.Fatal("no upstream body")
	}
	items := a1BodyInput(t, result.bodies[0]) // also asserts no top-level tools
	if idx, _ := a1FindItem(items, "additional_tools"); idx < 0 {
		t.Fatal("A1 arm did not promote the request")
	}
	if strings.Contains(result.bodies[0], "bpsexp_arm") || strings.Contains(result.bodies[0], expArmMetaKey) {
		t.Fatal("the arm field leaked into the upstream body")
	}

	// A bad arm runs as A0 (no additional_tools item) and is counted.
	before := relayCounterSnapshot()
	bad := a1Relay("exp-arm-bad", s5User("hi"))
	bad["metadata"] = map[string]any{"bpsexp_arm": "ZZZ"}
	result = h.run(bad, false, a1Always(a1Text("resp-arm-bad", "ok")))
	items = a1BodyInput(t, result.bodies[0])
	if idx, _ := a1FindItem(items, "additional_tools"); idx >= 0 {
		t.Fatal("a bad arm still promoted")
	}
	if a1CounterDelta(before)["bpsexp_bad_arm"] != 1 {
		t.Fatalf("bad arm not counted: %v", a1CounterDelta(before))
	}
}

// ---------------------------------------------------------------- capture

func TestExpCaptureWritesFixture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	h := newExpHarness(t, "stage-cap", "experiment_record_dir: "+filepath.ToSlash(dir)+"\nexperiment_scrub: Jennifer\n")

	req := a1Relay("exp-cap", s5User("go"))
	req["metadata"] = map[string]any{"bpsexp_arm": "A0", "bpsexp_trial": "trial-cap-1"}
	reply := a1Reply{chunks: s5Stream("resp-cap").text("Hi Jennifer, running C:\\Users").
		finish("response.completed", map[string]any{"model": "gpt-6-astra"})}
	h.run(req, false, a1Always(reply))

	entries, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(entries) != 1 {
		t.Fatalf("expected one fixture, got %v", entries)
	}
	info, err := os.Stat(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0600 {
			t.Fatalf("fixture mode = %v, want 0600", info.Mode().Perm())
		}
		if dinfo, err := os.Stat(dir); err == nil && dinfo.Mode().Perm() != 0700 {
			t.Fatalf("record dir mode = %v, want 0700", dinfo.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(entries[0])
	var fx struct {
		Req    map[string]any    `json:"req"`
		Hdr    map[string]string `json:"hdr"`
		Events [][]string        `json:"events"`
		Bpsexp map[string]any    `json:"bpsexp"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	if fx.Req == nil || fx.Req["input"] == nil {
		t.Fatal("fixture carries no request doc")
	}
	// Account header hashed, auth never stored.
	if hashed := fx.Hdr["chatgpt-account-id"]; !strings.HasPrefix(hashed, "h:") {
		t.Fatalf("account id not hashed: %q", hashed)
	}
	for name := range fx.Hdr {
		if name == "authorization" || name == "cookie" {
			t.Fatal("an auth header was stored")
		}
	}
	// Appendix C fields.
	if fx.Bpsexp["trial_id"] != "trial-cap-1" || fx.Bpsexp["arm"] != "A0" {
		t.Fatalf("bpsexp block = %v", fx.Bpsexp)
	}
	for _, key := range []string{"attempt", "task_id", "model", "actual_model", "outcome", "switches", "marker"} {
		if _, ok := fx.Bpsexp[key]; !ok {
			t.Fatalf("appendix-C field %q missing", key)
		}
	}
	if fx.Bpsexp["actual_model"] != "gpt-6-astra" {
		t.Fatalf("actual model = %v", fx.Bpsexp["actual_model"])
	}
	if fx.Bpsexp["marker"] != expMarker {
		t.Fatal("fixture marker missing")
	}
	// The scrubbed word must not survive anywhere in the fixture.
	if strings.Contains(string(raw), "Jennifer") {
		t.Fatal("the scrub word reached disk")
	}
}

// -------------------------------------------------- retries and re-routes

func TestExpRetryAndRerouteRecorded(t *testing.T) {
	h := newExpHarness(t, "stage-retry", "")

	before := relayCounterSnapshot()
	// A re-route to an abuse variant, refused with 404 every time.
	abuse := a1Reply{status: 404, body: `{"error":{"message":"The model ` + "`gpt-6-astra-degrade2-codexabuse-ev3`" +
		` does not exist or you do not have access to it.","type":"invalid_request_error"}}`}
	req := a1Relay("exp-reroute", s5User("x"))
	req["metadata"] = map[string]any{"bpsexp_arm": "A0"}
	h.run(req, false, a1Always(abuse))
	delta := a1CounterDelta(before)
	if delta["bpsexp_reroute:abuse"] == 0 {
		t.Fatalf("abuse re-route not recorded: %v", delta)
	}

	before = relayCounterSnapshot()
	limited := a1Reply{status: 429, body: `{"error":{"message":"Rate limit reached: 200000 tokens per min. Please try again in 50ms.","type":"rate_limit_exceeded"}}`}
	req = a1Relay("exp-429", s5User("x"))
	req["metadata"] = map[string]any{"bpsexp_arm": "A0"}
	h.run(req, false, a1Always(limited))
	if a1CounterDelta(before)["bpsexp_retry:rate_limit"] == 0 {
		t.Fatalf("429 not recorded: %v", a1CounterDelta(before))
	}
}

// ---------------------------------------------------- deterministic repair shadow

func TestExpRepairJSON(t *testing.T) {
	cases := []struct {
		name            string
		code            string
		ok, unique      bool
		schemaPasses    bool
		relayToolSource bool
	}{
		{"lone backslash, unique", `{"tool":"exec_command","args":{"cmd":"C:\Users"}}`, true, true, true, true},
		{"already valid", `{"tool":"exec_command","args":{"cmd":"ok"}}`, true, true, true, true},
		{"ambiguous tab escape", `{"tool":"exec_command","args":{"cmd":"a\tb\q"}}`, true, false, true, true},
		{"unrepairable embedded quotes", `{"tool":"exec_command","args":{"cmd":"echo "hi""}}`, false, false, false, true},
		{"not even close", `totally not json`, false, false, false, false},
	}
	source := withRelayOptions(s5RelayWith("k", s5RelayTools(), s5User("x")), defaultRelayOptions())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repaired, ok, unique := expRepairJSON(tc.code)
			if ok != tc.ok || (ok && unique != tc.unique) {
				t.Fatalf("repair(%q) = ok %v unique %v, want ok %v unique %v", tc.code, ok, unique, tc.ok, tc.unique)
			}
			if ok && tc.relayToolSource {
				if passes := expRepairPassesSchema(repaired, source); passes != tc.schemaPasses {
					t.Fatalf("schema passes = %v, want %v (repaired %q)", passes, tc.schemaPasses, repaired)
				}
			}
		})
	}
}

func TestExpRepairShadowCountsOnDecodeFailure(t *testing.T) {
	h := newExpHarness(t, "stage-repair", "")
	before := relayCounterSnapshot()
	// A run_officejs call whose inner code has a lone backslash: it fails to
	// decode (code_json) but a deterministic repair would fix it.
	broken := s5Transport("exp-repair-1", `{"tool":"exec_command","args":{"cmd":"C:\Users"}}`, []any{})
	req := a1Relay("exp-repair", s5User("x"))
	req["metadata"] = map[string]any{"bpsexp_arm": "A0"}
	result := h.run(req, true, a1Always(a1Reply{chunks: s5Stream("resp-repair").call(broken).completed()}))
	if !strings.Contains(result.closeErr, "invalid_tool_call") {
		t.Fatalf("expected an invalid_tool_call close, got %q", result.closeErr)
	}
	delta := a1CounterDelta(before)
	if delta["bpsexp_repair:attempted:code_json"] == 0 || delta["bpsexp_repair:repaired"] == 0 ||
		delta["bpsexp_repair:unique"] == 0 || delta["bpsexp_repair:passes_schema"] == 0 {
		t.Fatalf("repair shadow counters not set: %v", delta)
	}
}

// ---------------------------------------------------------------- switch S

func TestExpSwitchSOutboundPlaintext(t *testing.T) {
	expReset()
	expState.Lock()
	expState.armed = true
	expState.Unlock()
	req := ExecutorRequest{HostCallbackID: "cb-s"}
	expPutTrial(expTrialKey(req), &expTrial{id: "t-s", arm: "A0+S", switchS: true})

	body := map[string]any{"input": []any{
		map[string]any{"type": "function_call", "name": "collaboration.spawn_agent", "arguments": "{}"},
		map[string]any{"type": "function_call", "name": "exec_command", "arguments": "{}"},
	}}
	before := relayCounterSnapshot()
	expRewriteOutboundBody(req, body)
	items := body["input"].([]any)
	multi := objectValue(items[0])
	if _, ok := multi["encrypted_function_args"]; !ok {
		t.Fatal("switch S did not mark the multi-agent call")
	}
	plain := objectValue(items[1])
	if _, ok := plain["encrypted_function_args"]; ok {
		t.Fatal("switch S touched a non-multi-agent call")
	}
	if a1CounterDelta(before)["bpsexp_switch_s:applied"] != 1 {
		t.Fatalf("switch S not counted: %v", a1CounterDelta(before))
	}

	// Without the switch, nothing changes.
	expDropTrial(req)
	expPutTrial(expTrialKey(req), &expTrial{id: "t-s2", arm: "A0", switchS: false})
	body2 := map[string]any{"input": []any{
		map[string]any{"type": "function_call", "name": "collaboration.spawn_agent", "arguments": "{}"},
	}}
	expRewriteOutboundBody(req, body2)
	if _, ok := objectValue(body2["input"].([]any)[0])["encrypted_function_args"]; ok {
		t.Fatal("switch S fired without the arm")
	}
}

// ---------------------------------------------------------------- A2 shadow rewrite

func TestExpShadowRewriteFlush(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rec")
	h := newExpHarness(t, "stage-shadow", "experiment_shadow_rewrite: true\nexperiment_record_dir: "+filepath.ToSlash(dir)+"\n")

	// A relay (A0) request whose tool call cannot decode: the stream closes
	// with invalid_tool_call and the failure is queued for shadow rewrite.
	broken := s5Transport("exp-shadow-1", `{"tool":"exec_command","args":{"cmd":"echo "hi""}}`, []any{})
	req := a1Relay("exp-shadow", s5User("x"))
	req["metadata"] = map[string]any{"bpsexp_arm": "A0"}
	result := h.run(req, true, func(call int, _ string) a1Reply {
		if call == 0 {
			return a1Reply{chunks: s5Stream("resp-shadow").call(broken).completed()}
		}
		// The shadow resends answer plainly.
		return a1Text("resp-shadow-retry", "fixed")
	})
	if !strings.Contains(result.closeErr, "invalid_tool_call") {
		t.Fatalf("expected an invalid_tool_call close, got %q", result.closeErr)
	}
	emitsBefore := len(h.emits)

	expShadowQueue.Lock()
	queued := len(expShadowQueue.tasks)
	expShadowQueue.Unlock()
	if queued != 1 {
		t.Fatalf("shadow queue length = %d, want 1", queued)
	}

	bodiesBeforeFlush := len(h.bodies)
	before := relayCounterSnapshot()
	resp, ok := expManagementRoute(h.service, "/v0/resource/plugins/oai-basispoints/"+expShadowFlushPath, "POST", nil)
	if !ok {
		t.Fatal("flush route not served")
	}
	var summary struct {
		Processed    int `json:"processed"`
		Continuation int `json:"continuation"`
		Resample     int `json:"resample"`
	}
	_ = json.Unmarshal(resp["Body"].([]byte), &summary)
	if summary.Processed != 1 || summary.Continuation != 1 || summary.Resample != 1 {
		t.Fatalf("flush summary = %+v", summary)
	}
	// Two upstream calls were made (continuation + resample), 1:1.
	if len(h.bodies)-bodiesBeforeFlush != 2 {
		t.Fatalf("shadow issued %d upstream calls, want 2", len(h.bodies)-bodiesBeforeFlush)
	}
	// The client stream was never touched by the flush.
	if len(h.emits) != emitsBefore {
		t.Fatal("the shadow rewrite emitted to the client")
	}
	delta := a1CounterDelta(before)
	if delta["bpsexp_shadow:continuation"] != 1 || delta["bpsexp_shadow:resample"] != 1 {
		t.Fatalf("shadow counters = %v", delta)
	}
}

// --------------------------------------------- disarmed == production, marker

func TestExpDisarmedIsInert(t *testing.T) {
	expReset() // armed stays false
	req := ExecutorRequest{HostCallbackID: "cb-inert"}
	cfg := defaultConfig()
	snapshot := relayOptionsFromConfig(cfg)
	source := map[string]any{"metadata": map[string]any{"bpsexp_arm": "A1"}, "input": []any{s5User("x")}}
	expApplyArm(nil, &req, source, &cfg)
	if relayOptionsFromConfig(cfg) != snapshot {
		t.Fatal("expApplyArm changed cfg while disarmed")
	}
	// The experiment fields must be stripped even while disarmed: the strip is
	// unconditional defence in depth, not a side effect of the armed path
	// (audit iso-11).  Everything ELSE must still be inert.
	if _, present := objectValue(source["metadata"])["bpsexp_arm"]; present {
		t.Fatal("expApplyArm left bpsexp_arm in the body metadata while disarmed")
	}
	if got := expWithTrial(source, req); got[expTrialKeyOnSource] != nil {
		t.Fatal("expWithTrial marked source while disarmed")
	}
	if expArmMode(req, identityOff) != identityOff {
		t.Fatal("expArmMode changed the mode while disarmed")
	}
	status := expStatus()
	if status["armed"] != false || status["marker"] != expMarker {
		t.Fatalf("status = %v", status)
	}
}

func TestExpMarkerEmbedded(t *testing.T) {
	if expMarker != "BPSEXP-EXPERIMENT-DO-NOT-DEPLOY" {
		t.Fatalf("marker = %q", expMarker)
	}
	if expStatus()["marker"] != expMarker {
		t.Fatal("status omits the marker")
	}
}
