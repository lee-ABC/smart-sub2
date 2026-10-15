package basispoints

import (
	"strconv"
	"strings"
)

// relay_no_catalog_transport (a1wide2, 2026-10-15).
//
// A request with no client tool the model may call -- no tools at all, or
// tool_choice "none" -- has nothing a run_officejs call could stand for. The
// upstream offers its own run_officejs all the same, and the model used it in
// about one such request in a thousand: 146 of 129,326 between the restart on
// 2026-10-13 and a1wide, nearly all of them Codex compacting a long
// conversation (Codex sends its compaction request without tools, and the
// history it is asked to summarise is full of tool calls). h22 decodes that
// call as a relay call, which can only fail -- an empty catalogue has nothing
// to deliver -- so the stream ended in invalid_tool_call after the model's
// reasoning had already reached the client, the main site could not switch
// accounts, and Codex sent the whole ~200k-token request again.
//
//   error (default)     h22: decoded as a relay call.
//   continue            the call is never delivered and never decoded as a
//                       relay call. If it is what ended the upstream's turn,
//                       the plugin does what the add-in does after a tool call:
//                       it sends the turn back on the same credential with the
//                       call answered -- unavailable, nothing ran, answer as
//                       text -- and streams the model's continuation to the
//                       client as the rest of the same response (one
//                       response.created, one terminal, output indexes and
//                       sequence numbers carried on, usage of every upstream
//                       response summed). At most noCatalogContinuationLimit
//                       times per request. A failed or incomplete upstream
//                       terminal is passed on with the call left out.
//                       Anything else this cannot answer -- more output after
//                       the call, a call the client still has to answer, the
//                       limit reached -- fails exactly as h22 did.
//
// Nothing else changes: a request with client tools, and a tool-less request
// whose model never calls run_officejs, takes the h22/a1wide path. Like every
// relay switch, the default is h22's behaviour and deleting the line in
// config.yaml restores it.
//
// Counters (fixed labels, never request content):
//
//	no_catalog_transport:<shape>              each withheld call, by what its
//	    code looked like (noCatalogShape); the content itself is never kept
//	no_catalog_continuation:<stage>           started | answered (the
//	    continuation ended the turn) | unsuccessful_terminal | not_trailing |
//	    call_open | limit | unavailable | unbuildable | open_failed
const (
	noCatalogContinue = "continue"
	noCatalogError    = "error"

	// noCatalogContinuationLimit bounds the upstream calls one request may add.
	noCatalogContinuationLimit = 2
)

// noCatalogToolOutput answers a withheld call. It says what happened and what
// to do, in the words the add-in's own tool errors would use, and promises
// nothing about tools the client might have.
const noCatalogToolOutput = "run_officejs is not available here: this conversation is not connected to a workbook " +
	"and has no client tools, so nothing was run. Do not call run_officejs or any other tool again. " +
	"Continue, and give your complete answer as assistant text."

// noCatalogTransport reports whether run_officejs calls of this request are
// withheld and answered upstream: the switch says continue and no client tool
// is callable (relayOutputToolSpecs is empty, so a relay decode could never
// deliver anything).
func noCatalogTransport(source map[string]any) bool {
	if relayOptionsFrom(source).NoCatalogTransport != noCatalogContinue {
		return false
	}
	return len(relayOutputToolSpecs(source)) == 0
}

// noCatalogCandidate is the cheap test executeStream makes before it decides
// to keep the prepared body for a possible continuation: a request with no
// catalogue at all, or one whose tool_choice forbids every call.
func noCatalogCandidate(source map[string]any, kind string) bool {
	if relayOptionsFrom(source).NoCatalogTransport != noCatalogContinue {
		return false
	}
	return kind == "none" || toolChoiceIsNone(source)
}

// noteNoCatalogTransport counts one withheld call by its shape.
func noteNoCatalogTransport(native map[string]any) {
	defer func() { _ = recover() }()
	countRelayEvent("no_catalog_transport:" + noCatalogShape(native))
}

// noCatalogShape labels what a withheld run_officejs call carried, from fixed
// words only: no byte of the call is kept.
func noCatalogShape(native map[string]any) string {
	if stringValue(native["type"]) == "custom_tool_call" {
		return "custom_input"
	}
	arguments := parseArguments(native["arguments"])
	if arguments == nil {
		return "outer_unreadable"
	}
	code, present := arguments["code"]
	if !present {
		return "code_missing"
	}
	text, isString := code.(string)
	if !isString {
		if objectValue(code) != nil {
			return "code_object"
		}
		return "code_other_type"
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "code_empty"
	}
	if object := decodeTransportCode(text); object != nil {
		if stringValue(object["tool"]) != "" || stringValue(object["name"]) != "" {
			return "json_envelope"
		}
		return "json_other"
	}
	if strings.HasPrefix(trimmed, "{") {
		return "json_broken"
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.Contains(trimmed, "Excel.run") || strings.Contains(lower, "context.workbook") ||
		strings.Contains(trimmed, "Office.") || strings.Contains(trimmed, "Word.run") || strings.Contains(trimmed, "PowerPoint.run"):
		return "officejs"
	case strings.Contains(lower, "require(") || strings.Contains(lower, "child_process") || strings.Contains(lower, "process.env") ||
		strings.Contains(lower, "fs.read") || strings.Contains(lower, "fs.write"):
		return "node_js"
	case strings.Contains(trimmed, "=>") || strings.Contains(lower, "function") || strings.Contains(lower, "const ") ||
		strings.Contains(lower, "let ") || strings.Contains(lower, "await ") || strings.Contains(lower, "return ") ||
		strings.Contains(trimmed, ";"):
		return "javascript"
	}
	return "text"
}

