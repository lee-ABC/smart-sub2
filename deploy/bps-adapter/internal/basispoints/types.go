package basispoints

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const (
	Version  = "0.1.2-h28-workspace1-a1wide2"
	Provider = "oai-basispoints"
	// AuthProviderID must equal Provider.
	//
	// Upstream 0.1.1 set this to "codex", which made CPA hand EVERY `type: codex`
	// OAuth file to this plugin's auth.parse. Because the host skips its own
	// parsing once a plugin reports Handled=true, and the plugin rebuilt the
	// native record without access_token/refresh_token, every existing Codex
	// model started returning 401 the moment the plugin loaded. Returning two
	// auths from one file also made the host stamp plugin_virtual on BOTH of
	// them, which permanently disables persistence and token renewal.
	//
	// Keeping this equal to Provider means:
	//   - native `type: codex` files are never touched by this plugin,
	//   - the auth record's Provider matches Identifier(), so authProviderRecord
	//     resolves and auth.refresh is actually reachable.
	// This plugin now uses its own dedicated `type: oai-basispoints` credential.
	AuthProviderID = Provider
	PluginID       = Provider

	DefaultResponsesURL  = "https://bps.openai.com/basispoints/api/responses"
	DefaultUpstreamModel = "gpt-6-astra"
	DefaultModelID       = DefaultUpstreamModel
)

// bpsModels is the set Basis Points actually serves, verified against a live
// account: every other name (gpt-5.5, gpt-5.4, gpt-6-sol, ...) comes back as
// 403 basispoints_model_access_changed.
//
// Models are exposed under their real names rather than a plugin-specific
// alias, so a downstream model whitelist needs no rewriting and requests can be
// pooled with any other credential serving the same model.
var bpsModels = []string{
	"gpt-6-astra",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
}

func isBPSModel(model string) bool {
	model = strings.TrimSpace(model)
	for _, candidate := range bpsModels {
		if model == candidate {
			return true
		}
	}
	return false
}

// upstreamModelFor resolves what to send upstream for a requested model.
// Names Basis Points serves are passed through untouched; anything else falls
// back to the configured upstream_model, which keeps an explicit alias such as
// "gpt-6-astra-basispoints" working for existing deployments.
func upstreamModelFor(model string, cfg Config) string {
	model = strings.TrimSpace(model)
	if mapped := strings.TrimSpace(cfg.ModelUpstreams[model]); mapped != "" {
		return mapped
	}
	if isBPSModel(model) {
		return model
	}
	return cfg.UpstreamModel
}

var supportedReasoningEfforts = map[string]struct{}{
	"low": {}, "medium": {}, "high": {}, "xhigh": {},
}

// APIError carries a downstream HTTP status through the CPA plugin envelope.
type APIError struct {
	Status  int
	Kind    string
	Message string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *APIError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Status
}

func (e *APIError) Code() string {
	if e == nil || e.Kind == "" {
		return "plugin_error"
	}
	return e.Kind
}

func fail(status int, kind, message string) error {
	return &APIError{Status: status, Kind: kind, Message: message}
}

// allowedUpstreamHosts bounds where the ChatGPT OAuth bearer token may be sent.
// responses_url is operator-configurable and is also re-read from an on-disk
// settings file, so without this an attacker who can write that file (or a
// mistaken config edit) could redirect live credentials to any host.
var allowedUpstreamHosts = map[string]struct{}{
	"bps.openai.com": {},
}

func isAllowedUpstreamHost(host string) bool {
	_, ok := allowedUpstreamHosts[strings.ToLower(strings.TrimSpace(host))]
	return ok
}

type HostCall func(method string, payload any, out any) error

