package basispoints

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Incremental stream relay.
//
// Basis Points streams an answer as a few hundred SSE events. 0.1.x read that
// stream to completion, pulled the finished object out of response.completed
// and only then synthesised an event sequence from it, so the caller's first
// byte arrived after generation had already ended. Measured against
// production, time to first byte equalled total latency exactly -- 13.5s for a
// 400-character reply at low effort and 30.5s at high -- while the upstream
// itself produced its first text at 5.1s and 9.7s. The wait was entirely
// self-inflicted.
//
// Events are therefore forwarded as they arrive. The one thing that cannot be
// forwarded blind is the client-tool relay: client tools cannot be declared to
// Basis Points, so the model calls a transport tool whose arguments encode the
// real tool name, and that name is unknown until the arguments are complete.
// Only those items are held back. Text, reasoning and native Lite tools stream
// straight through.
//
// When a held relay call is delivered is relay_tool_delivery:
//   - immediate (default, = h22): on its own output_item.done, as soon as it
//     decodes. The terminal snapshot is checked against it afterwards.
//   - batch (rc2): every relay call of the response waits until the terminal
//     snapshot validates as a whole, so no partially valid batch ever becomes
//     executable. While calls wait, an SSE comment keeps the connection from
//     looking idle.

type rawSSEEvent struct {
	name string
	data string
}

// heldTransportCall buffers transport calls and arguments whose item has not
// arrived yet. Transport events are never replayed to the caller.
type heldTransportCall struct {
	events    []rawSSEEvent
	transport bool
	// unfinished: an item that completed with a non-completed status, kept
	// until the terminal says why (relay_incomplete: passthrough only).
	unfinished bool
}

type relayCacheWrite struct {
	native, client map[string]any
}

// streamProgress is what the client has actually been sent. executeStream
// reads it when it closes the stream -- also after a panic -- to tell a
// failure with no output at all (E5) from one in the middle of an answer.
type streamProgress struct {
	outputSent atomic.Bool
	// terminal is the upstream terminal event the relay finished on (for
	// relay_outcome only; see upstreamFailureTerminals).
	terminal atomic.Value
	// webSearch: the upstream emitted a web_search_call in this response (h26).
	webSearch atomic.Bool
	// scope: the caller-scoped conversation key of this stream (h27c).
	scope string
	// failedOver: this stream ended with a failover failure (h27d).
	failedOver atomic.Bool
}

// upstreamTerminal is the terminal event the relay finished on, if any.
func (p *streamProgress) upstreamTerminal() string {
	if p == nil {
		return ""
	}
	terminal, _ := p.terminal.Load().(string)
	return terminal
}

type streamRelay struct {
	service  *Service
	streamID string
	source   map[string]any
	// target and model price the completed response; a stream is the only
	// place its usage is ever reported.
	target usageTarget
	model  string
	// opts are the request's frozen h25 switches (relay_options.go).
	opts relayOptions

	decoder          *sseDecoder
	out              strings.Builder
	sequence         int
	receivedBytes    int
	maxInputBytes    int
	cacheAfterFlush  []relayCacheWrite
	nativeAfterFlush []map[string]any

	held map[int]*heldTransportCall
	// announced records the items whose identity was established by added.
	// Orphan argument events must wait until their tool identity is known.
	announced map[int]map[string]any
	// translatedByIndex: validated relay calls not delivered yet (batch).
	translatedByIndex map[int]map[string]any
	// deliveredByIndex: every client tool call written to the client, native
	// or relayed; relayIndexes marks the relayed ones and nativeByIndex keeps
	// the upstream transport item behind each relay call.
	deliveredByIndex   map[int]map[string]any
	relayIndexes       map[int]bool
	nativeByIndex      map[int]map[string]any
	callOwners         map[string]int
	itemOwners         map[string]int
	completedItems     map[int]bool
	countedToolIndexes map[int]bool

	// toolDelivered: a complete client tool call has been written to the
	// client. From then on a tool_policy_checks finding never fails the stream.
	toolDelivered bool
	// pendingOutput: written but not yet emitted output other than the
	// response.created / response.in_progress metadata.
	pendingOutput bool
	progress      *streamProgress

	// emitMu serialises host.stream.emit between the relay and the batch
	// heartbeat; lastEmit is guarded by it.
	emitMu   sync.Mutex
	lastEmit time.Time
	// holding: batch relay calls are being withheld right now.
	holding atomic.Bool
	// h27 web_search_failover: switch read at stream start, hold state, and
	// whether output is being held right now (read by the heartbeat).
	wsFailover  bool
	wsHoldAll   bool
	wsSwitchOn  bool
	wsState     int
	wsHoldStart time.Time
	wsPhases    map[int]string
	wsHolding   atomic.Bool

	// declared caches allDeclaredClientToolSpecs(source) for the native tool
	// policy checks; built on first use (declaredTools). Only the goroutine
	// that feeds the relay touches it.
	declared      map[string]toolSpec
	declaredBuilt bool

	// terminal is the event type that ended the stream, once one has.
	terminal string
	sawDone  bool

	// a1wide2 relay_no_catalog_transport (no_catalog_transport.go). noCatalog
	// is decided on the first run_officejs call (0 not yet, 1 withheld, -1
	// decoded as a relay call); withheld holds the calls kept from the
	// client. call is how to answer them upstream (nil: never). A terminal
	// that asks for a continuation sets continuation and ends this relay's
	// part; the relay that carries the stream on writes at indexOffset and
	// later, without a second response.created (suppressMeta), and merges
	// prior into its terminal.
	noCatalog    int8
	withheld     map[int]map[string]any
	call         *upstreamCall
	continuation *noCatalogContinuation
	continued    int
	indexOffset  int
	suppressMeta bool
	prior        *noCatalogPrior
}

func newStreamRelay(service *Service, streamID string, source map[string]any, target usageTarget, model string) *streamRelay {
	return &streamRelay{
		service:            service,
		streamID:           streamID,
		source:             source,
		target:             target,
		model:              model,
		opts:               relayOptionsFrom(source),
		decoder:            newSSEDecoder(),
		maxInputBytes:      service.config().MaxResponseBytes,
		held:               map[int]*heldTransportCall{},
		announced:          map[int]map[string]any{},
		translatedByIndex:  map[int]map[string]any{},
		deliveredByIndex:   map[int]map[string]any{},
		relayIndexes:       map[int]bool{},
		nativeByIndex:      map[int]map[string]any{},
		callOwners:         map[string]int{},
		itemOwners:         map[string]int{},
		completedItems:     map[int]bool{},
		countedToolIndexes: map[int]bool{},
		withheld:           map[int]map[string]any{},
		lastEmit:           time.Now(),
	}
}

