package basispoints

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const replayScopeSourceKey = "\x00cpa_trusted_replay_scope"

// Only local Go code can create this marker; a JSON body cannot produce this
// type, and it serialises as null if a source map is ever marshalled. A marker
// holding an empty key is authoritative: no body or fingerprint fallback.
//
// key is the replay cache scope (H25-SPEC S3.1): h22's conversationScope --
// prompt_cache_key / session key, else the first input item -- prefixed by the
// authenticated caller_scope when the host supplies one. Credentials are not
// part of it: the relay cache holds plaintext run_officejs calls that are not
// bound to any credential, and h22 kept a conversation's replay across a
// credential switch.
//
// strict is rc2's explicit-identity scope (caller + credential + explicit
// session). It never selects cache entries; history_conflict: reject only
// refuses a request when the delivery it contradicts was made under this same
// strict scope, which is exactly when rc2 refused it.
type trustedReplayScope struct {
	key    string
	strict string
}

func (trustedReplayScope) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

func replayScopeFromSource(source map[string]any) (string, bool) {
	scope, present := source[replayScopeSourceKey].(trustedReplayScope)
	return scope.key, present
}

func strictReplayScopeFromSource(source map[string]any) string {
	scope, _ := source[replayScopeSourceKey].(trustedReplayScope)
	return scope.strict
}

// withReplayScope pins the cache scope with no strict identity (helpers and
// tests); requests go through withFrozenReplayScope.
func withReplayScope(source map[string]any, scope string) map[string]any {
	return withReplayScopes(source, scope, "")
}

func withReplayScopes(source map[string]any, scope, strict string) map[string]any {
	copy := make(map[string]any, len(source)+1)
	for key, value := range source {
		copy[key] = value
	}
	copy[replayScopeSourceKey] = trustedReplayScope{key: scope, strict: strict}
	return copy
}

func withFrozenReplayScope(source map[string]any, request ExecutorRequest) map[string]any {
	return withReplayScopes(source, request.replayCacheScope, request.replayStrictScope)
}

// h22ConversationScope is h22's conversationScope, byte for byte.
func h22ConversationScope(source map[string]any) string {
	if key := explicitConversationKey(source); key != "" {
		return shortHash(key)
	}
	if items, ok := source["input"].([]any); ok {
		return conversationFingerprint(items)
	}
	return ""
}

// h22ConversationScopeSource names what h22 keyed the conversation on. Only
// these fixed labels are ever counted or logged.
func h22ConversationScopeSource(source map[string]any) string {
	for _, key := range []string{"prompt_cache_key", "promptCacheKey"} {
		if stringValue(source[key]) != "" {
			return "prompt_cache_key"
		}
	}
	for _, key := range []string{"session_id", "sessionId"} {
		if stringValue(source[key]) != "" {
			return "session_id"
		}
	}
	if metadata := objectValue(source["client_metadata"]); metadata != nil {
		for _, key := range []string{"session_id", "sessionId"} {
			if stringValue(metadata[key]) != "" {
				return "client_metadata"
			}
		}
	}
	if _, ok := source["input"].([]any); ok {
		return "first_item"
	}
	return "none"
}

// deriveReplayScope returns the replay cache scope and the label of what it
// was keyed on. It is non-empty exactly when h22's scope was, so replay hits
// and misses, the namespace memory and turn ids behave as in h22; within one
// caller_scope the key is h22's key, so two turns share entries exactly when
// they did in h22. A caller_scope only ever separates callers further.
func deriveReplayScope(request ExecutorRequest, source map[string]any) (string, string) {
	conversation := h22ConversationScope(source)
	reason := h22ConversationScopeSource(source)
	if conversation == "" {
		return "", reason
	}
	caller, ok := replayIdentityPart(request.Metadata["caller_scope"])
	if !ok {
		return conversation, reason
	}
	encoded := jsonBytes([]string{"cpa-replay-scope/v3", caller, conversation})
	digest := sha256.Sum256(encoded)
	return "replay-v3:" + hex.EncodeToString(digest[:]), reason
}

// deriveStrictReplayScope is rc2's deriveReplayScope, unchanged: an
// authenticated caller, the upstream credential and an explicit stable
// conversation signal, or nothing. It only gates history_conflict: reject.
func deriveStrictReplayScope(request ExecutorRequest, source map[string]any) (string, string) {
	caller, ok := replayIdentityPart(request.Metadata["caller_scope"])
	if !ok {
		return "", "missing_caller"
	}
	auth, ok := replayIdentityPart(request.AuthID)
	if !ok {
		return "", "missing_credential"
	}
	provider, ok := replayIdentityPart(request.AuthProvider)
	if !ok {
		return "", "missing_provider"
	}
	var session string
	conflict := false
	add := func(raw any, present bool) {
		if !present {
			return
		}
		value, valid := replayIdentityPart(raw)
		if !valid || (session != "" && value != session) {
			conflict = true
			return
		}
		session = value
	}
	for _, name := range []string{"Session-Id", "Session_id", "Conversation_id", "X-Session-Id", "X-Conversation-Id", "X-Thread-Id", "X-Claude-Code-Session-Id"} {
		for key, values := range request.Headers {
			if strings.EqualFold(key, name) {
				if len(values) == 0 {
					conflict = true
				}
				for _, value := range values {
					add(value, true)
				}
			}
		}
	}
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "conversationId", "thread_id", "threadId"} {
		value, present := source[key]
		add(value, present)
	}
	if metadata := objectValue(source["client_metadata"]); metadata != nil {
		for _, key := range []string{"session_id", "sessionId"} {
			value, present := metadata[key]
			add(value, present)
		}
	}
	if conflict {
		return "", "conflicting_session"
	}
	if session == "" {
		// A host-owned execution session is useful for a long-lived websocket,
		// but it does not override contradictory explicit client identities.
		if value, valid := replayIdentityPart(request.Metadata["execution_session_id"]); valid {
			session = value
		}
	}
	if session == "" {
		return "", "missing_session"
	}
	encoded := jsonBytes([]string{"cpa-replay-scope/v2", caller, provider, auth, session})
	digest := sha256.Sum256(encoded)
	return "replay-v2:" + hex.EncodeToString(digest[:]), "scoped"
}

func replayIdentityPart(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || len(text) == 0 || len(text) > 512 || strings.TrimSpace(text) != text || strings.ContainsAny(text, "\x00\r\n") {
		return "", false
	}
	return text, true
}

// freezeReplayScopeFromSource computes both scopes once per request, from the
// untouched request body, before anything rewrites it. It also keeps the
// fixed labels of what the scope was keyed on; countRelayRequest counts them
// once the request is admitted, so a request refused at the concurrency gate
// is not counted (review R5).
func freezeReplayScopeFromSource(request *ExecutorRequest, source map[string]any) {
	if request.replayScopeFrozen {
		return
	}
	var reason string
	request.replayCacheScope, reason = deriveReplayScope(*request, source)
	request.replayStrictScope, _ = deriveStrictReplayScope(*request, source)
	request.replayScopeFrozen = true
	if request.replayCacheScope == "" {
		reason = "unscoped"
	}
	request.replayScopeSource = reason
	if _, ok := replayIdentityPart(request.Metadata["caller_scope"]); ok {
		request.replayScopeCaller = "present"
	} else {
		request.replayScopeCaller = "absent"
	}
}
