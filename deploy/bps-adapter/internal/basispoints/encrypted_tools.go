package basispoints

import (
	"sort"
	"strings"
)

// relay_hide_encrypted_tools (H25-SPEC S4.1, exceptions E1 and E4).
//
// Some client tools take an argument the client only ever sends as
// ciphertext: Codex declares collaboration.spawn_agent / send_message /
// followup_task with "encrypted": true on their message parameter. Called
// natively (Lite), the upstream model produces that ciphertext itself. Written
// inside run_officejs, the model can only write plaintext. h21/h22 handed such
// a relay call back with its namespace, the client executed it, put the
// plaintext into the sub-agent's agent_message.encrypted_content, and the
// upstream answered that request with 400 invalid_encrypted_content. h17 handed
// it back without the namespace: the client answered "unsupported call" and
// nothing undecryptable ever reached the upstream.
//
// With the switch on (the default):
//   - E1: the relay catalogue (h22's: the top-level tools) does not list such
//     tools, and one sentence names them as unavailable. When every relay tool
//     is such a tool there is no catalogue left; the request then gets the
//     prologue h22 gives a request without relay tools, plus that sentence.
//   - E4: a run_officejs call to one of them -- whether the tool was declared
//     in the top-level tools, in additional_tools or in tool_search_output --
//     is handed to the client without its namespace and noted as
//     encrypted_tool_blocked. That is the h17 result: the client refuses it as
//     an unsupported call. If the bare name is also a tool the request
//     declares outside a namespace, it goes back as "<namespace>.<name>"
//     instead (encrypted_tool_blocked_collision; see blockedRelayCallName).
//
// Both look at every declaration under a key, not at the one a catalogue
// keeps (encryptedKeyClaims). A catalogue holds one tool per full key, and
// the last declaration wins, so a later declaration under the same key -- a
// flat tool literally named "agents.send", or the same namespace declared
// again without the mark -- would replace the encrypted tool and take it out
// of E1 and E4 depending on where it was written (h25 second review, low).
// So a key that any declaration claims for an encrypted namespaced tool is
// hidden by E1, and a relay call to a key that is claimed both ways is
// ambiguous and fails as invalid_tool_call, reason encrypted_tool_blocked,
// whatever the declaration order: no name is handed back that the client
// could run as the other tool. Real catalogues declare each namespaced tool
// once (or identically) and no tool name contains a dot, so neither case
// occurs there.
//
// A native (Lite) call never goes through the relay decoder and is untouched,
// and nothing about the request's history changes. Off, everything is h22's.
//
// The test is structural and never looks at names: the tool is declared inside
// a namespace, and some schema node of its parameters carries
// "encrypted": true (the boolean). A flat name such as collaboration__spawn_agent
// is not in a namespace and stays exactly as it was.

const (
	encryptedSchemaMaxDepth = 32
	encryptedSchemaMaxNodes = 4096
)

// encryptedNamespacedTool is the E1/E4 test for one declared tool.
func encryptedNamespacedTool(spec toolSpec) bool {
	if spec.Namespace == "" {
		return false
	}
	schema := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")
	if schema == nil {
		return false
	}
	budget := encryptedSchemaMaxNodes
	return schemaMarksEncrypted(schema, 0, &budget)
}

// Keywords whose value is one schema, keywords whose value is a list of
// schemas, and keywords whose value maps names to schemas.
var (
	encryptedSchemaSingle = []string{"items", "additionalProperties", "additionalItems", "unevaluatedItems",
		"unevaluatedProperties", "propertyNames", "contains", "not", "if", "then", "else"}
	encryptedSchemaLists = []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"}
	// dependencies is draft-7's: a name maps to a schema or to a list of
	// required names; a list is no schema and objectValue skips it.
	encryptedSchemaNamed = []string{"properties", "patternProperties", "dependentSchemas", "dependencies", "$defs", "definitions"}
)