// withholdsTransport reports, once per stream, whether this request's
// run_officejs calls are withheld (no_catalog_transport.go).
func (r *streamRelay) withholdsTransport() bool {
	if r.noCatalog == 0 {
		r.noCatalog = -1
		if noCatalogTransport(r.source) {
			r.noCatalog = 1
		}
	}
	return r.noCatalog > 0
}

func (r *streamRelay) batchDelivery() bool {
	return r.opts.ToolDelivery == relayToolDeliveryBatch
}

// rc2Terminals: relay_incomplete: passthrough also brings back rc2's stricter
// reading of the stream protocol (terminal without its response object,
// contradictory terminal status, trailing bytes after an event's JSON).
func (r *streamRelay) rc2Terminals() bool {
	return r.opts.Incomplete == relayIncompletePassthrough
}

func (r *streamRelay) feed(chunk []byte) error {
	if len(chunk) > r.maxInputBytes-r.receivedBytes {
		return fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
	}
	r.receivedBytes += len(chunk)
	err := r.decoder.feed(chunk, r.handle)
	r.holding.Store(r.batchDelivery() && r.terminal == "" && r.pendingRelayWork())
	return err
}

// pendingRelayWork: a relay call is still held from the client, either being
// generated or (batch) validated and waiting for the terminal snapshot.
func (r *streamRelay) pendingRelayWork() bool {
	return len(r.held) > 0 || len(r.translatedByIndex) > 0
}

func (r *streamRelay) decodeEvent(data string) map[string]any {
	return decodeEventPayloadWith(data, r.rc2Terminals())
}

func (r *streamRelay) handle(event, data string) (err error) {
	diagnosticEvent, diagnosticIndex := event, -1
	defer func() {
		err = withToolDiagnostic(err, diagnosticEvent, diagnosticIndex, r.toolDelivered || len(r.deliveredByIndex) > 0)
	}()
	// a1wide2: this response's turn is being carried on upstream; nothing
	// after its terminal belongs to the client (no_catalog_transport.go).
	if r.continuation != nil {
		return nil
	}
	if strings.TrimSpace(data) == "[DONE]" {
		// A sentinel is not a successful terminal response. In particular it
		// must never get ahead of a failed tool conversion or a truncated call.
		if r.terminal == "" {
			if r.pendingRelayWork() {
				return r.truncatedRelay()
			}
			return fail(502, "invalid_upstream_response", "Basis Points stream ended without a terminal event")
		}
		if r.sawDone {
			return nil
		}
		r.sawDone = true
		r.out.WriteString("data: [DONE]\n\n")
		return nil
	}
	// Only the first terminal event belongs to this response. Ignore any
	// trailing events instead of emitting a second terminal or tool call.
	if r.terminal != "" {
		return nil
	}
	payload := r.decodeEvent(data)
	if payload == nil {
		return fail(502, "invalid_upstream_response", "Basis Points returned an invalid stream event")
	}
	kind := stringValue(payload["type"])
	if kind == "" {
		kind = event
		payload["type"] = kind
	} else if event != "" && event != kind {
		return fail(502, "invalid_upstream_response", "Basis Points returned inconsistent stream event types")
	}
	diagnosticEvent, diagnosticIndex = kind, eventOutputIndex(payload)
	if handled, err := r.webSearchFailoverObserve(kind, payload); err != nil || handled {
		return err
	}
	if r.progress != nil && (kind == "response.output_item.added" || kind == "response.output_item.done") {
		if item := objectValue(payload["item"]); item != nil && stringValue(item["type"]) == "web_search_call" {
			r.progress.webSearch.Store(true)
		}
	}
	switch kind {
	case "response.created", "response.in_progress":
		if response := objectValue(payload["response"]); response != nil {
			// These normally have empty output, but an early snapshot must not
			// bypass the same transport validation as a terminal snapshot.
			output, _ := response["output"].([]any)
			for index, value := range output {
				item := objectValue(value)
				if item != nil && isTransportCall(item) {
					return invalidStreamToolCall("early_transport")
				}
				if err := r.checkNativeCall(item); err != nil {
					return err
				}
				if err := r.enforceToolCount(index, item); err != nil {
					return err
				}
			}
			if r.suppressMeta {
				// A continuation: the client has had these already.
				return nil
			}
			restoreCallerEcho(response, r.source)
			r.writeEvent(eventName(event, payload), payload)
			return nil
		}
		if r.suppressMeta {
			return nil
		}
	case "response.output_item.added":
		if item := objectValue(payload["item"]); item != nil {
			if err := r.checkNativeCall(item); err != nil {
				return err
			}
			index := eventOutputIndex(payload)
			if index >= 0 && (r.announced[index] != nil || r.translatedByIndex[index] != nil || r.relayIndexes[index]) {
				return invalidStreamToolCall("duplicate_identity")
			}
			if err := r.enforceToolCount(index, item); err != nil {
				return err
			}
			if err := r.reserveToolIdentity(index, item); err != nil {
				return err
			}
			kind := stringValue(item["type"])
			unknownTool := (kind == "function_call" || kind == "custom_tool_call") && stringValue(item["name"]) == ""
			if isTransportCall(item) || unknownTool {
				if err := r.hold(index, event, data); err != nil {
					return err
				}
				r.held[index].transport = r.held[index].transport || isTransportCall(item)
				if r.held[index].transport {
					r.held[index].events = nil
				}
				r.announced[index] = item
				return nil
			}
			if index >= 0 {
				if r.isHeld(index) {
					// An earlier transport cannot become a native tool under
					// the same index and thereby release its raw arguments.
					return invalidStreamToolCall("identity_mismatch")
				}
				r.announced[index] = item
			}
		}
	case "response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done":
		index := eventOutputIndex(payload)
		if r.isHeld(index) || r.announced[index] == nil || isTransportCall(r.announced[index]) {
			return r.hold(index, event, data)
		}
		if err := r.checkNativeDelta(kind, payload, r.announced[index]); err != nil {
			return err
		}
	case "response.output_item.done":
		index := eventOutputIndex(payload)
		item := objectValue(payload["item"])
		if r.isHeld(index) || (item != nil && isTransportCall(item)) {
			return r.release(index, payload, event, data)
		}
		if item != nil && isStreamToolItem(item) {
			if err := r.checkNativeCall(item); err != nil {
				return err
			}
			if r.completedItems[index] || !matchesAnnouncedStreamItem(r.announced[index], item) {
				return invalidStreamToolCall("identity_mismatch")
			}
			if err := r.enforceToolCount(index, item); err != nil {
				return err
			}
			if err := r.reserveToolIdentity(index, item); err != nil {
				return err
			}
			r.completedItems[index] = true
			r.deliveredByIndex[index] = cloneObject(item)
			observeNativeToolSchema(item, r.source)
			observeA1NativeCall(item, r.source, r.declaredTools)
			r.queueNativeProvenance(item)
		}
	case "response.completed", "response.failed", "response.incomplete":
		return r.emitTerminal(payload, event, data)
	case "error":
		// A top-level error event carries no response object and ends the
		// stream just as response.failed does.
		r.writeRaw(event, data)
		r.discardPendingTransport()
		r.terminal = "error"
		return nil
	}
	// Unknown event variants must not provide an alternate path for the
	// transport item to reach clients that dispatch on the SSE event label.
	if item := objectValue(payload["item"]); item != nil && isTransportCall(item) {
		return invalidStreamToolCall("unexpected_transport_event")
	}
	r.writeRaw(event, data)
	return nil
}

