package basispoints

import (
	"errors"
	"strings"
	"testing"
)

// h20: the upstream's "could not download your images in time" (a 400) is
// handed back as 413 for a request that carries more relayed images than the
// former ceilings allowed, so the caller in front of the gateway fails over as
// it did when such a request was refused here. Requests within the former
// ceilings keep the upstream's 400.

// The upstream's own words, as received on 2026-10-07.
const h20DownloadTimeoutBody = `{"error":{"code":"invalid_value","message":"Unable to download content from the provided URL before the timeout. Check that the URL is publicly accessible and responds promptly, or upload the file and provide a file_id instead.","param":"url","type":"invalid_request_error"}}`

const h20PublicBase = "https://relay.example.test/bp-img/"

func h20DownloadService() *Service {
	s := NewService()
	s.mu.Lock()
	s.cfg.PublicBaseURL = h20PublicBase
	s.mu.Unlock()
	return s
}

// h20Stash puts an image of the given size into the relay and returns the URL
// the plugin would have written into the outbound body for it.
func h20Stash(t *testing.T, seed, size int) string {
	t.Helper()
	raw := make([]byte, size)
	raw[0], raw[1], raw[2] = byte(seed), byte(seed>>8), byte(seed>>16)
	token, outcome := stashImage(raw, "image/png")
	if outcome != stashStored {
		t.Fatalf("could not stash a %d-byte test image: outcome %v", size, outcome)
	}
	return relayImageURL(h20PublicBase, token)
}

func h20OutboundBody(imageURLs ...string) map[string]any {
	parts := []any{map[string]any{"type": "input_text", "text": "look"}}
	for _, u := range imageURLs {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": u})
	}
	return map[string]any{
		"model": "gpt-6-astra",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": parts}},
	}
}

func h20Is413(err error) bool {
	coded, ok := err.(*APIError)
	return ok && coded.Status == 413 && coded.Kind == "image_download_timeout"
}

func TestH20DownloadTimeoutAboveFormerCeilingsBecomes413(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()
	s := h20DownloadService()
	upstream := statusError(400, []byte(h20DownloadTimeoutBody))

	// Bytes: five 11 MiB images, 55 MiB, more than the former 48 MiB.
	var big []string
	for i := 0; i < 5; i++ {
		big = append(big, h20Stash(t, 100+i, 11<<20))
	}
	if !strings.HasPrefix(big[0], "https://relay.example.test/bp-img/image?t=") {
		t.Fatalf("relay URL has an unexpected shape: %.70s", big[0])
	}
	got := s.imageDownloadRefusal(upstream, h20OutboundBody(big...))
	if !h20Is413(got) {
		t.Fatalf("55 MiB of relayed images, download timeout: got %v, want 413 image_download_timeout", got)
	}
	// The caller in front of this gateway moves a 413 elsewhere unless the
	// message reads like a context-window error.
	lower := strings.ToLower(got.(*APIError).Message)
	for _, marker := range []string{"context window", "context length", "context_length", "context_too_large", "token limit", "maximum context", "max context"} {
		if strings.Contains(lower, marker) {
			t.Fatalf("refusal %q contains %q", got.(*APIError).Message, marker)
		}
	}

	// Count: 129 references to one thumbnail, one more than the former 128.
	thumb := h20Stash(t, 200, 4096)
	many := make([]string, imageRelayFormerMaxPerRequest+1)
	for i := range many {
		many[i] = thumb
	}
	if got := s.imageDownloadRefusal(upstream, h20OutboundBody(many...)); !h20Is413(got) {
		t.Fatalf("129 relayed images, download timeout: got %v, want 413", got)
	}

	// Relay URLs are found wherever they sit in the outbound body.
	nested := map[string]any{"input": []any{map[string]any{
		"type": "function_call_output", "output": []any{
			map[string]any{"type": "input_image", "image_url": map[string]any{"url": big[0]}},
			map[string]any{"type": "input_image", "image_url": big[1]},
			map[string]any{"type": "input_image", "image_url": big[2]},
			map[string]any{"type": "input_image", "image_url": big[3]},
			map[string]any{"type": "input_image", "image_url": big[4]},
		},
	}}}
	if got := s.imageDownloadRefusal(upstream, nested); !h20Is413(got) {
		t.Fatalf("relay URLs inside a tool result were not counted: %v", got)
	}
	if count, size := relayedImageLoad(nested, h20PublicBase); count != 5 || size != 5*(11<<20) {
		t.Fatalf("relayedImageLoad = %d references, %d bytes; want 5 and %d", count, size, 5*(11<<20))
	}
}

