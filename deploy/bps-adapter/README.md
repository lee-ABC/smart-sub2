# Sub2 selected-account BPS adapter

A private transport adapter for the account **already selected by Sub2API**.
It embeds the pinned CPA Basis Points protocol engine, not a second CPA gateway.
Native credentials, BPS refresh, group selection, account priority, sticky
sessions, business expiry, failover and billing remain in Sub2API.

## Build and verify

```sh
make test
make race
make build
docker build -t sub2-bps-adapter:4aabe5c .
```

Go 1.27+ must use `-tags=http2legacy` (already in Makefile/Dockerfile); startup
rejects a build without it. See UPSTREAM.md for the compatibility rationale.

## Private contract

- `POST /internal/v1/responses`: Responses JSON, streaming or nonstreaming.
- `Authorization: Bearer <private transport key>`.
- Optional trusted `X-Sub2-Excel-Transport: websocket` selects upstream WS; absent/`sse` keeps HTTP/SSE.
- Selected BPS AT: `X-Sub2-Excel-Access-Token` (never native AT).
- JWT workspace: `X-Sub2-Excel-Account-Id`.
- JWT user: `X-Sub2-Excel-Account-User-Id`.
- Positive Sub2 account ID: `X-Sub2-Excel-Credential-Id`.
- Trusted per-key/group/account caller isolation: `X-Sub2-Excel-Scope`.
- Optional selected-account proxy: `X-Sub2-Excel-Proxy`.
- Health: unauthenticated `GET /health` or `/healthz`, no secrets/details.

Sub2 normalizes Responses / Chat / Messages into Responses before this hop and
converts outputs using its existing protocol paths. No arbitrary URL, OAuth
refresh, credential management, auth-file pool or external management routes
are exposed. BPS bearer destinations are exact HTTPS bps.openai.com responses;
redirects are not followed. Each selected identity+proxy has its own bounded
Chrome uTLS/HTTP2 connection pool. Request cancellation closes upstream bodies.

Accepted canonical models: gpt-6-sol, gpt-6-astra, gpt-5.6-sol,
gpt-5.6-terra, gpt-5.6-luna. Existing aliases gpt-6-excel,
gpt-6-astra-excel and gpt-5.6-{sol,terra,luna}-excel map explicitly to these
canonical names. In particular 6-sol does not silently become 6-astra. Upstream
may still reject a model not entitled on the selected account; that error is
returned, not retried under a different model.

## Deployment

- `BPS_LISTEN_ADDR` defaults to `:8000` (private Docker network only).
- `BPS_TRANSPORT_KEY_FILE` defaults to `/run/secrets/transport-key`.
- Optional `BPS_RELAY_CONFIG_FILE` supplies CPA relay behavior settings.
- Run with read-only filesystem; no storage or auth files are needed.
- Image defaults to UID/GID 10001; compose may set a compatible runtime user for
  an existing read-only secret. Do not change secret ownership opportunistically.

The wrapper forcibly disables CPA's separate concurrency caps, account mutation,
quota polling, data directory and public inline-image relay. It fixes the target,
models and mapping even if optional relay YAML tries to override them. Existing
plain-text and client-tool behavior remains upstream CPA; inline images fail
explicitly unless another supported path handled them before this adapter.

Only source/mock/local TLS tests are provided. They do not establish live account
entitlement or production inference success; do not enable expired/unschedulable
accounts merely to obtain a smoke-test result.

## Thread identity and safe diagnostics

Set `client_identity.mode: thread` in the private YAML referenced by
`BPS_RELAY_CONFIG_FILE`. Preserve the existing persisted `client_identity.pepper`;
do not regenerate or print it. Runtime config is read at sidecar startup.

With the pinned CPA core, the same credential keeps its task ID across
conversations. A new user turn receives a different turn ID; tool continuations
in the same turn retain its turn ID and increment agent iteration. Caller scopes
remain isolated. This is a user-turn distinction, not simply a login-user ID.

`/healthz` reports `client_identity_mode` and `identity_logging`. The server emits
fixed JSON events `bps.upstream.request`, `bps.upstream.response`, and
`bps.upstream.transport_error`, with selected account ID, local request ID,
upstream attempt, model, actual outbound task/turn/iteration, and response-header
status/latency. IDs and models are validated before logging. No bearer/refresh
tokens, cookies, prompts, tool outputs, pepper, or raw transport errors are logged
by these events. Response-header HTTP 200 is not proof of stream completion.

Thread identity itself is transport-independent. HTTP callers keep HTTPS/SSE
(`transport: https_sse`); the trusted Sub2 WS bridge can select upstream WSS
(`transport: wss`) through the private transport header. Both use the same CPA
request preparation, credential scope, task ID and turn cache. Model and reasoning
effort are taken from that prepared request, never from the HAR example.
See `docs/bps-thread-identity-deployment.md` for the bounded live checks on 40:8890.

## Upstream WebSocket contract

The WS transport reuses existing x/net WebSocket and Chrome uTLS dependencies.
HTTP/1.1 ALPN is confined to WS; the SSE HTTP/2 pool remains unchanged. The exact
BPS destination and selected-account proxy remain enforced. The observed public
subprotocol is `responses`, with a private `openai-bearer.<BPS AT>` subprotocol.
No authentication material, cookies, prompts, tool outputs or resume tokens are
logged. Logs add actual `reasoning_effort`, transport, and safe WS request UUID.

Only transport fields are added: `type: response.create`, a fresh
`basispoints_request_id`, a fresh opaque resume token, and connection affinity.
Task/turn/iteration are not regenerated in the WS layer. Model aliases and effort
normalization follow the unchanged CPA/SSE path. Private WS controls are consumed
before existing response/tool normalization; they never become user turns.

This is single-shot WS, not automatic reconnect/resume. A send attempt sets the
trusted private `X-Sub2-Excel-Upstream-Started: true` response header. Partial
writes, post-send disconnects or draining must not cause backend account failover
or replay; the bridge treats marked outcomes as request-scoped and stops. Proven
pre-send rejection and ordinary SSE/native behavior retain their existing policy.
