# BPS selected-account inline-image relay

## Root cause and boundary

The selected-account adapter unconditionally replaced `public_base_url` with an
empty string, and did not expose the CPA image resource. Account 20 on server40
therefore returned `image_relay_disabled` for otherwise valid inline images.
A public-base configuration alone could not fix that behavior.

Keep the upstream core unchanged. The wrapper now accepts a validated,
operator-provided absolute HTTP(S) image base, and optionally starts a separate
image-only listener using `BPS_IMAGE_LISTEN_ADDR`. Its exact configured image
path accepts GET/HEAD with a 64-hex-character capability token. All management,
quota, credential, health and inference paths remain absent from this public
listener. The existing private transport stays authenticated and internal.
Existing CPA in-memory image TTL, random token, deduplication, MIME, byte and
fetch ceilings remain authoritative. No account storage or scheduling change.

## Validation

- Adapter package: 117 test cases passed; server package compiled.
- Added tests for base validation, disabled defaults, end-to-end URL rewrite /
  byte retrieval, HEAD, unknown and malformed tokens, duplicate tokens, forbidden
  methods, and public/private route isolation.
- `go vet -tags=http2legacy ./internal ./cmd/server` passed.
- Core image regressions: `go test -tags=http2legacy ./internal/basispoints
  -run 'Image|Inline|DataURL|Relayed' -count=1` passed.
- Live canary account20 synthetic red PNG: completed, answer Red, 2.97 seconds.
- Live production adapter: completed, answer Red, 3.75 seconds.
- Full production `:8890/v1/responses` path: completed, answer Red, 4.61 seconds.
  These are small smoke tests, not a latency benchmark.

## Server40 deployment

Only `sub2api205-excel` was recreated using
`sub2-bps-adapter:image-relay-20261018`. The main `:8890` container, PostgreSQL,
Redis, login helper, account credentials and grouping were not changed.
The public image-only port is 8892, base `http://40.160.130.68:8892/bps-images`.
HTTPS reverse proxy is preferred if a TLS hostname is available. No inference or
management interface is exposed on that port. Canary port8893 was removed.
Backup: `/opt/sub2api205-excel/backups/image-relay-20261017T165906Z`.
Compose override: `/opt/sub2api205-excel/compose.image-relay-20261018.yaml`.

After deployment, the verification window had zero errors matching
`public_base_url`/`image_relay_disabled`. Remaining sampled errors were genuine
quota/rate limits, unavailable Grok accounts, invalidated OAuth credentials and
unavailable-capacity responses. Historical one-off empty-input and remote-file
fetch failures were not rewritten or falsely marked repaired. No forced token
refresh, account activation, cooldown clearing or usage-limit bypass was done.
