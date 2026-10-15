package basispoints

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// This file keeps h22's own decisions available byte for byte, so the default
// configuration can be held to them (H25-SPEC I1/I3):
//
//   - h22SchemaMatches is h22's bounded schema check, unchanged except that
//     an object's names are visited in sorted order. The rc2 checker in
//     schema.go fixed several of its false rejections, but it also rejects
//     inputs h22 accepted (integer 1.5, a schema without "type" that lists
//     required fields, additionalProperties given as a schema, enum "1"
//     against 1, boolean property schemas). A relay call must never fail where
//     h22 delivered it, so relay decoding accepts when h22's check accepts, or
//     when rc2's check proves the call compatible (schemaCompatible). rc2's
//     "unknown" -- an exhausted budget, a $ref, an unsupported keyword -- is
//     no acceptance (h25 review item 1: in map order it made one call decode
//     two ways, and it delivered "2" for an integer).
//
//     The sorted order: h22's check gives up as "accepted" once its 20000
//     visits are spent, so its verdict depends on the walk's order exactly
//     when the budget can run out before a contradiction is reached. Where
//     no order runs it out, every order gives the same verdict and sorting
//     changes nothing. Past it, h22 itself answered at random (measured on
//     h22-live: an integer array of 25000 elements next to "target":"2" was
//     accepted 174 and rejected 26 times in 200 decodes); h25 answers what
//     the same algorithm answers in sorted order, every time. Spending the
//     budget takes 20000 visits, one to three per value node (a type union
//     costs more), so at least ~6700 nodes of two or more bytes each: only a
//     very large array does it; production arguments average 1.85 KB.
//   - h22RelayCatalogText is h22's natural-language tool directory (E1 can
//     leave lines out of it; see encrypted_tools.go).
//
// Nothing here is called on a path that h22 did not have.

// relaySchemaAccepts is the acceptance rule for relay function arguments:
// h22's verdict, widened only where rc2's checker proves one of h22's false
// rejections wrong. Never by rc2's "unknown".
func relaySchemaAccepts(value any, schema map[string]any) bool {
	return h22SchemaMatches(value, schema) || rc2SchemaProves(value, schema)
}

// rc2SchemaProves: rc2's checker positively established compatibility.
func rc2SchemaProves(value any, schema map[string]any) bool {
	return checkSchemaCompatibility(value, schema) == schemaCompatible
}

func h22SchemaMatches(value any, schema map[string]any) bool {
	budget := schemaMaxVisits
	return h22SchemaMatchesWithin(value, schema, 0, &budget)
}

func h22SchemaMatchesWithin(value any, schema map[string]any, depth int, budget *int) bool {
	if len(schema) == 0 {
		return true
	}
	if depth > schemaMaxDepth || *budget <= 0 {
		return true
	}
	*budget--
	if alternatives, ok := schema["type"].([]any); ok {
		for _, alternative := range alternatives {
			copy := make(map[string]any, len(schema))
			for key, nested := range schema {
				copy[key] = nested
			}
			copy["type"] = alternative
			if h22SchemaMatchesWithin(value, copy, depth+1, budget) {
				return true
			}
			if *budget <= 0 {
				return true
			}
		}
		return false
	}
	switch stringValue(schema["type"]) {
	case "object":
		object := objectValue(value)
		if object == nil {
			return false
		}
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				if _, exists := object[stringValue(name)]; !exists {
					return false
				}
			}
		}
		properties := objectValue(schema["properties"])
		if properties == nil {
			// h22 walked the names and skipped every one.
			break
		}
		for _, key := range sortedSchemaKeys(object) {
			nested := object[key]
			nestedSchema := objectValue(properties[key])
			if nestedSchema == nil {
				if schema["additionalProperties"] == false {
					return false
				}
				continue
			}
			if !h22SchemaMatchesWithin(nested, nestedSchema, depth+1, budget) {
				return false
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		if itemSchema := objectValue(schema["items"]); itemSchema != nil {
			for _, item := range items {
				if !h22SchemaMatchesWithin(item, itemSchema, depth+1, budget) {
					return false
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return false
		}
	case "integer", "number":
		switch value.(type) {
		case json.Number, float64, int, int64:
		default:
			return false
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	case "null":
		if value != nil {
			return false
		}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		matched := false
		for _, option := range enum {
			if h22EnumValuesEqual(option, value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func h22EnumValuesEqual(left, right any) bool {
	a, aNumber := h22CanonicalEnumNumber(left)
	b, bNumber := h22CanonicalEnumNumber(right)
	if aNumber && bNumber {
		return a == b
	}
	return fmt.Sprint(left) == fmt.Sprint(right)
}

func h22CanonicalEnumNumber(value any) (string, bool) {
	switch value.(type) {
	case json.Number, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
	default:
		return "", false
	}
	text := fmt.Sprint(value)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	exponent := new(big.Int)
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		if _, ok := exponent.SetString(text[index+1:], 10); !ok {
			return "", false
		}
		text = text[:index]
	}
	if index := strings.IndexByte(text, '.'); index >= 0 {
		exponent.Sub(exponent, big.NewInt(int64(len(text)-index-1)))
		text = text[:index] + text[index+1:]
	}
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}
	text = strings.TrimLeft(text, "0")
	if text == "" {
		return "0", true
	}
	trimmed := strings.TrimRight(text, "0")
	exponent.Add(exponent, big.NewInt(int64(len(text)-len(trimmed))))
	return sign + trimmed + "e" + exponent.String(), true
}

// h22RelayCatalogText is h22's directory: every top-level function/custom tool
// in declaration order, one line each, parameter NAMES only. hidden (E1, see
// encrypted_tools.go) leaves out the lines of those keys; nil is h22's text.
func h22RelayCatalogText(source map[string]any, hidden map[string]bool) string {
	catalog := []string{}
	iterToolValues(source["tools"], "", func(spec toolSpec) {
		if hidden[spec.Key] {
			return
		}
		line := "- " + spec.Key + " (" + spec.Type + ")"
		if description := stringValue(spec.Spec["description"]); description != "" {
			line += ": " + description
		}
		if spec.Type == "function" {
			if parameters := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema"); parameters != nil {
				line += ". Its arguments are an object with " + describeParameterNames(parameters) + "."
			}
		} else {
			line += ". It receives raw text in input."
		}
		catalog = append(catalog, line)
	})
	return strings.Join(catalog, "\n")
}
