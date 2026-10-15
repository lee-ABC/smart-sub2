package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func (s *Service) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.clone()
}

// hasAnswerableInput reports whether `input` carries anything for the model to
// answer. A non-empty string is a prompt; a list needs at least one item. Every
// other shape (missing, null, a number) is nothing.
func hasAnswerableInput(rawInput any) bool {
	switch typed := rawInput.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	default:
		return false
	}
}

func (s *Service) prepareRequest(request ExecutorRequest) (map[string]any, credential, error) {
	body, c, _, err := s.prepareRequestWithRelayProtocol(request)
	return body, c, err
}

// Pin the relay protocol once, before image rewrites or upstream work. The
// response decoder uses this exact selection even if the replay cache changes.
func (s *Service) prepareRequestWithRelayProtocol(request ExecutorRequest) (map[string]any, credential, string, error) {
	return s.prepareRequestWithRelayContext(&request)
}

func (s *Service) prepareRequestWithRelayContext(request *ExecutorRequest) (map[string]any, credential, string, error) {
	c, err := credentialFromExecutor(*request)
	if err != nil {
		return nil, credential{}, "", err
	}
	if !c.ExpiresAt.IsZero() && !time.Now().Before(c.ExpiresAt) {
		return nil, credential{}, "", fail(401, "auth_expired", "ChatGPT OAuth access token has expired")
	}
	body := request.OriginalRequest
	if len(body) == 0 {
		body = request.Payload
	}
	source, err := rawObject(body)
	if err != nil {
		return nil, credential{}, "", err
	}
	// Read the scope before anything in this function rewrites source. Both
	// caches this scope names are filled from the response path, which parses
	// the untouched original bytes; reading it after the rewrites below made
	// the two disagree for any request that has no session key and opens with
	// an inline image, because the image relay writes a token that expires in
	// five minutes into the very item the fallback fingerprints.
	freezeReplayScopeFromSource(request, source)
	request.relayKind = relayRequestKind(source)
	scope := request.replayCacheScope
	source = withFrozenReplayScope(source, *request)
	cfg := s.config()
	// Experiment arm selection (bpsexp build only): pick this request's switch
	// combination from its arm field, strip the arm field so it never reaches
	// the upstream, and register the trial for capture. A no-op in the
	// production build, where cfg and source are left exactly as they were.
	expApplyArm(s, request, source, &cfg)
	// Every relay switch is frozen here, from the same snapshot that prepares
	// the upstream body; the response decoder reads this copy, never s.cfg.
	frozen := relayOptionsFromConfig(cfg)
	request.relayOptions = &frozen
	source = withRelayOptions(source, frozen)
	// History notes made while the history is translated are logged under
	// this request's callback id.
	s.attachRelayDecodeNotes(source, request.HostCallbackID)
	selected := selectRelayProtocol(source, cfg.RelayProtocol, scope)
	cfg.RelayProtocol = selected
	model := stringValue(source["model"])
	if model == "" {
		model = strings.TrimSpace(request.Model)
	}
	if model == "" {
		model = cfg.Models[0]
	}
	if !isResponseModel(model, cfg) && model != cfg.UpstreamModel {
		return nil, credential{}, "", fail(400, "unsupported_model", "model is not enabled in oai-basispoints: "+model)
	}
	// A request with nothing to answer is the caller's fault, so it is refused
	// here with a 4xx. The host reads 4xx as a fault of the request and 5xx as
	// a fault of the credential: letting an empty turn through would spend an
	// upstream round trip to be told the same thing, and any 5xx that came
	// back would cool this credential -- and then the next, for every
	// credential in the pool, because the same empty body fails on all of them.
	if !hasAnswerableInput(source["input"]) {
		return nil, credential{}, "", fail(400, "empty_input", "request carries no input to answer")
	}
	source["stream"] = request.Stream
	// Basis Points only accepts images it can fetch itself, so inline payloads
	// are relayed through this plugin's resource route first.
	if images := rewriteInlineImages(source["input"], cfg.PublicBaseURL); images.failed() {
		if cfg.PublicBaseURL == "" {
			return nil, credential{}, "", fail(400, "image_relay_disabled",
				"this request carries inline images, but public_base_url is not configured, so they cannot be relayed to Basis Points")
		}
		if images.OverBudget > 0 {
			// 413 for the reason given below, but said truthfully: the same
			// body can be retried forever and will be refused every time.
			return nil, credential{}, "", fail(413, "image_request_too_large",
				fmt.Sprintf("this request carries more inline image data than one request may (limit: %d images, %d MiB in total); shorten or compact the conversation",
					imageRelayMaxPerRequest, imageRelayMaxRequestBytes>>20))
		}
		if images.Overflowed > 0 {
			// 413 rather than 5xx on purpose: the host reads 4xx as a fault of
			// the request and 5xx as a fault of the credential, and a saturated
			// relay is neither the credential's doing nor a reason to pull it
			// out of service.
			return nil, credential{}, "", fail(413, "image_relay_full",
				"the image relay is holding as much as it is allowed to; retry in a few minutes")
		}
		return nil, credential{}, "", fail(400, "image_unsupported",
			"an image in this request could not be relayed to Basis Points (unreadable payload, unsupported media type, or over the size limit)")
	}
	who, mode := s.personaFor(*request, c)
	// Native promotion (a1_promotion.go) is decided here, from the same
	// frozen snapshot; off leaves plan nil and the body h25's.
	prepared, plan, err := prepareResponsesBodyPlanned(source, cfg, scope, who, mode)
	if err != nil {
		return nil, credential{}, "", err
	}
	request.a1 = plan
	if plan != nil && plan.applied {
		request.relayKind = relayKindPromoted
	}
	return prepared, c, selected, nil
}

