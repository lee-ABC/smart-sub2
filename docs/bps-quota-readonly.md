# BPS-only OAuth quota reads

## Scope

BPS-only OpenAI OAuth accounts can use **Query usage** and **Refresh reset-credit count** without importing native OAuth credentials. These reads use the account's validated `bps_credentials` profile and existing isolated BPS token provider. Both access-token refresh and workspace identity checks retain their existing BPS-specific lock, credential-slot, and compare-and-swap protections.

Accounts with any native access/refresh token retain native behavior; a native failure never silently falls back to BPS. API-key and agent-identity authentication are unchanged. This does not merge the native and BPS token namespaces.

## Read-only upstream behavior

- Quota queries call the existing WHAM usage and reset-credit GET endpoints. The account usage card uses this same path rather than a model-generation probe.
- The reset-credit refresh action updates a local count snapshot; it does not consume a credit. Actual manual/automatic credit consumption remains native-only.
- Quota reads do not clear account errors, enable scheduling, or change inference routing, model selection, concurrency limits, or group membership. Token renewal and telemetry persistence can still update their respective local fields.
- Missing windows remain absent rather than being rendered as zero usage. An absent fresh window retires its old normalized cached values; authentication, upstream, and persistence failures are reported instead of becoming an empty successful result.
- Usage snapshots obey the existing cache/refresh interval independently of whether WebSocket v2 is enabled.

## Meaning of the values

WHAM window utilization and available reset credits are separate metrics. A reset-credit count of zero does not mean zero remaining model calls. The account-list concurrency display is also unrelated to quota. These endpoints do not establish a separate per-model quota for every BPS model.

## Regression checks

`openai_bps_quota_test.go` covers isolated credential selection, malformed/mismatched profiles, native precedence, upstream failures, expired tokens, BPS-only renewal, and the unchanged native-only consumption boundary.

`account_usage_bps_test.go` covers GET-only card refresh, cache behavior, missing/stale windows, explicit errors, local statistics, and preservation of account health/scheduling.

Targeted suite (459 test/subtest passes on the initial verification):

```sh
cd backend
go test -p 3 -tags unit ./internal/service ./internal/handler/admin ./internal/repository \
  -run 'BPS|BPSToken|OpenAIQuota|QueryUsage|ResetCredit|GetOpenAIUsage|CodexSnapshot|CodexUsage|SparkShadow|NativeOAuth' -count=1
go vet -tags unit ./internal/service ./internal/handler/admin
go build -o /tmp/sub2api-bps-quota ./cmd/server
```

No schema, dependency, or frontend changes are required. This source change does not itself deploy a new container, restart a server, or migrate production data.
