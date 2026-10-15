package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// This observer never decides whether a tool may be sent. Call it only for the
// selected tool, behind a separate, default-off configuration switch. In
// particular, do not compile the whole catalogue or substitute this result for
// schemaMatches. Codes and reasons below are code-owned, safe diagnostic labels.
//
// jsonschema v6.0.3 has no context-aware Validate or instruction/time budget.
// These admission limits bound the data/graph handed to it, not wall-clock time
// or process memory. There are no timeout goroutines left running after return.
// A single nonblocking slot limits additional concurrent work in this process.
type schemaShadowResult struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Draft  string `json:"draft"`
}

const (
	shadowSchemaBytes    = 32 << 10
	shadowInstanceBytes  = 128 << 10
	shadowDocumentNodes  = 2048
	shadowDocumentDepth  = 32
	shadowSchemaVisits   = 256
	shadowEvaluationWork = 1 << 17
	shadowNumericWork    = 8192
	shadowNumberDigits   = 256
	shadowNumberExponent = 256
	shadowPatternBytes   = 128
	shadowPatternCount   = 8
	shadowRegexWork      = 1 << 20
	shadowSchemaURL      = "https://schema-shadow.invalid/selected-tool.json"
)

var schemaShadowSlot = make(chan struct{}, 1)

func shadowResult(code, reason, draft string) schemaShadowResult {
	return schemaShadowResult{Code: code, Reason: reason, Draft: draft}
}

