package basispoints

import "strings"

// History translation (H25-SPEC S3). translateInputItemsFor picks one of the
// two by history_conflict:
//
//   - replay (default): h22's translateInputItemsFor, byte for byte in what it
//     sends upstream. The only additions are notes (log lines and counters)
//     when the history differs from what was delivered; they never change
//     the body.
//   - reject: rc2's provenance-checked translation, corrected so that a call
//     the cache cannot vouch for (opaque, unverifiable, contradicted, unknown)
//     still takes h22's cold path -- wrapping into run_officejs and namespace
//     restoration -- instead of going upstream under its bare client name,
//     and so that an opaque result keeps its content while its type and id
//     follow its call.

// h22ToolNamespaces is h22's toolNamespaces: additional_tools declarations
// win over tool_search_output ones, and a name that one request's search
// results place in two namespaces is left alone.
func h22ToolNamespaces(items []any) map[string]string {
	catalogue := map[string]string{}
	searched := map[string]string{}
	conflicting := map[string]bool{}
	for _, value := range items {
		item := objectValue(value)
		if item == nil {
			continue
		}
		switch stringValue(item["type"]) {
		case "additional_tools":
			collectNamespaces(item["tools"], func(tool, namespace string) {
				catalogue[tool] = namespace
			})
		case "tool_search_output":
			collectNamespaces(item["tools"], func(tool, namespace string) {
				if known, seen := searched[tool]; seen && known != namespace {
					conflicting[tool] = true
				}
				searched[tool] = namespace
			})
		}
	}
	for tool, namespace := range searched {
		if conflicting[tool] {
			continue
		}
		if _, declared := catalogue[tool]; !declared {
			catalogue[tool] = namespace
		}
	}
	return catalogue
}

// noteHistory records a history note for this request.
func (o wireOptions) noteHistory(reason string) {
	if o.historyNote != nil {
		o.historyNote(reason)
		return
	}
	if relayHistoryReasons[reason] {
		countRelayEvent("history_note:" + reason)
	}
}

