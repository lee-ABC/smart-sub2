package basispoints

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Native promotion (A1, PLAN-real-upstream-json-fix.md).
//
// A relayed client declares its tools at the top level. The upstream never
// sees them: h22/h25 describe them in a developer message and the model calls
// them through run_officejs, writing the client tool's JSON by hand inside the
// code string -- the layer that breaks (code_json, ~80% of h25's reasons).
// A Responses-Lite client (Codex Desktop) declares the same kind of tools in
// an additional_tools item instead, and the upstream calls them natively.
//
// relay_native_promotion turns a relayed request into a Lite request before it
// is prepared: the top-level tools are rewritten the way Codex builds a Lite
// catalogue and put into an additional_tools item at input[0]; everything
// after that -- the Lite prologue, namespace memory, history replay, native
// call handling on the way back -- is the existing Lite pipeline.
//
//   off (default)  nothing happens; not even the decision is computed.
//   shadow         the decision is computed and counted; the upstream body is
//                  the relay body, byte for byte.
//   on             promotable requests go upstream promoted. A 400 before the
//                  stream opens is retried once, on the same credential, with
//                  the relay body; the client never sees the first attempt.
//
// The catalogue follows openai/codex @ea27864:
//   tools/src/tool_spec.rs:95-142  create_tools_json_for_responses_lite: flat
//     function and custom tools go into one namespace named "functions"
//     (inserted where the first of them stood), a client namespace named
//     "functions" is merged into it, every other namespace stays as it is.
//   tools/src/responses_api.rs:65-71  the functions namespace's description
//     is empty.
//   core/src/client.rs:964-998  the item is {type: additional_tools, id:
//     "at_" + uuid v5, role: developer, tools} and goes before everything
//     else in input.
//   tools/src/responses_api.rs:17-45  function = {type, name, description,
//     strict, parameters}; custom = {type, name, description, format}.
//   tools/src/mcp_tool.rs, json_schema.rs  parameters are lowered as Codex
//     lowers MCP/dynamic tool schemas (a1_schema.go).
// Hosted tool types (web_search, tool_search, image_generation, ...) are
// dropped and counted: the relay never offered them either.

const (
	a1ModeOff    = "off"
	a1ModeShadow = "shadow"
	a1ModeOn     = "on"

	a1HistoryH1 = "h1"
	a1HistoryH2 = "h2"

	// a1BlockNone: relay_native_promotion_block value that blocks nothing.
	a1BlockNone = "none"

	// relayKindPromoted labels relay_requests / relay_outcome for a request
	// that went upstream promoted (fallbacks included: intention to treat).
	relayKindPromoted = "promoted"

	// a1FunctionsNamespace is codex_protocol::DEFAULT_FUNCTION_NAMESPACE.
	a1FunctionsNamespace = "functions"

	// Catalogue bounds; past them the request stays on the relay.
	a1MaxCatalogTools = 4096
	a1MaxCatalogBytes = 4 << 20

	// The not-promotable cache: catalogues the upstream refused.
	a1DenyTTL     = 12 * time.Hour
	a1DenyEntries = 4096
)

// Schema and tool features. Fixed labels: counters and the block list.
const (
	a1FeatureRef               = "ref"         // $ref / $defs / definitions survive lowering
	a1FeatureComposition       = "composition" // anyOf / oneOf / allOf
	a1FeatureEnum              = "enum"        // enum (const becomes enum)
	a1FeatureTypeList          = "type_list"   // "type": [..]
	a1FeatureStrictTrue        = "strict_true" // function strict: true
	a1FeatureCustomFormatOther = "custom_format_other"
	a1FeatureAdditionalTrue    = "additional_properties_true"
	a1FeatureAdditionalSchema  = "additional_properties_schema"
	a1FeatureEncrypted         = "encrypted"    // "encrypted": true in a schema
	a1FeatureCompacted         = "compacted"    // a schema went through the size passes
	a1FeatureCustomTool        = "custom_tool"  // any custom (freeform) tool
	a1FeatureNamespace         = "namespace"    // a client namespace other than functions
	a1FeatureBuiltinName       = "builtin_name" // a name the upstream may own itself
	a1FeatureCompaction        = "compaction"   // the input carries a compaction item
	a1FeatureToolChoice        = "tool_choice"  // tool_choice other than auto (a1wide2)
)

// a1Features is every feature relay_native_promotion_block may name.
var a1Features = map[string]bool{
	a1FeatureRef: true, a1FeatureComposition: true, a1FeatureEnum: true, a1FeatureTypeList: true,
	a1FeatureStrictTrue: true, a1FeatureCustomFormatOther: true, a1FeatureAdditionalTrue: true,
	a1FeatureAdditionalSchema: true, a1FeatureEncrypted: true, a1FeatureCompacted: true,
	a1FeatureCustomTool: true, a1FeatureNamespace: true, a1FeatureBuiltinName: true,
	a1FeatureCompaction: true, a1FeatureToolChoice: true,
}

