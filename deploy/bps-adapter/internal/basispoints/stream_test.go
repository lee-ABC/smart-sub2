package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func sseEvent(name string, payload map[string]any) string {
	return "event: " + name + "\ndata: " + string(jsonBytes(payload)) + "\n\n"
}

func streamTestCredential() []byte {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	token := encode(map[string]any{"alg": "none"}) + "." + encode(map[string]any{
		"exp":                         4102444800,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-stream"},
	}) + "."
	return jsonBytes(map[string]any{"type": Provider, "access_token": token, "account_id": "acct-stream"})
}

type streamTestResult struct {
	emits    []string
	closeErr string
	// trace records host interactions in order, as "read" and "emit" entries,
	// so a test can tell whether the caller was served while the upstream was
	// still being consumed.
	trace []string
	// logs are the host.log payloads, in order.
	logs []map[string]any
}

// Cross-request cache tests carry a host-authenticated caller (part of the
// replay scope since h25) and an explicit execution session, which only
// matters to history_conflict: reject (the strict scope).
func streamTestReplayRequest(t *testing.T) ExecutorRequest {
	t.Helper()
	return ExecutorRequest{AuthID: "synthetic-stream-credential", AuthProvider: Provider,
		Metadata: map[string]any{"caller_scope": "synthetic-stream-caller", "execution_session_id": "stream-test-" + shortHash(t.Name())}}
}

func streamTestReplayScope(t *testing.T, source map[string]any) string {
	t.Helper()
	scope, _ := deriveReplayScope(streamTestReplayRequest(t), source)
	return scope
}

func (r streamTestResult) joined() string { return strings.Join(r.emits, "") }

// emitIndexContaining reports the position in the trace of the first emit whose
// payload contains needle, or -1.
func (r streamTestResult) emitIndexContaining(needle string) int {
	seen := 0
	for position, entry := range r.trace {
		if entry != "emit" {
			continue
		}
		if strings.Contains(r.emits[seen], needle) {
			return position
		}
		seen++
	}
	return -1
}

func (r streamTestResult) lastReadIndex() int {
	last := -1
	for position, entry := range r.trace {
		if entry == "read" {
			last = position
		}
	}
	return last
}

func runStreamTest(t *testing.T, requestBody map[string]any, chunks []string) streamTestResult {
	t.Helper()
	return runStreamTestWith(t, nil, requestBody, chunks)
}

// runStreamTestWith runs the relay with configure applied to the service's
// configuration first (the request freezes its switches from it).
func runStreamTestWith(t *testing.T, configure func(*Config), requestBody map[string]any, chunks []string) streamTestResult {
	t.Helper()
	service := NewService()
	if configure != nil {
		cfg := service.config()
		configure(&cfg)
		service.mu.Lock()
		service.cfg = cfg
		service.mu.Unlock()
	}
	var (
		mu       sync.Mutex
		emits    []string
		trace    []string
		logs     []map[string]any
		closeErr string
		next     int
	)
	done := make(chan struct{})
	service.SetHost(func(method string, payload any, out any) error {
		request, _ := payload.(map[string]any)
		switch method {
		case "host.http.do_stream":
			stream := out.(*upstreamStream)
			stream.StatusCode = 200
			stream.StreamID = "upstream-1"
			return nil
		case "host.http.stream_read":
			chunk := out.(*streamChunk)
			mu.Lock()
			defer mu.Unlock()
			trace = append(trace, "read")
			if next < len(chunks) {
				chunk.Payload = []byte(chunks[next])
				next++
				return nil
			}
			chunk.Done = true
			return nil
		case "host.stream.emit":
			mu.Lock()
			defer mu.Unlock()
			trace = append(trace, "emit")
			emits = append(emits, string(request["payload"].([]byte)))
			return nil
		case "host.stream.close":
			mu.Lock()
			if message, ok := request["error"].(string); ok {
				closeErr = message
			}
			mu.Unlock()
			close(done)
			return nil
		case "host.log":
			mu.Lock()
			logs = append(logs, request)
			mu.Unlock()
			return nil
		}
		return nil
	})

	identity := streamTestReplayRequest(t)
	raw, _ := json.Marshal(map[string]any{
		"OriginalRequest": jsonBytes(requestBody),
		"Payload":         jsonBytes(requestBody),
		"StorageJSON":     streamTestCredential(),
		"Model":           "gpt-6-astra",
		"Stream":          true,
		"stream_id":       "client-1",
		"AuthID":          identity.AuthID,
		"AuthProvider":    identity.AuthProvider,
		"Metadata":        identity.Metadata,
	})
	if _, err := service.Handle("executor.execute_stream", raw); err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay never closed the stream")
	}
	mu.Lock()
	defer mu.Unlock()
	return streamTestResult{
		emits:    append([]string(nil), emits...),
		closeErr: closeErr,
		trace:    append([]string(nil), trace...),
		logs:     append([]map[string]any(nil), logs...),
	}
}

