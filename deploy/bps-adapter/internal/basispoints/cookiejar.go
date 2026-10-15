package basispoints

import (
	"net/http"
	"sort"
	"strings"
	"sync"
)

// A browser keeps the cookies a site sets and sends them back. This relay never
// has, so every request it makes looks like a first visit: the real add-in
// carries about 637 bytes of them on every call, and we carry none.
//
// Roughly 380 of those bytes are Cloudflare's own (__cf_bm, __cflb, _cfuvid),
// handed to any client that asks -- an unauthenticated GET to the API returns
// all three. Those we can hold. The rest belong to a browser sign-in that this
// relay's credentials cannot perform: they are device tokens for the CLI OAuth
// app, with no path to a chatgpt.com browser session. So this closes most of
// the gap and not all of it, and turns "a brand new client, seventy thousand
// times a day" into "a browser that has not signed in".
//
// One jar per credential, never shared. A jar shared across seats would hand
// several credentials the same _cfuvid, which is a stabler join than the shared
// TLS connection that was removed on 2026-09-30 -- that one was a timing
// coincidence, this would be an identifier.
type cookieJar struct {
	mu   sync.Mutex
	jars map[string]map[string]string
}

func newCookieJar() *cookieJar {
	return &cookieJar{jars: map[string]map[string]string{}}
}

// remember stores the cookies a response set for one seat.
func (j *cookieJar) remember(seat string, headers http.Header) {
	if j == nil || seat == "" || len(headers) == 0 {
		return
	}
	raw := headers.Values("Set-Cookie")
	if len(raw) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	jar := j.jars[seat]
	if jar == nil {
		jar = map[string]string{}
		j.jars[seat] = jar
	}
	for _, line := range raw {
		name, value, ok := parseSetCookie(line)
		if !ok {
			continue
		}
		if value == "" {
			delete(jar, name)
			continue
		}
		jar[name] = value
	}
}

// header returns the Cookie header for one seat, or "" if it has no cookies.
func (j *cookieJar) header(seat string) string {
	if j == nil || seat == "" {
		return ""
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	jar := j.jars[seat]
	if len(jar) == 0 {
		return ""
	}
	names := make([]string, 0, len(jar))
	for name := range jar {
		names = append(names, name)
	}
	// A browser sends cookies in a stable order; sorting is as close as a map
	// gets and keeps the header from reshuffling between requests.
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+"="+jar[name])
	}
	return strings.Join(pairs, "; ")
}

// forget drops one seat's jar, used when a credential is disabled or removed so
// a later credential with the same file name cannot inherit it.
func (j *cookieJar) forget(seat string) {
	if j == nil || seat == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.jars, seat)
}

// parseSetCookie reads the name and value off a Set-Cookie line, ignoring the
// attributes. Expiry is not tracked: these are session-scoped anti-bot cookies
// that the server refreshes on its own, and holding one past its life costs a
// replacement header, not a failure.
func parseSetCookie(line string) (string, string, bool) {
	pair := line
	if semicolon := strings.IndexByte(pair, ';'); semicolon >= 0 {
		pair = pair[:semicolon]
	}
	equals := strings.IndexByte(pair, '=')
	if equals <= 0 {
		return "", "", false
	}
	name := strings.TrimSpace(pair[:equals])
	value := strings.TrimSpace(pair[equals+1:])
	if name == "" {
		return "", "", false
	}
	return name, value, true
}
