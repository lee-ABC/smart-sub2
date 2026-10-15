package basispoints

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// h20: calls with very large buffers are admitted against a shared byte
// budget, so a few of them arriving together cannot exhaust the container.

func resetLargeCalls(t *testing.T, budget int64) {
	t.Helper()
	saved := largeCallBudget
	largeCallMu.Lock()
	largeCallInUse = 0
	largeCallBudget = budget
	largeCallMu.Unlock()
	t.Cleanup(func() {
		largeCallMu.Lock()
		largeCallInUse = 0
		largeCallBudget = saved
		largeCallMu.Unlock()
	})
}

func refusal(t *testing.T, err error) *APIError {
	t.Helper()
	if err == nil {
		return nil
	}
	coded, ok := err.(*APIError)
	if !ok {
		t.Fatalf("refusal is %T (%v), want *APIError", err, err)
	}
	return coded
}

func TestH20LargeCallShippedLimits(t *testing.T) {
	// A request at the inline-image ceiling has to fit on its own: its body is
	// the base64 of the images plus some text, and the buffer carries the body
	// twice as base64.
	body := int64(imageRelayMaxRequestBytes)/3*4 + 16<<20
	buffer := body * 8 / 3
	if buffer > largeCallCeiling {
		t.Fatalf("a request at the image ceiling (buffer about %d MiB) is over the single-call ceiling (%d MiB)", buffer>>20, int64(largeCallCeiling)>>20)
	}
	if int64(largeCallCeiling) > largeCallBudget {
		t.Fatalf("the single-call ceiling (%d MiB) does not fit the budget (%d MiB)", int64(largeCallCeiling)>>20, largeCallBudget>>20)
	}
	// Two of them at once must not: that is what the budget is for.
	if 2*buffer <= largeCallBudget {
		t.Fatalf("the budget (%d MiB) admits two requests at the image ceiling at once", largeCallBudget>>20)
	}
	if largeCallBudget != 1280<<20 || largeCallThreshold != 64<<20 || largeCallCeiling != 768<<20 {
		t.Fatalf("shipped limits changed: threshold %d MiB, ceiling %d MiB, budget %d MiB",
			int64(largeCallThreshold)>>20, int64(largeCallCeiling)>>20, largeCallBudget>>20)
	}
}

func TestH20SmallCallsAreNeverCounted(t *testing.T) {
	resetLargeCalls(t, 100<<20)
	for i := 0; i < 1000; i++ {
		release, err := AdmitLargeCall(largeCallThreshold - 1)
		if err != nil {
			t.Fatalf("a call below the threshold was refused: %v", err)
		}
		_ = release // deliberately never released: small calls hold nothing
	}
	if used := largeCallBytesInUse(); used != 0 {
		t.Fatalf("small calls reserved %d bytes", used)
	}
}

func TestH20LargeCallBudget(t *testing.T) {
	resetLargeCalls(t, 1000<<20)

	first, err := AdmitLargeCall(700 << 20)
	if err != nil {
		t.Fatalf("the first large call was refused: %v", err)
	}
	// A second one of the same size does not fit beside it.
	_, err = AdmitLargeCall(700 << 20)
	got := refusal(t, err)
	if got == nil || got.Status != 413 || got.Kind != "large_requests_busy" {
		t.Fatalf("second large call: got %+v, want 413 large_requests_busy", got)
	}
	if used := largeCallBytesInUse(); used != 700<<20 {
		t.Fatalf("a refused call changed the reservation: %d bytes in use", used)
	}
	// A smaller one still does.
	second, err := AdmitLargeCall(300 << 20)
	if err != nil {
		t.Fatalf("a call that fits the remaining budget was refused: %v", err)
	}
	if used := largeCallBytesInUse(); used != 1000<<20 {
		t.Fatalf("in use = %d bytes, want the whole budget", used)
	}
	// Exactly full: one more counted byte is too many.
	if _, err = AdmitLargeCall(largeCallThreshold); err == nil {
		t.Fatalf("a call was admitted into a full budget")
	}

	// Giving room back is idempotent and makes room for the refused call.
	first()
	first()
	if used := largeCallBytesInUse(); used != 300<<20 {
		t.Fatalf("after releasing twice: %d bytes in use, want %d", used, 300<<20)
	}
	third, err := AdmitLargeCall(700 << 20)
	if err != nil {
		t.Fatalf("a large call was refused after room was given back: %v", err)
	}
	second()
	third()
	if used := largeCallBytesInUse(); used != 0 {
		t.Fatalf("everything released, yet %d bytes in use", used)
	}
}

