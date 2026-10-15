package basispoints

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// These are code-owned labels, not user or upstream strings. Unknown labels
// become "unspecified" so future callers cannot accidentally log payloads.
var toolRejectReasons = map[string]bool{
	"unspecified":     true,
	"incomplete_item": true, "invalid_envelope": true,
	"outer_json": true, "code_json": true, "missing_tool_name": true,
	"invalid_references": true, "ambiguous_references": true,
	"required_tool_missing": true, "forced_tool_count": true,
	"parallel_tool_count": true,
	"native_tool_not_allowed": true, "native_delta_identity_mismatch": true, "native_event_type_mismatch": true,
	"tool_not_in_catalog": true, "tool_not_allowed": true,
	"invalid_custom_input": true, "invalid_arguments_json": true,
	"schema_mismatch": true, "duplicate_identity": true,
	"invalid_index": true, "identity_mismatch": true,
	"terminal_item_missing": true, "terminal_id_missing": true,
	"terminal_id_mismatch": true, "terminal_args_changed": true,
	"terminal_args_unverified": true,
	"missing_terminal_event":   true, "early_transport": true,
	"unexpected_transport_event": true, "announced_identity_mismatch": true,
	"invalid_stream_tool": true, "relay_decode_failed": true,
	// E4: a blocked encrypted tool whose fallback name is declared too, or
	// whose full key the request also declares for another tool.
	"encrypted_tool_blocked": true,
}

var toolDiagnosticEvents = map[string]bool{
	"response.created": true, "response.in_progress": true,
	"response.output_item.added": true, "response.output_item.done": true,
	"response.function_call_arguments.delta": true,
	"response.function_call_arguments.done":  true,
	"response.custom_tool_call_input.delta":  true,
	"response.custom_tool_call_input.done":   true,
	"response.completed":                     true, "response.failed": true,
	"response.incomplete": true, "error": true,
	"non_stream_response": true, "stream_end": true,
}

type toolRejection struct {
	cause     *APIError
	reason    string
	reference string
	event     string
	index     int
	delivered bool
}

func (e *toolRejection) Error() string   { return e.cause.Error() }
func (e *toolRejection) Unwrap() error   { return e.cause }
func (e *toolRejection) StatusCode() int { return e.cause.Status }
func (e *toolRejection) Code() string    { return e.cause.Kind }

func toolReject(reason string) error {
	return newToolRejection(reason, false)
}

func streamIncomplete() error {
	return newToolRejection("missing_terminal_event", true)
}

func newToolRejection(reason string, incomplete bool) error {
	if !toolRejectReasons[reason] {
		reason = "unspecified"
	}
	var random [8]byte
	reference := "unavailable"
	if _, err := rand.Read(random[:]); err == nil {
		reference = "bpdiag_" + hex.EncodeToString(random[:])
	}
	status, kind, message := 422, "invalid_tool_call", "upstream tool response could not be safely converted"
	if incomplete {
		status, kind, message = 502, "invalid_upstream_response", "upstream stream ended without a terminal response"
	}
	return &toolRejection{
		cause: &APIError{Status: status, Kind: kind, Message: fmt.Sprintf(
			"%s [reason=%s ref=%s]: %s", kind, reason, reference, message)},
		reason: reason, reference: reference, event: "unknown", index: -1,
	}
}

func withToolDiagnostic(err error, event string, index int, delivered bool) error {
	var rejection *toolRejection
	if !errors.As(err, &rejection) {
		return err
	}
	copy := *rejection
	if !toolDiagnosticEvents[event] {
		event = "unknown"
	}
	if index < -1 || index > 65535 {
		index = -1
	}
	copy.event, copy.index, copy.delivered = event, index, delivered
	return &copy
}

// No names, schemas, arguments, input/output text, ciphertext, credential IDs,
// headers, or user-supplied identifiers are copied into diagnostic fields.
func toolDiagnosticFields(err error, source map[string]any) (map[string]any, bool) {
	var rejection *toolRejection
	if !errors.As(err, &rejection) {
		return nil, false
	}
	fields := map[string]any{
		"event": "relay_tool_rejected", "plugin_version": Version,
		"relay_protocol": relayProtocolForDecode(source), "relay_tool_delivery": relayOptionsFrom(source).ToolDelivery,
		"reason": rejection.reason, "diagnostic_ref": rejection.reference,
		"stage": rejection.event, "output_index": rejection.index,
		"tool_already_delivered": rejection.delivered,
		"failure_kind":           rejection.cause.Kind, "status_code": rejection.cause.Status,
	}
	if scope, present := replayScopeFromSource(source); present {
		fields["replay_cache_scoped"] = scope != ""
	}
	if rejection.cause.Kind == "invalid_upstream_response" {
		fields["event"] = "relay_stream_incomplete"
	}
	counts, truncated := diagnosticCatalogueCounts(source)
	for key, value := range counts {
		fields[key] = value
	}
	fields["catalogue_count_truncated"] = truncated
	return fields, true
}

func diagnosticCatalogueCounts(source map[string]any) (map[string]int, bool) {
	counts := map[string]int{
		"top_function_count": 0, "top_custom_count": 0,
		"additional_function_count": 0, "additional_custom_count": 0,
		"search_function_count": 0, "search_custom_count": 0,
	}
	remaining, truncated := 4096, false
	var visit func(any, string, int)
	visit = func(raw any, prefix string, depth int) {
		items, _ := raw.([]any)
		for _, value := range items {
			if remaining <= 0 || depth > 16 {
				truncated = true
				return
			}
			remaining--
			item := objectValue(value)
			switch stringValue(item["type"]) {
			case "function":
				counts[prefix+"_function_count"]++
			case "custom":
				counts[prefix+"_custom_count"]++
			case "namespace":
				visit(item["tools"], prefix, depth+1)
			}
		}
	}
	visit(source["tools"], "top", 0)
	items, _ := source["input"].([]any)
	for _, value := range items {
		if remaining <= 0 {
			truncated = true
			break
		}
		remaining--
		item := objectValue(value)
		switch stringValue(item["type"]) {
		case "additional_tools":
			visit(item["tools"], "additional", 0)
		case "tool_search_output":
			visit(item["tools"], "search", 0)
		}
	}
	return counts, truncated
}

func (s *Service) logToolRejection(callbackID string, source map[string]any, err error) {
	// Logging is ancillary: even a panicking host callback must not replace the
	// original failure or strand a stream/concurrency slot.
	defer func() { _ = recover() }()
	fields, ok := toolDiagnosticFields(err, source)
	if !ok {
		return
	}
	// fc7's text formatter prints only selected structured fields. Preserve
	// the complete, already projected safe record in the message as well, so
	// reason/ref/stage can actually be recovered from main.log.
	message := "basispoints relay failure " + string(jsonBytes(fields))
	_ = s.call("host.log", map[string]any{
		"host_callback_id": callbackID,
		"level":            "warn", "message": message,
		"fields": fields,
	}, nil)
}
