//go:build bpsexp

package basispoints

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Experiment capture (plan 3.3 item 2, appendix C). The exact bytes the plugin
// sends upstream and the upstream's own SSE are written to the configured
// directory in the tools/equiv fixture shape, so the driver can replay and
// compare them. Account identifiers are hashed and auth headers are never
// stored; the body's own account-id strings and any operator-declared profile
// name are scrubbed. Experiment-only; absent from the production build.
//
// One fixture per trial. The request body is captured when it is serialised
// (expRecordRequestBody); the SSE accumulates as it streams (expRecordStream*)
// or arrives whole (expRecordNonStreamSSE); the fixture is written when the
// response finishes.

// expPartial is the capture-in-progress for one trial, keyed the same way the
// trial registry is keyed (stream id or callback id).
type expPartial struct {
	id     string
	arm    string
	taskID string
	model  string // the model the request asked for
	stream bool
	reqDoc map[string]any
	hdr    map[string]string
	// prior holds the request bodies of earlier attempts on the SAME trial --
	// in practice the PROMOTED body that the upstream refused with a 400
	// before the A1 fallback re-sent the relay body. Keeping it is the whole
	// point of stage 1: the refused body is the evidence (audit drv-F6-3).
	prior     []map[string]any
	sse       []byte
	attempts  int
	startedAt time.Time
}

// expCaptureTTL bounds how long an unfinalised capture may sit in the
// registry. A request that dies before any finalise point used to leak its
// whole parsed body (hundreds of KB for a layered-history trial) for the life
// of the process (audit drv-F6-4).
const expCaptureTTL = 20 * time.Minute

// expSweepCaptures drops captures whose request can no longer finish. Called
// from expRecordRequestBody, so it runs once per upstream attempt and needs no
// timer of its own.
func expSweepCaptures(now time.Time) {
	expCapture.Lock()
	for key, partial := range expCapture.byKey {
		if partial == nil || now.Sub(partial.startedAt) > expCaptureTTL {
			delete(expCapture.byKey, key)
			countRelayEvent("bpsexp_capture:expired")
		}
	}
	expCapture.Unlock()
}

var expCapture = struct {
	sync.Mutex
	byKey map[string]*expPartial
}{byKey: map[string]*expPartial{}}

// expAccountHeaders are the only request headers whose (hashed) value is kept.
// Everything else -- Authorization, Cookie, the client hints -- is dropped.
var expAccountHeaders = map[string]bool{
	"chatgpt-account-id":       true,
	"x-openai-account-id":      true,
	"x-openai-account-user-id": true,
}

func expRecordRequestBody(request ExecutorRequest, outbound []byte, headers http.Header, stream bool) {
	if !expIsArmed() || expRecordDir() == "" {
		return
	}
	key := expTrialKey(request)
	if key == "" {
		return
	}
	trial := expTrialFor(request)
	if trial == nil {
		return
	}
	var doc map[string]any
	if decoder := json.NewDecoder(strings.NewReader(string(outbound))); decoder != nil {
		decoder.UseNumber()
		_ = decoder.Decode(&doc)
	}
	hdr := map[string]string{}
	for name, values := range headers {
		if expAccountHeaders[strings.ToLower(name)] && len(values) > 0 {
			hdr[strings.ToLower(name)] = "h:" + expHash(values[0])
		}
	}
	taskID := ""
	if doc != nil {
		if meta := objectValue(doc["metadata"]); meta != nil {
			taskID = "h:" + expHash(stringValue(meta["task_id"]))
		}
	}
	now := time.Now()
	expCapture.Lock()
	next := &expPartial{
		id: trial.id, arm: trial.arm, taskID: taskID, model: trial.model,
		stream: stream, reqDoc: doc, hdr: hdr, startedAt: now,
	}
	if previous := expCapture.byKey[key]; previous != nil {
		// Same trial, another attempt (A1 promoted -> relay fallback, or a
		// silent upstream retry): keep the earlier body instead of losing it,
		// and keep the original start time so the fixture's ts still marks
		// when the trial began.
		next.prior = append(append([]map[string]any(nil), previous.prior...), previous.reqDoc)
		next.startedAt = previous.startedAt
		next.sse = previous.sse
	}
	expCapture.byKey[key] = next
	expCapture.Unlock()
	expSweepCaptures(now)
}

