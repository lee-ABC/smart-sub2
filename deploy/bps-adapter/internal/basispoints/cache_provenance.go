package basispoints

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Replay caches (H25-SPEC S3).
//
// nativeCallCache is h22's cache: the upstream run_officejs call behind each
// relayed client call, keyed by replay scope and call_id, so the next turn
// replays upstream exactly what the model emitted. Its capacity and lifetime
// are h22's (4096 entries, two hours from the last write, a hit does not
// extend it); rc2 added a byte budget, kept here generously enough that the
// entry count is what binds: 256 MiB is 64 KiB per entry at h22's 4096,
// against production records of 1.85 KB on average and 28.5 KB at most
// (676 real run_officejs calls, 10-07). h22 had no byte limit at all, so the
// budget is the only reason this pool can forget a record h22 would replay;
// replay_byte_eviction:relay counts every such eviction (review R1/EH-5).
//
// One record may take up to 64 MiB (a quarter of the budget; a model's own
// output is a few hundred KB at most). A record past that is not kept, but
// the pool still remembers, in a marker holding no argument bytes, that its
// call_id was a relay call: the next turn's cold path then wraps the client's
// copy as run_officejs (h22's fallback) instead of sending it upstream under
// its bare client name, which the upstream has never declared (h25 review,
// cache boundary). replay_record_too_large:relay counts such records.
//
// nativeOriginCache holds the native (Lite) calls the client was handed. h22
// never cached them; rc2 put them into the relay cache, where at Lite volumes
// they took most of its places and halved the useful life of relay entries.
// They live in their own pool now and can never evict a relay entry. They
// only feed history_conflict (a log line under replay, a refusal under
// reject) and reject mode's namespace recovery; under replay they never
// change a byte sent upstream.
const (
	nativeCallCacheTTL     = 2 * time.Hour
	nativeCallCacheEntries = 4096
	nativeCallCacheBytes   = 256 << 20
	nativeCallEntryBytes   = 64 << 20

	nativeOriginCacheEntries = 4096
	nativeOriginCacheBytes   = 16 << 20
	nativeOriginEntryBytes   = 1 << 20

	// clientCallFingerprintBytes bounds the payload a fingerprint reads.
	clientCallFingerprintBytes = 1 << 20
)

type nativeCallEntry struct {
	item, client map[string]any
	fingerprint  string
	// origin: relay (delivered relay call), native (delivered native call),
	// history (h22: a run_officejs call met in client history), unverified
	// (legacy helper), conflict (reject mode tombstone).
	origin   string
	strict   string
	conflict bool
	// marker: a relay call whose record was too large to keep. It holds
	// neither the transport call nor a fingerprint, so it never replays and
	// never answers a history comparison; it only tells the cold path that
	// the call_id was a relay call (relayCallMarked).
	marker   bool
	weight   int
	storedAt time.Time
}

type replayPool struct {
	sync.Mutex
	items map[string]nativeCallEntry
	order []string
	bytes int

	maxEntries, maxBytes, maxEntry int
	// name labels the pool's counters.
	name string
	// markOversized: a record past maxEntry leaves a marker (relay pool).
	markOversized bool
}

var nativeCallCache = &replayPool{items: map[string]nativeCallEntry{}, name: "relay",
	maxEntries: nativeCallCacheEntries, maxBytes: nativeCallCacheBytes, maxEntry: nativeCallEntryBytes, markOversized: true}

var nativeOriginCache = &replayPool{items: map[string]nativeCallEntry{}, name: "native",
	maxEntries: nativeOriginCacheEntries, maxBytes: nativeOriginCacheBytes, maxEntry: nativeOriginEntryBytes}

type cachedToolCall struct {
	Origin string
	Native map[string]any
	Client map[string]any
	// Strict is the rc2 scope the matched (or contradicted) delivery was made
	// under; history_conflict: reject refuses only within that same scope.
	Strict string
}

// replayCacheTarget says where a write goes and how a different earlier
// record for the same call_id is treated.
type replayCacheTarget struct {
	scope  string
	strict string
	mode   string
}

func replayTarget(scope string) replayCacheTarget {
	return replayCacheTarget{scope: scope, mode: historyConflictReplay}
}

func replayTargetFor(source map[string]any) replayCacheTarget {
	return replayCacheTarget{scope: conversationScope(source), strict: strictReplayScopeFromSource(source),
		mode: relayOptionsFrom(source).HistoryConflict}
}

