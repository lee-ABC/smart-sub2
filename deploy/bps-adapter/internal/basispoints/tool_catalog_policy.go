package basispoints

import (
	"sort"
	"strings"
)

// The native declarations remain native. This catalogue only establishes one
// current name/contract/choice view for hints and output checks; it is never
// used to rewrap historical calls or remove additional_tools/search output.
func declaredToolCatalogs(source map[string]any) (map[string]toolSpec, map[string]toolSpec) {
	all, native := map[string]toolSpec{}, map[string]toolSpec{}
	iterToolValues(source["tools"], "", func(spec toolSpec) { all[spec.Key] = spec })
	searched, conflicts := map[string]toolSpec{}, map[string]bool{}
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		switch stringValue(item["type"]) {
		case "additional_tools":
			iterToolValues(item["tools"], "", func(spec toolSpec) { all[spec.Key], native[spec.Key] = spec, spec })
		case "tool_search_output":
			iterToolValues(item["tools"], "", func(spec toolSpec) {
				if previous, ok := searched[spec.Key]; ok && !sameToolCallContract(previous, spec) {
					conflicts[spec.Key] = true
				}
				searched[spec.Key] = spec
			})
		}
	}
	for key, spec := range searched {
		if _, explicit := all[key]; !explicit && !conflicts[key] {
			all[key], native[key] = spec, spec
		}
	}
	return all, native
}

// Discovery may update display wording/deferred-load state without changing a
// callable contract. Keep every other field, including custom grammar and all
// schema contents, so genuinely different declarations remain a conflict.
func sameToolCallContract(a, b toolSpec) bool {
	contract := func(spec toolSpec) map[string]any {
		out := make(map[string]any, len(spec.Spec))
		for key, value := range spec.Spec {
			if key != "description" && key != "defer_loading" {
				out[key] = value
			}
		}
		return out
	}
	return a.Key == b.Key && a.Type == b.Type && equivalentToolArguments(string(jsonBytes(contract(a))), string(jsonBytes(contract(b))))
}

// searchDuplicateMerged reports whether h22 would have left key out of the
// relay output catalogue -- tool_search_output declared it more than once and
// two consecutive declarations differ byte for byte (h22's relayOutputToolSpecs
// compared jsonBytes) -- while h25 takes them as one tool, because they differ
// only in description or defer_loading (sameToolCallContract, as in
// declaredToolCatalogs). A run_officejs call to such a tool was
// tool_not_in_catalog in h22 and is delivered now: an E3 repair, counted as
// search_duplicate_merged (review EH-4). Asked only for a relay call that has
// decoded, so it costs one pass over the input per delivered call.
func searchDuplicateMerged(source map[string]any, key string) bool {
	explicit := false
	mark := func(spec toolSpec) {
		if spec.Key == key {
			explicit = true
		}
	}
	iterToolValues(source["tools"], "", mark)
	var previous toolSpec
	var previousBytes string
	seen, differs, conflict := false, false, false
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		switch stringValue(item["type"]) {
		case "additional_tools":
			iterToolValues(item["tools"], "", mark)
		case "tool_search_output":
			iterToolValues(item["tools"], "", func(spec toolSpec) {
				if spec.Key != key {
					return
				}
				encoded := string(jsonBytes(spec.Spec))
				if seen && encoded != previousBytes {
					differs = true
					conflict = conflict || !sameToolCallContract(previous, spec)
				}
				previous, previousBytes, seen = spec, encoded, true
			})
		}
	}
	return differs && !conflict && !explicit
}

func allDeclaredClientToolSpecs(source map[string]any) map[string]toolSpec {
	all, _ := declaredToolCatalogs(source)
	return all
}

func declaredNativeToolSpecs(source map[string]any) map[string]toolSpec {
	_, native := declaredToolCatalogs(source)
	return native
}

func advertisedNativeToolSpecs(source map[string]any) map[string]toolSpec {
	native := declaredNativeToolSpecs(source)
	for key, spec := range native {
		if !relayToolAllowed(spec, source["tool_choice"]) {
			delete(native, key)
		}
	}
	return native
}

