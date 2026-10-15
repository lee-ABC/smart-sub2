package basispoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type Service struct {
	mu      sync.RWMutex
	cfg     Config
	host    HostCall
	stopped bool
	// draining is set while shutdown waits for live relays. Unlike stopped it
	// is never cleared by a reconfigure, because the whole point is that no new
	// relay may arm the wait group while drainStreams is waiting on it.
	draining bool
	// streams tracks relay goroutines that are still calling into the host.
	streams sync.WaitGroup
	// gate counts in-flight requests per credential to enforce its cap.
	gate *concurrencyGate
	// ledger keeps what each credential's requests were worth, and how its
	// quota moved, so the two can be turned into dollars.
	ledger *costLedger
	// cookies holds one cookie jar per credential, so a seat keeps the cookies
	// the upstream sets for it the way a browser would.
	cookies *cookieJar
	// personas remembers the identity each seat last presented.
	//
	// The management page can only see credential file names, and part of a
	// seat's identity -- the subscription date that floors its session age --
	// lives inside the credential itself. Without this the page would have to
	// recompute from the file name alone and would print a task_id that is not
	// the one on the wire, which is worse than printing nothing.
	personas   map[string]persona
	personasMu sync.RWMutex
	// configWarnings are the relay switch fallbacks of the last configuration;
	// configWarningsPending says they still have to be logged (on the next
	// request; see flushConfigWarnings). Both are guarded by mu.
	configWarnings        []string
	configWarningsPending bool
}

func NewService() *Service {
	cfg := defaultConfig()
	return &Service{cfg: cfg, gate: newConcurrencyGate(), ledger: newCostLedger(), cookies: newCookieJar(), personas: map[string]persona{}}
}

func (s *Service) SetHost(host HostCall) {
	s.mu.Lock()
	s.host = host
	s.mu.Unlock()
}

func (s *Service) call(method string, payload any, out any) error {
	s.mu.RLock()
	host := s.host
	s.mu.RUnlock()
	if host == nil {
		return errors.New("host callback is not initialized")
	}
	return host(method, payload, out)
}