// schemaMarksEncrypted walks schema nodes only, so a property that is merely
// NAMED "encrypted" is not a mark. Names are visited in sorted order: with the
// node budget, a walk in map order could give two answers for one schema.
func schemaMarksEncrypted(node any, depth int, budget *int) bool {
	schema := objectValue(node)
	if schema == nil || depth > encryptedSchemaMaxDepth || *budget <= 0 {
		return false
	}
	*budget--
	if marked, ok := schema["encrypted"].(bool); ok && marked {
		return true
	}
	for _, key := range encryptedSchemaSingle {
		if schemaMarksEncrypted(schema[key], depth+1, budget) {
			return true
		}
	}
	for _, key := range encryptedSchemaLists {
		list, _ := schema[key].([]any)
		for _, value := range list {
			if schemaMarksEncrypted(value, depth+1, budget) {
				return true
			}
		}
	}
	for _, key := range encryptedSchemaNamed {
		named := objectValue(schema[key])
		names := make([]string, 0, len(named))
		for name := range named {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if schemaMarksEncrypted(named[name], depth+1, budget) {
				return true
			}
		}
	}
	return false
}

// hiddenRelayToolKeys is the part of a relay catalogue that E1 keeps out of the
// prompt: nil with the switch off or when nothing qualifies. A key is hidden
// when its catalogue tool qualifies, or when another declaration under the
// same key does (encryptedKeyClaims): the catalogue kept whichever came last,
// and a relay call to such a key fails anyway (decodeRelayCall).
func hiddenRelayToolKeys(source map[string]any, catalogue map[string]toolSpec) map[string]bool {
	if !relayOptionsFrom(source).HideEncryptedTools {
		return nil
	}
	var hidden map[string]bool
	hide := func(key string) {
		if hidden == nil {
			hidden = map[string]bool{}
		}
		hidden[key] = true
	}
	var unmarked map[string]bool
	for key, spec := range catalogue {
		if encryptedNamespacedTool(spec) {
			hide(key)
		} else {
			if unmarked == nil {
				unmarked = map[string]bool{}
			}
			unmarked[key] = true
		}
	}
	if len(unmarked) != 0 {
		for key, claim := range encryptedKeyClaims(source, unmarked) {
			if claim.encrypted {
				hide(key)
			}
		}
	}
	return hidden
}

// encryptedKeyClaim is what the request's declarations under one full key
// are: some an encrypted namespaced tool (the E1 test), some not.
type encryptedKeyClaim struct{ encrypted, unmarked bool }

// ambiguous reports a key the request declares both ways.
func (c encryptedKeyClaim) ambiguous() bool { return c.encrypted && c.unmarked }

// encryptedKeyClaims looks at every declaration the request makes under the
// given keys, in every place it can make one (forEachDeclaredTool), so the
// answer does not depend on which declaration a catalogue kept. tool_choice
// is not consulted: it filters by full key and cannot tell the two apart.
// Only declarations under a wanted key have their schema walked.
func encryptedKeyClaims(source map[string]any, keys map[string]bool) map[string]encryptedKeyClaim {
	claims := map[string]encryptedKeyClaim{}
	forEachDeclaredTool(source, func(spec toolSpec) {
		if !keys[spec.Key] {
			return
		}
		claim := claims[spec.Key]
		if encryptedNamespacedTool(spec) {
			claim.encrypted = true
		} else {
			claim.unmarked = true
		}
		claims[spec.Key] = claim
	})
	return claims
}

// ambiguousEncryptedKey is the E4 order-independence check for a decoded
// relay call to key: with the switch on, some declaration under key is an
// encrypted namespaced tool and some other is not.
func ambiguousEncryptedKey(source map[string]any, key string) bool {
	if !relayOptionsFrom(source).HideEncryptedTools {
		return false
	}
	return encryptedKeyClaims(source, map[string]bool{key: true})[key].ambiguous()
}

// withoutHiddenTools returns catalogue minus hidden, as a new map.
func withoutHiddenTools(catalogue map[string]toolSpec, hidden map[string]bool) map[string]toolSpec {
	visible := make(map[string]toolSpec, len(catalogue))
	for key, spec := range catalogue {
		if !hidden[key] {
			visible[key] = spec
		}
	}
	return visible
}