func invalidStreamToolCall(reason ...string) error {
	category := "unspecified"
	if len(reason) > 0 {
		category = reason[0]
	}
	return toolReject(category)
}

// truncatedRelay is a stream that ended -- by EOF or by a bare [DONE] -- while a
// relay call was still held. relay_incomplete: error (default) reports it as
// h22 did, invalid_tool_call, with its actual cause as the reason;
// passthrough reports rc2's upstream truncation (invalid_upstream_response).
func (r *streamRelay) truncatedRelay() error {
	if r.rc2Terminals() {
		return streamIncomplete()
	}
	return invalidStreamToolCall("missing_terminal_event")
}

func isStreamToolItem(item map[string]any) bool {
	kind := stringValue(item["type"])
	return kind == "function_call" || kind == "custom_tool_call"
}

// policy decides one tool_policy_checks finding (relay_policy.go).
func (r *streamRelay) policy(reason string) error {
	return toolPolicyRejection(r.source, r.toolDelivered, reason)
}

// declaredTools is allDeclaredClientToolSpecs(r.source), computed once per
// stream: the request's source is never modified while its response streams.
// Rebuilt for every native argument fragment, it cost up to milliseconds per
// fragment on large Lite requests (review EH-1); cached, the policy results
// and logs are the same.
func (r *streamRelay) declaredTools() map[string]toolSpec {
	if !r.declaredBuilt {
		r.declared = allDeclaredClientToolSpecs(r.source)
		r.declaredBuilt = true
	}
	return r.declared
}

func (r *streamRelay) checkNativeCall(item map[string]any) error {
	if reason := nativeToolPolicyReasonIn(item, r.source, r.declaredTools); reason != "" {
		return r.policy(reason)
	}
	return nil
}

func (r *streamRelay) checkNativeDelta(kind string, payload, item map[string]any) error {
	if reason := nativeToolDeltaPolicyReasonIn(kind, payload, item, r.source, r.declaredTools); reason != "" {
		return r.policy(reason)
	}
	return nil
}

// Explicit single-call policies (parallel_tool_calls:false, a forced choice)
// are a tool_policy_checks finding. Repeated lifecycle events for the same
// output index do not consume another call; unknown/opaque item types do not
// become functions.
func (r *streamRelay) enforceToolCount(index int, item map[string]any) error {
	if !isStreamToolItem(item) || stringValue(item["name"]) == "" {
		return nil
	}
	if index < 0 {
		return invalidStreamToolCall("invalid_index")
	}
	if r.countedToolIndexes[index] {
		return nil
	}
	r.countedToolIndexes[index] = true
	if limit := clientToolCallLimit(r.source); limit > 0 && len(r.countedToolIndexes) > limit {
		choice := objectValue(r.source["tool_choice"])
		if kind := stringValue(choice["type"]); kind == "function" || kind == "custom" {
			return r.policy("forced_tool_count")
		}
		return r.policy("parallel_tool_count")
	}
	return nil
}

// A call or item identity belongs to one output index for the entire response,
// whether it arrived natively, through the relay, or only in the final snapshot.
func (r *streamRelay) reserveToolIdentity(index int, item map[string]any) error {
	if !isStreamToolItem(item) {
		return nil
	}
	if index < 0 {
		return invalidStreamToolCall("invalid_index")
	}
	for _, field := range []struct {
		name   string
		owners map[string]int
	}{{"call_id", r.callOwners}, {"id", r.itemOwners}} {
		identity := stringValue(item[field.name])
		if identity == "" {
			continue
		}
		if owner, exists := field.owners[identity]; exists && owner != index {
			return invalidStreamToolCall("duplicate_identity")
		}
		field.owners[identity] = index
	}
	return nil
}

func (r *streamRelay) hold(index int, event, data string) error {
	if index < 0 {
		return invalidStreamToolCall("invalid_index")
	}
	if r.completedItems[index] {
		return invalidStreamToolCall("duplicate_identity")
	}
	entry := r.held[index]
	if entry == nil {
		entry = &heldTransportCall{}
		r.held[index] = entry
	}
	if !entry.transport {
		entry.events = append(entry.events, rawSSEEvent{name: event, data: data})
	}
	return nil
}

func (r *streamRelay) isHeld(index int) bool {
	if index < 0 {
		return false
	}
	_, ok := r.held[index]
	return ok
}

