package basispoints

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// This file holds the client identity the relay presents upstream. Everything
// here is derived: no clock reads the caller can see, no state that outlives a
// turn, and nothing that depends on which credential served the previous turn.
//
// The shapes are copied from a capture of the real ChatGPT-for-Excel add-in
// (2026-10-01, WebView2 remote debugging). Where the capture shows a value, the
// value is used verbatim; where it shows only a shape, the shape is followed
// and the comment says so. Nothing here invents a field the real client does
// not send -- the capture settled that its metadata has exactly four keys, and
// none of them is a device or session id.

type identityMode int

const (
	// identityOff reproduces the pre-identity body byte for byte. It is the
	// deployed default and the baseline every golden test compares against.
	identityOff identityMode = iota
	// identityHygiene only removes and corrects: no field gains a value the
	// request did not already carry in some form.
	identityHygiene
	// identityThread additionally collapses task_id to one constant per
	// credential, so a seat presents as one never-closed conversation.
	identityThread
)

func parseIdentityMode(text string) (identityMode, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "", "off":
		return identityOff, true
	case "hygiene":
		return identityHygiene, true
	case "thread":
		return identityThread, true
	default:
		return identityOff, false
	}
}

func (m identityMode) String() string {
	switch m {
	case identityHygiene:
		return "hygiene"
	case identityThread:
		return "thread"
	default:
		return "off"
	}
}

// persona is one seat: one credential, one machine, one conversation.
type persona struct {
	// seed is stable for the lifetime of a credential and derived from the
	// bearer token's identity, never from the account. Four or five seats share
	// an account_id in this pool, and seats that share a persona are seats that
	// can be joined back together -- the same mistake the per-credential
	// connection work removed at the TLS layer.
	seed string
	// pepper is one constant for the whole pool, used only for the cache key.
	// It is deliberately not per-seat: the cache key has to stay stable while
	// CPA moves a conversation between credentials mid-turn, which it does for
	// 30-67% of adjacent turns.
	pepper string
	// accountUserID is the upstream's own per-user id, read from the access
	// token rather than synthesised.
	accountUserID string
	// subscriptionStart floors anything this relay claims about the account's
	// history. See seatBirthMs.
	subscriptionStart time.Time
}

func (p persona) empty() bool { return p.seed == "" }

// personaKey derives the seat key, mirroring concurrencyKey's fallbacks except
// for its last one.
//
// concurrencyKey ends with "account:" + AccountID, which is correct for a
// concurrency gate and wrong here: this pool runs 20 seats across 5 accounts,
// so that branch would hand four or five credentials the same machine and the
// same conversation. A seat with no usable key gets no persona at all and the
// request falls back to off, which is a visible no-op rather than a silent
// merge.
func personaKey(request ExecutorRequest, c credential) (string, bool) {
	if key := strings.TrimSpace(request.AuthID); key != "" {
		return key, true
	}
	if name := strings.TrimSpace(stringValue(request.AuthMetadata["file_name"])); name != "" {
		return credentialID(name), true
	}
	return "", false
}

// personaFor resolves the seat and the mode for one request.
//
// It is a pure function of the configuration, the credential and the request,
// so the body path and the header path can each call it and be certain they
// agree without threading the result between them.
func (s *Service) personaFor(request ExecutorRequest, c credential) (persona, identityMode) {
	cfg := s.config()
	mode := cfg.identityMode(c.Email, stringValue(request.AuthMetadata["file_name"]))
	// Experiment identity arm (bpsexp build only): an arm may raise this
	// request to hygiene/thread. A no-op in the production build.
	mode = expArmMode(request, mode)
	if mode == identityOff {
		return persona{}, identityOff
	}
	seed, ok := personaKey(request, c)
	if !ok {
		return persona{}, identityOff
	}
	who := persona{
		seed:              seed,
		pepper:            cfg.ClientIdentity.Pepper,
		accountUserID:     c.AccountUserID,
		subscriptionStart: c.SubscriptionStart,
	}
	s.rememberPersona(who)
	return who, mode
}

