package basispoints

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func h26ResetWebSearch(t *testing.T) func() {
	t.Helper()
	webSearchFailures.Lock()
	webSearchFailures.until = map[string]time.Time{}
	webSearchFailures.Unlock()
	saved := webSearchNow
	return func() {
		webSearchNow = saved
		webSearchFailures.Lock()
		webSearchFailures.until = map[string]time.Time{}
		webSearchFailures.Unlock()
	}
}

func h26Body(t *testing.T, cfg Config, scope string) map[string]any {
	t.Helper()
	source := map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"role": "user", "content": "what changed in rust?"}},
	}
	body, err := prepareResponsesBodyFor(source, cfg, scope, persona{}, identityOff)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func h26LastIsNotice(body map[string]any) bool {
	items, _ := body["input"].([]any)
	if len(items) == 0 {
		return false
	}
	last, _ := items[len(items)-1].(map[string]any)
	raw, _ := json.Marshal(last)
	return last["role"] == "developer" && strings.Contains(string(raw), "The web search you just attempted failed")
}

func TestH26WebSearchGuardDefaultIsOffAndByteIdentical(t *testing.T) {
	defer h26ResetWebSearch(t)()
	cfg := defaultConfig()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.WebSearchGuard != webSearchGuardOff || cfg.WebSearchGuardTTLSeconds != defaultWebSearchGuardTTLSeconds {
		t.Fatalf("defaults: %q %d", cfg.WebSearchGuard, cfg.WebSearchGuardTTLSeconds)
	}
	before := h26Body(t, cfg, "scope-a")
	noteWebSearchFailure("scope-a", time.Minute)
	after := h26Body(t, cfg, "scope-a")
	if !reflect.DeepEqual(before, after) || h26LastIsNotice(after) {
		t.Fatal("off must not change the upstream body even after a noted failure")
	}
}

func TestH26WebSearchGuardRetryOnlyAfterFailureAndOnlyThatScope(t *testing.T) {
	defer h26ResetWebSearch(t)()
	cfg := defaultConfig()
	cfg.WebSearchGuard = webSearchGuardRetry
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if h26LastIsNotice(h26Body(t, cfg, "scope-a")) {
		t.Fatal("no failure yet: no notice")
	}
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	webSearchNow = func() time.Time { return base }
	noteWebSearchFailure("scope-a", 15*time.Minute)
	withNotice := h26Body(t, cfg, "scope-a")
	if !h26LastIsNotice(withNotice) {
		t.Fatal("failure noted: notice expected at the end of input")
	}
	plain := h26Body(t, cfg, "scope-b")
	if h26LastIsNotice(plain) {
		t.Fatal("another conversation must not get the notice")
	}
	// Everything before the appended item is exactly the unguarded body.
	items := withNotice["input"].([]any)
	if !reflect.DeepEqual(items[:len(items)-1], plain["input"].([]any)) {
		t.Fatal("prefix changed: the notice must only be appended")
	}
	if h26LastIsNotice(h26Body(t, cfg, "")) {
		t.Fatal("empty scope must never match")
	}
	webSearchNow = func() time.Time { return base.Add(16 * time.Minute) }
	if h26LastIsNotice(h26Body(t, cfg, "scope-a")) {
		t.Fatal("notice must expire after the ttl")
	}
}

func TestH26WebSearchGuardAlways(t *testing.T) {
	defer h26ResetWebSearch(t)()
	cfg := defaultConfig()
	cfg.WebSearchGuard = webSearchGuardAlways
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if !h26LastIsNotice(h26Body(t, cfg, "")) {
		t.Fatal("always: notice expected")
	}
}

func TestH26WebSearchGuardConfigValidation(t *testing.T) {
	for _, bad := range []Config{{WebSearchGuard: "sometimes"}, {WebSearchGuard: "retry", WebSearchGuardTTLSeconds: 5}} {
		c := defaultConfig()
		c.WebSearchGuard, c.WebSearchGuardTTLSeconds = bad.WebSearchGuard, bad.WebSearchGuardTTLSeconds
		if err := c.normalize(); err == nil {
			t.Fatalf("expected rejection for %+v", bad)
		}
	}
}

func h26Event(kind string, payload map[string]any) string {
	payload["type"] = kind
	raw, _ := json.Marshal(payload)
	return "event: " + kind + "\ndata: " + string(raw) + "\n\n"
}

func TestH26StreamMarksWebSearchAndFailureNotesScope(t *testing.T) {
	defer h26ResetWebSearch(t)()
	progress := &streamProgress{}
	relay := newStreamRelay(NewService(), "client", weatherRequest(), usageTarget{}, "gpt-6-astra")
	relay.progress = progress
	ws := map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed",
		"action": map[string]any{"type": "search", "query": "q"}}
	if err := relay.feed([]byte(h26Event("response.output_item.added", map[string]any{"output_index": 0, "item": ws}))); err != nil {
		t.Fatal(err)
	}
	if !progress.webSearch.Load() {
		t.Fatal("web_search_call must be noticed")
	}
	_ = relay.feed([]byte(h26Event("response.failed", map[string]any{"response": map[string]any{
		"id": "resp_1", "status": "failed", "output": []any{ws},
		"error": map[string]any{"code": "server_error", "message": "An error occurred"}}})))
	cfg := defaultConfig()
	cfg.WebSearchGuard = webSearchGuardRetry
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	noteWebSearchStreamOutcome(cfg, "scope-x", nil, progress)
	if progress.upstreamTerminal() == "response.failed" && !webSearchFailedRecently("scope-x") {
		t.Fatal("a failed turn that searched must note its conversation")
	}
	if progress.upstreamTerminal() != "response.failed" {
		// The terminal may surface as an error instead; either way it counts.
		noteWebSearchStreamOutcome(cfg, "scope-y", errTestStream, progress)
		if !webSearchFailedRecently("scope-y") {
			t.Fatal("a failed turn that searched must note its conversation")
		}
	}
	// A turn that searched and completed is not a failure.
	ok := &streamProgress{}
	ok.webSearch.Store(true)
	ok.terminal.Store("response.completed")
	noteWebSearchStreamOutcome(cfg, "scope-z", nil, ok)
	if webSearchFailedRecently("scope-z") {
		t.Fatal("completed turn must not be noted")
	}
	// A failed turn without a search is not noted either.
	nows := &streamProgress{}
	nows.terminal.Store("response.failed")
	noteWebSearchStreamOutcome(cfg, "scope-w", nil, nows)
	if webSearchFailedRecently("scope-w") {
		t.Fatal("failure without web search must not be noted")
	}
}

var errTestStream = fail(502, "invalid_upstream_response", "test")
