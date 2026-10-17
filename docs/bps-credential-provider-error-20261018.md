# OpenAI credential failure incorrectly named Grok

## Evidence and cause

On server40:8890, error records5015/5017 (2026-10-18 02:30 Beijing) identify platform `openai`, group2 and account22. Requests `gpt-5.5` and `gpt-6-luna` select the OpenAI account and fail before opening upstream inference. Account22 has an isolated `bps_credentials` profile but no native OAuth AT/RT. These model IDs are outside the explicit Excel adapter model allowlist, so the existing contract retains native transport. No evidence of a Grok inference route.

The shared OpenAI-compatible handler `credentialFailoverClientResponse` defaulted all credential failures except two special reasons to the Grok unavailable message. Native/BPS credential failover already carried precise reasons and safe messages, which the handler discarded. WebSocket exhaustion had the same unconditional Grok message.

## Fix

- Name the existing native/BPS credential reason/message constants and use them in both construction and client formatting.
- Map native/BPS credential failures to their own static, actionable messages; only `grok_oauth_*` failures use the Grok message. Unknown reasons use a provider-neutral message.
- Reuse the mapping for WebSocket close/errors; Responses, Chat and Messages already share the same HTTP mapping.
- Do not forward arbitrary credential error details or raw tokens to clients.
- Preserve model names/effort, native fallback, API-key behavior, group scheduling, account priority, sticky sessions and failover policy. Do not pretend unsupported models are available via BPS or reuse BPS AT as native AT.

Requests for the two unsupported BPS IDs still require a separate native credential profile/account. This change corrects diagnosis; it does not grant model access or invent credentials. Existing historical error logs are retained.

## Validation and release

Targeted unit tests cover native/BPS/Grok/unknown provider mapping, Responses/Chat/Messages HTTP output, WebSocket close reasons and secret suppression. Existing credential/failover and BPS/model regression tests are also run. Build preserves the existing frontend assets and runtime entrypoint.

Deploy only server40:8890 main Sub2 container using an image-only Compose override, preserving data mounts/config/env and all other containers (adapter, login helper, PostgreSQL, Redis and GoPay). No schema/config/model/account data changes and no deployment to87/205.