// seatUUIDBits derives the two fields the real client seeds once per session:
// rand_a (12 bits) and the 14 bits that follow the variant.
//
// These were hard-coded to the single captured session's values (0xbb8, 0x2ec2)
// on the reasoning that two ids sharing them made them a client constant, and
// that one shared value could not be wrong in the direction that mattered. A
// second capture settled it and the reasoning was backwards: a fresh Excel
// session produced 0x881 and 0x999a, so the client seeds them per session and
// every real installation carries a different pair.
//
// Sharing one pair across the pool was therefore not the safe choice, it was
// the worst one. Twenty-six bits that are supposed to be random, identical
// across every credential and present in two fields of every single request, is
// a cross-account join key -- two real machines would collide on it with
// probability 2^-26. This file exists to remove such a key, and it had added
// one.
//
// The rule this encodes: a value shared pool-wide is safe only when it is also
// common in the wider population (a browser version, a locale, OpenAI's own
// Sentry key). A value that is supposed to be drawn at random is never safe to
// share, because sharing is itself the evidence.
//
// One pair per seat, constant for that seat's lifetime, matches both the
// client's behaviour and this design's claim that a seat is one never-closed
// session.
func seatUUIDBits(seed string) (uint16, uint16) {
	digest := identityDigest("bps-persona/v1/uuid-bits", seed)
	randA := binary.BigEndian.Uint16(digest[0:2]) & 0x0fff
	variantLow := binary.BigEndian.Uint16(digest[2:4]) & 0x3fff
	return randA, variantLow
}