// chromeUserAgent is what the rest of these headers already claim to be.
//
// Every other field here describes a browser: Stainless reports the JS SDK
// running as browser:chrome, with the OS and architecture it cannot see from
// there, and the Basis Points client fields describe the Excel add-in, which is
// a web add-in and therefore a browser too. The User-Agent alone disagreed --
// it read "oai-basispoints/0.1.2", naming this plugin and its version, a single
// string that identifies the relay with no correlation needed.
//
// The version is pinned to the one utls emulates, since CPA now performs this
// host's handshake with HelloChrome_Auto (= HelloChrome_133 in utls v1.8.2):
// claiming a Chrome the handshake does not match would only move the
// contradiction. Keep the two in step when either is upgraded.
//
// Still missing, and only fixable with a capture of the real add-in: a browser
// would also send sec-ch-ua, sec-fetch-* and Accept-Language on this request.
// chromeMajorVersion ties the User-Agent, the client hints and the handshake
// together: utls HelloChrome_Auto is HelloChrome_133 in v1.8.2. Bump all of
// them at once.
const chromeMajorVersion = "133"

const chromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" +
	chromeMajorVersion + ".0.0.0 Safari/537.36"

// chromeSecCHUA is the brand list Chrome sends alongside that User-Agent.
//
// The two real brands are derived from the version; the third is the GREASE
// entry Chrome varies to keep parsers honest. Its exact spelling is the one
// part of these headers that is not derivable, so it is a best guess -- but
// sending a well-formed list is still closer than sending none, which
// contradicts the User-Agent outright.
//
// The 2026-10-01 capture shows the real add-in is Edge WebView2 154, whose
// GREASE entry reads "Not A(Brand". That value is not copied here: the GREASE
// spelling rotates with the Chromium version, so pairing Edge 154's with a
// Chrome 133 User-Agent would replace one guess with a mismatch. It belongs to
// the edge154 profile below, which carries the capture's values together or
// not at all.
const chromeSecCHUA = `"Not(A:Brand";v="99", "Google Chrome";v="` + chromeMajorVersion +
	`", "Chromium";v="` + chromeMajorVersion + `"`

// browserProfile keeps the User-Agent, the client hints and the add-in's own
// browser-* headers in step.
//
// The real add-in derives its x-openai-internal-basispoints-browser-* values
// from the same client hints the browser sends, so they cannot be set
// independently: a request that announces Chrome in sec-ch-ua and Edge in
// browser-name is a client that does not exist. Every field a profile carries
// moves together.
type browserProfile struct {
	userAgent      string
	secCHUA        string
	brands         string
	name           string
	platform       string
	runtimeVersion string
}

// chromeProfile is what this relay has been sending, and stays the default: it
// is the one that matches the uTLS handshake CPA performs (HelloChrome_Auto is
// HelloChrome_133 in utls v1.8.2, which has no Edge 154 preset at all).
var chromeProfile = browserProfile{
	userAgent:      chromeUserAgent,
	secCHUA:        chromeSecCHUA,
	brands:         "Not(A:Brand,Google Chrome,Chromium",
	name:           "chrome",
	platform:       "Windows",
	runtimeVersion: chromeMajorVersion + ".0.0",
}