func (s *Service) configure(raw json.RawMessage) error {
	cfg := defaultConfig()
	var warnings []string
	var request struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return fail(400, "invalid_config", "plugin configuration request is invalid")
		}
	}
	// Defaults first, then the on-disk cache, then the operator's config.yaml.
	//
	// 0.1.1 read settings.json AFTER applying config_yaml and then wrote the
	// merged result straight back, so the cached file silently outranked the
	// operator and re-pinned itself on every register. Editing config.yaml could
	// not undo it. Loading the cache first keeps it as a fallback while leaving
	// CPA's configuration authoritative.
	if cfg.DataDir != "" {
		if data, err := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json")); err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				// A truncated or hand-edited cache must not silently define
				// where credentials get sent; fall back to defaults instead.
				cfg = defaultConfig()
			}
		}
	}
	// Every h25 relay switch is reset whether or not config.yaml came with
	// this call: a switch takes effect only from a line in config.yaml, so
	// deleting the line -- or the whole plugin section -- restores the default
	// (h22's behaviour) instead of the value cached in settings.json.
	cfg.resetRelaySwitches()
	if len(request.ConfigYAML) > 0 {
		// The caps are edited live from the control panel, so they must follow
		// config.yaml exactly. Unmarshalling into the cached map would only
		// ever merge keys, so an override removed in the panel would come back
		// from settings.json, and a cap deleted to restore the default would
		// keep its cached value instead.
		defaults := defaultConfig()
		cfg.MaxConcurrencyPerCredential = defaults.MaxConcurrencyPerCredential
		cfg.CredentialConcurrency = nil
		cfg.CompactThreshold = defaults.CompactThreshold
		// Same reason as the caps: a false written to turn the guard off must
		// not outlive the line that wrote it. Removing the line turns it back
		// on instead of leaving the cached false in charge.
		cfg.HideAccountName = defaults.HideAccountName
		// h26: the web search guard follows config.yaml too; deleting the line
		// turns it back off instead of keeping a cached value.
		cfg.WebSearchGuard = defaults.WebSearchGuard
		cfg.WebSearchGuardTTLSeconds = defaults.WebSearchGuardTTLSeconds
		cfg.WebSearchFailover = defaults.WebSearchFailover
		cfg.WebSearchFailoverScope = defaults.WebSearchFailoverScope
		// The identity switches are edited live too, so they follow config.yaml
		// for the same reason the caps do: a mode turned back off in the panel
		// must not be restored from the cache on the next register.
		//
		// The pepper is the exception and is deliberately not reset. It is not
		// a setting an operator chooses, it is a value the pool has to keep:
		// regenerating it changes every seat's cache key in the same second,
		// which is one pool-wide cold window bought for nothing.
		pepper := cfg.ClientIdentity.Pepper
		cfg.ClientIdentity = defaults.ClientIdentity
		cfg.ClientIdentity.Pepper = pepper
		// (The h25 relay switches were reset above, for the same reason.)
		if err := unmarshalYAML(request.ConfigYAML, &cfg); err != nil {
			return fail(400, "invalid_config", "plugin configuration is invalid: "+err.Error())
		}
		// The switches are tagged yaml:"-" and read here from raw nodes, so a
		// bad value is a warning and a default, never a failed registration.
		warnings = append(warnings, applyRelaySwitchYAML(request.ConfigYAML, &cfg)...)
		if strings.TrimSpace(cfg.ClientIdentity.Pepper) == "" {
			cfg.ClientIdentity.Pepper = pepper
		}
	}
	if strings.TrimSpace(cfg.ClientIdentity.Pepper) == "" {
		cfg.ClientIdentity.Pepper = newPepper()
	}
	warnings = append(warnings, cfg.normalizeRelaySwitches()...)
	if err := cfg.normalize(); err != nil {
		return err
	}
	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
			return fail(500, "config_storage", "cannot create plugin data directory")
		}
		data, _ := json.MarshalIndent(cfg, "", "  ")
		_ = os.WriteFile(filepath.Join(cfg.DataDir, "settings.json"), data, 0600)
	}
	s.ledger.setPath(cfg.ledgerPath())
	// Experiment self-check (bpsexp build only): arm or loudly disarm every
	// experiment feature from the configuration. A no-op in the production
	// build.
	expConfigure(s, request.ConfigYAML, cfg)
	s.mu.Lock()
	s.cfg = cfg
	s.configWarnings = warnings
	s.configWarningsPending = len(warnings) > 0
	// A reconfigure that lands while shutdown is draining must not revive the
	// service: the next stream would arm the wait group the drain is waiting on,
	// which is the fatal reuse. draining outlives this call and keeps the door
	// shut until the drain finishes.
	if !s.draining {
		s.stopped = false
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) Handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := s.configure(raw); err != nil {
			return nil, err
		}
		return registration(s.config()), nil
	case "plugin.quiesce":
		return map[string]any{}, nil
	case "auth.identifier":
		// CPA routes auth files to a plugin by this identifier. It must stay
		// equal to Provider: native `type: codex` files are left to the host,
		// and this plugin owns only its own `type: oai-basispoints` credential.
		return map[string]any{"identifier": s.authProviderID()}, nil
	case "executor.identifier":
		return map[string]any{"identifier": Provider}, nil
	case "management.register":
		return managementRegistration(), nil
	case "management.handle":
		return s.managementHandle(raw)
	case "quota.identifier":
		return map[string]any{"identifier": Provider}, nil
	case "quota.describe":
		return quotaDescribe(), nil
	case "quota.fetch":
		return s.quotaFetch(raw)
	case "quota.reset":
		return nil, fail(400, "reset_unsupported", "Basis Points quota resets on its own schedule and cannot be cleared")
	case "auth.parse":
		return s.authParse(raw)
	case "auth.login.start":
		return nil, fail(400, "login_unavailable", "Provision a `type: oai-basispoints` ChatGPT OAuth credential file; interactive login is not used")
	case "auth.login.poll":
		return map[string]any{"Status": "error", "Message": "Provision a `type: oai-basispoints` ChatGPT OAuth credential file"}, nil
	case "auth.refresh":
		return s.authRefresh(raw)
	case "model.register", "model.static", "model.for_auth":
		return modelRegistration(s.config()), nil
	case "executor.execute":
		return s.execute(raw, false)
	case "executor.execute_stream":
		return s.execute(raw, true)
	case "executor.count_tokens":
		return map[string]any{"Payload": jsonBytes(map[string]any{"input_tokens": 0})}, nil
	case "executor.http_request":
		return nil, fail(400, "unsupported_method", "use the Basis Points model executor")
	case "plugin.shutdown":
		s.mu.Lock()
		s.stopped = true
		s.draining = true
		s.mu.Unlock()
		s.drainStreams(streamDrainTimeout)
		s.ledger.flush()
		s.mu.Lock()
		s.draining = false
		s.mu.Unlock()
		return map[string]any{}, nil
	default:
		return nil, fail(400, "unsupported_method", "unsupported plugin method: "+method)
	}
}

