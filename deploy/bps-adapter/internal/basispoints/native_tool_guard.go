package basispoints

import "strings"

// Native calls stay native. This guard uses only their declared routing
// metadata to enforce explicit caller restrictions; it never changes a name,
// namespace, input, ciphertext or history item to make a call fit.
//
// It is an rc2 tool_policy_checks finding (relay_policy.go): the stream and
// the response decoder ask nativeToolPolicyReason and let the switch decide.
func validateNativeToolCall(item, source map[string]any) error {
	if reason := nativeToolPolicyReason(item, source); reason != "" {
		return toolReject(reason)
	}
	return nil
}

func nativeToolPolicyReason(item, source map[string]any) string {
	return nativeToolPolicyReasonIn(item, source, nil)
}

// declaredCatalog supplies allDeclaredClientToolSpecs(source); nil means
// "compute it now". A stream passes its per-request cache (streamRelay.
// declaredTools): the request does not change while its response streams, so
// the catalogue -- every declaration in tools, additional_tools and
// tool_search_output -- is built once per stream instead of once per native
// argument fragment (review EH-1). The result is the same map either way.
type declaredCatalog func() map[string]toolSpec

func nativeToolPolicyReasonIn(item, source map[string]any, catalog declaredCatalog) string {
	if !isStreamToolItem(item) || isTransportCall(item) {
		return ""
	}
	if toolChoiceIsNone(source) {
		return "native_tool_not_allowed"
	}
	// An unrelated hosted/future tool in allowed_tools cannot widen an
	// explicitly declared function/custom tool's permissions. Conservatism
	// for unknown native kinds is not an authorization for known exclusions.
	if spec, known := knownNativeToolSpecIn(item, source, catalog); known && !relayToolAllowed(spec, source["tool_choice"]) {
		return "native_tool_not_allowed"
	}
	if !nativeChoiceAllows(item, source["tool_choice"]) {
		return "native_tool_not_allowed"
	}
	return ""
}

// A complete explicit function/custom choice is authoritative even if its
// schema is not available here. Unsupported choice kinds remain opaque: do not
// infer that a hosted/MCP or future tool kind is equivalent to a function.
func nativeChoiceAllows(item map[string]any, raw any) bool {
	choice := objectValue(raw)
	if choice == nil {
		return true
	}
	key, named := nativeToolKey(item)
	if !named {
		return true
	} // The stream retains unnamed added items until done.
	kind := "function"
	if stringValue(item["type"]) == "custom_tool_call" {
		kind = "custom"
	}
	match := func(selected map[string]any) bool {
		selectedKey, selectedNamed := nativeToolKey(selected)
		return selectedNamed && stringValue(selected["type"]) == kind && selectedKey == key
	}
	switch stringValue(choice["type"]) {
	case "function", "custom":
		return match(choice)
	case "allowed_tools":
		if stringValue(choice["mode"]) == "none" {
			return false
		}
		allowed, ok := choice["tools"].([]any)
		if !ok {
			return true
		}
		unknown := false
		for _, value := range allowed {
			selected := objectValue(value)
			switch stringValue(selected["type"]) {
			case "function", "custom":
				if match(selected) {
					return true
				}
			default:
				unknown = true
			}
		}
		return unknown
	default:
		return true
	}
}

func nativeToolKey(item map[string]any) (string, bool) {
	name, ok := item["name"].(string)
	if !ok || name == "" {
		return "", false
	}
	namespace, _ := item["namespace"].(string)
	if namespace != "" {
		return namespace + "." + name, true
	}
	return name, true
}

// Positive provenance/schema observation requires the exact declared tuple,
// not a unique-looking suffix, model name, user agent or a cached guess.
func knownNativeToolSpec(item, source map[string]any) (toolSpec, bool) {
	return knownNativeToolSpecIn(item, source, nil)
}

func knownNativeToolSpecIn(item, source map[string]any, catalog declaredCatalog) (toolSpec, bool) {
	if !isStreamToolItem(item) || isTransportCall(item) {
		return toolSpec{}, false
	}
	key, named := nativeToolKey(item)
	if !named {
		return toolSpec{}, false
	}
	// The cheap early returns above stay ahead of the catalogue, so a stream
	// without native tool calls never builds it.
	var declared map[string]toolSpec
	if catalog != nil {
		declared = catalog()
	} else {
		declared = allDeclaredClientToolSpecs(source)
	}
	spec, found := declared[key]
	if !found || spec.Name != item["name"] || spec.Namespace != stringValue(item["namespace"]) {
		return toolSpec{}, false
	}
	want := "function"
	if stringValue(item["type"]) == "custom_tool_call" {
		want = "custom"
	}
	return spec, spec.Type == want
}

// Call only for a complete native item, never for argument deltas. This is the
// existing bounded, default-off observer and cannot reject a native payload.
func observeNativeToolSchema(item, source map[string]any) {
	if isOpaqueToolHistory(item) {
		return
	}
	spec, known := knownNativeToolSpec(item, source)
	if !known || spec.Type != "function" {
		return
	}
	if arguments := parseArguments(item["arguments"]); arguments != nil {
		observeSelectedToolSchema(source, spec, arguments)
	}
}

func validateNativeToolDelta(kind string, payload, item, source map[string]any) error {
	if reason := nativeToolDeltaPolicyReason(kind, payload, item, source); reason != "" {
		return toolReject(reason)
	}
	return nil
}

// nativeToolDeltaPolicyReason is the tool_policy_checks finding for one native
// argument event, if any.
func nativeToolDeltaPolicyReason(kind string, payload, item, source map[string]any) string {
	return nativeToolDeltaPolicyReasonIn(kind, payload, item, source, nil)
}

func nativeToolDeltaPolicyReasonIn(kind string, payload, item, source map[string]any, catalog declaredCatalog) string {
	if !isStreamToolItem(item) || isTransportCall(item) {
		return ""
	}
	if reason := nativeToolPolicyReasonIn(item, source, catalog); reason != "" {
		return reason
	}
	if strings.HasPrefix(kind, "response.function_call_arguments.") && stringValue(item["type"]) != "function_call" {
		return "native_event_type_mismatch"
	}
	if strings.HasPrefix(kind, "response.custom_tool_call_input.") && stringValue(item["type"]) != "custom_tool_call" {
		return "native_event_type_mismatch"
	}
	if id, known := item["id"].(string); known && id != "" {
		if fragmentID, present := payload["item_id"].(string); present && fragmentID != "" && fragmentID != id {
			return "native_delta_identity_mismatch"
		}
	}
	return ""
}