// a1DefaultBlock started out conservative -- every keyword that survives
// Codex's lowering but never appeared in the 907 real Lite requests, plus
// strict:true (8193/8193 Lite functions are strict:false) and custom tools
// that are not a lark grammar (896/896 Lite custom tools are).  Stage 1 (Q4,
// 2026-10-09, 53 real requests on one test account) measured the upstream
// instead of guessing, and five of those keywords came off the list: enum
// (7/7), anyOf|oneOf|allOf (7/7), $ref+$defs (3/3), an array-valued `type`
// (3/3) and additionalProperties in both forms (6/6) were all accepted AND
// answered with a real native call, as were the two realistic combinations
// (enum+anyOf 4/4, and enum+anyOf+$ref+type-array 4/4 -- the shape the
// largest production failure source needs).  That moves the promotable share
// of the 706 relay fixtures from 36.5% to 95.9%.
//
// What stays:
//   - custom_format_other: accepted, yet the model never used such a tool
//     natively (0/2), and it appears in 0 of the real fixtures.
//
// What came off on 2026-10-15 (a1wide), after a day of production in which
// the requests still on the relay failed with code_json at ~39 per thousand
// against ~0.03 per thousand promoted:
//   - strict_true: the upstream refuses strict:true before the stream opens,
//     so the promoted catalogue now always says strict:false (as all 8193
//     real Lite functions do). The relay never enforced strict either: the
//     model wrote the arguments by hand inside run_officejs.
//   - compaction: a request that carries a compaction item is a Lite request
//     like any other once promoted; real Lite clients send exactly that shape
//     all day. The 2026-09-29 tool loss after an upstream in-place compaction
//     happened at a 200k compact threshold; the plugin's threshold is 800k now.
// Either can be put back without a restart: list it in
// relay_native_promotion_block.
//
// What came off on 2026-10-15 (a1wide2): tool_choice. A forced choice
// ("required", one named tool, allowed_tools) kept a request on the relay,
// where summary mode never told the model about the choice at all -- it only
// filtered the calls on the way back -- and the model wrote its call by hand
// inside run_officejs. Promoted, the catalogue holds only the tools the choice
// allows and a forced choice is stated in one developer message at the end of
// the input (a1ToolChoiceNotice); the upstream refuses a tool_choice field.
// Listing tool_choice in relay_native_promotion_block puts it back.
const a1DefaultBlock = "custom_format_other"

// Why a relayed request was not promoted. Fixed labels, first match wins.
const (
	a1ReasonRelayOptions    = "relay_options"        // references/auto, full contracts or history_conflict: reject
	a1ReasonToolChoice      = "tool_choice"          // tool_choice other than absent / "auto"
	a1ReasonCompaction      = "compaction"           // the input carries a compaction item
	a1ReasonAdditionalTools = "has_additional_tools" // the input already declares additional_tools
	a1ReasonToolShape       = "tool_shape"           // a name or nesting the Lite shape cannot carry
	a1ReasonDuplicateTool   = "duplicate_tool"       // one key, two different definitions
	a1ReasonSchemaInvalid   = "schema_invalid"       // Codex would refuse the schema
	a1ReasonNoTools         = "no_tools"             // nothing left after dropping hosted tools
	a1ReasonCatalogTooLarge = "catalog_too_large"
	a1ReasonSchemaFeature   = "schema_feature"   // a feature on relay_native_promotion_block
	a1ReasonCatalogRejected = "catalog_rejected" // the upstream refused this catalogue before
	a1ReasonBuildFailed     = "build_failed"     // preparing the promoted body failed
)

var a1Reasons = map[string]bool{
	a1ReasonRelayOptions: true, a1ReasonToolChoice: true, a1ReasonCompaction: true,
	a1ReasonAdditionalTools: true, a1ReasonToolShape: true, a1ReasonDuplicateTool: true,
	a1ReasonSchemaInvalid: true, a1ReasonNoTools: true, a1ReasonCatalogTooLarge: true,
	a1ReasonSchemaFeature: true, a1ReasonCatalogRejected: true, a1ReasonBuildFailed: true,
}

// a1BuiltinNames are names the upstream is known or suspected to declare
// itself (Q3). Counted, never blocked by default.
var a1BuiltinNames = map[string]bool{
	"update_plan": true, "file_search": true, "web_search": true,
	"list_skills": true, "read_skills": true, "read_sheets_metadata": true,
}

// a1ToolNamePattern: the Responses API function name rule.
var a1ToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ---------------------------------------------------------------- switches

// normalizeA1Block canonicalises relay_native_promotion_block: known feature
// words, sorted and joined by commas; "none" blocks nothing; empty is the
// default. Anything else is invalid (the caller falls back with a warning).
func normalizeA1Block(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return a1DefaultBlock, true
	}
	if value == a1BlockNone {
		return a1BlockNone, true
	}
	words := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	set := map[string]bool{}
	for _, word := range words {
		if !a1Features[word] {
			return "", false
		}
		set[word] = true
	}
	if len(set) == 0 {
		return "", false
	}
	list := make([]string, 0, len(set))
	for word := range set {
		list = append(list, word)
	}
	sort.Strings(list)
	return strings.Join(list, ","), true
}

func a1BlockedFeatures(canonical string) map[string]bool {
	normalized, ok := normalizeA1Block(canonical)
	if !ok {
		normalized = a1DefaultBlock
	}
	blocked := map[string]bool{}
	if normalized == a1BlockNone {
		return blocked
	}
	for _, word := range strings.Split(normalized, ",") {
		blocked[word] = true
	}
	return blocked
}

// ---------------------------------------------------------------- the plan

// a1Decision is whether one relayed request can be promoted. It holds no
// request content beyond the catalogue it built (tools), which is dropped as
// soon as the promoted body exists.
type a1Decision struct {
	promotable bool
	reason     string
	features   map[string]bool
	blocked    []string
	hosted     map[string]int
	tools      []any
	toolCount  int
	bytes      int
	hash       string
	compacted  int
	// choice is the request's tool_choice as promotion read it (a1wide2).
	choice a1ToolChoiceSpec
}

