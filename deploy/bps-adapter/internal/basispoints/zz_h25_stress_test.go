package basispoints

// Concurrency and stressness stress tests (h25 verification, review R6),
// ported from the stressness review: batch heartbeat with many streams,
// client hang-ups and host panics in host.stream.emit; immediate delivery
// under live reconfiguration and relay-status reads; the shared replay scope
// through Service.Handle; replay pool invariants under concurrent writers;
// configuration warnings logged once. They pass without -race in a few
// seconds; the build runs them again under -race.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestH25StressRaceCanary proves the race detector is live in a toolchain:
// run with H25_RACE_CANARY=1 and -race, it must report a DATA RACE.
func TestH25StressRaceCanary(t *testing.T) {
	if os.Getenv("H25_RACE_CANARY") == "" {
		t.Skip("canary only on demand")
	}
	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); counter++ }()
	}
	wg.Wait()
	_ = counter
}

type stressHost struct {
	mu         sync.Mutex
	chunks     []string
	readDelay  time.Duration
	emitDelay  time.Duration
	failEmit   func(streamID, payload string, rnd *rand.Rand) bool
	panicEmit  func(streamID, payload string, rnd *rand.Rand) bool
	nextUp     atomic.Int64
	reads      map[string]int
	inflight   map[string]int
	closed     map[string]int
	closeErr   map[string]string
	emits      map[string][]string
	violations []string
	logs       int
	closedCh   chan string
	rnd        *rand.Rand
}

func newStressHost(chunks []string, streams int) *stressHost {
	return &stressHost{chunks: chunks, reads: map[string]int{}, inflight: map[string]int{}, closed: map[string]int{},
		closeErr: map[string]string{}, emits: map[string][]string{}, closedCh: make(chan string, streams*4), rnd: rand.New(rand.NewSource(7))}
}

func (h *stressHost) violate(format string, args ...any) {
	h.mu.Lock()
	h.violations = append(h.violations, fmt.Sprintf(format, args...))
	h.mu.Unlock()
}

func (h *stressHost) call(method string, payload any, out any) error {
	request, _ := payload.(map[string]any)
	switch method {
	case "host.http.do_stream":
		stream := out.(*upstreamStream)
		stream.StatusCode, stream.StreamID = 200, fmt.Sprintf("up-%d", h.nextUp.Add(1))
	case "host.http.stream_read":
		id := stringValue(request["stream_id"])
		h.mu.Lock()
		index := h.reads[id]
		h.reads[id]++
		h.mu.Unlock()
		if index > 0 && h.readDelay > 0 {
			time.Sleep(h.readDelay)
		}
		chunk := out.(*streamChunk)
		if index < len(h.chunks) {
			chunk.Payload = []byte(h.chunks[index])
		} else {
			chunk.Done = true
		}
	case "host.stream.emit":
		id := stringValue(request["stream_id"])
		text := string(request["payload"].([]byte))
		h.mu.Lock()
		if h.closed[id] > 0 {
			h.violations = append(h.violations, "emit after close on "+id)
		}
		h.inflight[id]++
		if h.inflight[id] > 1 {
			h.violations = append(h.violations, "concurrent emit on "+id)
		}
		doFail := h.failEmit != nil && h.failEmit(id, text, h.rnd)
		doPanic := h.panicEmit != nil && h.panicEmit(id, text, h.rnd)
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			h.inflight[id]--
			h.mu.Unlock()
		}()
		if h.emitDelay > 0 {
			time.Sleep(h.emitDelay)
		}
		if doPanic {
			panic("synthetic host panic in host.stream.emit")
		}
		if doFail {
			return errors.New("synthetic client hang-up")
		}
		h.mu.Lock()
		h.emits[id] = append(h.emits[id], text)
		h.mu.Unlock()
	case "host.stream.close":
		id := stringValue(request["stream_id"])
		h.mu.Lock()
		h.closed[id]++
		if h.closed[id] > 1 {
			h.violations = append(h.violations, "double close on "+id)
		}
		if h.inflight[id] != 0 {
			h.violations = append(h.violations, "close while an emit is in flight on "+id)
		}
		h.closeErr[id], _ = request["error"].(string)
		h.mu.Unlock()
		h.closedCh <- id
	case "host.log":
		h.mu.Lock()
		h.logs++
		h.mu.Unlock()
	}
	return nil
}

