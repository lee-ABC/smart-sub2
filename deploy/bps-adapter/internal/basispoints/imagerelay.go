package basispoints

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Image relay.
//
// Basis Points accepts images, but only as a URL it fetches itself: a data URL
// is rejected with 422 (verified against a live account, down to a 1x1 PNG,
// while a 600 KB text-only request succeeds). Clients such as Codex send
// images inline as base64, so the two cannot meet without somewhere to put the
// bytes.
//
// Inline images are therefore stashed in memory and the request is rewritten to
// point at this plugin's own resource route. Basis Points downloads from there
// and the entry is dropped.
//
// That route is unauthenticated by CPA's design, and the upstream fetches it
// from the public internet, so the bytes are briefly reachable by anyone
// holding the URL. The exposure is bounded deliberately:
//
//   - the path token is 32 random bytes, so URLs cannot be guessed or walked,
//   - entries expire after imageRelayTTL regardless of use,
//   - each entry serves at most imageRelayMaxFetches times (a couple, not one,
//     so an upstream retry or redirect does not break the request),
//   - only image media types are stored and served,
//   - per-image and total memory are capped.
const (
	imageRelayTTL = 5 * time.Minute
	// Fetches are capped to bound how long a URL stays live, but the cap has to
	// clear a whole conversation turn, not a single image. Entries are shared
	// by content now, and Codex resends a thread's screenshots together, so one
	// request can reference the same bytes many times over and the upstream
	// fetches each reference. At the old value of 4 the entry retired partway
	// through such a request and the remaining references 404'd. The time to
	// live is the real bound; this is only a stop against runaway fetching.
	// It was 64 while a request could carry 128 images, which already left a
	// request with 65 or more copies of one screenshot short; it now follows
	// the count ceiling, twice over so that one retry by the upstream fits.
	imageRelayMaxFetches = 2 * imageRelayMaxPerRequest
	imageRelayMaxBytes   = 12 << 20 // per image

	// Per-request ceilings, checked from the declared base64 length before
	// anything is decoded.
	//
	// Decoding happens in prepareRequest, which runs before the concurrency
	// gate, so nothing capped how many copies of a body could be inflated at
	// once: a 336 MB request measured 2.8 GB of allocation, and a handful in
	// parallel would exhaust the container. An OOM kill is not a failed
	// request -- it takes the process and every credential with it. Rejecting
	// on the declared length costs nothing and happens before the allocation.
	// Sized from live traffic, not from a guess: the busiest requests observed
	// carry 40 inline images totalling about 16 MB, and 25-37 images is
	// ordinary for this workload. An earlier value of 24 rejected real
	// requests with 413. The count is deliberately far above what anything
	// legitimate sends -- the byte ceiling is what actually bounds memory, and
	// the count only stops a body made of thousands of tiny images.
	//
	// Raised 2026-10-06 (h20) from 128 images / 48 MiB. The byte ceiling was
	// where ordinary long Codex Desktop threads land: bodies of 69-86 MB
	// carrying about twenty full-size PNG screenshots were refused eleven
	// times that day, and every refusal moved the conversation to another
	// upstream for good, because the caller fails over on 413 and its
	// session affinity then keeps it there. The count ceiling was hit too,
	// by a 5.5 MB body made of 160 thumbnails. What a request at these
	// ceilings costs the process, and what keeps several of them from being
	// inflated at once, is in largecall.go.
	imageRelayMaxPerRequest   = 500
	imageRelayMaxRequestBytes = 192 << 20
	imageRelayPath       = "image"
	imageRelayTokenParam = "t"
)

// imageRelayMaxTotal caps the bytes held across all live entries.
//
// It was 64 MB while the gateway ran under a 2 GiB memory limit, and even
// with deduplication it saturated under real Codex traffic: dozens of
// concurrent threads each carrying their own screenshots, rejected with
// image_relay_full. The container now has 16 GiB, and a quarter of a
// gigabyte of images in flight is comfortably inside that.
//
// A variable rather than a constant so tests can exercise a full relay
// without allocating the whole production budget.
var imageRelayMaxTotal = 2 << 30 // 2 GiB: raised 2026-09-29 after relay-full 413s at peak (was 256 MiB)

type relayedImage struct {
	Data      []byte
	MediaType string
	ExpiresAt time.Time
	Fetches   int
	Hash      string
}

// stashOutcome distinguishes an image this relay will not carry from one it
// simply has no room for, because the two need different answers: the first is
// the caller's to fix, the second is this gateway's own capacity.
type stashOutcome int

