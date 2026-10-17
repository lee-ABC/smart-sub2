# Stream termination repair (2026-10-17)

The private adapter continues to use CPA protocol code pinned at `4aabe5c`.
The newer upstream `f711f08` changes nginx frame policy, not the relay engine.
Model/effort selection, tool/history conversion, task/turn identity, account
selection, and the 300-second request budget are unchanged.

## Reproduced integration defects

1. The adapter's deadline branch returned an empty EOF. Its expired invocation
   also rejected the relay's subsequent close notification. Sub2 therefore
   reported `OpenAI stream ended before a terminal event` instead of a timeout.
2. `io.Reader` may return valid bytes together with a non-EOF error. Passing both
   to CPA in one stream chunk discarded the bytes, because CPA checks the error
   first. The wrapper now defers that error until the next read, allowing a
   valid completed terminal to be consumed first.

A missing terminal now produces `response.failed` and `[DONE]`, never a false
success. A queued/observed upstream terminal wins a deadline or late close
error. A disconnected caller is not sent a synthetic failure. Submitted
requests are not replayed by this repair.

`bps.stream.end` records fixed metadata, outcome, terminal type, elapsed time,
upstream/downstream byte counts and read timings. It excludes credentials,
headers, prompts, tool arguments, and arbitrary upstream error text.

## Verification

- Fault-injection regressions fail before the fix for deadline/empty EOF,
  truncated body, and terminal bytes returned with a read error.
- Full adapter and vendored Basis Points tests, `go vet`, and targeted race
  tests pass with `http2legacy`.
- On the upgraded 205 adapter, two text turns and a tool-call/result roundtrip
  all return `response.completed` plus `[DONE]` (about 1.9–3.8 seconds).
- Prior production failures lasted exactly 300 seconds. Their upstream stall
  trigger is not established by the old header-only logs; this fix makes the
  outcome explicit and adds evidence for any recurrence. It does not promise
  that an upstream can never stall.
