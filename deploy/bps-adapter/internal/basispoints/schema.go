package basispoints

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// This is a bounded compatibility check, NOT a JSON Schema validator. It only
// rejects contradictions established by its supported assertions. Unknown
// keywords, dialect-dependent regexes and exhausted budgets remain unknown.
// Relay decoding takes only schemaCompatible as an acceptance of its own
// (h25 review, E3); unknown leaves the verdict to h22's check.
// Object applicators follow JSON Schema 2020-12 sections 10.3.2.1-3: properties
// and EVERY matching pattern both apply; additionalProperties only applies to
// names matched by neither. No references are fetched or evaluated here.
type schemaCompatibility uint8

const (
	schemaCompatible schemaCompatibility = iota
	schemaUnknown
	schemaMismatch
	schemaMaxDepth        = 64
	schemaMaxVisits       = 20000
	schemaMaxTextBytes    = 4 << 20
	schemaMaxPatternBytes = 1024
	schemaMaxRegexWork    = 4 << 20
)

type schemaBudget struct {
	visits, textBytes, regexWork int
	maxDepth                     int
	patterns                     map[string]*regexp.Regexp // nil records an unsupported pattern
}

func newSchemaBudget() *schemaBudget {
	return &schemaBudget{visits: schemaMaxVisits, textBytes: schemaMaxTextBytes, regexWork: schemaMaxRegexWork, maxDepth: schemaMaxDepth, patterns: make(map[string]*regexp.Regexp)}
}

func (b *schemaBudget) visit() bool {
	if b.visits <= 0 {
		return false
	}
	b.visits--
	return true
}

func (b *schemaBudget) text(n int) bool {
	if n < 0 || n > b.textBytes {
		return false
	}
	b.textBytes -= n
	return true
}

func combineSchemaResults(a, b schemaCompatibility) schemaCompatibility {
	if a == schemaMismatch || b == schemaMismatch {
		return schemaMismatch
	}
	if a == schemaUnknown || b == schemaUnknown {
		return schemaUnknown
	}
	return schemaCompatible
}

// true means "not proven incompatible", including schemaUnknown. That makes
// it a rejection test only: relay decoding never takes it as an acceptance
// (relaySchemaAccepts in h22_compat.go wants schemaCompatible).
func schemaMatches(value any, schema map[string]any) bool {
	return checkSchemaCompatibility(value, schema) != schemaMismatch
}

func checkSchemaCompatibility(value, schema any) schemaCompatibility {
	return schemaCheckWithin(value, schema, 0, newSchemaBudget())
}