func advertisedRelayToolSpecs(source map[string]any) map[string]toolSpec {
	all, native := declaredToolCatalogs(source)
	for key, spec := range all {
		if _, direct := native[key]; direct || !relayToolAllowed(spec, source["tool_choice"]) {
			delete(all, key)
		}
	}
	return all
}

func nativeClientToolHint(source map[string]any) string {
	if toolChoiceIsNone(source) {
		return ""
	}
	native := advertisedNativeToolSpecs(source)
	if len(native) != 0 {
		names := make([]string, 0, len(native))
		for key := range native {
			names = append(names, key)
		}
		sort.Strings(names)
		return "The client also declares native tools in additional_tools or tool_search_output. Use these directly with their declared type, full name, namespace, parameters or raw custom input; do not wrap them in run_officejs. Current native client tools: " + strings.Join(names, ", ") + ". Their original declarations remain in the input."
	}
	// A continuation with known native-looking history is not evidence that
	// its tools disappeared. This hint creates no definitions or names and
	// permits only tools actually available in the current conversation.
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		kind := stringValue(item["type"])
		if kind == "additional_tools" || kind == "tool_search_output" {
			if tools, ok := item["tools"].([]any); ok && len(tools) > 0 {
				return "Native client tool declarations remain in the additional_tools or tool_search_output input. Use only those currently available and permitted by this request's tool_choice, directly as declared; do not turn them into workbook tools or invent missing definitions."
			}
		}
		if (kind == "function_call" || kind == "custom_tool_call") && !isTransportCall(item) && stringValue(item["namespace"]) != "" {
			return "This conversation contains prior namespaced client tool activity. Continue using only client tools actually available in this conversation; omitted discovery declarations do not by themselves prove that tools are unavailable. Do not invent tools or replace them with workbook tools."
		}
	}
	return ""
}

func toolChoiceInstruction(source map[string]any) string {
	prefix := ""
	if parallel, ok := source["parallel_tool_calls"].(bool); ok && !parallel {
		prefix = "At most one client function/custom tool call is permitted in this response. "
	}
	choice := source["tool_choice"]
	if toolChoiceIsNone(source) {
		return "Tool choice for this request is none: do not emit client tool calls; answer as assistant text."
	}
	if stringValue(choice) == "required" {
		return prefix + "Tool choice for this request is required: call at least one available declared tool; a text-only answer does not satisfy this request."
	}
	if spec := objectValue(choice); spec != nil {
		switch stringValue(spec["type"]) {
		case "function", "custom":
			name := stringValue(spec["name"])
			if namespace := stringValue(spec["namespace"]); namespace != "" {
				name = namespace + "." + name
			}
			return prefix + "Tool choice requires exactly one call to the declared " + stringValue(spec["type"]) + " tool " + name + ". Use its existing native or relay route and no other client tool."
		case "allowed_tools":
			if stringValue(spec["mode"]) == "required" {
				return prefix + "Only the advertised allowed tools may be called, and at least one is required; a text-only answer is insufficient."
			}
			return prefix + "Only the advertised allowed tools may be called. Text-only responses remain allowed."
		}
	}
	return strings.TrimSpace(prefix)
}

// Zero means no local count restriction. The limit concerns executable client
// function/custom items, not hosted searches or approval activity.
func clientToolCallLimit(source map[string]any) int {
	choice := objectValue(source["tool_choice"])
	if kind := stringValue(choice["type"]); kind == "function" || kind == "custom" {
		return 1
	}
	if parallel, ok := source["parallel_tool_calls"].(bool); ok && !parallel {
		return 1
	}
	return 0
}

// Successful function/custom contracts are checked for the entire batch.
// Standard hosted call/approval events count only when the corresponding kind
// was explicitly declared, never merely because their type ends in _call.
//
// These are tool_policy_checks findings (relay_policy.go); callers on the
// response path use completedToolChoiceViolation and let the switch decide.
func validateCompletedToolChoice(response, source map[string]any) error {
	if reason := completedToolChoiceViolation(response, source); reason != "" {
		return toolReject(reason)
	}
	return nil
}