// translateInputItemsReplay is h22's history translation.
//
// A natively promoted request (opts.a1History, a1_promotion.go) takes it with
// two differences, and only those:
//   - h1: a cold historical call that this conversation already had answered
//     natively (the native pool says so) is sent natively, not wrapped as
//     run_officejs; every other call is translated as h22 would (relay calls
//     replay their run_officejs, cold ones are wrapped against the relay
//     catalogue the request came with). The history is mixed.
//   - h2: no call is shown as run_officejs except one the client itself
//     replays under that name. A relay call -- cached, marked or cold -- goes
//     up as the client's own call with its namespace restored, and its
//     output keeps the client's own type.
//
// The promotion's own additional_tools item (opts.a1Synthetic) is not
// remembered as a client declaration: it is rebuilt on every promoted turn,
// and an unpromoted turn of the same conversation must not inherit it.
func translateInputItemsReplay(items []any, allowed map[string]toolSpec, scope string, opts wireOptions) []any {
	result := make([]any, 0, len(items))
	origins := map[string]string{}
	declared := h22ToolNamespaces(items)
	if opts.a1Synthetic > 0 && opts.a1Synthetic <= len(items) {
		rememberNamespaces(scope, h22ToolNamespaces(items[opts.a1Synthetic:]))
	} else {
		rememberNamespaces(scope, declared)
	}
	namespaces := rememberedNamespaces(scope, declared)
	replayed, wrapped := false, false
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		// Every item that reaches result has to be ours alone; see h22.
		item = cloneObject(stripClientMetadata(item))
		itemType := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
		if itemType == "function_call" || itemType == "custom_tool_call" {
			callID := stringValue(item["call_id"])
			if native := rememberedNativeCall(scope, callID); native != nil {
				if opts.a1History == a1HistoryH2 && !isTransportName(stringValue(item["name"])) {
					countRelayEvent("a1_history:h2_rewritten")
					result = append(result, withToolNamespace(item, namespaces))
					continue
				}
				if callID != "" {
					origins[callID] = stringValue(native["name"])
				}
				// h22 replays the model's own transport call even when the
				// client has rewritten its copy (e.g. added default argument
				// keys); only the observation is new.
				if _, state := cachedCallForHistory(scope, item); state == "conflict" {
					opts.noteHistory("relay_history_mismatch")
				}
				replayed = true
				result = append(result, native)
				continue
			}
			name := stringValue(item["name"])
			if name == transportName || name == transportAlias {
				rememberNativeCall(scope, item)
				if callID != "" {
					origins[callID] = transportName
				}
				result = append(result, item)
				continue
			}
			if _, exists := allowed[name]; exists {
				if opts.a1History == a1HistoryH1 && a1NativeOriginKnown(scope, callID) {
					// Answered natively on an earlier promoted turn.
					countRelayEvent("a1_history:h1_native_kept")
					result = append(result, withToolNamespace(item, namespaces))
					continue
				}
				if callID != "" {
					origins[callID] = transportName
				}
				wrapped = true
				result = append(result, fallbackTransportCallFor(item, opts.plainPrologue))
				continue
			}
			if relayCallMarked(scope, callID) && opts.a1History != a1HistoryH2 {
				// A relay call whose record was too large to keep (h22 kept
				// and replayed it): wrapped like a declared one, whatever this
				// turn declares, never sent upstream under its bare client
				// name (h25 review, cache boundary).
				origins[callID] = transportName
				countRelayEvent("history_marker_wrapped")
				result = append(result, fallbackTransportCallFor(item, opts.plainPrologue))
				continue
			}
			// An upstream-native tool, replayed as the model emitted it -- give
			// it back the namespace the request declares it in.
			if _, state := cachedCallForHistory(scope, item); state == "conflict" {
				opts.noteHistory("native_history_mismatch")
			}
			result = append(result, withToolNamespace(item, namespaces))
			continue
		}
		if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			callID := stringValue(item["call_id"])
			relayOutput := origins[callID] == transportName || rememberedNativeCall(scope, callID) != nil || relayCallMarked(scope, callID)
			if opts.a1History == a1HistoryH2 {
				// h2 shows a call as run_officejs only when the client itself
				// replayed it under that name; every other output answers the
				// client's own call and keeps its type.
				relayOutput = origins[callID] == transportName
			}
			if relayOutput {
				copy := cloneObject(item)
				copy["type"] = "function_call_output"
				copy["id"] = functionItemID(callID)
				if strings.TrimSpace(itemText(copy["output"])) == "" {
					copy["output"] = "(tool call succeeded with no output)"
				}
				result = append(result, copy)
			} else {
				result = append(result, item)
			}
			continue
		}
		if itemType == "reasoning" {
			if rebuilt := translatedReasoningItem(item, opts); rebuilt != nil {
				result = append(result, rebuilt)
			}
			continue
		}
		if itemType == "item_reference" {
			continue
		}
		if opts.clientShapes {
			item = clientMessageShape(item, itemType)
		}
		result = append(result, item)
	}
	if replayed {
		countRelayEvent("history_relay_replayed")
	}
	if wrapped {
		countRelayEvent("history_cold_wrapped")
	}
	return result
}

// a1NativeOriginKnown: the native pool holds a delivered native call under
// this call_id (cache_provenance.go), i.e. the upstream made it natively.
func a1NativeOriginKnown(scope, callID string) bool {
	entry, found := nativeOriginCache.lookup(scope, callID)
	return found && !entry.conflict && entry.origin == "native"
}

