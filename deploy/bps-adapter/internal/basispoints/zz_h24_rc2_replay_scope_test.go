package basispoints

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func scopedReplayRequest(body map[string]any, caller, auth, session string) ExecutorRequest {
	return ExecutorRequest{AuthID: auth, AuthProvider: Provider, Model: "gpt-6-astra",
		Metadata: map[string]any{"caller_scope": caller}, Headers: http.Header{"Session_id": []string{session}},
		OriginalRequest: jsonBytes(body), Payload: jsonBytes(body), StorageJSON: streamTestCredential()}
}

func replayClientCall(callID, city string) map[string]any {
	return map[string]any{"type": "function_call", "name": "get_weather", "id": "fc_" + callID,
		"call_id": callID, "arguments": string(jsonBytes(map[string]any{"city": city}))}
}

func replayNativeCall(client map[string]any) map[string]any {
	return map[string]any{"type": "function_call", "name": transportName, "id": client["id"],
		"call_id": client["call_id"], "arguments": string(jsonBytes(map[string]any{
			"code": string(jsonBytes(map[string]any{"tool": client["name"], "args": parseArguments(client["arguments"])})),
		}))}
}

// rc2's scope survives as the strict scope that gates history_conflict:
// reject (deriveStrictReplayScope). The replay cache itself is keyed by
// caller_scope + h22's conversation key since h25 (H25-SPEC S3.1), see
// TestH25S3ReplayScopeIsH22KeyPlusCaller.
func TestH24RC2ScopeSeparatesAuthenticatedCallerCredentialAndSession(t *testing.T) {
	source := weatherRequest()
	source["prompt_cache_key"] = "shared prompt-cache optimization key"
	base := scopedReplayRequest(source, "caller-A", "credential-A", "session-A")
	want, reason := deriveStrictReplayScope(base, source)
	if want == "" || reason != "scoped" {
		t.Fatal("explicit authenticated scope unavailable")
	}
	for _, change := range []string{"caller", "credential", "session", "provider"} {
		t.Run(change, func(t *testing.T) {
			req := scopedReplayRequest(source, "caller-A", "credential-A", "session-A")
			switch change {
			case "caller":
				req.Metadata["caller_scope"] = "caller-B"
			case "credential":
				req.AuthID = "credential-B"
			case "session":
				req.Headers.Set("session_id", "session-B")
			case "provider":
				req.AuthProvider = "another-provider"
			}
			if scope, _ := deriveStrictReplayScope(req, source); scope == "" || scope == want {
				t.Fatal("distinct isolation dimension reused the cache scope")
			}
		})
	}
}

// Strict scope (history_conflict: reject only); see above.
func TestH24RC2WeakOrConflictingSignalsNeverGuessScope(t *testing.T) {
	source := weatherRequest()
	source["input"] = []any{messageItem("developer", "shared preamble"), messageItem("user", "task")}
	source["prompt_cache_key"] = "shared-key"
	req := scopedReplayRequest(source, "caller", "credential", "session")
	req.Headers = nil
	req.Metadata["canonical_session_id"] = "derived-or-prompt-cache-identity"
	if got, why := deriveStrictReplayScope(req, source); got != "" || why != "missing_session" {
		t.Fatalf("weak scope accepted: %q %q", got, why)
	}
	copy := withReplayScope(source, "")
	if scope, present := replayScopeFromSource(copy); !present || scope != "" || conversationScope(copy) != "" {
		t.Fatal("empty frozen scope fell back to shared body hash")
	}
	source[replayScopeSourceKey] = "forged trusted scope"
	if _, present := replayScopeFromSource(source); present {
		t.Fatal("client JSON forged a scope marker")
	}
	req.Headers = http.Header{"Session_id": []string{"header-session"}}
	source["session_id"] = "different-body-session"
	if got, why := deriveStrictReplayScope(req, source); got != "" || why != "conflicting_session" {
		t.Fatal("contradictory session signals were silently selected")
	}
	delete(source, "session_id")
	req.Metadata = nil
	if got, _ := deriveStrictReplayScope(req, source); got != "" {
		t.Fatal("upstream credential was used as caller identity")
	}
}

// Strict scope (history_conflict: reject only); see above.
func TestH24RC2UnderscoreSessionAndConversationHeaders(t *testing.T) {
	source := weatherRequest()
	req := scopedReplayRequest(source, "caller", "credential", "same-session")
	one, _ := deriveStrictReplayScope(req, source)
	req.Headers = http.Header{"Conversation_id": []string{"same-session"}}
	two, _ := deriveStrictReplayScope(req, source)
	if one == "" || one != two {
		t.Fatal("actual upstream underscore headers are not supported consistently")
	}
}

