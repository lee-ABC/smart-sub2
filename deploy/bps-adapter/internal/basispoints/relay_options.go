package basispoints

import (
	"net/http"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// h25 relay switches (H25-SPEC section 2).
//
// Every default reproduces h22. Three rules keep a switch from ever taking the
// provider down or changing behaviour behind the operator's back:
//
//  1. Deleting a line restores the default: configure() resets every switch
//     before config.yaml is read, so a value cannot resurrect itself from the
//     settings.json cache.
//  2. An unreadable value falls back to the default with a warning; it never
//     fails plugin.register / plugin.reconfigure. A failed registration takes
//     the plugin out of CPA's snapshot and with it every credential. The YAML
//     is read into raw nodes for exactly this reason: a typed bool field turns
//     `coerce_scalar_strings: maybe` into a YAML type error, and a YAML type
//     error is a failed registration.
//  3. Switches that change preparation or decoding are read once per request
//     and frozen onto that request (withRelayOptions). One stream decodes the
//     same transport call twice -- when the item completes and again when the
//     terminal snapshot is checked -- and a reconfigure between the two must
//     not make the second decode disagree with the first.
const (
	relayProtocolAuto = "auto"

	relayToolContractsSummary = "summary"
	relayToolContractsFull    = "full"

	relayToolDeliveryImmediate = "immediate"
	relayToolDeliveryBatch     = "batch"

	toolPolicyChecksLog     = "log"
	toolPolicyChecksEnforce = "enforce"

	historyConflictReplay = "replay"
	historyConflictReject = "reject"

	relayIncompleteError       = "error"
	relayIncompletePassthrough = "passthrough"

	relayStatusPath = "relay-status"
)

// relayOptions is the per-request snapshot of every h25 switch.
type relayOptions struct {
	Protocol            string
	ToolContracts       string
	ToolDelivery        string
	PolicyChecks        string
	HistoryConflict     string
	Incomplete          string
	HideEncryptedTools  bool
	CoerceScalarStrings bool
	RetryableErrorHint  bool
	SchemaShadow        bool
	// Native promotion (A1, a1_promotion.go): off | shadow | on, the history
	// rendering (h1 | h2) and the canonical block list.
	NativePromotion        string
	NativePromotionHistory string
	NativePromotionBlock   string
	// NoCatalogTransport: continue | error (no_catalog_transport.go).
	NoCatalogTransport string
}

func defaultRelayOptions() relayOptions {
	return relayOptionsFromConfig(defaultConfig())
}

func relayOptionsFromConfig(cfg Config) relayOptions {
	return relayOptions{
		Protocol:            cfg.RelayProtocol,
		ToolContracts:       cfg.RelayToolContracts,
		ToolDelivery:        cfg.RelayToolDelivery,
		PolicyChecks:        cfg.ToolPolicyChecks,
		HistoryConflict:     cfg.HistoryConflict,
		Incomplete:          cfg.RelayIncomplete,
		HideEncryptedTools:  cfg.RelayHideEncryptedTools,
		CoerceScalarStrings: cfg.CoerceScalarStrings,
		RetryableErrorHint:  cfg.RetryableErrorHint,
		SchemaShadow:        cfg.SchemaShadow,

		NativePromotion:        cfg.RelayNativePromotion,
		NativePromotionHistory: cfg.RelayNativePromotionHistory,
		NativePromotionBlock:   cfg.RelayNativePromotionBlock,
		NoCatalogTransport:     cfg.RelayNoCatalogTransport,
	}
}

func (o relayOptions) describe() map[string]any {
	return map[string]any{
		"relay_protocol":             o.Protocol,
		"relay_tool_contracts":       o.ToolContracts,
		"relay_tool_delivery":        o.ToolDelivery,
		"tool_policy_checks":         o.PolicyChecks,
		"history_conflict":           o.HistoryConflict,
		"relay_incomplete":           o.Incomplete,
		"relay_hide_encrypted_tools": o.HideEncryptedTools,
		"coerce_scalar_strings":      o.CoerceScalarStrings,
		"retryable_error_hint":       o.RetryableErrorHint,
		"schema_shadow":              o.SchemaShadow,

		"relay_native_promotion":         o.NativePromotion,
		"relay_native_promotion_history": o.NativePromotionHistory,
		"relay_native_promotion_block":   o.NativePromotionBlock,
		"relay_no_catalog_transport":     o.NoCatalogTransport,
	}
}

// The snapshot rides on the request's local source map under a key a JSON
// body cannot produce with this type, and serialises as null if a source map
// is ever marshalled.
const relayOptionsKey = "\x00cpa_relay_options"

type frozenRelayOptions struct{ options relayOptions }

func (*frozenRelayOptions) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

func withRelayOptions(source map[string]any, options relayOptions) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	result[relayOptionsKey] = &frozenRelayOptions{options: options}
	return result
}

