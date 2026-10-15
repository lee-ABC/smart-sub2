package basispoints

import (
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// h24StreamRun makes each run of a test that writes the process-wide replay
// cache use its own conversation scope, so -count=N repeats stay independent.
var h24StreamRun atomic.Int64

func TestH24StreamPendingRelayBatchNeverPartiallyExecutes(t *testing.T) {
	good := h21TransportItem("batch_good", "get_weather", map[string]any{"city": "Paris"})
	bad := h21TransportItem("batch_bad", "NOT_FOR_THE_CALLER", map[string]any{"city": "Oslo"})
	changed := h21TransportItem("batch_good", "get_weather", map[string]any{"city": "Oslo"})
	for name, tail := range map[string]string{
		"second_bad_done":     h21ItemEvent("response.output_item.done", 1, bad),
		"second_bad_terminal": h21Terminal("response.completed", good, bad),
		"changed_terminal":    h21Terminal("response.completed", changed),
		"failed_terminal":     h23TerminalResponse("response.failed", map[string]any{"status": "failed", "output": []any{}, "error": map[string]any{"code": "server_error"}}),
		"incomplete_terminal": h23TerminalResponse("response.incomplete", map[string]any{"status": "incomplete", "output": []any{}, "incomplete_details": map[string]any{"reason": "max_output_tokens"}}),
		"error_event":         sseEvent("error", map[string]any{"type": "error", "error": map[string]any{"code": "server_error"}}),
		"eof":                 "",
	} {
		for _, coalesced := range []bool{false, true} {
			// Default (= h22, immediate delivery): the good call goes out on its
			// own done -- once, never as part of a success -- and is replay
			// history. The rest of the stream fails as h22's did, except an
			// upstream error event, which h22 passed through.
			t.Run("default/"+name+map[bool]string{true: "/coalesced", false: "/chunked"}[coalesced], func(t *testing.T) {
				source := weatherRequest()
				source["input"] = []any{messageItem("user", t.Name())}
				chunks := []string{h21ItemEvent("response.output_item.done", 0, good), tail}
				if coalesced {
					chunks = []string{strings.Join(chunks, "")}
				}
				result := runStreamTest(t, source, chunks)
				calls := strings.Count(result.joined(), "event: response.output_item.done")
				if strings.Contains(result.joined(), "run_officejs") || strings.Contains(result.joined(), "event: response.completed") || strings.Contains(result.joined(), "Oslo") {
					t.Fatalf("failed stream leaked a transport, a success or a bad call: %s", result.joined())
				}
				if name == "error_event" {
					if result.closeErr != "" || calls != 1 {
						t.Fatalf("upstream error was reclassified or the delivered call lost: %q", result.closeErr)
					}
					return
				}
				if result.closeErr == "" {
					t.Fatal("failure was hidden")
				}
				// Coalesced, the failure lands in the same host write as the
				// done, which is then never emitted (nor cached) at all. A bare
				// EOF fails only after that write.
				wantCalls := 1
				if coalesced && name != "eof" {
					wantCalls = 0
				}
				if calls != wantCalls {
					t.Fatalf("delivered %d calls, want %d", calls, wantCalls)
				}
				if cached := rememberedNativeCall(streamTestReplayScope(t, source), "call_batch_good"); (cached != nil) != (wantCalls == 1) {
					t.Fatal("replay history disagrees with what the client received")
				}
				if rememberedNativeCall(streamTestReplayScope(t, source), "call_batch_bad") != nil {
					t.Fatal("bad call cached")
				}
			})
			// relay_tool_delivery: batch (rc2 switches).
			t.Run("rc2/"+name+map[bool]string{true: "/coalesced", false: "/chunked"}[coalesced], func(t *testing.T) {
				source := weatherRequest()
				source["input"] = []any{messageItem("user", t.Name())}
				chunks := []string{h21ItemEvent("response.output_item.done", 0, good), tail}
				if coalesced {
					chunks = []string{strings.Join(chunks, "")}
				}
				result := runStreamTestWith(t, s2RC2, source, chunks)
				if strings.Contains(result.joined(), "event: response.output_item.done") || strings.Contains(result.joined(), "run_officejs") || strings.Contains(result.joined(), "event: response.completed") {
					t.Fatalf("failed batch leaked an executable tool or success: %s", result.joined())
				}
				if rememberedNativeCall(streamTestReplayScope(t, source), "call_batch_good") != nil || rememberedNativeCall(streamTestReplayScope(t, source), "call_batch_bad") != nil {
					t.Fatal("failed pending batch wrote replay cache")
				}
				if name == "failed_terminal" || name == "incomplete_terminal" || name == "error_event" {
					if result.closeErr != "" {
						t.Fatalf("upstream failure was reclassified: %q", result.closeErr)
					}
				} else if result.closeErr == "" {
					t.Fatal("batch failure was hidden")
				}
			})
		}
	}
}

func TestH24StreamCachesBatchOnlyAfterSuccessfulHostWrite(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[reject], func(t *testing.T) {
			service := NewService()
			writes := 0
			service.SetHost(func(method string, payload any, out any) error {
				if method != "host.stream.emit" {
					return errors.New("unexpected test callback")
				}
				writes++
				if reject {
					return errors.New("synthetic host rejection")
				}
				return nil
			})
			source := weatherRequest()
			// A scope of this run's own: the cache outlives a run (-count=N).
			source["input"] = []any{messageItem("user", t.Name()+"#"+strconv.FormatInt(h24StreamRun.Add(1), 10))}
			item := h21TransportItem("host_write", "get_weather", map[string]any{"city": "Paris"})
			relay := newStreamRelay(service, "fake", source, usageTarget{}, "gpt-6-astra")
			if err := relay.feed([]byte(h21ItemEvent("response.output_item.done", 0, item))); err != nil {
				t.Fatal(err)
			}
			if err := relay.feed([]byte(h21Terminal("response.completed", item))); err != nil {
				t.Fatal(err)
			}
			if rememberedNativeCall(conversationScope(source), "call_host_write") != nil {
				t.Fatal("validated but unflushed batch was cached")
			}
			err := relay.flush()
			if writes != 1 {
				t.Fatalf("batch was not one host emission: %d", writes)
			}
			cached := rememberedNativeCall(conversationScope(source), "call_host_write")
			if reject {
				if err == nil || cached != nil || len(relay.cacheAfterFlush) != 0 {
					t.Fatal("failed host write cached unaccepted tools")
				}
			} else if err != nil || string(jsonBytes(cached)) != string(jsonBytes(item)) {
				t.Fatalf("accepted batch not correctly cached: %v", err)
			}
		})
	}
}