// uuidV7 lays out a version 7 UUID: 48 bits of Unix milliseconds, then the
// version and rand_a, then the variant and the seat's own bits, then 48 bits
// from tail.
func uuidV7(unixMilli int64, randA, variantLow uint16, tail []byte) string {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[0:8], uint64(unixMilli)<<16)
	raw[6] = 0x70 | byte((randA>>8)&0x0f)
	raw[7] = byte(randA & 0xff)
	raw[8] = 0x80 | byte((variantLow>>8)&0x3f)
	raw[9] = byte(variantLow & 0xff)
	for index := 0; index < 6; index++ {
		if index < len(tail) {
			raw[10+index] = tail[index]
		}
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// uuidV7For is uuidV7 with the seat's bits looked up from its seed.
func uuidV7For(who persona, unixMilli int64, tail []byte) string {
	randA, variantLow := seatUUIDBits(who.seed)
	return uuidV7(unixMilli, randA, variantLow, tail)
}

func identityDigest(parts ...string) []byte {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hash.Sum(nil)
}

// When a seat's one never-closed conversation began.
//
// The first version drew this from a fixed window 30 to 400 days back and
// referred to nothing the upstream knows. That produced conversations older
// than the subscription that pays for them -- one seat was given a birthday
// five months before its ChatGPT subscription started, which is not a thing
// that can happen to a real user.
//
// The floor is now the subscription's own start date, which the upstream itself
// issued in the credential's id_token (chatgpt_subscription_active_start). It
// is the one date in a credential that is both stable across token refreshes --
// it describes the subscription, not the session -- and already known to the
// other side, so a claim measured from it cannot contradict their records.
//
// The seat's offset into the first two weeks of that subscription comes from
// its seed, which keeps seats imported in one batch from sharing a birthday
// even though they often share a subscription start.
const (
	seatBirthWindowMs int64 = 14 * 24 * 60 * 60 * 1000
	// A conversation is never claimed to have started in the last hour: the
	// point of the claim is that this session has been open a while.
	seatBirthMinAgeMs int64 = 60 * 60 * 1000
	// Used only when a credential carries no subscription claim at all. It
	// keeps the old fixed anchor (2026-06-01, reaching 300 days back) so that
	// such a seat still gets one stable birthday, but nothing anchors it to the
	// account, so it can still claim more history than the account has. Every
	// credential in the pool today carries the claim, which is what keeps this
	// path unused.
	seatBirthFallbackLatestMs int64 = 1780272000000
	seatBirthFallbackSpreadMs int64 = 300 * 24 * 60 * 60 * 1000
)

func seatBirthMs(who persona) int64 {
	digest := identityDigest("bps-persona/v1/birth", who.seed)
	if who.subscriptionStart.IsZero() {
		offset := int64(binary.BigEndian.Uint64(digest[0:8]) % uint64(seatBirthFallbackSpreadMs))
		return seatBirthFallbackLatestMs - offset
	}
	offset := int64(binary.BigEndian.Uint64(digest[0:8]) % uint64(seatBirthWindowMs))
	birth := who.subscriptionStart.UnixMilli() + offset
	// A subscription that started days ago cannot already hold a conversation
	// from next week. The clamp only engages while a subscription is younger
	// than the window, and stops moving once it is older.
	if ceiling := time.Now().UnixMilli() - seatBirthMinAgeMs; birth > ceiling {
		birth = ceiling
	}
	return birth
}


// seatTaskID is the conversation a seat never closes.
func seatTaskID(who persona) string {
	return uuidV7For(who, seatBirthMs(who), identityDigest("bps-persona/v1/task", who.seed)[:6])
}

// turnIDCache keeps one turn id for the turns of one conversation.
//
// The real client mints a turn id when the user sends, and reuses it for every
// iteration of the tool loop that follows; the capture shows two consecutive
// requests sharing a turn_id while agent_iteration goes 1 then 2. Each of those
// iterations reaches this relay as a separate inbound request, so the id has to
// be remembered somewhere to come out the same.
//
// It is keyed by the conversation scope exactly as the native-call and
// namespace caches are, and for the same reason: a key that is only the turn
// fingerprint would be shared by every tenant whose turn happens to hash alike,
// which is the shape of the splicing fault those caches were scoped to fix. A
// miss costs one turn id that moves mid-turn, never a wrong one.
var turnIDCache = struct {
	sync.Mutex
	items map[string]turnIDEntry
	order []string
}{items: map[string]turnIDEntry{}}

type turnIDEntry struct {
	id       string
	storedAt time.Time
}

const (
	turnIDCacheEntries = 4096
	turnIDCacheTTL     = 30 * time.Minute
)

func turnIDFor(scope, turnFingerprint string, who persona) string {
	tail := identityDigest("bps-persona/v1/turn", who.seed, turnFingerprint)[:6]
	if scope == "" {
		// Nothing to remember it under, so the timestamp has to come from the
		// turn itself. It will not track arrival time; a turn id that moved
		// between the iterations of one turn would be worse.
		return uuidV7For(who, seatBirthMs(who), tail)
	}
	key := nativeCallCacheKey(scope, "turn\x00"+turnFingerprint)
	now := time.Now()
	turnIDCache.Lock()
	defer turnIDCache.Unlock()
	if entry, ok := turnIDCache.items[key]; ok && now.Sub(entry.storedAt) <= turnIDCacheTTL {
		return entry.id
	}
	id := uuidV7For(who, now.UnixMilli(), tail)
	if _, exists := turnIDCache.items[key]; !exists {
		turnIDCache.order = append(turnIDCache.order, key)
	}
	turnIDCache.items[key] = turnIDEntry{id: id, storedAt: now}
	for len(turnIDCache.order) > turnIDCacheEntries {
		oldest := turnIDCache.order[0]
		turnIDCache.order = turnIDCache.order[1:]
		delete(turnIDCache.items, oldest)
	}
	return id
}

// Sentry constants, verbatim from the capture. The DSN belongs to OpenAI's own
// project, so these two are the same for every real seat in the world and are
// the only part of this pair that is not derived.
const (
	sentryPublicKey = "d3e75214397bc4256a94b9b4a591c90b"
	sentryOrgID     = "33249"
)

// sentryTrace builds the tracing pair the real client sends on every call.
//
// The capture shows one trace id and one sample_rand across 126 requests of a
// single session, with a fresh span id on each -- so the trace is the session
// and the span is the request. The trace id here is derived from the seat, so a
// seat keeps one trace for as long as it exists, which is the same claim the
// rest of this file makes: one person, one never-closed session.
//
// Note what this does to OpenAI's own Sentry project: we never post an envelope
// to it, so these spans arrive with no client root. That is the honest cost of
// the header and the reason it is off by default.
func sentryTrace(who persona) (string, string) {
	traceID := hex.EncodeToString(identityDigest("bps-persona/v1/sentry-trace", who.seed)[:16])
	spanID := hex.EncodeToString(randomBytes(8))
	// sample_rand is a per-session draw, so it is derived from the seat and not
	// re-rolled: a value that moved every request would say the SDK had been
	// reinitialised every request.
	draw := float64(binary.BigEndian.Uint64(identityDigest("bps-persona/v1/sentry-rand", who.seed)[:8])>>11) /
		float64(uint64(1)<<53)
	baggage := strings.Join([]string{
		"sentry-environment=production",
		"sentry-public_key=" + sentryPublicKey,
		"sentry-trace_id=" + traceID,
		"sentry-org_id=" + sentryOrgID,
		"sentry-sampled=true",
		fmt.Sprintf("sentry-sample_rand=%.17g", draw),
		"sentry-sample_rate=1",
	}, ",")
	return traceID + "-" + spanID + "-1", baggage
}

// newPepper mints the pool's cache-key pepper once. It is written to
// settings.json and read back on every later configure, so this runs on a fresh
// install and never again -- which is the point: a pepper that changed would
// change every seat's cache key at the same moment.
func newPepper() string {
	return hex.EncodeToString(randomBytes(32))
}

func randomBytes(size int) []byte {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand does not fail in practice, and a span id that repeats is
		// still a well-formed span id, so there is nothing here worth failing
		// a request over.
		for index := range buffer {
			buffer[index] = byte(index)
		}
	}
	return buffer
}