// upstreamCall is what a stream needs to answer a withheld call upstream: the
// body that produced the response being relayed, and a way to open another
// stream for the same request on the same credential and identity.
type upstreamCall struct {
	body       map[string]any
	open       func(body map[string]any) (upstreamStream, error)
	callbackID string
	// clientShapes: the request's history went up in the add-in's own item
	// shapes (client_identity.client_shapes), so the continuation's does too.
	clientShapes bool
}

// requestClientShapes is the client_shapes decision request preparation made
// for this request and credential (prepareResponsesBodyCore).
func (s *Service) requestClientShapes(request ExecutorRequest, c credential) bool {
	_, mode := s.personaFor(request, c)
	return s.config().ClientIdentity.ClientShapes && mode >= identityHygiene
}

// noCatalogPrior is what earlier upstream responses of this request already
// gave the client: the id it saw in response.created, the converted items it
// holds (output indexes 0..len-1) and their summed usage.
type noCatalogPrior struct {
	id     any
	output []any
	usage  map[string]any
}

// noCatalogContinuation is a terminal's request for one more upstream call.
type noCatalogContinuation struct {
	body  map[string]any
	prior *noCatalogPrior
}

// noCatalogPlan checks a completed upstream response that carries run_officejs
// calls of a no-catalogue request. ok: every such call comes after everything
// else and no other call is waiting for the client's answer, so the turn can
// be carried on. visible is the output before the first call. reason is the
// counter label when it cannot.
func noCatalogPlan(output []any) (visible []any, calls []map[string]any, reason string) {
	first := -1
	for index, raw := range output {
		item := objectValue(raw)
		if item == nil {
			if first >= 0 {
				return nil, nil, "not_trailing"
			}
			continue
		}
		if isTransportCall(item) {
			if status := stringValue(item["status"]); status != "" && status != "completed" {
				// A truncated call is never answered (h22: incomplete_item).
				return nil, nil, "not_trailing"
			}
			if first < 0 {
				first = index
			}
			calls = append(calls, item)
			continue
		}
		if first >= 0 {
			return nil, nil, "not_trailing"
		}
		if isStreamToolItem(item) {
			// A call the client was handed has to be answered by the client;
			// a continuation sent without its output would be refused.
			return nil, nil, "call_open"
		}
	}
	if first < 0 {
		return nil, nil, "not_trailing"
	}
	return output[:first], calls, ""
}

// noCatalogContinuationBody is body carried on the way the add-in carries a
// turn after a tool call: the response's output appended to the input
// (reasoning rebuilt as the history path rebuilds it, clientShapes included),
// every run_officejs call answered as unavailable, agent_iteration one higher.
// Everything else -- the prologue, the cached prefix, prompt_cache_key, the
// identity -- is body's.
func noCatalogContinuationBody(body map[string]any, output []any, clientShapes bool) (map[string]any, bool) {
	input, ok := body["input"].([]any)
	if !ok {
		return nil, false
	}
	items := make([]any, 0, len(input)+len(output)+1)
	items = append(items, input...)
	var answers []any
	for _, raw := range output {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		switch {
		case stringValue(item["type"]) == "reasoning":
			if rebuilt := translatedReasoningItem(item, wireOptions{clientShapes: clientShapes}); rebuilt != nil {
				items = append(items, rebuilt)
			}
		case isTransportCall(item):
			callID := stringValue(item["call_id"])
			if callID == "" || stringValue(item["type"]) != "function_call" {
				return nil, false
			}
			items = append(items, cloneObject(item))
			answers = append(answers, map[string]any{"type": "function_call_output", "call_id": callID, "output": noCatalogToolOutput})
		default:
			items = append(items, cloneObject(item))
		}
	}
	if len(answers) == 0 {
		return nil, false
	}
	next := make(map[string]any, len(body))
	for key, value := range body {
		next[key] = value
	}
	next["input"] = append(items, answers...)
	if metadata := objectValue(body["metadata"]); metadata != nil {
		copied := cloneObject(metadata)
		if iteration, err := strconv.Atoi(stringValue(metadata["agent_iteration"])); err == nil {
			copied["agent_iteration"] = strconv.Itoa(iteration + len(answers))
		}
		next["metadata"] = copied
	}
	return next, true
}

