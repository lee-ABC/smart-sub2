package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tool input schema lowering for native promotion (A1), ported from Codex.
//
// A relayed client declares its tools at the top level with whatever JSON
// Schema it likes ($schema, default, title, minLength, ...). Codex never sends
// such schemas into additional_tools: every MCP and dynamic tool's input
// schema goes through parse_tool_input_schema first, and the Lite capture
// (907 requests, 10-06) only ever shows the keywords that survive it. To give
// the upstream the same kind of catalogue a real Lite client gives it, a
// promoted tool's parameters take the same route, rule for rule:
//
//   openai/codex @ea27864, codex-rs/tools/src/
//   - mcp_tool.rs:48-55     a missing or null top-level "properties" becomes {}
//   - json_schema.rs:47-52  sanitize, then prune unreachable definitions
//   - json_schema.rs:80-158 sanitize_json_schema (bool schemas, const -> enum,
//     type normalisation and inference, default children)
//   - json_schema.rs:166-181 malformed definition tables are dropped
//   - json_schema.rs:207-334 reachable-definition pruning, local refs only
//   - json_schema/compaction.rs:15-16,22-38 over 5,000 bytes (normalised,
//     compact JSON) the four lossy passes run in order until it fits
//   - json_schema/types.rs:36-75 the JsonSchema struct is the keyword
//     whitelist: everything else is dropped on deserialisation, and a value of
//     the wrong JSON type fails the whole schema
//   - json_schema.rs:54-63  a top-level singleton "null" type is an error
//
// A schema Codex would refuse (a wrong JSON type under a kept keyword) makes
// the whole request not promotable (schema_invalid): it stays on the relay,
// where it already works. Nothing here ever changes a request that is not
// promoted.

const (
	// a1CompactSchemaBytes is DEFAULT_COMPACT_TOOL_SCHEMA_BYTES.
	a1CompactSchemaBytes = 5000
	// a1CompactSchemaDepth is MAX_COMPACT_TOOL_SCHEMA_DEPTH.
	a1CompactSchemaDepth = 3
	// a1SchemaMaxDepth bounds recursion on hostile input; Codex has none, a
	// real tool schema is a handful of levels deep.
	a1SchemaMaxDepth = 64
)

var (
	a1DefinitionTables   = []string{"$defs", "definitions"}
	a1CompositionKeys    = []string{"anyOf", "oneOf", "allOf"}
	a1SchemaChildKeys    = []string{"items", "anyOf", "oneOf", "allOf"}
	a1PrimitiveTypeNames = map[string]bool{"string": true, "number": true, "boolean": true, "integer": true, "object": true, "array": true, "null": true}
	errA1SchemaInvalid   = errors.New("tool input schema cannot be represented")
	errA1SchemaTooDeep   = errors.New("tool input schema is nested too deeply")
)

// a1SchemaInfo is what one lowered schema contains, for the not-promotable
// list and the counters. Fixed labels only.
type a1SchemaInfo struct {
	ref, composition, enum, typeList bool
	additionalTrue, additionalSchema bool
	encrypted, compacted             bool
}

// a1ToolParameters lowers a function tool's "parameters" the way Codex lowers
// an MCP tool's input schema. raw is never modified.
func a1ToolParameters(raw any) (map[string]any, a1SchemaInfo, error) {
	var root map[string]any
	switch typed := raw.(type) {
	case nil:
		// A function without parameters (or with null): Codex has no such
		// tool; the MCP rule below turns {} into an empty object schema.
		root = map[string]any{}
	case map[string]any:
		root = cloneObject(typed)
	default:
		return nil, a1SchemaInfo{}, errA1SchemaInvalid
	}
	// mcp_tool.rs:48-55
	if properties, ok := root["properties"]; !ok || properties == nil {
		root["properties"] = map[string]any{}
	}
	lowered, info, err := a1LowerSchema(root)
	if err != nil {
		return nil, info, err
	}
	return lowered, info, nil
}