func (s *Service) execute(raw json.RawMessage, stream bool) (any, error) {
	s.mu.RLock()
	stopped := s.stopped
	s.mu.RUnlock()
	if stopped {
		// 409, not 503: the credential is fine, the plugin is merely closing.
		// A 5xx here made the host cool each credential it tried, so every
		// request arriving during a reload window retired the whole pool.
		return nil, fail(409, "plugin_stopped", "oai-basispoints is shut down")
	}
	var request ExecutorRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fail(400, "invalid_request", "executor request is invalid")
	}
	s.flushConfigWarnings(request.HostCallbackID)
	// A large request on a credential that is already at its cap is turned
	// away before its body is parsed; see fullBeforeDecoding.
	if refusal := s.fullBeforeDecoding(request); refusal != nil {
		return nil, refusal
	}
	body, credential, selectedProtocol, err := s.prepareRequestWithRelayContext(&request)
	if err != nil {
		return nil, err
	}
	request.selectedRelayProtocol = selectedProtocol
	// Claimed only once the request is known to be well formed, so a malformed
	// one never holds a slot.
	limit, _ := s.config().concurrencyLimit(credential.Email, stringValue(request.AuthMetadata["file_name"]))
	release, admitted := s.gate.acquire(concurrencyKey(request, credential), limit)
	if !admitted {
		return nil, concurrencyLimitError(limit)
	}
	countRelayRequest(request)
	if stream {
		// The stream keeps its slot until the relay finishes; executeStream
		// hands release to the relay or calls it itself if it never starts.
		// Once the relay has started it records its own outcome.
		result, err := s.executeStream(request, body, credential, release)
		if err != nil {
			countRelayOutcome(request, err)
		}
		return result, err
	}
	defer release()
	result, terminal, err := s.executeNonStream(request, body, credential)
	countRelayOutcomeAfter(request, err, terminal)
	return result, err
}

// executeNonStream is one admitted non-streaming request, from the upstream
// call to the converted response. terminal is "response.incomplete" (or
// "response.failed") when the response returned to the caller carries that
// status, for relay_outcome only.
func (s *Service) executeNonStream(request ExecutorRequest, body map[string]any, credential credential) (any, string, error) {
	result, status, err := s.executeNonStreamResponse(request, body, credential)
	if err != nil || (status != "incomplete" && status != "failed") {
		return result, "", err
	}
	return result, "response." + status, nil
}

