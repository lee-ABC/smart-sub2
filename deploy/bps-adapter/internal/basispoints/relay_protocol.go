package basispoints

import (
	"regexp"
	"strings"
)

const (
	relayProtocolLegacy       = "legacy"
	relayProtocolReferences   = "references"
	relayProtocolSelectionKey = "__cpa_selected_relay_protocol"
	relayHistoryInspectItems  = 4096
	relayHistoryInspectBytes  = 4 << 20
)

// A JSON request cannot construct this private Go type. The service freezes
// the selection during request preparation and attaches it only to its local
// response-decoder source map, never to the outbound body or client history.
type relayProtocolSelection string

func withRelayProtocolSelection(source map[string]any, selected string) map[string]any {
	if selected != relayProtocolLegacy && selected != relayProtocolReferences {
		configured := selected
		if strings.TrimSpace(configured) == "" {
			// Nothing was selected during preparation: use the request's own
			// frozen switch (default legacy), never an implicit auto.
			configured = relayOptionsFrom(source).Protocol
		}
		selected = selectRelayProtocol(source, configured, conversationScope(source))
	}
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	result[relayProtocolSelectionKey] = relayProtocolSelection(selected)
	return result
}

func relayProtocolForDecode(source map[string]any) string {
	if selected, ok := source[relayProtocolSelectionKey].(relayProtocolSelection); ok {
		if selected == relayProtocolLegacy || selected == relayProtocolReferences {
			return string(selected)
		}
	}
	// No frozen selection: follow the frozen switch (default legacy = h22).
	return selectRelayProtocol(source, relayOptionsFrom(source).Protocol, conversationScope(source))
}

// Auto changes NEW relay conversations only. Explicit/cached legacy calls win
// over references evidence. Cold/unknown tool history and item references keep
// legacy generation; unknown native/opaque items are never converted merely to
// make this decision. References history can continue alongside opaque reasoning
// when every visible tool call has known provenance. An explicit operator mode
// overrides generation policy, not the byte-for-byte replay of cached/native calls.
func selectRelayProtocol(source map[string]any, configured, scope string) string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case relayProtocolLegacy:
		return relayProtocolLegacy
	case relayProtocolReferences:
		return relayProtocolReferences
	case relayProtocolAuto:
	default:
		// Empty means "the default", and the default is legacy (h22).
		return relayProtocolLegacy
	}
	// Lite/native path remains unchanged; so does a request whose relay tools
	// are all hidden by relay_hide_encrypted_tools (its prompt is the Lite one).
	if specs, _ := fullRelayCatalogue(source); len(specs) == 0 {
		return relayProtocolLegacy
	}
	if stringValue(source["previous_response_id"]) != "" {
		return relayProtocolLegacy
	}
	items, _ := source["input"].([]any)
	if len(items) > relayHistoryInspectItems {
		return relayProtocolLegacy
	}
	legacy, references, unknown, opaque := false, false, false, false
	// The history this request will send: under history_conflict: replay
	// (h22) any call_id with a cached transport call replays it; under reject
	// only a call matching its delivered client call does.
	replay := relayOptionsFrom(source).HistoryConflict != historyConflictReject
	known := make(map[string]string)
	bytesLeft := relayHistoryInspectBytes
	observe := func(item map[string]any) string {
		if raw, ok := item["arguments"].(string); ok {
			if len(raw) > bytesLeft {
				unknown = true
				return ""
			}
			bytesLeft -= len(raw)
		}
		mode := historicalRelayProtocol(item)
		switch mode {
		case relayProtocolLegacy:
			legacy = true
		case relayProtocolReferences:
			references = true
		default:
			unknown = true
		}
		return mode
	}
	for _, raw := range items {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		switch stringValue(item["type"]) {
		case "function_call", "custom_tool_call":
			callID := stringValue(item["call_id"])
			var native map[string]any
			if isTransportCall(item) {
				native = item
			} else if replay {
				native = rememberedNativeCall(scope, callID)
			} else {
				cached, state := cachedCallForHistory(scope, item)
				if state == "hit" && cached.Origin == "relay" {
					native = cached.Native
				}
			}
			if native == nil {
				unknown = true
				continue
			}
			known[callID] = observe(native)
		case "item_reference":
			unknown = true
		case "tool_search_call":
			unknown = true
		case "reasoning", "compaction", "compaction_trigger":
			opaque = true
		}
		if _, present := item["encrypted_content"]; present {
			opaque = true
		}
	}
	for _, raw := range items {
		item := objectValue(raw)
		if item == nil {
			continue
		}
		kind := stringValue(item["type"])
		if kind != "function_call_output" && kind != "custom_tool_call_output" {
			continue
		}
		callID := stringValue(item["call_id"])
		if known[callID] != "" {
			continue
		}
		if replay {
			if native := rememberedNativeCall(scope, callID); native != nil {
				observe(native)
			} else {
				unknown = true
			}
		} else if cached, verified := cachedCallForOutput(scope, callID); verified && cached.Origin == "relay" {
			observe(cached.Native)
		} else {
			unknown = true
		}
	}
	if legacy || unknown {
		return relayProtocolLegacy
	}
	if references {
		return relayProtocolReferences
	}
	if opaque {
		return relayProtocolLegacy
	}
	return relayProtocolReferences
}

