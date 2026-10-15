//go:build bpsexp

package basispoints

import (
	"errors"
	"net/http"
	"strings"
	"sync"
)

// Experiment deterministic-repair shadow (plan 3.3 item 5) and A2 shadow
// rewrite (plan 3.3 item 4, 4.2), plus the experiment-only management routes.
// Experiment build only.

// -------------------------------------------------- deterministic-repair shadow

// expObserveRepairShadow is called at every relay decode. On a failure it only
// measures, never delivers (plan item 5): can the inner JSON be repaired, is
// the repair unambiguous, and does the repaired call pass the tool's schema.
// The counters are
//
//	bpsexp_repair:attempted:<reason>
//	bpsexp_repair:unrepairable
//	bpsexp_repair:repaired          (parses after one deterministic repair)
//	bpsexp_repair:unique            (exactly one plausible repair)
//	bpsexp_repair:ambiguous         (more than one reading)
//	bpsexp_repair:passes_schema     (repaired args match the tool schema)
func expObserveRepairShadow(native, source map[string]any, reason string) {
	if !expIsArmed() || reason == "" {
		return
	}
	// Only the inner-JSON failures are repairable; a missing tool or an
	// incomplete item is not a JSON-repair problem.
	if reason != "code_json" && reason != "outer_json" {
		return
	}
	if !isTransportCall(native) {
		return
	}
	notes, _ := source[relayDecodeNotesKey].(*relayDecodeNotes)
	if notes != nil && !notes.claimObservation("repair\x00"+relayCallNoteID(native)) {
		return
	}
	defer func() { _ = recover() }()
	countRelayEvent("bpsexp_repair:attempted:" + toolReasonLabel(reason))

	args := parseArguments(native["arguments"])
	code, _ := args["code"].(string)
	if reason == "outer_json" {
		code, _ = native["arguments"].(string)
	}
	repaired, ok, unique := expRepairJSON(code)
	if !ok {
		countRelayEvent("bpsexp_repair:unrepairable")
		return
	}
	countRelayEvent("bpsexp_repair:repaired")
	if unique {
		countRelayEvent("bpsexp_repair:unique")
	} else {
		countRelayEvent("bpsexp_repair:ambiguous")
	}
	if expRepairPassesSchema(repaired, source) {
		countRelayEvent("bpsexp_repair:passes_schema")
	}
}

// expRepairJSON applies the one deterministic repair the plan describes
// (double a lone backslash, escape a raw newline/tab inside a string) and
// reports whether the result parses and whether the reading was unambiguous.
// A string that already contains a valid JSON escape such as \t or \n could be
// read two ways (literal tab vs. the escape), so it is repaired but not
// unique -- which is why the plan keeps this shadow-only.
func expRepairJSON(code string) (repaired string, ok bool, unique bool) {
	code = strings.TrimSpace(stripCodeFence(code))
	if code == "" {
		return "", false, false
	}
	if _, already := decodeOneToolJSON(code); already {
		// It already parses; nothing to repair.
		return code, true, true
	}
	ambiguous := expHasAmbiguousEscape(code)
	fixed := expEscapeStringInteriors(code)
	if _, good := decodeOneToolJSON(fixed); good {
		return fixed, true, !ambiguous
	}
	return "", false, false
}

// expHasAmbiguousEscape reports whether code contains a backslash escape whose
// intended reading is not unique (\t \n \r \f \b and the like read as either
// the control character or two literal characters).
func expHasAmbiguousEscape(code string) bool {
	for i := 0; i+1 < len(code); i++ {
		if code[i] != '\\' {
			continue
		}
		switch code[i+1] {
		case 't', 'n', 'r', 'f', 'b':
			return true
		}
	}
	return false
}

