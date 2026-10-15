package basispoints

// Regression tests for the P1 fixes. Each one guards a way a single request
// used to take the whole credential pool down, or quietly eat its capacity.

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- status classification --------------------------------------------------

func TestInBandFailureStatusNeverBlamesTheCredential(t *testing.T) {
	cases := []struct {
		name    string
		failure map[string]any
		want    int
	}{
		{"flagged prompt", map[string]any{"type": "invalid_prompt", "message": "Your prompt was flagged"}, 400},
		{"invalid request", map[string]any{"type": "invalid_request_error"}, 400},
		{"context too long", map[string]any{"code": "context_length_exceeded", "message": "too long"}, 400},
		{"invalid value", map[string]any{"type": "invalid_value"}, 400},
		{"no type at all", map[string]any{"message": "something"}, 400},
		{"empty object", map[string]any{}, 400},
		{"non-string type", map[string]any{"type": 7}, 400},
		{"rate limited", map[string]any{"type": "rate_limit_exceeded"}, 429},
		{"quota gone", map[string]any{"code": "insufficient_quota"}, 429},
		{"genuine upstream fault", map[string]any{"type": "server_error"}, 502},
	}
	for _, c := range cases {
		if got := inBandFailureStatus(c.failure); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	// The property that matters: the upstream answered 200, so only an
	// explicit server fault may be charged to the credential.
	for _, c := range cases {
		if c.want != 502 && inBandFailureStatus(c.failure) >= 500 {
			t.Errorf("%s: a client-caused failure was reported as 5xx, which cools every credential", c.name)
		}
	}
}

func TestStreamCloseReasonSeparatesTheCauses(t *testing.T) {
	cases := map[int]string{
		499: "[client_disconnected] ",
		504: "[upstream_timeout] ",
		500: "[plugin_bug] ",
		413: "[request_error] ",
		502: "[upstream_error] ",
	}
	for status, want := range cases {
		if got := streamCloseReason(fail(status, "x", "y")); got != want {
			t.Errorf("status %d: reason %q, want %q", status, got, want)
		}
	}
	// A client hang-up must not read as an upstream failure any more.
	if streamCloseReason(fail(499, "client_closed", "gone")) == streamCloseReason(fail(502, "upstream_error", "dead")) {
		t.Error("a client disconnect is still indistinguishable from a dead upstream")
	}
}

// --- organisation rate-limit retry -----------------------------------------

func TestOrgRateLimitBackoff(t *testing.T) {
	const realMessage = `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached for gpt-6-sol in organization org-msnGs3 on tokens per min (TPM): Limit 40000000, Used 39982266, Requested 75363. Please try again in 86ms."}}`
	wait, ok := orgRateLimitBackoff(fail(429, "upstream_error", realMessage))
	if !ok {
		t.Fatal("the real production message was not recognised as retryable")
	}
	if wait < 86*time.Millisecond || wait > time.Second {
		t.Errorf("wait %v is not the advertised 86ms plus a small margin", wait)
	}

	// Anything that is not a momentary pool refusal must not be retried.
	notRetryable := []error{
		fail(429, "upstream_error", `{"error":{"type":"usage_limit_reached","plan_type":"pro"}}`),
		fail(403, "upstream_error", "403: This request was blocked by our usage policy."),
		fail(500, "upstream_error", "boom"),
		fail(429, "upstream_error", "Rate limit reached ... Please try again in 45s."),
	}
	for _, err := range notRetryable {
		if _, ok := orgRateLimitBackoff(err); ok {
			t.Errorf("retried something it should not have: %v", err)
		}
	}
}

func TestWithOrgRateLimitRetrySucceedsOnSecondTry(t *testing.T) {
	calls := 0
	limited := fail(429, "upstream_error", "on tokens per min (TPM): Limit 1, Used 1. Please try again in 1ms.")
	got, err := withOrgRateLimitRetry(func() (string, error) {
		calls++
		if calls == 1 {
			return "", limited
		}
		return "served", nil
	})
	if err != nil || got != "served" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if calls != 2 {
		t.Errorf("attempts = %d, want 2", calls)
	}

	// It must give up rather than spin.
	calls = 0
	if _, err := withOrgRateLimitRetry(func() (string, error) { calls++; return "", limited }); err == nil {
		t.Error("a permanently full pool eventually has to surface")
	}
	if calls != 3 {
		t.Errorf("attempts = %d, want 3", calls)
	}

	// A non-retryable error must cost exactly one attempt.
	calls = 0
	_, _ = withOrgRateLimitRetry(func() (string, error) { calls++; return "", fail(403, "x", "blocked by our usage policy") })
	if calls != 1 {
		t.Errorf("a 403 was retried %d times", calls)
	}
}

// --- schema walk ------------------------------------------------------------

// TestSchemaMatchesIsBounded builds the shape that used to burn seventeen
// seconds inside the relay goroutine while holding a credential's slot.
func TestSchemaMatchesIsBounded(t *testing.T) {
	// A three-way type alternative nested deeply: 3^depth branches before the
	// budget existed.
	schema := map[string]any{"type": []any{"object", "object", "object"}, "required": []any{"never-present"}}
	value := any(map[string]any{})
	for i := 0; i < 400; i++ {
		schema = map[string]any{
			"type":       []any{"object", "object", "object"},
			"properties": map[string]any{"a": schema},
		}
		value = map[string]any{"a": value}
	}
	start := time.Now()
	_ = schemaMatches(value, schema)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("schemaMatches took %v -- the walk is still unbounded", elapsed)
	}

	// Deep single-alternative nesting was the other half of the cost.
	deep := map[string]any{"type": "object"}
	deepValue := any(map[string]any{})
	for i := 0; i < 4000; i++ {
		deep = map[string]any{"type": "object", "properties": map[string]any{"a": deep}}
		deepValue = map[string]any{"a": deepValue}
	}
	start = time.Now()
	_ = schemaMatches(deepValue, deep)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("deep schema took %v", elapsed)
	}
}