// relayOptionsFrom returns the request's frozen switches, or the defaults (=
// h22 behaviour) for a source that never went through request preparation.
func relayOptionsFrom(source map[string]any) relayOptions {
	if frozen, ok := source[relayOptionsKey].(*frozenRelayOptions); ok && frozen != nil {
		return frozen.options
	}
	return defaultRelayOptions()
}

func (s *Service) relayOptionsFor(request ExecutorRequest) relayOptions {
	if request.relayOptions != nil {
		return *request.relayOptions
	}
	return relayOptionsFromConfig(s.config())
}

// relaySwitchNodes receives the switches as raw YAML nodes; see rule 2 above.
type relaySwitchNodes struct {
	RelayProtocol           yaml.Node `yaml:"relay_protocol"`
	RelayToolContracts      yaml.Node `yaml:"relay_tool_contracts"`
	RelayToolDelivery       yaml.Node `yaml:"relay_tool_delivery"`
	ToolPolicyChecks        yaml.Node `yaml:"tool_policy_checks"`
	HistoryConflict         yaml.Node `yaml:"history_conflict"`
	RelayIncomplete         yaml.Node `yaml:"relay_incomplete"`
	RelayHideEncryptedTools yaml.Node `yaml:"relay_hide_encrypted_tools"`
	CoerceScalarStrings     yaml.Node `yaml:"coerce_scalar_strings"`
	RetryableErrorHint      yaml.Node `yaml:"retryable_error_hint"`
	SchemaShadow            yaml.Node `yaml:"schema_shadow"`

	RelayNativePromotion        yaml.Node `yaml:"relay_native_promotion"`
	RelayNativePromotionHistory yaml.Node `yaml:"relay_native_promotion_history"`
	RelayNativePromotionBlock   yaml.Node `yaml:"relay_native_promotion_block"`
	RelayNoCatalogTransport     yaml.Node `yaml:"relay_no_catalog_transport"`
}

// resetRelaySwitches puts every switch back to its default. configure() calls
// it before reading config.yaml, so a deleted line means "default".
func (c *Config) resetRelaySwitches() {
	defaults := defaultConfig()
	c.RelayProtocol = defaults.RelayProtocol
	c.RelayToolContracts = defaults.RelayToolContracts
	c.RelayToolDelivery = defaults.RelayToolDelivery
	c.ToolPolicyChecks = defaults.ToolPolicyChecks
	c.HistoryConflict = defaults.HistoryConflict
	c.RelayIncomplete = defaults.RelayIncomplete
	c.RelayHideEncryptedTools = defaults.RelayHideEncryptedTools
	c.CoerceScalarStrings = defaults.CoerceScalarStrings
	c.RetryableErrorHint = defaults.RetryableErrorHint
	c.SchemaShadow = defaults.SchemaShadow
	c.RelayNativePromotion = defaults.RelayNativePromotion
	c.RelayNativePromotionHistory = defaults.RelayNativePromotionHistory
	c.RelayNativePromotionBlock = defaults.RelayNativePromotionBlock
	c.RelayNoCatalogTransport = defaults.RelayNoCatalogTransport
}