func textChunks() []string {
	return []string{
		sseEvent("response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_1", "status": "in_progress", "output": []any{}},
		}),
		sseEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{}},
		}) + sseEvent("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "output_index": 0,
			"item_id": "msg_1", "content_index": 0, "delta": "先说前半句",
		}),
		sseEvent("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "output_index": 0,
			"item_id": "msg_1", "content_index": 0, "delta": "再说后半句",
		}),
		sseEvent("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp_1", "status": "completed",
				"output": []any{map[string]any{
					"type": "message", "id": "msg_1", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "先说前半句再说后半句"}},
				}},
			},
		}) + "data: [DONE]\n\n",
	}
}

// The regression this whole relay exists for.
//
// 0.1.x buffered the upstream stream to completion before emitting anything, so
// the caller's first byte arrived only once the answer was fully generated.
// Text must reach the caller while the upstream is still being read.
func TestStreamRelayForwardsTextBeforeTheAnswerIsComplete(t *testing.T) {
	result := runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hi"}, textChunks())
	if result.closeErr != "" {
		t.Fatalf("stream closed with error: %s", result.closeErr)
	}
	if len(result.emits) < 2 {
		t.Fatalf("relay emitted %d time(s); a buffered relay emits once", len(result.emits))
	}
	firstText := result.emitIndexContaining("先说前半句")
	if firstText < 0 {
		t.Fatalf("no emit carried the first text delta: %q", result.joined())
	}
	if lastRead := result.lastReadIndex(); firstText > lastRead {
		t.Fatalf("first text reached the caller at trace position %d, after the last upstream read at %d: still buffering", firstText, lastRead)
	}
	if !strings.Contains(result.joined(), "response.completed") || !strings.Contains(result.joined(), "[DONE]") {
		t.Fatalf("stream did not terminate properly: %q", result.joined())
	}
}

func transportChunks(arguments string) []string {
	transportItem := map[string]any{
		"type": "function_call", "id": "fc_1", "call_id": "call_1",
		"name": "run_officejs", "status": "completed", "arguments": arguments,
	}
	return []string{
		sseEvent("response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_2", "status": "in_progress", "output": []any{}},
		}),
		sseEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "run_officejs", "arguments": ""},
		}),
		sseEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "output_index": 0,
			"item_id": "fc_1", "delta": arguments,
		}),
		sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": transportItem,
		}),
		sseEvent("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp_2", "status": "completed",
				"output": []any{transportItem},
			},
		}) + "data: [DONE]\n\n",
	}
}

func weatherRequest() map[string]any {
	return map[string]any{
		"model": "gpt-6-astra",
		"input": "天气",
		"tools": []any{map[string]any{
			"type": "function",
			"name": "get_weather",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		}},
	}
}