func (s *Service) executeNonStreamResponse(request ExecutorRequest, body map[string]any, credential credential) (any, string, error) {
	response, err := s.upstreamRequest(request, body, credential, false)
	// Native promotion: a promoted body refused with 400 is sent once more
	// as the relay body, on this credential (a1_promotion.go).
	if relayBody, fallback := s.a1FallbackBody(&request, err); fallback {
		body = relayBody
		response, err = s.upstreamRequest(request, body, credential, false)
		s.a1FallbackDone(&request, err)
	}
	a1Settled(&request)
	// Experiment capture of the raw upstream SSE body (non-streaming);
	// finalises this trial's fixture with the REAL outcome. A no-op in the
	// production build.
	expRecordNonStreamSSE(request, response.Body, err)
	if err != nil {
		return nil, "", s.imageDownloadRefusal(err, body)
	}
	source, err := rawObject(request.OriginalRequest)
	if err != nil {
		source, err = rawObject(request.Payload)
	}
	if err != nil {
		return nil, "", err
	}
	source = withFrozenReplayScope(source, request)
	source = withRelayOptions(source, s.relayOptionsFor(request))
	source = withRelayProtocolSelection(source, request.selectedRelayProtocol)
	source = withA1Response(source, request.a1)
	// Experiment trial marker (bpsexp build only) so decode-time observers can
	// find this request's trial. A no-op in the production build.
	source = expWithTrial(source, request)
	s.attachSchemaObservation(source, request.HostCallbackID)
	s.attachRelayDecodeNotes(source, request.HostCallbackID)
	// Basis Points answers with an SSE stream even when the request sets
	// stream:false, so the raw body is an event stream rather than a Responses
	// object. Decoding it as JSON is what made every non-streaming call fail
	// with "Basis Points returned invalid JSON". parseFinalStreamResponse
	// accepts both shapes: it returns a bare JSON object untouched and
	// otherwise extracts the completed response from the event stream.
	finalResponse, err := parseFinalStreamResponse(response.Body)
	if err != nil {
		return nil, "", err
	}
	target := targetFor(request, credential)
	s.recordResponse(target, request.Model, finalResponse)
	s.maybeSampleQuota(target, credential)
	// a1wide2: run_officejs calls no client tool can stand for are answered
	// upstream and the responses joined (no_catalog_transport.go).
	if noCatalogTransport(source) {
		if finalResponse, err = s.answerNoCatalogNonStream(request, body, credential, target, finalResponse); err != nil {
			return nil, "", err
		}
	}
	transformed, _, _, err := transformResponseBody(jsonBytes(finalResponse), source)
	if err != nil {
		err = withToolDiagnostic(err, "non_stream_response", -1, false)
		s.logToolRejection(request.HostCallbackID, source, err)
		return nil, "", err
	}
	return map[string]any{
		"Payload": transformed,
		"Headers": responseJSONHeaders(response.Headers),
	}, stringValue(finalResponse["status"]), nil
}

// responseJSONHeaders drops upstream transfer framing headers that describe the
// SSE body we just collapsed into a single JSON payload. Passing
// Content-Type: text/event-stream back to a non-streaming caller makes clients
// wait for events that will never arrive.
func responseJSONHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	cleaned := make(http.Header, len(headers))
	for name, values := range headers {
		switch http.CanonicalHeaderKey(name) {
		case "Content-Type", "Content-Length", "Transfer-Encoding", "Content-Encoding":
			continue
		}
		cleaned[name] = values
	}
	cleaned.Set("Content-Type", "application/json")
	return cleaned
}

// beginStream arms the wait group for a relay that is about to start, and
// refuses once shutdown has begun.
//
// The increment has to happen under the same lock that sets `stopped`. The
// check at the top of execute() is far too early to stand in for it: a request
// that passes it then spends a whole upstream connection setup in
// upstreamStream before it would reach this point, and plugin.shutdown can
// land in that gap. sync.WaitGroup reads an Add that arrives while a Wait is
// unblocking a zero counter as reuse of the group and calls it a fatal error
// ("WaitGroup is reused before previous Wait has returned"), which is a
// runtime throw rather than a panic: the recover in the relay goroutine cannot
// catch it, nor can the one wrapping the cgo entry point, so it takes the
// whole gateway process down -- every provider, every credential, every
// request in flight. Holding the lock across the check and the Add means no
// Add can follow the flag, so drainStreams' Wait never races one.
func (s *Service) beginStream() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.draining {
		return false
	}
	s.streams.Add(1)
	return true
}