func stressService(t *testing.T, host *stressHost, configure func(*Config)) *Service {
	t.Helper()
	service := NewService()
	cfg := service.config()
	cfg.DataDir = ""
	cfg.MaxConcurrencyPerCredential = 0
	cfg.QuotaSampleSeconds = 0
	if configure != nil {
		configure(&cfg)
	}
	service.mu.Lock()
	service.cfg = cfg
	service.mu.Unlock()
	service.SetHost(host.call)
	return service
}

func stressStreamRequest(index int, body map[string]any) []byte {
	raw, _ := json.Marshal(map[string]any{
		"OriginalRequest": jsonBytes(body), "Payload": jsonBytes(body), "StorageJSON": streamTestCredential(),
		"Model": "gpt-6-astra", "Stream": true, "stream_id": fmt.Sprintf("client-%d", index),
		"host_callback_id": fmt.Sprintf("cb-%d", index), "AuthID": "stress-cred", "AuthProvider": Provider,
		"Metadata": map[string]any{"caller_scope": "stress-caller"},
	})
	return raw
}

func stressRelayChunks() []string {
	paris := h21TransportItem("stress", "get_weather", map[string]any{"city": "Paris"})
	added := cloneObject(paris)
	added["status"], added["arguments"] = "in_progress", ""
	return []string{
		s2Created() + h21ItemEvent("response.output_item.added", 0, added),
		sseEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": "{"}),
		h21ItemEvent("response.output_item.done", 0, paris),
		sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 1, "delta": "after"}),
		h21Terminal("response.completed", paris),
	}
}