// Client tools cannot be declared to Basis Points, so the model calls a
// transport tool that encodes the real one. The caller must never see that
// transport name -- not in the streamed items and not in the final response.
func TestStreamRelayTranslatesTransportCallWithoutLeakingIt(t *testing.T) {
	arguments := string(jsonBytes(map[string]any{
		"summary": "查询天气",
		"code":    string(jsonBytes(map[string]any{"tool": "get_weather", "args": map[string]any{"city": "北京"}})),
	}))
	result := runStreamTest(t, weatherRequest(), transportChunks(arguments))
	if result.closeErr != "" {
		t.Fatalf("stream closed with error: %s", result.closeErr)
	}
	joined := result.joined()
	if strings.Contains(joined, "run_officejs") {
		t.Fatalf("transport tool name leaked to the caller: %q", joined)
	}
	if !strings.Contains(joined, "get_weather") {
		t.Fatalf("decoded tool name missing: %q", joined)
	}
	if !strings.Contains(joined, `{\"city\":\"北京\"}`) && !strings.Contains(joined, `{"city":"北京"}`) {
		t.Fatalf("decoded arguments missing: %q", joined)
	}
}

// An invalid transport call must fail without placing an unexecutable tool in
// the caller's conversation history.
func TestStreamRelayRejectsTransportCallItCannotDecode(t *testing.T) {
	arguments := string(jsonBytes(map[string]any{
		"summary": "未知工具",
		"code":    string(jsonBytes(map[string]any{"tool": "not_declared", "args": map[string]any{}})),
	}))
	result := runStreamTest(t, weatherRequest(), transportChunks(arguments))
	if !strings.Contains(result.closeErr, "invalid_tool_call") {
		t.Fatalf("expected a clear tool-conversion failure, got %q", result.closeErr)
	}
	joined := result.joined()
	if strings.Contains(joined, "run_officejs") || strings.Contains(joined, "not_declared") {
		t.Fatalf("undecodable transport call reached the caller: %q", joined)
	}
	if strings.Contains(joined, "response.completed") || strings.Contains(joined, "[DONE]") {
		t.Fatalf("failed conversion announced success: %q", joined)
	}
}

// A tool call that is not the transport must stream through untouched rather
// than being held back.
func TestStreamRelayLeavesUnrelatedToolCallsAlone(t *testing.T) {
	item := map[string]any{
		"type": "function_call", "id": "fc_9", "call_id": "call_9",
		"name": "get_weather", "status": "completed", "arguments": `{"city":"上海"}`,
	}
	chunks := []string{
		sseEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0, "item": item,
		}),
		sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": item,
		}),
		sseEvent("response.completed", map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_3", "status": "completed", "output": []any{item}},
		}) + "data: [DONE]\n\n",
	}
	result := runStreamTest(t, weatherRequest(), chunks)
	if result.closeErr != "" {
		t.Fatalf("stream closed with error: %s", result.closeErr)
	}
	addedAt := result.emitIndexContaining("response.output_item.added")
	if addedAt < 0 || addedAt > result.lastReadIndex() {
		t.Fatalf("unrelated tool call was held back instead of forwarded: trace=%v", result.trace)
	}
}

// An upstream that dies mid-answer must surface as a stream error rather than a
// silently truncated success.
func TestStreamRelayReportsStreamThatNeverCompletes(t *testing.T) {
	chunks := textChunks()[:3] // everything except response.completed
	result := runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hi"}, chunks)
	if result.closeErr == "" {
		t.Fatal("truncated upstream stream closed as a success")
	}
	if !strings.Contains(result.joined(), "先说前半句") {
		t.Fatalf("text received before the truncation was discarded: %q", result.joined())
	}
}

// plugin.shutdown must not return while a relay is still calling into the host.
func TestShutdownWaitsForRelaysToStop(t *testing.T) {
	service := NewService()
	release := make(chan struct{})
	service.streams.Add(1)
	go func() {
		defer service.streams.Done()
		<-release
	}()
	if service.drainStreams(50 * time.Millisecond) {
		t.Fatal("drain reported success while a relay was still running")
	}
	close(release)
	if !service.drainStreams(2 * time.Second) {
		t.Fatal("drain did not observe the relay finishing")
	}
}