// a1Plan rides on the request from preparation to the end of its upstream
// call. It is created only when relay_native_promotion is shadow or on and
// the request is a relayed one (it has a relay catalogue).
type a1Plan struct {
	mode     string
	history  string
	decision a1Decision
	// applied: the prepared upstream body is the promoted one.
	applied bool
	// fellBack: the promoted body was refused before the stream opened and
	// the relay body was sent instead.
	fellBack bool
	// relayBody builds the relay (h25) body for the fallback; nil once the
	// upstream call is settled.
	relayBody func() (map[string]any, error)
}

// a1HistoryMarker rides on the promoted source (never on a body) and tells
// request preparation how to translate the history (history_replay.go).
type a1HistoryMarker struct {
	history string
	// relayCatalogue is the original request's relay catalogue (h22's
	// clientToolSpecs): under h1 a cold historical call of one of these tools
	// is wrapped as run_officejs exactly as the relay would.
	relayCatalogue map[string]toolSpec
	// originalInput is the client's own input, for turn state.
	originalInput any
	// toolChoiceNotice is the developer message a forced tool_choice adds at
	// the end of the input (a1ToolChoiceNotice); "" for none.
	toolChoiceNotice string
}

func (*a1HistoryMarker) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

const a1HistoryKey = "\x00cpa_a1_history"

func a1HistoryMarkerFrom(source map[string]any) *a1HistoryMarker {
	marker, _ := source[a1HistoryKey].(*a1HistoryMarker)
	return marker
}

// a1ResponseState rides on the response decoder's source of a request that
// went upstream promoted (and was not fallen back).
type a1ResponseState struct{ history string }

func (*a1ResponseState) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

const a1ResponseKey = "\x00cpa_a1_response"

func withA1Response(source map[string]any, plan *a1Plan) map[string]any {
	if plan == nil || !plan.applied {
		return source
	}
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	result[a1ResponseKey] = &a1ResponseState{history: plan.history}
	return result
}

func a1ResponseFrom(source map[string]any) *a1ResponseState {
	state, _ := source[a1ResponseKey].(*a1ResponseState)
	return state
}

// planNativePromotion decides, from the switches frozen in cfg, whether this
// request is promoted. It returns nil for off and for any request that is not
// a relayed one; promoted is the source to prepare instead (on only). It
// never modifies source.
func planNativePromotion(source map[string]any, cfg Config, scope string) (plan *a1Plan, promoted map[string]any) {
	options := relayOptionsFromConfig(cfg)
	mode := options.NativePromotion
	if mode != a1ModeShadow && mode != a1ModeOn {
		return nil, nil
	}
	relayCatalogue := clientToolSpecs(source)
	if len(relayCatalogue) == 0 {
		return nil, nil
	}
	history := options.NativePromotionHistory
	if history != a1HistoryH2 {
		history = a1HistoryH1
	}
	selected := selectRelayProtocol(source, cfg.RelayProtocol, scope)
	plan = &a1Plan{mode: mode, history: history, decision: decideNativePromotion(source, options, selected)}
	tools := plan.decision.tools
	plan.decision.tools = nil
	if mode != a1ModeOn || !plan.decision.promotable {
		return plan, nil
	}
	marker := &a1HistoryMarker{history: history, relayCatalogue: relayCatalogue, originalInput: source["input"],
		toolChoiceNotice: a1ToolChoiceNotice(plan.decision.choice, source["parallel_tool_calls"])}
	plan.applied = true
	return plan, a1PromotedSource(source, a1AdditionalToolsItem(tools, scope), marker)
}

// decideNativePromotion is the not-promotable list. The catalogue is built
// even when an earlier rule already says no, so the features of every relayed
// request are counted.
func decideNativePromotion(source map[string]any, options relayOptions, selectedProtocol string) a1Decision {
	// a1wide2: a forced or allowed_tools choice narrows the catalogue to the
	// tools it allows.
	choice := a1ParseToolChoice(source["tool_choice"])
	var keep func(kind, key string) bool
	if choice.keep != nil {
		keep = func(kind, key string) bool { return choice.keep[kind+":"+key] }
	}
	catalogue := buildA1CatalogueFor(source["tools"], keep)
	decision := a1Decision{
		features: catalogue.features, hosted: catalogue.hosted, toolCount: catalogue.count,
		compacted: catalogue.compacted, choice: choice,
	}
	if choice.kind != a1ChoiceAuto {
		decision.features[a1FeatureToolChoice] = true
	}
	if catalogue.reason == "" {
		encoded := jsonBytes(catalogue.tools)
		decision.tools, decision.bytes, decision.hash = catalogue.tools, len(encoded), shortHash(string(encoded))
		if decision.bytes > a1MaxCatalogBytes {
			catalogue.reason = a1ReasonCatalogTooLarge
		}
	}
	// A compaction item is a feature of the request, not of its catalogue:
	// counted like one, and kept on the relay only when it is blocked.
	compaction := a1HasCompactionItem(source["input"])
	if compaction {
		decision.features[a1FeatureCompaction] = true
	}
	blockedSet := a1BlockedFeatures(options.NativePromotionBlock)
	for feature := range decision.features {
		if blockedSet[feature] {
			decision.blocked = append(decision.blocked, feature)
		}
	}
	sort.Strings(decision.blocked)
	switch {
	case selectedProtocol != relayProtocolLegacy || options.ToolContracts == relayToolContractsFull ||
		options.HistoryConflict == historyConflictReject:
		decision.reason = a1ReasonRelayOptions
	case choice.kind == a1ChoiceUnsupported || (choice.kind != a1ChoiceAuto && blockedSet[a1FeatureToolChoice]):
		decision.reason = a1ReasonToolChoice
	case choice.kind != a1ChoiceAuto && catalogue.count == 0:
		// The tools the choice names or allows are not in the catalogue.
		decision.reason = a1ReasonToolChoice
	case compaction && blockedSet[a1FeatureCompaction]:
		decision.reason = a1ReasonCompaction
	case a1HasAdditionalToolsItem(source["input"]):
		decision.reason = a1ReasonAdditionalTools
	case catalogue.reason != "":
		decision.reason = catalogue.reason
	case len(decision.blocked) > 0:
		decision.reason = a1ReasonSchemaFeature
	case promotionCatalogDenied(decision.hash):
		decision.reason = a1ReasonCatalogRejected
	default:
		decision.promotable = true
	}
	if !decision.promotable {
		decision.tools = nil
	}
	return decision
}