func historicalRelayProtocol(native map[string]any) string {
	if !isTransportCall(native) {
		return ""
	}
	args := parseArguments(native["arguments"])
	if args == nil {
		return ""
	}
	// A payload that also looks like a legacy wrapper is ambiguous evidence
	// about an old conversation. Preserve legacy generation without changing
	// the actual stored call. New response routing never uses this heuristic.
	if inner, reason := legacyTransportEnvelopeWithReason(native); reason == "" && completeLegacyEnvelope(inner) {
		return relayProtocolLegacy
	}
	refs, ok := args["references"].([]any)
	if !ok || len(refs) != 1 {
		return ""
	}
	name, ok := refs[0].(string)
	if !ok || name == "" || strings.TrimSpace(name) != name || isTransportName(name) {
		return ""
	}
	if _, ok := args["code"].(string); !ok {
		return ""
	}
	return relayProtocolReferences
}

func completeLegacyEnvelope(inner map[string]any) bool {
	name, ok := inner["tool"].(string)
	if !ok || name == "" {
		name, ok = inner["name"].(string)
	}
	if !ok || name == "" || isTransportName(name) {
		return false
	}
	for _, key := range []string{"args", "arguments", "input"} {
		if _, exists := inner[key]; exists {
			return true
		}
	}
	return false
}

func relayNameDeclaredIgnoringChoice(source map[string]any, name string) bool {
	if _, present := source["tool_choice"]; !present {
		_, ok := relayOutputToolSpecs(source)[name]
		return ok
	}
	unrestricted := make(map[string]any, len(source))
	for key, value := range source {
		if key != "tool_choice" {
			unrestricted[key] = value
		}
	}
	_, ok := relayOutputToolSpecs(unrestricted)[name]
	return ok
}