// upstreamToolsChunks mimics what Basis Points actually sends: every response
// object carries its own Excel tool catalogue, whatever the caller declared.
func upstreamToolsChunks() []string {
	upstreamTools := []any{
		map[string]any{"type": "function", "name": "run_officejs"},
		map[string]any{"type": "function", "name": "read_ranges"},
	}
	responseObject := func(status string) map[string]any {
		return map[string]any{
			"id": "resp_4", "status": status, "output": []any{},
			"tools": upstreamTools,
			// Stands in for the ~60 KB spreadsheet agent prompt the real
			// upstream echoes back in every response object.
			"instructions": "You are the spreadsheet agent for the ChatGPT Excel Plugin.",
		}
	}
	return []string{
		sseEvent("response.created", map[string]any{"type": "response.created", "response": responseObject("in_progress")}),
		sseEvent("response.in_progress", map[string]any{"type": "response.in_progress", "response": responseObject("in_progress")}),
		sseEvent("response.completed", map[string]any{"type": "response.completed", "response": responseObject("completed")}) + "data: [DONE]\n\n",
	}
}

// The response must report the tools the caller asked with, not the upstream's
// own catalogue. A caller that rebuilds its next request from the previous
// response would otherwise re-declare Basis Points' Excel tools in place of its
// own and lose access to the tools it actually implements.
func TestStreamRelayEchoesCallerToolsNotTheUpstreamCatalogue(t *testing.T) {
	result := runStreamTest(t, weatherRequest(), upstreamToolsChunks())
	if result.closeErr != "" {
		t.Fatalf("stream closed with error: %s", result.closeErr)
	}
	joined := result.joined()
	for _, leaked := range []string{"run_officejs", "read_ranges", "spreadsheet agent"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("upstream tool %q reached the caller: %q", leaked, joined)
		}
	}
	if count := strings.Count(joined, "get_weather"); count != 3 {
		t.Fatalf("caller tools echoed in %d response object(s), want 3: %q", count, joined)
	}
}

func TestStreamRelayEchoesAnEmptyToolListWhenCallerDeclaredNone(t *testing.T) {
	result := runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hi"}, upstreamToolsChunks())
	if result.closeErr != "" {
		t.Fatalf("stream closed with error: %s", result.closeErr)
	}
	joined := result.joined()
	for _, leaked := range []string{"run_officejs", "read_ranges", "spreadsheet agent"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("upstream %q reached a caller that declared none: %q", leaked, joined)
		}
	}
	if count := strings.Count(joined, `"tools":[]`); count != 3 {
		t.Fatalf("empty tool list present in %d response object(s), want 3: %q", count, joined)
	}
	if count := strings.Count(joined, `"instructions":null`); count != 3 {
		t.Fatalf("instructions cleared in %d response object(s), want 3: %q", count, joined)
	}
}

func TestNonStreamingResponseEchoesCallerTools(t *testing.T) {
	upstream := map[string]any{
		"id": "resp_5", "status": "completed", "output": []any{},
		"tools": []any{map[string]any{"type": "function", "name": "run_officejs"}},
	}
	transformed, _, _, err := transformResponseBody(jsonBytes(upstream), weatherRequest())
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(transformed, &response); err != nil {
		t.Fatal(err)
	}
	tools, _ := response["tools"].([]any)
	if len(tools) != 1 || objectValue(tools[0])["name"] != "get_weather" {
		t.Fatalf("non-streaming response advertises %#v", tools)
	}
}

// The caller's own instructions must survive the round trip: they are relayed
// upstream as a developer message, so the response object is the only place
// they can come back from.
func TestStreamRelayEchoesCallerInstructions(t *testing.T) {
	request := map[string]any{
		"model":        "gpt-6-astra",
		"input":        "hi",
		"instructions": "只说中文。",
	}
	result := runStreamTest(t, request, upstreamToolsChunks())
	if result.closeErr != "" {
		t.Fatalf("stream closed with error: %s", result.closeErr)
	}
	joined := result.joined()
	if strings.Contains(joined, "spreadsheet agent") {
		t.Fatalf("upstream instructions reached the caller: %q", joined)
	}
	if count := strings.Count(joined, "只说中文。"); count != 3 {
		t.Fatalf("caller instructions echoed in %d response object(s), want 3: %q", count, joined)
	}
}

