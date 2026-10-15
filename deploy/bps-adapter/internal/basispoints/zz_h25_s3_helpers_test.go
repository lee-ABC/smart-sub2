package basispoints

// H25 S3 test helpers: rc2's history semantics are history_conflict: reject.

// translateInputItemsReject is translateInputItems under history_conflict:
// reject (rc2's provenance-checked history, with the S3.4 corrections).
func translateInputItemsReject(rawInput any, allowed map[string]toolSpec, scope string) []any {
	return translateInputItemsFor(rawInput, allowed, scope, wireOptions{historyConflict: historyConflictReject})
}

// rememberRelayCallRejecting records a delivered relay call the way a
// history_conflict: reject request does (rc2: a second, different delivery
// of the same call_id leaves a tombstone).
func rememberRelayCallRejecting(scope string, native, client map[string]any) {
	storeRelayRecord(replayCacheTarget{scope: scope, mode: historyConflictReject}, native, client, "relay")
}

func s3Reject(cfg *Config) { cfg.HistoryConflict = historyConflictReject }