// edgeProfile is the capture, verbatim. Selecting it trades a wrong set of
// browser headers for a wrong TLS fingerprint, so it is off by default and
// should not be turned on before the handshake can follow it.
var edgeProfile = browserProfile{
	userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/154.0.0.0 Safari/537.36 Edg/154.0.0.0",
	secCHUA: `"Microsoft Edge";v="154", "Not A(Brand";v="99", ` +
		`"Microsoft Edge WebView2";v="154", "Chromium";v="154"`,
	brands:         "Microsoft Edge,Not A(Brand,Microsoft Edge WebView2,Chromium",
	name:           "edge",
	platform:       "Windows",
	runtimeVersion: "154.0.0",
}

func profileFor(cfg Config) browserProfile {
	if strings.EqualFold(strings.TrimSpace(cfg.ClientIdentity.UAProfile), "edge154") {
		return edgeProfile
	}
	return chromeProfile
}

// authHeaders is the header set with no persona at all. Keeping it is what
// makes "mode=off changes nothing" a property of the code rather than a claim:
// the identity-free path is a separate, shorter function, and every existing
// test still calls it.
func authHeaders(c credential, stream bool) http.Header {
	return authHeadersFor(c, stream, persona{}, identityOff, Config{}, "")
}

func authHeadersFor(c credential, stream bool, who persona, mode identityMode, cfg Config, cookie string) http.Header {
	profile := profileFor(cfg)
	// Accept is pinned rather than following the stream flag. Basis Points
	// answers with an event stream either way -- parseFinalStreamResponse
	// exists because it does -- and the capture shows the real client asking
	// for text/event-stream on every call, so the application/json branch only
	// ever described a request this upstream does not serve.
	accept := "text/event-stream"
	if mode == identityOff && !stream {
		accept = "application/json"
	}
	// These headers match the Excel/Basis Points client profile. The access
	// token itself is never logged by this plugin.
	headers := http.Header{
		"Authorization":       []string{"Bearer " + c.AccessToken},
		"ChatGPT-Account-ID":  []string{c.AccountID},
		"X-OpenAI-Account-ID": []string{c.AccountID},
		// X-Basispoints-Auth-Mode was removed from here on the reasoning that
		// it is not part of the X-OpenAI-Internal-Basispoints-* family the real
		// client sends. The capture settled that: the real client sends it on
		// every call to every one of these paths. It is restored under hygiene
		// rather than unconditionally only so that mode=off stays byte-identical
		// to what is deployed today.
		"Content-Type": []string{"application/json"},
		"Accept":       []string{accept},
		// No Accept-Encoding on purpose. Setting one turns off Go's transparent
		// decompression, which is why this used to say "identity" and read the
		// SSE stream in the clear -- but no browser asks for an uncompressed
		// response. Leaving it out lets the transport advertise gzip and unwrap
		// the body itself, so the relay still sees plain text.
		"Origin": []string{"https://bps.openai.com"},
		// Chrome sends these on every secure request, and their absence
		// contradicts a Chrome User-Agent. The fetch is same-origin because the
		// Origin above is the host being called: the add-in's page is served
		// from it, as web add-ins are.
		"Sec-Ch-Ua":          []string{profile.secCHUA},
		"Sec-Ch-Ua-Mobile":   []string{"?0"},
		"Sec-Ch-Ua-Platform": []string{`"` + profile.platform + `"`},
		"Sec-Fetch-Dest":     []string{"empty"},
		"Sec-Fetch-Mode":     []string{"cors"},
		"Sec-Fetch-Site":     []string{"same-origin"},
		"X-OpenAI-Internal-Basispoints-Client-Agent-Profile":  []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Editor":         []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Host":           []string{"office"},
		"X-OpenAI-Internal-Basispoints-Client-Platform":       []string{"excel"},
		"X-OpenAI-Internal-Basispoints-Client-Platform-Class": []string{"PC"},
		"X-OpenAI-Internal-Basispoints-Client-Product":        []string{"basispoints-excel-plugin"},
		"X-OpenAI-Internal-Basispoints-Client-Runtime":        []string{"desktop"},
		"X-OpenAI-Internal-Basispoints-Office-Host":           []string{"Excel"},
		"X-OpenAI-Internal-Basispoints-Office-Platform":       []string{"PC"},
		"X-Stainless-Arch":            []string{"unknown"},
		"X-Stainless-Lang":            []string{"js"},
		"X-Stainless-OS":              []string{"Unknown"},
		"X-Stainless-Package-Version": []string{"6.31.0"},
		"X-Stainless-Retry-Count":     []string{"0"},
		// browser:chrome even on the Edge profile. The real client runs in Edge
		// WebView2 and still reports chrome here, because Stainless classifies
		// any Chromium as chrome -- "correcting" this to browser:edge would
		// turn a match into an invention.
		"X-Stainless-Runtime": []string{"browser:chrome"},
		"User-Agent":          []string{profile.userAgent},
	}
	if mode == identityOff {
		return headers
	}
	// Everything below is capture-backed: these are headers the real add-in
	// sends on every call and this relay sent none of.
	headers.Set("X-Basispoints-Auth-Mode", c.AuthMode)
	headers.Set("X-Stainless-Runtime-Version", profile.runtimeVersion)
	headers.Set("Priority", "u=1, i")
	if who.accountUserID != "" {
		headers.Set("X-OpenAI-Account-User-Id", who.accountUserID)
	}
	if language := strings.TrimSpace(cfg.ClientIdentity.AcceptLanguage); language != "" {
		headers.Set("Accept-Language", language)
	}
	if cfg.ClientIdentity.SendBrowserHeaders {
		headers.Set("X-OpenAI-Internal-Basispoints-Browser-Name", profile.name)
		headers.Set("X-OpenAI-Internal-Basispoints-Browser-Ua-Brands", profile.brands)
		// Unquoted, and spelled out rather than "?0": these are the add-in's
		// own fields, not client hints, and the capture shows it writing the
		// values plainly even where the hint beside it is encoded.
		headers.Set("X-OpenAI-Internal-Basispoints-Browser-Ua-Mobile", "false")
		headers.Set("X-OpenAI-Internal-Basispoints-Browser-Ua-Platform", profile.platform)
	}
	if cookie != "" {
		headers.Set("Cookie", cookie)
	}
	if cfg.ClientIdentity.SendSentryTrace {
		trace, baggage := sentryTrace(who)
		headers.Set("Sentry-Trace", trace)
		headers.Set("Baggage", baggage)
	}
	return headers
}

