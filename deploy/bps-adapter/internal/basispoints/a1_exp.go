//go:build bpsexp

package basispoints

import (
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Experiment build (-tags bpsexp). PLAN-real-upstream-json-fix.md section 3.3,
// 4.2, 4.7 and appendix C. Everything here exists ONLY in this build; the
// production build (a1_exp_stub.go) has an empty, zero-cost copy of each hook,
// so the shipped .so behaves exactly as A1 does (EXP-BUILD.md). This file is
// never compiled into the deployable plugin.
//
// Nothing here runs until the anti-misuse self-check arms the experiment
// (expConfigure): the configuration must declare an experiment_stage_id equal
// to the BPSEXP_STAGE_ID environment variable, public_base_url must be empty,
// and only one credential may ever be seen. A build that is armed carries the
// marker below so a production postcheck can grep for its absence.

// expMarker is embedded in the binary of this build and surfaced in
// relay-status. The production build never defines it, so
//
//	grep -l BPSEXP-EXPERIMENT-DO-NOT-DEPLOY <the deployed .so>
//
// must find nothing (deploy/postcheck). Referenced from expStatus and the
// fault string so the linker always keeps it.
const expMarker = "BPSEXP-EXPERIMENT-DO-NOT-DEPLOY"

// expStageEnv is the environment variable the configured stage id must match.
const expStageEnv = "BPSEXP_STAGE_ID"

// expArmHeader is the per-request arm field. It is read from the request's own
// headers or its metadata and stripped before anything goes upstream.
const (
	expArmHeader   = "X-Bpsexp-Arm"
	expArmMetaKey  = "bpsexp_arm"
	expTrialMetaID = "bpsexp_trial"
)

// expState is the whole experiment's run-time state. It is process-wide
// because the self-check, the arm selection and the capture all have to agree
// on whether the experiment is armed and on the single credential in use.
var expState = struct {
	sync.RWMutex
	armed        bool
	fault        string // non-empty = disarmed because the self-check failed/tripped
	faultPending bool   // the fault still has to be logged (on the next request)
	stageID      string
	recordDir    string
	defaultArm   string
	authID       string // the single credential the operator declared (optional)
	seenAuth     string // the first credential actually seen
	shadow       bool   // A2 shadow rewrite enabled
	scrub        []string
}{}

// expConfig is the experiment's own config.yaml keys. They are read from the
// raw bytes here rather than added to Config, so the production struct and its
// golden tests are untouched.
type expConfig struct {
	StageID    string `yaml:"experiment_stage_id"`
	RecordDir  string `yaml:"experiment_record_dir"`
	DefaultArm string `yaml:"experiment_default_arm"`
	AuthID     string `yaml:"experiment_auth_id"`
	Shadow     bool   `yaml:"experiment_shadow_rewrite"`
	Scrub      string `yaml:"experiment_scrub"`
}

// expConfigure runs the anti-misuse self-check and arms or loudly disarms the
// experiment. A failure here does not fail registration (that would retire the
// provider); it disarms every experiment hook, so this build then behaves like
// the production one, and records a fault that the next request logs and that
// relay-status shows.
func expConfigure(_ *Service, raw []byte, cfg Config) {
	var ec expConfig
	if len(raw) > 0 {
		_ = yaml.Unmarshal(raw, &ec)
	}
	stage := strings.TrimSpace(ec.StageID)
	wantStage := strings.TrimSpace(os.Getenv(expStageEnv))

	expState.Lock()
	defer expState.Unlock()
	// Keep the single-credential observation across a reconfigure: a config
	// reload must not reset the count that proves only one credential is used.
	seen := expState.seenAuth
	expState.stageID = stage
	expState.recordDir = strings.TrimSpace(ec.RecordDir)
	expState.defaultArm = strings.TrimSpace(ec.DefaultArm)
	expState.authID = strings.TrimSpace(ec.AuthID)
	expState.shadow = ec.Shadow
	expState.scrub = expSplitWords(ec.Scrub)
	expState.seenAuth = seen

	// "Requested" means an operator tried to turn the experiment on: a stage
	// id or a record dir in config, or the stage environment variable. A build
	// with none of these is simply running as production -- disarmed, silent,
	// no fault. The loud error is reserved for the dangerous case, a request
	// that the self-check then refuses (plan item 7).
	requested := stage != "" || expState.recordDir != "" || wantStage != ""
	if !requested {
		expState.armed = false
		expState.fault = ""
		expState.faultPending = false
		return
	}

	fault := ""
	switch {
	case stage == "":
		fault = "experiment_stage_id is not set"
	case wantStage == "":
		fault = expStageEnv + " is not set in the environment"
	case stage != wantStage:
		fault = "experiment_stage_id does not match " + expStageEnv
	case strings.TrimSpace(cfg.PublicBaseURL) != "":
		fault = "public_base_url must be empty for the experiment (" + expMarker + ")"
	}
	if fault != "" {
		expState.armed = false
		if expState.fault != fault {
			expState.fault = fault
			expState.faultPending = true
		}
		return
	}
	expState.armed = true
	expState.fault = ""
	expState.faultPending = false
}

func expIsArmed() bool {
	expState.RLock()
	defer expState.RUnlock()
	return expState.armed
}

// expTrip disarms every experiment hook from this point on and records why.
// It is used when a second credential appears or an unknown arm is seen in a
// way that must stop the run, not just this request.
func expTrip(fault string) {
	expState.Lock()
	if expState.armed || expState.fault != fault {
		expState.armed = false
		expState.fault = fault
		expState.faultPending = true
	}
	expState.Unlock()
}

// expFlushFault logs a pending self-check fault once, on a request, the way
// relay switch warnings are logged (never from inside plugin.register).
func expFlushFault(s *Service, callbackID string) {
	expState.Lock()
	if !expState.faultPending {
		expState.Unlock()
		return
	}
	expState.faultPending = false
	fault := expState.fault
	expState.Unlock()
	defer func() { _ = recover() }()
	fields := map[string]any{
		"event": "bpsexp_disarmed", "plugin_version": Version,
		"marker": expMarker, "fault": fault,
	}
	_ = s.call("host.log", map[string]any{
		"host_callback_id": callbackID, "level": "error",
		"message": "BPSEXP experiment disarmed: " + fault,
		"fields":  fields,
	}, nil)
}

// ----------------------------------------------------------------- the trial

// expTrial is one request's experiment context. It is held in a process-wide
// registry keyed by the request's stream id (streaming) or host callback id
// (non-streaming), so the hooks that run after arm selection -- the identity
// mode override, the outbound rewrite, the capture, the decode-time observers
// -- can find it without a field on ExecutorRequest.
type expTrial struct {
	id       string
	arm      string
	modeSet  bool
	mode     identityMode
	switchS  bool
	switches map[string]any // the effective switch snapshot, for the record
	model    string
	attempts int
	reroutes []string
}

var expTrials = struct {
	sync.Mutex
	byKey map[string]*expTrial
}{byKey: map[string]*expTrial{}}

func expTrialKey(request ExecutorRequest) string {
	if request.StreamID != "" {
		return "s:" + request.StreamID
	}
	if request.HostCallbackID != "" {
		return "c:" + request.HostCallbackID
	}
	return ""
}

func expTrialFor(request ExecutorRequest) *expTrial {
	key := expTrialKey(request)
	if key == "" {
		return nil
	}
	expTrials.Lock()
	defer expTrials.Unlock()
	return expTrials.byKey[key]
}

func expPutTrial(key string, trial *expTrial) {
	expTrials.Lock()
	expTrials.byKey[key] = trial
	expTrials.Unlock()
}

func expDropTrial(request ExecutorRequest) *expTrial {
	key := expTrialKey(request)
	if key == "" {
		return nil
	}
	expTrials.Lock()
	trial := expTrials.byKey[key]
	delete(expTrials.byKey, key)
	expTrials.Unlock()
	return trial
}

// ------------------------------------------------------------- arm selection

func expApplyArm(s *Service, request *ExecutorRequest, source map[string]any, cfg *Config) {
	if request.HostCallbackID != "" {
		expFlushFault(s, request.HostCallbackID)
	}
	if !expIsArmed() {
		// Defence in depth (audit iso-11): the experiment fields are removed
		// even when the experiment is disarmed.  Stripping used to hang off
		// `armed`, which only held because an arm override (identityoff) can
		// only be applied while armed -- so disarmed + `client_identity.mode:
		// off` would have serialised `metadata.bpsexp_arm` straight to the
		// upstream.  Now the strip has no precondition at all.
		expStripArmFields(request, source)
		return
	}
	// One credential only (plan item 7): a second distinct credential trips
	// the whole experiment. The operator may also declare the id up front.
	if authID := strings.TrimSpace(request.AuthID); authID != "" {
		expState.Lock()
		declared := expState.authID
		first := expState.seenAuth
		if first == "" {
			expState.seenAuth = authID
			first = authID
		}
		expState.Unlock()
		if declared != "" && authID != declared {
			expTrip("a credential other than experiment_auth_id was used")
			return
		}
		if authID != first {
			expTrip("more than one credential was seen (" + expMarker + ")")
			return
		}
	}

	// Read the trial id before the arm fields are stripped from source.
	trialID := expReadTrialID(request, source)
	arm := expReadArm(request, source)
	countRelayEvent("bpsexp_arm:" + expArmLabel(arm))
	overrides, ok := expArmOverrides(arm)
	if !ok {
		// An unrecognised arm runs as A0 (h25) for this request and is
		// counted, rather than silently applying a wrong combination.
		countRelayEvent("bpsexp_bad_arm")
		arm = "A0"
		overrides = expArmSpec{}
	}
	overrides.applyTo(cfg)

	trial := &expTrial{
		id:      trialID,
		arm:     arm,
		switchS: overrides.switchS,
		model:   strings.TrimSpace(stringValue(source["model"])),
	}
	if overrides.modeSet {
		trial.modeSet = true
		trial.mode = overrides.mode
	}
	trial.switches = relayOptionsFromConfig(*cfg).describe()
	if key := expTrialKey(*request); key != "" {
		expPutTrial(key, trial)
	}
}

// expStripArmFields removes the experiment fields from everywhere they could
// reach the upstream -- the request headers, the request body's metadata and
// CPA's own request metadata -- and returns the arm value it found on the way.
//
// It has NO precondition: it runs armed or disarmed, so "no experiment field
// ever reaches the upstream body or headers" is true by construction rather
// than true because of a correlation with the arm-override path. Headers are
// never forwarded by this plugin anyway; the body metadata is only rebuilt
// (and unknown keys dropped) at client_identity.mode >= hygiene, so the body
// strip is the one that actually matters.
func expStripArmFields(request *ExecutorRequest, source map[string]any) string {
	arm := ""
	if request != nil && request.Headers != nil {
		for name, values := range request.Headers {
			if strings.EqualFold(name, expArmHeader) {
				if len(values) > 0 {
					arm = strings.TrimSpace(values[0])
				}
				delete(request.Headers, name)
			}
		}
	}
	if meta := objectValue(source["metadata"]); meta != nil {
		if v, ok := meta[expArmMetaKey]; ok {
			if arm == "" {
				arm = strings.TrimSpace(stringValue(v))
			}
			delete(meta, expArmMetaKey)
		}
		delete(meta, expTrialMetaID)
	}
	if request != nil && request.Metadata != nil {
		if v, ok := request.Metadata[expArmMetaKey]; ok {
			if arm == "" {
				arm = strings.TrimSpace(stringValue(v))
			}
			// EXP-BUILD section 3 promises this key is removed from the CPA
			// metadata too; it used to be read and left in place.
			delete(request.Metadata, expArmMetaKey)
		}
	}
	return arm
}

// expReadArm returns the arm for this request, stripping the fields first and
// falling back to the configured default arm.
func expReadArm(request *ExecutorRequest, source map[string]any) string {
	arm := expStripArmFields(request, source)
	if arm == "" {
		expState.RLock()
		arm = expState.defaultArm
		expState.RUnlock()
	}
	if arm == "" {
		arm = "A0"
	}
	return arm
}

func expReadTrialID(request *ExecutorRequest, source map[string]any) string {
	if request.Metadata != nil {
		if id := strings.TrimSpace(stringValue(request.Metadata[expTrialMetaID])); id != "" {
			return id
		}
	}
	if meta := objectValue(source["metadata"]); meta != nil {
		if id := strings.TrimSpace(stringValue(meta[expTrialMetaID])); id != "" {
			return id
		}
	}
	if request.HostCallbackID != "" {
		return "cb-" + shortHash(request.HostCallbackID)[:16]
	}
	return "sid-" + shortHash(request.StreamID)[:16]
}

// expArmSpec is a resolved arm: the cfg overrides it applies plus the two
// overrides that are not cfg fields (the identity mode and switch S).
type expArmSpec struct {
	relayNativePromotion string
	promotionHistory     string
	promotionBlock       string
	toolContracts        string
	relayProtocol        string
	modeSet              bool
	mode                 identityMode
	switchS              bool
}

func (a expArmSpec) applyTo(cfg *Config) {
	if a.relayNativePromotion != "" {
		cfg.RelayNativePromotion = a.relayNativePromotion
	}
	if a.promotionHistory != "" {
		cfg.RelayNativePromotionHistory = a.promotionHistory
	}
	if a.promotionBlock != "" {
		cfg.RelayNativePromotionBlock = a.promotionBlock
	}
	if a.toolContracts != "" {
		cfg.RelayToolContracts = a.toolContracts
	}
	if a.relayProtocol != "" {
		cfg.RelayProtocol = a.relayProtocol
	}
	if a.modeSet {
		cfg.ClientIdentity.Mode = a.mode.String()
	}
}

// expArmOverrides turns an arm string into its overrides. An arm is one or
// more tokens joined by '+' (or ',' / spaces): A0, A1, A1shadow, B3, H1, H2,
// blocknone, references, auto, hygiene, thread, identityoff, S. Unknown token
// -> not ok.
func expArmOverrides(arm string) (expArmSpec, bool) {
	spec := expArmSpec{}
	for _, token := range strings.FieldsFunc(arm, func(r rune) bool { return r == '+' || r == ',' || r == ' ' }) {
		switch strings.ToLower(strings.TrimSpace(token)) {
		case "a0", "h25", "":
			// h25 default; no override.
		case "a1":
			spec.relayNativePromotion = a1ModeOn
		case "a1shadow", "shadow":
			spec.relayNativePromotion = a1ModeShadow
		case "b3", "full":
			spec.toolContracts = relayToolContractsFull
		case "h1":
			spec.promotionHistory = a1HistoryH1
		case "h2":
			spec.promotionHistory = a1HistoryH2
		case "blocknone", "block_none":
			spec.promotionBlock = a1BlockNone
		case "references":
			spec.relayProtocol = relayProtocolReferences
		case "auto":
			spec.relayProtocol = relayProtocolAuto
		case "hygiene":
			spec.modeSet, spec.mode = true, identityHygiene
		case "thread":
			spec.modeSet, spec.mode = true, identityThread
		case "identityoff", "off":
			spec.modeSet, spec.mode = true, identityOff
		case "s", "plainmulti":
			spec.switchS = true
		default:
			return expArmSpec{}, false
		}
	}
	return spec, true
}

func expArmLabel(arm string) string {
	label := strings.ToLower(strings.TrimSpace(arm))
	label = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '_':
			return r
		case r == ',' || r == ' ':
			return '+'
		}
		return '-'
	}, label)
	if label == "" {
		return "none"
	}
	if len(label) > 48 {
		label = label[:48]
	}
	return label
}