func TestH20LargeCallCeilingAndMessages(t *testing.T) {
	resetLargeCalls(t, largeCallBudget)

	_, err := AdmitLargeCall(largeCallCeiling + 1)
	got := refusal(t, err)
	if got == nil || got.Status != 413 || got.Kind != "request_too_large" {
		t.Fatalf("a call over the single-call ceiling: got %+v, want 413 request_too_large", got)
	}
	if used := largeCallBytesInUse(); used != 0 {
		t.Fatalf("a call over the ceiling reserved %d bytes", used)
	}
	if _, err := AdmitLargeCall(largeCallCeiling); err != nil {
		t.Fatalf("a call exactly at the ceiling was refused: %v", err)
	}
	_, busyErr := AdmitLargeCall(largeCallCeiling)
	busy := refusal(t, busyErr)
	if busy == nil || busy.Kind != "large_requests_busy" {
		t.Fatalf("second call at the ceiling: got %+v, want large_requests_busy", busy)
	}

	// The caller in front of this gateway moves a 413 to another upstream,
	// except when the message reads like a context-window error, which it
	// returns to the client instead. Neither refusal may read like one.
	for _, message := range []string{got.Message, busy.Message} {
		lower := strings.ToLower(message)
		for _, marker := range []string{"context window", "context length", "context_length", "context_too_large", "token limit", "maximum context", "max context"} {
			if strings.Contains(lower, marker) {
				t.Fatalf("refusal %q contains %q and would be taken for a context-window error", message, marker)
			}
		}
	}
}

// Many calls racing for the budget: the reservation never exceeds it and
// returns to zero. Run with -race.
func TestH20LargeCallConcurrentAdmission(t *testing.T) {
	const budget = 1280 << 20
	const each = 300 << 20 // four fit, a fifth does not
	resetLargeCalls(t, budget)

	var holding, peak, overdrawn, admitted, refused int64
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 200; round++ {
				release, err := AdmitLargeCall(each)
				if err != nil {
					atomic.AddInt64(&refused, 1)
					continue
				}
				now := atomic.AddInt64(&holding, 1)
				for {
					seen := atomic.LoadInt64(&peak)
					if now <= seen || atomic.CompareAndSwapInt64(&peak, seen, now) {
						break
					}
				}
				// Hold the room for a moment, so that calls really do overlap,
				// and look at the books while holding it.
				time.Sleep(200 * time.Microsecond)
				if largeCallBytesInUse() > budget {
					atomic.AddInt64(&overdrawn, 1)
				}
				atomic.AddInt64(&admitted, 1)
				atomic.AddInt64(&holding, -1)
				release()
			}
		}()
	}
	wg.Wait()
	if overdrawn > 0 {
		t.Fatalf("the reservation was seen above the budget %d times", overdrawn)
	}
	if peak > budget/each {
		t.Fatalf("%d calls of %d MiB held the budget at once; only %d fit in %d MiB", peak, each>>20, budget/each, budget>>20)
	}
	if admitted == 0 || refused == 0 {
		t.Fatalf("the calls never competed for room: %d admitted, %d refused", admitted, refused)
	}
	if used := largeCallBytesInUse(); used != 0 {
		t.Fatalf("all calls done, yet %d bytes still reserved", used)
	}
}