// Tool choice kinds as promotion reads them (a1wide2). Fixed labels: they name
// the a1_tool_choice counters.
const (
	a1ChoiceAuto            = ""
	a1ChoiceRequired        = "required"
	a1ChoiceFunction        = "function"
	a1ChoiceCustom          = "custom"
	a1ChoiceAllowedAuto     = "allowed_tools_auto"
	a1ChoiceAllowedRequired = "allowed_tools_required"
	a1ChoiceUnsupported     = "unsupported"
)

// a1ToolChoiceSpec is a relayed request's tool_choice. keep is the set of
// "<type>:<catalogue key>" a forced or allowed_tools choice allows (nil:
// every tool); name is the forced tool's catalogue key.
type a1ToolChoiceSpec struct {
	kind string
	keep map[string]bool
	name string
}

// a1ParseToolChoice reads tool_choice in its Responses shapes: absent or
// "auto" (Codex always sends "auto", client.rs:1057), "required", {type:
// function|custom, name, namespace?} and {type: allowed_tools, mode:
// auto|required, tools}. Anything else -- "none" never gets here, it leaves
// no relay catalogue -- stays on the relay.
func a1ParseToolChoice(raw any) a1ToolChoiceSpec {
	if raw == nil {
		return a1ToolChoiceSpec{}
	}
	if text, ok := raw.(string); ok {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "auto":
			return a1ToolChoiceSpec{}
		case "required":
			return a1ToolChoiceSpec{kind: a1ChoiceRequired}
		}
		return a1ToolChoiceSpec{kind: a1ChoiceUnsupported}
	}
	choice := objectValue(raw)
	switch kind := stringValue(choice["type"]); kind {
	case "function", "custom":
		key, ok := a1ChoiceKey(choice)
		if !ok {
			break
		}
		return a1ToolChoiceSpec{kind: kind, keep: map[string]bool{kind + ":" + key: true}, name: key}
	case "allowed_tools":
		spec := a1ToolChoiceSpec{keep: map[string]bool{}}
		switch stringValue(choice["mode"]) {
		case "auto":
			spec.kind = a1ChoiceAllowedAuto
		case "required":
			spec.kind = a1ChoiceAllowedRequired
		default:
			return a1ToolChoiceSpec{kind: a1ChoiceUnsupported}
		}
		tools, ok := choice["tools"].([]any)
		if !ok {
			break
		}
		for _, value := range tools {
			tool := objectValue(value)
			switch toolType := stringValue(tool["type"]); toolType {
			case "function", "custom":
				key, ok := a1ChoiceKey(tool)
				if !ok {
					return a1ToolChoiceSpec{kind: a1ChoiceUnsupported}
				}
				spec.keep[toolType+":"+key] = true
			}
			// A hosted tool is never promoted, so allowing it adds nothing.
		}
		return spec
	}
	return a1ToolChoiceSpec{kind: a1ChoiceUnsupported}
}

// a1ChoiceKey is the catalogue key a choice names (h22's key: name, or
// namespace.name), only for names the Lite catalogue can carry.
func a1ChoiceKey(choice map[string]any) (string, bool) {
	name := stringValue(choice["name"])
	if !a1ToolNamePattern.MatchString(name) {
		return "", false
	}
	if namespace := stringValue(choice["namespace"]); namespace != "" {
		if !a1ToolNamePattern.MatchString(namespace) {
			return "", false
		}
		return namespace + "." + name, true
	}
	return name, true
}

// a1ToolChoiceNotice is the developer message a promoted request with a forced
// tool_choice carries at the end of its input. The upstream refuses a
// tool_choice field (422, 2026-10-09), so a forced choice can only travel as
// text, and at the end it moves no cached prefix. "" when nothing is forced
// (auto, allowed_tools auto: the narrowed catalogue says it all).
func a1ToolChoiceNotice(choice a1ToolChoiceSpec, parallel any) string {
	switch choice.kind {
	case a1ChoiceRequired, a1ChoiceAllowedRequired:
		count := "at least one"
		if allowed, ok := parallel.(bool); ok && !allowed {
			count = "exactly one"
		}
		return "Tool choice for this response is required: call " + count + " of the client tools declared in additional_tools, directly as declared. A text-only answer does not satisfy this request."
	case a1ChoiceFunction, a1ChoiceCustom:
		return "Tool choice for this response requires exactly one call to the client tool " + a1ChoiceDisplayName(choice.name) +
			", directly as declared in additional_tools. A text-only answer does not satisfy this request."
	}
	return ""
}

// a1ChoiceDisplayName names a catalogue key the way the Lite catalogue shows
// it: flat tools sit in the functions namespace.
func a1ChoiceDisplayName(key string) string {
	if namespace, name, found := strings.Cut(key, "."); found {
		if namespace == a1FunctionsNamespace {
			return name
		}
		return name + " (namespace " + namespace + ")"
	}
	return key
}