// a1LowerSchema is parse_tool_input_schema: prepare (sanitize + prune), the
// size policy, then the keyword whitelist. value is consumed.
func a1LowerSchema(value any) (map[string]any, a1SchemaInfo, error) {
	var info a1SchemaInfo
	value, err := a1Sanitize(value, 0)
	if err != nil {
		return nil, info, err
	}
	a1PruneUnreachableDefinitions(value)
	value, info.compacted = a1CompactLargeSchema(value, a1CompactSchemaBytes)
	projected, err := a1ProjectSchema(value, 0)
	if err != nil {
		return nil, info, err
	}
	if kind, ok := projected["type"].(string); ok && kind == "null" {
		// json_schema.rs:56-61 singleton_null_schema_error
		return nil, info, errA1SchemaInvalid
	}
	a1ScanSchema(projected, &info, 0)
	return projected, info, nil
}

// ---------------------------------------------------------------- sanitize

// a1Sanitize is sanitize_json_schema (json_schema.rs:80-158). Maps are
// changed in place; the returned value replaces the argument.
func a1Sanitize(value any, depth int) (any, error) {
	if depth > a1SchemaMaxDepth {
		return nil, errA1SchemaTooDeep
	}
	switch typed := value.(type) {
	case bool:
		// JSON Schema boolean form: an accept-all string (json_schema.rs:82-85).
		return map[string]any{"type": "string"}, nil
	case []any:
		for index, element := range typed {
			cleaned, err := a1Sanitize(element, depth+1)
			if err != nil {
				return nil, err
			}
			typed[index] = cleaned
		}
		return typed, nil
	case map[string]any:
		if properties, ok := typed["properties"].(map[string]any); ok {
			for name, child := range properties {
				cleaned, err := a1Sanitize(child, depth+1)
				if err != nil {
					return nil, err
				}
				properties[name] = cleaned
			}
		}
		if items, ok := typed["items"]; ok {
			cleaned, err := a1Sanitize(items, depth+1)
			if err != nil {
				return nil, err
			}
			typed["items"] = cleaned
		}
		if additional, ok := typed["additionalProperties"]; ok {
			if _, isBool := additional.(bool); !isBool {
				cleaned, err := a1Sanitize(additional, depth+1)
				if err != nil {
					return nil, err
				}
				typed["additionalProperties"] = cleaned
			}
		}
		if prefix, ok := typed["prefixItems"]; ok {
			cleaned, err := a1Sanitize(prefix, depth+1)
			if err != nil {
				return nil, err
			}
			typed["prefixItems"] = cleaned
		}
		for _, key := range a1CompositionKeys {
			if child, ok := typed[key]; ok {
				cleaned, err := a1Sanitize(child, depth+1)
				if err != nil {
					return nil, err
				}
				typed[key] = cleaned
			}
		}
		for _, table := range a1DefinitionTables {
			// sanitize_schema_table: an object table is sanitised entry by
			// entry; any other value (null included) is dropped.
			raw, present := typed[table]
			if !present {
				continue
			}
			definitions, ok := raw.(map[string]any)
			if !ok {
				delete(typed, table)
				continue
			}
			for name, definition := range definitions {
				cleaned, err := a1Sanitize(definition, depth+1)
				if err != nil {
					return nil, err
				}
				definitions[name] = cleaned
			}
		}
		if constant, ok := typed["const"]; ok {
			delete(typed, "const")
			typed["enum"] = []any{constant}
		}
		types := a1NormalizedSchemaTypes(typed)
		_, hasRef := typed["$ref"]
		if len(types) == 0 && (hasRef || a1HasComposition(typed)) {
			return typed, nil
		}
		if len(types) == 0 {
			switch {
			case a1HasAny(typed, "properties", "required", "additionalProperties"):
				types = append(types, "object")
			case a1HasAny(typed, "items", "prefixItems"):
				types = append(types, "array")
			case a1HasAny(typed, "enum", "format"):
				types = append(types, "string")
			case a1HasAny(typed, "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"):
				types = append(types, "number")
			default:
				// No recognised hint: the empty, permissive schema.
				for key := range typed {
					delete(typed, key)
				}
				return typed, nil
			}
		}
		a1WriteSchemaTypes(typed, types)
		// ensure_default_children_for_schema_types (json_schema.rs:183-197)
		if a1ContainsType(types, "object") {
			if _, ok := typed["properties"]; !ok {
				typed["properties"] = map[string]any{}
			}
		}
		if a1ContainsType(types, "array") {
			if _, ok := typed["items"]; !ok {
				typed["items"] = map[string]any{"type": "string"}
			}
		}
		return typed, nil
	default:
		return value, nil
	}
}

