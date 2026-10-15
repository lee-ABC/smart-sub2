package basispoints

// Regression tests for the three P0 fixes.
//
//   - turnState no longer panics on an empty input list (zz_golden_test.go
//     covers the fingerprint side of that one).
//   - beginStream refuses once shutdown has begun, so drainStreams' Wait can
//     never race an Add.
//   - the transport-call cache is scoped to one conversation, so one caller's
//     call can no longer be spliced into another's.

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- cross-conversation splicing -------------------------------------------

// tenantSource builds a request whose conversation identity is its own.
func tenantSource(sessionKey, secret string) map[string]any {
	return map[string]any{
		"model":            "gpt-5.6-sol",
		"prompt_cache_key": sessionKey,
		"input": []any{
			map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": secret}}},
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "get_weather",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
	}
}

// nativeTransportCall is what the upstream emits: a run_officejs call carrying
// the caller's own arguments.
func nativeTransportCall(callID, city string) map[string]any {
	return map[string]any{
		"type": "function_call", "id": "fc_" + callID, "call_id": callID,
		"name": transportName, "status": "completed",
		"arguments": string(jsonBytes(map[string]any{
			"summary": "weather",
			"code":    string(jsonBytes(map[string]any{"tool": "get_weather", "args": map[string]any{"city": city}})),
		})),
	}
}

func TestTransportCacheIsScopedToOneConversation(t *testing.T) {
	const sharedCallID = "call_1" // the id several agent frameworks emit verbatim

	// Tenant A runs a turn whose upstream answer contains a transport call.
	sourceA := tenantSource("session-A", "SECRET-A")
	if _, ok := extractNativeClientToolCall(
		map[string]any{"output": []any{nativeTransportCall(sharedCallID, "TENANT-A-CITY")}}, sourceA); !ok {
		t.Fatal("tenant A transport call was not decoded")
	}

	// Tenant B, a different conversation, replays a call of its own that
	// happens to reuse the id.
	sourceB := tenantSource("session-B", "SECRET-B")
	ownCall := map[string]any{
		"type": "function_call", "call_id": sharedCallID,
		"name": "get_weather", "arguments": `{"city":"TENANT-B-CITY"}`,
	}
	got := translateInputItems([]any{ownCall}, clientToolSpecs(sourceB), conversationScope(sourceB))
	rendered := string(jsonBytes(got))

	if strings.Contains(rendered, "TENANT-A-CITY") {
		t.Errorf("tenant A's arguments leaked into tenant B's request: %s", rendered)
	}
	if !strings.Contains(rendered, "TENANT-B-CITY") {
		t.Errorf("tenant B lost its own arguments: %s", rendered)
	}

	// Tenant A's own replay must still work: that is what the cache is for.
	clientCallA := map[string]any{
		"type": "function_call", "call_id": sharedCallID,
		"name": "get_weather", "arguments": `{"city":"TENANT-A-CITY"}`,
	}
	backA := translateInputItems([]any{clientCallA}, clientToolSpecs(sourceA), conversationScope(sourceA))
	item := objectValue(backA[0])
	if item == nil || stringValue(item["name"]) != transportName {
		t.Fatalf("tenant A's own call was not restored to the transport form: %#v", backA[0])
	}
}

func TestTransportCacheRejectsNonTransportAndExpired(t *testing.T) {
	scope := "scope-x"
	// A cached entry that is not a transport call must never stand in for a
	// replayed item.
	rememberNativeCall(scope, map[string]any{
		"type": "function_call", "call_id": "c1", "name": "some_other_tool", "arguments": "{}",
	})
	if got := rememberedNativeCall(scope, "c1"); got != nil {
		t.Errorf("non-transport entry was served: %#v", got)
	}
	// An expired entry must not be served either.
	rememberNativeCall(scope, nativeTransportCall("c2", "X"))
	nativeCallCache.Lock()
	entry := nativeCallCache.items[nativeCallCacheKey(scope, "c2")]
	entry.storedAt = time.Now().Add(-nativeCallCacheTTL - time.Minute)
	nativeCallCache.items[nativeCallCacheKey(scope, "c2")] = entry
	nativeCallCache.Unlock()
	if got := rememberedNativeCall(scope, "c2"); got != nil {
		t.Errorf("expired entry was served: %#v", got)
	}
	// An empty scope or call id stores and serves nothing.
	rememberNativeCall("", nativeTransportCall("c3", "X"))
	if got := rememberedNativeCall("", "c3"); got != nil {
		t.Errorf("unscoped entry was served: %#v", got)
	}
}

func TestConversationScopeIsStableAcrossTurns(t *testing.T) {
	// The scope has to survive the conversation growing, or a turn would stop
	// finding the call the previous turn stored.
	first := tenantSource("session-A", "hello")
	second := tenantSource("session-A", "hello")
	second["input"] = append(second["input"].([]any),
		map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "hi"}}})
	if conversationScope(first) != conversationScope(second) {
		t.Error("scope moved when the conversation grew")
	}
	// With no session key the first item carries the identity, and that also
	// has to survive growth.
	noKey := tenantSource("", "hello")
	delete(noKey, "prompt_cache_key")
	grown := tenantSource("", "hello")
	delete(grown, "prompt_cache_key")
	grown["input"] = append(grown["input"].([]any),
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "again"}}})
	if conversationScope(noKey) != conversationScope(grown) {
		t.Error("keyless scope moved when the conversation grew")
	}
	// Two different conversations must not share a scope.
	if conversationScope(tenantSource("session-A", "x")) == conversationScope(tenantSource("session-B", "x")) {
		t.Error("two sessions share a scope")
	}
}

