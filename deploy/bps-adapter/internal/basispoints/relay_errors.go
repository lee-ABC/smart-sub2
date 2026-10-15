package basispoints

import "errors"

// Stream failure wording (H25-SPEC E5).
//
// A failure in the middle of a stream reaches the main site (sub2api v0.2.14,
// account 6202/6477: platform openai, type apikey, pool_mode) as one string:
// host.stream.close carries no status, and CPA wraps the string as the message
// of an in-band failure event -- response.failed for Codex clients, a bare
// `error` event for everybody else -- with code internal_server_error and type
// server_error. Whether the main site then retries on another account, and
// whether it does anything to the account, depends on that text alone
// (backend/internal/service/, v0.2.14):
//
//   - It considers retrying only while it has written no output to its client
//     (openai_gateway_response_handling.go:552, 576-589).
//   - response.failed: retried unless the text holds one of the markers
//     "invalid_request", "content_policy", "policy", "safety",
//     "high-risk cyber", "not allowed", "violat"
//     (openai_gateway_passthrough.go:1547-1597, list at 1582-1590).
//   - error event: retried only on an explicit status / capacity signal, or if
//     the text holds "temporary", "try again" or "please retry"
//     (openai_gateway_passthrough.go:1599-1624, the words at 1621-1623).
//     Without them a zero-output failure went straight to a non-Codex client.
//   - Account side effects follow the semantic status, which for this text
//     stays 502 (openai_gateway_passthrough.go:1390-1423: "rate_limit" -> 429,
//     "authentication"/"unauthorized"/"invalid_api_key" -> 401,
//     "permission"/"forbidden"/"access denied" -> 403; context-window,
//     capacity and access-state checks are separate). A 502 has none:
//     handleOpenAIStreamTerminalAccountSideEffects acts only on 401/403/429/
//     529 (1626-1660), and 502 is not a pool-mode same-account retry status
//     (account.go:1181, default 401/403/429), so the retry goes to another
//     account (handler/openai_gateway_handler.go:893-936). The handler reports
//     the same scheduler failure with or without a retry (903 / 966). The one
//     account counter only a retry feeds is the opt-in API-key health breaker
//     (openai_apikey_health_breaker.go:22-46, setting
//     openai_apikey_health_breaker_settings, default off), which already
//     counted every Codex zero-output failure under h22.
//
// So the hint below changes exactly one thing: a zero-output failure seen by a
// non-Codex client becomes the same account switch a Codex client already
// got in h22. The words and every message this plugin can close a stream with
// are checked against all of these rules by TestH25S2CloseMessagesAgainstMainSiteKeywords.
//
// Only a failure before any output: once the client has text, reasoning or a
// tool call, a retry would repeat it. A client that hung up (499) is not
// retried either.
const retryableErrorHint = "; temporary relay failure, please retry"

// streamCloseMessage is the string host.stream.close carries for err.
// zeroOutput: nothing but response.created / response.in_progress reached the
// client before the failure.
func streamCloseMessage(err error, options relayOptions, zeroOutput bool) string {
	message := streamCloseReason(err) + safeError(err)
	if options.RetryableErrorHint && zeroOutput && retryableFailure(err) {
		message += retryableErrorHint
		countRelayEvent("retryable_error_hint")
	}
	return message
}

func retryableFailure(err error) bool {
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) && coded.StatusCode() == 499 {
		return false
	}
	return true
}