func (s *Service) executeStream(request ExecutorRequest, body map[string]any, credential credential, release func()) (any, error) {
	// Until the relay goroutine takes ownership of the slot, every early
	// return below has to give it back.
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	if request.StreamID == "" {
		return nil, fail(500, "stream_id_missing", "executor.execute_stream requires stream_id")
	}
	// The relay needs the caller's own request to recognise which client tool a
	// transport call stands for. Parsing it up front turns a malformed request
	// into a plain error instead of a stream that dies halfway through.
	source, err := rawObject(request.OriginalRequest)
	if err != nil {
		source, err = rawObject(request.Payload)
	}
	if err != nil {
		return nil, err
	}
	freezeReplayScopeFromSource(&request, source)
	source = withFrozenReplayScope(source, request)
	source = withRelayOptions(source, s.relayOptionsFor(request))
	source = withRelayProtocolSelection(source, request.selectedRelayProtocol)
	s.attachSchemaObservation(source, request.HostCallbackID)
	s.attachRelayDecodeNotes(source, request.HostCallbackID)
	upstream, err := s.upstreamStream(request, body, credential)
	// Native promotion: a promoted body refused with 400 before the stream
	// opened is sent once more as the relay body, on this credential. The
	// client has been sent nothing yet (a1_promotion.go).
	if relayBody, fallback := s.a1FallbackBody(&request, err); fallback {
		body = relayBody
		upstream, err = s.upstreamStream(request, body, credential)
		s.a1FallbackDone(&request, err)
	}
	a1Settled(&request)
	if err != nil {
		// The stream never opened, so the relay goroutine (and with it
		// closeWithError -> expRecordStreamEnd) will never run. Finalise the
		// capture here or this trial -- exactly the promoted-body-refused-by-400
		// case stage 1 exists to collect -- leaves no fixture at all and leaks
		// its captured request body in the registry (audit drv-F6-1).
		expRecordStreamEnd(request, source, err)
		return nil, s.imageDownloadRefusal(err, body)
	}
	// Only a response to the promoted body is decoded as a promoted one.
	source = withA1Response(source, request.a1)
	// Experiment trial marker (bpsexp build only) so decode-time observers can
	// find this request's trial. A no-op in the production build.
	source = expWithTrial(source, request)
	// a1wide2: a request without a callable client tool keeps its prepared
	// body, so that a run_officejs call can be answered upstream on this
	// credential (no_catalog_transport.go). Every other stream lets go of it.
	var call *upstreamCall
	if noCatalogCandidate(source, request.relayKind) {
		continuationRequest := request
		continuationRequest.OriginalRequest, continuationRequest.Payload = nil, nil
		call = &upstreamCall{body: body, callbackID: request.HostCallbackID,
			clientShapes: s.requestClientShapes(request, credential),
			open: func(next map[string]any) (upstreamStream, error) {
				return s.upstreamStream(continuationRequest, next, credential)
			}}
	}
	// Registered before the goroutine starts so plugin.shutdown cannot observe
	// an idle wait group while this relay is about to call into the host.
	if !s.beginStream() {
		return nil, fail(409, "plugin_stopped", "oai-basispoints is shut down")
	}
	// The relay below needs the stream id, the model and the credential's
	// identity, not the request body. Letting go of the two copies of it here
	// means a long answer no longer pins them: for a request at the image
	// ceiling that is about half a gigabyte for as long as the stream runs.
	request.OriginalRequest, request.Payload = nil, nil
	handedOff = true
	outcome := newRelayOutcomeRecorder(request)
	// h26: the same caller-scoped conversation key the body builder used.
	webSearchScope := request.replayCacheScope
	go func() {
		defer s.streams.Done()
		// Deferred ahead of the recover below, so it runs after it: the slot
		// is freed once the stream is closed, even when the relay panics.
		defer release()
		// What the client has been sent, also known after a panic.
		progress := &streamProgress{scope: webSearchScope}
		closeWithError := func(err error) {
			outcome.recordAfter(err, progress.upstreamTerminal())
			noteWebSearchStreamOutcome(s.config(), webSearchScope, err, progress)
			// Experiment: finalise this trial's fixture with the captured SSE
			// and the outcome, and queue an A2 shadow rewrite if this relay
			// arm failed with invalid_tool_call. No-ops in production.
			expRecordStreamEnd(request, source, err)
			expEnqueueShadowRewrite(s, request, source, credential, err)
			payload := map[string]any{"stream_id": request.StreamID}
			if err != nil {
				s.logToolRejection(request.HostCallbackID, source, err)
				// host.stream.close carries no status field, so everything
				// that goes wrong mid-stream reaches the host as one opaque
				// string and a client hanging up looks exactly like a dead
				// upstream. The cause is prefixed into the message instead:
				// the wire shape stays a string, which the host is known to
				// decode, and the log finally says which it was.
				payload["error"] = streamCloseMessage(err, relayOptionsFrom(source), !progress.outputSent.Load())
			}
			_ = s.call("host.stream.close", payload, nil)
		}
		// This plugin is a dynamic library loaded into the CPA process, and this
		// goroutine runs after executor.execute_stream has already returned, so
		// it is outside any host-side recover. An unhandled panic here would
		// take the whole gateway down, not just this request.
		defer func() {
			if recovered := recover(); recovered != nil {
				closeWithError(fail(500, "stream_panic", fmt.Sprintf("basispoints stream aborted: %v", recovered)))
			}
		}()
		target := targetFor(request, credential)
		closeWithError(s.relayUpstreamStreamCall(request.StreamID, upstream, source, target, request.Model, progress, call))
		s.maybeSampleQuota(target, credential)
	}()
	return map[string]any{"Headers": map[string][]string{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}}, nil
}

