# Scheduled account test panic isolation

## Incident (2026-10-18)

The server40 port8890 container (`bps-opt2-20261017`, build98cc198) restarted every minute. A due protected OAuth account probe reached `prepareIntelligentTestProtection`, which wrote `session-id` into the nil Header of the background request. Because cron runs outside Gin's HTTP recovery middleware, the panic terminated the entire gateway. Calls from server87 during restart could receive ECONNREFUSED. The separate transport-fault policy can then temporarily exclude the upstream account for ten minutes; the health-protection switch does not control that policy.

## Scoped fix

- Construct a complete background probe request, with initialized headers and a valid local request path. This does not send another request or change the requested model.
- Defensively initialize a missing incoming Header in intelligent test protection, retaining existing session/thread/device identity rules.
- Recover panics at the background account-test boundary. Return an explicitly failed scheduled-test result with no partial response or raw panic text. The existing scheduler can persist failure and advance next_run_at, without treating it as successful recovery. Logs include account ID, panic type and stack, not the panic value.

No changes to account/group configuration, ten-minute transport cooldown, account health-protection rules, credential storage, BPS identities, model/reasoning selection, database schema, helper services or dependencies. Existing unrelated frontend lockfile changes are excluded.

## Validation

The protected OAuth background regression reproduced the exact nil Header stack before the fix. Regression coverage after the fix includes ordinary and protected probes, independent sessions, manually constructed nil Header requests, panic redaction, subsequent probe execution and failed scheduled-result persistence/next-run advancement. 72 related service tests pass. Production smoke checks must cover at least two existing scheduler ticks and compare container restart count; an HTTP200 check alone does not establish stability.