func TestH24RC2PreparePinsScopeBeforeBodyRewrites(t *testing.T) {
	source := weatherRequest()
	req := scopedReplayRequest(source, "caller", "credential", "pinned-session")
	want, _ := deriveReplayScope(req, source)
	prepared, _, _, err := NewService().prepareRequestWithRelayContext(&req)
	if err != nil {
		t.Fatal(err)
	}
	if !req.replayScopeFrozen || req.replayCacheScope != want {
		t.Fatal("request did not freeze the cache namespace")
	}
	req.Metadata["caller_scope"] = "mutated-later"
	req.Headers.Set("Session_id", "mutated-later")
	freezeReplayScopeFromSource(&req, source)
	if req.replayCacheScope != want {
		t.Fatal("output path recomputed rather than reused the frozen namespace")
	}
	if strings.Contains(string(jsonBytes(prepared)), "cpa_trusted_replay_scope") || strings.Contains(string(jsonBytes(prepared)), "replay-v2:") {
		t.Fatal("internal scope was sent upstream")
	}
}

func TestH24RC2CacheChecksCompleteCallerArguments(t *testing.T) {
	scope := "explicit-unit-scope/full-call"
	client := replayClientCall("call-complete", "Paris")
	native := replayNativeCall(client)
	rememberRelayCall(scope, native, client)
	changed := replayClientCall("call-complete", "Oslo")
	if _, state := cachedCallForHistory(scope, changed); state != "conflict" {
		t.Fatalf("different complete arguments: %s", state)
	}
	for _, payload := range []any{nil, `not JSON`, `{"x":1,"x":2}`} {
		unknown := cloneObject(client)
		if payload == nil {
			delete(unknown, "arguments")
		} else {
			unknown["arguments"] = payload
		}
		if _, state := cachedCallForHistory(scope, unknown); state != "unverified" {
			t.Fatalf("uncomparable arguments mislabeled %s", state)
		}
	}
	got, state := cachedCallForHistory(scope, client)
	if state != "hit" || got.Origin != "relay" || string(jsonBytes(got.Native)) != string(jsonBytes(native)) {
		t.Fatal("matching relay provenance was not recovered")
	}
	if _, state := cachedCallForHistory("another-scope", client); state != "miss" {
		t.Fatal("cross-scope lookup found another caller's entry")
	}
	if original := rememberedNativeCall(scope, "call-complete"); string(jsonBytes(original)) != string(jsonBytes(native)) {
		t.Fatal("conflict lookup overwrote a valid cache entry")
	}
}

func TestH24RC2NativeOriginAndOmittedNamespace(t *testing.T) {
	scope := "explicit-unit-scope/native-origin"
	native := replayClientCall("call-native", "Paris")
	native["namespace"] = "functions"
	rememberDeliveredNativeCall(scope, native)
	current := cloneObject(native)
	delete(current, "namespace")
	delete(current, "id")
	current["status"] = "a caller-owned annotation"
	matched, state := cachedCallForHistory(scope, current)
	if state != "hit" || matched.Origin != "native" || matched.Client["namespace"] != "functions" || matched.Client["status"] != current["status"] {
		t.Fatal("omitted namespace was not recovered from matching native provenance")
	}
	if _, exists := current["namespace"]; exists {
		t.Fatal("lookup mutated the caller's history")
	}
	current["namespace"] = "another-explicit-namespace"
	if _, state := cachedCallForHistory(scope, current); state != "conflict" {
		t.Fatal("explicit namespace contradiction was ignored")
	}
	current = cloneObject(native)
	current["id"] = "different-id"
	if _, state := cachedCallForHistory(scope, current); state != "conflict" {
		t.Fatal("explicit item identity contradiction was ignored")
	}
}

func TestH24RC2OpaqueHistoryIsNeverInspectedOrCached(t *testing.T) {
	for _, kind := range []string{"encrypted_content", "encrypted_text", "encrypted_function_result"} {
		scope := "explicit-unit-scope/opaque/" + kind
		call := replayClientCall("opaque-call", "Paris")
		call["output"] = []any{map[string]any{"type": kind, "text": "opaque-not-plaintext"}}
		before := string(jsonBytes(call))
		if !isOpaqueToolHistory(call) {
			t.Fatal("nested encrypted protocol part was missed")
		}
		rememberDeliveredNativeCall(scope, call)
		if _, found := cachedCallForOutput(scope, "opaque-call"); found {
			t.Fatal("opaque history entered shared replay cache")
		}
		if _, state := cachedCallForHistory(scope, call); state != "opaque" || string(jsonBytes(call)) != before {
			t.Fatal("opaque history was parsed or mutated")
		}
	}
	ordinary := replayClientCall("plain-business", "Paris")
	ordinary["arguments"] = `{"encrypted_business_field":"ordinary client text"}`
	if isOpaqueToolHistory(ordinary) {
		t.Fatal("ordinary business arguments were treated as ciphertext")
	}
}