// expEscapeStringInteriors doubles lone backslashes and escapes raw control
// characters that appear inside JSON string literals. It walks the text with a
// simple in-string/out-of-string state machine; it does not try to be clever,
// which is the point of measuring uniqueness separately.
func expEscapeStringInteriors(code string) string {
	var out strings.Builder
	inString := false
	for i := 0; i < len(code); i++ {
		c := code[i]
		if !inString {
			out.WriteByte(c)
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			out.WriteByte(c)
			inString = false
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\\':
			next := byte(0)
			if i+1 < len(code) {
				next = code[i+1]
			}
			switch next {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
				// Already a valid escape; keep it.
				out.WriteByte(c)
				out.WriteByte(next)
				i++
			default:
				// A lone backslash: double it.
				out.WriteString(`\\`)
			}
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// expRepairPassesSchema reports whether the repaired inner JSON, read as a
// relay envelope, names a declared tool and matches its schema (h22's own
// check). Shadow only; nothing is delivered.
func expRepairPassesSchema(repaired string, source map[string]any) bool {
	value, ok := decodeOneToolJSON(repaired)
	if !ok {
		return false
	}
	object, _ := value.(map[string]any)
	if object == nil {
		return false
	}
	name := stringValue(object["tool"])
	if name == "" {
		name = stringValue(object["name"])
	}
	var argObject map[string]any
	if name != "" {
		argObject = parseArguments(object["args"])
		if argObject == nil {
			argObject = parseArguments(object["arguments"])
		}
	} else {
		// The repaired code is itself the arguments; no declared name to match.
		argObject = object
	}
	specs := relayOutputToolSpecs(source)
	spec, exists := specs[name]
	if !exists || argObject == nil {
		return false
	}
	schema := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")
	return h22SchemaMatches(argObject, schema)
}

// ----------------------------------------------------------- A2 shadow rewrite

const expShadowFlushPath = "bpsexp-flush"

// expShadowTask is one queued relay failure: the two shadow requests to send
// after the block closes (continuation correction and direct resample), on the
// same credential, delivered to no one.
type expShadowTask struct {
	continuation ExecutorRequest
	resample     ExecutorRequest
	credential   credential
}

var expShadowQueue = struct {
	sync.Mutex
	tasks []expShadowTask
}{}

// expEnqueueShadowRewrite queues a relay-arm invalid_tool_call failure for the
// A2 shadow rewrite. It never touches the client and does not resend here; the
// resends happen on a flush (expShadowFlush) after the block ends.
func expEnqueueShadowRewrite(s *Service, request ExecutorRequest, source map[string]any, c credential, err error) {
	if !expIsArmed() || err == nil {
		return
	}
	expState.RLock()
	on := expState.shadow
	expState.RUnlock()
	if !on {
		return
	}
	// Relay arm only (not a promoted A1 request), and only an invalid_tool_call.
	if request.relayKind == relayKindPromoted {
		return
	}
	var rejection *toolRejection
	if !errors.As(err, &rejection) || rejection.cause == nil || rejection.cause.Kind != "invalid_tool_call" {
		return
	}
	clean := expStripPrivate(source)
	cont := expShadowRequest(request, clean, "continuation")
	resample := expShadowRequest(request, clean, "resample")
	if cont == nil || resample == nil {
		return
	}
	expShadowQueue.Lock()
	expShadowQueue.tasks = append(expShadowQueue.tasks, expShadowTask{
		continuation: *cont, resample: *resample, credential: c,
	})
	expShadowQueue.Unlock()
	countRelayEvent("bpsexp_shadow:enqueued")
}

// expShadowRequest builds one shadow ExecutorRequest from the failed request's
// own identity and body. continuation appends a developer note asking the
// model to re-emit the single failed call; resample resends the body as is.
func expShadowRequest(orig ExecutorRequest, clean map[string]any, variant string) *ExecutorRequest {
	body := map[string]any{}
	for key, value := range clean {
		body[key] = value
	}
	if variant == "continuation" {
		items, _ := body["input"].([]any)
		note := messageItem("developer",
			"The previous tool call was not executed because its arguments could not be read. "+
				"Re-emit exactly one corrected call for the same tool, with valid JSON arguments.")
		body["input"] = append(append([]any{}, items...), note)
	}
	raw := jsonBytes(body)
	if len(raw) == 0 {
		return nil
	}
	meta := map[string]any{}
	for key, value := range orig.Metadata {
		meta[key] = value
	}
	trialID := expReadTrialID(&orig, clean)
	meta[expTrialMetaID] = trialID + "-shadow-" + variant
	return &ExecutorRequest{
		AuthID:          orig.AuthID,
		AuthProvider:    orig.AuthProvider,
		Model:           orig.Model,
		Stream:          false,
		OriginalRequest: raw,
		StorageJSON:     orig.StorageJSON,
		AuthMetadata:    orig.AuthMetadata,
		AuthAttributes:  orig.AuthAttributes,
		Metadata:        meta,
		HostCallbackID:  orig.HostCallbackID + "-shadow-" + variant,
	}
}

// expStripPrivate copies source without the private \x00-prefixed keys the
// relay rides on it (they marshal to null and would corrupt a reserialised
// body).
func expStripPrivate(source map[string]any) map[string]any {
	clean := make(map[string]any, len(source))
	for key, value := range source {
		if strings.HasPrefix(key, "\x00") {
			continue
		}
		clean[key] = value
	}
	return clean
}

// expShadowFlush issues every queued shadow rewrite. Each task sends its
// continuation and resample on the same credential, under the concurrency
// gate, records both results and delivers nothing. 1:1 by construction.
func (s *Service) expShadowFlush() map[string]any {
	expShadowQueue.Lock()
	tasks := expShadowQueue.tasks
	expShadowQueue.tasks = nil
	expShadowQueue.Unlock()

	continuation, resample, skipped := 0, 0, 0
	for _, task := range tasks {
		if s.expShadowSend(task.continuation, task.credential) {
			continuation++
		} else {
			skipped++
		}
		if s.expShadowSend(task.resample, task.credential) {
			resample++
		} else {
			skipped++
		}
	}
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": jsonBytes(map[string]any{
			"processed":    len(tasks),
			"continuation": continuation,
			"resample":     resample,
			"skipped":      skipped,
			"marker":       expMarker,
		}),
	}
}