// TestSchemaMatchesStillDiscriminates checks the bound did not turn the check
// into a rubber stamp for ordinary schemas, and that it fails OPEN rather than
// closed when it does give up.
func TestSchemaMatchesStillDiscriminates(t *testing.T) {
	weather := map[string]any{
		"type":     "object",
		"required": []any{"city"},
		"properties": map[string]any{
			"city":  map[string]any{"type": "string"},
			"days":  map[string]any{"type": "integer"},
			"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"exact": map[string]any{"type": []any{"string", "number", "boolean", "null", "array", "object"}},
		},
	}
	if !schemaMatches(map[string]any{"city": "Tokyo", "days": 3, "tags": []any{"a"}, "exact": true}, weather) {
		t.Error("a valid call was rejected")
	}
	if schemaMatches(map[string]any{"days": 3}, weather) {
		t.Error("a call missing a required property was accepted")
	}
	if schemaMatches(map[string]any{"city": 7}, weather) {
		t.Error("a wrongly typed property was accepted")
	}
	// All six legal JSON Schema alternatives must still be considered: capping
	// the list would reject valid calls and relay a raw transport item to a
	// client that cannot run it.
	sixWay := map[string]any{"type": []any{"string", "number", "boolean", "null", "array", "object"}}
	if !schemaMatches(map[string]any{"any": "shape"}, sixWay) {
		t.Error("the sixth type alternative was not considered")
	}
}

// TestSchemaMatchesDoesNotMutateTheCallersSchema guards the correction the
// review insisted on: the spec maps are aliased by the parsed request and are
// echoed back to the caller, so the alternatives branch must copy.
func TestSchemaMatchesDoesNotMutateTheCallersSchema(t *testing.T) {
	inner := map[string]any{"type": []any{"string", "number"}}
	schema := map[string]any{"type": "object", "properties": map[string]any{"a": inner}}
	_ = schemaMatches(map[string]any{"a": 5}, schema)
	got, ok := inner["type"].([]any)
	if !ok || len(got) != 2 {
		t.Fatalf("the caller's schema was rewritten: %#v", inner["type"])
	}
}

// --- image budget -----------------------------------------------------------

func TestImageBudgetRejectsBeforeDecoding(t *testing.T) {
	// One image past the per-request byte ceiling, declared but never decoded.
	huge := "data:image/png;base64," + strings.Repeat("A", (imageRelayMaxRequestBytes/3*4)+1024)
	input := []any{map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_image", "image_url": huge}},
	}}
	start := time.Now()
	result := rewriteInlineImages(input, "https://example.invalid/relay")
	if result.Rewritten != 0 || result.OverBudget != 1 {
		t.Errorf("result = %+v, want it refused on the declared length", result)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v -- it decoded the payload before refusing it", elapsed)
	}

	// The count ceiling has to sit well above what live traffic sends: real
	// requests were measured at up to 40 inline images, and a cap of 24
	// rejected them with 413.
	if imageRelayMaxPerRequest < 64 {
		t.Fatalf("per-request image cap %d is below observed real traffic (40)", imageRelayMaxPerRequest)
	}
	small := "data:image/png;base64," + strings.Repeat("A", 64)
	parts := make([]any, 0, imageRelayMaxPerRequest+5)
	for i := 0; i < imageRelayMaxPerRequest+5; i++ {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": small})
	}
	result = rewriteInlineImages([]any{map[string]any{"type": "message", "role": "user", "content": parts}},
		"https://example.invalid/relay")
	if result.Rewritten+result.Unusable > imageRelayMaxPerRequest {
		t.Errorf("accepted %d images, cap is %d", result.Rewritten+result.Unusable, imageRelayMaxPerRequest)
	}
	if result.OverBudget < 5 {
		t.Errorf("result = %+v, want the excess reported as over the per-request budget", result)
	}
}