// applyRelaySwitchYAML reads the switches from config.yaml. It never fails: a
// value that cannot be read leaves the default in place and returns a warning.
// An absent key, or a key with no value, keeps the default silently.
func applyRelaySwitchYAML(raw []byte, c *Config) []string {
	var nodes relaySwitchNodes
	if err := yaml.Unmarshal(raw, &nodes); err != nil {
		return []string{"relay switches could not be read from config.yaml; all relay switches keep their defaults"}
	}
	var warnings []string
	present := func(node *yaml.Node) bool {
		if node.Kind == 0 {
			return false
		}
		return !(node.Kind == yaml.ScalarNode && node.Tag == "!!null")
	}
	text := func(key string, node *yaml.Node, target *string) {
		if !present(node) {
			return
		}
		var value string
		if node.Kind != yaml.ScalarNode || node.Decode(&value) != nil {
			warnings = append(warnings, key+": value is not a single word; using the default")
			return
		}
		*target = value
	}
	flag := func(key string, node *yaml.Node, target *bool) {
		if !present(node) {
			return
		}
		value, ok := yamlCoreBool(node)
		if !ok {
			warnings = append(warnings, key+": value is not true or false; using the default")
			return
		}
		*target = value
	}
	text("relay_protocol", &nodes.RelayProtocol, &c.RelayProtocol)
	text("relay_tool_contracts", &nodes.RelayToolContracts, &c.RelayToolContracts)
	text("relay_tool_delivery", &nodes.RelayToolDelivery, &c.RelayToolDelivery)
	text("tool_policy_checks", &nodes.ToolPolicyChecks, &c.ToolPolicyChecks)
	text("history_conflict", &nodes.HistoryConflict, &c.HistoryConflict)
	text("relay_incomplete", &nodes.RelayIncomplete, &c.RelayIncomplete)
	flag("relay_hide_encrypted_tools", &nodes.RelayHideEncryptedTools, &c.RelayHideEncryptedTools)
	flag("coerce_scalar_strings", &nodes.CoerceScalarStrings, &c.CoerceScalarStrings)
	flag("retryable_error_hint", &nodes.RetryableErrorHint, &c.RetryableErrorHint)
	flag("schema_shadow", &nodes.SchemaShadow, &c.SchemaShadow)
	text("relay_native_promotion", &nodes.RelayNativePromotion, &c.RelayNativePromotion)
	text("relay_native_promotion_history", &nodes.RelayNativePromotionHistory, &c.RelayNativePromotionHistory)
	text("relay_no_catalog_transport", &nodes.RelayNoCatalogTransport, &c.RelayNoCatalogTransport)
	// The block list is one comma-separated word list, or a YAML list of
	// words; an empty list blocks nothing. normalizeRelaySwitches validates it.
	if node := &nodes.RelayNativePromotionBlock; present(node) {
		switch node.Kind {
		case yaml.SequenceNode:
			words := make([]string, 0, len(node.Content))
			valid := true
			for _, element := range node.Content {
				var word string
				if element.Kind != yaml.ScalarNode || element.Decode(&word) != nil {
					valid = false
					break
				}
				words = append(words, word)
			}
			switch {
			case !valid:
				warnings = append(warnings, "relay_native_promotion_block: value is not a list of words; using the default")
			case len(words) == 0:
				c.RelayNativePromotionBlock = a1BlockNone
			default:
				c.RelayNativePromotionBlock = strings.Join(words, ",")
			}
		default:
			text("relay_native_promotion_block", node, &c.RelayNativePromotionBlock)
		}
	}
	return warnings
}

// yamlCoreBool reads a boolean switch. Only a YAML 1.2 core-schema boolean is
// one: an unquoted true/True/TRUE or false/False/FALSE, which the parser tags
// !!bool. node.Decode into a bool would also take YAML 1.1's yes/no/on/off/y/n,
// quoted or not, so `coerce_scalar_strings: "no"` would silently switch E3 off
// (review F2/F3); those, and 1/0 or any other spelling, now fall back to the
// default with a warning instead.
func yamlCoreBool(node *yaml.Node) (bool, bool) {
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!bool" {
		return false, false
	}
	switch node.Value {
	case "true", "True", "TRUE":
		return true, true
	case "false", "False", "FALSE":
		return false, true
	}
	return false, false
}