func schemaCheckWithin(value, rawSchema any, depth int, budget *schemaBudget) schemaCompatibility {
	if depth > schemaMaxDepth || !budget.visit() {
		return schemaUnknown
	}
	if allow, ok := rawSchema.(bool); ok {
		if allow {
			return schemaCompatible
		}
		return schemaMismatch
	}
	schema, ok := rawSchema.(map[string]any)
	if !ok {
		return schemaUnknown
	}
	if _, hasReference := schema["$ref"]; hasReference {
		// Drafts through draft-7 ignore siblings of $ref, while newer drafts
		// apply them. We neither resolve references nor track inherited
		// dialects, so even a sibling type/required cannot prove rejection at
		// this location. Independent assertions in the parent still apply.
		return schemaUnknown
	}
	result := schemaCompatible
	// An enormous set of unknown keywords must not consume the assertion
	// budget and hide a cheap, definite type/required contradiction.
	if len(schema) > 64 {
		result = schemaUnknown
	} else {
		for keyword := range schema {
			switch keyword {
			case "type", "required", "properties", "patternProperties", "additionalProperties", "items", "enum":
			case "title", "description", "default", "examples", "$comment", "deprecated", "readOnly", "writeOnly":
			default:
				result = schemaUnknown
			}
		}
	}
	if declared, present := schema["type"]; present {
		result = combineSchemaResults(result, schemaTypeCompatibility(value, declared, budget))
		if result == schemaMismatch {
			return result
		}
	}
	if object, ok := value.(map[string]any); ok {
		result = combineSchemaResults(result, schemaObjectCompatibility(object, schema, depth, budget))
		if result == schemaMismatch {
			return result
		}
	}
	if array, ok := value.([]any); ok {
		if itemSchema, present := schema["items"]; present {
			if _, prefixPresent := schema["prefixItems"]; prefixPresent {
				// Applying items to the prefix would wrongly reject valid 2020-12
				// schemas. Prefix evaluation is outside this check's scope.
				result = combineSchemaResults(result, schemaUnknown)
			} else {
				for _, item := range array {
					if !budget.visit() {
						return combineSchemaResults(result, schemaUnknown)
					}
					result = combineSchemaResults(result, schemaCheckWithin(item, itemSchema, depth+1, budget))
					if result == schemaMismatch {
						return result
					}
				}
			}
		}
	}
	if rawEnum, present := schema["enum"]; present {
		options, valid := rawEnum.([]any)
		if !valid || len(options) == 0 {
			return combineSchemaResults(result, schemaUnknown)
		}
		enumResult := schemaMismatch
		for index, option := range options {
			comparison := equivalentJSONValuesWithin(option, value, depth+1, budget)
			if comparison == schemaCompatible {
				enumResult = schemaCompatible
				break
			}
			if comparison == schemaUnknown {
				enumResult = schemaUnknown
			}
			if budget.visits <= 0 {
				if index+1 < len(options) {
					enumResult = schemaUnknown
				}
				break
			}
		}
		result = combineSchemaResults(result, enumResult)
	}
	return result
}

func schemaTypeCompatibility(value, declared any, budget *schemaBudget) schemaCompatibility {
	if options, ok := declared.([]any); ok {
		if len(options) == 0 {
			return schemaUnknown
		}
		result := schemaMismatch
		matched := false
		for _, option := range options {
			if !budget.visit() {
				return schemaUnknown
			}
			// Arrays are unions of type NAMES, not recursively nested unions.
			name, ok := option.(string)
			if !ok {
				result = schemaUnknown
				continue
			}
			check := schemaTypeCompatibility(value, name, budget)
			if check == schemaCompatible {
				matched = true
			}
			if check == schemaUnknown {
				result = schemaUnknown
			}
		}
		if matched && result != schemaUnknown {
			return schemaCompatible
		}
		return result
	}
	name, ok := declared.(string)
	if !ok {
		return schemaUnknown
	}
	matched := false
	switch name {
	case "object":
		_, matched = value.(map[string]any)
	case "array":
		_, matched = value.([]any)
	case "string":
		_, matched = value.(string)
	case "boolean":
		_, matched = value.(bool)
	case "null":
		matched = value == nil
	case "number", "integer":
		if !isJSONNumberType(value) {
			return schemaMismatch
		}
		if !budget.text(len(fmt.Sprint(value))) {
			return schemaUnknown
		}
		canonical, valid := canonicalEnumNumber(value)
		if !valid {
			return schemaUnknown
		}
		matched = true
		if name == "integer" && canonical != "0" {
			matched = !strings.Contains(canonical, "e-")
		}
	default:
		return schemaUnknown
	}
	if matched {
		return schemaCompatible
	}
	return schemaMismatch
}