func expRecordStreamChunk(streamID string, payload []byte) {
	if !expIsArmed() || expRecordDir() == "" || len(payload) == 0 {
		return
	}
	key := "s:" + streamID
	expCapture.Lock()
	if partial := expCapture.byKey[key]; partial != nil {
		// Bound what one trial accumulates; the driver only needs structure.
		if len(partial.sse) < 8<<20 {
			partial.sse = append(partial.sse, payload...)
		}
	}
	expCapture.Unlock()
}

func expRecordStreamEnd(request ExecutorRequest, _ map[string]any, err error) {
	if !expIsArmed() || expRecordDir() == "" {
		return
	}
	expFinalise(request, err)
}

// expRecordNonStreamSSE finalises a non-streaming trial. `upstreamErr` is the
// error the upstream call actually returned: it used to be hard-coded to nil,
// so every failed non-streaming request -- every A2 shadow request among them
// -- was recorded with outcome "ok" and an empty event list, which would have
// made the shadow rescue rate look better than it was (audit drv-F6-2).
func expRecordNonStreamSSE(request ExecutorRequest, raw []byte, upstreamErr error) {
	if !expIsArmed() || expRecordDir() == "" {
		return
	}
	key := expTrialKey(request)
	expCapture.Lock()
	if partial := expCapture.byKey[key]; partial != nil {
		partial.sse = append([]byte(nil), raw...)
	}
	expCapture.Unlock()
	expFinalise(request, upstreamErr)
}

// expFinalise writes one trial's fixture and clears both the capture and the
// trial. It is the single place a fixture reaches disk.
func expFinalise(request ExecutorRequest, upstreamErr error) {
	key := expTrialKey(request)
	expCapture.Lock()
	partial := expCapture.byKey[key]
	delete(expCapture.byKey, key)
	expCapture.Unlock()
	trial := expDropTrial(request)
	if partial == nil {
		return
	}
	attempts := 1
	outcome := "ok"
	actualModel := ""
	events := expParseSSE(partial.sse)
	if upstreamErr != nil {
		outcome = expErrorOutcome(upstreamErr)
	}
	if model, done := expSSEModelAndTerminal(events); model != "" || done != "" {
		if model != "" {
			actualModel = model
		}
		if done != "" && upstreamErr == nil {
			outcome = done
		}
	}
	switches := map[string]any{}
	if trial != nil {
		attempts += trial.attempts
		switches = trial.switches
		if len(trial.reroutes) > 0 {
			outcome = outcome + "+reroute"
		}
	}
	// Scrub account-id substrings and the operator-declared profile name from
	// everything written (request doc and SSE).
	scrub := expScrubList(partial)
	reqDoc := expScrubValue(partial.reqDoc, scrub)
	events = expScrubEvents(events, scrub)
	var priorDocs []any
	for _, doc := range partial.prior {
		priorDocs = append(priorDocs, expScrubValue(doc, scrub))
	}

	fixture := map[string]any{
		"id":  partial.id,
		"ts":  float64(partial.startedAt.UnixNano()) / 1e9,
		"req": reqDoc,
		// Earlier attempts on the same trial, oldest first: the promoted body
		// a 400 refused is here, not overwritten by the relay body that
		// replaced it.
		"req_attempts": priorDocs,
		"hdr":          partial.hdr,
		"events":       events,
		"model":        partial.model,
		// Appendix C: the identifiers and outcome the driver files the trial by.
		"bpsexp": map[string]any{
			"trial_id":     partial.id,
			"arm":          partial.arm,
			"attempt":      attempts,
			"task_id":      partial.taskID,
			"model":        partial.model,
			"actual_model": actualModel,
			"stream":       partial.stream,
			"outcome":      outcome,
			"switches":     switches,
			"captured_at":  partial.startedAt.UTC().Format(time.RFC3339Nano),
			"marker":       expMarker,
		},
	}
	expWriteFixture(partial.id, fixture)
}