const (
	stashStored stashOutcome = iota
	stashUnusable
	stashFull
)

var (
	imageRelayMu     sync.Mutex
	imageRelay       = map[string]*relayedImage{}
	imageRelayByHash = map[string]string{}
	imageRelayBytes  int
)

// stashImage stores image bytes and returns the token addressing them.
//
// Identical bytes already held are reused rather than stored again. Codex
// resends the whole conversation on every turn, screenshots included, so the
// same images arrive over and over; storing a fresh copy each time is what
// exhausted the relay in production, where a handful of threads carrying
// twenty-odd screenshots filled the then 64 MB budget within one TTL window and
// made perfectly ordinary PNGs fail to relay. Reusing the live entry keeps
// memory proportional to the distinct images in flight instead of to images
// times turns.
func stashImage(data []byte, mediaType string) (string, stashOutcome) {
	if len(data) == 0 || len(data) > imageRelayMaxBytes || !isImageMediaType(mediaType) {
		return "", stashUnusable
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])

	imageRelayMu.Lock()
	defer imageRelayMu.Unlock()
	now := time.Now()
	pruneImagesLocked(now)

	if token, known := imageRelayByHash[hash]; known {
		if entry, live := imageRelay[token]; live && now.Before(entry.ExpiresAt) {
			// Push the expiry out and hand back the fetch budget: this image is
			// part of a conversation that is still going.
			entry.ExpiresAt = now.Add(imageRelayTTL)
			entry.Fetches = 0
			return token, stashStored
		}
	}

	if imageRelayBytes+len(data) > imageRelayMaxTotal {
		return "", stashFull
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", stashUnusable
	}
	token := hex.EncodeToString(raw)
	imageRelay[token] = &relayedImage{
		Data:      data,
		MediaType: mediaType,
		ExpiresAt: now.Add(imageRelayTTL),
		Hash:      hash,
	}
	imageRelayByHash[hash] = token
	imageRelayBytes += len(data)
	return token, stashStored
}

// takeImage returns the bytes for a token and retires the entry once it has
// been served often enough.
func takeImage(token string) ([]byte, string, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, "", false
	}
	imageRelayMu.Lock()
	defer imageRelayMu.Unlock()
	now := time.Now()
	pruneImagesLocked(now)
	entry, ok := imageRelay[token]
	if !ok || now.After(entry.ExpiresAt) {
		return nil, "", false
	}
	entry.Fetches++
	data, mediaType := entry.Data, entry.MediaType
	if entry.Fetches >= imageRelayMaxFetches {
		dropImageLocked(token)
	}
	return data, mediaType, true
}

func pruneImagesLocked(now time.Time) {
	for token, entry := range imageRelay {
		if now.After(entry.ExpiresAt) {
			dropImageLocked(token)
		}
	}
}

func dropImageLocked(token string) {
	if entry, ok := imageRelay[token]; ok {
		imageRelayBytes -= len(entry.Data)
		if imageRelayByHash[entry.Hash] == token {
			delete(imageRelayByHash, entry.Hash)
		}
		delete(imageRelay, token)
	}
}

func isImageMediaType(mediaType string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch mediaType {
	case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/gif", "image/bmp", "image/tiff":
		return true
	}
	return false
}

// declaredPayloadBytes reports how many bytes a data: URL will decode to,
// without decoding it. Base64 carries three bytes in every four characters.
// hasDataURLPrefix tests the scheme without lowercasing the whole value.
// strings.ToLower on a data: URL copies the entire payload -- tens of
// megabytes -- to look at five characters.
func hasDataURLPrefix(value string) bool {
	return len(value) >= 5 && strings.EqualFold(value[:5], "data:")
}

func declaredPayloadBytes(value string) int {
	comma := strings.Index(value, ",")
	if comma < 0 {
		return 0
	}
	encoded := len(value) - comma - 1
	if encoded <= 0 {
		return 0
	}
	return encoded / 4 * 3
}

// decodeDataURL splits a data: URL into its media type and bytes.
// Returns ok=false for anything that is not an inline base64 payload.
func decodeDataURL(value string) ([]byte, string, bool) {
	value = strings.TrimSpace(value)
	if !hasDataURLPrefix(value) {
		return nil, "", false
	}
	comma := strings.Index(value, ",")
	if comma < 0 {
		return nil, "", false
	}
	meta := value[len("data:"):comma]
	payload := value[comma+1:]
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return nil, "", false
	}
	mediaType := strings.TrimSpace(strings.SplitN(meta, ";", 2)[0])
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
	if err != nil {
		// Some clients emit unpadded or URL-safe base64.
		data, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(payload))
		if err != nil {
			return nil, "", false
		}
	}
	return data, mediaType, true
}

