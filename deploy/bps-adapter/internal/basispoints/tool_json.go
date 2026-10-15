package basispoints

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const toolJSONMaxBytes = 16 << 20

// Function arguments are compared as exact JSON values, never as floats or
// serialized text. Custom tool input is intentionally outside this function.
// The bool wrapper does not claim equality when the bounded comparison cannot
// establish it. Stream callers use the tri-state form to distinguish unknown
// from an actual change.
func equivalentToolArguments(left, right string) bool {
	return toolArgumentsCompatibility(left, right) == schemaCompatible
}

func toolArgumentsCompatibility(left, right string) schemaCompatibility {
	if left == right {
		// Preserve h22's success for identical large arguments, but do not
		// accept identical invalid/trailing JSON. This scan performs no
		// numeric conversion or recursive object allocation and has no new
		// argument-size ceiling beyond the enclosing request's existing cap.
		if json.Valid([]byte(left)) {
			return schemaCompatible
		}
		return schemaMismatch
	}
	if len(left) > toolJSONMaxBytes || len(right) > toolJSONMaxBytes {
		return schemaUnknown
	}
	a, ok := decodeOneToolJSON(left)
	if !ok {
		return schemaMismatch
	}
	b, ok := decodeOneToolJSON(right)
	if !ok {
		return schemaMismatch
	}
	budget := newSchemaBudget()
	// These values were decoded from bounded, acyclic JSON. Their node count
	// cannot exceed the input byte count, and encoding/json limits nesting to
	// 10,000. Unlike schema graphs, deep legitimate arguments need not be cut
	// off at the much smaller schema recursion limit.
	budget.visits = len(left) + len(right) + 1
	budget.textBytes = 2 * toolJSONMaxBytes
	budget.maxDepth = 10000
	return equivalentJSONValuesWithin(a, b, 0, budget)
}

func decodeOneToolJSON(text string) (any, bool) {
	if len(text) > toolJSONMaxBytes {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var result, trailing any
	if decoder.Decode(&result) != nil || decoder.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return result, true
}

func enumValuesEqual(left, right any) bool {
	return equivalentJSONValuesWithin(left, right, 0, newSchemaBudget()) == schemaCompatible
}

// Reuse the tri-state budget for schema enums: exhaustion must mean unknown,
// not "the enum definitely excludes this otherwise legal value".
func equivalentJSONValuesWithin(left, right any, depth int, budget *schemaBudget) schemaCompatibility {
	if depth > budget.maxDepth || !budget.visit() {
		return schemaUnknown
	}
	if isJSONNumberType(left) || isJSONNumberType(right) {
		if !isJSONNumberType(left) || !isJSONNumberType(right) {
			return schemaMismatch
		}
		if !budget.text(len(fmt.Sprint(left)) + len(fmt.Sprint(right))) {
			return schemaUnknown
		}
		a, aOK := canonicalEnumNumber(left)
		b, bOK := canonicalEnumNumber(right)
		if !aOK || !bOK {
			return schemaUnknown
		}
		if a == b {
			return schemaCompatible
		}
		return schemaMismatch
	}
	switch a := left.(type) {
	case nil:
		if right == nil {
			return schemaCompatible
		}
	case bool:
		if b, ok := right.(bool); ok && a == b {
			return schemaCompatible
		}
	case string:
		b, ok := right.(string)
		if !ok || len(a) != len(b) {
			return schemaMismatch
		}
		if !budget.text(len(a) + len(b)) {
			return schemaUnknown
		}
		if a == b {
			return schemaCompatible
		}
	case []any:
		b, ok := right.([]any)
		if !ok || len(a) != len(b) {
			return schemaMismatch
		}
		result := schemaCompatible
		for i := range a {
			result = combineSchemaResults(result, equivalentJSONValuesWithin(a[i], b[i], depth+1, budget))
			if result == schemaMismatch {
				return result
			}
			if budget.visits <= 0 && i+1 < len(a) {
				return combineSchemaResults(result, schemaUnknown)
			}
		}
		return result
	case map[string]any:
		b, ok := right.(map[string]any)
		if !ok || len(a) != len(b) {
			return schemaMismatch
		}
		result := schemaCompatible
		visited := 0
		// Sorted, so an exhausted budget cuts the walk at the same place
		// every time (h25 review item 1).
		for _, key := range sortedSchemaKeys(a) {
			value := a[key]
			if !budget.text(len(key)) {
				return combineSchemaResults(result, schemaUnknown)
			}
			other, present := b[key]
			if !present {
				return schemaMismatch
			}
			result = combineSchemaResults(result, equivalentJSONValuesWithin(value, other, depth+1, budget))
			visited++
			if result == schemaMismatch {
				return result
			}
			if budget.visits <= 0 && visited < len(a) {
				return combineSchemaResults(result, schemaUnknown)
			}
		}
		return result
	default:
		return schemaUnknown // Not a value in the JSON data model.
	}
	return schemaMismatch
}

func isJSONNumberType(value any) bool {
	switch value.(type) {
	case json.Number, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	}
	return false
}

// Compare decimal numbers without float64 or powers of ten. Canonical form is
// a nonzero coefficient with trailing zeroes removed and a decimal exponent.
// Exponents use linear decimal-string arithmetic, so neither huge exponent
// values nor huge exponent spellings allocate powers of ten or big integers.
func canonicalEnumNumber(value any) (string, bool) {
	if !isJSONNumberType(value) {
		return "", false
	}
	text := fmt.Sprint(value)
	if len(text) == 0 || len(text) > toolJSONMaxBytes {
		return "", false
	}
	// Number values normally came from UseNumber. Validate native/test-created
	// json.Number too; accepting "", NaN or malformed exponents would invent a
	// numeric equivalence that the JSON data model does not have.
	if !json.Valid([]byte(text)) {
		return "", false
	}
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	exponent := "0"
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent = text[index+1:]
		text = text[:index]
	}
	fractionDigits := 0
	if index := strings.IndexByte(text, '.'); index >= 0 {
		fractionDigits = len(text) - index - 1
		text = text[:index] + text[index+1:]
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return "", false
		}
	}
	text = strings.TrimLeft(text, "0")
	if text == "" {
		return "0", true
	}
	trimmed := strings.TrimRight(text, "0")
	exponent = addDecimalExponent(exponent, len(text)-len(trimmed)-fractionDigits)
	return sign + trimmed + "e" + exponent, true
}

