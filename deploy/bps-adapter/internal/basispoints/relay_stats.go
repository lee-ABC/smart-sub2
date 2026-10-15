package basispoints

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Observability (H25-SPEC S4.2).
//
// Everything here is a process-wide counter under a code-owned name, read back
// through the relay-status route; nothing from a request -- no tool name,
// argument, identifier or text -- ever becomes part of a name. The log lines
// that go with them (relay_decode_note, relay_tool_policy,
// relay_history_note, relay_tool_rejected) carry fixed labels only.
//
// Requests, per frozen relay protocol and request kind (relay: h22's relay
// catalogue is non-empty; lite: additional_tools or tool_search_output tools;
// none: neither), counted once the request is admitted:
//
//	relay_requests:<protocol>:<kind>
//	relay_outcome:<protocol>:<kind>:succeeded
//	relay_outcome:<protocol>:<kind>:<reason>            a tool rejection's reason code
//	relay_outcome:<protocol>:<kind>:error:<error kind>:<status>  any other failure
//	relay_outcome:<protocol>:<kind>:upstream_terminal:<event>  the upstream
//	    ended the response itself with error, response.failed or
//	    response.incomplete. The relay forwards that terminal and closes the
//	    stream without an error of its own (the client already has the
//	    failure in-band), but the response did not succeed (h25 review item
//	    9). A non-streaming response.incomplete is counted the same way.
//
// run_officejs calls, once per call and request however often a stream
// decodes them:
//
//	relay_call:<protocol>:ok | relay_call:<protocol>:<reason>
//	references_entries:<protocol>:<absent|not_list|unreadable|0|1|2|3|4+>
//
// The exceptions (H25-SPEC I1/I2), each counted where it happens:
//
//	E1 encrypted_tools_hidden        a prompt left encrypted tools out
//	E2 searched_tools_prologue       a prompt used the tool_search_output text
//	E3 scalar_coerced, schema_corrected, terminal_id_restored, terminal_args_equivalent
//	   (relay calls); terminal_id_restored_native, terminal_args_equivalent_native
//	   (native Lite calls); search_duplicate_merged (a relay call to a tool
//	   that tool_search_output declared twice, differing only in description
//	   or defer_loading -- h22 left such a tool out of the catalogue)
//	E4 encrypted_tool_blocked        a relay call handed back without namespace
//	   encrypted_tool_blocked_collision  ... under "<namespace>.<name>", because
//	   the bare name is another declared tool's
//	E5 retryable_error_hint          a failure worded for a retry
//
// The decode-side exceptions (scalar_coerced, schema_corrected,
// encrypted_tool_blocked) are noted when the call decodes, once per call; if
// another call of the same response then fails it, the note stays.
//
// Replay scope reasons (replay_scope:<source>, replay_scope_caller:<present|
// absent>) are counted with relay_requests, once the request is admitted
// (review R5). History notes (history_note:*, history_record_*,
// history_relay_replayed, history_cold_wrapped) and the prompt-side E1/E2
// counters are counted where the history and the prompt are prepared, which
// is before admission: a request the concurrency gate then turns away (and
// the host retries) is counted once per attempt.

// relayRequestKind classifies a request by the catalogue it brings. It only
// labels counters, so it can never fail the request it describes.
func relayRequestKind(source map[string]any) (kind string) {
	defer func() {
		if recover() != nil {
			kind = "none"
		}
	}()
	if len(clientToolSpecs(source)) > 0 {
		return "relay"
	}
	if hasAdditionalTools(source) || len(declaredNativeToolSpecs(source)) > 0 {
		return "lite"
	}
	return "none"
}

func relayRequestLabels(request ExecutorRequest) (string, string) {
	protocol := request.selectedRelayProtocol
	if protocol != relayProtocolReferences {
		protocol = relayProtocolLegacy
	}
	kind := request.relayKind
	if kind == "" {
		kind = "none"
	}
	return protocol, kind
}

// countRelayRequest counts an admitted request, and with it the replay scope
// it was frozen with (replay_scope.go), so both have the same base.
func countRelayRequest(request ExecutorRequest) {
	protocol, kind := relayRequestLabels(request)
	countRelayEvent("relay_requests:" + protocol + ":" + kind)
	if request.replayScopeFrozen {
		countRelayEvent("replay_scope:" + request.replayScopeSource)
		countRelayEvent("replay_scope_caller:" + request.replayScopeCaller)
	}
	// Native promotion's decision (a1_promotion.go): nothing unless
	// relay_native_promotion is shadow or on.
	countA1Request(request)
}

func countRelayOutcome(request ExecutorRequest, err error) {
	countRelayOutcomeAfter(request, err, "")
}