func completedToolChoiceViolation(response, source map[string]any) string {
	if status := stringValue(response["status"]); status != "" && status != "completed" {
		return ""
	}
	choice := objectValue(source["tool_choice"])
	forced := choice != nil && (stringValue(choice["type"]) == "function" || stringValue(choice["type"]) == "custom")
	required := stringValue(source["tool_choice"]) == "required" || forced || (choice != nil && stringValue(choice["type"]) == "allowed_tools" && stringValue(choice["mode"]) == "required")
	limit := clientToolCallLimit(source)
	if !required && limit == 0 {
		return ""
	}
	functionCalls, otherToolActivity, refusal := 0, false, false
	output, _ := response["output"].([]any)
	for _, raw := range output {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		switch stringValue(item["type"]) {
		case "function_call", "custom_tool_call":
			functionCalls++
		case "message":
			content, _ := item["content"].([]any)
			for _, rawPart := range content {
				if part := objectValue(rawPart); part != nil && stringValue(part["type"]) == "refusal" {
					refusal = true
				}
			}
		default:
			if explicitlyDeclaredHostedActivity(item, source) {
				otherToolActivity = true
			}
		}
	}
	if limit > 0 && functionCalls > limit {
		if forced {
			return "forced_tool_count"
		}
		return "parallel_tool_count"
	}
	if refusal && functionCalls == 0 {
		return ""
	}
	if forced {
		if functionCalls == 0 {
			return "required_tool_missing"
		}
		if functionCalls != 1 {
			return "forced_tool_count"
		}
	} else if required && functionCalls == 0 && !otherToolActivity {
		return "required_tool_missing"
	}
	return ""
}

func explicitlyDeclaredHostedActivity(item, source map[string]any) bool {
	wanted := map[string][]string{
		"tool_search_call": {"tool_search"}, "web_search_call": {"web_search", "web_search_preview", "web_search_preview_2025_03_11"},
		"file_search_call": {"file_search"}, "computer_call": {"computer", "computer_use", "computer_use_preview"},
		"code_interpreter_call": {"code_interpreter"}, "image_generation_call": {"image_generation"},
		"mcp_call": {"mcp"}, "mcp_approval_request": {"mcp"},
	}[stringValue(item["type"])]
	if len(wanted) == 0 {
		return false
	}
	check := func(raw any) bool {
		list, _ := raw.([]any)
		for _, value := range list {
			tool := objectValue(value)
			if tool == nil {
				continue
			}
			for _, kind := range wanted {
				if stringValue(tool["type"]) != kind {
					continue
				}
				if kind == "mcp" && stringValue(tool["server_label"]) != stringValue(item["server_label"]) {
					continue
				}
				if execution := stringValue(tool["execution"]); execution != "" && stringValue(item["execution"]) != "" && stringValue(item["execution"]) != execution {
					continue
				}
				if !hostedToolChoiceAllows(tool, item, source["tool_choice"]) {
					continue
				}
				return true
			}
		}
		return false
	}
	if check(source["tools"]) {
		return true
	}
	items, _ := source["input"].([]any)
	for _, raw := range items {
		decl := objectValue(raw)
		if decl == nil {
			continue
		}
		if kind := stringValue(decl["type"]); kind == "additional_tools" || kind == "tool_search_output" {
			if check(decl["tools"]) {
				return true
			}
		}
	}
	return false
}

func hostedToolChoiceAllows(definition, item map[string]any, rawChoice any) bool {
	choice := objectValue(rawChoice)
	if choice == nil {
		return stringValue(rawChoice) != "none"
	}
	if stringValue(choice["type"]) != "allowed_tools" {
		return false
	}
	if stringValue(choice["mode"]) == "none" {
		return false
	}
	tools, _ := choice["tools"].([]any)
	for _, raw := range tools {
		allowed := objectValue(raw)
		if allowed == nil || stringValue(allowed["type"]) != stringValue(definition["type"]) {
			continue
		}
		if label := stringValue(allowed["server_label"]); label != "" && label != stringValue(item["server_label"]) {
			continue
		}
		if name := stringValue(allowed["name"]); name != "" && name != stringValue(item["name"]) {
			continue
		}
		return true
	}
	return false
}
