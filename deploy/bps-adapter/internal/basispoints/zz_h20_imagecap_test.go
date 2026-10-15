package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// h20: the per-request inline-image ceilings went from 128 images / 48 MiB to
// 500 images / 192 MiB, and a request that exceeds them is now told so instead
// of being told that the shared relay is full.

// h20ImageURL returns an inline image of the given decoded size whose bytes
// differ per seed, so the relay's content dedup cannot collapse two of them.
func h20ImageURL(seed, size int) string {
	raw := make([]byte, size)
	raw[0], raw[1], raw[2] = byte(seed), byte(seed>>8), byte(seed>>16)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func h20Message(parts []any) []any {
	return []any{map[string]any{"type": "message", "role": "user", "content": parts}}
}

func TestH20PerRequestCeilings(t *testing.T) {
	if imageRelayMaxRequestBytes != 192<<20 {
		t.Fatalf("per-request byte ceiling = %d MiB, want 192", imageRelayMaxRequestBytes>>20)
	}
	if imageRelayMaxPerRequest != 500 {
		t.Fatalf("per-request image count ceiling = %d, want 500", imageRelayMaxPerRequest)
	}
	// The shared pool has to hold several full-size requests at once, or one
	// heavy conversation could lock every other image request out.
	if imageRelayMaxTotal < 8*imageRelayMaxRequestBytes {
		t.Fatalf("shared pool (%d MiB) holds fewer than 8 full requests (%d MiB each)",
			imageRelayMaxTotal>>20, imageRelayMaxRequestBytes>>20)
	}
}

func TestH20AcceptsRequestsTheOldCeilingsRefused(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()

	// Seven distinct 11 MiB images: 77 MiB, over the old 48 MiB ceiling and
	// under the new one, each within the unchanged 12 MiB per-image limit.
	const each = 11 << 20
	parts := make([]any, 0, 7)
	for i := 0; i < 7; i++ {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": h20ImageURL(i+1, each)})
	}
	result := rewriteInlineImages(h20Message(parts), "https://example.test/bp")
	if result.failed() || result.Rewritten != 7 {
		t.Fatalf("77 MiB of images: result = %+v, want all 7 relayed", result)
	}
	if held := relayBytesHeld(); held != 7*each {
		t.Fatalf("relay holds %d bytes, want %d", held, 7*each)
	}
	for i, part := range parts {
		url := stringValue(part.(map[string]any)["image_url"])
		if !strings.HasPrefix(url, "https://example.test/bp/image?t=") {
			t.Fatalf("part %d was not rewritten to a relay URL: %.60s", i, url)
		}
	}

	// 160 thumbnails in one request: over the old count ceiling of 128. This
	// is the shape of a real request refused on 2026-10-06 (5.5 MB body).
	resetImageRelay()
	small := make([]any, 0, 160)
	for i := 0; i < 160; i++ {
		small = append(small, map[string]any{"type": "input_image", "image_url": h20ImageURL(1000+i, 30<<10)})
	}
	result = rewriteInlineImages(h20Message(small), "https://example.test/bp")
	if result.failed() || result.Rewritten != 160 {
		t.Fatalf("160 small images: result = %+v, want all relayed", result)
	}
}

func TestH20OverBudgetIsCountedApartFromAFullRelay(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()

	// Bytes: declared past the ceiling, refused before anything is decoded.
	huge := "data:image/png;base64," + strings.Repeat("A", (imageRelayMaxRequestBytes/3*4)+1024)
	result := rewriteInlineImages(h20Message([]any{map[string]any{"type": "input_image", "image_url": huge}}),
		"https://example.test/bp")
	if result.OverBudget != 1 || result.Overflowed != 0 || result.Unusable != 0 || result.Rewritten != 0 {
		t.Fatalf("over the byte ceiling: result = %+v, want exactly one over-budget image", result)
	}
	if !result.failed() {
		t.Fatalf("an over-budget request must fail rather than go upstream without its image")
	}
	if held := relayBytesHeld(); held != 0 {
		t.Fatalf("an over-budget image was stashed anyway (%d bytes held)", held)
	}

	// Count: one image too many refuses the request, and nothing is relayed.
	resetImageRelay()
	many := make([]any, 0, imageRelayMaxPerRequest+1)
	for i := 0; i < imageRelayMaxPerRequest+1; i++ {
		many = append(many, map[string]any{"type": "input_image", "image_url": h20ImageURL(5000+i, 64)})
	}
	result = rewriteInlineImages(h20Message(many), "https://example.test/bp")
	if result.OverBudget != 1 || result.Overflowed != 0 || result.Rewritten != 0 {
		t.Fatalf("one image over the count ceiling: result = %+v", result)
	}
	if held := relayBytesHeld(); held != 0 {
		t.Fatalf("a request refused on its image count still stashed %d bytes", held)
	}
}

// A refused request must cost nothing: no image of it may be decoded or
// stashed. Deciding the budget image by image used to relay everything that
// still fitted, which at the new ceiling would park up to 192 MiB of bytes no
// upstream will ever fetch in the shared relay, on every retry.
func TestH20RefusedRequestStashesNothing(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()

	// Eighteen images of 11 MiB each: 198 MiB, six MiB over the ceiling. The
	// first seventeen fit, so an image-by-image budget would relay them.
	img := "data:image/png;base64," + strings.Repeat("A", (11<<20)/3*4)
	parts := make([]any, 0, 18)
	for i := 0; i < 18; i++ {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": img})
	}
	result := rewriteInlineImages(h20Message(parts), "https://example.test/bp")
	if result.OverBudget != 1 || result.Rewritten != 0 || result.Unusable != 0 || result.Overflowed != 0 {
		t.Fatalf("198 MiB of images: result = %+v, want the request refused with nothing relayed", result)
	}
	if held := relayBytesHeld(); held != 0 {
		t.Fatalf("a refused request left %d bytes in the relay", held)
	}
	for i, part := range parts {
		if url := stringValue(part.(map[string]any)["image_url"]); !strings.HasPrefix(url, "data:") {
			t.Fatalf("part %d of a refused request was rewritten to %.60s", i, url)
		}
	}

	// Images that are already URLs are not this relay's to carry and do not
	// count against either ceiling.
	urls := make([]any, 0, imageRelayMaxPerRequest+10)
	for i := 0; i < imageRelayMaxPerRequest+10; i++ {
		urls = append(urls, map[string]any{"type": "input_image", "image_url": "https://example.test/pictures/1.png"})
	}
	result = rewriteInlineImages(h20Message(urls), "https://example.test/bp")
	if result.failed() {
		t.Fatalf("%d plain image URLs were refused: %+v", len(urls), result)
	}
}

func h20Prepare(t *testing.T, parts []any) *APIError {
	t.Helper()
	s := NewService()
	s.mu.Lock()
	s.cfg.PublicBaseURL = "https://example.test/bp"
	s.mu.Unlock()
	body, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": h20Message(parts)})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.prepareRequest(ExecutorRequest{
		OriginalRequest: body,
		StorageJSON:     fakeCredential(t),
		AuthMetadata:    map[string]any{"file_name": "probe.json"},
	})
	if err == nil {
		return nil
	}
	coded, ok := err.(*APIError)
	if !ok {
		t.Fatalf("prepareRequest returned %T (%v), want *APIError", err, err)
	}
	return coded
}