// expWriteFixture writes one fixture atomically with 0600 under a 0700
// directory. It never returns an error to the caller: a capture failure must
// not change how the request is served.
func expWriteFixture(id string, fixture map[string]any) {
	dir := expRecordDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		countRelayEvent("bpsexp_capture:error")
		return
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		countRelayEvent("bpsexp_capture:error")
		return
	}
	name := expSafeName(id)
	path := filepath.Join(dir, name+".json")
	tmp := filepath.Join(dir, name+".json.tmp")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		countRelayEvent("bpsexp_capture:error")
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		countRelayEvent("bpsexp_capture:error")
		return
	}
	countRelayEvent("bpsexp_capture:written")
}

func expRecordDir() string {
	expState.RLock()
	defer expState.RUnlock()
	return expState.recordDir
}

func expHash(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func expSafeName(id string) string {
	if id == "" {
		return "trial-" + expHash(fmt.Sprint(time.Now().UnixNano()))
	}
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

// expParseSSE turns raw SSE bytes into the [[event,data],...] shape the equiv
// fixtures use. It does not validate; it only splits on blank lines.
func expParseSSE(raw []byte) [][]string {
	if len(raw) == 0 {
		return [][]string{}
	}
	out := [][]string{}
	decoder := newSSEDecoder()
	_ = decoder.feed(raw, func(event, data string) error {
		out = append(out, []string{event, data})
		return nil
	})
	return out
}

// expSSEModelAndTerminal reads the actual answering model and the terminal
// event type out of the captured SSE, for appendix C. Code fields only.
func expSSEModelAndTerminal(events [][]string) (model, terminal string) {
	for _, pair := range events {
		if len(pair) != 2 {
			continue
		}
		var object map[string]any
		if json.Unmarshal([]byte(pair[1]), &object) != nil {
			continue
		}
		switch stringValue(object["type"]) {
		case "response.completed", "response.incomplete", "response.failed":
			terminal = stringValue(object["type"])
		case "error":
			terminal = "error"
		}
		if response := objectValue(object["response"]); response != nil {
			if m := stringValue(response["model"]); m != "" {
				model = m
			}
		}
	}
	return model, terminal
}

func expErrorOutcome(err error) string {
	var rejection *toolRejection
	if errors.As(err, &rejection) {
		return "tool_reject:" + rejection.reason
	}
	var api *APIError
	if errors.As(err, &api) {
		return fmt.Sprintf("error:%s:%d", api.Kind, api.Status)
	}
	return "error"
}

// expScrubList is the substrings to remove before a fixture reaches disk: the
// hashed account-id values do not appear in the body, but the raw account id,
// account-user-id and any operator-declared profile name can, so each is
// replaced by its own hash.
func expScrubList(partial *expPartial) map[string]string {
	out := map[string]string{}
	// Account ids from the request doc's metadata are not present (metadata is
	// task_id/turn_id only), but the operator-declared scrub words are.
	expState.RLock()
	for _, word := range expState.scrub {
		if word != "" {
			out[word] = "h:" + expHash(word)
		}
	}
	expState.RUnlock()
	return out
}

func expScrubEvents(events [][]string, scrub map[string]string) [][]string {
	if len(scrub) == 0 {
		return events
	}
	for i, pair := range events {
		for j, part := range pair {
			events[i][j] = expScrubString(part, scrub)
		}
	}
	return events
}

func expScrubValue(value any, scrub map[string]string) any {
	if len(scrub) == 0 {
		return value
	}
	switch typed := value.(type) {
	case string:
		return expScrubString(typed, scrub)
	case map[string]any:
		for key, inner := range typed {
			typed[key] = expScrubValue(inner, scrub)
		}
		return typed
	case []any:
		for i, inner := range typed {
			typed[i] = expScrubValue(inner, scrub)
		}
		return typed
	}
	return value
}

func expScrubString(text string, scrub map[string]string) string {
	for word, replacement := range scrub {
		if strings.Contains(text, word) {
			text = strings.ReplaceAll(text, word, replacement)
		}
	}
	return text
}