// The host resolves the bearer token for its own outbound calls -- the control
// panel's quota card among them -- from the auth record's metadata. Omitting it
// left the panel reporting "auth token not found" for a credential that served
// traffic perfectly well.
func TestAuthRecordCarriesAccessTokenForHostInitiatedCalls(t *testing.T) {
	request, _ := json.Marshal(authParseRequest{
		Provider: Provider,
		FileName: "basispoints-account.json",
		RawJSON:  streamTestCredential(),
	})
	response, err := NewService().authParse(request)
	if err != nil {
		t.Fatal(err)
	}
	metadata := objectValue(objectValue(response["Auth"])["Metadata"])
	token, _ := metadata["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		t.Fatalf("auth metadata carries no access_token: %#v", metadata)
	}
	var stored map[string]any
	if err := json.Unmarshal(streamTestCredential(), &stored); err != nil {
		t.Fatal(err)
	}
	if token != stored["access_token"] {
		t.Fatalf("published token does not match the credential's own")
	}
}

// Basis Points reports a cache_write_tokens breakdown the Responses API does
// not define, and downstream accounting prices it as its own category at above
// the input rate. The tokens are already inside input_tokens, so the breakdown
// is dropped and cached_tokens -- which is standard, and what makes cache hits
// bill as cheap reads -- is kept.
func TestUsageDropsTheNonStandardCacheWriteBreakdown(t *testing.T) {
	upstreamUsage := func() map[string]any {
		return map[string]any{
			"input_tokens": 24664,
			"input_tokens_details": map[string]any{
				"cached_tokens":      22132,
				"cache_write_tokens": 2532,
			},
			"output_tokens": 5,
		}
	}

	assert := func(label string, usage map[string]any) {
		t.Helper()
		details := objectValue(usage["input_tokens_details"])
		if details == nil {
			t.Fatalf("%s: usage lost its input_tokens_details", label)
		}
		if _, present := details["cache_write_tokens"]; present {
			t.Fatalf("%s: cache_write_tokens survived: %#v", label, details)
		}
		if fmt.Sprint(details["cached_tokens"]) != "22132" {
			t.Fatalf("%s: cached_tokens was disturbed: %#v", label, details)
		}
		if fmt.Sprint(usage["input_tokens"]) != "24664" {
			t.Fatalf("%s: input_tokens was disturbed: %#v", label, usage)
		}
	}

	// Non-streaming.
	body := jsonBytes(map[string]any{"id": "resp_u", "status": "completed",
		"output": []any{}, "usage": upstreamUsage()})
	transformed, _, _, err := transformResponseBody(body, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(transformed, &response); err != nil {
		t.Fatal(err)
	}
	assert("non-streaming", objectValue(response["usage"]))

	// Streaming: the same object rides out on response.completed.
	chunks := []string{
		sseEvent("response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{"id": "resp_u", "status": "completed",
				"output": []any{}, "usage": upstreamUsage()},
		}) + "data: [DONE]\n\n",
	}
	result := runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hi"}, chunks)
	if result.closeErr != "" {
		t.Fatalf("stream closed with error: %s", result.closeErr)
	}
	if strings.Contains(result.joined(), "cache_write_tokens") {
		t.Fatalf("streamed usage still carries cache_write_tokens: %q", result.joined())
	}
	if !strings.Contains(result.joined(), "22132") {
		t.Fatalf("streamed usage lost cached_tokens: %q", result.joined())
	}
}

func resetImageRelay() {
	imageRelayMu.Lock()
	defer imageRelayMu.Unlock()
	imageRelay = map[string]*relayedImage{}
	imageRelayByHash = map[string]string{}
	imageRelayBytes = 0
}

func relayBytesHeld() int {
	imageRelayMu.Lock()
	defer imageRelayMu.Unlock()
	return imageRelayBytes
}