// release turns a completed transport call into its validated client tool.
// Unknown argument fragments are replayed only after a native tool is known;
// a native Lite lifecycle stays incremental.
func (r *streamRelay) release(index int, payload map[string]any, event, data string) error {
	if index < 0 {
		return invalidStreamToolCall("invalid_index")
	}
	if r.completedItems[index] {
		return invalidStreamToolCall("duplicate_identity")
	}
	if entry := r.held[index]; entry != nil && entry.unfinished {
		return invalidStreamToolCall("incomplete_item")
	}
	item := objectValue(payload["item"])
	if item == nil {
		return invalidStreamToolCall("terminal_item_missing")
	}
	if !matchesAnnouncedStreamItem(r.announced[index], item) {
		return invalidStreamToolCall("identity_mismatch")
	}
	if err := r.enforceToolCount(index, item); err != nil {
		return err
	}
	if err := r.reserveToolIdentity(index, item); err != nil {
		return err
	}
	if !isTransportCall(item) {
		entry := r.held[index]
		if entry == nil || entry.transport {
			return invalidStreamToolCall("identity_mismatch")
		}
		// No added event identified this tool. Its complete native item now
		// proves that the buffered arguments were not transport fragments.
		if kind := stringValue(item["type"]); kind != "function_call" && kind != "custom_tool_call" {
			return invalidStreamToolCall("identity_mismatch")
		}
		if err := r.checkNativeCall(item); err != nil {
			return err
		}
		// Orphan native argument events should carry the same identity and
		// event family as the complete item that now names them.
		for _, buffered := range entry.events {
			value := r.decodeEvent(buffered.data)
			if err := r.checkNativeDelta(eventName(buffered.name, value), value, item); err != nil {
				return err
			}
		}
		delete(r.held, index)
		r.completedItems[index] = true
		r.deliveredByIndex[index] = cloneObject(item)
		observeNativeToolSchema(item, r.source)
		observeA1NativeCall(item, r.source, r.declaredTools)
		r.queueNativeProvenance(item)
		for _, buffered := range entry.events {
			r.writeRaw(buffered.name, buffered.data)
		}
		r.writeRaw(event, data)
		return nil
	}
	if status := stringValue(item["status"]); status != "" && status != "completed" {
		if !r.rc2Terminals() {
			// h22: a truncated transport call is never executable.
			return invalidStreamToolCall("incomplete_item")
		}
		// passthrough: keep it non-executable while waiting for the
		// terminal's actual cause.
		if err := r.hold(index, event, data); err != nil {
			return err
		}
		r.held[index].transport, r.held[index].unfinished = true, true
		r.held[index].events = nil
		if r.announced[index] == nil {
			r.announced[index] = cloneObject(item)
		}
		return nil
	}
	if r.withholdsTransport() {
		// a1wide2: no client tool can stand for this call. It is kept from
		// the client and never decoded; the terminal decides whether the
		// turn is carried on upstream (no_catalog_transport.go).
		delete(r.held, index)
		r.completedItems[index] = true
		r.withheld[index] = cloneObject(item)
		noteNoCatalogTransport(item)
		return nil
	}
	translated, reason := decodeTransportItemWithReason(item, r.source)
	if reason != "" {
		return invalidStreamToolCall(reason)
	}
	delete(r.held, index)
	if r.translatedByIndex[index] != nil || r.relayIndexes[index] {
		return invalidStreamToolCall("duplicate_identity")
	}
	if err := r.reserveToolIdentity(index, translated); err != nil {
		return err
	}
	r.nativeByIndex[index] = cloneObject(item)
	r.relayIndexes[index] = true
	r.completedItems[index] = true
	if r.batchDelivery() {
		r.translatedByIndex[index] = translated
		return nil
	}
	// immediate (h22): the call is delivered on its own done. Its replay
	// record is written once the host has accepted the bytes (flush).
	r.deliveredByIndex[index] = cloneObject(translated)
	r.cacheAfterFlush = append(r.cacheAfterFlush, relayCacheWrite{native: cloneObject(item), client: cloneObject(translated)})
	r.emitTranslatedCall(index, translated)
	return nil
}

func (r *streamRelay) emitTranslatedCall(index int, translated map[string]any) {
	final := cloneObject(translated)
	final["status"] = "completed"
	itemID := stringValue(final["id"])
	r.writeEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": index,
		"item":         cloneObject(final),
	})
	if arguments := stringValue(final["arguments"]); arguments != "" {
		r.writeEvent("response.function_call_arguments.done", map[string]any{
			"type":         "response.function_call_arguments.done",
			"output_index": index,
			"item_id":      itemID,
			"arguments":    arguments,
		})
	}
	if input, ok := final["input"].(string); ok {
		r.writeEvent("response.custom_tool_call_input.done", map[string]any{
			"type":         "response.custom_tool_call_input.done",
			"output_index": index,
			"item_id":      itemID,
			"input":        input,
		})
	}
	r.writeEvent("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": index,
		"item":         final,
	})
}