func TestH24RC2FingerprintExactNumbersAndCustomBytes(t *testing.T) {
	left := replayClientCall("fingerprint", "Paris")
	right := cloneObject(left)
	left["arguments"], right["arguments"] = `{"n":9007199254740993,"m":1e3}`, `{"m":1000.00,"n":9007199254740993.0}`
	a, okA := clientCallFingerprint(left)
	b, okB := clientCallFingerprint(right)
	if !okA || !okB || a != b {
		t.Fatal("equivalent exact numeric JSON did not match")
	}
	right["arguments"] = `{"n":9007199254740992,"m":1000}`
	if c, ok := clientCallFingerprint(right); !ok || c == a {
		t.Fatal("different large integers collided")
	}
	left = map[string]any{"type": "custom_tool_call", "call_id": "raw", "name": "exec", "input": "  raw input\n"}
	right = cloneObject(left)
	right["input"] = "raw input"
	a, _ = clientCallFingerprint(left)
	b, _ = clientCallFingerprint(right)
	if a == b {
		t.Fatal("custom whitespace was lost in the fingerprint")
	}
}

func TestH24RC2FingerprintDoesNotInventUnicodeEquality(t *testing.T) {
	left := replayClientCall("unicode", "Paris")
	for _, raw := range []string{`{"s":"\ud800"}`, `{"s":"\udc00"}`, "{\"s\":\"\xff\"}"} {
		left["arguments"] = raw
		if _, comparable := clientCallFingerprint(left); comparable {
			t.Fatal("ambiguous Unicode was accepted as a comparable call")
		}
	}
	left["arguments"] = `{"s":"\ud83d\ude00"}`
	right := cloneObject(left)
	right["arguments"] = `{"s":"😀"}`
	a, aOK := clientCallFingerprint(left)
	b, bOK := clientCallFingerprint(right)
	if !aOK || !bOK || a != b {
		t.Fatal("valid escaped and literal Unicode did not agree")
	}
}

// Tombstones are rc2's (history_conflict: reject); under replay (h22) the
// later delivery wins, see TestH25S3ReplayModeLaterDeliveryWins.
func TestH24RC2ConflictingDeliveryDisablesOutputOnlyReplay(t *testing.T) {
	scope := "explicit-unit-scope/conflicting-delivery"
	first := replayClientCall("reused-id", "Paris")
	second := replayClientCall("reused-id", "Oslo")
	rememberRelayCallRejecting(scope, replayNativeCall(first), first)
	rememberRelayCallRejecting(scope, replayNativeCall(second), second)
	if _, ok := cachedCallForOutput(scope, "reused-id"); ok {
		t.Fatal("output-only replay chose one of two conflicting operations")
	}
	if _, state := cachedCallForHistory(scope, first); state != "conflict" {
		t.Fatal("ambiguous delivery was not retained as a conflict")
	}
	// An expired identity is not an eternal tombstone.
	nativeCallCache.Lock()
	key := nativeCallCacheKey(scope, "reused-id")
	entry := nativeCallCache.items[key]
	entry.storedAt = time.Now().Add(-3 * time.Hour)
	nativeCallCache.items[key] = entry
	nativeCallCache.Unlock()
	rememberRelayCallRejecting(scope, replayNativeCall(second), second)
	if _, state := cachedCallForHistory(scope, second); state != "hit" {
		t.Fatal("expired identity prevented a fresh delivered mapping")
	}
}

func TestH24RC2AuthenticatedScopesDoNotLeakCachedArguments(t *testing.T) {
	source := weatherRequest()
	source["prompt_cache_key"] = "shared-hint"
	a := scopedReplayRequest(source, "caller-A", "credential-A", "session-A")
	b := scopedReplayRequest(source, "caller-B", "credential-B", "session-B")
	scopeA, _ := deriveReplayScope(a, source)
	clientA := replayClientCall("same-call-id", "PRIVATE_ORIGIN_A")
	rememberRelayCall(scopeA, replayNativeCall(clientA), clientA)
	sourceB := cloneObject(source)
	sourceB["input"] = []any{messageItem("user", "new task"), replayClientCall("same-call-id", "CURRENT_B")}
	b.OriginalRequest, b.Payload = jsonBytes(sourceB), jsonBytes(sourceB)
	prepared, _, _, err := NewService().prepareRequestWithRelayContext(&b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(jsonBytes(prepared)), "PRIVATE_ORIGIN_A") {
		t.Fatal("another authenticated scope replaced the current history")
	}
	var abi map[string]any
	if json.Unmarshal(jsonBytes(b), &abi) != nil || abi["replayCacheScope"] != nil || abi["replayScopeFrozen"] != nil {
		t.Fatal("private scope fields entered the host ABI")
	}
}