// translateInputItemsStrict is rc2's translation with the H25-SPEC S3.4
// corrections; history_conflict: reject only.
func translateInputItemsStrict(items []any, allowed map[string]toolSpec, scope string, opts wireOptions) []any {
	result := make([]any, 0, len(items))
	origins := map[string]string{}
	// Presence is request-wide, not order-dependent. A later opaque/current
	// call must prevent an earlier output from borrowing an old relay record.
	currentCalls := map[string]int{}
	for _, raw := range items {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
		if kind == "function_call" || kind == "custom_tool_call" {
			if callID := stringValue(item["call_id"]); callID != "" {
				currentCalls[callID]++
			}
		}
	}
	declared := toolNamespaces(items)
	rememberNamespaces(scope, declared)
	namespaces := rememberedNamespaces(scope, declared)
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		item = cloneObject(stripClientMetadata(item))
		itemType := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
		if itemType == "function_call" || itemType == "custom_tool_call" {
			callID := stringValue(item["call_id"])
			if !isTransportCall(item) && !isOpaqueToolHistory(item) {
				cached, state := cachedCallForHistory(scope, item)
				if state == "hit" {
					if cached.Origin == "relay" {
						origins[callID] = transportName
						result = append(result, cached.Native)
					} else {
						result = append(result, cached.Client)
					}
					continue
				}
				// A contradiction that was not refused (outside the strict
				// scope), an unverifiable call (e.g. arguments that are not
				// JSON) or a miss: no provenance either way, so the call takes
				// h22's cold path below rather than going upstream raw.
			}
			name := stringValue(item["name"])
			if name == transportName || name == transportAlias {
				if callID != "" {
					origins[callID] = transportName
				}
				result = append(result, item)
				continue
			}
			if _, exists := allowed[name]; (exists && stringValue(item["namespace"]) == "") || relayCallMarked(scope, callID) {
				// A configuration choice describes future model output, not the
				// unknown origin of a cold historical call. Keep the h22 legacy
				// fallback even when new calls are requested through references.
				if callID != "" {
					origins[callID] = transportName
				}
				result = append(result, fallbackTransportCallFor(item, opts.plainPrologue))
				continue
			}
			result = append(result, withToolNamespace(item, namespaces))
			continue
		}
		if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			// Output normalization is deferred until every current call has
			// been classified. Retain this exact position in the history.
			result = append(result, item)
			continue
		}
		if itemType == "reasoning" {
			if rebuilt := translatedReasoningItem(item, opts); rebuilt != nil {
				result = append(result, rebuilt)
			}
			continue
		}
		if itemType == "item_reference" {
			continue
		}
		if opts.clientShapes {
			item = clientMessageShape(item, itemType)
		}
		result = append(result, item)
	}
	for index, raw := range result {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
		if kind != "function_call_output" && kind != "custom_tool_call_output" {
			continue
		}
		callID := stringValue(item["call_id"])
		normalize := currentCalls[callID] == 1 && origins[callID] == transportName
		if currentCalls[callID] == 0 {
			cached, verified := cachedCallForOutput(scope, callID)
			normalize = verified && cached.Origin == "relay"
		}
		// Duplicate current identities are ambiguous, even if one occurrence
		// is a valid relay call. Never bind their output to an older record.
		if !normalize {
			continue
		}
		copy := cloneObject(item)
		copy["type"] = "function_call_output"
		copy["id"] = functionItemID(callID)
		// An opaque result keeps its content as it is; only its type and id
		// follow the run_officejs call it answers.
		if !isOpaqueToolHistory(item) {
			output, present := copy["output"]
			text, isString := output.(string)
			if !present || output == nil || (isString && len(text) == 0) {
				copy["output"] = "(empty tool output)"
			}
		}
		// Structured, multimodal, scalar and whitespace-only values carry
		// information. Absence of textual content is not proof of emptiness
		// and certainly not proof that a tool succeeded.
		result[index] = copy
	}
	return result
}

// translatedReasoningItem rebuilds a reasoning item as h22 did; nil drops it.
func translatedReasoningItem(item map[string]any, opts wireOptions) map[string]any {
	encrypted := stringValue(item["encrypted_content"])
	if encrypted == "" {
		return nil
	}
	rebuilt := map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted}
	if opts.clientShapes {
		// A real reasoning item carries five fields. Rebuilding it with three
		// drops the id the upstream issued and the summary content it
		// returned, which no client would do to an item it is replaying back.
		if id := stringValue(item["id"]); id != "" {
			rebuilt["id"] = id
		}
		if content, exists := item["content"]; exists {
			rebuilt["content"] = content
		}
		if summary, exists := item["summary"]; exists {
			rebuilt["summary"] = summary
		}
	}
	return rebuilt
}