// emitTerminal validates every output item, including calls that arrived only
// in this snapshot. Already streamed tools must agree with the final output.
func (r *streamRelay) emitTerminal(payload map[string]any, event, data string) error {
	eventType := stringValue(payload["type"])
	response := objectValue(payload["response"])
	if response == nil {
		if r.rc2Terminals() {
			return fail(502, "invalid_upstream_response", "Basis Points terminal event is missing its response object")
		}
		// h22: nothing to validate it against, so it ends the stream as it
		// came -- unless a relay call is still open or was already delivered.
		if r.pendingRelayWork() || len(r.relayIndexes) > 0 {
			return invalidStreamToolCall("terminal_item_missing")
		}
		r.writeRaw(event, data)
		r.terminal = eventType
		return nil
	}
	if r.rc2Terminals() {
		// Priced before the caller's echo is restored and the non-standard
		// usage details are dropped, so what is priced is what the upstream
		// reported.
		r.service.recordResponse(r.target, r.model, response)
		if eventType == "response.failed" || eventType == "response.incomplete" {
			return r.emitUnsuccessfulTerminal(payload, event)
		}
		if status := stringValue(response["status"]); status != "" && status != "completed" {
			return fail(502, "invalid_upstream_response", "Basis Points terminal event has an inconsistent response status")
		}
		response["status"] = "completed"
		for _, entry := range r.held {
			if entry.unfinished {
				return invalidStreamToolCall("incomplete_item")
			}
		}
	} else {
		// h22: every terminal kind is validated and converted the same way.
		if eventType == "response.completed" {
			response["status"] = "completed"
		}
		r.service.recordResponse(r.target, r.model, response)
	}
	originalOutput, _ := response["output"].([]any)
	if r.noCatalogTerminalApplies(originalOutput) {
		// a1wide2: run_officejs calls no client tool can stand for
		// (no_catalog_transport.go). Not handled: the h22 path below fails
		// the response as before.
		if handled, err := r.noCatalogTerminal(payload, event, eventType, response, originalOutput); handled || err != nil {
			return err
		}
	}
	for _, value := range originalOutput {
		if item := objectValue(value); item != nil && isTransportCall(item) {
			if status := stringValue(item["status"]); status != "" && status != "completed" {
				return invalidStreamToolCall("incomplete_item")
			}
		}
	}
	_, converted, _, err := transformResponseBodyChecked(jsonBytes(response), r.source, false, r.policy)
	if err != nil {
		return err
	}
	output, _ := converted["output"].([]any)
	r.restoreMissingTerminalIDs(originalOutput, output)
	for index, final := range r.knownToolItems() {
		relayed := r.relayIndexes[index]
		// A relay call that the terminal contradicts is invalid, as in h22. A
		// native call's terminal snapshot was never compared by h22: that is a
		// tool_policy_checks finding.
		check := func(reason string) error {
			if relayed {
				return invalidStreamToolCall(reason)
			}
			return r.policy(reason)
		}
		if index >= len(output) {
			if err := check("terminal_item_missing"); err != nil {
				return err
			}
			continue
		}
		if relayed && index < len(originalOutput) {
			// rc2 also compares the upstream transport identity itself, which
			// h22 only saw through the translated ids: a policy finding.
			if native := r.nativeByIndex[index]; native != nil && !matchesAnnouncedStreamItem(native, objectValue(originalOutput[index])) {
				if err := r.policy("terminal_id_mismatch"); err != nil {
					return err
				}
			}
		}
		if reason := streamToolDifference(final, objectValue(output[index])); reason != "" {
			if err := check(reason); err != nil {
				return err
			}
			continue
		}
		// Semantic equality permits formatting differences but the client
		// should receive one stable spelling in lifecycle and terminal
		// history. Keep the previously validated done-item argument bytes.
		if stringValue(final["type"]) == "function_call" {
			if stringValue(objectValue(output[index])["arguments"]) != stringValue(final["arguments"]) {
				if relayed {
					// h22 compared these bytes and failed the relay call (E3).
					countRelayEvent("terminal_args_equivalent")
				} else {
					// h22 passed a native call's terminal bytes through
					// unchecked; the client now sees the done spelling in
					// the terminal snapshot too (E3, native; review EH-3).
					countRelayEvent("terminal_args_equivalent_native")
				}
			}
			objectValue(output[index])["arguments"] = final["arguments"]
		}
	}
	// A final snapshot can complete a held call, but it cannot silently erase
	// or change a call that was announced as a transport tool.
	for index, entry := range r.held {
		if index >= len(output) || index >= len(originalOutput) {
			return invalidStreamToolCall("terminal_item_missing")
		}
		item := objectValue(originalOutput[index])
		if item == nil || (entry.transport && !isTransportCall(item)) || !matchesAnnouncedStreamItem(r.announced[index], item) {
			return invalidStreamToolCall("identity_mismatch")
		}
		if kind := stringValue(item["type"]); kind != "function_call" && kind != "custom_tool_call" {
			return invalidStreamToolCall("identity_mismatch")
		}
	}
	for index, value := range output {
		item := objectValue(value)
		if index >= len(originalOutput) || !matchesAnnouncedStreamItem(r.announced[index], objectValue(originalOutput[index])) {
			return invalidStreamToolCall("terminal_id_mismatch")
		}
		if err := r.reserveToolIdentity(index, item); err != nil {
			return err
		}
	}
	// Only a fully validated snapshot is queued for replay history. The
	// actual cache write waits for host.stream.emit to accept its payload;
	// validation alone does not mean the client received executable calls.
	for index, value := range originalOutput {
		item := objectValue(value)
		if item == nil {
			continue
		}
		if isTransportCall(item) {
			// immediate (h22): the terminal's own transport item is what
			// replay history keeps, exactly as h22 remembered it here after
			// the done; the client call is the one already validated equal.
			// batch (rc2): the item that was validated at its done.
			if pending := r.nativeByIndex[index]; pending != nil && r.batchDelivery() {
				item = pending
			}
			r.cacheAfterFlush = append(r.cacheAfterFlush, relayCacheWrite{native: cloneObject(item), client: cloneObject(objectValue(output[index]))})
		} else if isStreamToolItem(item) && r.deliveredByIndex[index] == nil {
			// Terminal-only native calls also become visible in this host
			// payload. Do not synthesize a new lifecycle or guess their origin.
			r.queueNativeProvenance(objectValue(output[index]))
		}
	}
	r.held = map[int]*heldTransportCall{}
	// Validation is complete for the entire snapshot before emitting any
	// missing tool lifecycle. Clients need these events to execute tools even
	// when the upstream supplied them only in its terminal response object
	// (and, with batch delivery, for every relay call of the response).
	for index, value := range originalOutput {
		if item := objectValue(value); item != nil && isTransportCall(item) && r.deliveredByIndex[index] == nil {
			final := objectValue(output[index])
			r.deliveredByIndex[index] = cloneObject(final)
			r.relayIndexes[index] = true
			r.nativeByIndex[index] = cloneObject(item)
			r.completedItems[index] = true
			r.emitTranslatedCall(index, final)
		}
	}
	r.translatedByIndex = map[int]map[string]any{}
	// a1wide2: a continuation's terminal is the whole request's.
	if r.prior != nil && eventType == "response.completed" {
		countRelayEvent("no_catalog_continuation:answered")
	}
	mergeNoCatalogPrior(r.prior, converted)
	payload["response"] = converted
	r.writeEvent(eventName(event, payload), payload)
	r.terminal = eventType
	return nil
}

// noCatalogTerminalApplies: this terminal belongs to a request whose
// run_officejs calls are withheld, and it carries or followed one.
func (r *streamRelay) noCatalogTerminalApplies(output []any) bool {
	if len(r.withheld) > 0 {
		return true
	}
	for _, value := range output {
		if item := objectValue(value); item != nil && isTransportCall(item) {
			return r.withholdsTransport()
		}
	}
	return false
}

// noCatalogTerminal ends, or hands on, a response whose run_officejs calls are
// withheld (no_catalog_transport.go). A failed or incomplete turn is passed on
// without the calls. A completed one whose calls end it is carried on: the
// terminal is kept from the client and continuation is set for the relay loop.
// handled false: the calls cannot be answered, and the h22 path fails the
// response exactly as before.
func (r *streamRelay) noCatalogTerminal(payload map[string]any, event, eventType string, response map[string]any, output []any) (bool, error) {
	if eventType != "response.completed" {
		countRelayEvent("no_catalog_continuation:unsuccessful_terminal")
		return true, r.emitUnsuccessfulTerminal(payload, event)
	}
	visible, calls, reason := noCatalogPlan(output)
	if reason == "" {
		for index := range r.withheld {
			if index < len(visible) || index >= len(output) {
				reason = "not_trailing"
			}
		}
	}
	if reason == "" {
		for offset, call := range calls {
			index := len(visible) + offset
			if streamed := r.withheld[index]; streamed == nil {
				// A call that only the terminal snapshot carries.
				noteNoCatalogTransport(call)
			} else if !matchesAnnouncedStreamItem(streamed, call) {
				return true, invalidStreamToolCall("terminal_id_mismatch")
			}
		}
		for index, value := range visible {
			if !matchesAnnouncedStreamItem(r.announced[index], objectValue(value)) {
				return true, invalidStreamToolCall("terminal_id_mismatch")
			}
		}
	}
	if reason == "" && r.continued >= noCatalogContinuationLimit {
		reason = "limit"
	}
	if reason == "" && (r.call == nil || r.call.open == nil) {
		reason = "unavailable"
	}
	var next map[string]any
	if reason == "" {
		var ok bool
		if next, ok = noCatalogContinuationBody(r.call.body, output, r.call.clientShapes); !ok {
			reason = "unbuildable"
		}
	}
	if reason != "" {
		countRelayEvent("no_catalog_continuation:" + reason)
		return false, nil
	}
	shown := cloneObject(response)
	shown["output"] = append([]any{}, visible...)
	_, converted, _, err := transformResponseBodyChecked(jsonBytes(shown), r.source, false, r.policy)
	if err != nil {
		return true, err
	}
	convertedOutput, _ := converted["output"].([]any)
	r.continuation = &noCatalogContinuation{
		body:  next,
		prior: nextNoCatalogPrior(r.prior, converted["id"], convertedOutput, objectValue(converted["usage"])),
	}
	r.discardPendingTransport()
	countRelayEvent("no_catalog_continuation:started")
	r.service.logNoCatalog(r.call.callbackID, "continue", map[string]any{
		"calls": len(calls), "visible_items": len(visible), "continued": r.continued + 1,
	})
	return true, nil
}

