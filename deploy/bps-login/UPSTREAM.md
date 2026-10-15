# Source pin and modifications

Source: https://github.com/lee-ABC/cpa_bps
Commit: `4aabe5c3c7543e5c08741eeaa51569d792455a86`

`runtime/` copies CPA login-runtime source, the original package-lock and the
Linux x64 native TLS asset. Sentinel SDK/runner/protocol sources are unchanged;
the existing reviewed SHA checks remain enabled in paired mode. Cookie prefix
hardening in tlsClientBootstrap and rtProtocolClient includes the latest fix.
The runtime config disables SDK auto-updates; no account data is present.

SDK SHA256: `49d0284bf3eea8a59ebcad0e6b5dd8a53edd4c72606f15bbf51ebe5610a88efd`
Native TLS SHA256: `2ec853496634545e7a7ea028715763948d55bbdd97aca7ecaa9fea8c2ebb08df`

`upstream/server` and `upstream/shared` retain CPA's login engine, state machine,
worker/paired SDK host and workspace policy. Only three upstream files change:
worker-host passes context.proxyUrl to the native client rather than null;
workspace-inspector and business-usage-verifier accept proxyUrl and pass it into
native TLS Session. Engine, state-machine and credential validation are unchanged.

New `src/` supplies owner-bound bounded memory jobs, a private HTTP API, and an
in-process host integration. The token exchange uses CPA's existing exchange
validator with a native TLS fetch implementation bound to the same job proxy.
No CPA panel, scheduler, account import, storage, auth files, or refresh daemon
is exposed. Jobs/results live only in memory and expire after ten minutes.

Dependencies are the original node-tls-client 2.1.0 and tough-cookie 4.1.4 lock.
Original module metadata/ISC and dependency/TLS/CPA notices remain under licenses.
Docker base uses exactly the CPA pinned Node22 Bookworm digest.