func a1HasAny(object map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

func a1HasComposition(object map[string]any) bool {
	return a1HasAny(object, a1CompositionKeys...)
}

func a1ContainsType(types []string, want string) bool {
	for _, kind := range types {
		if kind == want {
			return true
		}
	}
	return false
}

// a1NormalizedSchemaTypes is normalized_schema_types (json_schema.rs:336-352):
// known type names only, from a string or a list.
func a1NormalizedSchemaTypes(object map[string]any) []string {
	switch typed := object["type"].(type) {
	case string:
		if a1PrimitiveTypeNames[typed] {
			return []string{typed}
		}
	case []any:
		var types []string
		for _, value := range typed {
			if name, ok := value.(string); ok && a1PrimitiveTypeNames[name] {
				types = append(types, name)
			}
		}
		return types
	}
	return nil
}

// a1WriteSchemaTypes is write_schema_types (json_schema.rs:354-383).
func a1WriteSchemaTypes(object map[string]any, types []string) {
	switch len(types) {
	case 0:
		delete(object, "type")
	case 1:
		object["type"] = types[0]
	default:
		list := make([]any, len(types))
		for index, kind := range types {
			list[index] = kind
		}
		object["type"] = list
	}
}

// ---------------------------------------------------------------- definitions

type a1DefinitionPointer struct{ table, name string }

// a1PruneUnreachableDefinitions is prune_unreachable_definitions
// (json_schema.rs:207-237): root definition entries no reference outside the
// tables reaches, directly or through other definitions, are dropped; an
// emptied table goes with them.
func a1PruneUnreachableDefinitions(value any) {
	root, ok := value.(map[string]any)
	if !ok {
		return
	}
	reachable := map[a1DefinitionPointer]bool{}
	var pending []a1DefinitionPointer
	a1CollectRefsOutsideDefinitions(value, &pending, 0)
	for len(pending) > 0 {
		pointer := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if reachable[pointer] {
			continue
		}
		reachable[pointer] = true
		if table, ok := root[pointer.table].(map[string]any); ok {
			if definition, ok := table[pointer.name]; ok {
				a1CollectRefs(definition, &pending, 0)
			}
		}
	}
	for _, name := range a1DefinitionTables {
		table, ok := root[name].(map[string]any)
		if !ok {
			continue
		}
		for entry := range table {
			if !reachable[a1DefinitionPointer{table: name, name: entry}] {
				delete(table, entry)
			}
		}
		if len(table) == 0 {
			delete(root, name)
		}
	}
}

func a1CollectRefsOutsideDefinitions(value any, refs *[]a1DefinitionPointer, depth int) {
	if depth > a1SchemaMaxDepth {
		return
	}
	switch typed := value.(type) {
	case []any:
		for _, element := range typed {
			a1CollectRefsOutsideDefinitions(element, refs, depth+1)
		}
	case map[string]any:
		a1CollectRefFromMap(typed, refs)
		a1ForEachSchemaChild(typed, false, func(child any) any {
			a1CollectRefsOutsideDefinitions(child, refs, depth+1)
			return child
		})
	}
}

func a1CollectRefs(value any, refs *[]a1DefinitionPointer, depth int) {
	if depth > a1SchemaMaxDepth {
		return
	}
	switch typed := value.(type) {
	case []any:
		for _, element := range typed {
			a1CollectRefs(element, refs, depth+1)
		}
	case map[string]any:
		a1CollectRefFromMap(typed, refs)
		for _, child := range typed {
			a1CollectRefs(child, refs, depth+1)
		}
	}
}

func a1CollectRefFromMap(object map[string]any, refs *[]a1DefinitionPointer) {
	if ref, ok := object["$ref"].(string); ok {
		if pointer, ok := a1ParseLocalDefinitionRef(ref); ok {
			*refs = append(*refs, pointer)
		}
	}
}

// a1ParseLocalDefinitionRef is parse_local_definition_ref
// (json_schema.rs:316-334): "#" + a percent-encoded JSON pointer whose first
// token names a definition table; the second token is the definition, and a
// longer pointer keeps its parent definition reachable.
func a1ParseLocalDefinitionRef(ref string) (a1DefinitionPointer, bool) {
	fragment, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return a1DefinitionPointer{}, false
	}
	decoded, ok := a1PercentDecode(fragment)
	if !ok {
		return a1DefinitionPointer{}, false
	}
	if decoded == "" || !strings.HasPrefix(decoded, "/") {
		return a1DefinitionPointer{}, false
	}
	tokens := strings.Split(decoded[1:], "/")
	if len(tokens) < 2 {
		return a1DefinitionPointer{}, false
	}
	table, ok := a1PointerToken(tokens[0])
	if !ok || (table != "$defs" && table != "definitions") {
		return a1DefinitionPointer{}, false
	}
	name, ok := a1PointerToken(tokens[1])
	if !ok {
		return a1DefinitionPointer{}, false
	}
	return a1DefinitionPointer{table: table, name: name}, true
}