// continuationRelay is the relay that carries this request's stream on with
// the upstream response to continuation's body.
func (r *streamRelay) continuationRelay(continuation *noCatalogContinuation) *streamRelay {
	next := newStreamRelay(r.service, r.streamID, r.source, r.target, r.model)
	next.progress = r.progress
	next.sequence = r.sequence
	next.receivedBytes = r.receivedBytes
	next.toolDelivered = r.toolDelivered
	next.lastEmit = r.lastEmit
	next.noCatalog = r.noCatalog
	next.call = &upstreamCall{body: continuation.body, open: r.call.open, callbackID: r.call.callbackID, clientShapes: r.call.clientShapes}
	next.continued = r.continued + 1
	next.prior = continuation.prior
	next.indexOffset = len(continuation.prior.output)
	next.suppressMeta = true
	// h27: a continuation is never held; a zero-output server error is still
	// switched away when nothing at all has reached the client.
	next.wsSwitchOn = r.wsSwitchOn
	return next
}

// A failed/incomplete response is evidence of failure, not a second inventory
// of executable calls (relay_incomplete: passthrough). It may omit output
// entirely. Preserve that failure and its cause, but never expose internal
// transport payloads or complete pending tools from this snapshot. Previously
// emitted calls and replay cache stay as they were; in particular no retry or
// synthetic tool lifecycle is generated.
func (r *streamRelay) emitUnsuccessfulTerminal(payload map[string]any, event string) error {
	response := objectValue(payload["response"])
	restoreCallerEcho(response, r.source)
	dropNonStandardUsageDetails(response)
	if output, ok := response["output"].([]any); ok {
		visible := make([]any, 0, len(output))
		for _, value := range output {
			if item := objectValue(value); item != nil && isTransportCall(item) {
				continue
			}
			visible = append(visible, value)
		}
		response["output"] = visible
	}
	r.discardPendingTransport()
	// a1wide2: a continuation's terminal is the whole request's.
	mergeNoCatalogPrior(r.prior, response)
	r.writeEvent(eventName(event, payload), payload)
	r.terminal = stringValue(payload["type"])
	return nil
}

// Only a missing optional id can be recovered. Both the call identity and its
// output position must uniquely identify a validated done item (pending relay
// or delivered tool). A present id, conflicting name or duplicate is never
// repaired. Restore the upstream id separately from a custom call's ctc_ id.
func (r *streamRelay) restoreMissingTerminalIDs(original, converted []any) {
	known := r.knownToolItems()
	for index, value := range original {
		item := objectValue(value)
		if index >= len(converted) || !isStreamToolItem(item) {
			continue
		}
		if id, exists := item["id"]; exists && id != nil {
			continue
		}
		call := objectValue(converted[index])
		expected := known[index]
		if !sameStreamCallIdentity(expected, call) || stringValue(expected["call_id"]) == "" {
			continue
		}
		matches := 0
		for _, emitted := range known {
			if sameStreamCallIdentity(emitted, call) {
				matches++
			}
		}
		if matches != 1 || stringValue(expected["id"]) == "" {
			continue
		}
		call["id"] = expected["id"]
		if native := r.nativeByIndex[index]; native != nil {
			// A relay call h22 failed for its missing terminal id (E3).
			countRelayEvent("terminal_id_restored")
			if id, exists := native["id"]; exists {
				item["id"] = id
			}
		} else {
			// A native (Lite) call: h22 failed it too, its announced id not
			// matching the terminal item (E3, native; review EH-3).
			countRelayEvent("terminal_id_restored_native")
			item["id"] = expected["id"]
		}
	}
}

func (r *streamRelay) knownToolItems() map[int]map[string]any {
	known := make(map[int]map[string]any, len(r.deliveredByIndex)+len(r.translatedByIndex))
	for index, item := range r.deliveredByIndex {
		known[index] = item
	}
	for index, item := range r.translatedByIndex {
		known[index] = item
	}
	return known
}

// discardPendingTransport drops relay calls that were never delivered. Replay
// records already queued belong to calls that were written to the client and
// are kept: they reach the cache only if the host accepts those bytes.
func (r *streamRelay) discardPendingTransport() {
	r.held = map[int]*heldTransportCall{}
	r.translatedByIndex = map[int]map[string]any{}
}

func (r *streamRelay) queueNativeProvenance(item map[string]any) {
	if status := stringValue(item["status"]); status != "" && status != "completed" {
		return
	}
	if _, known := knownNativeToolSpecIn(item, r.source, r.declaredTools); !known || isOpaqueToolHistory(item) {
		return
	}
	r.nativeAfterFlush = append(r.nativeAfterFlush, cloneObject(item))
}

func matchesAnnouncedStreamItem(announced, final map[string]any) bool {
	for _, field := range []string{"id", "call_id"} {
		if value := stringValue(announced[field]); value != "" && value != stringValue(final[field]) {
			return false
		}
	}
	return true
}

func sameStreamToolCall(left, right map[string]any) bool {
	return streamToolDifference(left, right) == ""
}

func sameStreamCallIdentity(left, right map[string]any) bool {
	if left == nil || right == nil {
		return false
	}
	for _, field := range []string{"type", "call_id", "name", "namespace"} {
		if stringValue(left[field]) != stringValue(right[field]) {
			return false
		}
	}
	return true
}

func streamToolDifference(left, right map[string]any) string {
	if right == nil {
		return "terminal_item_missing"
	}
	if !sameStreamCallIdentity(left, right) {
		return "identity_mismatch"
	}
	if stringValue(left["id"]) != stringValue(right["id"]) {
		return "terminal_id_mismatch"
	}
	if stringValue(left["type"]) == "function_call" {
		a, aOK := left["arguments"].(string)
		b, bOK := right["arguments"].(string)
		if !aOK || !bOK {
			return "terminal_args_changed"
		}
		switch toolArgumentsCompatibility(a, b) {
		case schemaUnknown:
			return "terminal_args_unverified"
		case schemaMismatch:
			return "terminal_args_changed"
		}
	} else {
		a, aOK := left["input"].(string)
		b, bOK := right["input"].(string)
		if !aOK || !bOK || a != b {
			return "terminal_args_changed"
		}
	}
	return ""
}