func (t replayCacheTarget) rejects() bool { return t.mode == historyConflictReject }

// rememberNativeCall is h22's write: record a transport call under scope.
// Later writes replace earlier ones. Used for a run_officejs call met in
// client history (h22) and by older fixtures.
func rememberNativeCall(scope string, item map[string]any) {
	storeRelayRecord(replayTarget(scope), item, nil, "unverified")
}

// rememberedNativeCall is h22's lookup: the transport call recorded for
// callID in this scope, whatever the client now sends under that call_id.
func rememberedNativeCall(scope, callID string) map[string]any {
	entry, found := nativeCallCache.lookup(scope, callID)
	// h22 checks the name only.
	if !found || entry.conflict || !isTransportName(stringValue(entry.item["name"])) {
		return nil
	}
	return cloneObject(entry.item)
}

// relayCallMarked reports a live marker for callID in scope: a relay call
// whose record was too large to keep.
func relayCallMarked(scope, callID string) bool {
	entry, found := nativeCallCache.lookup(scope, callID)
	return found && entry.marker
}

func rememberRelayCall(scope string, native, client map[string]any) {
	storeRelayRecord(replayTarget(scope), native, client, "relay")
}

// rememberRelayCallFor records a relay call the client has been handed,
// under the request's frozen scopes and history_conflict mode.
func rememberRelayCallFor(source map[string]any, native, client map[string]any) {
	if storeRelayRecord(replayTargetFor(source), native, client, "relay") {
		noteRelayHistory(source, "relay_record_overwritten")
	}
}

func rememberDeliveredNativeCall(scope string, item map[string]any) {
	storeNativeRecord(replayTarget(scope), item)
}

func rememberDeliveredNativeCallFor(source map[string]any, item map[string]any) {
	storeNativeRecord(replayTargetFor(source), item)
}

// storeRelayRecord reports whether it replaced a different delivered call
// (replay mode only; reject mode leaves a tombstone instead).
func storeRelayRecord(target replayCacheTarget, native, client map[string]any, origin string) bool {
	// h22 recorded anything named run_officejs; only such a record is ever
	// read back.
	if !isTransportName(stringValue(native["name"])) {
		return false
	}
	callID := stringValue(native["call_id"])
	if target.scope == "" || callID == "" {
		return false
	}
	reject := target.rejects()
	// rc2 (reject): ciphertext is never recorded. h22 (replay) recorded every
	// transport call it delivered; Basis Points' own run_officejs items carry
	// no client ciphertext.
	if reject && (isOpaqueToolHistory(native) || isOpaqueToolHistory(client)) {
		return false
	}
	fingerprint := ""
	if client != nil {
		if value, valid := clientCallFingerprint(client); valid && stringValue(client["call_id"]) == callID {
			fingerprint = value
		} else if reject {
			// rc2: a delivery that cannot be compared is not recorded. h22
			// replays the transport call regardless.
			return false
		}
	}
	// Of the delivered client call only what a later comparison needs is
	// kept: its fingerprint, item id and namespace. Replay sends item.
	entry := nativeCallEntry{item: cloneObject(native), client: deliveredCallStub(client), fingerprint: fingerprint,
		origin: origin, strict: target.strict}
	return nativeCallCache.put(nativeCallCacheKey(target.scope, callID), entry, reject)
}

func deliveredCallStub(client map[string]any) map[string]any {
	if client == nil {
		return nil
	}
	stub := map[string]any{}
	for _, key := range []string{"id", "namespace"} {
		if value, present := client[key]; present {
			stub[key] = cloneValue(value)
		}
	}
	return stub
}

// recordWeight approximates the memory a record holds: its strings once,
// not re-escaped as JSON.
func recordWeight(objects ...map[string]any) int {
	total := 0
	for _, object := range objects {
		for key, value := range object {
			total += len(key)
			if text, ok := value.(string); ok {
				total += len(text)
			} else {
				total += len(jsonBytes(value))
			}
		}
	}
	return total
}

func storeNativeRecord(target replayCacheTarget, item map[string]any) {
	if isTransportCall(item) {
		return
	}
	callID := stringValue(item["call_id"])
	if target.scope == "" || callID == "" || isOpaqueToolHistory(item) {
		return
	}
	fingerprint, valid := clientCallFingerprint(item)
	if !valid {
		return
	}
	// A native record is the delivered call itself; nothing is replayed from
	// it, so it is kept once.
	entry := nativeCallEntry{client: cloneObject(item), fingerprint: fingerprint,
		origin: "native", strict: target.strict}
	nativeOriginCache.put(nativeCallCacheKey(target.scope, callID), entry, target.rejects())
}