func TestDeclaredPayloadBytes(t *testing.T) {
	// Four base64 characters carry three bytes.
	if got := declaredPayloadBytes("data:image/png;base64," + strings.Repeat("A", 4000)); got != 3000 {
		t.Errorf("declared = %d, want 3000", got)
	}
	if got := declaredPayloadBytes("no-comma-here"); got != 0 {
		t.Errorf("declared = %d for a malformed URL, want 0", got)
	}
}

// TestNamespaceSurvivesARequestThatStopsDeclaringTools reproduces the live 400:
// a conversation declares its tools once, then replays calls without the
// declarations, and every later turn was rejected with
// "Missing namespace for function_call 'spawn_agent'".
func TestNamespaceSurvivesARequestThatStopsDeclaringTools(t *testing.T) {
	scope := "conv-namespace-test"
	declaring := []any{
		map[string]any{"type": "additional_tools", "tools": []any{
			map[string]any{"type": "namespace", "name": "agents", "tools": []any{
				map[string]any{"name": "spawn_agent"},
				map[string]any{"name": "wait_agent"},
			}},
		}},
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "go"}}},
	}
	_ = translateInputItems(declaring, map[string]toolSpec{}, scope)

	// A later turn of the same conversation: no declarations at all, just a
	// replayed call. This is the shape captured from production.
	replaying := []any{
		map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "again"}}},
		map[string]any{"type": "function_call", "name": "spawn_agent", "call_id": "c9", "arguments": "{}"},
	}
	got := translateInputItems(replaying, map[string]toolSpec{}, scope)

	var call map[string]any
	for _, item := range got {
		if object := objectValue(item); object != nil && stringValue(object["name"]) == "spawn_agent" {
			call = object
		}
	}
	if call == nil {
		t.Fatal("the replayed call disappeared")
	}
	if ns := stringValue(call["namespace"]); ns != "agents" {
		t.Errorf("namespace = %q, want \"agents\" -- the upstream rejects the whole turn without it", ns)
	}

	// Another conversation must not inherit it.
	other := translateInputItems(replaying, map[string]toolSpec{}, "a-different-conversation")
	for _, item := range other {
		if object := objectValue(item); object != nil && stringValue(object["name"]) == "spawn_agent" {
			if ns := stringValue(object["namespace"]); ns != "" {
				t.Errorf("a different conversation inherited namespace %q", ns)
			}
		}
	}
}

// TestRelayedImageIsCacheableButPrivate pins the two properties that matter:
// the fetcher may reuse the bytes it was given, and no shared cache may keep
// them. A copy must also not outlive the relay entry itself.
func TestRelayedImageIsCacheableButPrivate(t *testing.T) {
	token, outcome := stashImage([]byte("\x89PNG\r\n\x1a\n-not-a-real-image"), "image/png")
	if outcome != stashStored {
		t.Fatalf("could not stash a test image: %v", outcome)
	}
	response := serveRelayedImage(map[string][]string{imageRelayTokenParam: {token}})
	if response["StatusCode"] != 200 {
		t.Fatalf("serve returned %v", response["StatusCode"])
	}
	headers := response["Headers"].(map[string][]string)
	cache := strings.Join(headers["Cache-Control"], "")
	if strings.Contains(cache, "no-store") {
		t.Error("no-store is back: every turn will re-download every image")
	}
	if !strings.Contains(cache, "private") {
		t.Errorf("Cache-Control = %q -- a shared cache could keep one user's image", cache)
	}
	wantAge := "max-age=" + strconv.Itoa(int(imageRelayTTL/time.Second))
	if !strings.Contains(cache, wantAge) {
		t.Errorf("Cache-Control = %q, want %s so a cached copy cannot outlive the relay entry", cache, wantAge)
	}
	if !strings.Contains(strings.Join(headers["X-Robots-Tag"], ""), "noindex") {
		t.Error("lost the noindex hint")
	}
}