// orgRateLimitBackoff returns how long to wait before retrying a 429, and
// whether retrying is worth it at all.
//
// Basis Points bills every tenant of its service organisation against one
// tokens-per-minute pool. We measured our own share at about an eighth of it,
// and the minutes we were refused looked no busier on our side than the ones
// we were not, so this is somebody else's traffic filling the bucket and no
// amount of rationing here prevents it. The refusal carries its own wait,
// usually well under a second, and CPA's own failover already rescued most of
// these by re-sending on another credential -- which spends a whole extra
// round trip to make the same call. Waiting the advertised moment in place is
// cheaper and keeps the session on the credential that owns its prompt cache.
//
// Only a rate limit qualifies. A usage limit means this credential is out of
// quota and waiting will not change that.
func orgRateLimitBackoff(err error) (time.Duration, bool) {
	var coded interface {
		StatusCode() int
		Error() string
	}
	if !errors.As(err, &coded) || coded.StatusCode() != 429 {
		return 0, false
	}
	message := strings.ToLower(coded.Error())
	if !strings.Contains(message, "tokens per min") && !strings.Contains(message, "requests per min") {
		return 0, false
	}
	if strings.Contains(message, "usage_limit") || strings.Contains(message, "usage limit") {
		return 0, false
	}
	wait := 250 * time.Millisecond
	if match := retryAfterPattern.FindStringSubmatch(message); match != nil {
		if value, convErr := strconv.ParseFloat(match[1], 64); convErr == nil {
			unit := time.Millisecond
			if strings.HasPrefix(match[2], "s") {
				unit = time.Second
			}
			if scaled := time.Duration(value * float64(unit)); scaled > 0 {
				wait = scaled
			}
		}
	}
	// A pool that is full right now clears in well under a second; anything
	// longer is not worth holding a slot for.
	if wait > 2*time.Second {
		return 0, false
	}
	return wait + 50*time.Millisecond, true
}