// relayImageURL builds the absolute URL Basis Points will fetch.
func relayImageURL(base, token string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || token == "" {
		return ""
	}
	return base + "/" + imageRelayPath + "?" + imageRelayTokenParam + "=" + token
}

// rewriteInlineImages replaces inline image payloads in a Responses input tree
// with relay URLs. It reports how many parts were rewritten and how many had to
// be left alone, so the caller can fail loudly instead of quietly dropping an
// image the user expected the model to see.
// The tree is walked in full rather than only through item.content, because
// images do not live in one predictable place: Codex returns screenshots inside
// a tool result's `output` array (input[i].output[j]), and an earlier version
// that only inspected `content` silently passed those straight through to an
// upstream that rejects them.
// imageRelayResult reports what became of a request's inline images.
type imageRelayResult struct {
	Rewritten int
	// Unusable counts images this relay will not carry: no URL, a payload that
	// will not decode, an unsupported media type, or one over the per-image cap.
	Unusable int
	// Overflowed counts images that were perfectly fine but arrived when the
	// relay had no room left. Keeping this apart from Unusable matters: it is
	// the difference between "fix your image" and "this gateway is saturated",
	// and reporting the former for the latter sent an earlier investigation
	// looking at a 116 KB PNG that was never the problem.
	Overflowed int
	// OverBudget counts images refused because this one request already
	// carries more inline image data, in bytes or in count, than a single
	// request may. It is kept apart from Overflowed for the reason Overflowed
	// is kept apart from Unusable: a full relay clears by itself within
	// minutes, an oversized request never will, and telling its sender to
	// "retry in a few minutes" starts a retry loop that cannot succeed.
	OverBudget int
}

func (r imageRelayResult) failed() bool {
	return r.Unusable > 0 || r.Overflowed > 0 || r.OverBudget > 0
}

func rewriteInlineImages(input any, publicBase string) imageRelayResult {
	var result imageRelayResult
	// Budget first, for the whole request and on declared lengths alone. A
	// request over a ceiling is refused whole, so it is decided before the
	// first image is decoded. Deciding it image by image, as this used to,
	// decoded, hashed and stashed everything that still fitted: up to a full
	// ceiling of bytes no upstream would ever fetch, parked in the shared
	// relay for five minutes and put back on every retry of the same
	// conversation. The body has been parsed by the time this runs, so this
	// spares the image work, not the parse.
	if over := inlineImagesOverBudget(input); over > 0 {
		result.OverBudget = over
		return result
	}
	walkImageParts(input, func(part map[string]any) {
		url := imagePartURL(part)
		if url == "" {
			result.Unusable++
			return
		}
		if !hasDataURLPrefix(url) {
			// Already a URL the upstream can fetch.
			part["image_url"] = url
			return
		}
		// Nothing is decoded when there is nowhere to relay it to, nor when
		// the declared length is clearly past the per-image limit: with the
		// request ceiling at 192 MiB a single oversized payload would
		// otherwise be inflated in full just to be turned away. The declared
		// length can run a little over the decoded one (padding, line
		// breaks), hence the margin; stashImage still applies the exact limit.
		if publicBase == "" || declaredPayloadBytes(url) > imageRelayMaxBytes+imageRelayMaxBytes/16 {
			result.Unusable++
			return
		}
		data, mediaType, ok := decodeDataURL(url)
		if !ok {
			result.Unusable++
			return
		}
		token, outcome := stashImage(data, mediaType)
		switch outcome {
		case stashFull:
			result.Overflowed++
			return
		case stashUnusable:
			result.Unusable++
			return
		}
		part["image_url"] = relayImageURL(publicBase, token)
		result.Rewritten++
	})
	return result
}

// upstreamImageDownloadTimeout is how the upstream words its refusal when it
// could not fetch a request's images within its own deadline.
const upstreamImageDownloadTimeout = "Unable to download content from the provided URL"

// The per-request ceilings before h20. A request within them was always sent
// to the upstream, whatever the upstream then made of it.
const (
	imageRelayFormerMaxPerRequest   = 128
	imageRelayFormerMaxRequestBytes = 48 << 20
)