// a1PointerToken decodes one JSON pointer token (~1 = "/", ~0 = "~"); any
// other "~" escape is malformed, as in jsonptr.
func a1PointerToken(token string) (string, bool) {
	if !strings.Contains(token, "~") {
		return token, true
	}
	var out strings.Builder
	for index := 0; index < len(token); index++ {
		if token[index] != '~' {
			out.WriteByte(token[index])
			continue
		}
		if index+1 >= len(token) {
			return "", false
		}
		switch token[index+1] {
		case '0':
			out.WriteByte('~')
		case '1':
			out.WriteByte('/')
		default:
			return "", false
		}
		index++
	}
	return out.String(), true
}

// a1PercentDecode is urlencoding::decode: %XX becomes the byte, anything else
// is kept, and the result must be UTF-8.
func a1PercentDecode(text string) (string, bool) {
	if !strings.Contains(text, "%") {
		return text, utf8.ValidString(text)
	}
	var out bytes.Buffer
	for index := 0; index < len(text); index++ {
		if text[index] == '%' && index+2 < len(text) {
			if value, err := strconv.ParseUint(text[index+1:index+3], 16, 8); err == nil {
				out.WriteByte(byte(value))
				index += 2
				continue
			}
		}
		out.WriteByte(text[index])
	}
	decoded := out.String()
	return decoded, utf8.ValidString(decoded)
}

// a1ForEachSchemaChild is for_each_schema_child(_mut)
// (json_schema/traversal.rs:13-87): properties' values, items, anyOf, oneOf,
// allOf, a schema-valued additionalProperties and, when includeDefinitions,
// the root definition tables. visit returns the replacement.
func a1ForEachSchemaChild(object map[string]any, includeDefinitions bool, visit func(any) any) {
	if properties, ok := object["properties"].(map[string]any); ok {
		for name, child := range properties {
			properties[name] = visit(child)
		}
	}
	for _, key := range a1SchemaChildKeys {
		if child, ok := object[key]; ok {
			object[key] = visit(child)
		}
	}
	if additional, ok := object["additionalProperties"]; ok {
		if _, isBool := additional.(bool); !isBool {
			object["additionalProperties"] = visit(additional)
		}
	}
	if includeDefinitions {
		for _, table := range a1DefinitionTables {
			if definitions, ok := object[table].(map[string]any); ok {
				for name, child := range definitions {
					definitions[name] = visit(child)
				}
			}
		}
	}
}

// ---------------------------------------------------------------- size policy

// a1CompactLargeSchema is compact_large_tool_schema
// (json_schema/compaction.rs:22-38): while the normalised schema is over
// budget, apply the next lossy pass. It reports whether any pass ran.
func a1CompactLargeSchema(value any, budget int) (any, bool) {
	passes := []func(any) any{
		a1StripSchemaDescriptions,
		a1DropSchemaDefinitions,
		func(value any) any { return a1CollapseDeepSchemaObjects(value, 0) },
		a1PruneSchemaCompositions,
	}
	compacted := false
	for _, pass := range passes {
		if a1NormalizedSchemaLen(value) <= budget {
			break
		}
		value = pass(value)
		compacted = true
	}
	return value, compacted
}

// a1NormalizedSchemaLen is compact_normalized_schema_len: the compact JSON of
// the schema after the keyword whitelist, or 0 when it would not deserialise.
func a1NormalizedSchemaLen(value any) int {
	// Projection builds a new tree; value itself is not touched.
	projected, err := a1ProjectSchema(value, 0)
	if err != nil {
		return 0
	}
	return len(a1CompactJSON(projected))
}

