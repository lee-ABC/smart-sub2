package basispoints

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"sync"
)

// coerce_scalar_strings (H25-SPEC S1.4).
//
// About a fifth of the relay calls that failed in production failed only
// because the model wrote a number or boolean as a string -- {"limit":"20"}
// for an integer field. The repair is deliberately narrow:
//
//   - it runs only after the call has already failed the relay schema check
//     (h22's check rejects it and rc2's does not prove it compatible), so it
//     can never change a call h22 would have delivered;
//   - it only touches a string at a position whose schema declares number,
//     integer or boolean (and not also string), reached through properties,
//     additionalProperties or items -- never through $ref, combinators or
//     patternProperties;
//   - a string is converted only when it is exactly a JSON number literal
//     (integer: no fraction and no exponent, so "1.0" stays a string) or
//     exactly "true"/"false";
//   - and only when the number is one the client can hold: an integer must
//     fit in int64, a number must parse to a finite float64 ("1e400" would
//     be Infinity in JavaScript and an error in serde). Anything larger stays
//     a string and the call fails as in h22 (review F6);
//   - the converted arguments must then pass the same check (h22's check, or
//     rc2's proving them compatible -- never rc2's "unknown"), or the call
//     fails exactly as it would have without the switch;
//   - it is a pure function of the arguments and the schema (object keys are
//     visited in sorted order so even the visit budget is spent the same way
//     every time), so decoding the same call twice in one stream agrees.
//
// Custom tools are never touched: their input is raw text.
const (
	scalarCoerceMaxDepth  = 64
	scalarCoerceMaxVisits = 20000
	scalarCoerceMaxText   = 1024
)

var (
	jsonNumberLiteral  = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	jsonIntegerLiteral = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)
)

// coerceScalarStrings returns a converted copy of args and true when at least
// one string was converted. args itself is never modified.
func coerceScalarStrings(args map[string]any, schema map[string]any) (map[string]any, bool) {
	if args == nil || schema == nil {
		return nil, false
	}
	visits := scalarCoerceMaxVisits
	converted, changed := coerceScalarValue(args, schema, 0, &visits)
	object, ok := converted.(map[string]any)
	if !changed || !ok {
		return nil, false
	}
	return object, true
}

func coerceScalarValue(value any, rawSchema any, depth int, visits *int) (any, bool) {
	schema, ok := rawSchema.(map[string]any)
	if !ok || depth > scalarCoerceMaxDepth || *visits <= 0 {
		return value, false
	}
	*visits--
	if _, reference := schema["$ref"]; reference {
		return value, false
	}
	switch typed := value.(type) {
	case string:
		return coerceScalarString(typed, schema["type"])
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		additional, _ := schema["additionalProperties"].(map[string]any)
		_, patterns := schema["patternProperties"]
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var copy map[string]any
		for _, key := range keys {
			child, declared := properties[key]
			if !declared {
				if patterns || additional == nil {
					continue
				}
				child = additional
			}
			replacement, changed := coerceScalarValue(typed[key], child, depth+1, visits)
			if !changed {
				continue
			}
			if copy == nil {
				copy = make(map[string]any, len(typed))
				for k, v := range typed {
					copy[k] = v
				}
			}
			copy[key] = replacement
		}
		if copy != nil {
			return copy, true
		}
	case []any:
		items, ok := schema["items"].(map[string]any)
		if _, prefix := schema["prefixItems"]; !ok || prefix {
			return value, false
		}
		var copy []any
		for index, element := range typed {
			replacement, changed := coerceScalarValue(element, items, depth+1, visits)
			if !changed {
				continue
			}
			if copy == nil {
				copy = append([]any(nil), typed...)
			}
			copy[index] = replacement
		}
		if copy != nil {
			return copy, true
		}
	}
	return value, false
}

func coerceScalarString(text string, declared any) (any, bool) {
	var types []string
	switch typed := declared.(type) {
	case string:
		types = []string{typed}
	case []any:
		for _, option := range typed {
			if name, ok := option.(string); ok {
				types = append(types, name)
			}
		}
	}
	wants := map[string]bool{}
	for _, name := range types {
		wants[name] = true
	}
	// A string where a string is allowed is already valid; anything else
	// failing here is not this repair's business.
	if len(wants) == 0 || wants["string"] || len(text) > scalarCoerceMaxText {
		return text, false
	}
	switch {
	case wants["integer"] && jsonIntegerLiteral.MatchString(text) && fitsInt64(text):
		return json.Number(text), true
	case wants["number"] && jsonNumberLiteral.MatchString(text) && finiteFloat64(text):
		return json.Number(text), true
	case wants["boolean"] && text == "true":
		return true, true
	case wants["boolean"] && text == "false":
		return false, true
	}
	return text, false
}