// ExecutorRequest mirrors CPA's JSON executor contract. HTTPClient is not part
// of the JSON ABI; this plugin deliberately uses the host callback instead.
type ExecutorRequest struct {
	AuthID                string            `json:"AuthID"`
	AuthProvider          string            `json:"AuthProvider"`
	Model                 string            `json:"Model"`
	Format                string            `json:"Format"`
	Stream                bool              `json:"Stream"`
	Alt                   string            `json:"Alt"`
	Headers               http.Header       `json:"Headers"`
	Query                 url.Values        `json:"Query"`
	OriginalRequest       []byte            `json:"OriginalRequest"`
	SourceFormat          string            `json:"SourceFormat"`
	Payload               []byte            `json:"Payload"`
	Metadata              map[string]any    `json:"Metadata"`
	StorageJSON           []byte            `json:"StorageJSON"`
	AuthMetadata          map[string]any    `json:"AuthMetadata"`
	AuthAttributes        map[string]string `json:"AuthAttributes"`
	StreamID              string            `json:"stream_id,omitempty"`
	HostCallbackID        string            `json:"host_callback_id,omitempty"`
	selectedRelayProtocol string            // Internal only; unexported and never part of the host JSON ABI.
	relayOptions          *relayOptions     // Switches frozen when the request was prepared; never serialised.
	replayCacheScope      string            // Frozen replay cache scope: caller_scope + h22's conversation key.
	replayStrictScope     string            // Frozen rc2 caller/credential/explicit-session scope; gates history_conflict: reject only.
	replayScopeFrozen     bool              // Even an empty scope is an authoritative opt-out of shared caches.
	replayScopeSource     string            // What the scope was keyed on (fixed label), counted on admission.
	replayScopeCaller     string            // present / absent: caller_scope, counted on admission.
	relayKind             string            // relay / lite / none / promoted, for the relay-status counters only.
	a1                    *a1Plan           // Native promotion plan (shadow / on only); never serialised.
}