func a1StripSchemaDescriptions(value any) any {
	switch typed := value.(type) {
	case []any:
		for index, element := range typed {
			typed[index] = a1StripSchemaDescriptions(element)
		}
	case map[string]any:
		delete(typed, "description")
		a1ForEachSchemaChild(typed, true, a1StripSchemaDescriptions)
	}
	return value
}

func a1DropSchemaDefinitions(value any) any {
	value = a1RewriteDefinitionRefsToEmptySchemas(value)
	if root, ok := value.(map[string]any); ok {
		for _, table := range a1DefinitionTables {
			delete(root, table)
		}
	}
	return value
}

func a1RewriteDefinitionRefsToEmptySchemas(value any) any {
	switch typed := value.(type) {
	case []any:
		for index, element := range typed {
			typed[index] = a1RewriteDefinitionRefsToEmptySchemas(element)
		}
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok {
			if _, local := a1ParseLocalDefinitionRef(ref); local {
				return map[string]any{}
			}
		}
		a1ForEachSchemaChild(typed, false, a1RewriteDefinitionRefsToEmptySchemas)
	}
	return value
}

func a1CollapseDeepSchemaObjects(value any, depth int) any {
	switch typed := value.(type) {
	case []any:
		for index, element := range typed {
			typed[index] = a1CollapseDeepSchemaObjects(element, depth)
		}
	case map[string]any:
		if depth >= a1CompactSchemaDepth && a1IsComplexSchemaObject(typed) {
			return map[string]any{}
		}
		a1ForEachSchemaChild(typed, false, func(child any) any { return a1CollapseDeepSchemaObjects(child, depth+1) })
	}
	return value
}

func a1PruneSchemaCompositions(value any) any {
	switch typed := value.(type) {
	case []any:
		for index, element := range typed {
			typed[index] = a1PruneSchemaCompositions(element)
		}
	case map[string]any:
		if a1HasComposition(typed) {
			return map[string]any{}
		}
		a1ForEachSchemaChild(typed, false, a1PruneSchemaCompositions)
	}
	return value
}

func a1IsComplexSchemaObject(object map[string]any) bool {
	return a1HasAny(object, "items", "anyOf", "oneOf", "allOf", "properties", "additionalProperties", "$ref")
}

// ---------------------------------------------------------------- whitelist

// a1ProjectSchema is deserialising into Codex's JsonSchema and serialising it
// again (json_schema/types.rs:36-75): only the struct's keywords survive, a
// null is the same as absent, and a value of the wrong JSON type is an error.
func a1ProjectSchema(value any, depth int) (map[string]any, error) {
	if depth > a1SchemaMaxDepth {
		return nil, errA1SchemaTooDeep
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errA1SchemaInvalid
	}
	out := map[string]any{}
	if raw, present := object["$ref"]; present && raw != nil {
		ref, ok := raw.(string)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		out["$ref"] = ref
	}
	if raw, present := object["type"]; present && raw != nil {
		switch typed := raw.(type) {
		case string:
			if !a1PrimitiveTypeNames[typed] {
				return nil, errA1SchemaInvalid
			}
			out["type"] = typed
		case []any:
			list := make([]any, 0, len(typed))
			for _, element := range typed {
				name, ok := element.(string)
				if !ok || !a1PrimitiveTypeNames[name] {
					return nil, errA1SchemaInvalid
				}
				list = append(list, name)
			}
			out["type"] = list
		default:
			return nil, errA1SchemaInvalid
		}
	}
	if raw, present := object["description"]; present && raw != nil {
		text, ok := raw.(string)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		out["description"] = text
	}
	if raw, present := object["encrypted"]; present && raw != nil {
		flag, ok := raw.(bool)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		out["encrypted"] = flag
	}
	if raw, present := object["enum"]; present && raw != nil {
		values, ok := raw.([]any)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		out["enum"] = cloneValue(values)
	}
	if raw, present := object["items"]; present && raw != nil {
		items, err := a1ProjectSchema(raw, depth+1)
		if err != nil {
			return nil, err
		}
		out["items"] = items
	}
	if raw, present := object["minItems"]; present && raw != nil {
		number, ok := a1NonNegativeInteger(raw)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		out["minItems"] = number
	}
	if raw, present := object["properties"]; present && raw != nil {
		properties, ok := raw.(map[string]any)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		projected := make(map[string]any, len(properties))
		for name, child := range properties {
			schema, err := a1ProjectSchema(child, depth+1)
			if err != nil {
				return nil, err
			}
			projected[name] = schema
		}
		out["properties"] = projected
	}
	if raw, present := object["required"]; present && raw != nil {
		list, ok := raw.([]any)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		required := make([]any, 0, len(list))
		for _, element := range list {
			name, ok := element.(string)
			if !ok {
				return nil, errA1SchemaInvalid
			}
			required = append(required, name)
		}
		out["required"] = required
	}
	if raw, present := object["additionalProperties"]; present && raw != nil {
		switch typed := raw.(type) {
		case bool:
			out["additionalProperties"] = typed
		default:
			schema, err := a1ProjectSchema(typed, depth+1)
			if err != nil {
				return nil, err
			}
			out["additionalProperties"] = schema
		}
	}
	for _, key := range a1CompositionKeys {
		raw, present := object[key]
		if !present || raw == nil {
			continue
		}
		list, ok := raw.([]any)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		variants := make([]any, 0, len(list))
		for _, element := range list {
			schema, err := a1ProjectSchema(element, depth+1)
			if err != nil {
				return nil, err
			}
			variants = append(variants, schema)
		}
		out[key] = variants
	}
	for _, table := range a1DefinitionTables {
		raw, present := object[table]
		if !present || raw == nil {
			continue
		}
		definitions, ok := raw.(map[string]any)
		if !ok {
			return nil, errA1SchemaInvalid
		}
		projected := make(map[string]any, len(definitions))
		for name, child := range definitions {
			schema, err := a1ProjectSchema(child, depth+1)
			if err != nil {
				return nil, err
			}
			projected[name] = schema
		}
		out[table] = projected
	}
	return out, nil
}