// countRelayOutcomeAfter: terminal is the upstream event that ended a
// response the relay passed on without an error of its own.
func countRelayOutcomeAfter(request ExecutorRequest, err error, terminal string) {
	protocol, kind := relayRequestLabels(request)
	countRelayEvent("relay_outcome:" + protocol + ":" + kind + ":" + relayOutcomeLabelAfter(err, terminal))
}

// upstreamFailureTerminals are the terminal events by which the upstream
// itself ends a response unsuccessfully. Fixed labels: the only terminal
// values a counter name can carry.
var upstreamFailureTerminals = map[string]bool{"error": true, "response.failed": true, "response.incomplete": true}

// relayOutcomeLabelAfter is relayOutcomeLabel, except that a response the
// upstream ended with a failure terminal is not "succeeded".
func relayOutcomeLabelAfter(err error, terminal string) string {
	if err == nil && upstreamFailureTerminals[terminal] {
		return "upstream_terminal:" + terminal
	}
	return relayOutcomeLabel(err)
}

var relayErrorKindPattern = regexp.MustCompile(`^[a-z0-9_]{1,48}$`)

// relayOutcomeLabel names how a request ended, from code-owned values only.
func relayOutcomeLabel(err error) string {
	if err == nil {
		return "succeeded"
	}
	var rejection *toolRejection
	if errors.As(err, &rejection) {
		return toolReasonLabel(rejection.reason)
	}
	kind, status := "other", 0
	var api *APIError
	if errors.As(err, &api) {
		if relayErrorKindPattern.MatchString(api.Kind) {
			kind = api.Kind
		}
		status = api.Status
	}
	if status < 100 || status > 599 {
		status = 0
	}
	return "error:" + kind + ":" + strconv.Itoa(status)
}

func toolReasonLabel(reason string) string {
	if toolRejectReasons[reason] {
		return reason
	}
	return "unspecified"
}

// observeRelayCallDecode counts one run_officejs call's decode result and the
// number of references entries the model wrote, once per call and request.
func observeRelayCallDecode(native, source map[string]any, reason string) {
	if !isTransportCall(native) {
		return
	}
	notes, _ := source[relayDecodeNotesKey].(*relayDecodeNotes)
	if notes == nil || !notes.claimObservation(relayCallNoteID(native)) {
		return
	}
	// Counting must never change a decode.
	defer func() { _ = recover() }()
	protocol := relayProtocolForDecode(source)
	outcome := "ok"
	if reason != "" {
		outcome = toolReasonLabel(reason)
	}
	countRelayEvent("relay_call:" + protocol + ":" + outcome)
	countRelayEvent("references_entries:" + protocol + ":" + referencesBucket(native))
}

// relayCallNoteID is the call id decodeRelayCall notes a call under.
func relayCallNoteID(native map[string]any) string {
	if callID := stringValue(native["call_id"]); callID != "" {
		return callID
	}
	return "call_bp_" + shortHash(string(jsonBytes(native)))[:24]
}

// referencesBucket is the number of entries in the call's references, never
// the entries themselves.
func referencesBucket(native map[string]any) string {
	args := parseArguments(native["arguments"])
	if args == nil {
		return "unreadable"
	}
	raw, present := args["references"]
	if !present {
		return "absent"
	}
	list, ok := raw.([]any)
	if !ok {
		return "not_list"
	}
	if len(list) > 3 {
		return "4+"
	}
	return strconv.Itoa(len(list))
}

const relayObservationsPerRequest = 4096

// claimObservation reports whether key is observed for the first time in this
// request. It has its own set so that counting never uses up the log notes.
func (n *relayDecodeNotes) claimObservation(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.observed == nil {
		n.observed = map[string]bool{}
	}
	if n.observed[key] || len(n.observed) >= relayObservationsPerRequest {
		return false
	}
	n.observed[key] = true
	return true
}

// notePromptExceptions counts the prompt-side exceptions of one request.
// Counting must never fail the request.
func notePromptExceptions(source map[string]any, protocolText string) {
	defer func() { _ = recover() }()
	hidden := relayPromptHiddenTools(source)
	if len(hidden) > 0 {
		countRelayEvent("encrypted_tools_hidden")
	}
	if strings.HasPrefix(protocolText, relaySearchedTools) || strings.HasPrefix(protocolText, plainSearchedTools) {
		countRelayEvent("searched_tools_prologue")
	}
}

// relayPromptHiddenTools is what E1 left out of this request's prompt.
func relayPromptHiddenTools(source map[string]any) map[string]bool {
	if relayToolContractsFor(source) == relayToolContractsFull {
		_, hidden := fullRelayCatalogue(source)
		return hidden
	}
	_, hidden := summaryRelayCatalogue(source)
	return hidden
}