// addUsage sums two Responses usage objects: input, output and total tokens
// and their cached / reasoning details. Either may be missing.
func addUsage(first, second map[string]any) map[string]any {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	sum := cloneObject(second)
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		sum[key] = numberValue(first[key]) + numberValue(second[key])
	}
	for _, pair := range [][2]string{{"input_tokens_details", "cached_tokens"}, {"output_tokens_details", "reasoning_tokens"}} {
		left, right := objectValue(first[pair[0]]), objectValue(second[pair[0]])
		if left == nil && right == nil {
			continue
		}
		details := cloneObject(right)
		if details == nil {
			details = map[string]any{}
		}
		details[pair[1]] = numberValue(left[pair[1]]) + numberValue(right[pair[1]])
		sum[pair[0]] = details
	}
	return sum
}

// mergeNoCatalogPrior turns response -- the last upstream response of the
// request -- into the response the client receives: the id it saw first, the
// output it already holds in front, the usage of every upstream response.
func mergeNoCatalogPrior(prior *noCatalogPrior, response map[string]any) {
	if prior == nil || response == nil {
		return
	}
	output, _ := response["output"].([]any)
	response["output"] = append(append([]any{}, prior.output...), output...)
	if prior.id != nil {
		response["id"] = prior.id
	}
	if usage := addUsage(prior.usage, objectValue(response["usage"])); usage != nil {
		response["usage"] = usage
	}
}

// nextNoCatalogPrior is prior extended by one more upstream response's
// visible output and usage.
func nextNoCatalogPrior(prior *noCatalogPrior, id any, visible []any, usage map[string]any) *noCatalogPrior {
	next := &noCatalogPrior{id: id, output: append([]any{}, visible...), usage: usage}
	if prior != nil {
		next.id = prior.id
		next.output = append(append([]any{}, prior.output...), visible...)
		next.usage = addUsage(prior.usage, usage)
	}
	return next
}

// stripTransportItems leaves the run_officejs calls out of a response's
// output.
func stripTransportItems(response map[string]any) {
	output, ok := response["output"].([]any)
	if !ok {
		return
	}
	visible := make([]any, 0, len(output))
	for _, value := range output {
		if item := objectValue(value); item != nil && isTransportCall(item) {
			continue
		}
		visible = append(visible, value)
	}
	response["output"] = visible
}

// answerNoCatalogNonStream is relay_no_catalog_transport for a non-streaming
// request: the same continuation, the upstream responses joined into the one
// response returned. body is the request that produced response. A response
// whose calls cannot be answered is returned as it is, and the h22 decode
// fails it as before.
func (s *Service) answerNoCatalogNonStream(request ExecutorRequest, body map[string]any, c credential, target usageTarget, response map[string]any) (map[string]any, error) {
	var prior *noCatalogPrior
	clientShapes := s.requestClientShapes(request, c)
	for continued := 0; ; continued++ {
		output, _ := response["output"].([]any)
		transport := false
		for _, raw := range output {
			if item := objectValue(raw); item != nil && isTransportCall(item) {
				transport = true
				noteNoCatalogTransport(item)
			}
		}
		if !transport {
			if prior != nil && stringValue(response["status"]) == "completed" {
				countRelayEvent("no_catalog_continuation:answered")
			}
			mergeNoCatalogPrior(prior, response)
			return response, nil
		}
		if status := stringValue(response["status"]); status != "" && status != "completed" {
			countRelayEvent("no_catalog_continuation:unsuccessful_terminal")
			stripTransportItems(response)
			mergeNoCatalogPrior(prior, response)
			return response, nil
		}
		visible, calls, reason := noCatalogPlan(output)
		if reason == "" && continued >= noCatalogContinuationLimit {
			reason = "limit"
		}
		var next map[string]any
		if reason == "" {
			var ok bool
			if next, ok = noCatalogContinuationBody(body, output, clientShapes); !ok {
				reason = "unbuildable"
			}
		}
		if reason != "" {
			countRelayEvent("no_catalog_continuation:" + reason)
			return response, nil
		}
		countRelayEvent("no_catalog_continuation:started")
		s.logNoCatalog(request.HostCallbackID, "continue", map[string]any{
			"calls": len(calls), "visible_items": len(visible), "continued": continued + 1, "stream": false,
		})
		answer, err := s.upstreamRequest(request, next, c, false)
		if err != nil {
			countRelayEvent("no_catalog_continuation:open_failed")
			return nil, err
		}
		nextResponse, err := parseFinalStreamResponse(answer.Body)
		if err != nil {
			return nil, err
		}
		s.recordResponse(target, request.Model, nextResponse)
		prior = nextNoCatalogPrior(prior, response["id"], visible, objectValue(response["usage"]))
		body, response = next, nextResponse
	}
}

// logNoCatalog writes one relay_no_catalog_transport line: fixed labels only.
func (s *Service) logNoCatalog(callbackID, stage string, extra map[string]any) {
	defer func() { _ = recover() }()
	fields := map[string]any{"event": "relay_no_catalog_transport", "plugin_version": Version, "stage": stage}
	for key, value := range extra {
		fields[key] = value
	}
	_ = s.call("host.log", map[string]any{
		"host_callback_id": callbackID, "level": "info",
		"message": "basispoints no-catalog transport " + string(jsonBytes(fields)),
		"fields":  fields,
	}, nil)
}
