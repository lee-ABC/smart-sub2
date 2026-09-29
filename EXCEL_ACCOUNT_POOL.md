# Excel account-pool integration

The native Sub2 scheduler still selects accounts within the API key's group. Priority, sticky sessions, concurrency, billing and failover remain in effect; this is not a new equal round-robin scheduler.

## Routing

- Groups default to native routing. Enable Excel in the group editor or authenticated GET/PUT `/api/v1/admin/groups/:id/excel-mode`.
- Excel mode applies only to supported OAuth models: GPT-6 Sol/Astra and GPT-5.6 Sol/Terra/Luna, including existing Excel aliases.
- Other models keep their native upstream; this is not a GPT-5.5-only exception. API-key accounts retain their own upstream.
- An Excel request failure does not silently downgrade to native.
- Model discovery retains native defaults, original metadata and group model restrictions while adding supported Excel models. Listing a model does not guarantee an account has its entitlement or available quota.
- Admin tests report the selected group and actual route.

## Private transport

Configuration defaults to `/app/data/excel-routing.json`; override with `SUB2_EXCEL_CONFIG_FILE`. Fields include `gateway_url` (for example `http://excel-sub2api:8000/internal/v1`), `transport_key_file` and `group_modes`. Keep the configuration directory writable when using the admin editor.

Use a separate private transport key matching the sidecar's `EXCEL_SUB2API_TRANSPORT_KEY_FILE`. Keep public API, administration and private transport credentials distinct. Do not publish the sidecar port to the Internet.

The matching bridge adaptation is published under `lee-ABC/ghcp_proxy/integrations/sub2api/`. It patches the pinned excel-codex-bridge source and is not the Copilot service. Selected account credentials and request context are scoped per request, not supplied by the old global push-session timer.

## Verification and build

The 205 validation on 2026-09-29 retained all 17 native model entries and added gpt-6-sol. Separate real requests returned HTTP 200 for gpt-5.5 via native and gpt-6-sol via Excel. These checks establish routing and compatibility, not a guarantee of model answer quality.

See SOURCE_RELEASE.md for the existing build process. This publication does not include credentials, generated binaries, runtime data or database migrations.