// Codex resends the entire conversation every turn, screenshots and all, so the
// same image arrives again on each turn. Storing a fresh copy per turn is what
// filled the relay in production and made ordinary PNGs fail with "could not be
// relayed"; memory must track distinct images, not images times turns.
func TestImageRelayStoresOneCopyOfAnImageResentEachTurn(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()

	screenshot := make([]byte, 512*1024)
	for i := range screenshot {
		screenshot[i] = byte(i * 7)
	}
	first, outcome := stashImage(screenshot, "image/png")
	if outcome != stashStored || first == "" {
		t.Fatalf("first stash = %v", outcome)
	}
	afterFirst := relayBytesHeld()

	for turn := 0; turn < 200; turn++ {
		token, outcome := stashImage(screenshot, "image/png")
		if outcome != stashStored {
			t.Fatalf("turn %d: stash = %v (relay filled up on re-sent bytes)", turn, outcome)
		}
		if token != first {
			t.Fatalf("turn %d: got a new token for identical bytes", turn)
		}
	}
	if held := relayBytesHeld(); held != afterFirst {
		t.Fatalf("relay grew from %d to %d bytes while the same image was resent", afterFirst, held)
	}

	// Distinct bytes must still be stored separately.
	other := make([]byte, len(screenshot))
	copy(other, screenshot)
	other[0]++
	second, outcome := stashImage(other, "image/png")
	if outcome != stashStored || second == first {
		t.Fatalf("a different image reused the first token: %v %q", outcome, second)
	}
	if held := relayBytesHeld(); held != afterFirst*2 {
		t.Fatalf("held %d bytes, want two distinct copies (%d)", held, afterFirst*2)
	}
}

// A saturated relay must not be reported as a bad image: that message sent an
// earlier investigation looking at a 116 KB PNG that was never the problem.
func TestImageRelaySeparatesSaturationFromUnusableImages(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()

	// Shrink the budget so filling it does not allocate the full production
	// 256 MB -- which the race detector would multiply several times over.
	saved := imageRelayMaxTotal
	imageRelayMaxTotal = 8 << 20
	defer func() { imageRelayMaxTotal = saved }()

	// Fill the relay to the brim with distinct images, so that even a tiny one
	// no longer fits. Each block must differ, or dedup would collapse them.
	const block = 1 << 20
	full := false
	for stashed := 0; stashed < imageRelayMaxTotal/block+2; stashed++ {
		payload := make([]byte, block)
		payload[0] = byte(stashed)
		payload[1] = byte(stashed >> 8)
		if _, outcome := stashImage(payload, "image/png"); outcome == stashFull {
			full = true
			break
		}
	}
	if !full {
		t.Fatalf("relay never reported itself full; held %d of %d bytes",
			relayBytesHeld(), imageRelayMaxTotal)
	}

	input := []any{map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{
			"type":      "input_image",
			"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("small-but-valid-png-bytes")),
		}},
	}}
	result := rewriteInlineImages(input, "https://example.test/bp")
	if result.Overflowed != 1 {
		t.Fatalf("saturated relay reported %#v, want one overflow", result)
	}
	if result.Unusable != 0 {
		t.Fatalf("saturated relay blamed the image: %#v", result)
	}
}

// Deduplicated entries are shared, so one request referencing the same
// screenshot many times draws on a single fetch budget. The budget has to
// outlast a whole turn: at the old cap of 4 the entry retired mid-request and
// the remaining references were served 404.
func TestImageRelaySurvivesManyFetchesOfOneSharedEntry(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()

	payload := []byte("shared-screenshot-bytes")
	token, outcome := stashImage(payload, "image/png")
	if outcome != stashStored {
		t.Fatalf("stash = %v", outcome)
	}
	// A single turn can easily reference one screenshot a couple of dozen times.
	for fetch := 1; fetch <= 24; fetch++ {
		if _, _, ok := takeImage(token); !ok {
			t.Fatalf("fetch %d was refused; a turn cannot finish", fetch)
		}
	}
	// Re-stashing on the next turn hands the budget back.
	again, outcome := stashImage(payload, "image/png")
	if outcome != stashStored || again != token {
		t.Fatalf("next turn did not reuse the entry: %v %q", outcome, again)
	}
	for fetch := 1; fetch <= 24; fetch++ {
		if _, _, ok := takeImage(again); !ok {
			t.Fatalf("fetch %d of the next turn was refused", fetch)
		}
	}
}