func addDecimalExponent(exponent string, offset int) string {
	negative := strings.HasPrefix(exponent, "-")
	exponent = strings.TrimLeft(strings.TrimLeft(exponent, "+-"), "0")
	if exponent == "" {
		return strconv.Itoa(offset)
	}
	if offset == 0 {
		if negative {
			return "-" + exponent
		}
		return exponent
	}
	offsetNegative := offset < 0
	if offsetNegative {
		offset = -offset
	}
	other := strconv.Itoa(offset)
	if negative == offsetNegative {
		result := addDecimalMagnitudes(exponent, other)
		if negative {
			return "-" + result
		}
		return result
	}
	if len(exponent) == len(other) && exponent == other {
		return "0"
	}
	if len(exponent) < len(other) || (len(exponent) == len(other) && exponent < other) {
		exponent, other = other, exponent
		negative = offsetNegative
	}
	result := subtractDecimalMagnitudes(exponent, other)
	if negative {
		return "-" + result
	}
	return result
}

func addDecimalMagnitudes(a, b string) string {
	size := len(a)
	if len(b) > size {
		size = len(b)
	}
	out := make([]byte, size+1)
	carry := 0
	for i, j, k := len(a)-1, len(b)-1, size; k >= 0; i, j, k = i-1, j-1, k-1 {
		digit := carry
		if i >= 0 {
			digit += int(a[i] - '0')
		}
		if j >= 0 {
			digit += int(b[j] - '0')
		}
		out[k], carry = byte(digit%10)+'0', digit/10
	}
	return strings.TrimLeft(string(out), "0")
}

// a and b are positive decimal magnitudes with a > b.
func subtractDecimalMagnitudes(a, b string) string {
	out := make([]byte, len(a))
	borrow := 0
	for i, j := len(a)-1, len(b)-1; i >= 0; i, j = i-1, j-1 {
		digit := int(a[i]-'0') - borrow
		if j >= 0 {
			digit -= int(b[j] - '0')
		}
		borrow = 0
		if digit < 0 {
			digit += 10
			borrow = 1
		}
		out[i] = byte(digit) + '0'
	}
	return strings.TrimLeft(string(out), "0")
}