func (r *streamRelay) finish() error {
	if r.wsFailover && r.wsHolding.Load() {
		r.webSearchRelease()
	}
	// Partial transport calls are never executable and must never be replayed.
	if r.terminal == "" && r.pendingRelayWork() {
		return withToolDiagnostic(r.truncatedRelay(), "stream_end", -1, r.toolDelivered || len(r.deliveredByIndex) > 0)
	}
	if r.terminal != "" && !r.sawDone {
		r.out.WriteString("data: [DONE]\n\n")
		r.sawDone = true
	}
	if r.terminal != "" && r.progress != nil {
		r.progress.terminal.Store(r.terminal)
	}
	if err := r.flush(); err != nil {
		return err
	}
	// response.failed, response.incomplete and error are terminal events just
	// as response.completed is: the failure has already reached the caller
	// in-band. Reporting such a stream as broken as well turned every
	// context_length_exceeded into a 502, which the main site charges to the
	// credential as an upstream fault instead of to the request -- 105 of
	// them in one day. Only a stream that stops without any terminal event
	// is genuinely cut short.
	if r.terminal == "" {
		return fail(502, "invalid_upstream_response", "Basis Points stream ended without a terminal event")
	}
	return nil
}

func (r *streamRelay) flush() error {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.wsHolding.Load() {
		return nil
	}
	if r.out.Len() == 0 {
		return nil
	}
	payload := []byte(r.out.String())
	r.out.Reset()
	if err := r.service.call("host.stream.emit", map[string]any{"stream_id": r.streamID, "payload": payload}, nil); err != nil {
		r.cacheAfterFlush = nil
		r.nativeAfterFlush = nil
		return fail(499, "client_disconnected", "client disconnected while receiving stream")
	}
	r.lastEmit = time.Now()
	if r.pendingOutput {
		r.pendingOutput = false
		if r.progress != nil {
			r.progress.outputSent.Store(true)
		}
	}
	for _, pending := range r.cacheAfterFlush {
		rememberRelayCallFor(r.source, pending.native, pending.client)
	}
	for _, item := range r.nativeAfterFlush {
		rememberDeliveredNativeCallFor(r.source, item)
	}
	r.cacheAfterFlush = nil
	r.nativeAfterFlush = nil
	return nil
}

func (r *streamRelay) writeRaw(event, data string) {
	if payload := r.decodeEvent(data); payload != nil {
		r.writeEvent(eventName(event, payload), payload)
		return
	}
	r.noteWritten(event, nil)
	if event != "" {
		r.out.WriteString("event: ")
		r.out.WriteString(event)
		r.out.WriteString("\n")
	}
	r.out.WriteString("data: ")
	r.out.WriteString(data)
	r.out.WriteString("\n\n")
}

func (r *streamRelay) writeEvent(name string, payload any) {
	r.noteWritten(name, objectValue(payload))
	if value := objectValue(payload); value != nil {
		frame := cloneObject(value)
		frame["sequence_number"] = r.sequence
		r.sequence++
		if r.indexOffset != 0 {
			// a1wide2: a continuation's items follow the ones the client
			// already holds (no_catalog_transport.go).
			if index := eventOutputIndex(frame); index >= 0 {
				frame["output_index"] = index + r.indexOffset
			}
		}
		if stringValue(frame["type"]) == "" {
			frame["type"] = name
		}
		writeSSE(&r.out, name, frame)
		return
	}
	writeSSE(&r.out, name, payload)
}

// noteWritten keeps the two facts E5 and tool_policy_checks need about what
// the client is being sent: whether it is output at all (anything but the
// response.created / response.in_progress metadata, which the main site does
// not count as output either), and whether a complete client tool call is
// among it.
//
// response.created / response.in_progress are metadata only while their
// response.output is missing, null or empty. A snapshot that already carries
// output items has handed the client that output (h25 review item 7), so a
// failure after it is no zero-output failure and gets no E5 retry hint.
func (r *streamRelay) noteWritten(name string, payload map[string]any) {
	if name == "" {
		name = stringValue(payload["type"])
	}
	switch name {
	case "response.created", "response.in_progress":
		if !snapshotCarriesOutput(payload) {
			return
		}
	}
	r.pendingOutput = true
	if name == "response.output_item.done" {
		if item := objectValue(payload["item"]); isStreamToolItem(item) && !isTransportCall(item) {
			r.toolDelivered = true
		}
	}
}

// snapshotCarriesOutput: the event's response.output is present and is not an
// empty list. A value that is not a list at all is counted as output too, so
// that nothing the client may have read as output can still earn a hint.
func snapshotCarriesOutput(payload map[string]any) bool {
	response := objectValue(payload["response"])
	if response == nil {
		return false
	}
	switch output := response["output"].(type) {
	case nil:
		return false
	case []any:
		return len(output) > 0
	default:
		return true
	}
}

// relayHeartbeatFrame is an SSE comment: every SSE reader ignores it, and it
// is not an event, so it is neither output nor a sequence number.
const relayHeartbeatFrame = ": keepalive\n\n"

// relayHeartbeatInterval is how long batch delivery may withhold relay calls
// without sending the client anything; relayHeartbeatPoll is how often the
// heartbeat looks. Variables only so tests need not wait.
var (
	relayHeartbeatInterval = 15 * time.Second
	relayHeartbeatPoll     = time.Second
)