// Basis Points groups its own tools into namespaces and refuses a replayed
// function_call that does not say which one it came from:
//
//	Missing namespace for function_call 'spawn_agent'. It does not exist in
//	the default namespace.
//
// Clients have been seen replaying those items with the field stripped, which
// kills the conversation from the second tool call on. The declarations ride
// along in the same request, so the field is restored from them.
func TestReplayedNativeCallsRegainTheirDeclaredNamespace(t *testing.T) {
	catalogue := map[string]any{
		"type": "additional_tools", "id": "at_1", "role": "developer",
		"tools": []any{
			map[string]any{"type": "namespace", "name": "functions", "tools": []any{
				map[string]any{"type": "custom", "name": "exec"},
			}},
			map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
				map[string]any{"type": "function", "name": "spawn_agent"},
				map[string]any{"type": "function", "name": "list_agents"},
			}},
		},
	}
	input := []any{
		catalogue,
		map[string]any{"type": "function_call", "name": "spawn_agent",
			"call_id": "call_a", "id": "fc_a", "arguments": `{"task":"x"}`},
		map[string]any{"type": "function_call", "name": "exec",
			"call_id": "call_b", "id": "fc_b", "arguments": "{}"},
		// A tool the request never declared must be left exactly as it came.
		map[string]any{"type": "function_call", "name": "mystery_tool",
			"call_id": "call_c", "id": "fc_c", "arguments": "{}"},
		// An item that already states its namespace must not be second-guessed.
		map[string]any{"type": "function_call", "name": "list_agents",
			"call_id": "call_d", "id": "fc_d", "arguments": "{}", "namespace": "explicit"},
	}

	translated := translateInputItems(input, map[string]toolSpec{}, "namespace-test")
	got := map[string]string{}
	for _, value := range translated {
		item := objectValue(value)
		if item == nil || stringValue(item["type"]) != "function_call" {
			continue
		}
		got[stringValue(item["name"])] = stringValue(item["namespace"])
	}
	for name, want := range map[string]string{
		"spawn_agent":  "collaboration",
		"exec":         "functions",
		"mystery_tool": "",
		"list_agents":  "explicit",
	} {
		if got[name] != want {
			t.Fatalf("%s namespace = %q, want %q (all: %#v)", name, got[name], want, got)
		}
	}
}

// Basis Points rejects item ids over 64 characters. Prefixing "fc_" onto a
// 64-character client call id produced exactly the 67 it complained about.
func TestFunctionItemIDNeverExceedsTheUpstreamLimit(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, callID := range []string{long, "fc_" + long, strings.Repeat("z", 200)} {
		id := functionItemID(callID)
		if len(id) > maxItemIDLength {
			t.Fatalf("call id of %d chars became item id of %d chars", len(callID), len(id))
		}
		if !strings.HasPrefix(id, "fc_") {
			t.Fatalf("item id %q lost its fc_ prefix", id)
		}
		// The same call id must map to the same item id on every turn.
		if again := functionItemID(callID); again != id {
			t.Fatalf("item id is not deterministic: %q then %q", id, again)
		}
	}
	// Short ids keep their readable form.
	if got := functionItemID("call_abc"); got != "fc_call_abc" {
		t.Fatalf("short id rewritten to %q", got)
	}
	if got := functionItemID("fc_already"); got != "fc_already" {
		t.Fatalf("prefixed id rewritten to %q", got)
	}
	// Distinct long ids must not collapse into one.
	if functionItemID(strings.Repeat("a", 70)) == functionItemID(strings.Repeat("b", 70)) {
		t.Fatal("two different long call ids produced the same item id")
	}
}