func schemaObjectCompatibility(object, schema map[string]any, depth int, budget *schemaBudget) schemaCompatibility {
	result := schemaCompatible
	if rawRequired, present := schema["required"]; present {
		required, ok := rawRequired.([]any)
		if !ok {
			result = schemaUnknown
		}
		for _, rawName := range required {
			if !budget.visit() {
				return combineSchemaResults(result, schemaUnknown)
			}
			name, valid := rawName.(string)
			if !valid || !budget.text(len(name)) {
				result = schemaUnknown
				continue
			}
			if _, exists := object[name]; !exists {
				return schemaMismatch
			}
		}
	}
	properties, propertyPresent := schema["properties"]
	declared, propertiesValid := properties.(map[string]any)
	patterns, patternPresent := schema["patternProperties"]
	patternSchemas, patternsValid := patterns.(map[string]any)
	invalidCatalog := (propertyPresent && !propertiesValid) || (patternPresent && !patternsValid)
	if invalidCatalog {
		result = schemaUnknown
	}
	// Names and patterns are visited in sorted order. The budget makes the
	// verdict depend on the order of the walk once it runs out; in map order
	// one call could get two verdicts (review: E3 determinism).
	patternNames := sortedSchemaKeys(patternSchemas)
	for _, name := range sortedSchemaKeys(object) {
		value := object[name]
		if !budget.visit() || !budget.text(len(name)) {
			return combineSchemaResults(result, schemaUnknown)
		}
		covered, uncertainCoverage := false, invalidCatalog
		if child, present := declared[name]; present {
			covered = true
			result = combineSchemaResults(result, schemaCheckWithin(value, child, depth+1, budget))
			if result == schemaMismatch {
				return result
			}
		}
		for _, pattern := range patternNames {
			child := patternSchemas[pattern]
			matches, known := schemaPatternMatch(pattern, name, budget)
			if !known {
				uncertainCoverage = true
				result = combineSchemaResults(result, schemaUnknown)
			}
			if known && matches {
				covered = true
				result = combineSchemaResults(result, schemaCheckWithin(value, child, depth+1, budget))
				if result == schemaMismatch {
					return result
				}
			}
			if budget.visits <= 0 {
				return combineSchemaResults(result, schemaUnknown)
			}
		}
		if !covered && !uncertainCoverage {
			if additional, present := schema["additionalProperties"]; present {
				result = combineSchemaResults(result, schemaCheckWithin(value, additional, depth+1, budget))
				if result == schemaMismatch {
					return result
				}
			}
		}
	}
	return result
}

// sortedSchemaKeys is the deterministic visiting order of an object's names.
func sortedSchemaKeys(object map[string]any) []string {
	if len(object) == 0 {
		return nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// RE2 is not ECMA-262. We use it only for the shared flagless syntax on
// printable ASCII property names. Other names/patterns, lookaround, backrefs,
// Unicode/engine-specific escapes and counted repetitions are unknown. This
// avoids incorrect extra-property rejections and bounds compilation as well
// as matching. A cache and aggregate pattern*input work budget bound the
// otherwise quadratic patternProperties-by-instance-properties cross product.
func schemaPatternMatch(pattern, name string, budget *schemaBudget) (bool, bool) {
	if !budget.visit() || len(pattern) > schemaMaxPatternBytes {
		return false, false
	}
	if len(name)+1 > budget.regexWork/(len(pattern)+1) {
		return false, false
	}
	budget.regexWork -= (len(name) + 1) * (len(pattern) + 1)
	if !printableASCII(name) {
		return false, false
	}
	compiled, cached := budget.patterns[pattern]
	if !cached {
		if !budget.text(len(pattern)) || !portableSchemaPattern(pattern) {
			budget.patterns[pattern] = nil
			return false, false
		}
		var err error
		compiled, err = regexp.Compile(pattern)
		if err != nil {
			compiled = nil
		}
		budget.patterns[pattern] = compiled
	}
	if compiled == nil {
		return false, false
	}
	return compiled.MatchString(name), true
}

func printableASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < 0x20 || text[i] > 0x7e {
			return false
		}
	}
	return true
}

func portableSchemaPattern(pattern string) bool {
	if !printableASCII(pattern) {
		return false
	}
	inClass := false
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
			if i >= len(pattern) || !strings.ContainsRune(`dDwWsS\^$.|?*+()[]{}-/`, rune(pattern[i])) {
				return false
			}
		case '[':
			if inClass {
				return false
			}
			inClass = true
		case ']':
			if !inClass {
				return false
			}
			inClass = false
		case '{', '}':
			if !inClass {
				return false
			}
		case '(':
			if !inClass && i+1 < len(pattern) && pattern[i+1] == '?' && (i+2 >= len(pattern) || pattern[i+2] != ':') {
				return false
			}
		}
	}
	return !inClass
}
