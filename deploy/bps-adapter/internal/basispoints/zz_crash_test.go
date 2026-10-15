package basispoints

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestNoWaitGroupReuseUnderReloadLoad is the same scenario that kills the
// unfixed plugin: relays arming while a drain is waiting on a counter that
// keeps reaching zero. On the unfixed code this raises "sync: WaitGroup misuse:
// Add called concurrently with Wait" within a few rounds, which is a runtime
// throw -- no recover catches it and the whole gateway process dies. Here every
// late arm goes through beginStream, which refuses once the drain has started,
// so the loop has to stay quiet.
func TestNoWaitGroupReuseUnderReloadLoad(t *testing.T) {
	runtime.GOMAXPROCS(runtime.NumCPU())
	refused := 0
	for round := 0; round < 4000; round++ {
		s := NewService()
		var wg sync.WaitGroup
		stop := make(chan struct{})
		var mu sync.Mutex

		for i := 0; i < runtime.NumCPU(); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if s.beginStream() {
						s.streams.Done()
					} else {
						mu.Lock()
						refused++
						mu.Unlock()
					}
				}
			}()
		}
		time.Sleep(20 * time.Microsecond)
		_, _ = s.Handle("plugin.shutdown", nil)
		close(stop)
		wg.Wait()
	}
	if refused == 0 {
		t.Fatal("no arm was ever refused -- the test never reached the drain window, so it proves nothing")
	}
	t.Logf("arms refused during drain: %d", refused)
}