// startHeartbeat runs only with relay_tool_delivery: batch. host.http.stream_read
// blocks until the upstream sends more, so a comment can only come from a
// second goroutine; emitMu keeps it between, never inside, the relay's frames.
// The returned stop waits for that goroutine, so no heartbeat can follow the
// relay's last emit or the host.stream.close after it.
func (r *streamRelay) startHeartbeat() func() {
	stop := make(chan struct{})
	var done sync.WaitGroup
	done.Add(1)
	// Read here, on the relay's goroutine; the heartbeat never touches source.
	notes, _ := r.source[relayDecodeNotesKey].(*relayDecodeNotes)
	go func() {
		defer done.Done()
		// Ancillary: a failing host must not take the gateway down. A
		// heartbeat that stops before its stream ends is counted and logged
		// (batch_heartbeat_stopped), so a stream that later times out while
		// calls were held can be told apart from one that was kept alive.
		defer func() {
			if recover() != nil {
				r.heartbeatStopped(notes, "panic")
			}
		}()
		ticker := time.NewTicker(relayHeartbeatPoll)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !r.heartbeat() {
					r.heartbeatStopped(notes, "emit_failed")
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		done.Wait()
	}
}

// heartbeat reports whether the heartbeat should keep running.
func (r *streamRelay) heartbeat() bool {
	if !r.holding.Load() && !r.wsHolding.Load() {
		return true
	}
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if time.Since(r.lastEmit) < relayHeartbeatInterval {
		return true
	}
	if err := r.service.call("host.stream.emit", map[string]any{"stream_id": r.streamID, "payload": []byte(relayHeartbeatFrame)}, nil); err != nil {
		// The relay finds the same failure on its own next emit.
		return false
	}
	r.lastEmit = time.Now()
	countRelayEvent("batch_heartbeat")
	return true
}

// heartbeatStopped counts and logs the batch heartbeat giving up early. The
// stream itself carries on; the relay meets a failed emit on its own next
// write. Neither the count nor the log may take the gateway down.
func (r *streamRelay) heartbeatStopped(notes *relayDecodeNotes, reason string) {
	defer func() { _ = recover() }()
	countRelayEvent("batch_heartbeat_stopped")
	if notes != nil && notes.heartbeatStopped != nil {
		notes.heartbeatStopped(reason)
	}
}

// eventName keeps the upstream's SSE event label when it had one, falling back
// to the payload's own type.
func eventName(event string, payload map[string]any) string {
	if event != "" {
		return event
	}
	return stringValue(payload["type"])
}

// decodeEventPayload reads one event's JSON and refuses anything after it.
func decodeEventPayload(data string) map[string]any {
	return decodeEventPayloadWith(data, true)
}

// decodeEventPayloadWith: strict is rc2's reading (bytes after the JSON value
// make the event invalid); otherwise h22's, which read the first JSON value.
func decodeEventPayloadWith(data string, strict bool) map[string]any {
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil
	}
	if !strict {
		return payload
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil
	}
	return payload
}

func eventOutputIndex(payload map[string]any) int {
	switch typed := payload["output_index"].(type) {
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return int(number)
		}
	case float64:
		if typed >= 0 && typed == float64(int(typed)) {
			return int(typed)
		}
	}
	return -1
}

// relayUpstreamStream pumps the upstream stream to the caller as it arrives.
func (s *Service) relayUpstreamStream(streamID string, upstream upstreamStream, source map[string]any, target usageTarget, model string) error {
	return s.relayUpstreamStreamWith(streamID, upstream, source, target, model, nil)
}

// relayUpstreamStreamWith also records in progress what the client was sent.
func (s *Service) relayUpstreamStreamWith(streamID string, upstream upstreamStream, source map[string]any, target usageTarget, model string, progress *streamProgress) error {
	return s.relayUpstreamStreamCall(streamID, upstream, source, target, model, progress, nil)
}

// relayUpstreamStreamCall: call, when set, lets withheld run_officejs calls be
// answered upstream (no_catalog_transport.go); each continuation's stream is
// pumped into the same client stream by the relay that carries it on. All of
// them share one deadline and one response size budget.
func (s *Service) relayUpstreamStreamCall(streamID string, upstream upstreamStream, source map[string]any, target usageTarget, model string, progress *streamProgress, call *upstreamCall) error {
	cfg := s.config()
	relay := newStreamRelay(s, streamID, source, target, model)
	relay.progress = progress
	relay.call = call
	relay.wsFailover, relay.wsHoldAll = webSearchFailoverFor(cfg, progressScope(progress))
	relay.wsSwitchOn = cfg.WebSearchFailover
	deadline := time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	total := 0
	for {
		if err := s.pumpUpstream(relay, upstream, cfg, deadline, &total); err != nil {
			return err
		}
		continuation := relay.continuation
		if continuation == nil {
			return nil
		}
		next, err := relay.call.open(continuation.body)
		if err != nil {
			countRelayEvent("no_catalog_continuation:open_failed")
			return err
		}
		relay, upstream = relay.continuationRelay(continuation), next
	}
}

// pumpUpstream feeds one upstream stream to relay until the relay ends the
// stream or asks for a continuation (relay.continuation set, nil returned),
// and closes that upstream stream either way.
func (s *Service) pumpUpstream(relay *streamRelay, upstream upstreamStream, cfg Config, deadline time.Time, total *int) error {
	if upstream.StreamID == "" {
		return fail(502, "upstream_transport", "upstream stream ID is empty")
	}
	defer func() { _ = s.call("host.http.stream_close", map[string]any{"stream_id": upstream.StreamID}, nil) }()
	if relay.batchDelivery() || relay.wsFailover {
		stop := relay.startHeartbeat()
		defer stop()
	}
	for {
		if time.Now().After(deadline) {
			return timeoutError(cfg)
		}
		var chunk streamChunk
		if err := s.call("host.http.stream_read", map[string]any{"stream_id": upstream.StreamID}, &chunk); err != nil {
			return fail(502, "upstream_transport", "Basis Points stream read failed: "+safeError(err))
		}
		if chunk.Error != "" {
			return fail(502, "upstream_transport", "Basis Points stream interrupted: "+safeError(errors.New(chunk.Error)))
		}
		if len(chunk.Payload) > 0 {
			*total += len(chunk.Payload)
			if *total > cfg.MaxResponseBytes {
				return fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
			}
			// Experiment capture of the raw upstream SSE (bpsexp build only),
			// keyed by the client stream id. A no-op in the production build.
			expRecordStreamChunk(relay.streamID, chunk.Payload)
			if err := relay.feed(chunk.Payload); err != nil {
				return err
			}
			// Emitting once per upstream chunk rather than once per event keeps
			// host round trips proportional to the upstream's own chunking,
			// which is far coarser than its event count, without holding
			// anything back.
			if err := relay.flush(); err != nil {
				return err
			}
			// a1wide2: the turn goes on in another upstream call.
			if relay.continuation != nil {
				return nil
			}
			// Once a terminal event has been sent, closing the upstream cannot
			// turn it into a second, contradictory stream failure.
			if relay.terminal != "" {
				return relay.finish()
			}
		}
		if chunk.Done {
			return relay.finish()
		}
	}
}

// streamDrainTimeout bounds how long plugin.shutdown waits for relays to stop.
// Long enough that a relay mid-answer finishes normally, short enough that one
// wedged upstream cannot hold the gateway's shutdown open indefinitely.
const streamDrainTimeout = 15 * time.Second

// drainStreams waits for in-flight relay goroutines to stop calling the host.
//
// The plugin and CPA are two independent Go runtimes sharing one process, and a
// relay reaches the host through a cgo callback. Tearing the host binding down
// while a relay is mid-call crosses back into a runtime on a thread that is
// already executing Go, which the scheduler reports as
// "fatal error: wirep: already in go" -- observed taking the whole gateway down
// during plugin reloads. That is a runtime fatal error, not a panic, so no
// amount of recover() in the exported entry points can catch it; not racing in
// the first place is the only fix. It reports whether every relay finished.
func (s *Service) drainStreams(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.streams.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