func contextOverflowChunks() []string {
	failure := map[string]any{
		"code":    "context_length_exceeded",
		"message": "Your input exceeds the context window of this model. Please adjust your input and try again.",
		"param":   "input",
		"type":    "invalid_request_error",
	}
	return []string{
		sseEvent("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{"id": "resp_f", "status": "in_progress", "output": []any{},
				"instructions": "You are the spreadsheet agent for the ChatGPT Excel Plugin."},
		}),
		sseEvent("response.failed", map[string]any{
			"type": "response.failed", "sequence_number": 2,
			"response": map[string]any{"id": "resp_f", "status": "failed", "error": failure,
				"instructions": "You are the spreadsheet agent for the ChatGPT Excel Plugin.",
				"tools":        []any{map[string]any{"type": "function", "name": "run_officejs"}}},
		}),
	}
}

// Basis Points reports context_length_exceeded as response.failed after a 200.
// That is a terminal event: the failure already reached the caller in-band.
// Treating the stream as broken on top of it turned every such request into a
// 502, which the main site charged to the credential instead of the request.
func TestStreamRelayTreatsResponseFailedAsTerminal(t *testing.T) {
	result := runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hi"}, contextOverflowChunks())
	if result.closeErr != "" {
		t.Fatalf("a stream ending in response.failed was closed as broken: %s", result.closeErr)
	}
	joined := result.joined()
	if !strings.Contains(joined, "context_length_exceeded") {
		t.Fatalf("the failure never reached the caller: %q", joined)
	}
	if !strings.Contains(joined, "response.failed") {
		t.Fatalf("response.failed was not forwarded: %q", joined)
	}
	// The failed response object carries the same upstream prompt and
	// catalogue as a completed one, and must be cleaned the same way.
	for _, leaked := range []string{"spreadsheet agent", "run_officejs"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("response.failed still carries upstream %q: %q", leaked, joined)
		}
	}
}

func TestStreamRelayTreatsErrorEventAsTerminal(t *testing.T) {
	chunks := []string{
		sseEvent("error", map[string]any{
			"type": "error", "sequence_number": 2,
			"error": map[string]any{"code": "context_length_exceeded", "type": "invalid_request_error",
				"message": "Your input exceeds the context window of this model."},
		}),
	}
	result := runStreamTest(t, map[string]any{"model": "gpt-6-astra", "input": "hi"}, chunks)
	if result.closeErr != "" {
		t.Fatalf("a stream ending in an error event was closed as broken: %s", result.closeErr)
	}
	if !strings.Contains(result.joined(), "context_length_exceeded") {
		t.Fatalf("the error never reached the caller: %q", result.joined())
	}
}

// A request's own fault must come back as the request's fault. Basis Points
// answers even stream:false with SSE, so the in-band failure has to be lifted
// out of the stream and given its real status and envelope.
func TestNonStreamingSurfacesInBandFailureAsItsOwnStatus(t *testing.T) {
	raw := []byte(strings.Join(contextOverflowChunks(), ""))
	_, err := parseFinalStreamResponse(raw)
	if err == nil {
		t.Fatal("a failed response was returned as a success")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is %T, want *APIError", err)
	}
	if apiErr.Status != 400 {
		t.Fatalf("context_length_exceeded surfaced as %d, want 400", apiErr.Status)
	}
	var envelope map[string]any
	if jsonErr := json.Unmarshal([]byte(err.Error()), &envelope); jsonErr != nil {
		t.Fatalf("error lost its JSON envelope (%v): %s", jsonErr, err.Error())
	}
	if objectValue(envelope["error"])["type"] != "invalid_request_error" {
		t.Fatalf("error.type was lost: %s", err.Error())
	}
}

// response.incomplete is a finished response, not an error: the Responses API
// returns it with 200 and status "incomplete".
func TestNonStreamingReturnsAnIncompleteResponse(t *testing.T) {
	raw := []byte(sseEvent("response.incomplete", map[string]any{
		"type": "response.incomplete",
		"response": map[string]any{"id": "resp_i", "status": "incomplete",
			"incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output": []any{}},
	}))
	response, err := parseFinalStreamResponse(raw)
	if err != nil {
		t.Fatalf("an incomplete response was reported as an error: %v", err)
	}
	if response["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", response["status"])
	}
}
