# BPS integration into Sub2API

## Scope

This source includes the selected-account Basis Points adapter, separate native
and BPS OAuth profiles, BPS account-file import, interactive password/TOTP login,
and optional per-credential thread identity diagnostics. The CPA source is pinned
to `4aabe5c3c7543e5c08741eeaa51569d792455a86`; this is the deployed integration pin,
not a claim that it is the newest upstream commit.

Group OpenAI outbound selection takes effect only after normal Sub2 account
selection. Priority, sticky sessions, concurrency and failover remain owned by
Sub2API. The BPS route supports Responses, Chat and Messages for gpt-6-sol,
gpt-6-astra, gpt-5.6-sol, gpt-5.6-terra and gpt-5.6-luna plus documented Excel
aliases. Other models, including gpt-5.5, and API-key accounts remain native.
An upstream-reported model can differ from the requested model; this integration
does not make model identity guarantees or suppress mismatch reporting.

## Credentials and import

Native credentials retain the existing root fields. BPS credentials occupy the
separate sensitive `bps_credentials` profile, with isolated refresh and token
caches. File imports merge only after proving the same JWT subject/user/workspace;
email alone is not sufficient. Opposite-profile credentials and business settings
are preserved. BPS requests never borrow the native OAuth token as a fallback.

Account management supports a BPS2FA login flow with email/password and an optional
Base32 TOTP secret. Email OTP/workspace challenges are interactive. Successful
login produces a safe preview; explicit confirmation imports the credential.
AT/RT and password/TOTP are not returned to the browser or written to helper logs.
Consult `deploy/bps-login/README.md` for owner binding and bounded job lifetime.

## Private services and builds

Build the main app with the repository Dockerfile. Build the protocol adapter
from `deploy/bps-adapter` and the login helper from `deploy/bps-login`.
The helpers are private services; do not publish their ports directly.

- Adapter: Go1.27, **required `-tags=http2legacy`** (Makefile/Dockerfile include it).
- Login helper: Node22 Linux x64, locked npm dependencies and pinned native TLS
  asset included under `runtime/vendor`. No account or deployment secrets ship.
- Set `SUB2_BPS_LOGIN_URL` and `SUB2_BPS_LOGIN_KEY_FILE` for the main app.
- Mount the same private login key into the helper at `/run/secrets/login-key`.
- Adapter key defaults to `/run/secrets/transport-key`.
- Generate deployment-specific secrets locally; do not commit runtime YAML,
  pepper, credentials, account files, HARs, databases, logs or image archives.

Source provenance and component notices are retained under each helper's
`UPSTREAM.md`, `licenses/` and third-party notice files. The native binary SHA256
is `2ec853496634545e7a7ea028715763948d55bbdd97aca7ecaa9fea8c2ebb08df`.

## Batch 2FA login

The OpenAI BPS login panel defaults to a bounded batch mode. Paste one credential
per line as `email----password----Base32-TOTP-secret`; the parser uses the first and
last separator so a password may contain `----`. It accepts at most100 non-empty
rows and starts at most5 private login jobs at once. Invalid or duplicate rows are
reported by line number without echoing their contents.

Only rows reaching a validated `ready` state are offered to the one-click import
button. Email OTP and workspace challenges remain per-row and can be completed
without resending the password or TOTP secret. Failed, expired and cancelled rows
are hidden by default; unknown import outcomes are marked for review and are not
automatically retried. The import freezes the current proxy/groups/priority and
other account settings, uses the full email as the account name, and preserves
native credentials separately.

## Thread identity and safe logging

The optional private YAML named by `BPS_RELAY_CONFIG_FILE` accepts
`client_identity.mode: thread`. Preserve the deployment's existing pepper. The
file is read at startup, so reload only the adapter when changing this option.

The current Sub2 wrapper uses `sub2:<account-record-ID>` as its stable identity
seed. Task IDs are calculated, not captured from the first successful connection
or written back into the account record. A new user turn gets a new turn ID;
tool continuations reuse its cached turn ID and increment the iteration. The
turn cache is in memory and bounded by a30-minute TTL and4096 entries; a restart
or eviction can end turn-ID reuse. Caller scopes remain isolated.

New JSON events `bps.upstream.request`, `bps.upstream.response`, and
`bps.upstream.transport_error` record selected account/request IDs, actual outbound
identity metadata, requested/upstream model, response status and header latency.
They exclude tokens, cookies, prompt/tool content, pepper and raw error text.
`/healthz` exposes the configured identity mode and whether logging is enabled.

The upstream transport is HTTPS/SSE, not WebSocket. Thread mode does not change
native/BPS route selection. HTTP200 at header arrival is not stream completion;
validate the terminal event separately. Two bounded live parallel checks in the
deployed environment completed successfully; this is not proof that all403s,
concurrency issues or model mismatches are resolved.

## Verification commands

```sh
(cd deploy/bps-adapter && make test && make race)
(cd deploy/bps-login/runtime && npm ci --omit=dev --no-audit --no-fund)
(cd deploy/bps-login && npm test && npm run check-native)
(cd backend && go test -tags=unit ./internal/handler/admin ./internal/server/middleware ./internal/service ./internal/repository -run 'BPS|Bps|Excel|CodexImport')
(cd frontend && pnpm typecheck)
```

Database integration checks require a separate disposable PostgreSQL instance;
never point test DSNs at production. Publishing source does not rebuild/restart
services, run migrations, rotate tokens or enable accounts. Preserve the original
native scheduler and existing deployment state when integrating these services.
