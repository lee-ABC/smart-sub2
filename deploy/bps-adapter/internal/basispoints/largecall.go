package basispoints

import (
	"fmt"
	"sync"
)

// Large-call admission.
//
// Every executor call reaches the plugin as one JSON buffer that carries the
// request body twice, base64-encoded, so the buffer is about 2.7 times the
// HTTP body. Turning it back into a request copies it several more times on
// both sides of the plugin boundary. Measured on 2026-10-07 with the gateway
// isolated and a fake upstream: one request with 190 MiB of inline images (a
// 266 MB body, a buffer of about 710 MB) took the process from 20 MiB to
// 8.6 GiB and spent 47 seconds before the upstream was called; a 67 MB body
// cost 1.9 GiB. That is about twelve bytes of memory for every buffer byte,
// and nothing bounded how many such calls ran at once, because the
// per-credential gate sits after all of the decoding. Four bodies at the
// image ceiling arriving together reached 24 GiB; on top of the 10 GiB this
// process peaks at on an ordinary day that is past the container's 32 GiB
// limit, and an out-of-memory kill is not a failed request: it ends every
// stream in flight on every credential.
//
// So calls above a size that ordinary traffic does not reach are admitted
// against a shared byte budget, before the buffer is copied out of the host's
// memory. A call that does not fit is refused at once with 413. The host
// reads that as a fault of the request, so no credential is cooled and
// nothing is retried down the pool, and the caller's own failover can take
// the request elsewhere. Waiting for room instead would keep the host's
// copies of every queued body alive.
//
// This bounds the plugin's share only. The host has already read, translated
// and re-encoded the body before the call arrives here: measured the same
// night, a 308 MB body refused at this gate still cost the process 5.4 GiB
// and 40 seconds. With the gate, four bodies at the image ceiling arriving
// together peaked at 18 GiB instead of 24, and six 84 MB bodies at 12 GiB
// with five of them served. A hard bound needs the same admission in the
// host's HTTP handler.
const (
	// largeCallThreshold is the buffer size from which a call is counted: a
	// body of about 25 MB. Fewer than one request in two hundred is larger.
	largeCallThreshold = 64 << 20
	// largeCallCeiling is the largest single buffer accepted at all: a body of
	// 288 MiB. A request at the inline-image ceiling arrives as about 683 MiB
	// of buffer, so this leaves it 32 MiB of other content, and it is above
	// the 256 MiB that the caller in front of this gateway lets through.
	// Anything larger is refused before any of it is copied.
	largeCallCeiling = 768 << 20
)

// largeCallBudget is how many buffer bytes the counted calls may hold between
// them: one request at the image ceiling with a few ordinary large ones beside
// it, or five 84 MB bodies. A variable so tests can shrink it.
var largeCallBudget int64 = 1280 << 20

var (
	largeCallMu    sync.Mutex
	largeCallInUse int64
)

// AdmitLargeCall reserves room for a call whose buffer is size bytes long. It
// returns the function that gives the room back, which is safe to call more
// than once, or the refusal. Calls below the threshold are admitted without
// any bookkeeping.
func AdmitLargeCall(size int64) (func(), error) {
	if size < largeCallThreshold {
		return func() {}, nil
	}
	if size > largeCallCeiling {
		return nil, fail(413, "request_too_large", fmt.Sprintf(
			"this request is too large to process (about %d MiB, the limit is about %d MiB); shorten or compact the conversation",
			largeCallBodyMiB(size), largeCallBodyMiB(largeCallCeiling)))
	}
	largeCallMu.Lock()
	defer largeCallMu.Unlock()
	if largeCallInUse+size > largeCallBudget {
		return nil, fail(413, "large_requests_busy",
			"too many very large requests are being processed at the moment; retry shortly")
	}
	largeCallInUse += size
	var once sync.Once
	return func() {
		once.Do(func() {
			largeCallMu.Lock()
			largeCallInUse -= size
			largeCallMu.Unlock()
		})
	}, nil
}

// largeCallBodyMiB turns a buffer size into the approximate size in MiB of
// the HTTP body inside it, for messages: the body is carried twice, each time
// as base64, so the buffer is eight thirds of it.
func largeCallBodyMiB(size int64) int64 {
	return size * 3 / 8 >> 20
}

func largeCallBytesInUse() int64 {
	largeCallMu.Lock()
	defer largeCallMu.Unlock()
	return largeCallInUse
}