// relayEnvelopeWithReason turns one run_officejs call into the inner routing
// object {"tool": key, "args"/"input": payload} that decodeTransportItem
// resolves against the catalogue.
//
// Frozen legacy (the default) is h22's transportEnvelope and nothing else:
// references is never read, whatever it holds -- absent, null, a string, file
// paths, tool names. Models fill it with paths under h22's own prompt
// (references=[]), so reading it can only turn h22 successes into failures.
//
// Frozen references reads code FIRST:
//  1. code is a complete old envelope whose inner tool is declared: decode it
//     as one, whatever references says (h22 would have delivered it).
//  2. otherwise references must name exactly one declared catalogue key
//     (duplicates and undeclared entries such as file paths are ignored);
//     code is then that tool's payload.
//  3. otherwise the call fails, and the reason describes code before
//     references: a broken old envelope is code_json, not
//     tool_not_in_catalog or ambiguous_references.
//
// In no mode is a routing envelope -- {"tool":...,"args":...}, a nested
// run_officejs wrapper, or text that starts like one but does not parse --
// delivered as a custom input or as function arguments. Such a call decodes
// as the envelope it is (and fails exactly where h22 fails) or not at all.
func relayEnvelopeWithReason(native map[string]any, source map[string]any) (map[string]any, string) {
	if stringValue(native["type"]) != "function_call" || !isTransportName(stringValue(native["name"])) {
		return nil, "invalid_envelope"
	}
	if relayProtocolForDecode(source) != relayProtocolReferences {
		return legacyTransportEnvelopeWithReason(native)
	}
	args := parseArguments(native["arguments"])
	if args == nil {
		return nil, "outer_json"
	}
	inner, legacyReason := legacyTransportEnvelopeWithReason(native)
	if legacyReason == "" && completeLegacyEnvelope(inner) && relayNameDeclaredIgnoringChoice(source, legacyEnvelopeToolName(inner)) {
		return inner, ""
	}
	codeObject := decodeTransportCode(args["code"])
	if routingEnvelopeObject(codeObject, func(name string) bool { return relayNameDeclaredIgnoringChoice(source, name) }) {
		// An envelope naming an undeclared tool fails as it does in h22
		// (tool_not_in_catalog); it is never re-read as a payload.
		if legacyReason != "" {
			return nil, legacyReason
		}
		return inner, ""
	}
	code, isText := args["code"].(string)
	if isText && codeObject == nil && routingEnvelopeText(code) {
		return nil, "code_json"
	}
	name, refsReason := selectDeclaredReference(args, source)
	if refsReason == "" {
		if !isText {
			return nil, "code_json"
		}
		return map[string]any{"tool": name, "args": code}, ""
	}
	if codeObject == nil && (!isText || looksLikeJSONObjectText(code)) {
		return nil, "code_json"
	}
	return nil, refsReason
}

// legacyEnvelopeToolName is the name decodeTransportItem routes an envelope
// by: "tool", else "name", both trimmed (stringValue), exactly as in h22.
func legacyEnvelopeToolName(inner map[string]any) string {
	if name := stringValue(inner["tool"]); name != "" {
		return name
	}
	return stringValue(inner["name"])
}

// routingEnvelopeObject recognises an old envelope: a nested run_officejs
// wrapper, or a "tool" key that either comes with a payload key or names a
// declared tool. A function whose own schema has such fields cannot be told
// apart from it, so it is treated as an envelope -- which is what h22 did
// with it too.
func routingEnvelopeObject(object map[string]any, declared func(string) bool) bool {
	if object == nil {
		return false
	}
	if isTransportName(stringValue(object["name"])) {
		return true
	}
	name, ok := object["tool"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return false
	}
	for _, key := range []string{"args", "arguments", "input"} {
		if _, present := object[key]; present {
			return true
		}
	}
	return isTransportName(strings.TrimSpace(name)) || declared(strings.TrimSpace(name))
}

// The main failure in production is an old envelope whose inner JSON escaping
// is broken. It no longer parses, but it still starts like an envelope, and as
// a custom tool's raw input it would be executed as garbage.
var routingEnvelopePrefix = regexp.MustCompile(`^\{\s*"(?:tool|name)"\s*:`)

func routingEnvelopeText(code string) bool {
	return routingEnvelopePrefix.MatchString(stripCodeFence(code))
}

func looksLikeJSONObjectText(code string) bool {
	return strings.HasPrefix(stripCodeFence(code), "{")
}

// stripCodeFence mirrors decodeTransportCode's tolerance for a fenced block.
func stripCodeFence(code string) string {
	text := strings.TrimSpace(code)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = text[newline+1:]
		}
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	return text
}