// expShadowSend sends one shadow request. It counts against the in-flight gate
// just as a live request does (plan 3.4), records the result and returns
// whether it actually went upstream. Nothing is ever written to a client.
func (s *Service) expShadowSend(request ExecutorRequest, c credential) bool {
	body, cred, err := s.prepareRequest(request)
	if err != nil {
		countRelayEvent("bpsexp_shadow:skipped")
		return false
	}
	limit, _ := s.config().concurrencyLimit(cred.Email, stringValue(request.AuthMetadata["file_name"]))
	release, admitted := s.gate.acquire(concurrencyKey(request, cred), limit)
	if !admitted {
		countRelayEvent("bpsexp_shadow:gate_full")
		return false
	}
	defer release()
	response, callErr := s.upstreamRequest(request, body, cred, false)
	// Finalise this shadow trial's fixture directly (it never goes through the
	// non-stream executor that would otherwise record the SSE).
	expRecordNonStreamSSE(request, response.Body, callErr)
	variant := "continuation"
	if strings.HasSuffix(request.HostCallbackID, "-resample") {
		variant = "resample"
	}
	if callErr != nil {
		countRelayEvent("bpsexp_shadow:" + variant + "_error")
	} else {
		countRelayEvent("bpsexp_shadow:" + variant)
	}
	return true
}

// ----------------------------------------------------------- management routes

// expManagementRoute serves the experiment-only routes: the shadow-rewrite
// flush and an experiment status page. Unknown paths are left to the caller.
func expManagementRoute(s *Service, path, _ string, _ map[string][]string) (map[string]any, bool) {
	switch {
	case strings.HasSuffix(path, expShadowFlushPath):
		return s.expShadowFlush(), true
	}
	return nil, false
}
