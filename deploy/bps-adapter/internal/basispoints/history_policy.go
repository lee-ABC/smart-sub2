package basispoints

// history_conflict (H25-SPEC S3.2).
//
//   - replay (default, = h22): a relay call whose call_id is in the replay
//     cache is replayed as the transport call the model emitted, whatever the
//     client now sends under that call_id; every other call is sent as it
//     arrives (h22's cold wrapping and namespace restoration). A difference
//     between the history and what was delivered is logged, never refused.
//   - reject (rc2): a history call that contradicts a call delivered under the
//     same strict scope (authenticated caller + credential + explicit session,
//     rc2's scope) is refused with 400 history_conflict before anything is
//     sent upstream. A contradiction outside that scope is logged and the call
//     takes h22's cold path, so a weak scope can never refuse a request.
func validateToolHistoryBeforeReplay(source map[string]any, scope string) error {
	if relayOptionsFrom(source).HistoryConflict != historyConflictReject {
		return nil
	}
	strict := strictReplayScopeFromSource(source)
	items, _ := source["input"].([]any)
	unscoped := false
	for _, raw := range items {
		item := objectValue(raw)
		if item == nil || isTransportCall(item) || isOpaqueToolHistory(item) {
			continue
		}
		kind := stringValue(item["type"])
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		cached, state := cachedCallForHistory(scope, item)
		if state != "conflict" {
			continue
		}
		if strict != "" && cached.Strict == strict {
			noteRelayHistory(source, "history_conflict_rejected")
			return fail(400, "history_conflict", "client tool history conflicts with a previously delivered call; retain the original call identity and arguments")
		}
		unscoped = true
	}
	if unscoped {
		noteRelayHistory(source, "history_conflict_unscoped")
	}
	return nil
}

// relayHistoryReasons are the only labels a history note may carry.
var relayHistoryReasons = map[string]bool{
	// replay: a cached relay call was replayed although the client's history
	// differs from the call it was handed.
	"relay_history_mismatch": true,
	// replay: a native call in the history differs from the one delivered;
	// it is sent as the client sends it.
	"native_history_mismatch": true,
	// replay: one call_id was delivered twice with different content in one
	// scope; the later delivery is what replays.
	"relay_record_overwritten": true,
	// reject: refused with 400.
	"history_conflict_rejected": true,
	// reject: a contradiction outside the strict scope; not refused.
	"history_conflict_unscoped": true,
}

// noteRelayHistory counts and logs a history note once per request and
// reason. The label is code-owned; no tool name, argument or identifier is
// ever logged.
func noteRelayHistory(source map[string]any, reason string) {
	if !relayHistoryReasons[reason] {
		return
	}
	notes, _ := source[relayDecodeNotesKey].(*relayDecodeNotes)
	if notes == nil {
		countRelayEvent("history_note:" + reason)
		return
	}
	// A note must never change a decision, even if the logger panics.
	defer func() { _ = recover() }()
	key := "history\x00" + reason
	notes.mu.Lock()
	if notes.seen[key] || len(notes.seen) >= relayDecodeNotesPerRequest {
		notes.mu.Unlock()
		return
	}
	notes.seen[key] = true
	notes.mu.Unlock()
	countRelayEvent("history_note:" + reason)
	if notes.history != nil {
		notes.history(reason)
	}
}
