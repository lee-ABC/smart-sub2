# BPS model routing verification — 2026-10-18

## Scope and evidence

The reported failures were `gpt-5.5` and `gpt-6-luna` on the OpenAI Excel/BPS
channel. Missing the local model allowlist did not establish upstream support.
An isolated binary, built from the same adapter engine, sent each candidate under
an explicit identity mapping (`model_upstreams[id] = id`). It used the selected
account's separate BPS credentials on server 40; credentials were passed in memory
and were not printed or saved. No scheduler/account/credential settings changed.
Only small real text requests were made; these consume upstream usage.

For every request the probe recorded the actual outbound model/effort, upstream
HTTP status, terminal event, response model and answer. Successful entries below
all returned `response.completed`, the **same exact response model**, and `OK`.
The effort used in these live probes was `low`; other effort preservation is
covered by regression tests rather than claims of live availability.

| Requested model | Upstream HTTP | Result | Seconds |
| --- | --- | --- | --- |
| gpt-6-luna | 200 | completed, same model | 3.48 |
| gpt-6-astra | 200 | completed, same model | 4.19 |
| gpt-6-sol | 200 | completed, same model | 2.99 |
| gpt-6.1-sol | 200 | completed, same model | 2.64 |
| gpt-5.6-sol | 200 | completed, same model | 3.74 |
| gpt-5.6-terra | 200 | completed, same model | 1.89 |
| gpt-5.6-luna | 200 | completed, same model | 3.68 |
| gpt-5.5 | 403 | basispoints_model_access_changed | 0.47 |

Also tested under their exact names: `gpt-6-terra`, `gpt-6.1-astra`,
`gpt-6.1-luna`, `gpt-6.1-terra`, `gpt-6`, `gpt-5.6`, `gpt-5.4`,
`gpt-5.4-mini`, `gpt-5.2`, `gpt-4o`, `gpt-4o-audio-preview`,
`gpt-4o-realtime-preview`, `gpt-5.3-codex-spark`, `codex-auto-review`.
All returned upstream HTTP 403 `basispoints_model_access_changed` within 0.50s.
These are current selected-account observations, **not permanent/global claims**.
The authenticated access endpoint returned `model_catalog: null`, so there is no
complete authoritative dynamic catalog to claim exhaustive future coverage from.
CPA origin/main was checked at `f711f08`; its static historical model list/comments
are not a substitute for these live results.

## Change and invariants

- Add `gpt-6-luna` to backend public/admin catalog, BPS routing allowlist, adapter
  model list (which generates an explicit original-name mapping), and frontend
  OpenAI whitelist. All seven confirmed models are now represented.
- Preserve `gpt-5.5` and other unconfirmed models on the existing native path.
  Do not alias them to `gpt-6-astra` to manufacture a successful request.
- Native-selected groups, API-key accounts, account selection/priority,
  stickiness, failover, credential stores, task/turn identity and billing logic
  are unchanged. Existing explicit `*-excel` aliases are unchanged.
- Responses / Chat / Messages and WS bridge reuse the shared model classifier.

## Verification

- New luna model/effort adapter regression failed before the change (local 400,
  upstream not called), then passed for streaming/non-streaming × four efforts.
- Service regressions include protocol × model × stream × group mode × account
  type, and model/effort preservation through the WS bridge.
- Full vendored engine suite has one existing failing golden test:
  `TestH25S5DefaultConfigurationIsH22EndToEnd` (identity_off/plain_model_upstream_mapping).
  Reproduced on clean pre-change HEAD `d954816`; no vendor sources/fixtures are
  changed here. It expects the historical gpt-6-sol rejection; current engine
  forwards it. Report this baseline rather than rewriting historical fixtures.
