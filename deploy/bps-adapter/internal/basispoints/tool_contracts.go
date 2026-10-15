package basispoints

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// The relay cannot ask a model to follow rules it never sends. Preserve the
// complete client definition, including nested JSON Schema and custom grammar,
// as data in the developer catalogue. This is a prompt contract, not a claim
// that the BPS transport provides native strict/grammar enforcement.
const (
	relayContractMaxTools     = 4096
	relayContractMaxBytes     = 4 << 20
	relayContractMaxToolBytes = 1 << 20
	relayContractMaxNodes     = 65536
	relayContractMaxDepth     = 64
)

type relayContractText string

const relayContractTextKey = "\x00cpa_relay_contracts"

func renderRelayToolContracts(source map[string]any) (string, error) {
	if text, ok := source[relayContractTextKey].(relayContractText); ok {
		return string(text), nil
	}
	// E1: tools hidden by relay_hide_encrypted_tools get no definition.
	specs, _ := fullRelayCatalogue(source)
	if len(specs) > relayContractMaxTools {
		return "", relayContractLimitError()
	}
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	var text strings.Builder
	for _, name := range names {
		spec := specs[name]
		if len(name) > relayContractMaxToolBytes {
			return "", relayContractLimitError()
		}
		nodes, bytes := relayContractMaxNodes, relayContractMaxToolBytes
		if !withinRelayContractBudget(spec.Spec, 0, &nodes, &bytes) {
			return "", relayContractLimitError()
		}
		definition, err := marshalContractJSON(spec.Spec)
		if err != nil {
			return "", fail(400, "invalid_tool_definition", "client tool definition is not valid JSON")
		}
		if len(definition) > relayContractMaxToolBytes {
			return "", relayContractLimitError()
		}
		line := "- Full tool key: " + name + "; definition: " + string(definition) + "\n"
		if len(line) > relayContractMaxBytes-text.Len() {
			return "", relayContractLimitError()
		}
		text.WriteString(line)
	}
	return text.String(), nil
}

// marshalContractJSON writes a definition as the model should read it: "<",
// ">" and "&" stay themselves instead of becoming <-style escapes, which
// cost tokens and change what a description or grammar literally says.
func marshalContractJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// Run before constructing any upstream request. Never silently truncate the
// contract and later reject a model for violating the omitted constraints.
// An error here makes the caller fall back to summary (h22's directory) for
// the whole request; it is never returned to the client.
func withPreparedRelayToolContracts(source map[string]any) (map[string]any, error) {
	text, err := renderRelayToolContracts(source)
	if err != nil {
		return nil, err
	}
	prepared := make(map[string]any, len(source)+1)
	for key, value := range source {
		prepared[key] = value
	}
	prepared[relayContractTextKey] = relayContractText(text)
	return prepared, nil
}

func relayContractLimitError() error {
	return fail(413, "tool_catalog_too_large", "client tool definitions exceed the relay catalogue budget; reduce the active tool definitions or use deferred tool discovery")
}

func withinRelayContractBudget(value any, depth int, nodes, bytes *int) bool {
	if depth > relayContractMaxDepth || *nodes <= 0 {
		return false
	}
	*nodes--
	consume := func(n int) bool {
		if n > *bytes {
			return false
		}
		*bytes -= n
		return true
	}
	switch value := value.(type) {
	case map[string]any:
		if len(value) > *nodes {
			return false
		}
		for key, child := range value {
			if !consume(len(key)) || !withinRelayContractBudget(child, depth+1, nodes, bytes) {
				return false
			}
		}
	case []any:
		if len(value) > *nodes {
			return false
		}
		for _, child := range value {
			if !withinRelayContractBudget(child, depth+1, nodes, bytes) {
				return false
			}
		}
	case string:
		return consume(len(value))
	case json.Number:
		return consume(len(value))
	}
	return true
}
