# BPS stream budgets and native tool promotion (2026-10-17)

## Observed on server 87

Account 7268 generated `upstream_timeout` at 300032 ms even though the adapter
last received upstream bytes 53 ms before cancellation. This is an adapter total
request deadline, not evidence that the model or credentials are unsupported.
The same account also completed many gpt-6.1-sol requests normally.

Separate failed responses carried `invalid_tool_call [reason=code_json]`.
The server's relay config only selected immediate tool delivery; native promotion
was absent and therefore defaulted off. The pinned CPA engine already supports
native promotion: eligible top-level tools are sent through an additional_tools
catalog instead of asking the model to encode tool JSON inside run_officejs code.
Invalid or unsupported calls still fail closed; do not fabricate arguments or
replay requests after an unknown execution result.

## Change

The private adapter now honors a validated `timeout_seconds` configuration
(10–1800, default remains 300) consistently across the CPA engine, invocation
context, HTTP client and private server write budget (request budget plus 10s).
Previously the wrapper overwrote the config with 300 and independently hardcoded
the HTTP client and server timeouts. Cancellation, explicit failed terminals and
no-replay behavior remain in force.

Server 87's private relay configuration is set to:

```yaml
timeout_seconds: 900
relay_native_promotion: "on"
relay_native_promotion_history: "h2"
```

Existing identity settings and pepper, immediate delivery, main image, database,
login helper, selected-account routing, model and reasoning effort are preserved.
This increases the allowed duration; it does not make upstream generation faster
or guarantee that upstream finishes. Ineligible tools can still use the legacy
relay and remain subject to its conversion checks.

## Validation

- New timeout configuration regressions reproduced the hardcoded 300s behavior.
- Adapter tests and vet pass with `-tags=http2legacy`.
- Targeted vendored native promotion, stream, terminal and cancellation tests pass.
- Live gpt-6.1-sol / medium, account 7268: native read_value call completed in
  2.560s; tool result continuation completed in 2.541s and answered 42.
- This smoke does not prove a real request longer than 300s will finish; the
  configured budget propagation is covered by deterministic tests.

No migrations, account login/import or credential changes are required. Rollback
uses the previous adapter image and previous private relay configuration; main
service and data volumes do not need replacement.