// put stores entry under key. replay (h22): the later write wins. reject
// (rc2): a different delivery of the same call_id within the same strict
// scope leaves a tombstone, so neither operation is replayed or used to
// answer an output. Re-writing a key keeps its place in the FIFO and
// restarts its lifetime, as in h22.
//
// A record larger than the pool's per-entry limit is not kept, and neither is
// the earlier record under its key: in h22 the later write replaced it, so
// keeping it would replay a call the model has since superseded (review
// R1/EH-5). A reject-mode tombstone is the one exception -- it holds no
// argument data, and it is exactly what a successful write would leave. In
// the relay pool a marker takes the record's place (markOversized): the
// later write still wins, and the cold path still knows a relay call.
func (p *replayPool) put(key string, entry nativeCallEntry, reject bool) bool {
	entry.storedAt = time.Now()
	entry.weight = recordWeight(entry.item, entry.client) + len(key)
	replaced := false
	p.Lock()
	defer p.Unlock()
	if old, exists := p.items[key]; exists && time.Since(old.storedAt) > nativeCallCacheTTL {
		p.removeLocked(key)
	}
	old, exists := p.items[key]
	if exists {
		if reject {
			rc2Conflict := old.conflict || (old.fingerprint != "" &&
				(old.fingerprint != entry.fingerprint || old.origin != entry.origin || conflictingOptionalID(old.client, entry.client)))
			if rc2Conflict && old.strict == entry.strict {
				// rc2: preserve a tombstone, not the old or new argument data.
				entry = nativeCallEntry{origin: "conflict", conflict: true, strict: entry.strict, storedAt: entry.storedAt, weight: len(key)}
				countRelayEvent("history_record_tombstoned")
			} else if rc2Conflict {
				// Made under another strict scope: rc2 kept the two apart.
				replaced = true
			}
		} else if old.fingerprint != "" && entry.fingerprint != "" &&
			(old.fingerprint != entry.fingerprint || conflictingOptionalID(old.client, entry.client)) {
			replaced = true
		}
		if replaced {
			countRelayEvent("history_record_overwritten")
		}
	}
	if entry.weight > p.maxEntry {
		countRelayEvent("replay_record_too_large:" + p.name)
		if !p.markOversized {
			if exists {
				p.removeLocked(key)
			}
			return replaced
		}
		entry = nativeCallEntry{origin: entry.origin, strict: entry.strict, marker: true,
			storedAt: entry.storedAt, weight: len(key) + len(entry.origin) + len(entry.strict)}
	}
	if exists {
		p.bytes -= old.weight
	} else {
		p.order = append(p.order, key)
	}
	p.items[key] = entry
	p.bytes += entry.weight
	for len(p.items) > p.maxEntries || p.bytes > p.maxBytes {
		if len(p.items) <= p.maxEntries {
			// The byte budget, not h22's entry count, is evicting: visible so
			// the budget can be checked against production record sizes.
			countRelayEvent("replay_byte_eviction:" + p.name)
		}
		// The record just written is never its own victim. A re-written key
		// keeps its (possibly oldest) FIFO place; if its new size is what
		// overflows the budget, the next oldest record goes instead. It fits
		// on its own: maxEntry is below maxBytes.
		if p.order[0] == key && len(p.order) == 1 {
			break // only the new record is left
		}
		var oldest string
		if p.order[0] != key {
			oldest, p.order = p.order[0], p.order[1:]
		} else {
			oldest = p.order[1]
			p.order = append(p.order[:1], p.order[2:]...)
		}
		if old, exists := p.items[oldest]; exists {
			p.bytes -= old.weight
			delete(p.items, oldest)
		}
	}
	return replaced
}

func (p *replayPool) lookup(scope, callID string) (nativeCallEntry, bool) {
	if scope == "" || callID == "" {
		return nativeCallEntry{}, false
	}
	key := nativeCallCacheKey(scope, callID)
	p.Lock()
	defer p.Unlock()
	entry, present := p.items[key]
	if !present {
		return nativeCallEntry{}, false
	}
	if time.Since(entry.storedAt) > nativeCallCacheTTL {
		p.removeLocked(key)
		return nativeCallEntry{}, false
	}
	// Stored maps are immutable; every public result is cloned.
	return entry, true
}