// "Please try again in 86ms" / "in 1.5s"
var retryAfterPattern = regexp.MustCompile(`try again in ([0-9.]+)\s*(ms|s)`)

// withOrgRateLimitRetry re-runs attempt while the shared organisation pool is
// momentarily full. Safe to retry: nothing has been written to the client yet
// on either path, because both are called before any output is relayed.
func withOrgRateLimitRetry[T any](attempt func() (T, error)) (T, error) {
	return withUpstreamRetry("", attempt)
}

// withUpstreamRetry re-runs attempt on the same credential while the failure
// is one that credential is expected to get past: the organisation pool being
// momentarily full, or Basis Points re-routing model to a variant it cannot
// serve (see modelreroute.go). A re-route that is still failing when the
// attempts run out is marked so CPA moves on without suspending the model.
func withUpstreamRetry[T any](model string, attempt func() (T, error)) (T, error) {
	const maxAttempts = 3
	var result T
	var err error
	for try := 0; try < maxAttempts; try++ {
		result, err = attempt()
		if err == nil {
			return result, nil
		}
		wait, retryable := orgRateLimitBackoff(err)
		if !retryable {
			if _, rerouted := reroutedModel(err, model); rerouted {
				wait, retryable = modelRerouteBackoff, true
			}
		}
		if !retryable || try == maxAttempts-1 {
			break
		}
		time.Sleep(wait)
	}
	return result, markModelReroute(err, model)
}

func (s *Service) upstreamRequest(request ExecutorRequest, body map[string]any, c credential, stream bool) (upstreamResponse, error) {
	model := stringValue(body["model"])
	// Experiment capture of the plugin's own retries and model re-routes
	// (bpsexp build only); a zero-cost no-op in the production build.
	tap := expBeginRetry(request, model)
	result, err := withUpstreamRetry(model, func() (upstreamResponse, error) {
		response, attemptErr := s.upstreamRequestOnce(request, body, c, stream)
		tap.attempt(attemptErr)
		return response, attemptErr
	})
	tap.done(err)
	return result, err
}

func (s *Service) upstreamRequestOnce(request ExecutorRequest, body map[string]any, c credential, stream bool) (upstreamResponse, error) {
	cfg := s.config()
	if cfg.ResponsesURL == "" {
		return upstreamResponse{}, fail(500, "invalid_config", "responses_url is empty")
	}
	who, mode := s.personaFor(request, c)
	// Experiment multi-agent plaintext switch (bpsexp build only): rewrites
	// the body before it is serialised. A no-op in the production build.
	expRewriteOutboundBody(request, body)
	headers := authHeadersFor(c, stream, who, mode, cfg, s.cookieHeader(who, cfg, mode))
	outbound := outboundBody(body, mode)
	// Experiment capture of the exact bytes and headers sent upstream
	// (account identifiers hashed, auth dropped); a no-op in production.
	expRecordRequestBody(request, outbound, headers, stream)
	payload := map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost,
		"url":              cfg.ResponsesURL,
		"headers":          headers,
		"body":             outbound,
	}
	var response upstreamResponse
	if err := s.call("host.http.do", payload, &response); err != nil {
		return upstreamResponse{}, fail(502, "upstream_transport", "Basis Points transport failed: "+safeError(err))
	}
	s.rememberCookies(who, cfg, mode, response.Headers)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, statusError(response.StatusCode, response.Body)
	}
	return response, nil
}

func (s *Service) upstreamStream(request ExecutorRequest, body map[string]any, c credential) (upstreamStream, error) {
	model := stringValue(body["model"])
	// Experiment capture of the plugin's own retries and model re-routes
	// (bpsexp build only); a zero-cost no-op in the production build.
	tap := expBeginRetry(request, model)
	result, err := withUpstreamRetry(model, func() (upstreamStream, error) {
		stream, attemptErr := s.upstreamStreamOnce(request, body, c)
		tap.attempt(attemptErr)
		return stream, attemptErr
	})
	tap.done(err)
	return result, err
}

