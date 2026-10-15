# Pinned upstream and component notices

- Source: https://github.com/lee-ABC/cpa_bps
- Commit: `4aabe5c3c7543e5c08741eeaa51569d792455a86` (2026-10-15 checkout).
- Copied directory: `cpa-plugin/internal/basispoints` including upstream tests.
- Original module identity: `github.com/jaxson-wang/cpa-plugin-oai-basispoints`.
- Original module identity retained; go.mod adds only existing CPA host dependency pins (`utls v1.8.2`, `x/net v0.57.0`) needed for browser transport.
- Original source copyright statements retained. The supplied CPA fork reports no
  standalone license for the Basis Points plugin; this adapter does not invent one.
- Upstream's `THIRD_PARTY_NOTICES.md` is retained beside this file.

The wrapper is new Sub2 integration code; vendored protocol source is unchanged.
`internal/utls_transport.go` is a bounded extraction of CPA core
`internal/runtime/executor/helps/utls_client.go` from this same commit. Claude
transport and CPA global cache/selector are excluded. The private wrapper uses a
bounded (256) per-selected-identity+proxy cache. Invalid proxy errors fail closed;
canceled TLS handshakes close their sockets. Logrus callbacks are omitted.
`internal/proxyutil` is copied unchanged from CPA's `sdk/proxyutil`. Their MIT
license is retained in CPA-MIT-LICENSE. Connection reuse/retirement tests are
copied with only package/constructor/cache test-helper adaptations.

Build and test with `-tags=http2legacy` (Dockerfile/Makefile do this). Go 1.27's
new x/net HTTP2 wrapper changes reservation/retirement semantics; this tag keeps
the same implementation CPA uses under Go 1.26. Startup rejects Go 1.27 builds
that omit the tag. Dependencies' notices are retained under licenses/.
It exposes only authenticated POST /internal/v1/responses and /health[z].
No CPA account pool, auth files, OAuth flow, refresh, management, quota polling,
credential disabling, or scheduler is exposed. Sub2 owns these responsibilities.
Explicit model mapping prevents gpt-6-sol silently becoming gpt-6-astra.