func (p *replayPool) removeLocked(key string) {
	if entry, exists := p.items[key]; exists {
		p.bytes -= entry.weight
		delete(p.items, key)
	}
	for i, old := range p.order {
		if old == key {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
}

// cachedEntry is the provenance record for callID: a comparable relay record
// first, otherwise the native record.
func cachedEntry(scope, callID string) (nativeCallEntry, bool) {
	entry, found := nativeCallCache.lookup(scope, callID)
	if found && (entry.fingerprint != "" || entry.conflict) {
		return entry, true
	}
	if native, nativeFound := nativeOriginCache.lookup(scope, callID); nativeFound {
		return native, true
	}
	return entry, found
}

// cachedCallForHistory compares a complete current client call with what was
// delivered. Missing payload, invalid JSON or a bounded comparison failure is
// "unverified", never a claim that arguments changed. Ciphertext is not
// decoded or rewritten. Used by history_conflict: reject, by relay_protocol:
// auto, and under replay only to log a mismatch.
func cachedCallForHistory(scope string, current map[string]any) (cachedToolCall, string) {
	if isOpaqueToolHistory(current) {
		return cachedToolCall{}, "opaque"
	}
	currentFingerprint, comparable := clientCallFingerprint(current)
	if !comparable {
		return cachedToolCall{}, "unverified"
	}
	entry, found := cachedEntry(scope, stringValue(current["call_id"]))
	if !found || entry.fingerprint == "" && !entry.conflict {
		return cachedToolCall{}, "miss"
	}
	if entry.conflict {
		return cachedToolCall{Strict: entry.strict}, "conflict"
	}
	matched := cloneObject(current)
	fingerprint := currentFingerprint
	// Omitted namespace can be restored only from this exact delivered call,
	// never from a shortened tool name or an ambiguous global namespace map.
	if _, present := matched["namespace"]; !present {
		if namespace, present := entry.client["namespace"]; present {
			matched["namespace"] = namespace
			fingerprint, comparable = clientCallFingerprint(matched)
		}
	}
	if !comparable {
		return cachedToolCall{}, "unverified"
	}
	if fingerprint != entry.fingerprint || conflictingOptionalID(entry.client, current) {
		return cachedToolCall{Strict: entry.strict}, "conflict"
	}
	return cachedToolCall{Origin: entry.origin, Native: cloneObject(entry.item), Client: matched, Strict: entry.strict}, "hit"
}

func cachedCallForOutput(scope, callID string) (cachedToolCall, bool) {
	entry, found := cachedEntry(scope, callID)
	if !found || entry.conflict || entry.fingerprint == "" || (entry.origin != "relay" && entry.origin != "native") {
		return cachedToolCall{}, false
	}
	return cachedToolCall{Origin: entry.origin, Native: cloneObject(entry.item), Client: cloneObject(entry.client), Strict: entry.strict}, true
}

func conflictingOptionalID(previous, current map[string]any) bool {
	oldID, oldOK := previous["id"].(string)
	newID, newOK := current["id"].(string)
	return oldOK && newOK && oldID != "" && newID != "" && oldID != newID
}

func isOpaqueToolHistory(item map[string]any) bool {
	for key, value := range item {
		if strings.HasPrefix(key, "encrypted_") {
			switch v := value.(type) {
			case nil:
			case string:
				if v != "" {
					return true
				}
			case []any:
				if len(v) > 0 {
					return true
				}
			case []string:
				if len(v) > 0 {
					return true
				}
			default:
				return true
			}
		}
	}
	for _, field := range []string{"content", "output"} {
		parts, _ := item[field].([]any)
		for _, value := range parts {
			part := objectValue(value)
			if strings.HasPrefix(stringValue(part["type"]), "encrypted_") {
				return true
			}
			if _, exists := part["encrypted_content"]; exists {
				return true
			}
		}
	}
	return false
}

// Logical identity excludes optional item.id. A supplied different item.id is
// checked separately. Function values use exact JSON number/value semantics;
// custom input is raw bytes. Length-delimited type tags prevent string/number,
// object/array and object-property collisions in the fingerprint encoding.
func clientCallFingerprint(item map[string]any) (string, bool) {
	if item == nil || isOpaqueToolHistory(item) {
		return "", false
	}
	kind, kindOK := item["type"].(string)
	name, nameOK := item["name"].(string)
	callID, idOK := item["call_id"].(string)
	if !kindOK || !nameOK || !idOK || (kind != "function_call" && kind != "custom_tool_call") || name == "" || callID == "" || len(name)+len(callID) > 4096 {
		return "", false
	}
	if strings.TrimSpace(name) != name || strings.TrimSpace(callID) != callID {
		return "", false
	}
	namespace := ""
	if value, present := item["namespace"]; present {
		var ok bool
		namespace, ok = value.(string)
		if !ok || len(namespace) > 1024 || strings.TrimSpace(namespace) != namespace {
			return "", false
		}
	}
	if value, present := item["id"]; present && value != nil {
		if id, ok := value.(string); !ok || len(id) > 1024 || strings.TrimSpace(id) != id {
			return "", false
		}
	}
	h := sha256.New()
	for _, value := range []string{"client-call/v2", kind, callID, name, namespace} {
		fingerprintText(h, value)
	}
	if kind == "custom_tool_call" {
		input, ok := item["input"].(string)
		if !ok || len(input) > clientCallFingerprintBytes {
			return "", false
		}
		fingerprintText(h, input)
	} else {
		arguments, ok := item["arguments"].(string)
		if !ok || len(arguments) > clientCallFingerprintBytes || !fingerprintUnicodeUnambiguous(arguments) {
			return "", false
		}
		decoder := json.NewDecoder(strings.NewReader(arguments))
		decoder.UseNumber()
		budget := 65536
		value, ok := fingerprintDecodeJSON(decoder, 0, &budget)
		if !ok {
			return "", false
		}
		if _, err := decoder.Token(); err != io.EOF {
			return "", false
		}
		if !fingerprintJSON(h, value) {
			return "", false
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// encoding/json replaces malformed UTF-8 and unpaired UTF-16 escapes with
// U+FFFD. Treat those spellings as unverified instead of equating different
// client strings. Well-formed escaped surrogate pairs and UTF-8 text agree.
func fingerprintUnicodeUnambiguous(raw string) bool {
	if !utf8.ValidString(raw) {
		return false
	}
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		code, err := strconv.ParseUint(raw[i+1:i+5], 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(raw[i+3:i+7], 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inString
}

func fingerprintText(h hash.Hash, value string) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value)) + ":"))
	_, _ = h.Write([]byte(value))
}

func fingerprintDecodeJSON(decoder *json.Decoder, depth int, budget *int) (any, bool) {
	if depth > 128 || *budget <= 0 {
		return nil, false
	}
	*budget--
	token, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	switch delim := token.(type) {
	case json.Delim:
		switch delim {
		case '{':
			result := map[string]any{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return nil, false
				}
				if _, duplicate := result[name]; duplicate {
					return nil, false
				}
				value, ok := fingerprintDecodeJSON(decoder, depth+1, budget)
				if !ok {
					return nil, false
				}
				result[name] = value
			}
			end, err := decoder.Token()
			return result, err == nil && end == json.Delim('}')
		case '[':
			result := []any{}
			for decoder.More() {
				value, ok := fingerprintDecodeJSON(decoder, depth+1, budget)
				if !ok {
					return nil, false
				}
				result = append(result, value)
			}
			end, err := decoder.Token()
			return result, err == nil && end == json.Delim(']')
		}
		return nil, false
	default:
		return token, true
	}
}

func fingerprintJSON(h hash.Hash, value any) bool {
	switch v := value.(type) {
	case nil:
		fingerprintText(h, "null")
	case bool:
		fingerprintText(h, "bool")
		fingerprintText(h, strconv.FormatBool(v))
	case string:
		fingerprintText(h, "string")
		fingerprintText(h, v)
	case json.Number:
		canonical, ok := canonicalEnumNumber(v)
		if !ok {
			return false
		}
		fingerprintText(h, "number")
		fingerprintText(h, canonical)
	case []any:
		fingerprintText(h, "array")
		fingerprintText(h, strconv.Itoa(len(v)))
		for _, child := range v {
			if !fingerprintJSON(h, child) {
				return false
			}
		}
	case map[string]any:
		fingerprintText(h, "object")
		fingerprintText(h, strconv.Itoa(len(v)))
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fingerprintText(h, key)
			if !fingerprintJSON(h, v[key]) {
				return false
			}
		}
	default:
		return false
	}
	return true
}