// --- shutdown race ----------------------------------------------------------

func TestBeginStreamRefusesAfterShutdown(t *testing.T) {
	s := NewService()
	if !s.beginStream() {
		t.Fatal("a fresh service refused a stream")
	}
	s.streams.Done()

	if _, err := s.Handle("plugin.shutdown", nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if s.beginStream() {
		s.streams.Done()
		t.Fatal("a stream was armed after shutdown -- this is the Add that races drainStreams' Wait")
	}
}

// TestShutdownUnderLoadDoesNotReuseWaitGroup is the actual crash reproducer.
// Before the fix, arming the group while drainStreams was waiting raised
// "sync: WaitGroup is reused before previous Wait has returned", a runtime
// throw that no recover can catch and that kills the whole gateway process.
// Under the fix every late arm is refused instead, so the loop is quiet.
func TestShutdownUnderLoadDoesNotReuseWaitGroup(t *testing.T) {
	for round := 0; round < 200; round++ {
		s := NewService()
		var wg sync.WaitGroup
		// Callers racing the shutdown, each arriving in the gap the real
		// executeStream leaves between the stopped check and the Add.
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s.beginStream() {
					s.streams.Done()
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Handle("plugin.shutdown", nil)
		}()
		wg.Wait()
	}
}

// TestExecuteStreamRefusesAfterShutdown drives the real RPC entry point.
func TestExecuteStreamRefusesAfterShutdown(t *testing.T) {
	s := NewService()
	if _, err := s.Handle("plugin.shutdown", nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"Stream":          true,
		"stream_id":       "s-1",
		"OriginalRequest": json.RawMessage(`{"model":"gpt-5.6-sol","input":"hi"}`),
		"StorageJSON":     json.RawMessage(fakeCredential(t)),
	})
	_, err := s.Handle("executor.execute_stream", body)
	if err == nil {
		t.Fatal("a stream was accepted after shutdown")
	}
	if !strings.Contains(err.Error(), "shut down") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestReconfigureMidDrainCannotArmWaitGroup covers the second way into the
// fatal reuse: a reconfigure landing while shutdown is draining clears
// `stopped`, and before the fix the next stream armed the very wait group the
// drain was waiting on.
func TestReconfigureMidDrainCannotArmWaitGroup(t *testing.T) {
	s := NewService()

	// One relay in flight, so the drain actually has to wait.
	if !s.beginStream() {
		t.Fatal("first stream refused")
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		_, _ = s.Handle("plugin.shutdown", nil)
	}()

	// Give shutdown time to set the flags and enter the drain.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.RLock()
		draining := s.draining
		s.mu.RUnlock()
		if draining || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// A reconfigure arrives mid-drain and clears `stopped`.
	cfg, _ := json.Marshal(map[string]any{
		"responses_url": "https://bps.openai.com/basispoints/api/responses",
		"models":        []string{"gpt-6-astra"},
	})
	_, _ = s.Handle("plugin.reconfigure", cfg)

	// The door must still be shut: this is the Add that used to kill the process.
	if s.beginStream() {
		s.streams.Done()
		t.Fatal("a stream was armed during the drain after a reconfigure")
	}

	s.streams.Done() // let the drain finish
	<-shutdownDone
}
