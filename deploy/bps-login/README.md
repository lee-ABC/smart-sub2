# Private Sub2 BPS login helper

This service obtains a **BPS-only AT/RT pair** using CPA's existing email,
password, TOTP and paired SDK login. It does not replace native credentials,
schedule accounts, import into a pool, or refresh existing credentials.

## Run and test

```sh
(cd runtime && npm ci --omit=dev --no-audit --no-fund)
npm test
npm run check-native   # local SDK/hash/dlopen probe; no account or network request
docker build -t sub2-bps-login:4aabe5c .
```

The runtime uses a pinned Node22 Bookworm slim image, retaining glibc and the
same paired native SDK. The packaged `--check-native` probe and login tests
validate the slim image; no login credentials are needed for this probe. Budget
for the **uncompressed** image size and Docker import scratch space, not only a
compressed image transfer.

Node22 on Linux x64 is required for the supplied pinned native asset. The image
starts on private port8001. Mount a read-only secret file at
`/run/secrets/login-key` (override `BPS_LOGIN_KEY_FILE`); key format is 32–256
URL-safe characters and file permissions must be0600. The existing private
network, not a public published port, should expose this helper to Sub2API.
A read-only root filesystem with a64MiB `/tmp` tmpfs is supported; temporary
native-library copies contain public code only, never account credentials.

## Sub2 connection configuration

The Sub2 application container must set the complete worker API prefix:

```text
SUB2_BPS_LOGIN_URL=http://bps-login:8001/internal/v1
SUB2_BPS_LOGIN_KEY_FILE=/app/data/bps-login-key
```

The application key file and helper key must contain the same private secret.
A URL without `/internal/v1` fails validation before any login job is created.
A healthy `/healthz` alone does not verify this integration. Verify the runtime
application URL and matching keys, then issue an authenticated GET for a
nonexistent job: the expected 404 confirms the private path/authentication
without submitting a login or importing an account.

## HTTP contract

Private requests require `Authorization: Bearer <login key>` plus trusted
`X-Sub2-BPS-Owner: <positive authenticated admin ID>`. Unknown/foreign job IDs
always return404; cross-owner result/input/cancel operations are not allowed.

- `POST /internal/v1/jobs`: JSON `{email,password,totp_secret?,workspace_id?,workspace_preference?,proxy_url?}`.
- `GET /internal/v1/jobs/:id`: sanitized status only.
- `POST /internal/v1/jobs/:id/input`: `{kind:"email_otp"|"workspace",value:"..."}`; only the current challenge/explicit offered workspace is accepted.
- `POST /internal/v1/jobs/:id/result`: `{credential:{type:"oai-basispoints",...}}` only when ready; remains idempotently readable for the job TTL.
- `DELETE /internal/v1/jobs/:id`: cancels child/native work and erases result/input references.
- `GET /healthz`: `{ready:true|false}` without account information.

Statuses: running, awaiting_input, ready, failed, cancelled. Phases are CPA's
session/email/password/mfa/email_otp/authorization/exchange/validate. Fixed codes
never contain arbitrary upstream responses. Successful status includes bounded
nonsecret preview fields; AT/RT are available only from the private result route.

Jobs have a10-minute absolute TTL including ready results, at most5 active jobs
(including waits) and128 retained jobs. No passwords/TOTP/tokens go to disk, argv,
progress output or logs. A restart intentionally loses unfinished jobs/results.

`team_first` defaults to CPA's strict complete Business workspace verification;
personal/team/first or an explicit workspace are supported. Six-digit TOTP
codes are not secret keys and are rejected as totp_secret; email challenge codes
use the input route. Invalid proxies fail closed. A supplied proxy governs all
authorize, password, MFA, workspace, Sentinel and token-exchange network hops.
No real-account smoke is run without a fresh user-supplied login tuple.