func TestH20DownloadTimeoutWithinFormerCeilingsIsLeftAlone(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()
	s := h20DownloadService()
	timeout := statusError(400, []byte(h20DownloadTimeoutBody))

	// An ordinary request: three images, 3 MiB. This answer is seen on such
	// requests in production and has always gone back to the client as it is.
	var small []string
	for i := 0; i < 3; i++ {
		small = append(small, h20Stash(t, 300+i, 1<<20))
	}
	if got := s.imageDownloadRefusal(timeout, h20OutboundBody(small...)); got != timeout {
		t.Fatalf("a download timeout within the former ceilings was rewritten: %v", got)
	}
	// Exactly at the former ceilings: 128 references, 48 MiB.
	four := []string{h20Stash(t, 400, 12<<20), h20Stash(t, 401, 12<<20), h20Stash(t, 402, 12<<20), h20Stash(t, 403, 12<<20)}
	if got := s.imageDownloadRefusal(timeout, h20OutboundBody(four...)); got != timeout {
		t.Fatalf("exactly 48 MiB of relayed images was rewritten: %v", got)
	}
	thumb := h20Stash(t, 500, 4096)
	exact := make([]string, imageRelayFormerMaxPerRequest)
	for i := range exact {
		exact[i] = thumb
	}
	if got := s.imageDownloadRefusal(timeout, h20OutboundBody(exact...)); got != timeout {
		t.Fatalf("exactly 128 relayed images was rewritten: %v", got)
	}

	// The client's own URLs, however many: its failure to see.
	own := make([]string, 300)
	for i := range own {
		own[i] = "https://pictures.example.test/a.png"
	}
	if got := s.imageDownloadRefusal(timeout, h20OutboundBody(own...)); got != timeout {
		t.Fatalf("a download timeout on the client's own URLs was rewritten: %v", got)
	}
	// A URL that merely shares the relay's host is not a relay URL.
	lookalike := make([]string, 300)
	for i := range lookalike {
		lookalike[i] = "https://relay.example.test/bp-img/other?t=1"
	}
	if got := s.imageDownloadRefusal(timeout, h20OutboundBody(lookalike...)); got != timeout {
		t.Fatalf("non-relay URLs on the relay's host were counted as relayed images: %v", got)
	}
}

func TestH20OtherUpstreamAnswersAreLeftAlone(t *testing.T) {
	resetImageRelay()
	defer resetImageRelay()
	s := h20DownloadService()
	var big []string
	for i := 0; i < 5; i++ {
		big = append(big, h20Stash(t, 600+i, 11<<20))
	}
	body := h20OutboundBody(big...)
	for _, other := range []error{
		statusError(400, []byte(`{"error":{"message":"The image data you provided does not represent a valid image.","type":"invalid_request_error"}}`)),
		statusError(500, []byte(h20DownloadTimeoutBody)),
		statusError(429, []byte(`{"error":{"message":"rate limited","type":"rate_limit_exceeded"}}`)),
		errors.New("Unable to download content from the provided URL"),
	} {
		if got := s.imageDownloadRefusal(other, body); got != other {
			t.Fatalf("an unrelated upstream error was rewritten: %v -> %v", other, got)
		}
	}
	// Without a public base there is no relay, so nothing can be a relay URL.
	bare := NewService()
	timeout := statusError(400, []byte(h20DownloadTimeoutBody))
	if got := bare.imageDownloadRefusal(timeout, body); got != timeout {
		t.Fatalf("with no public base configured the error was rewritten: %v", got)
	}
	if count, _ := relayedImageLoad(body, "  "); count != 0 {
		t.Fatalf("an empty public base matched %d relay URLs", count)
	}
	// References to entries the relay no longer holds still count, with no size.
	resetImageRelay()
	if count, size := relayedImageLoad(body, h20PublicBase); count != 5 || size != 0 {
		t.Fatalf("after the entries were dropped: %d references, %d bytes; want 5 and 0", count, size)
	}
}