// selectDeclaredReference returns the single declared catalogue key named in
// references, or the reason there is none. Declared ignores tool_choice, so a
// declared-but-excluded tool is still selected and then rejected downstream as
// tool_not_allowed rather than as an unknown name.
func selectDeclaredReference(args, source map[string]any) (string, string) {
	raw, present := args["references"]
	if !present {
		return "", "missing_tool_name"
	}
	refs, ok := raw.([]any)
	if !ok {
		return "", "invalid_references"
	}
	if len(refs) == 0 {
		return "", "missing_tool_name"
	}
	declared := map[string]bool{}
	plausible := 0
	selected := ""
	for _, value := range refs {
		name, ok := value.(string)
		if !ok || name == "" || name != strings.TrimSpace(name) || isTransportName(name) {
			continue
		}
		plausible++
		if !declared[name] && relayNameDeclaredIgnoringChoice(source, name) {
			declared[name] = true
			selected = name
		}
	}
	switch {
	case len(declared) == 1:
		return selected, ""
	case len(declared) > 1:
		return "", "ambiguous_references"
	case plausible == 1 && len(refs) == 1:
		return "", "tool_not_in_catalog"
	default:
		return "", "invalid_references"
	}
}

// The references prompt follows relay_tool_contracts like the legacy one does:
// summary uses h22's directory and h22's catalogue (every top-level tool,
// none when tool_choice is "none") and carries none of rc2's added hints;
// full is rc2's text with complete definitions, native-tool and tool_choice
// hints. rc2's promise that a function's own fields named tool/args are plain
// business arguments is gone: such a payload cannot be told apart from an old
// envelope, and relayEnvelopeWithReason now decodes it as one. E1 hides tools
// from this catalogue exactly as from the legacy one (encrypted_tools.go).
func referencesToolProtocolInstructions(source map[string]any, plain bool) string {
	full := relayToolContractsFor(source) == relayToolContractsFull
	specs, hidden := summaryRelayCatalogue(source)
	if full {
		specs, hidden = fullRelayCatalogue(source)
	}
	if len(specs) == 0 {
		return clientToolProtocolInstructionsFor(source, plain)
	}
	prefix, handoff := toolCatalogPrefix, relayHandoff
	if plain {
		prefix, handoff = plainCatalogPrefix, plainHandoff
	}
	lead, catalog := "", h22RelayCatalogText(source, hidden)
	if len(hidden) > 0 {
		catalog += "\n" + encryptedToolsUnavailable(hidden, plain)
	}
	if full {
		lead = strings.TrimSpace(nativeClientToolHint(source)+" "+toolChoiceInstruction(source)) + " "
		catalog = clientToolCatalogText(source)
		if len(hidden) > 0 {
			catalog += encryptedToolsUnavailable(hidden, plain) + "\n"
		}
	}
	return lead + prefix + " Other server-injected Excel, Office, connector and workbook tools are unavailable. Use one outer run_officejs transport call per client tool. Set references to an array containing exactly one complete catalog tool key, including its namespace. Put only that selected tool's payload in code: for a function tool, code is the exact JSON text of its arguments object; for a custom tool, code is the original raw input string, preserving every leading/trailing space, quote, backslash and newline. An empty custom input stays empty. Do not add a tool/args envelope or another run_officejs wrapper around the payload. Include summary, extended_summary and destructive=false as usual. Do not guess or shorten tool names. " + handoff + " Interpret each returned result as the named client tool output and never repeat a call whose output is already present. Available client tools:\n" + catalog + "\n" + referencesToolProtocolReminder(source)
}

func referencesToolProtocolReminder(source map[string]any) string {
	const reminder = "Reminder: references selects exactly one complete catalog key. code contains only that function's JSON arguments object or that custom tool's unchanged raw input; no routing wrapper inside code. Use the declared tool when action is needed, and never invent a name."
	if relayToolContractsFor(source) != relayToolContractsFull {
		if specs, _ := summaryRelayCatalogue(source); len(specs) == 0 {
			return ""
		}
		return reminder
	}
	if specs, _ := fullRelayCatalogue(source); len(specs) == 0 {
		return ""
	}
	return reminder + " " + toolChoiceInstruction(source)
}
