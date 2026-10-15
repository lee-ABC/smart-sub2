package basispoints

// tool_policy_checks (H25-SPEC S2.2).
//
// rc2 added checks that h22 never made: how many client tool calls one
// response may carry (parallel_tool_calls:false, a forced tool_choice), whether
// a required/forced tool_choice was honoured, whether tool_choice permits a
// native (Lite) call, whether a native call's argument events and its terminal
// snapshot agree with what was streamed, and whether a relay call's upstream
// transport identity changed between its done event and the terminal snapshot.
// The plugin never forwards parallel_tool_calls or tool_choice to Basis Points,
// so each of these is a claim about model output that nothing upstream was
// asked to honour, and h22 delivered every such response.
//
//   - log (default, = h22): the finding is logged under a fixed label and the
//     response is delivered exactly as h22 delivered it.
//   - enforce (rc2): the finding rejects the response -- but only while no
//     complete client tool call has been delivered. Once one has, the client
//     may already be executing it; failing the stream then cannot undo that,
//     it only turns a usable answer into an error, so the finding is logged.
//
// Checks h22 already made (an unknown or undeclared relay tool, a relay call
// that changes between done and terminal, duplicate identities, truncated
// transport calls, ...) are not policy checks and always reject, as in h22.

// toolPolicyRejection is the decision for one finding. delivered says whether a
// complete client tool call has already been written to the client.
func toolPolicyRejection(source map[string]any, delivered bool, reason string) error {
	enforce := relayOptionsFrom(source).PolicyChecks == toolPolicyChecksEnforce
	if enforce && !delivered {
		return toolReject(reason)
	}
	mode := toolPolicyChecksLog
	if enforce {
		mode = "enforce_after_delivery"
	}
	noteToolPolicy(source, reason, mode)
	return nil
}

// noteToolPolicy logs a finding once per request and reason. The label is
// code-owned; no tool name, argument or identifier is ever logged.
func noteToolPolicy(source map[string]any, reason, mode string) {
	if !toolRejectReasons[reason] {
		reason = "unspecified"
	}
	notes, _ := source[relayDecodeNotesKey].(*relayDecodeNotes)
	if notes == nil {
		countRelayEvent("tool_policy_logged")
		return
	}
	// A note must never change a decision, even if the logger panics.
	defer func() { _ = recover() }()
	key := "policy\x00" + mode + "\x00" + reason
	notes.mu.Lock()
	if notes.seen[key] || len(notes.seen) >= relayDecodeNotesPerRequest {
		notes.mu.Unlock()
		return
	}
	notes.seen[key] = true
	notes.mu.Unlock()
	countRelayEvent("tool_policy_logged")
	countRelayEvent("tool_policy_logged:" + reason)
	if notes.policy != nil {
		notes.policy(reason, mode)
	}
}

// sourceToolPolicy is the policy for a whole response decoded at once (the
// non-streaming path, or a decode outside any stream): nothing of it has
// reached the client yet.
func sourceToolPolicy(source map[string]any) func(string) error {
	return func(reason string) error { return toolPolicyRejection(source, false, reason) }
}
