package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// h20, smaller pieces that the larger ceilings made necessary: the fetch
// allowance follows the image count, an oversized image is not decoded before
// it is refused, and a large request is not parsed for a credential that is
// already at its cap.

// One screenshot referenced many times in one request: the upstream fetches
// every reference, so the shared entry has to survive that many fetches.
func TestH20SharedImageSurvivesAsManyFetchesAsARequestMayReference(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()
	if imageRelayMaxFetches < imageRelayMaxPerRequest {
		t.Fatalf("an entry is retired after %d fetches, but one request may reference it %d times", imageRelayMaxFetches, imageRelayMaxPerRequest)
	}

	same := h20ImageURL(77, 4096)
	parts := make([]any, 0, 300)
	for i := 0; i < 300; i++ {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": same})
	}
	result := rewriteInlineImages(h20Message(parts), "https://example.test/bp")
	if result.failed() || result.Rewritten != 300 {
		t.Fatalf("300 references to one image: result = %+v", result)
	}
	if held := relayBytesHeld(); held != 4096 {
		t.Fatalf("300 identical images are held as %d bytes, want one copy (4096)", held)
	}
	url := stringValue(parts[0].(map[string]any)["image_url"])
	token := url[strings.Index(url, "?t=")+3:]
	for i := 0; i < 300; i++ {
		if _, _, ok := takeImage(token); !ok {
			t.Fatalf("fetch %d of 300 found the entry gone", i+1)
		}
	}
}

// An image clearly over the per-image limit is turned away on its declared
// length; it is not decoded first. The 48 MiB request ceiling used to bound
// that waste and the 192 MiB one no longer does.
func TestH20OversizedImageIsNotDecodedBeforeItIsRefused(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()

	// 100 MiB of zero bytes as valid base64: within the request ceiling, far
	// over the 12 MiB per-image limit.
	huge := "data:image/png;base64," + strings.Repeat("A", (100<<20)/3*4)
	input := h20Message([]any{map[string]any{"type": "input_image", "image_url": huge}})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	result := rewriteInlineImages(input, "https://example.test/bp")
	runtime.ReadMemStats(&after)

	if result.Unusable != 1 || result.Rewritten != 0 || result.OverBudget != 0 || result.Overflowed != 0 {
		t.Fatalf("a 100 MiB image: result = %+v, want it refused as unusable", result)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Fatalf("refusing a 100 MiB image allocated %d MiB: it was decoded first", grew>>20)
	}

	// An image a little under the limit is still relayed, and one a little
	// over it is still refused by the exact check after decoding.
	under := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, imageRelayMaxBytes-1024))
	over := "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, imageRelayMaxBytes+1024))
	result = rewriteInlineImages(h20Message([]any{
		map[string]any{"type": "input_image", "image_url": under},
		map[string]any{"type": "input_image", "image_url": over},
	}), "https://example.test/bp")
	if result.Rewritten != 1 || result.Unusable != 1 {
		t.Fatalf("one image under and one over the per-image limit: result = %+v", result)
	}

	// With no public base nothing can be relayed, and nothing is decoded.
	big := "data:image/png;base64," + strings.Repeat("A", (11<<20)/3*4)
	input = h20Message([]any{map[string]any{"type": "input_image", "image_url": big}})
	runtime.GC()
	runtime.ReadMemStats(&before)
	result = rewriteInlineImages(input, "")
	runtime.ReadMemStats(&after)
	if result.Unusable != 1 || result.Rewritten != 0 {
		t.Fatalf("no public base: result = %+v", result)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4<<20 {
		t.Fatalf("with no public base an 11 MiB image still cost %d MiB of allocation", grew>>20)
	}
}

func h20ExecutorRaw(t *testing.T, authID string, body []byte) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(ExecutorRequest{
		AuthID:          authID,
		OriginalRequest: body,
		StorageJSON:     fakeCredential(t),
		AuthMetadata:    map[string]any{"file_name": "probe.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func h20Kind(err error) string {
	if coded, ok := err.(*APIError); ok {
		return coded.Kind
	}
	if err == nil {
		return ""
	}
	return "other: " + err.Error()
}

// A large request that lands on a credential already at its cap is bounced
// before its body is parsed, so the next credential is not the second one to
// decode the same images.
func TestH20LargeRequestOnAFullCredentialIsBouncedBeforeDecoding(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()
	s := NewService()
	s.mu.Lock()
	s.cfg.PublicBaseURL = "https://example.test/bp"
	limit := s.cfg.MaxConcurrencyPerCredential
	s.mu.Unlock()
	if limit <= 0 {
		t.Fatalf("the default per-credential cap is %d; this test needs a cap", limit)
	}
	const authID = "auth-h20-early"

	large, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": h20Message([]any{
		map[string]any{"type": "input_text", "text": "look"},
		map[string]any{"type": "input_image", "image_url": h20ImageURL(900, 9<<20)},
	})})
	if err != nil {
		t.Fatal(err)
	}
	if len(large) < earlyCapBodyBytes {
		t.Fatalf("the test body (%d bytes) is below the early-check size", len(large))
	}
	// Large and malformed (nothing to answer), and small and malformed.
	largeEmpty, _ := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": []any{}, "padding": strings.Repeat("x", earlyCapBodyBytes)})
	smallEmpty := []byte(`{"model":"gpt-5.6-sol","input":[]}`)

	// Fill the credential to its cap.
	var releases []func()
	for i := 0; i < limit; i++ {
		release, ok := s.gate.acquire(authID, limit)
		if !ok {
			t.Fatalf("could not take slot %d of %d", i+1, limit)
		}
		releases = append(releases, release)
	}

	_, err = s.execute(h20ExecutorRaw(t, authID, large), true)
	if h20Kind(err) != concurrencyLimitMarker {
		t.Fatalf("large request on a full credential: got %v, want the concurrency bounce", err)
	}
	if held := relayBytesHeld(); held != 0 {
		t.Fatalf("the bounced request's image was decoded and stashed anyway (%d bytes held)", held)
	}
	// The documented side effect: a large malformed request hears "busy" first.
	if _, err = s.execute(h20ExecutorRaw(t, authID, largeEmpty), true); h20Kind(err) != concurrencyLimitMarker {
		t.Fatalf("large malformed request on a full credential: got %v, want the concurrency bounce", err)
	}
	// A small request is not looked at early: it is still told what is wrong with it.
	if _, err = s.execute(h20ExecutorRaw(t, authID, smallEmpty), true); h20Kind(err) != "empty_input" {
		t.Fatalf("small malformed request on a full credential: got %v, want empty_input", err)
	}

	// With room on the credential the same large requests go the usual way.
	for _, release := range releases {
		release()
	}
	if _, err = s.execute(h20ExecutorRaw(t, authID, largeEmpty), true); h20Kind(err) != "empty_input" {
		t.Fatalf("large malformed request on a free credential: got %v, want empty_input", err)
	}
	_, err = s.execute(h20ExecutorRaw(t, authID, large), true)
	if h20Kind(err) == concurrencyLimitMarker {
		t.Fatalf("large request on a free credential was bounced: %v", err)
	}
	if held := relayBytesHeld(); held != 9<<20 {
		t.Fatalf("the admitted request's image should have been relayed: %d bytes held, want %d", held, 9<<20)
	}
	if now := s.gate.current(authID); now != 0 {
		t.Fatalf("a failed call left %d slots taken", now)
	}
}