func (s *Service) upstreamStreamOnce(request ExecutorRequest, body map[string]any, c credential) (upstreamStream, error) {
	cfg := s.config()
	who, mode := s.personaFor(request, c)
	// Experiment multi-agent plaintext switch (bpsexp build only): rewrites
	// the body before it is serialised. A no-op in the production build.
	expRewriteOutboundBody(request, body)
	headers := authHeadersFor(c, true, who, mode, cfg, s.cookieHeader(who, cfg, mode))
	outbound := outboundBody(body, mode)
	// Experiment capture of the exact bytes and headers sent upstream
	// (account identifiers hashed, auth dropped); a no-op in production.
	expRecordRequestBody(request, outbound, headers, true)
	payload := map[string]any{
		"host_callback_id": request.HostCallbackID,
		"method":           http.MethodPost,
		"url":              cfg.ResponsesURL,
		"headers":          headers,
		"body":             outbound,
	}
	var stream upstreamStream
	if err := s.call("host.http.do_stream", payload, &stream); err != nil {
		return stream, fail(502, "upstream_transport", "Basis Points stream transport failed: "+safeError(err))
	}
	s.rememberCookies(who, cfg, mode, stream.Headers)
	if stream.StreamID == "" {
		return stream, fail(502, "upstream_transport", "host returned no Basis Points stream ID")
	}
	if stream.StatusCode > 0 && stream.StatusCode < 400 && (stream.StatusCode < 200 || stream.StatusCode >= 300) {
		// A 1xx or 3xx is something on the path -- a proxy, a CDN edge, a
		// captive portal, an auth redirect -- not the credential. It used to
		// land in fail() unclamped and come back out as a 500, so every
		// request walked and cooled all 22 credentials for a fault that no
		// credential could have avoided. 409 fails this one request fast.
		_, _ = s.readUpstreamStream(stream)
		return stream, fail(409, "upstream_redirect",
			fmt.Sprintf("Basis Points answered %d instead of a stream; a proxy or redirect is in the path", stream.StatusCode))
	}
	if stream.StatusCode < 200 || stream.StatusCode >= 300 {
		// Drain the rejected stream so the caller learns why. A fixed
		// "stream was rejected" string told nobody anything: the caller could
		// not see that, say, an image part was unsupported, and CPA could not
		// read error.type to tell a malformed request apart from a bad
		// credential -- so it charged the failure to the credential and pulled
		// it out of service.
		body, readErr := s.readUpstreamStream(stream)
		if readErr != nil || len(bytes.TrimSpace(body)) == 0 {
			return stream, fail(stream.StatusCode, "upstream_error", "Basis Points rejected the request and returned no detail")
		}
		return stream, statusError(stream.StatusCode, body)
	}
	return stream, nil
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return redactTokenMessage(err.Error())
}

func (s *Service) readUpstreamStream(stream upstreamStream) ([]byte, error) {
	cfg := s.config()
	if stream.StreamID == "" {
		return nil, fail(502, "upstream_transport", "upstream stream ID is empty")
	}
	defer func() { _ = s.call("host.http.stream_close", map[string]any{"stream_id": stream.StreamID}, nil) }()
	deadline := time.Now().Add(time.Duration(cfg.TimeoutSeconds) * time.Second)
	var buffer bytes.Buffer
	for {
		if time.Now().After(deadline) {
			return nil, timeoutError(cfg)
		}
		var chunk streamChunk
		if err := s.call("host.http.stream_read", map[string]any{"stream_id": stream.StreamID}, &chunk); err != nil {
			return nil, fail(502, "upstream_transport", "Basis Points stream read failed: "+safeError(err))
		}
		if chunk.Error != "" {
			return nil, fail(502, "upstream_transport", "Basis Points stream interrupted: "+safeError(errors.New(chunk.Error)))
		}
		if len(chunk.Payload) > 0 {
			if buffer.Len()+len(chunk.Payload) > cfg.MaxResponseBytes {
				return nil, fail(502, "upstream_response_too_large", "Basis Points response exceeds configured limit")
			}
			_, _ = buffer.Write(chunk.Payload)
		}
		if chunk.Done {
			return buffer.Bytes(), nil
		}
	}
}