// expArmMode lets an arm raise (or lower) one request's identity mode.
func expArmMode(request ExecutorRequest, mode identityMode) identityMode {
	if !expIsArmed() {
		return mode
	}
	if trial := expTrialFor(request); trial != nil && trial.modeSet {
		return trial.mode
	}
	return mode
}

// ----------------------------------------------------------------- switch S

// expRewriteOutboundBody applies the multi-agent plaintext switch (plan 3.3
// item 6, section 2 ⑤): a relay-restored multi-agent call is sent upstream
// with encrypted_function_args: [] so the upstream treats its arguments as
// plaintext. It touches only function_call items whose name is a known
// multi-agent (collaboration) tool and only when the request's arm set S.
func expRewriteOutboundBody(request ExecutorRequest, body map[string]any) {
	if !expIsArmed() || body == nil {
		return
	}
	trial := expTrialFor(request)
	if trial == nil || !trial.switchS {
		return
	}
	items, _ := body["input"].([]any)
	applied := 0
	for _, raw := range items {
		item := objectValue(raw)
		if item == nil || stringValue(item["type"]) != "function_call" {
			continue
		}
		if !expIsMultiAgentName(stringValue(item["name"])) {
			continue
		}
		if _, present := item["encrypted_function_args"]; present {
			continue
		}
		item["encrypted_function_args"] = []any{}
		applied++
	}
	if applied > 0 {
		countRelayEventN("bpsexp_switch_s:applied", applied)
	}
}