// a1NonNegativeInteger reads minItems the way serde reads a usize: a JSON
// integer literal >= 0 (1.0 and 1e0 are refused).
func a1NonNegativeInteger(value any) (any, bool) {
	switch typed := value.(type) {
	case json.Number:
		if _, err := strconv.ParseUint(string(typed), 10, 64); err != nil {
			return nil, false
		}
		return typed, true
	case float64:
		if typed < 0 || typed != math.Trunc(typed) || typed > math.MaxInt64 {
			return nil, false
		}
		return json.Number(strconv.FormatInt(int64(typed), 10)), true
	case int:
		if typed < 0 {
			return nil, false
		}
		return json.Number(strconv.Itoa(typed)), true
	case int64:
		if typed < 0 {
			return nil, false
		}
		return json.Number(strconv.FormatInt(typed, 10)), true
	}
	return nil, false
}

// a1ScanSchema records which of the keywords that survive lowering, but never
// appeared in a real Lite catalogue, this schema uses.
func a1ScanSchema(value any, info *a1SchemaInfo, depth int) {
	if depth > a1SchemaMaxDepth {
		return
	}
	switch typed := value.(type) {
	case []any:
		for _, element := range typed {
			a1ScanSchema(element, info, depth+1)
		}
	case map[string]any:
		if _, ok := typed["$ref"]; ok {
			info.ref = true
		}
		for _, table := range a1DefinitionTables {
			if _, ok := typed[table]; ok {
				info.ref = true
			}
		}
		if a1HasComposition(typed) {
			info.composition = true
		}
		if _, ok := typed["enum"]; ok {
			info.enum = true
		}
		if _, ok := typed["type"].([]any); ok {
			info.typeList = true
		}
		switch additional := typed["additionalProperties"].(type) {
		case bool:
			if additional {
				info.additionalTrue = true
			}
		case map[string]any:
			info.additionalSchema = true
		}
		if flag, ok := typed["encrypted"].(bool); ok && flag {
			info.encrypted = true
		}
		a1ForEachSchemaChild(typed, true, func(child any) any {
			a1ScanSchema(child, info, depth+1)
			return child
		})
	}
}

// a1CompactJSON is serde_json::to_vec: compact, no HTML escaping. The
// budget compares lengths only, so key order does not matter.
func a1CompactJSON(value any) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return jsonBytes(value)
	}
	return bytes.TrimRight(buffer.Bytes(), "\n")
}