func TestH24StreamPendingBuffersRespectResponseBudget(t *testing.T) {
	relay := newStreamRelay(NewService(), "fake", weatherRequest(), usageTarget{}, "gpt-6-astra")
	item := h21TransportItem("bounded", "get_weather", map[string]any{"city": "Paris"})
	first := []byte(h21ItemEvent("response.output_item.added", 0, item))
	relay.maxInputBytes = len(first) + 32
	if err := relay.feed(first); err != nil {
		t.Fatal(err)
	}
	if len(relay.held[0].events) != 0 {
		t.Fatal("known transport unnecessarily retained raw argument frames")
	}
	if err := relay.feed([]byte(strings.Repeat("x", 33))); err == nil {
		t.Fatal("pending transport exceeded cumulative response budget")
	}
	if relay.out.Len() != 0 || rememberedNativeCall(conversationScope(relay.source), "call_bounded") != nil {
		t.Fatal("over-budget pending tools were emitted or cached")
	}
}

func TestH24StreamNativeLiteToolRemainsIncrementalAndOpaque(t *testing.T) {
	native := map[string]any{"type": "function_call", "name": "get_weather", "namespace": "functions", "id": "fc_native_lite", "call_id": "native_lite", "arguments": `{"city":"Paris"}`, "encrypted_content": "OPAQUE_SYNTHETIC_DATA"}
	source := weatherRequest()
	source["input"] = []any{map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": source["tools"]}}}, messageItem("user", t.Name())}
	delete(source, "tools")
	bad := h21TransportItem("bad_after_native", "NOT_FOR_THE_CALLER", map[string]any{})
	result := runStreamTest(t, source, []string{h21ItemEvent("response.output_item.done", 0, native), h21ItemEvent("response.output_item.done", 1, bad)})
	h21AssertToolFailure(t, result)
	var items []map[string]any
	for _, event := range h23Events(t, result.joined()) {
		if event["type"] == "response.output_item.done" {
			items = append(items, objectValue(event["item"]))
		}
	}
	if len(items) != 1 || string(jsonBytes(items[0])) != string(jsonBytes(native)) {
		t.Fatal("native Lite tool or opaque content was changed")
	}
	if result.emitIndexContaining("OPAQUE_SYNTHETIC_DATA") >= result.lastReadIndex() {
		t.Fatal("native Lite lifecycle stopped streaming")
	}
}

func TestH24StreamTextStaysIncrementalWhileRelayToolsWait(t *testing.T) {
	first := h21TransportItem("first", "get_weather", map[string]any{"city": "Paris"})
	second := h21TransportItem("second", "get_weather", map[string]any{"city": "Oslo"})
	message := map[string]any{"type": "message", "id": "msg_visible", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "TEXT_VISIBLE_BEFORE_TOOLS"}}}
	addedMessage := cloneObject(message)
	addedMessage["content"], addedMessage["status"] = []any{}, "in_progress"
	chunks := []string{
		h21ItemEvent("response.output_item.done", 0, first),
		h21ItemEvent("response.output_item.added", 1, addedMessage),
		sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 1, "content_index": 0, "item_id": "msg_visible", "delta": "TEXT_VISIBLE_BEFORE_TOOLS", "sequence_number": 88}),
		h21ItemEvent("response.output_item.done", 1, message),
		h21ItemEvent("response.output_item.done", 2, second),
		h21Terminal("response.completed", first, message, second),
	}
	for _, mode := range s2Modes() {
		for _, coalesced := range []bool{false, true} {
			t.Run(mode.name+"/"+map[bool]string{true: "coalesced", false: "chunked"}[coalesced], func(t *testing.T) {
				source := weatherRequest()
				source["input"] = []any{messageItem("user", t.Name())}
				input := chunks
				if coalesced {
					input = []string{strings.Join(chunks, "")}
				}
				result := runStreamTestWith(t, mode.configure, source, input)
				if result.closeErr != "" {
					t.Fatal(result.closeErr)
				}
				if !coalesced && result.emitIndexContaining("TEXT_VISIBLE_BEFORE_TOOLS") >= result.lastReadIndex() {
					t.Fatal("text stopped streaming")
				}
				// batch (rc2): the tools wait for the completed snapshot.
				if mode.rc2 && !coalesced && result.emitIndexContaining(`"name":"get_weather"`) < result.lastReadIndex()-1 {
					t.Fatal("tool was delivered before completed snapshot")
				}
				// Default (= h22, immediate): the first tool goes out on its own
				// done, before the text that follows it.
				if !mode.rc2 && !coalesced && result.emitIndexContaining(`"name":"get_weather"`) > result.emitIndexContaining("TEXT_VISIBLE_BEFORE_TOOLS") {
					t.Fatal("tool was not delivered on its own done")
				}
				toolIndexes := []int{}
				for sequence, event := range h23Events(t, result.joined()) {
					if value, ok := event["sequence_number"].(float64); !ok || int(value) != sequence {
						t.Fatalf("sequence not contiguous at %d", sequence)
					}
					if event["type"] == "response.output_item.done" && isStreamToolItem(objectValue(event["item"])) {
						toolIndexes = append(toolIndexes, int(event["output_index"].(float64)))
					}
				}
				if len(toolIndexes) != 2 || toolIndexes[0] != 0 || toolIndexes[1] != 2 {
					t.Fatalf("tool order/index changed: %v", toolIndexes)
				}
				if rememberedNativeCall(streamTestReplayScope(t, source), "call_first") == nil || rememberedNativeCall(streamTestReplayScope(t, source), "call_second") == nil {
					t.Fatal("validated batch not cached")
				}
			})
		}
	}
}