// expIsMultiAgentName recognises the collaboration / multi-agent tools whose
// arguments the upstream normally expects as ciphertext.
func expIsMultiAgentName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, prefix := range []string{"collaboration.", "collaboration_", "agents.", "agent_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "spawn_agent", "send_message", "send_agent_message", "run_agent":
		return true
	}
	return false
}

// ------------------------------------------------------------- trial marker

// expTrialState rides on the response decoder's source so decode-time
// observers (the repair shadow) can find the trial. It marshals to null and a
// JSON body cannot forge it, exactly like the A1 markers.
type expTrialState struct{ id, arm string }

func (*expTrialState) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

const expTrialKeyOnSource = "\x00cpa_bpsexp_trial"

func expWithTrial(source map[string]any, request ExecutorRequest) map[string]any {
	if !expIsArmed() {
		return source
	}
	trial := expTrialFor(request)
	if trial == nil {
		return source
	}
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	result[expTrialKeyOnSource] = &expTrialState{id: trial.id, arm: trial.arm}
	return result
}

func expTrialStateFrom(source map[string]any) *expTrialState {
	state, _ := source[expTrialKeyOnSource].(*expTrialState)
	return state
}

// ----------------------------------------------------------------- the retry tap

type expRetryTap struct {
	key   string
	model string
	armed bool
}