// rewriteCacheKey makes the cache key opaque without making it unstable.
//
// The real client never sends prompt_cache_key at all, but production says we
// cannot simply drop it: conversations that reach the upstream without one lose
// 5 to 21 points of cache hit rate at the prompt sizes this pool runs. So the
// field stays and only its value changes, from the caller's own session UUID --
// whose first 48 bits are the millisecond it was created -- to something that
// carries nothing.
//
// The pepper is one value for the whole pool. A per-seat pepper would change
// the key in the middle of a conversation every time CPA moved it to another
// credential, which is the one thing a cache key must never do.
//
// The output is shaped like a UUID rather than carrying a prefix. The first
// version wrote "bp_" in front of the digest, which made every request this
// pool sends self-identifying in a single literal: the real client sends no
// prompt_cache_key at all, so nothing else on this endpoint could ever produce
// that string. A UUID is what the clients calling this relay already send, so
// it is the shape that says the least.
func (p persona) rewriteCacheKey(clientKey string) string {
	if clientKey == "" || p.pepper == "" {
		return clientKey
	}
	mac := hmac.New(sha256.New, []byte(p.pepper))
	_, _ = mac.Write([]byte(clientKey))
	digest := mac.Sum(nil)
	// Version 4 and the standard variant, so it is a well-formed UUID and not a
	// random hex string wearing dashes.
	digest[6] = (digest[6] & 0x0f) | 0x40
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

// applyConvergedIdentity writes the identity fields of one request.
//
// It only ever writes to output. The inbound source map is read after this by
// turnState and again by the whole response path, and both of them key caches
// that a changed source would corrupt.
func applyConvergedIdentity(output map[string]any, metadata map[string]any, mode identityMode,
	who persona, scope, conversation, turnFingerprint, iteration string) {
	if mode == identityOff || who.empty() {
		metadata["task_id"] = uuidV5("cpa-oai-basispoints/" + conversation)
		metadata["turn_id"] = uuidV5("cpa-oai-basispoints/" + conversation + "/turn/" + turnFingerprint)
		metadata["agent_iteration"] = iteration
		return
	}
	if mode >= identityThread {
		// One seat, one conversation, for as long as the seat exists.
		metadata["task_id"] = seatTaskID(who)
	} else {
		metadata["task_id"] = uuidV7For(who,
			seatBirthMs(who),
			identityDigest("bps-persona/v1/task", who.seed, conversation)[:6])
	}
	metadata["turn_id"] = turnIDFor(scope, turnFingerprint, who)
	metadata["agent_iteration"] = iteration
}


// rememberPersona and lastPersona let the management page show the identity a
// seat actually presented rather than one recomputed from its file name.
func (s *Service) rememberPersona(who persona) {
	if s == nil || who.empty() {
		return
	}
	s.personasMu.Lock()
	if s.personas == nil {
		s.personas = map[string]persona{}
	}
	s.personas[who.seed] = who
	s.personasMu.Unlock()
}

func (s *Service) lastPersona(seed string) (persona, bool) {
	if s == nil {
		return persona{}, false
	}
	s.personasMu.RLock()
	who, ok := s.personas[seed]
	s.personasMu.RUnlock()
	return who, ok
}