// The two refusals share a status, because the host reads 413 as the request's
// fault and leaves the credential alone, but not a message: only one of them
// is worth retrying.
func TestH20PrepareRequestNamesTheRealReason(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()

	// One request over the per-request ceiling (by count, which is cheap).
	parts := []any{map[string]any{"type": "input_text", "text": "look"}}
	for i := 0; i < imageRelayMaxPerRequest+1; i++ {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": h20ImageURL(9000+i, 64)})
	}
	got := h20Prepare(t, parts)
	if got == nil || got.Status != 413 || got.Kind != "image_request_too_large" {
		t.Fatalf("oversized request: got %+v, want 413 image_request_too_large", got)
	}
	if !strings.Contains(got.Message, "500 images") || !strings.Contains(got.Message, "192 MiB") {
		t.Fatalf("oversized request: message does not state the limits: %q", got.Message)
	}
	if strings.Contains(got.Message, "retry in a few minutes") {
		t.Fatalf("oversized request is still told to retry: %q", got.Message)
	}

	// The shared relay genuinely full: still the old answer.
	resetImageRelay()
	saved := imageRelayMaxTotal
	imageRelayMaxTotal = 1 << 20
	defer func() { imageRelayMaxTotal = saved }()
	filler := make([]byte, 1<<20)
	filler[0] = 0x7f
	if _, outcome := stashImage(filler, "image/png"); outcome != stashStored {
		t.Fatalf("could not fill the relay: outcome %v", outcome)
	}
	got = h20Prepare(t, []any{
		map[string]any{"type": "input_text", "text": "look"},
		map[string]any{"type": "input_image", "image_url": h20ImageURL(42, 4096)},
	})
	if got == nil || got.Status != 413 || got.Kind != "image_relay_full" {
		t.Fatalf("full relay: got %+v, want 413 image_relay_full", got)
	}
	if !strings.Contains(got.Message, "retry in a few minutes") {
		t.Fatalf("full relay: message lost its retry hint: %q", got.Message)
	}

	// And a request within both ceilings is not refused for its images.
	resetImageRelay()
	imageRelayMaxTotal = saved
	got = h20Prepare(t, []any{
		map[string]any{"type": "input_text", "text": "look"},
		map[string]any{"type": "input_image", "image_url": h20ImageURL(43, 4096)},
	})
	if got != nil && (got.Kind == "image_request_too_large" || got.Kind == "image_relay_full" || got.Kind == "image_unsupported") {
		t.Fatalf("an ordinary image request was refused: %+v", got)
	}
}