func stressGoroutineSettle(t *testing.T, baseline int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline+2 || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stressOutcomeTotal() uint64 {
	total := uint64(0)
	for key, value := range relayCounterSnapshot() {
		if strings.HasPrefix(key, "relay_outcome:") {
			total += value
		}
	}
	return total
}

type stressScenario struct {
	name       string
	configure  func(*Config)
	failEmit   func(string, string, *rand.Rand) bool
	panicEmit  func(string, string, *rand.Rand) bool
	reconfig   bool
	wantClean  bool
	wantBeats  bool
	streams    int
	rounds     int
	readDelay  time.Duration
	emitDelay  time.Duration
	readStatus bool
}

func runStressScenario(t *testing.T, sc stressScenario) {
	interval, poll := relayHeartbeatInterval, relayHeartbeatPoll
	relayHeartbeatInterval, relayHeartbeatPoll = 2*time.Millisecond, time.Millisecond
	defer func() { relayHeartbeatInterval, relayHeartbeatPoll = interval, poll }()
	baseline := runtime.NumGoroutine()
	host := newStressHost(stressRelayChunks(), sc.streams*sc.rounds)
	host.readDelay, host.emitDelay = sc.readDelay, sc.emitDelay
	host.failEmit, host.panicEmit = sc.failEmit, sc.panicEmit
	service := stressService(t, host, sc.configure)
	beforeOutcomes := stressOutcomeTotal()
	beforeBeats := relayCounterSnapshot()["batch_heartbeat"]
	stopSide := make(chan struct{})
	var side sync.WaitGroup
	if sc.reconfig {
		side.Add(1)
		go func() {
			defer side.Done()
			values := []string{"immediate", "batch", "bogus", ""}
			for i := 0; ; i++ {
				select {
				case <-stopSide:
					return
				default:
				}
				yaml := fmt.Sprintf("max_concurrency_per_credential: 0\nquota_sample_seconds: 0\nrelay_tool_delivery: %s\ntool_policy_checks: %s\nretryable_error_hint: %v\n",
					values[i%len(values)], []string{"log", "enforce", "x"}[i%3], i%2 == 0)
				raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
				if _, err := service.Handle("plugin.reconfigure", raw); err != nil {
					host.violate("reconfigure failed: %v", err)
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	if sc.readStatus {
		side.Add(1)
		go func() {
			defer side.Done()
			for {
				select {
				case <-stopSide:
					return
				default:
				}
				status := service.serveRelayStatus()
				if status["StatusCode"] != 200 {
					host.violate("relay-status failed")
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	total := 0
	for round := 0; round < sc.rounds; round++ {
		var launch sync.WaitGroup
		errs := make(chan error, sc.streams)
		for i := 0; i < sc.streams; i++ {
			launch.Add(1)
			index := round*sc.streams + i
			go func() {
				defer launch.Done()
				if _, err := service.Handle("executor.execute_stream", stressStreamRequest(index, weatherRequest())); err != nil {
					errs <- err
				}
			}()
		}
		launch.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("execute_stream: %v", err)
		}
		total += sc.streams
		timeout := time.After(20 * time.Second)
		for closed := 0; closed < sc.streams; closed++ {
			select {
			case <-host.closedCh:
			case <-timeout:
				t.Fatalf("%s: only %d/%d streams closed (deadlock?)", sc.name, closed, sc.streams)
			}
		}
	}
	close(stopSide)
	side.Wait()
	if !service.drainStreams(5 * time.Second) {
		t.Fatalf("%s: relay goroutines did not drain", sc.name)
	}
	if n := stressGoroutineSettle(t, baseline); n > baseline+2 {
		buf := make([]byte, 1<<20)
		t.Fatalf("%s: goroutines %d > baseline %d\n%s", sc.name, n, baseline, buf[:runtime.Stack(buf, true)])
	}
	service.gate.mu.Lock()
	for key, value := range service.gate.inFlight {
		if value != 0 {
			host.violate("gate slot leaked: %s=%d", key, value)
		}
	}
	service.gate.mu.Unlock()
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.violations) > 0 {
		t.Fatalf("%s: %d violations, first: %v", sc.name, len(host.violations), host.violations[:min(5, len(host.violations))])
	}
	if got := stressOutcomeTotal() - beforeOutcomes; got != uint64(total) {
		t.Fatalf("%s: relay_outcome counted %d times for %d streams", sc.name, got, total)
	}
	clean, disconnected, panicked, other := 0, 0, 0, 0
	for _, message := range host.closeErr {
		switch {
		case message == "":
			clean++
		case strings.Contains(message, "client_disconnected"):
			disconnected++
		case strings.Contains(message, "stream aborted"):
			panicked++
		default:
			other++
		}
	}
	beats := relayCounterSnapshot()["batch_heartbeat"] - beforeBeats
	t.Logf("%s: streams=%d clean=%d disconnected=%d panicked=%d other=%d heartbeats=%d logs=%d", sc.name, total, clean, disconnected, panicked, other, beats, host.logs)
	if sc.wantClean && clean != total {
		for id, message := range host.closeErr {
			if message != "" {
				t.Fatalf("%s: %s closed with %q", sc.name, id, message)
			}
		}
	}
	if other > 0 && !sc.reconfig {
		for _, message := range host.closeErr {
			if message != "" && !strings.Contains(message, "client_disconnected") && !strings.Contains(message, "stream aborted") {
				t.Fatalf("%s: unexpected close error %q", sc.name, message)
			}
		}
	}
	if sc.wantBeats && beats == 0 {
		t.Fatalf("%s: batch never sent a heartbeat", sc.name)
	}
	for id, emits := range host.emits {
		joined := strings.Join(emits, "")
		if strings.Contains(joined, "run_officejs") {
			t.Fatalf("%s: transport leaked on %s", sc.name, id)
		}
		for _, emit := range emits {
			if strings.Contains(emit, ": keepalive") && emit != relayHeartbeatFrame {
				t.Fatalf("%s: heartbeat spliced into a frame on %s", sc.name, id)
			}
		}
	}
}

func TestH25StressBatchHeartbeatManyStreams(t *testing.T) {
	runStressScenario(t, stressScenario{name: "batch_clean", configure: s2Batch, streams: 40, rounds: 3,
		readDelay: 15 * time.Millisecond, emitDelay: 200 * time.Microsecond, wantClean: true, wantBeats: true, readStatus: true})
}

func TestH25StressBatchHeartbeatClientHangups(t *testing.T) {
	runStressScenario(t, stressScenario{name: "batch_hangup", configure: s2Batch, streams: 40, rounds: 3,
		readDelay: 10 * time.Millisecond, emitDelay: 100 * time.Microsecond,
		failEmit: func(_, _ string, rnd *rand.Rand) bool { return rnd.Intn(10) == 0 }})
}

func TestH25StressBatchHeartbeatHostPanics(t *testing.T) {
	runStressScenario(t, stressScenario{name: "batch_host_panic", configure: s2Batch, streams: 40, rounds: 3,
		readDelay: 10 * time.Millisecond, emitDelay: 100 * time.Microsecond,
		panicEmit: func(_, _ string, rnd *rand.Rand) bool { return rnd.Intn(12) == 0 }})
}

func TestH25StressImmediateWithReconfigureAndStatus(t *testing.T) {
	runStressScenario(t, stressScenario{name: "immediate_reconfig", streams: 40, rounds: 3,
		readDelay: 5 * time.Millisecond, reconfig: true, readStatus: true})
}

func TestH25StressImmediateHostPanics(t *testing.T) {
	runStressScenario(t, stressScenario{name: "immediate_host_panic", streams: 40, rounds: 2,
		readDelay: 2 * time.Millisecond,
		panicEmit: func(_, _ string, rnd *rand.Rand) bool { return rnd.Intn(8) == 0 }})
}

// The replay pools under concurrent writers, readers and expiry: the byte
// account, the FIFO and the map must always agree.
func TestH25StressReplayPoolInvariants(t *testing.T) {
	pool := &replayPool{items: map[string]nativeCallEntry{}, name: "stress", maxEntries: 64, maxBytes: 64 << 10, maxEntry: 8 << 10}
	var wg sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed))
			for op := 0; op < 4000; op++ {
				key := fmt.Sprintf("scope\x00call_%d", rnd.Intn(200))
				switch rnd.Intn(10) {
				case 0, 1, 2:
					pool.lookup("scope", fmt.Sprintf("call_%d", rnd.Intn(200)))
				case 3:
					// Age a random entry past the TTL, as two hours passing would.
					pool.Lock()
					if entry, ok := pool.items[key]; ok {
						entry.storedAt = entry.storedAt.Add(-3 * time.Hour)
						pool.items[key] = entry
					}
					pool.Unlock()
				default:
					size := rnd.Intn(10 << 10)
					entry := nativeCallEntry{item: map[string]any{"name": "run_officejs", "arguments": strings.Repeat("x", size)},
						fingerprint: fmt.Sprintf("fp%d", rnd.Intn(3)), origin: "relay"}
					pool.put(key, entry, rnd.Intn(4) == 0)
				}
			}
		}(int64(worker))
	}
	wg.Wait()
	pool.Lock()
	defer pool.Unlock()
	sum, seen := 0, map[string]bool{}
	for _, key := range pool.order {
		if seen[key] {
			t.Fatalf("key twice in FIFO")
		}
		seen[key] = true
		if _, ok := pool.items[key]; !ok {
			t.Fatalf("FIFO key without an item")
		}
	}
	for key, entry := range pool.items {
		if !seen[key] {
			t.Fatalf("item missing from FIFO")
		}
		sum += entry.weight
	}
	if sum != pool.bytes || len(pool.items) > pool.maxEntries || pool.bytes > pool.maxBytes {
		t.Fatalf("pool accounting: sum=%d bytes=%d items=%d", sum, pool.bytes, len(pool.items))
	}
	t.Logf("pool after stress: items=%d bytes=%d", len(pool.items), pool.bytes)
}

// Configuration warnings are logged once, by exactly one of many concurrent
// requests, whatever reconfigures race with them.
func TestH25StressConfigWarningsFlushOnce(t *testing.T) {
	service := NewService()
	var logs atomic.Int64
	service.SetHost(func(method string, payload any, out any) error {
		if method == "host.log" {
			if fields, _ := payload.(map[string]any)["fields"].(map[string]any); fields["event"] == "relay_config_fallback" {
				logs.Add(1)
			}
		}
		return nil
	})
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("data_dir: \"\"\nrelay_protocol: bogus\ncoerce_scalar_strings: maybe\n")})
	if _, err := service.Handle("plugin.reconfigure", raw); err != nil {
		t.Fatalf("invalid switch values failed registration: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request, _ := json.Marshal(map[string]any{"host_callback_id": fmt.Sprintf("cb-%d", i), "Model": "gpt-6-astra"})
			_, _ = service.Handle("executor.execute", request)
		}(i)
	}
	wg.Wait()
	if logs.Load() != 1 {
		t.Fatalf("relay_config_fallback logged %d times", logs.Load())
	}
}

// Shared replay scope: every request carries the same prompt_cache_key and a
// history that names the call the streams keep (re)delivering, so prepare-time
// cache reads (h22 replay + rc2 observation) race the flush-time writes of
// other streams, relay and Lite, streaming and not.
func TestH25StressSharedReplayScopeThroughHandle(t *testing.T) {
	relayHistory := func(i int) map[string]any {
		body := weatherRequest()
		body["prompt_cache_key"] = "stress-shared"
		body["input"] = []any{messageItem("user", "first"),
			map[string]any{"type": "function_call", "call_id": "call_stress", "name": "get_weather", "arguments": fmt.Sprintf(`{"city":"C%d"}`, i%3)},
			map[string]any{"type": "function_call_output", "call_id": "call_stress", "output": "sunny"},
			messageItem("user", "again")}
		return body
	}
	liteHistory := func(i int) map[string]any {
		return map[string]any{"model": "gpt-6-astra", "prompt_cache_key": "stress-shared-lite", "input": []any{
			s2Additional(s2Weather()), messageItem("user", "first"),
			map[string]any{"type": "function_call", "call_id": "call_native", "name": "get_weather", "arguments": fmt.Sprintf(`{"city":"N%d"}`, i%3)},
			map[string]any{"type": "function_call_output", "call_id": "call_native", "output": "ok"},
			messageItem("user", "again")}}
	}
	native := s2NativeCall("native", `{"city":"Paris"}`)
	nativeAdded := cloneObject(native)
	nativeAdded["status"], nativeAdded["arguments"] = "in_progress", ""
	liteChunks := []string{s2Created() + h21ItemEvent("response.output_item.added", 0, nativeAdded),
		h21ItemEvent("response.output_item.done", 0, native), h21Terminal("response.completed", native)}
	for _, mode := range []struct {
		name   string
		chunks []string
		body   func(int) map[string]any
	}{{"relay", stressRelayChunks(), relayHistory}, {"lite", liteChunks, liteHistory}} {
		baseline := runtime.NumGoroutine()
		host := newStressHost(mode.chunks, 200)
		host.readDelay = time.Millisecond
		service := stressService(t, host, nil)
		var wg sync.WaitGroup
		streams := 60
		for i := 0; i < streams; i++ {
			wg.Add(2)
			go func(i int) {
				defer wg.Done()
				if _, err := service.Handle("executor.execute_stream", stressStreamRequest(i, mode.body(i))); err != nil {
					host.violate("stream %d: %v", i, err)
				}
			}(i)
			go func(i int) {
				defer wg.Done()
				body := mode.body(i)
				raw, _ := json.Marshal(map[string]any{"OriginalRequest": jsonBytes(body), "Payload": jsonBytes(body), "StorageJSON": streamTestCredential(),
					"Model": "gpt-6-astra", "AuthID": "stress-cred", "AuthProvider": Provider, "Metadata": map[string]any{"caller_scope": "stress-caller"}})
				// Non-streaming: the fake upstream answers with the same SSE body.
				_, _ = service.Handle("executor.execute", raw)
			}(i)
		}
		wg.Wait()
		timeout := time.After(20 * time.Second)
		for closed := 0; closed < streams; closed++ {
			select {
			case <-host.closedCh:
			case <-timeout:
				t.Fatalf("%s: only %d/%d streams closed", mode.name, closed, streams)
			}
		}
		if !service.drainStreams(5 * time.Second) {
			t.Fatalf("%s: not drained", mode.name)
		}
		stressGoroutineSettle(t, baseline)
		host.mu.Lock()
		bad := 0
		for _, message := range host.closeErr {
			if message != "" {
				bad++
			}
		}
		t.Logf("%s: streams=%d failed=%d violations=%d logs=%d", mode.name, streams, bad, len(host.violations), host.logs)
		if len(host.violations) > 0 || bad > 0 {
			host.mu.Unlock()
			t.Fatalf("%s: violations %v close errors %d", mode.name, host.violations, bad)
		}
		host.mu.Unlock()
	}
	c := relayCounterSnapshot()
	t.Logf("history_relay_replayed=%d history_note:relay_history_mismatch=%d history_note:native_history_mismatch=%d history_record_overwritten=%d",
		c["history_relay_replayed"], c["history_note:relay_history_mismatch"], c["history_note:native_history_mismatch"], c["history_record_overwritten"])
}