// encryptedToolsUnavailable is the one E1 sentence. It names the hidden tools
// in sorted order, so it is the same on every turn of a conversation. The
// plain wording says nothing about a relay, as plain_prologue requires.
func encryptedToolsUnavailable(hidden map[string]bool, plain bool) string {
	if len(hidden) == 0 {
		return ""
	}
	names := make([]string, 0, len(hidden))
	for name := range hidden {
		names = append(names, name)
	}
	sort.Strings(names)
	if plain {
		return "These client tools cannot be dispatched through run_officejs, so do not call them: " + strings.Join(names, ", ") + "."
	}
	return "These client tools are declared by the client but are unavailable through this relay, so do not call them: " + strings.Join(names, ", ") + "."
}

// hiddenRelayCall is the E4 test for a decoded relay call.
func hiddenRelayCall(source map[string]any, spec toolSpec) bool {
	return relayOptionsFrom(source).HideEncryptedTools && encryptedNamespacedTool(spec)
}

// blockedRelayCallName is the name an E4 call is handed back under, without a
// namespace. Normally the bare name (spawn_agent): that is h17's result, and
// the client answers it as an unsupported call. But a client dispatches a
// call without a namespace by name, so when the request also declares a tool
// under that bare name outside any namespace, the bare name would run that
// other tool (h25 review item 6: top-level send next to agents.send). Then
// the call is handed back as "<namespace>.<name>" (agents.send), still
// without a namespace field: no declaration outside a namespace carries that
// name either, so it is unsupported as well. collision reports that case. If
// even that name is declared outside a namespace (no real catalogue does:
// tool names cannot contain a dot), ok is false and the call fails as
// invalid_tool_call, reason encrypted_tool_blocked. decodeRelayCall already
// fails that request before it gets here, in either declaration order (such
// a flat declaration makes the key ambiguous, ambiguousEncryptedKey); the
// check stays so that this function never returns a declared name.
//
// Tools inside another namespace do not count: a call without a namespace
// cannot address them, which is the very property E4 relies on (h17: a bare
// spawn_agent was refused although collaboration.spawn_agent was declared).
func blockedRelayCallName(source map[string]any, spec toolSpec) (name string, collision, ok bool) {
	declared := bareDeclaredToolNames(source)
	if !declared[spec.Name] {
		return spec.Name, false, true
	}
	if !declared[spec.Key] {
		return spec.Key, true, true
	}
	return "", true, false
}

// bareDeclaredToolNames is every function/custom tool name the request
// declares outside a namespace, wherever it declares it: the top-level tools,
// additional_tools, tool_search_output. Flat names such as
// collaboration__spawn_agent are among them. tool_choice is not consulted: a
// tool this request does not allow is still one the client can run.
func bareDeclaredToolNames(source map[string]any) map[string]bool {
	names := map[string]bool{}
	forEachDeclaredTool(source, func(spec toolSpec) {
		if spec.Namespace == "" {
			names[spec.Name] = true
		}
	})
	return names
}

// forEachDeclaredTool visits every function/custom tool declaration in the
// request: the top-level tools, then each additional_tools and
// tool_search_output item in input order.
func forEachDeclaredTool(source map[string]any, visit func(toolSpec)) {
	iterToolValues(source["tools"], "", visit)
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		if kind := stringValue(item["type"]); kind == "additional_tools" || kind == "tool_search_output" {
			iterToolValues(item["tools"], "", visit)
		}
	}
}

// summaryRelayCatalogue is the relay catalogue the summary prompts list: h22's
// catalogue minus what E1 hides, with the hidden keys.
func summaryRelayCatalogue(source map[string]any) (map[string]toolSpec, map[string]bool) {
	specs := clientToolSpecs(source)
	hidden := hiddenRelayToolKeys(source, specs)
	if len(hidden) == 0 {
		return specs, nil
	}
	return withoutHiddenTools(specs, hidden), hidden
}

// fullRelayCatalogue is the same for relay_tool_contracts: full (rc2's
// advertised catalogue).
func fullRelayCatalogue(source map[string]any) (map[string]toolSpec, map[string]bool) {
	specs := advertisedRelayToolSpecs(source)
	hidden := hiddenRelayToolKeys(source, specs)
	if len(hidden) == 0 {
		return specs, nil
	}
	return withoutHiddenTools(specs, hidden), hidden
}