func expBeginRetry(request ExecutorRequest, model string) expRetryTap {
	if !expIsArmed() {
		return expRetryTap{}
	}
	return expRetryTap{key: expTrialKey(request), model: model, armed: true}
}

func (t expRetryTap) attempt(err error) {
	if !t.armed || err == nil {
		return
	}
	if name, rerouted := reroutedModel(err, t.model); rerouted {
		bucket := "other"
		low := strings.ToLower(name)
		switch {
		case strings.Contains(low, "abuse"):
			bucket = "abuse"
		case strings.Contains(low, "degrade"):
			bucket = "degrade"
		}
		countRelayEvent("bpsexp_reroute:" + bucket)
		t.note(func(tr *expTrial) { tr.reroutes = append(tr.reroutes, bucket) })
		return
	}
	if _, retryable := orgRateLimitBackoff(err); retryable {
		countRelayEvent("bpsexp_retry:rate_limit")
		t.note(func(tr *expTrial) { tr.attempts++ })
	}
}

func (t expRetryTap) done(error) {}

func (t expRetryTap) note(apply func(*expTrial)) {
	if t.key == "" {
		return
	}
	expTrials.Lock()
	if tr := expTrials.byKey[t.key]; tr != nil {
		apply(tr)
	}
	expTrials.Unlock()
}

// ----------------------------------------------------------------- status

func expStatus() map[string]any {
	expState.RLock()
	status := map[string]any{
		"marker":      expMarker,
		"armed":       expState.armed,
		"stage_id":    expState.stageID != "",
		"record_dir":  expState.recordDir != "",
		"shadow":      expState.shadow,
		"one_auth":    expState.seenAuth != "",
		"fault":       expState.fault,
		"env_present": os.Getenv(expStageEnv) != "",
	}
	expState.RUnlock()
	return status
}

func expSplitWords(value string) []string {
	var out []string
	for _, word := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' }) {
		if trimmed := strings.TrimSpace(word); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