func parseFinalStreamResponse(raw []byte) (map[string]any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fail(502, "invalid_upstream_response", "Basis Points returned an empty stream")
	}
	if strings.HasPrefix(trimmed, "{") {
		var object map[string]any
		if json.Unmarshal([]byte(trimmed), &object) == nil {
			return object, nil
		}
	}
	decoder := newSSEDecoder()
	var completed, failure map[string]any
	err := decoder.feed([]byte(raw), func(event, data string) error {
		if strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		var object map[string]any
		if json.Unmarshal([]byte(data), &object) != nil {
			return nil
		}
		typeName := stringValue(object["type"])
		switch typeName {
		case "response.completed", "response.incomplete":
			// An incomplete response -- cut off at the output limit, say -- is
			// still a finished response, and the Responses API returns it with
			// 200 and status "incomplete" rather than as an error.
			if response := objectValue(object["response"]); response != nil {
				completed = response
			}
		case "response.failed":
			if response := objectValue(object["response"]); response != nil {
				if reason := objectValue(response["error"]); reason != nil {
					failure = reason
				}
			}
		case "error":
			if reason := objectValue(object["error"]); reason != nil {
				failure = reason
			}
		}
		if response := objectValue(object["response"]); response != nil && stringValue(response["status"]) == "completed" {
			completed = response
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if completed == nil {
		if failure != nil {
			// Basis Points reports rejections such as context_length_exceeded
			// in-band, as response.failed after a 200. Surface it as the error
			// it is, with its own envelope, so the host can tell a bad request
			// from a failing credential; a generic 502 blamed the credential.
			return nil, statusError(inBandFailureStatus(failure), jsonBytes(map[string]any{"error": failure}))
		}
		return nil, fail(502, "invalid_upstream_response", "Basis Points stream ended without a terminal event")
	}
	return completed, nil
}

// inBandFailureStatus gives an in-stream failure the HTTP status it would have
// carried as an ordinary error response. Anything that is not explicitly the
// request's fault stays an upstream failure.
// inBandFailureStatus classifies a response.failed that arrived inside an
// HTTP 200 body.
//
// The upstream answered 200, so the credential demonstrably works: reporting
// these as 5xx made the host cool it and move on to the next one, and because
// the same prompt fails identically everywhere, one client-caused rejection --
// a flagged prompt, an over-long context -- walked the whole pool and cooled
// every credential in it. Anything that names the request as the problem is a
// 4xx, quota and rate limits are 429, and an unrecognised or missing type
// falls back to 400 rather than 502 for the same reason: a 200 from the
// upstream is not evidence of a credential fault.
func inBandFailureStatus(failure map[string]any) int {
	errorType := strings.ToLower(strings.TrimSpace(stringValue(failure["type"])))
	errorCode := strings.ToLower(strings.TrimSpace(stringValue(failure["code"])))
	switch errorType {
	case "rate_limit_exceeded", "insufficient_quota":
		return 429
	case "server_error", "api_error", "internal_error":
		return 502
	}
	switch errorCode {
	case "rate_limit_exceeded", "insufficient_quota":
		return 429
	}
	return 400
}

type sseDecoder struct {
	buffer strings.Builder
	data   []string
	event  string
}

func newSSEDecoder() *sseDecoder { return &sseDecoder{} }

func (d *sseDecoder) feed(chunk []byte, emit func(event, data string) error) error {
	d.buffer.Write(chunk)
	text := d.buffer.String()
	for {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			d.buffer.Reset()
			d.buffer.WriteString(text)
			return nil
		}
		line := strings.TrimSuffix(text[:index], "\r")
		text = text[index+1:]
		if line == "" {
			if len(d.data) > 0 {
				if err := emit(d.event, strings.Join(d.data, "\n")); err != nil {
					return err
				}
			}
			d.data = nil
			d.event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			d.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			d.data = append(d.data, strings.TrimPrefix(value, " "))
		}
	}
}

// outboundBody serialises the request. Off keeps Go's own marshalling, so the
// bytes on the wire are unchanged from what is deployed; from hygiene upwards
// the body is written in the real client's key order and without HTML escaping.
func outboundBody(body map[string]any, mode identityMode) []byte {
	if mode == identityOff {
		return jsonBytes(body)
	}
	return responsesBodyBytes(body)
}

// cookieHeader and rememberCookies are the two ends of the jar. Both are no-ops
// unless the seat has a persona and the operator has turned cookies on, so the
// off path allocates nothing and sends nothing.
func (s *Service) cookieHeader(who persona, cfg Config, mode identityMode) string {
	if mode == identityOff || who.empty() || !cfg.ClientIdentity.KeepCookies {
		return ""
	}
	return s.cookies.header(who.seed)
}

func (s *Service) rememberCookies(who persona, cfg Config, mode identityMode, headers http.Header) {
	if mode == identityOff || who.empty() || !cfg.ClientIdentity.KeepCookies {
		return
	}
	s.cookies.remember(who.seed, headers)
}