// imageDownloadRefusal turns the upstream's "could not download your images in
// time" into a 413 for a request that only reaches the upstream because the
// ceilings were raised.
//
// The upstream fetches every image itself and gives up after a few seconds.
// Measured against it on 2026-10-07 through a single-credential instance:
// 190 MiB in one request was accepted as 95 images of 2 MiB, 47 of 4 MiB and
// 31 of 6 MiB, and so were 500 thumbnails; 23 images of 8 MiB and 12 or 16 of
// 11.8 MiB were refused within six seconds with this 400, while 8 of 11.8 MiB
// went through. So what it cannot take is many very large files at once, not
// a total, and no ceiling on this side expresses that exactly.
//
// A 400 is handed straight back to the client by the caller in front of this
// gateway, and the client's next attempt carries the same images and fails
// the same way. While the ceilings were 128 images and 48 MiB such a request
// never reached the upstream: it was refused here with 413 and the caller took
// it to another upstream that accepts inline images. Answering 413 keeps that
// behaviour for those requests and only for them. A request within the former
// ceilings gets the upstream's 400 unchanged, as it always did: the same
// answer turned up about fifteen times on 2026-10-06 on ordinary requests,
// down to a single image, and there it is the upstream's fetch failing, which
// a retry usually clears. Images the client supplied as its own URLs are not
// counted at all.
func (s *Service) imageDownloadRefusal(err error, body map[string]any) error {
	coded, ok := err.(*APIError)
	if !ok || coded.Status != 400 || !strings.Contains(coded.Message, upstreamImageDownloadTimeout) {
		return err
	}
	count, size := relayedImageLoad(body, s.config().PublicBaseURL)
	if count <= imageRelayFormerMaxPerRequest && size <= imageRelayFormerMaxRequestBytes {
		return err
	}
	return fail(413, "image_download_timeout",
		"the upstream could not download this request's images in time (too many large images in one request); use fewer or smaller images")
}

// relayedImageLoad reports how many references to this relay's image route an
// outbound body carries and how many bytes they stand for. Every reference
// counts, as every inline image did against the per-request ceilings; a
// reference whose entry is no longer held counts with a size of zero.
func relayedImageLoad(node any, publicBase string) (count, size int) {
	base := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	if base == "" {
		return 0, 0
	}
	prefix := base + "/" + imageRelayPath + "?" + imageRelayTokenParam + "="
	var tokens []string
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case string:
			if strings.HasPrefix(typed, prefix) {
				tokens = append(tokens, typed[len(prefix):])
			}
		case map[string]any:
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(node)
	imageRelayMu.Lock()
	defer imageRelayMu.Unlock()
	for _, token := range tokens {
		count++
		if entry, held := imageRelay[token]; held {
			size += len(entry.Data)
		}
	}
	return count, size
}

// imagePartURL returns an image part's URL. An object-shaped image_url is
// rejected upstream ("expected an image URL"), so the common {url: ...}
// spelling is accepted here as well.
func imagePartURL(part map[string]any) string {
	url := stringValue(part["image_url"])
	if url == "" {
		if nested := objectValue(part["image_url"]); nested != nil {
			url = stringValue(nested["url"])
		}
	}
	return url
}

// inlineImagesOverBudget reports whether a request's inline images exceed the
// per-request ceilings, judged on declared length and without decoding any of
// them: the result is zero exactly when they fit, and otherwise the number
// that did not, counted in walk order. Images that are already URLs cost this
// relay nothing and are not counted.
func inlineImagesOverBudget(input any) int {
	count, total, over := 0, 0, 0
	walkImageParts(input, func(part map[string]any) {
		url := imagePartURL(part)
		if !hasDataURLPrefix(url) {
			return
		}
		declared := declaredPayloadBytes(url)
		if count >= imageRelayMaxPerRequest || total+declared > imageRelayMaxRequestBytes {
			over++
			return
		}
		count++
		total += declared
	})
	return over
}

// walkImageParts visits every object in the tree that describes an image part.
func walkImageParts(node any, visit func(map[string]any)) {
	switch typed := node.(type) {
	case map[string]any:
		switch strings.ToLower(strings.TrimSpace(stringValue(typed["type"]))) {
		case "input_image", "computer_screenshot":
			if _, hasURL := typed["image_url"]; hasURL {
				visit(typed)
				return
			}
		}
		for _, value := range typed {
			walkImageParts(value, visit)
		}
	case []any:
		for _, value := range typed {
			walkImageParts(value, visit)
		}
	}
}