func (s *Service) status() map[string]any {
	cfg := s.config()
	s.mu.RLock()
	stopped := s.stopped
	s.mu.RUnlock()
	return map[string]any{
		"provider":          Provider,
		"version":           Version,
		"responses_url":     cfg.ResponsesURL,
		"upstream_model":    cfg.UpstreamModel,
		"models":            cfg.Models,
		"stopped":           stopped,
		"reasoning_efforts": []string{"low", "medium", "high", "xhigh"},
	}
}

func registration(cfg Config) map[string]any {
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{
			"Name":             "OpenAI Basis Points",
			"Version":          Version,
			"Author":           "jaxson-wang",
			"GitHubRepository": "https://github.com/xiao-qiu-qiu/ClinePassBridge",
			"Description":      "CPA Responses adapter for bps.openai.com with safe client-tool relay",
			"ConfigFields": []map[string]any{
				{"Name": "responses_url", "Type": "string", "Description": "Basis Points Responses endpoint."},
				{"Name": "upstream_model", "Type": "string", "Description": "Model name sent to Basis Points."},
				{"Name": "models", "Type": "array", "Description": "CPA-facing model aliases served by this plugin."},
				{"Name": "timeout_seconds", "Type": "integer", "Description": "Upstream request timeout."},
				{"Name": "max_response_bytes", "Type": "integer", "Description": "Maximum upstream response size."},
				{"Name": "auth_mode", "Type": "string", "Description": "Basis Points authentication mode; normally chatgpt."},
				{"Name": "tools_version_id", "Type": "string", "Description": "Optional authoritative Basis Points tools catalog version."},
				{"Name": "web_search_guard", "Type": "string", "Description": "上游自带联网搜索故障兜底:off(默认,与 h25 相同)、retry(某对话出现「搜索后上游报错」后,在 web_search_guard_ttl_seconds 内给该对话的请求末尾加一句「刚才的搜索失败了,别再搜,继续任务」)、always(所有请求都加,应急用)。新值对之后的请求生效;删行即回 off。"},
				{"Name": "web_search_failover", "Type": "boolean", "Description": "上游自带联网搜索失败时让主站换号重做(h27):以思考或搜索开头的回合先扣住不发,出现正文或工具调用即放行;若搜索后上游报 server_error,则以零输出失败结束,主站换到其它上游重做这一轮。默认 false(与 h25 相同);删行即回默认;新值对之后的请求生效。"},
				{"Name": "web_search_failover_scope", "Type": "string", "Description": "web_search_failover 的范围(h27c):after_failure(默认,只对 web_search_guard_ttl_seconds 内出过「搜索后失败」的对话整轮扣住再换号,其余对话不受影响)、all(h27b:所有以思考/搜索开头的回合都扣住,会拖慢首字)。"},
				{"Name": "web_search_guard_ttl_seconds", "Type": "integer", "Description": "retry 模式下,一个对话在搜索失败后保持加提示的秒数,默认 900,范围 60~86400。"},
				{"Name": "max_concurrency_per_credential", "Type": "integer", "Description": "每个凭据同时处理的最大请求数,所有凭据默认使用此值。0 为不限。修改后立即生效。"},
				{"Name": "credential_concurrency", "Type": "object", "Description": "单独设置某些凭据的并发上限,JSON 格式,键为邮箱,例如 {\"someone@example.com\": 3}。未列出的凭据使用默认值。"},
				{"Name": "hide_account_name", "Type": "boolean", "Description": "不让模型用上游注入的账号资料名称呼调用方(也不写进落款、提交作者等)。默认开启;写 false 关闭。修改后立即生效。"},
				{"Name": "relay_protocol", "Type": "string", "Description": "中继工具调用的信封格式:legacy(默认,与 h22 相同,忽略 references)、auto(仅新的中继会话用 references)、references。删行即回默认;写错回落默认并记一条警告。新值对之后的请求生效。"},
				{"Name": "relay_tool_contracts", "Type": "string", "Description": "中继目录写法:summary(默认,与 h22 相同,只列参数名)、full(完整工具定义,超出预算时自动退回 summary)。"},
				{"Name": "relay_tool_delivery", "Type": "string", "Description": "中继调用交付时机:immediate(默认,与 h22 相同,每个调用完成即交付)、batch(整轮校验后一起交付,扣住期间每 15 秒发一行 SSE 注释保活)。"},
				{"Name": "tool_policy_checks", "Type": "string", "Description": "并行数量、tool_choice、原生调用一致性等新增检查:log(默认,只记日志照常交付)、enforce(拒绝;但已向客户端交付过完整工具调用后只记日志,不再让整条流失败)。"},
				{"Name": "history_conflict", "Type": "string", "Description": "历史调用与已交付的不一致时:replay(默认,与 h22 相同:中继调用回放缓存里模型原始的 run_officejs,原生调用按客户端当前内容发,只记日志,从不拒绝)、reject(rc2 的历史处理:同一调用方、同一凭据、同一显式会话内不一致才返回 400,其余记日志后按 h22 冷路径发)。"},
				{"Name": "relay_incomplete", "Type": "string", "Description": "上游截断的中继调用:error(默认,与 h22 相同,报 invalid_tool_call 并带原因码)、passthrough(透传 response.incomplete/failed,并按 rc2 更严格地读流:终态缺 response、状态矛盾、事件 JSON 后有多余字节都判失败)。"},
				{"Name": "relay_hide_encrypted_tools", "Type": "boolean", "Description": "中继模式下,参数 schema 里带 encrypted:true 的命名空间工具(如 collaboration.spawn_agent)不列入中继目录,改为一句「不可用」说明;模型仍经 run_officejs 调用到它们时(不论声明在 tools、additional_tools 还是 tool_search_output),交回客户端时不带 namespace,客户端会回 unsupported call,不再把明文当密文送上游。Lite 原生调用不受影响;扁平名不受影响。默认开启;写 false 关闭(与 h22 相同)。删行即回默认。新值对之后的请求生效。"},
				{"Name": "coerce_scalar_strings", "Type": "boolean", "Description": "中继 function 调用里数字/布尔被写成字符串且因此校验失败时,按 schema 转回后再校验。默认开启;写 false 关闭。"},
				{"Name": "retryable_error_hint", "Type": "boolean", "Description": "零输出失败的报错文案带上主站会换号重试的提示词。默认开启;写 false 关闭。"},
				{"Name": "schema_shadow", "Type": "boolean", "Description": "只记录参数与完整 JSON Schema 的比对结果,不改变任何判断。默认关闭。"},
				{"Name": "relay_native_promotion", "Type": "string", "Description": "原生提升(A1,实验):off(默认,与 h25 相同)、shadow(只计算能否提升并计数,发往上游的请求体逐字节不变)、on(中继客户端的顶层 tools 按 Codex 官方 Lite 规则放进 input[0] 的 additional_tools,由上游原生调用;上游在开流前回 400 时同号改用中继请求体重发一次,客户端收不到任何字节)。删行即回默认;写错回落 off 并记一条警告。新值对之后的请求生效。"},
				{"Name": "relay_native_promotion_history", "Type": "string", "Description": "原生提升后历史调用的写法:h1(默认,以前经 run_officejs 的调用照旧回放 run_officejs,新旧混合)、h2(以前的调用一律按客户端原样以原生调用发往上游)。只在 relay_native_promotion 为 on 时起作用。"},
				{"Name": "relay_native_promotion_block", "Type": "string", "Description": "带这些特征的请求不做原生提升(逗号分隔):ref、composition、enum、type_list、strict_true、custom_format_other、additional_properties_true、additional_properties_schema、encrypted、compacted、custom_tool、namespace、builtin_name、compaction(对话里带压缩项)、tool_choice(tool_choice 不是 auto);none 表示都不拦。默认 custom_format_other(2026-10-09 阶段 1 实测:上游接受 enum/组合/ref/type_list/additional_properties 并真的原生调用;custom 文法虽收但模型不用。2026-10-15 a1wide 起 strict_true 与 compaction 也提升:提升后的目录一律 strict:false;a1wide2 起 tool_choice 也提升:指定工具/allowed_tools 只声明允许的工具,required/指定工具在输入末尾加一句强制说明。要恢复 a1wide 写 custom_format_other,tool_choice;恢复更早的写 compaction,custom_format_other,strict_true,tool_choice,热生效)。写错回落默认并记一条警告。"},
				{"Name": "relay_no_catalog_transport", "Type": "string", "Description": "请求里没有任何可调用的客户端工具(没声明工具或 tool_choice:none)时模型却调了上游的 run_officejs(多见于 Codex 压缩对话):error(默认,与 h22/a1wide 相同:按隧道解码,报 invalid_tool_call);continue(a1wide2)不当隧道解码、不发给客户端,若它结束了这一轮,插件同号给它回一条「不可用、请直接文字作答」再续一轮上游,续写接在同一个流里(用量合计,每请求最多 2 次)。删行即回默认;写错回落默认并记一条警告。新值对之后的请求生效。"},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":           true,
			"model_provider":          true,
			"executor":                true,
			"executor_model_scope":    "both",
			"executor_input_formats":  []string{"openai-response"},
			"executor_output_formats": []string{"openai-response"},
			"management_api":          true,
			"quota_provider":          true,
		},
		"config": cfg,
	}
}

func modelRegistration(cfg Config) map[string]any {
	models := make([]map[string]any, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		models = append(models, map[string]any{
			"ID":     model,
			"Object": "model",
			// Each exposed model reports the name it actually reaches upstream.
			// 0.1.1 stamped every entry with the single global upstream_model,
			// so exposing more than one alias silently collapsed them all onto
			// the same model.
			"Name":                       upstreamModelFor(model, cfg),
			"OwnedBy":                    Provider,
			"DisplayName":                model,
			"SupportedGenerationMethods": []string{"responses"},
			"SupportedInputModalities":   []string{"text"},
			"SupportedOutputModalities":  []string{"text"},
			"Thinking":                   map[string]any{"Min": 0, "Max": 0},
			"UserDefined":                true,
		})
	}
	return map[string]any{"Provider": Provider, "Models": models}
}

func unmarshalYAML(raw []byte, value any) error {
	// Kept in one function so config parsing is easy to test and the service
	// package does not expose YAML details to the ABI layer.
	return yaml.Unmarshal(raw, value)
}