// relayExceptionCounters maps each exception to the counters that record it.
var relayExceptionCounters = []struct{ name, counter string }{
	{"E1_encrypted_tools_hidden", "encrypted_tools_hidden"},
	{"E2_searched_tools_prologue", "searched_tools_prologue"},
	{"E3_scalar_coerced", "scalar_coerced"},
	{"E3_schema_corrected", "schema_corrected"},
	{"E3_terminal_id_restored", "terminal_id_restored"},
	{"E3_terminal_args_equivalent", "terminal_args_equivalent"},
	{"E3_terminal_id_restored_native", "terminal_id_restored_native"},
	{"E3_terminal_args_equivalent_native", "terminal_args_equivalent_native"},
	{"E3_search_duplicate_merged", "search_duplicate_merged"},
	{"E4_encrypted_tool_blocked", "encrypted_tool_blocked"},
	{"E4_encrypted_tool_blocked_collision", "encrypted_tool_blocked_collision"},
	{"E5_retryable_error_hint", "retryable_error_hint"},
}

// relayStatusSummary arranges the counters for reading: requests and outcomes
// by protocol and kind, run_officejs calls and references entries by protocol,
// the exceptions, replay scope reasons and history notes. Derived from the
// counters alone, so it can never disagree with them.
func relayStatusSummary(counters map[string]uint64) map[string]any {
	byProtocol := map[string]map[string]map[string]any{}
	kindEntry := func(protocol, kind string) map[string]any {
		if byProtocol[protocol] == nil {
			byProtocol[protocol] = map[string]map[string]any{}
		}
		entry := byProtocol[protocol][kind]
		if entry == nil {
			entry = map[string]any{"requests": uint64(0), "outcomes": map[string]uint64{}}
			byProtocol[protocol][kind] = entry
		}
		return entry
	}
	calls := map[string]map[string]uint64{}
	references := map[string]map[string]uint64{}
	scope := map[string]uint64{}
	history := map[string]uint64{}
	group := func(target map[string]map[string]uint64, protocol, label string, value uint64) {
		if target[protocol] == nil {
			target[protocol] = map[string]uint64{}
		}
		target[protocol][label] += value
	}
	keys := make([]string, 0, len(counters))
	for key := range counters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := counters[key]
		parts := strings.SplitN(key, ":", 4)
		switch {
		case parts[0] == "relay_requests" && len(parts) == 3:
			entry := kindEntry(parts[1], parts[2])
			entry["requests"] = entry["requests"].(uint64) + value
		case parts[0] == "relay_outcome" && len(parts) == 4:
			outcomes := kindEntry(parts[1], parts[2])["outcomes"].(map[string]uint64)
			outcomes[parts[3]] += value
		case parts[0] == "relay_call" && len(parts) == 3:
			group(calls, parts[1], parts[2], value)
		case parts[0] == "references_entries" && len(parts) == 3:
			group(references, parts[1], parts[2], value)
		case strings.HasPrefix(key, "replay_scope"):
			scope[key] = value
		case strings.HasPrefix(key, "history_"):
			history[key] = value
		}
	}
	exceptions := map[string]uint64{}
	for _, exception := range relayExceptionCounters {
		exceptions[exception.name] = counters[exception.counter]
	}
	summary := map[string]any{
		"requests_by_protocol":           byProtocol,
		"relay_calls_by_protocol":        calls,
		"references_entries_by_protocol": references,
		"exceptions":                     exceptions,
		"replay_scope":                   scope,
		"history":                        history,
	}
	// Native promotion (a1_promotion.go): present only once an a1_* counter
	// exists, i.e. never while relay_native_promotion has always been off.
	if a1 := a1Summary(counters); a1 != nil {
		summary["a1"] = a1
	}
	return summary
}

// relayOutcomeRecorder counts a stream's outcome exactly once, whichever of
// the relay's exits (normal close, panic recovery) reports it. It keeps the
// labels only, not the request.
type relayOutcomeRecorder struct {
	once           sync.Once
	protocol, kind string
}

func newRelayOutcomeRecorder(request ExecutorRequest) *relayOutcomeRecorder {
	protocol, kind := relayRequestLabels(request)
	return &relayOutcomeRecorder{protocol: protocol, kind: kind}
}

func (r *relayOutcomeRecorder) record(err error) {
	r.recordAfter(err, "")
}

// recordAfter: terminal as in countRelayOutcomeAfter.
func (r *relayOutcomeRecorder) recordAfter(err error, terminal string) {
	r.once.Do(func() {
		countRelayEvent("relay_outcome:" + r.protocol + ":" + r.kind + ":" + relayOutcomeLabelAfter(err, terminal))
	})
}