// fitsInt64: an integer literal within int64.
func fitsInt64(text string) bool {
	_, err := strconv.ParseInt(text, 10, 64)
	return err == nil
}

// finiteFloat64: a number literal that parses to a finite float64. A value
// too small to represent rounds to zero, as every JSON client does; one too
// large is out of range (ParseFloat reports it and returns an infinity).
func finiteFloat64(text string) bool {
	value, err := strconv.ParseFloat(text, 64)
	return err == nil && !math.IsInf(value, 0) && !math.IsNaN(value)
}

// Decode notes record that a relay call was delivered only thanks to a repair
// (today: scalar_coerced). The service attaches a recorder to the request's
// source; the decoder itself stays free of host calls. Each call is noted
// once, however many times one stream decodes it.
const relayDecodeNotesKey = "\x00cpa_relay_decode_notes"

type relayDecodeNotes struct {
	mu   sync.Mutex
	seen map[string]bool
	log  func(reason string)
	// policy logs a tool_policy_checks finding that did not reject the
	// response (see relay_policy.go). Same dedup set and cap as log.
	policy func(reason, mode string)
	// history logs a history_conflict note (see history_policy.go). Same
	// dedup set and cap as log.
	history func(reason string)
	// observed: run_officejs calls already counted (relay_stats.go). A set of
	// its own, so counting never uses up the cap on log notes.
	observed map[string]bool
	// heartbeatStopped logs that the batch heartbeat stopped before its
	// stream ended (stream.go). At most once per stream, by construction.
	heartbeatStopped func(reason string)
}

func (*relayDecodeNotes) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

const relayDecodeNotesPerRequest = 256

func noteRelayDecode(source map[string]any, callID, reason string) {
	notes, _ := source[relayDecodeNotesKey].(*relayDecodeNotes)
	if notes == nil {
		return
	}
	// A note must never change a tool decision, even if the logger panics.
	defer func() { _ = recover() }()
	key := reason + "\x00" + callID
	notes.mu.Lock()
	if notes.seen[key] || len(notes.seen) >= relayDecodeNotesPerRequest {
		notes.mu.Unlock()
		return
	}
	notes.seen[key] = true
	notes.mu.Unlock()
	countRelayEvent(reason)
	if notes.log != nil {
		notes.log(reason)
	}
}

func (s *Service) attachRelayDecodeNotes(source map[string]any, callbackID string) {
	source[relayDecodeNotesKey] = &relayDecodeNotes{seen: map[string]bool{}, log: func(reason string) {
		// Fixed labels only: no tool names, arguments or identifiers.
		fields := map[string]any{
			"event": "relay_decode_note", "plugin_version": Version,
			"reason": reason, "relay_protocol": relayProtocolForDecode(source),
		}
		_ = s.call("host.log", map[string]any{
			"host_callback_id": callbackID, "level": "info",
			"message": "basispoints relay decode " + string(jsonBytes(fields)),
			"fields":  fields,
		}, nil)
	}, policy: func(reason, mode string) {
		// Fixed labels only, as above.
		fields := map[string]any{
			"event": "relay_tool_policy", "plugin_version": Version,
			"reason": reason, "tool_policy_checks": mode, "enforced": false,
			"relay_protocol": relayProtocolForDecode(source),
		}
		_ = s.call("host.log", map[string]any{
			"host_callback_id": callbackID, "level": "info",
			"message": "basispoints relay tool policy " + string(jsonBytes(fields)),
			"fields":  fields,
		}, nil)
	}, history: func(reason string) {
		// Fixed labels only, as above.
		scope, _ := replayScopeFromSource(source)
		fields := map[string]any{
			"event": "relay_history_note", "plugin_version": Version,
			"reason": reason, "history_conflict": relayOptionsFrom(source).HistoryConflict,
			"replay_cache_scoped": scope != "", "strict_scoped": strictReplayScopeFromSource(source) != "",
		}
		_ = s.call("host.log", map[string]any{
			"host_callback_id": callbackID, "level": "info",
			"message": "basispoints relay history " + string(jsonBytes(fields)),
			"fields":  fields,
		}, nil)
	}, heartbeatStopped: func(reason string) {
		// Fixed labels only, as above.
		// Called from the heartbeat goroutine: reads nothing from source.
		fields := map[string]any{
			"event": "relay_batch_heartbeat_stopped", "plugin_version": Version,
			"reason": reason, "relay_tool_delivery": relayToolDeliveryBatch,
		}
		_ = s.call("host.log", map[string]any{
			"host_callback_id": callbackID, "level": "warn",
			"message": "basispoints relay batch heartbeat stopped " + string(jsonBytes(fields)),
			"fields":  fields,
		}, nil)
	}}
}