func observeSchemaShadow(schema, instance any) (result schemaShadowResult) {
	result = shadowResult("unknown", "observer_failure", "unknown")
	defer func() {
		if recover() != nil {
			result = shadowResult("unknown", "observer_failure", "unknown")
		}
	}()
	select {
	case schemaShadowSlot <- struct{}{}:
		defer func() { <-schemaShadowSlot }()
	default:
		return shadowResult("unknown", "busy", "unknown")
	}
	if schema == nil {
		return shadowResult("unsupported", "no_schema", "unknown")
	}

	schemaCopy, schemaStats, reason := shadowDocument(schema, shadowSchemaBytes)
	if reason != "" {
		return shadowResult("unknown", "schema_"+reason, "unknown")
	}
	instanceCopy, instanceStats, reason := shadowDocument(instance, shadowInstanceBytes)
	if reason != "" {
		return shadowResult("unknown", "instance_"+reason, "unknown")
	}
	draft, draftName, ok := shadowDraft(schemaCopy)
	if !ok {
		return shadowResult("unsupported", "draft", "unknown")
	}
	inspection := shadowSchemaInspection{root: schemaCopy, visits: shadowSchemaVisits, active: map[string]bool{"#": true}}
	if code, why := inspection.inspect(schemaCopy, 0); code != "" {
		return shadowResult(code, why, draftName)
	}
	evaluations := shadowSchemaVisits - inspection.visits
	if evaluations*instanceStats.nodes > shadowEvaluationWork {
		return shadowResult("unknown", "evaluation_budget", draftName)
	}
	// uniqueItems can compare many pairs of nested values. Its extra estimate
	// is deliberately conservative, including the schema expansion factor.
	if inspection.unique && evaluations*instanceStats.nodes*instanceStats.maxArray > shadowEvaluationWork {
		return shadowResult("unknown", "unique_items_budget", draftName)
	}
	if inspection.patterns > 0 {
		// RE2 and ECMA-262 differ for Unicode, newlines and some regex syntax.
		// Do not report a mismatch in cases outside our shared ASCII subset.
		if !instanceStats.ascii || !schemaStats.ascii || inspection.patternBytes*(instanceStats.textBytes+1) > shadowRegexWork {
			return shadowResult("unsupported", "regex_dialect_or_budget", draftName)
		}
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(draft)
	compiler.UseLoader(shadowNoExternalLoader{})
	// Use the library's standard Go regexp engine; preflight admits only a
	// restricted shared syntax, so no backtracking engine is installed.
	if compiler.AddResource(shadowSchemaURL, schemaCopy) != nil {
		return shadowResult("compile_error", "invalid_schema", draftName)
	}
	compiled, err := compiler.Compile(shadowSchemaURL)
	if err != nil {
		return shadowResult("compile_error", "invalid_schema", draftName)
	}
	// Never call Error() on library diagnostics: they may contain argument
	// values, property names, schemas or reference URLs supplied by a client.
	if err := compiled.Validate(instanceCopy); err != nil {
		var mismatch *jsonschema.ValidationError
		if errors.As(err, &mismatch) {
			return shadowResult("mismatch", "assertion", draftName)
		}
		return shadowResult("unknown", "validator_failure", draftName)
	}
	return shadowResult("match", "validated", draftName)
}

type shadowNoExternalLoader struct{}

func (shadowNoExternalLoader) Load(string) (any, error) {
	return nil, errors.New("external schema resources are disabled")
}

// No declaration means 2020-12; explicit draft-07 is also accepted. Other
// dialects/custom metaschemas are unsupported rather than silently reinterpreted.
// The preflight currently skips format/content observations. The library defaults
// differ by draft; AssertContent/AssertFormat/AssertVocabs are never enabled.
func shadowDraft(schema any) (*jsonschema.Draft, string, bool) {
	if schema, ok := schema.(map[string]any); ok {
		if raw, exists := schema["$schema"]; exists {
			declared, ok := raw.(string)
			if !ok {
				return nil, "unknown", false
			}
			switch declared {
			case "https://json-schema.org/draft/2020-12/schema", "https://json-schema.org/draft/2020-12/schema#":
				return jsonschema.Draft2020, "2020-12", true
			case "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft-07/schema#", "http://json-schema.org/draft-07/schema", "https://json-schema.org/draft-07/schema":
				return jsonschema.Draft7, "draft-07", true
			default:
				return nil, "unknown", false
			}
		}
	}
	return jsonschema.Draft2020, "2020-12", true
}

type shadowDocumentStats struct {
	nodes, textBytes, numericWork, maxArray int
	ascii                                   bool
}

// Walk before marshaling: cyclic/deep or very large input must not start an
// unbounded JSON allocation. Only JSON-shaped builtins are admitted; arbitrary
// MarshalJSON implementations are never invoked. Round-tripping then provides
// private, normalized trees while preserving exact json.Number values.
func shadowDocument(value any, maxBytes int) (any, shadowDocumentStats, string) {
	stats := shadowDocumentStats{ascii: true}
	var visit func(any, int) string
	visit = func(value any, depth int) string {
		stats.nodes++
		if depth > shadowDocumentDepth || stats.nodes > shadowDocumentNodes {
			return "shape_budget"
		}
		addText := func(text string) string {
			stats.textBytes += len(text)
			if stats.textBytes > maxBytes {
				return "byte_budget"
			}
			stats.ascii = stats.ascii && printableASCII(text)
			return ""
		}
		switch typed := value.(type) {
		case nil, bool:
			return ""
		case string:
			return addText(typed)
		case json.Number, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			number := fmt.Sprint(typed)
			if len(number) > shadowNumberDigits || !json.Valid([]byte(number)) {
				return "number_budget"
			}
			work := len(number)
			if exponent := strings.IndexAny(number, "eE"); exponent >= 0 {
				n, err := strconv.Atoi(number[exponent+1:])
				if err != nil || n < -shadowNumberExponent || n > shadowNumberExponent {
					return "number_budget"
				}
				if n < 0 {
					n = -n
				}
				work += n
			}
			stats.numericWork += work
			if stats.numericWork > shadowNumericWork {
				return "number_budget"
			}
			return ""
		case []any:
			if len(typed) > shadowDocumentNodes-stats.nodes {
				return "shape_budget"
			}
			if len(typed) > stats.maxArray {
				stats.maxArray = len(typed)
			}
			for _, child := range typed {
				if reason := visit(child, depth+1); reason != "" {
					return reason
				}
			}
			return ""
		case map[string]any:
			if len(typed) > shadowDocumentNodes-stats.nodes {
				return "shape_budget"
			}
			for name, child := range typed {
				if reason := addText(name); reason != "" {
					return reason
				}
				if reason := visit(child, depth+1); reason != "" {
					return reason
				}
			}
			return ""
		default:
			return "non_json_value"
		}
	}
	if reason := visit(value, 0); reason != "" {
		return nil, stats, reason
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, stats, "invalid_json"
	}
	if len(raw) > maxBytes {
		return nil, stats, "byte_budget"
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var copy any
	if decoder.Decode(&copy) != nil {
		return nil, stats, "invalid_json"
	}
	return copy, stats, ""
}

type shadowSchemaInspection struct {
	root                           any
	visits, patterns, patternBytes int
	active                         map[string]bool
	unique                         bool
}

func (s *shadowSchemaInspection) inspect(raw any, depth int) (string, string) {
	if s.visits <= 0 || depth > shadowDocumentDepth {
		return "unknown", "schema_expansion_budget"
	}
	s.visits--
	if _, ok := raw.(bool); ok {
		return "", ""
	}
	schema, ok := raw.(map[string]any)
	if !ok {
		return "compile_error", "invalid_schema"
	}
	for keyword, value := range schema {
		switch keyword {
		case "$schema":
			if depth != 0 {
				return "unsupported", "nested_draft"
			}
		case "$id", "id", "$anchor", "$dynamicAnchor", "$dynamicRef", "$recursiveRef", "$recursiveAnchor", "$vocabulary":
			return "unsupported", "reference_scope"
		case "$ref":
			ref, ok := value.(string)
			if !ok {
				return "compile_error", "invalid_reference"
			}
			if !strings.HasPrefix(ref, "#") {
				return "unsupported", "external_reference"
			}
			fragment, err := url.PathUnescape(ref[1:])
			if err != nil || (fragment != "" && !strings.HasPrefix(fragment, "/")) {
				return "unsupported", "reference_syntax"
			}
			key := "#" + fragment
			if s.active[key] {
				return "unsupported", "recursive_reference"
			}
			target, ok := shadowResolvePointer(s.root, fragment)
			if !ok {
				return "compile_error", "invalid_reference"
			}
			s.active[key] = true
			code, why := s.inspect(target, depth+1)
			delete(s.active, key)
			if code != "" {
				return code, why
			}
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
			children, ok := value.(map[string]any)
			if !ok {
				return "compile_error", "invalid_schema"
			}
			for key, child := range children {
				if keyword == "patternProperties" {
					if !s.pattern(key) {
						return "unsupported", "regex_syntax_or_budget"
					}
				}
				if code, why := s.inspect(child, depth+1); code != "" {
					return code, why
				}
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			children, ok := value.([]any)
			if !ok {
				return "compile_error", "invalid_schema"
			}
			for _, child := range children {
				if code, why := s.inspect(child, depth+1); code != "" {
					return code, why
				}
			}
		case "items":
			if children, ok := value.([]any); ok {
				for _, child := range children {
					if code, why := s.inspect(child, depth+1); code != "" {
						return code, why
					}
				}
			} else if code, why := s.inspect(value, depth+1); code != "" {
				return code, why
			}
		case "additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else":
			if code, why := s.inspect(value, depth+1); code != "" {
				return code, why
			}
		case "pattern":
			pattern, ok := value.(string)
			if !ok || !s.pattern(pattern) {
				return "unsupported", "regex_syntax_or_budget"
			}
		case "uniqueItems":
			if flag, ok := value.(bool); ok && flag {
				s.unique = true
			}
		case "format":
			// Format 'regex' would compile client instance text in draft-07.
			// Skip all format observations rather than expose unbudgeted engines
			// or pretend draft-dependent annotation behavior is an assertion.
			return "unsupported", "format"
		case "contentEncoding", "contentMediaType", "contentSchema", "dependencies":
			return "unsupported", "keyword"
		case "type", "required", "enum", "const", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties", "minContains", "maxContains", "dependentRequired", "title", "description", "default", "examples", "$comment", "deprecated", "readOnly", "writeOnly":
			// Assertions are evaluated by the pinned library, not duplicated here.
		default:
			return "unsupported", "keyword"
		}
	}
	return "", ""
}

func (s *shadowSchemaInspection) pattern(pattern string) bool {
	s.patterns++
	s.patternBytes += len(pattern) + 1
	if s.patterns > shadowPatternCount || len(pattern) > shadowPatternBytes || !portableSchemaPattern(pattern) {
		return false
	}
	_, err := regexp.Compile(pattern)
	return err == nil
}

func shadowResolvePointer(root any, pointer string) (any, bool) {
	if pointer == "" {
		return root, true
	}
	current := root
	for _, raw := range strings.Split(pointer[1:], "/") {
		// Reject malformed JSON Pointer escapes rather than reinterpret them.
		for i := 0; i < len(raw); i++ {
			if raw[i] == '~' {
				i++
				if i >= len(raw) || (raw[i] != '0' && raw[i] != '1') {
					return nil, false
				}
			}
		}
		part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch value := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = value[part]
			if !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(value) || strconv.Itoa(i) != part {
				return nil, false
			}
			current = value[i]
		default:
			return nil, false
		}
	}
	return current, true
}