// a1HasCompactionItem: the input carries a compaction item (compaction,
// compaction_trigger, ...). Lite conversations lost their additional_tools
// after some upstream in-place compactions at a 200k threshold
// (lite-tools-drop-after-compaction-20260929); since a1wide such a request is
// promoted unless relay_native_promotion_block names "compaction".
func a1HasCompactionItem(input any) bool {
	items, _ := input.([]any)
	for _, raw := range items {
		if item := objectValue(raw); item != nil &&
			strings.HasPrefix(strings.ToLower(strings.TrimSpace(stringValue(item["type"]))), "compaction") {
			return true
		}
	}
	return false
}

func a1HasAdditionalToolsItem(input any) bool {
	items, _ := input.([]any)
	for _, raw := range items {
		if item := objectValue(raw); item != nil &&
			strings.EqualFold(strings.TrimSpace(stringValue(item["type"])), "additional_tools") {
			return true
		}
	}
	return false
}

// a1PromotedSource is source with its top-level tools moved into an
// additional_tools item at input[0]. Every other field is shared, read only.
func a1PromotedSource(source, item map[string]any, marker *a1HistoryMarker) map[string]any {
	promoted := make(map[string]any, len(source)+1)
	for key, value := range source {
		if key != "tools" {
			promoted[key] = value
		}
	}
	input := []any{item}
	switch typed := source["input"].(type) {
	case string:
		input = append(input, messageItem("user", typed))
	case []any:
		input = append(input, typed...)
	}
	promoted["input"] = input
	promoted[a1HistoryKey] = marker
	return promoted
}

// a1AdditionalToolsItem is client.rs:967-983: id "at_" + a v5 UUID of the
// catalogue under a per-thread namespace. The replay scope stands in for the
// thread, so the id is stable across the turns of one conversation and the
// cached prefix does not move.
func a1AdditionalToolsItem(tools []any, scope string) map[string]any {
	thread := a1UUIDv5(a1OIDNamespace, []byte("cpa-a1-thread\x00"+scope))
	id := "at_" + a1FormatUUID(a1UUIDv5(thread, jsonBytes(tools)))
	return map[string]any{"type": "additional_tools", "id": id, "role": "developer", "tools": tools}
}

// a1OIDNamespace is uuid::Uuid::NAMESPACE_OID.
var a1OIDNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

func a1UUIDv5(namespace [16]byte, name []byte) [16]byte {
	hash := sha1.New()
	_, _ = hash.Write(namespace[:])
	_, _ = hash.Write(name)
	var out [16]byte
	copy(out[:], hash.Sum(nil))
	out[6] = (out[6] & 0x0f) | 0x50
	out[8] = (out[8] & 0x3f) | 0x80
	return out
}