// normalizeRelaySwitches canonicalises the word-valued switches. An empty
// value means the default; an unknown value falls back to the default with a
// warning instead of failing configuration.
func (c *Config) normalizeRelaySwitches() []string {
	defaults := defaultConfig()
	var warnings []string
	choose := func(key string, target *string, fallback string, allowed ...string) {
		value := strings.ToLower(strings.TrimSpace(*target))
		if value == "" {
			*target = fallback
			return
		}
		for _, candidate := range allowed {
			if value == candidate {
				*target = value
				return
			}
		}
		shown := value
		if len(shown) > 32 {
			shown = shown[:32] + "..."
		}
		warnings = append(warnings, key+": unsupported value "+quoteForWarning(shown)+"; using "+fallback)
		*target = fallback
	}
	choose("relay_protocol", &c.RelayProtocol, defaults.RelayProtocol, relayProtocolLegacy, relayProtocolAuto, relayProtocolReferences)
	choose("relay_tool_contracts", &c.RelayToolContracts, defaults.RelayToolContracts, relayToolContractsSummary, relayToolContractsFull)
	choose("relay_tool_delivery", &c.RelayToolDelivery, defaults.RelayToolDelivery, relayToolDeliveryImmediate, relayToolDeliveryBatch)
	choose("tool_policy_checks", &c.ToolPolicyChecks, defaults.ToolPolicyChecks, toolPolicyChecksLog, toolPolicyChecksEnforce)
	choose("history_conflict", &c.HistoryConflict, defaults.HistoryConflict, historyConflictReplay, historyConflictReject)
	choose("relay_incomplete", &c.RelayIncomplete, defaults.RelayIncomplete, relayIncompleteError, relayIncompletePassthrough)
	choose("relay_native_promotion", &c.RelayNativePromotion, defaults.RelayNativePromotion, a1ModeOff, a1ModeShadow, a1ModeOn)
	choose("relay_native_promotion_history", &c.RelayNativePromotionHistory, defaults.RelayNativePromotionHistory, a1HistoryH1, a1HistoryH2)
	choose("relay_no_catalog_transport", &c.RelayNoCatalogTransport, defaults.RelayNoCatalogTransport, noCatalogContinue, noCatalogError)
	if canonical, ok := normalizeA1Block(c.RelayNativePromotionBlock); ok {
		c.RelayNativePromotionBlock = canonical
	} else {
		shown := strings.ToLower(strings.TrimSpace(c.RelayNativePromotionBlock))
		if len(shown) > 32 {
			shown = shown[:32] + "..."
		}
		warnings = append(warnings, "relay_native_promotion_block: unsupported value "+quoteForWarning(shown)+"; using "+defaults.RelayNativePromotionBlock)
		c.RelayNativePromotionBlock = defaults.RelayNativePromotionBlock
	}
	return warnings
}

func quoteForWarning(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		if r < 0x20 || r == '"' || r == '\\' || r == 0x7f {
			out.WriteByte('?')
			continue
		}
		out.WriteRune(r)
	}
	out.WriteByte('"')
	return out.String()
}

// Configuration warnings are logged on the next request rather than inside
// plugin.register: a host callback made while the host is registering the
// plugin is a re-entrant call whose safety has never been verified on fc7,
// whereas host.log from inside a request is the path rc2 already exercises.
func (s *Service) flushConfigWarnings(callbackID string) {
	s.mu.Lock()
	if !s.configWarningsPending {
		s.mu.Unlock()
		return
	}
	s.configWarningsPending = false
	warnings := append([]string(nil), s.configWarnings...)
	s.mu.Unlock()
	defer func() { _ = recover() }()
	fields := map[string]any{
		"event": "relay_config_fallback", "plugin_version": Version,
		"warnings": warnings,
	}
	_ = s.call("host.log", map[string]any{
		"host_callback_id": callbackID, "level": "warn",
		"message": "basispoints configuration fallback " + string(jsonBytes(fields)),
		"fields":  fields,
	}, nil)
}

// Process-wide event counters for the read-only status route. Only fixed,
// code-owned names are counted; no request content ever becomes a key.
var relayCounters = struct {
	sync.Mutex
	values map[string]uint64
}{values: map[string]uint64{}}

func countRelayEvent(name string) {
	relayCounters.Lock()
	relayCounters.values[name]++
	relayCounters.Unlock()
}

func relayCounterSnapshot() map[string]uint64 {
	relayCounters.Lock()
	defer relayCounters.Unlock()
	snapshot := make(map[string]uint64, len(relayCounters.values))
	for key, value := range relayCounters.values {
		snapshot[key] = value
	}
	return snapshot
}

// serveRelayStatus reads back the switches in effect, the warnings of the last
// configuration, the counters and their summary (relay_stats.go). Read-only,
// no request content.
func (s *Service) serveRelayStatus() map[string]any {
	cfg := s.config()
	s.mu.RLock()
	warnings := append([]string{}, s.configWarnings...)
	s.mu.RUnlock()
	// encoding/json writes map keys sorted, so the document is stable.
	counters := relayCounterSnapshot()
	document := map[string]any{
		"plugin_version":  Version,
		"effective":       relayOptionsFromConfig(cfg).describe(),
		"config_warnings": warnings,
		"counters":        counters,
		// The same counters arranged for reading (relay_stats.go).
		"summary": relayStatusSummary(counters),
	}
	// Experiment self-check and capture status (bpsexp build only); nil in the
	// production build, where this key never appears.
	if status := expStatus(); status != nil {
		document["bpsexp"] = status
	}
	body := jsonBytes(document)
	return map[string]any{
		"StatusCode": http.StatusOK,
		"Headers": map[string][]string{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		"Body": body,
	}
}