type ExecutorResponse struct {
	Payload  []byte         `json:"Payload"`
	Headers  http.Header    `json:"Headers"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type StreamResponse struct {
	Headers http.Header `json:"Headers"`
}

// upstreamResponse mirrors pluginapi.HTTPResponse, which the host marshals with
// no struct tags (bare json.Marshal in marshalRPCResult). The wire keys are the
// Go field names, so snake_case tags here silently decode StatusCode as 0 and
// turn every non-streaming call into a bogus 502.
// The streaming path is different: host.http.do_stream marshals
// rpcHostHTTPStreamResponse, which DOES carry snake_case tags -- see upstreamStream.
type upstreamResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type upstreamStream struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type streamChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

type Config struct {
	DataDir          string            `yaml:"data_dir" json:"data_dir"`
	ResponsesURL     string            `yaml:"responses_url" json:"responses_url"`
	UpstreamModel    string            `yaml:"upstream_model" json:"upstream_model"`
	ModelUpstreams   map[string]string `yaml:"model_upstreams" json:"model_upstreams"`
	Models           []string          `yaml:"models" json:"models"`
	TimeoutSeconds   int               `yaml:"timeout_seconds" json:"timeout_seconds"`
	MaxResponseBytes int               `yaml:"max_response_bytes" json:"max_response_bytes"`
	AuthMode         string            `yaml:"auth_mode" json:"auth_mode"`
	ToolsVersionID   string            `yaml:"tools_version_id" json:"tools_version_id"`
	// The h25 relay switches. Every default reproduces h22. They are read from
	// config.yaml by applyRelaySwitchYAML rather than by the typed decoder
	// (yaml:"-"), so an unreadable value falls back to its default with a
	// warning instead of failing registration; see relay_options.go.
	//
	// SchemaShadow observes selected function arguments without changing the
	// compatibility decision. It is opt-in until real client dialects have
	// been compared. Unsupported or resource-heavy schemas are skipped.
	SchemaShadow bool `yaml:"-" json:"schema_shadow"`
	// RelayProtocol: legacy (h22's run_officejs envelope, references ignored),
	// auto (references for new relay conversations only) or references.
	RelayProtocol string `yaml:"-" json:"relay_protocol"`
	// RelayToolContracts: summary (h22's one-line directory) or full (complete
	// definitions; falls back to summary when over budget).
	RelayToolContracts string `yaml:"-" json:"relay_tool_contracts"`
	// RelayToolDelivery: immediate (each relay call delivered on its own
	// output_item.done, h22) or batch (the whole turn's calls together).
	RelayToolDelivery string `yaml:"-" json:"relay_tool_delivery"`
	// ToolPolicyChecks: log (rc2's extra tool checks only log) or enforce.
	ToolPolicyChecks string `yaml:"-" json:"tool_policy_checks"`
	// HistoryConflict: replay (h22's history translation: a cached relay call
	// replays the model's own transport call whatever the client now sends,
	// and a difference is only logged) or reject (rc2's provenance-checked
	// history: 400 history_conflict when the contradicted delivery was made
	// under the same caller, credential and explicit session).
	HistoryConflict string `yaml:"-" json:"history_conflict"`
	// RelayIncomplete: error (h22: a truncated relay call is invalid_tool_call)
	// or passthrough (response.incomplete reaches the client).
	RelayIncomplete string `yaml:"-" json:"relay_incomplete"`
	// RelayHideEncryptedTools hides namespaced tools whose parameters demand
	// ciphertext from the relay catalogue.
	RelayHideEncryptedTools bool `yaml:"-" json:"relay_hide_encrypted_tools"`
	// CoerceScalarStrings turns "5"/"true" back into 5/true where the schema
	// asks for a number/integer/boolean and the call would otherwise fail.
	CoerceScalarStrings bool `yaml:"-" json:"coerce_scalar_strings"`
	// RetryableErrorHint words zero-output failures so the main site retries
	// them on another account.
	RetryableErrorHint bool `yaml:"-" json:"retryable_error_hint"`
	// RelayNativePromotion (A1, a1_promotion.go): off (h25), shadow (decide
	// and count, the upstream body unchanged) or on (a relayed request's
	// top-level tools go upstream in an additional_tools item, the Lite way;
	// a 400 before the stream opens falls back to the relay body once).
	RelayNativePromotion string `yaml:"-" json:"relay_native_promotion"`
	// RelayNativePromotionHistory: h1 (earlier relay calls stay run_officejs,
	// a mixed history) or h2 (every earlier client call is sent natively).
	RelayNativePromotionHistory string `yaml:"-" json:"relay_native_promotion_history"`
	// RelayNativePromotionBlock: the schema/tool features that keep a request
	// on the relay, comma-separated (canonical: sorted), or "none".
	RelayNativePromotionBlock string `yaml:"-" json:"relay_native_promotion_block"`
	// RelayNoCatalogTransport: what a run_officejs call means in a request
	// with no client tool it could stand for (no_catalog_transport.go):
	// error (the default, h22: decode it as a relay call, which fails) or
	// continue (answer it upstream as unavailable and stream the model's
	// continuation).
	RelayNoCatalogTransport string `yaml:"-" json:"relay_no_catalog_transport"`
	// PublicBaseURL is where Basis Points can reach this plugin's resource
	// routes from the public internet, e.g. https://api.example.com/bp-img.
	// Inline images are relayed through it; leaving it empty disables image
	// support and the request fails with an explicit reason instead of the
	// upstream's opaque 422.
	PublicBaseURL string `yaml:"public_base_url" json:"public_base_url"`
	// MaxConcurrencyPerCredential caps how many requests one credential serves
	// at once. Zero means no cap.
	MaxConcurrencyPerCredential int `yaml:"max_concurrency_per_credential" json:"max_concurrency_per_credential"`
	// CredentialConcurrency overrides that cap for individual credentials,
	// keyed by account email; a credential file name is accepted too.
	CredentialConcurrency map[string]int `yaml:"credential_concurrency" json:"credential_concurrency"`
	// CompactThreshold is the context size, in tokens, at which the upstream
	// compacts a conversation server-side when the client sends no
	// context_management of its own.
	//
	// It has to stay above the client's own auto-compaction point. Codex
	// compacts at 90% of its 272k window (about 245k); an upstream in-place
	// compaction that fires first dropped the Responses-Lite additional_tools
	// from what the model sees in roughly a third of cases (2026-09-29), leaving
	// Codex Desktop with only the Excel tools. The upstream accepts contexts far
	// larger than 200k (up to ~925k observed, window ~1M), so this is only a backstop for clients
	// that never compact.
	CompactThreshold int `yaml:"compact_threshold" json:"compact_threshold"`
	// QuotaSampleSeconds is how often one credential's ChatGPT quota may be
	// read while it is serving traffic. This is the only upstream traffic the
	// cost estimate adds, so it is paced per credential and jittered rather
	// than swept. Zero disables the estimate entirely.
	QuotaSampleSeconds int `yaml:"quota_sample_seconds" json:"quota_sample_seconds"`
	// DowngradeGuardEnabled lets the operator stop the model-swap guard from
	// disabling credentials, from the panel, without stopping the guard: it
	// keeps watching and keeps saying what it sees. The flag lives here rather
	// than in the guard's own file because the panel can only write through
	// the authenticated management API, and a plugin setting written there
	// applies live. Absent means enabled, so a configuration that predates
	// this field leaves the guard armed.
	DowngradeGuardEnabled bool `yaml:"downgrade_guard_enabled" json:"downgrade_guard_enabled"`
	// HideAccountName appends accountNameGuard to the prologue, so the model
	// stops addressing callers by the profile name of the credential that
	// served them. On unless config.yaml says false; it applies live.
	HideAccountName bool `yaml:"hide_account_name" json:"hide_account_name"`
	// WebSearchGuard: off (default) | retry | always; see websearch_guard.go.
	WebSearchGuard string `yaml:"web_search_guard" json:"web_search_guard"`
	// WebSearchGuardTTLSeconds: how long a conversation stays guarded after a
	// turn failed following a web search (retry mode). Default 900.
	WebSearchGuardTTLSeconds int `yaml:"web_search_guard_ttl_seconds" json:"web_search_guard_ttl_seconds"`
	// WebSearchFailover (h27): hold a turn that opens with reasoning or a web
	// search, and fail it with zero output when the search then fails upstream,
	// so the main site replays it on another account. See websearch_failover.go.
	WebSearchFailover bool `yaml:"web_search_failover" json:"web_search_failover"`
	// WebSearchFailoverScope (h27c): after_failure (default) | all.
	WebSearchFailoverScope string `yaml:"web_search_failover_scope" json:"web_search_failover_scope"`
	// ModelPrices overrides list prices per model, in US dollars per million
	// tokens. Models left out use the built-in table.
	ModelPrices map[string]modelPriceConfig `yaml:"model_prices" json:"model_prices"`
	// ClientIdentity decides how much of the real add-in's client identity this
	// relay reproduces. Everything in it defaults to off.
	ClientIdentity ClientIdentityConfig `yaml:"client_identity" json:"client_identity"`
}

// ClientIdentityConfig is the whole identity surface, and every field is
// inert until an operator sets it.
type ClientIdentityConfig struct {
	// Mode is off, hygiene or thread. The modes earlier drafts called device
	// and session are gone: a capture of the real add-in showed its metadata
	// has exactly four keys and that neither an installation nor a session id
	// is among them, so those rungs would have added a field only this pool
	// sends.
	Mode string `yaml:"mode" json:"mode"`
	// CredentialModes overrides Mode for individual credentials, keyed the way
	// CredentialConcurrency is: account email first, credential file name
	// accepted too.
	CredentialModes map[string]string `yaml:"credential_modes" json:"credential_modes"`
	// Pepper keys the cache-key rewrite. It is one value for the whole pool and
	// it must survive a reconfigure: regenerating it rewrites every seat's
	// cache key at once, which is one pool-wide cold window for no gain.
	Pepper string `yaml:"pepper" json:"pepper"`
	// AcceptLanguage is sent verbatim. The real add-in sends the user's Office
	// locale; one value for the pool is the closest a relay can get.
	AcceptLanguage string `yaml:"accept_language" json:"accept_language"`
	// SendBrowserHeaders adds the browser-* and priority headers the real
	// client sends on every call, and the auth-mode header this relay used to
	// send and removed by mistake.
	SendBrowserHeaders bool `yaml:"send_browser_headers" json:"send_browser_headers"`
	// SendSentryTrace adds the Sentry tracing pair. The public key and org id
	// belong to OpenAI's own project and are identical for every real seat.
	SendSentryTrace bool `yaml:"send_sentry_trace" json:"send_sentry_trace"`
	// KeepCookies gives each credential its own cookie jar. The real client
	// carries 637 bytes of them; roughly 380 of those are what Cloudflare hands
	// out unprompted, and the rest belong to a browser sign-in this relay's
	// device-token credentials cannot perform.
	KeepCookies bool `yaml:"keep_cookies" json:"keep_cookies"`
	// PlainPrologue rewrites the developer prologue so it no longer announces
	// that the request is relayed. It costs one cold cache window, and
	// reverting costs a second one.
	PlainPrologue bool `yaml:"plain_prologue" json:"plain_prologue"`
	// ClientShapes normalises input items to the field sets the capture shows:
	// no id on user and developer messages, annotations and logprobs on
	// assistant text parts, reasoning items keeping their id and content. It
	// rewrites history bytes once, so it costs one cold cache window.
	ClientShapes bool `yaml:"client_shapes" json:"client_shapes"`
	// UAProfile is chrome133 (the default) or edge154. The capture says the
	// real client is Edge WebView2 154, but the handshake CPA performs is
	// Chrome 133 and uTLS has no Edge preset to move it to, so selecting
	// edge154 trades a set of wrong headers for a wrong TLS fingerprint. Leave
	// it alone until the handshake can follow.
	UAProfile string `yaml:"ua_profile" json:"ua_profile"`
}

// DefaultCompactThreshold is used when neither the client nor the operator
// sets a compaction threshold.
const DefaultCompactThreshold = 700000

func defaultConfig() Config {
	return Config{
		DataDir:          "plugins/oai-basispoints-data",
		ResponsesURL:     DefaultResponsesURL,
		UpstreamModel:    DefaultUpstreamModel,
		Models:           append([]string(nil), bpsModels...),
		TimeoutSeconds:   300,
		MaxResponseBytes: 64 << 20,
		AuthMode:         "chatgpt",
		// h25 relay switches: every default is h22's behaviour.
		RelayProtocol:           relayProtocolLegacy,
		RelayToolContracts:      relayToolContractsSummary,
		RelayToolDelivery:       relayToolDeliveryImmediate,
		ToolPolicyChecks:        toolPolicyChecksLog,
		HistoryConflict:         historyConflictReplay,
		RelayIncomplete:         relayIncompleteError,
		RelayHideEncryptedTools: true,
		CoerceScalarStrings:     true,
		RetryableErrorHint:      true,
		SchemaShadow:            false,
		// A1 native promotion: off, so the default is h25.
		RelayNativePromotion:        a1ModeOff,
		RelayNativePromotionHistory: a1HistoryH1,
		RelayNativePromotionBlock:   a1DefaultBlock,
		// a1wide2: error is h22 (decoded as a relay call, which fails);
		// production turns continue on with one line in config.yaml.
		RelayNoCatalogTransport: noCatalogError,
		// Five at once per account keeps a single ChatGPT seat well clear of
		// the upstream's own rate limit.
		MaxConcurrencyPerCredential: 5,
		CompactThreshold:            DefaultCompactThreshold,
		// Roughly a thousand readings a week across ten credentials, spread out
		// by jitter: enough to catch a quota window moving, far too little to
		// look like polling.
		QuotaSampleSeconds:       900,
		DowngradeGuardEnabled:    true,
		HideAccountName:          true,
		WebSearchGuard:           webSearchGuardOff,
		WebSearchGuardTTLSeconds: defaultWebSearchGuardTTLSeconds,
		ClientIdentity: ClientIdentityConfig{
			// Off, and every sub-switch with it. The code ships inert and an
			// operator turns it on one credential at a time.
			Mode: "off",
			// Sent only from hygiene upwards, so this default is inert too. One
			// value for the pool is the closest a relay gets to the real
			// client's own Office locale.
			AcceptLanguage: "en-US,en;q=0.9",
			UAProfile:      "chrome133",
		},
	}
}

func (c *Config) normalize() error {
	if c == nil {
		return fail(400, "invalid_config", "configuration is missing")
	}
	// Never an error: an unknown relay switch value falls back to its default.
	// configure() collects the warnings this returns before calling normalize.
	_ = c.normalizeRelaySwitches()
	// An unreadable identity mode is refused rather than quietly treated as
	// off: a typo in the pool-wide switch would otherwise look like the feature
	// simply not working, with nothing anywhere saying why.
	if _, ok := parseIdentityMode(c.ClientIdentity.Mode); !ok {
		return fail(400, "invalid_config",
			"client_identity.mode must be off, hygiene or thread")
	}
	for key, mode := range c.ClientIdentity.CredentialModes {
		if _, ok := parseIdentityMode(mode); !ok {
			return fail(400, "invalid_config",
				"client_identity.credential_modes["+key+"] must be off, hygiene or thread")
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.ClientIdentity.UAProfile)) {
	case "", "chrome133", "edge154":
	default:
		return fail(400, "invalid_config", "client_identity.ua_profile must be chrome133 or edge154")
	}
	c.ResponsesURL = strings.TrimSpace(c.ResponsesURL)
	if c.ResponsesURL == "" {
		c.ResponsesURL = DefaultResponsesURL
	}
	u, err := url.Parse(c.ResponsesURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fail(400, "invalid_config", "responses_url must be an absolute HTTPS URL")
	}
	// This URL receives a ChatGPT OAuth bearer token on every request, so it is
	// not a free-form endpoint: plaintext http or an arbitrary host would leak
	// the credential. Restrict it to https on a known Basis Points host.
	if u.Scheme != "https" {
		return fail(400, "invalid_config", "responses_url must use https")
	}
	if !isAllowedUpstreamHost(u.Hostname()) {
		return fail(400, "invalid_config", "responses_url host is not an allowed Basis Points endpoint")
	}
	c.UpstreamModel = strings.TrimSpace(c.UpstreamModel)
	if c.UpstreamModel == "" {
		c.UpstreamModel = DefaultUpstreamModel
	}
	c.AuthMode = strings.TrimSpace(c.AuthMode)
	if c.AuthMode == "" {
		c.AuthMode = "chatgpt"
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 1800 {
		return fail(400, "invalid_config", "timeout_seconds must be between 10 and 1800")
	}
	if c.MaxResponseBytes < 64<<10 || c.MaxResponseBytes > 128<<20 {
		return fail(400, "invalid_config", "max_response_bytes must be between 64 KiB and 128 MiB")
	}
	if c.CompactThreshold == 0 {
		c.CompactThreshold = DefaultCompactThreshold
	}
	if c.CompactThreshold < 100000 || c.CompactThreshold > 2000000 {
		return fail(400, "invalid_config", "compact_threshold must be between 100000 and 2000000")
	}
	if err := normalizeWebSearchGuard(c); err != nil {
		return err
	}
	if err := normalizeWebSearchFailover(c); err != nil {
		return err
	}
	seen := map[string]bool{}
	models := make([]string, 0, len(c.Models))
	for _, model := range c.Models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	if len(models) == 0 {
		models = append([]string(nil), bpsModels...)
	}
	c.Models = models
	if c.MaxConcurrencyPerCredential < 0 {
		c.MaxConcurrencyPerCredential = 0
	}
	if c.MaxConcurrencyPerCredential > maxConcurrencyLimit {
		c.MaxConcurrencyPerCredential = maxConcurrencyLimit
	}
	if len(c.CredentialConcurrency) > 0 {
		// Keys are matched case-insensitively against emails and file names.
		overrides := make(map[string]int, len(c.CredentialConcurrency))
		for key, limit := range c.CredentialConcurrency {
			key = strings.ToLower(strings.TrimSpace(key))
			if key == "" {
				continue
			}
			if limit < 0 {
				limit = 0
			}
			if limit > maxConcurrencyLimit {
				limit = maxConcurrencyLimit
			}
			overrides[key] = limit
		}
		c.CredentialConcurrency = overrides
	}
	return nil
}

func (c Config) clone() Config {
	c.Models = append([]string(nil), c.Models...)
	if c.CredentialConcurrency != nil {
		overrides := make(map[string]int, len(c.CredentialConcurrency))
		for key, limit := range c.CredentialConcurrency {
			overrides[key] = limit
		}
		c.CredentialConcurrency = overrides
	}
	if c.ClientIdentity.CredentialModes != nil {
		modes := make(map[string]string, len(c.ClientIdentity.CredentialModes))
		for key, mode := range c.ClientIdentity.CredentialModes {
			modes[key] = mode
		}
		c.ClientIdentity.CredentialModes = modes
	}
	return c
}

// identityMode returns the mode for one credential, resolved the same way
// concurrencyLimit resolves its override: account email first, then the
// credential file name with and without its suffix.
//
// An unreadable mode falls back to off rather than to the pool default. A
// typo in one credential's entry should narrow that credential's behaviour,
// never widen it.
func (c Config) identityMode(email, fileName string) identityMode {
	for _, key := range []string{email, fileName, strings.TrimSuffix(fileName, ".json")} {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		if text, ok := c.ClientIdentity.CredentialModes[key]; ok {
			mode, valid := parseIdentityMode(text)
			if !valid {
				return identityOff
			}
			return mode
		}
	}
	mode, valid := parseIdentityMode(c.ClientIdentity.Mode)
	if !valid {
		return identityOff
	}
	return mode
}

// concurrencyLimit returns the cap for a credential and whether it came from a
// per-credential override rather than the default. A cap of zero means none.
func (c Config) concurrencyLimit(email, fileName string) (int, bool) {
	for _, key := range []string{email, fileName, strings.TrimSuffix(fileName, ".json")} {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		if limit, ok := c.CredentialConcurrency[key]; ok {
			return limit, true
		}
	}
	return c.MaxConcurrencyPerCredential, false
}

// concurrencyOverrideKey is the credential_concurrency key the concurrency page
// edits for a credential: the key an existing override was found under, so an
// edit replaces it rather than adding a second one, or else the account email.
func (c Config) concurrencyOverrideKey(email, fileName string) string {
	for _, key := range []string{email, fileName, strings.TrimSuffix(fileName, ".json")} {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		if _, ok := c.CredentialConcurrency[key]; ok {
			return key
		}
	}
	if key := strings.ToLower(strings.TrimSpace(email)); key != "" {
		return key
	}
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(fileName, ".json")))
}

// normalizeEffort maps a requested reasoning effort onto what Basis Points
// accepts: low, medium, high, xhigh. Anything above that range is clamped to
// xhigh rather than dropped to the default.
//
// 0.1.1 sent "medium" for max and ultra, so a caller asking for the deepest
// reasoning silently got the middle setting -- two steps below both what it
// asked for and what the upstream can actually do. Clamping keeps the request
// as close to its intent as the upstream allows. Unrecognized values still fall
// back to medium, because they express no ordering to clamp against.
func normalizeEffort(value any) string {
	s, _ := value.(string)
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "x-high", "extra-high", "extra_high":
		s = "xhigh"
	case "max", "ultra", "maximum", "highest":
		// Above the upstream ceiling: give the most reasoning available.
		return "xhigh"
	case "minimal", "min", "minimum", "lowest", "none", "off":
		// Below the upstream floor. Basis Points accepts exactly low, medium,
		// high and xhigh -- verified against a live account, where both
		// "minimal" and "none" come back 422 -- so a caller asking for less
		// thinking than low has to be answered with low.
		//
		// Falling through to the medium default instead, as 0.1.x did, handed
		// them MORE reasoning than they asked for: Codex offers a "minimal"
		// setting on the gpt-5 family, and choosing it made answers slower and
		// spent more of the same weekly quota, with nothing said about it.
		return "low"
	}
	if _, ok := supportedReasoningEfforts[s]; ok {
		return s
	}
	return "medium"
}

func rawObject(raw []byte) (map[string]any, error) {
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fail(400, "invalid_request", "request body must be a JSON object")
	}
	return object, nil
}

func jsonBytes(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

// responsesBodyKeyOrder and metadataKeyOrder are the orders the real add-in
// writes, identical across all three captured requests.
//
// Go sorts map keys when it marshals, so every request this relay sends is in
// perfect lexicographic order -- a property no hand-written client object has,
// and one that holds on 100% of our traffic. prompt_cache_key has no place in
// the capture because the real client never sends it; it goes last, which is a
// choice rather than an observation.
var (
	responsesBodyKeyOrder = []string{
		"model", "input", "stream", "store", "context_management",
		"metadata", "reasoning_effort", "model_selection", "prompt_cache_key",
	}
	metadataKeyOrder = []string{"task_id", "turn_id", "bps_tools_version_id", "agent_iteration"}
)

// responsesBodyBytes serialises the outgoing body the way a browser client
// would: in the client's own key order, and without escaping <, > and &.
//
// JSON.stringify never escapes those three, and Go's encoder always does, so
// any body carrying them -- about three quarters of ours, since agent
// histories are full of markup and shell redirection -- arrives with <
// sequences no JavaScript client can produce.
//
// This applies to the outer body only. jsonBytes keeps its escaping, and must:
// it also serialises the strings that go inside "code" and "arguments", where
// the escaping is content the model reads, and the fingerprints that derive
// task_id and turn_id, where changing the bytes would re-root both.
func responsesBodyBytes(body map[string]any) []byte {
	var buffer bytes.Buffer
	writeOrderedObject(&buffer, body, responsesBodyKeyOrder, func(key string, value any, into *bytes.Buffer) bool {
		if key != "metadata" {
			return false
		}
		metadata, ok := value.(map[string]any)
		if !ok {
			return false
		}
		writeOrderedObject(into, metadata, metadataKeyOrder, nil)
		return true
	})
	return buffer.Bytes()
}

// writeOrderedObject writes object with the listed keys first, in that order,
// and anything left over after them in sorted order so the result stays
// deterministic for keys the order does not name.
func writeOrderedObject(into *bytes.Buffer, object map[string]any, order []string,
	special func(string, any, *bytes.Buffer) bool) {
	listed := make(map[string]bool, len(order))
	for _, key := range order {
		listed[key] = true
	}
	rest := make([]string, 0, len(object))
	for key := range object {
		if !listed[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	keys := make([]string, 0, len(object))
	for _, key := range order {
		if _, exists := object[key]; exists {
			keys = append(keys, key)
		}
	}
	keys = append(keys, rest...)

	into.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			into.WriteByte(',')
		}
		into.Write(unescapedJSON(key))
		into.WriteByte(':')
		if special != nil && special(key, object[key], into) {
			continue
		}
		into.Write(unescapedJSON(object[key]))
	}
	into.WriteByte('}')
}

func unescapedJSON(value any) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return jsonBytes(value)
	}
	// Encode appends a newline that no other caller wants.
	return bytes.TrimRight(buffer.Bytes(), "\n")
}

func stringValue(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

func numberValue(value any) int64 {
	switch n := value.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

func errorMessage(body []byte) string {
	var object map[string]any
	if json.Unmarshal(body, &object) == nil {
		if nested, ok := object["error"].(map[string]any); ok {
			if message := stringValue(nested["message"]); message != "" {
				return message
			}
		}
		if message := stringValue(object["message"]); message != "" {
			return message
		}
		if message := stringValue(object["error"]); message != "" {
			return message
		}
	}
	if len(body) > 0 {
		message := strings.TrimSpace(string(body))
		if len(message) > 500 {
			message = message[:500]
		}
		return message
	}
	return "Basis Points upstream request failed"
}

// statusError converts an upstream failure into a plugin error.
//
// The message deliberately keeps the upstream's JSON error object intact
// instead of flattening it to its message string. CPA decides whether a failure
// belongs to the request or to the credential by parsing this message as JSON
// and reading error.type / error.code (clienterror.hasRequestFaultBody). Losing
// the envelope hid "type": "invalid_request_error" from that classifier, so a
// request the caller malformed -- an unsupported image part, for instance --
// was charged to the credential and took it out of service until restart.
// Passing the object through also gives the caller the upstream's real reason
// rather than a summary of it.
func statusError(status int, body []byte) error {
	if status < 400 {
		status = 502
	}
	if envelope := errorEnvelope(body); envelope != "" {
		return fail(status, "upstream_error", envelope)
	}
	return fail(status, "upstream_error", errorMessage(body))
}

// errorEnvelope returns the upstream body verbatim when it is a JSON object
// carrying an error description, and "" when there is nothing to preserve.
func errorEnvelope(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return ""
	}
	var object map[string]any
	if json.Unmarshal([]byte(trimmed), &object) != nil {
		return ""
	}
	if _, ok := object["error"]; !ok {
		return ""
	}
	if len(trimmed) > 2000 {
		return ""
	}
	return trimmed
}

func timeoutError(cfg Config) error {
	return fail(504, "upstream_timeout", fmt.Sprintf("Basis Points request timed out after %d seconds", cfg.TimeoutSeconds))
}

func isResponseModel(model string, cfg Config) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, candidate := range cfg.Models {
		if model == candidate {
			return true
		}
	}
	return false
}