func a1FormatUUID(value [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

// ---------------------------------------------------------------- catalogue

type a1CatalogueResult struct {
	tools     []any
	features  map[string]bool
	hosted    map[string]int
	count     int
	compacted int
	reason    string
}

// a1NamespaceEntry collects one client namespace's members (two declarations
// of the same namespace are merged, as h22's catalogue merges them).
type a1NamespaceEntry struct {
	name, description string
	members           []any
	seen              map[string]string
}

// buildA1Catalogue is create_tools_json_for_responses_lite over the request's
// top-level tools, with every tool definition rewritten to Codex's shape.
func buildA1Catalogue(rawTools any) a1CatalogueResult {
	return buildA1CatalogueFor(rawTools, nil)
}

// buildA1CatalogueFor is buildA1Catalogue over the function and custom tools
// keep allows, by type and h22 catalogue key (nil: all of them). A namespace
// left without members is left out, as an empty one always was.
func buildA1CatalogueFor(rawTools any, keep func(kind, key string) bool) a1CatalogueResult {
	result := a1CatalogueResult{features: map[string]bool{}, hosted: map[string]int{}}
	fail := func(reason string) {
		if result.reason == "" {
			result.reason = reason
		}
	}
	functions := &a1NamespaceEntry{name: a1FunctionsNamespace, seen: map[string]string{}}
	functionsIndex := -1
	// order holds the namespace entries in place; nil marks where functions go.
	var order []*a1NamespaceEntry
	namespaces := map[string]*a1NamespaceEntry{}
	addMember := func(entry *a1NamespaceEntry, kind, name string, tool map[string]any) {
		definition, reason := a1NormalizeTool(tool, kind, name, &result)
		if reason != "" {
			fail(reason)
			return
		}
		encoded := string(jsonBytes(definition))
		if previous, seen := entry.seen[name]; seen {
			if previous != encoded {
				fail(a1ReasonDuplicateTool)
			}
			return
		}
		entry.seen[name] = encoded
		entry.members = append(entry.members, definition)
		result.count++
	}
	list, _ := rawTools.([]any)
	for _, raw := range list {
		tool := objectValue(raw)
		if tool == nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.TrimSpace(stringValue(tool["name"]))
		switch kind {
		case "function", "custom":
			if name == "" {
				// h22's catalogue ignores a nameless tool too.
				continue
			}
			if keep != nil && !keep(kind, name) {
				continue
			}
			addMember(functions, kind, name, tool)
			if functionsIndex < 0 {
				functionsIndex = len(order)
			}
		case "namespace":
			if name == "" {
				continue
			}
			if !a1ToolNamePattern.MatchString(name) || isTransportName(name) {
				fail(a1ReasonToolShape)
				continue
			}
			entry := functions
			if name == a1FunctionsNamespace {
				// tool_spec.rs:118-123: merged into the default namespace,
				// whose description it may supply.
				if description, ok := tool["description"].(string); ok && strings.TrimSpace(description) != "" {
					functions.description = description
				}
				if functionsIndex < 0 {
					functionsIndex = len(order)
				}
			} else {
				result.features[a1FeatureNamespace] = true
				entry = namespaces[name]
				if entry == nil {
					entry = &a1NamespaceEntry{name: name, seen: map[string]string{}}
					entry.description, _ = tool["description"].(string)
					namespaces[name] = entry
					order = append(order, entry)
				}
			}
			members, _ := tool["tools"].([]any)
			for _, rawMember := range members {
				member := objectValue(rawMember)
				if member == nil {
					continue
				}
				memberKind := strings.ToLower(strings.TrimSpace(stringValue(member["type"])))
				memberName := strings.TrimSpace(stringValue(member["name"]))
				switch memberKind {
				case "function", "custom":
					if memberName != "" && (keep == nil || keep(memberKind, name+"."+memberName)) {
						addMember(entry, memberKind, memberName, member)
					}
				case "namespace":
					// A namespace inside a namespace has no Lite shape.
					fail(a1ReasonToolShape)
				default:
					result.hosted[a1HostedLabel(memberKind)]++
				}
			}
		default:
			result.hosted[a1HostedLabel(kind)]++
		}
	}
	tools := make([]any, 0, len(order)+1)
	for index, entry := range order {
		if index == functionsIndex && len(functions.members) > 0 {
			tools = append(tools, a1NamespaceDefinition(functions))
		}
		if len(entry.members) > 0 {
			tools = append(tools, a1NamespaceDefinition(entry))
		}
	}
	if functionsIndex >= len(order) && len(functions.members) > 0 {
		tools = append(tools, a1NamespaceDefinition(functions))
	}
	result.tools = tools
	switch {
	case result.count == 0:
		fail(a1ReasonNoTools)
	case result.count > a1MaxCatalogTools:
		fail(a1ReasonCatalogTooLarge)
	}
	return result
}

func a1NamespaceDefinition(entry *a1NamespaceEntry) map[string]any {
	return map[string]any{"type": "namespace", "name": entry.name, "description": entry.description, "tools": entry.members}
}

// a1NormalizeTool is one function or custom tool in Codex's Lite shape.
func a1NormalizeTool(tool map[string]any, kind, name string, result *a1CatalogueResult) (map[string]any, string) {
	if !a1ToolNamePattern.MatchString(name) || isTransportName(name) {
		return nil, a1ReasonToolShape
	}
	if a1BuiltinNames[name] {
		result.features[a1FeatureBuiltinName] = true
	}
	// The description is the client's, unchanged; none is "" (Codex's
	// unwrap_or_default).
	description, _ := tool["description"].(string)
	if kind == "custom" {
		result.features[a1FeatureCustomTool] = true
		definition := map[string]any{"type": "custom", "name": name, "description": description}
		format := objectValue(tool["format"])
		if definitionText, lark := a1LarkGrammar(format); lark {
			definition["format"] = map[string]any{"type": "grammar", "syntax": "lark", "definition": definitionText}
		} else {
			result.features[a1FeatureCustomFormatOther] = true
			if format != nil {
				definition["format"] = cloneObject(format)
			}
		}
		return definition, ""
	}
	parameters, info, err := a1ToolParameters(tool["parameters"])
	if err != nil {
		return nil, a1ReasonSchemaInvalid
	}
	for feature, present := range map[string]bool{
		a1FeatureRef: info.ref, a1FeatureComposition: info.composition, a1FeatureEnum: info.enum,
		a1FeatureTypeList: info.typeList, a1FeatureAdditionalTrue: info.additionalTrue,
		a1FeatureAdditionalSchema: info.additionalSchema, a1FeatureEncrypted: info.encrypted,
		a1FeatureCompacted: info.compacted,
	} {
		if present {
			result.features[feature] = true
		}
	}
	if info.compacted {
		result.compacted++
	}
	// strict:true is counted (and can be blocked), but never sent: the
	// upstream refuses it before the stream opens, and every real Lite
	// function is strict:false.
	if tool["strict"] == true {
		result.features[a1FeatureStrictTrue] = true
	}
	return map[string]any{"type": "function", "name": name, "description": description, "strict": false, "parameters": parameters}, ""
}

// a1LarkGrammar: the only custom format a real Lite catalogue carries
// (896/896): {type: grammar, syntax: lark, definition}.
func a1LarkGrammar(format map[string]any) (string, bool) {
	if format == nil || format["type"] != "grammar" || format["syntax"] != "lark" {
		return "", false
	}
	definition, ok := format["definition"].(string)
	return definition, ok
}

// a1HostedLabel maps a hosted tool type to a fixed counter label.
func a1HostedLabel(kind string) string {
	switch kind {
	case "web_search", "web_search_preview":
		return "web_search"
	case "tool_search", "image_generation", "file_search", "code_interpreter", "local_shell", "mcp":
		return kind
	case "computer_use", "computer_use_preview":
		return "computer_use"
	}
	return "other"
}

// ---------------------------------------------------------------- not-promotable cache

// a1DenyCache remembers catalogues the upstream refused before the stream
// opened: promoting them again only buys another 400. Keyed by the
// catalogue's hash, bounded in size and lifetime.
var a1DenyCache = struct {
	sync.Mutex
	items map[string]time.Time
	order []string
}{items: map[string]time.Time{}}

func denyPromotionCatalog(hash string) {
	if hash == "" {
		return
	}
	a1DenyCache.Lock()
	defer a1DenyCache.Unlock()
	if _, exists := a1DenyCache.items[hash]; !exists {
		a1DenyCache.order = append(a1DenyCache.order, hash)
	}
	a1DenyCache.items[hash] = time.Now()
	for len(a1DenyCache.order) > a1DenyEntries {
		oldest := a1DenyCache.order[0]
		a1DenyCache.order = a1DenyCache.order[1:]
		delete(a1DenyCache.items, oldest)
	}
}

func promotionCatalogDenied(hash string) bool {
	if hash == "" {
		return false
	}
	a1DenyCache.Lock()
	defer a1DenyCache.Unlock()
	stored, exists := a1DenyCache.items[hash]
	if !exists {
		return false
	}
	if time.Since(stored) > a1DenyTTL {
		delete(a1DenyCache.items, hash)
		for index, key := range a1DenyCache.order {
			if key == hash {
				a1DenyCache.order = append(a1DenyCache.order[:index], a1DenyCache.order[index+1:]...)
				break
			}
		}
		return false
	}
	return true
}

// ---------------------------------------------------------------- fallback

// a1FallbackBody decides whether a failed upstream call is the A1 fallback
// case -- the promoted body went up and was refused with 400 before any
// stream opened -- and if so returns the relay body to send instead, once.
func (s *Service) a1FallbackBody(request *ExecutorRequest, err error) (map[string]any, bool) {
	plan := request.a1
	if plan == nil || !plan.applied || plan.relayBody == nil || err == nil {
		return nil, false
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status != 400 {
		return nil, false
	}
	build := plan.relayBody
	plan.relayBody = nil
	body, buildErr := build()
	if buildErr != nil {
		countRelayEvent("a1_fallback:build_failed")
		s.logA1(request.HostCallbackID, "fallback_build_failed", plan, map[string]any{})
		return nil, false
	}
	plan.applied, plan.fellBack = false, true
	bucket := a1FallbackBucket(api.Message)
	countRelayEvent("a1_fallback:attempted")
	countRelayEvent("a1_fallback_reason:" + bucket)
	s.logA1(request.HostCallbackID, "fallback", plan, map[string]any{"reason": bucket, "upstream_code": a1UpstreamCode(api.Message)})
	return body, true
}

// a1FallbackDone records how the relay retry went. A catalogue whose relay
// body was accepted is remembered as not promotable; one whose relay body was
// refused as well proves nothing about the catalogue and is not.
func (s *Service) a1FallbackDone(request *ExecutorRequest, err error) {
	plan := request.a1
	if plan == nil || !plan.fellBack {
		return
	}
	if err == nil {
		countRelayEvent("a1_fallback_result:succeeded")
		denyPromotionCatalog(plan.decision.hash)
		countRelayEvent("a1_catalog_denied")
		return
	}
	status := 0
	var api *APIError
	if errors.As(err, &api) && api.Status >= 100 && api.Status <= 599 {
		status = api.Status
	}
	countRelayEvent("a1_fallback_result:failed:" + strconv.Itoa(status))
}

// a1Settled drops what the fallback needed once the upstream call is decided.
func a1Settled(request *ExecutorRequest) {
	if request.a1 != nil {
		request.a1.relayBody = nil
	}
}

// a1FallbackBucket classifies the upstream's refusal by fixed words only.
func a1FallbackBucket(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "missing namespace"):
		return "missing_namespace"
	case strings.Contains(lower, "additional_tools"):
		return "additional_tools"
	case strings.Contains(lower, "grammar") || strings.Contains(lower, "lark"):
		return "grammar"
	case strings.Contains(lower, "strict"):
		return "strict"
	case strings.Contains(lower, "schema") || strings.Contains(lower, "parameters"):
		return "schema"
	case strings.Contains(lower, "namespace"):
		return "namespace"
	case strings.Contains(lower, "tool"):
		return "tools"
	case strings.Contains(lower, "input["):
		return "input"
	}
	return "other"
}

var a1CodePattern = regexp.MustCompile(`^[a-z0-9_]{1,48}$`)

// a1UpstreamCode is the upstream's own error.code (or type), when it is a
// short code word; never free text.
func a1UpstreamCode(message string) string {
	var envelope struct {
		Error struct {
			Code any    `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(message), &envelope) != nil {
		return ""
	}
	if code, ok := envelope.Error.Code.(string); ok && a1CodePattern.MatchString(code) {
		return code
	}
	if a1CodePattern.MatchString(envelope.Error.Type) {
		return envelope.Error.Type
	}
	return ""
}

// logA1 writes one relay_native_promotion line: fixed labels, the catalogue
// hash and sizes, never a tool name, schema or argument.
func (s *Service) logA1(callbackID, stage string, plan *a1Plan, extra map[string]any) {
	defer func() { _ = recover() }()
	fields := map[string]any{
		"event": "relay_native_promotion", "plugin_version": Version, "stage": stage,
		"mode": plan.mode, "history": plan.history, "tools": plan.decision.toolCount,
		"catalog_bytes": plan.decision.bytes, "catalog": a1HashPrefix(plan.decision.hash),
	}
	for key, value := range extra {
		if value != "" {
			fields[key] = value
		}
	}
	_ = s.call("host.log", map[string]any{
		"host_callback_id": callbackID, "level": "info",
		"message": "basispoints relay native promotion " + string(jsonBytes(fields)),
		"fields":  fields,
	}, nil)
}

func a1HashPrefix(hash string) string {
	if len(hash) > 16 {
		return hash[:16]
	}
	return hash
}

// ---------------------------------------------------------------- counters

// countRelayEventN adds n to a counter.
func countRelayEventN(name string, n int) {
	if n <= 0 {
		return
	}
	relayCounters.Lock()
	relayCounters.values[name] += uint64(n)
	relayCounters.Unlock()
}

// countA1Request counts an admitted relayed request's A1 decision, with
// relay_requests (relay_stats.go). Counting must never fail the request.
//
//	a1_requests:<mode>:<promoted|promotable|not_promotable>
//	a1_not_promotable:<mode>:<reason>
//	a1_feature:<mode>:<feature>        every feature the catalogue uses
//	a1_blocked:<mode>:<feature>        features that kept it on the relay
//	a1_hosted_dropped:<mode>:<type>    hosted tools left out (per tool)
//	a1_schema_compacted:<mode>         tools whose schema went through the size passes
//	a1_catalog_tools:<mode>:<bucket>, a1_catalog_bytes:<mode>:<bucket>
//	a1_history:<h1|h2>                 promoted requests, by history rendering
//	a1_tool_choice:<mode>:<kind>       promoted requests with a forced or
//	                                   allowed_tools choice (a1wide2)
func countA1Request(request ExecutorRequest) {
	plan := request.a1
	if plan == nil {
		return
	}
	defer func() { _ = recover() }()
	mode := plan.mode
	decision := plan.decision
	switch {
	case plan.applied || plan.fellBack:
		countRelayEvent("a1_requests:" + mode + ":promoted")
		countRelayEvent("a1_history:" + plan.history)
		if kind := decision.choice.kind; kind != a1ChoiceAuto && kind != a1ChoiceUnsupported {
			// a1wide2: a promoted forced / allowed_tools choice, by kind.
			countRelayEvent("a1_tool_choice:" + mode + ":" + kind)
		}
	case decision.promotable:
		countRelayEvent("a1_requests:" + mode + ":promotable")
	default:
		countRelayEvent("a1_requests:" + mode + ":not_promotable")
		reason := decision.reason
		if !a1Reasons[reason] {
			reason = "unspecified"
		}
		countRelayEvent("a1_not_promotable:" + mode + ":" + reason)
	}
	for feature := range decision.features {
		if a1Features[feature] {
			countRelayEvent("a1_feature:" + mode + ":" + feature)
		}
	}
	for _, feature := range decision.blocked {
		countRelayEvent("a1_blocked:" + mode + ":" + feature)
	}
	for label, n := range decision.hosted {
		countRelayEventN("a1_hosted_dropped:"+mode+":"+label, n)
	}
	countRelayEventN("a1_schema_compacted:"+mode, decision.compacted)
	countRelayEvent("a1_catalog_tools:" + mode + ":" + a1CountBucket(decision.toolCount))
	if decision.hash != "" {
		countRelayEvent("a1_catalog_bytes:" + mode + ":" + a1BytesBucket(decision.bytes))
	}
}

func a1CountBucket(n int) string {
	switch {
	case n == 0:
		return "0"
	case n <= 5:
		return "1-5"
	case n <= 20:
		return "6-20"
	case n <= 60:
		return "21-60"
	case n <= 120:
		return "61-120"
	}
	return "121+"
}

func a1BytesBucket(n int) string {
	switch {
	case n < 16<<10:
		return "<16k"
	case n < 64<<10:
		return "16k-64k"
	case n < 256<<10:
		return "64k-256k"
	}
	return "256k+"
}

// observeA1NativeCall counts one native client tool call the upstream made in
// a promoted response, once per call however often the stream sees it:
//
//	a1_native_calls:<function|custom>:<declared|functions_namespace|undeclared>
//
// functions_namespace: the call came back as namespace "functions", which
// the Lite capture never showed (the upstream drops it, and Codex reads a
// bare name as functions -- router.rs:257-267, tool_name.rs:39-44). Nothing is
// rewritten: on the way back a promoted response is a Lite response.
func observeA1NativeCall(item, source map[string]any, catalog declaredCatalog) {
	if a1ResponseFrom(source) == nil || !isStreamToolItem(item) || isTransportCall(item) {
		return
	}
	notes, _ := source[relayDecodeNotesKey].(*relayDecodeNotes)
	callID := stringValue(item["call_id"])
	if notes == nil || callID == "" || !notes.claimObservation("a1n\x00"+callID) {
		return
	}
	defer func() { _ = recover() }()
	kind := "function"
	if stringValue(item["type"]) == "custom_tool_call" {
		kind = "custom"
	}
	where := "undeclared"
	if _, known := knownNativeToolSpecIn(item, source, catalog); known {
		where = "declared"
	} else if stringValue(item["namespace"]) == a1FunctionsNamespace {
		where = "functions_namespace"
	}
	countRelayEvent("a1_native_calls:" + kind + ":" + where)
}

// observeA1TransportCall counts a run_officejs call in a promoted response:
// the model went back to the relay transport (Q8).
//
//	a1_transport_calls:<ok|reason>
func observeA1TransportCall(native, source map[string]any, reason string) {
	if a1ResponseFrom(source) == nil || !isTransportCall(native) {
		return
	}
	notes, _ := source[relayDecodeNotesKey].(*relayDecodeNotes)
	if notes == nil || !notes.claimObservation("a1t\x00"+relayCallNoteID(native)) {
		return
	}
	defer func() { _ = recover() }()
	outcome := "ok"
	if reason != "" {
		outcome = toolReasonLabel(reason)
	}
	countRelayEvent("a1_transport_calls:" + outcome)
}

// a1Summary is the a1_* counters for relay-status, or nil when there are none.
func a1Summary(counters map[string]uint64) map[string]uint64 {
	var out map[string]uint64
	for key, value := range counters {
		if strings.HasPrefix(key, "a1_") {
			if out == nil {
				out = map[string]uint64{}
			}
			out[key] = value
		}
	}
	return out
}
