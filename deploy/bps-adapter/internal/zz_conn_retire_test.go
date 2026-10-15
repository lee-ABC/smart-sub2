//go:build !go1.27 || http2legacy

package adapter

// Local patch G1 (fixes from the 2026-10-11 review): retiring a held connection
// sends no frame, a request-level failure does not cost the connection, the
// requests the peer provably never processed are sent once more, and dialling
// happens outside the credential's lock.
//
// The committed tests in zz_conn_reuse_test.go run against Go's own
// http2.Server, which accepts a client GOAWAY whose last_stream_id is an odd
// (client-initiated) stream ID. nghttp2 -- and so node's http2, nginx, Apache,
// envoy -- rejects it as a connection error ("GOAWAY: invalid last_stream_id")
// and tears the connection down. strictH2 is a frame-level server that applies
// that rule and records every GOAWAY the client sends.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	stdtls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type strictH2 struct {
	addr    string
	release chan struct{}

	mu             sync.Mutex
	accepted       int
	clientGoAways  []uint32
	protocolErrors int
	hits           map[string]int
	serverGoAway   func(lastStreamID uint32)
}

// startStrictH2 serves h2 in plaintext (tests inject the connection) or, with
// useTLS, behind a TLS handshake that negotiates h2 through ALPN, so the round
// tripper dials it for real.
func startStrictH2(t *testing.T, maxStreams uint32, useTLS bool) *strictH2 {
	t.Helper()
	var ln net.Listener
	var err error
	if useTLS {
		cert, pool := strictTestCertificate(t)
		trustForTest(t, pool)
		ln, err = stdtls.Listen("tcp", "127.0.0.1:0", &stdtls.Config{Certificates: []stdtls.Certificate{cert}, NextProtos: []string{"h2"}})
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &strictH2{addr: ln.Addr().String(), release: make(chan struct{}), hits: make(map[string]int)}
	go func() {
		for {
			c, errAccept := ln.Accept()
			if errAccept != nil {
				return
			}
			s.mu.Lock()
			s.accepted++
			s.mu.Unlock()
			go s.serve(c, maxStreams)
		}
	}()
	return s
}

func (s *strictH2) hit(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits[path]++
	return s.hits[path]
}

func (s *strictH2) hitsFor(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func (s *strictH2) connectionsAccepted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

func (s *strictH2) serve(c net.Conn, maxStreams uint32) {
	defer func() { _ = c.Close() }()
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(c, preface); err != nil || string(preface) != http2.ClientPreface {
		return // e.g. a TLS ClientHello from a replacement dial: not for this plaintext server
	}
	fr := http2.NewFramer(c, c)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	var wmu sync.Mutex
	write := func(fn func() error) { wmu.Lock(); _ = fn(); wmu.Unlock() }
	var settings []http2.Setting
	if maxStreams > 0 {
		settings = append(settings, http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: maxStreams})
	}
	write(func() error { return fr.WriteSettings(settings...) })
	var lastClientStream atomic.Uint32
	s.mu.Lock()
	s.serverGoAway = func(last uint32) {
		write(func() error { return fr.WriteGoAway(last, http2.ErrCodeNo, nil) })
	}
	s.mu.Unlock()
	headers := func(status string) []byte {
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: status})
		_ = enc.WriteField(hpack.HeaderField{Name: "content-type", Value: "text/plain"})
		return buf.Bytes()
	}
	answer := func(id uint32) {
		hdr := headers("200")
		write(func() error {
			return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: hdr, EndHeaders: true})
		})
		write(func() error { return fr.WriteData(id, true, []byte("ok")) })
	}
	for {
		frame, err := fr.ReadFrame()
		if err != nil {
			return
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				write(func() error { return fr.WriteSettingsAck() })
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				data := f.Data
				write(func() error { return fr.WritePing(true, data) })
			}
		case *http2.MetaHeadersFrame:
			id := f.StreamID
			lastClientStream.Store(id)
			path := f.PseudoValue("path")
			n := s.hit(path)
			switch {
			case strings.HasPrefix(path, "/hold"):
				hdr := headers("200")
				write(func() error {
					return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: hdr, EndHeaders: true})
				})
				write(func() error { return fr.WriteData(id, false, []byte("first\n")) })
				go func() {
					<-s.release
					write(func() error { return fr.WriteData(id, true, []byte("last\n")) })
				}()
			case strings.HasPrefix(path, "/never"):
				// The upstream is still thinking; the client gives up first.
			case strings.HasPrefix(path, "/refuse-once"):
				if n == 1 {
					write(func() error { return fr.WriteRSTStream(id, http2.ErrCodeRefusedStream) })
				} else {
					answer(id)
				}
			case strings.HasPrefix(path, "/refuse"):
				write(func() error { return fr.WriteRSTStream(id, http2.ErrCodeRefusedStream) })
			case strings.HasPrefix(path, "/goaway-twice"):
				if n <= 2 {
					last := uint32(0) // the first stream of a fresh connection: nothing processed
					if id > 2 {
						last = id - 2
					}
					write(func() error { return fr.WriteGoAway(last, http2.ErrCodeNo, nil) })
				} else {
					answer(id)
				}
			case strings.HasPrefix(path, "/goaway-once"):
				if n == 1 {
					// Graceful GOAWAY that does not cover this stream: never processed.
					write(func() error { return fr.WriteGoAway(id-2, http2.ErrCodeNo, nil) })
				} else {
					answer(id)
				}
			case strings.HasPrefix(path, "/goaway-hold"):
				write(func() error { return fr.WriteGoAway(id-2, http2.ErrCodeNo, nil) })
			default:
				answer(id)
			}
		case *http2.GoAwayFrame:
			s.mu.Lock()
			s.clientGoAways = append(s.clientGoAways, f.LastStreamID)
			s.mu.Unlock()
			if f.LastStreamID%2 == 1 { // not a server-initiated stream: nghttp2 rejects it
				s.mu.Lock()
				s.protocolErrors++
				s.mu.Unlock()
				last := lastClientStream.Load()
				write(func() error {
					return fr.WriteGoAway(last, http2.ErrCodeProtocol, []byte("GOAWAY: invalid last_stream_id"))
				})
				return
			}
		}
	}
}

func (s *strictH2) inject(t *testing.T, rt *utlsRoundTripper) *http2.ClientConn {
	t.Helper()
	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	h2Conn, err := (&http2.Transport{}).NewClientConn(conn)
	if err != nil {
		t.Fatalf("new client conn: %v", err)
	}
	rt.mu.Lock()
	rt.conns[s.addr] = h2Conn
	rt.mu.Unlock()
	return h2Conn
}

// assertNoClientGoAway is the frame-level rule this file exists for.
func (s *strictH2) assertNoClientGoAway(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	goAways, protoErrs := append([]uint32(nil), s.clientGoAways...), s.protocolErrors
	s.mu.Unlock()
	if len(goAways) > 0 {
		t.Errorf("client sent GOAWAY last_stream_id=%v; an nghttp2 peer answers PROTOCOL_ERROR (%d time(s)) and drops the connection", goAways, protoErrs)
	}
}

func strictTestCertificate(t *testing.T) (stdtls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "utls-conn-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return stdtls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// trustForTest makes the Chrome handshake trust pool for the rest of the test.
func trustForTest(t *testing.T, pool *x509.CertPool) {
	t.Helper()
	previous := utlsRootCAs
	utlsRootCAs = pool
	t.Cleanup(func() { utlsRootCAs = previous })
}

func strictReq(t *testing.T, ctx context.Context, addr, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func startHold(t *testing.T, rt *utlsRoundTripper, addr string) *http.Response {
	t.Helper()
	resp, err := rt.RoundTrip(strictReq(t, context.Background(), addr, "/hold"))
	if err != nil {
		t.Fatalf("long stream: %v", err)
	}
	first := make([]byte, len("first\n"))
	if _, err = io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("long stream first chunk: %v", err)
	}
	return resp
}

func finishHold(resp *http.Response) (string, error) {
	defer func() { _ = resp.Body.Close() }()
	rest, err := io.ReadAll(resp.Body)
	return string(rest), err
}

func roundTripOK(t *testing.T, rt *utlsRoundTripper, req *http.Request) {
	t.Helper()
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip %s: %v", req.URL.Path, err)
	}
	body, errRead := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if errRead != nil || string(body) != "ok" {
		t.Fatalf("%s: body %q err=%v", req.URL.Path, body, errRead)
	}
}

// plainReq builds a request without a *testing.T, for use in goroutines.
func plainReq(ctx context.Context, addr, path string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+path, nil)
	return req
}

// One request of a credential is cancelled by its client before the upstream
// answered (Codex ESC, sub2api giving up, a conductor failover). The
// credential's other stream on the same connection must not be affected, and
// the healthy connection stays in the pool.
func TestCancelledRequestKeepsSiblingStreamAndConnection(t *testing.T) {
	srv := startStrictH2(t, 0, false)
	rt := testRoundTripper(t)
	h2Conn := srv.inject(t, rt)

	running := startHold(t, rt, srv.addr)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := rt.RoundTrip(strictReq(t, ctx, srv.addr, "/never")); err == nil {
		t.Fatal("the cancelled request succeeded")
	}
	time.Sleep(300 * time.Millisecond) // retirement and the liveness ping are asynchronous

	close(srv.release)
	if rest, err := finishHold(running); err != nil || rest != "last\n" {
		t.Errorf("the sibling stream was cut after another request was cancelled: rest=%q err=%v", rest, err)
	}
	srv.assertNoClientGoAway(t)
	rt.mu.Lock()
	held := rt.conns[srv.addr]
	rt.mu.Unlock()
	if held != h2Conn {
		t.Error("a client-side cancel retired the credential's healthy connection")
	}
}

// A connection that stops answering is still dropped after a cancel: the
// liveness ping goes unanswered.
func TestCancelledRequestOnDeadConnectionDropsIt(t *testing.T) {
	previous := utlsCancelPingTimeout
	utlsCancelPingTimeout = 200 * time.Millisecond
	t.Cleanup(func() { utlsCancelPingTimeout = previous })

	// A peer that completes the preface and SETTINGS exchange, then goes silent.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, errAccept := ln.Accept()
		if errAccept != nil {
			return
		}
		defer func() { _ = c.Close() }()
		preface := make([]byte, len(http2.ClientPreface))
		if _, errRead := io.ReadFull(c, preface); errRead != nil {
			return
		}
		fr := http2.NewFramer(c, c)
		_ = fr.WriteSettings()
		for {
			if _, errRead := fr.ReadFrame(); errRead != nil {
				return
			}
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	h2Conn, err := (&http2.Transport{}).NewClientConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	rt := testRoundTripper(t)
	rt.mu.Lock()
	rt.conns[ln.Addr().String()] = h2Conn
	rt.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err = rt.RoundTrip(strictReq(t, ctx, ln.Addr().String(), "/")); err == nil {
		t.Fatal("a request to a silent peer succeeded")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && rt.heldConnections() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.heldConnections() != 0 {
		t.Error("a connection whose peer stopped answering pings is still handed out")
	}
}

// The peer announces a graceful GOAWAY (rolling restart, connection max-age)
// that still covers the running stream. The next request of the credential
// must not make the peer abandon that stream.
func TestServerGoAwayKeepsAnnouncedStream(t *testing.T) {
	srv := startStrictH2(t, 0, false)
	rt := testRoundTripper(t)
	h2Conn := srv.inject(t, rt)

	running := startHold(t, rt, srv.addr) // stream 1
	srv.mu.Lock()
	goAway := srv.serverGoAway
	srv.mu.Unlock()
	goAway(1) // "I will finish stream 1, open no more"
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = rt.RoundTrip(strictReq(t, ctx, srv.addr, "/")) // needs a new connection; the plaintext peer cannot give one
	time.Sleep(300 * time.Millisecond)

	close(srv.release)
	if rest, err := finishHold(running); err != nil || rest != "last\n" {
		t.Errorf("a stream the peer promised to finish was cut: rest=%q err=%v", rest, err)
	}
	srv.assertNoClientGoAway(t)
	if !eventuallyClosed(t, h2Conn) {
		t.Error("the GOAWAY'd connection was not closed after its last stream")
	}
}

// The connection is momentarily full (the peer's SETTINGS_MAX_CONCURRENT_STREAMS
// reached). The streams already running on it are not cut.
func TestFullConnectionKeepsRunningStreamsOnStrictPeer(t *testing.T) {
	srv := startStrictH2(t, 2, false)
	rt := testRoundTripper(t)
	h2Conn := srv.inject(t, rt)
	roundTripOK(t, rt, strictReq(t, context.Background(), srv.addr, "/")) // learn the peer's SETTINGS
	a := startHold(t, rt, srv.addr)
	b := startHold(t, rt, srv.addr)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = rt.RoundTrip(strictReq(t, ctx, srv.addr, "/"))
	time.Sleep(300 * time.Millisecond)

	close(srv.release)
	for name, resp := range map[string]*http.Response{"a": a, "b": b} {
		if rest, err := finishHold(resp); err != nil || rest != "last\n" {
			t.Errorf("running stream %s was cut when the full connection was retired: rest=%q err=%v", name, rest, err)
		}
	}
	srv.assertNoClientGoAway(t)
	if !eventuallyClosed(t, h2Conn) {
		t.Error("the retired full connection was not closed after its last stream")
	}
}

// REFUSED_STREAM means the peer did not process the request. It is sent again
// (as x/net's Transport would, but at most utlsUnprocessedRetries times), its
// sibling is not affected, and the healthy connection is kept.
func TestRefusedStreamIsRetriedAndSiblingSurvives(t *testing.T) {
	srv := startStrictH2(t, 0, false)
	rt := testRoundTripper(t)
	h2Conn := srv.inject(t, rt)
	running := startHold(t, rt, srv.addr)

	roundTripOK(t, rt, strictReq(t, context.Background(), srv.addr, "/refuse-once"))
	if n := srv.hitsFor("/refuse-once"); n != 2 {
		t.Errorf("the refused request reached the peer %d time(s), want 2 (refused, then retried)", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rt.RoundTrip(strictReq(t, ctx, srv.addr, "/refuse")); err == nil {
		t.Error("a request the peer always refuses succeeded")
	}
	if n := srv.hitsFor("/refuse"); n != 1+utlsUnprocessedRetries {
		t.Errorf("an always-refused request reached the peer %d time(s), want exactly %d (bounded retries)", n, 1+utlsUnprocessedRetries)
	}

	close(srv.release)
	if rest, err := finishHold(running); err != nil || rest != "last\n" {
		t.Errorf("sibling stream cut after a REFUSED_STREAM on another stream: rest=%q err=%v", rest, err)
	}
	srv.assertNoClientGoAway(t)
	rt.mu.Lock()
	held := rt.conns[srv.addr]
	rt.mu.Unlock()
	if held != h2Conn || srv.connectionsAccepted() != 1 {
		t.Errorf("a refused stream cost the connection (held same=%v, connections=%d)", held == h2Conn, srv.connectionsAccepted())
	}
}

// A graceful GOAWAY that does not cover a request means the peer never
// processed it: it is sent again on a new connection instead of failing.
func TestRequestBeyondGoAwayIsRetriedOnNewConnection(t *testing.T) {
	srv := startStrictH2(t, 0, true)
	rt := testRoundTripper(t)
	roundTripOK(t, rt, strictReq(t, context.Background(), srv.addr, "/")) // stream 1 on connection 1

	roundTripOK(t, rt, strictReq(t, context.Background(), srv.addr, "/goaway-once"))
	if n := srv.hitsFor("/goaway-once"); n != 2 {
		t.Errorf("the request reached the peer %d time(s), want 2", n)
	}
	if n := srv.connectionsAccepted(); n != 2 {
		t.Errorf("%d connections, want 2 (the GOAWAY'd one and its replacement)", n)
	}
	srv.assertNoClientGoAway(t)
}

// Bounced by two connections' GOAWAYs in a row (a rolling restart racing a
// busy credential): still delivered, on the third connection.
func TestRequestBouncedByTwoGoAwaysIsDelivered(t *testing.T) {
	srv := startStrictH2(t, 0, true)
	rt := testRoundTripper(t)
	roundTripOK(t, rt, strictReq(t, context.Background(), srv.addr, "/"))
	start := time.Now()
	roundTripOK(t, rt, strictReq(t, context.Background(), srv.addr, "/goaway-twice"))
	if n := srv.hitsFor("/goaway-twice"); n != 3 {
		t.Errorf("the request reached the peer %d time(s), want 3", n)
	}
	if took := time.Since(start); took < utlsRetryBackoff {
		t.Errorf("the second retry did not back off (%s)", took)
	}
	srv.assertNoClientGoAway(t)
}

// A stream reserved in connFor whose connection is retired before RoundTrip
// sends it (a sibling failure, or the peer's GOAWAY landing in between) was
// never sent: it goes out on the replacement connection, and the retired
// connection is closed once that abandoned reservation is released.
func TestRequestLostToConcurrentRetirementIsRetried(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	first := server.connect(t, rt)
	server.get(t, rt)

	var once sync.Once
	testHookAfterReserve = func(addr string, h2Conn *http2.ClientConn) {
		once.Do(func() {
			rt.discard(addr, h2Conn)
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				if st := h2Conn.State(); st.Closing || st.Closed {
					break
				}
			}
			server.connect(t, rt)
		})
	}
	t.Cleanup(func() { testHookAfterReserve = func(string, *http2.ClientConn) {} })

	server.get(t, rt)
	if !eventuallyClosed(t, first) {
		t.Error("the retired connection stayed open after the reservation on it was abandoned")
	}
}

// The errors requestNotProcessed matches by text are the ones the x/net in
// go.mod produces; a version that rewords them must fail here, not silently
// stop retrying.
func TestRequestNotProcessedMatchesXNetErrors(t *testing.T) {
	// client conn not usable: a request on a connection marked not reusable.
	server := startH2Server(t)
	rt := testRoundTripper(t)
	h2Conn := server.connect(t, rt)
	server.get(t, rt)
	h2Conn.SetDoNotReuse()
	_, errUnusable := h2Conn.RoundTrip(strictReq(t, context.Background(), server.addr, "/"))
	if errUnusable == nil || errUnusable.Error() != errTextClientConnUnusable || !requestNotProcessed(errUnusable) {
		t.Errorf("not-usable error = %v, want %q recognised", errUnusable, errTextClientConnUnusable)
	}

	// graceful GOAWAY that does not cover the stream.
	strict := startStrictH2(t, 0, false)
	rt2 := testRoundTripper(t)
	conn2 := strict.inject(t, rt2)
	if resp, err := conn2.RoundTrip(strictReq(t, context.Background(), strict.addr, "/")); err == nil {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	} else {
		t.Fatalf("warm-up: %v", err)
	}
	_, errGoAway := conn2.RoundTrip(strictReq(t, context.Background(), strict.addr, "/goaway-hold"))
	if errGoAway == nil || errGoAway.Error() != errTextClientConnGotGoAway || !requestNotProcessed(errGoAway) {
		t.Errorf("GOAWAY error = %v, want %q recognised", errGoAway, errTextClientConnGotGoAway)
	}

	// REFUSED_STREAM.
	_, errRefused := strict.inject(t, rt2).RoundTrip(strictReq(t, context.Background(), strict.addr, "/refuse"))
	if !requestNotProcessed(errRefused) {
		t.Errorf("REFUSED_STREAM error %v not recognised", errRefused)
	}

	// client conn could not be established: the peer closed a brand-new connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, errAccept := ln.Accept()
		if errAccept != nil {
			return
		}
		preface := make([]byte, len(http2.ClientPreface))
		_, _ = io.ReadFull(c, preface)
		_ = c.Close()
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := (&http2.Transport{}).NewClientConn(raw)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !fresh.State().Closed; time.Sleep(5 * time.Millisecond) {
	}
	_, errNotOpened := fresh.RoundTrip(strictReq(t, context.Background(), ln.Addr().String(), "/"))
	if errNotOpened == nil || errNotOpened.Error() != errTextClientConnNotOpened || !requestNotProcessed(errNotOpened) {
		t.Errorf("never-established error = %v, want %q recognised", errNotOpened, errTextClientConnNotOpened)
	}

	for _, err := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		io.EOF,
		http2.StreamError{StreamID: 3, Code: http2.ErrCodeInternal},
		errors.New("http2: client connection force closed via ClientConn.Close"),
	} {
		if requestNotProcessed(err) {
			t.Errorf("%v must not be retried: the peer may have processed it", err)
		}
	}
}

// A busy credential over a real uTLS+h2 handshake. A quarter of the requests
// are abandoned by their client before the upstream answers. The others all
// succeed, and the credential keeps one connection (every extra one is a full
// handshake on the wire). Also covers the one connFor line the injected-
// connection tests cannot reach: a freshly handshaken h2 connection is pooled.
func TestTLSCredentialKeepsOneConnectionThroughClientCancels(t *testing.T) {
	var accepted atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			http.Error(w, "want h2, got "+r.Proto, http.StatusHTTPVersionNotSupported)
			return
		}
		select {
		case <-time.After(40 * time.Millisecond): // upstream thinking time before headers
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	trustForTest(t, pool)

	rt := testRoundTripper(t)
	// The first request dials and pools the connection; the abandoned ones below
	// must not churn it (a dial abandoned half way would count as a second
	// accepted connection and say nothing about reuse).
	warm, err := rt.RoundTrip(plainReq(context.Background(), strings.TrimPrefix(srv.URL, "https://"), "/"))
	if err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	_, _ = io.ReadAll(warm.Body)
	_ = warm.Body.Close()

	const workers, perWorker = 6, 40
	var okCount, failed atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	samples := map[string]int{}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				ctx, cancel := context.Background(), context.CancelFunc(func() {})
				abandon := (w*perWorker+i)%4 == 0
				if abandon {
					ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
				}
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", nil)
				resp, err := rt.RoundTrip(req)
				if err == nil {
					var body []byte
					body, err = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err == nil && string(body) != "ok" {
						err = errors.New("body " + string(body))
					}
				}
				cancel()
				switch {
				case abandon:
				case err != nil:
					failed.Add(1)
					mu.Lock()
					samples[err.Error()]++
					mu.Unlock()
				default:
					okCount.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	for msg, n := range samples {
		t.Logf("  %4d x %s", n, msg)
	}
	if failed.Load() > 0 {
		t.Errorf("%d requests that nobody cancelled failed", failed.Load())
	}
	if okCount.Load() != workers*perWorker*3/4 {
		t.Errorf("ok=%d, want %d", okCount.Load(), workers*perWorker*3/4)
	}
	if n := accepted.Load(); n != 1 {
		t.Errorf("one credential opened %d TLS connections for %d requests, want 1", n, workers*perWorker)
	}
	if rt.heldConnections() != 1 {
		t.Errorf("the round tripper holds %d connections, want 1", rt.heldConnections())
	}
}

// gatedDialer counts dials and can hold them until released (G1-4 tests).
type gatedDialer struct {
	mu    sync.Mutex
	dials int
	gate  chan struct{}
	delay time.Duration
}

func (d *gatedDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *gatedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dials++
	gate, delay := d.gate, d.delay
	d.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

func (d *gatedDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// Concurrent first requests of a credential wait for the one dial in progress
// and share its connection.
func TestConcurrentFirstRequestsShareOneDial(t *testing.T) {
	srv := startStrictH2(t, 0, true)
	rt := testRoundTripper(t)
	dialer := &gatedDialer{delay: 150 * time.Millisecond}
	rt.dialer = dialer

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := rt.RoundTrip(plainReq(context.Background(), srv.addr, "/"))
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("request failed: %v", err)
	}
	if dialer.count() != 1 || srv.connectionsAccepted() != 1 {
		t.Errorf("eight concurrent first requests made %d dials / %d connections, want 1 / 1", dialer.count(), srv.connectionsAccepted())
	}
}

// A request waiting for another request's dial leaves as soon as its own
// context ends, and eviction is not held up by the dial either.
func TestWaitingRequestAndEvictionAreNotHeldByADial(t *testing.T) {
	srv := startStrictH2(t, 0, true)
	rt := testRoundTripper(t)
	dialer := &gatedDialer{gate: make(chan struct{})}
	rt.dialer = dialer

	firstDone := make(chan error, 1)
	go func() {
		resp, err := rt.RoundTrip(plainReq(context.Background(), srv.addr, "/"))
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		firstDone <- err
	}()
	for deadline := time.Now().Add(2 * time.Second); dialer.count() == 0 && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := rt.RoundTrip(strictReq(t, ctx, srv.addr, "/"))
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting request returned %v, want its own deadline", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the waiting request was held %s by another request's dial", waited)
	}

	evicted := make(chan struct{})
	go func() { rt.closeIdleConnections(); close(evicted) }()
	select {
	case <-evicted:
	case <-time.After(time.Second):
		t.Error("eviction was held up by a dial in progress")
	}

	close(dialer.gate)
	if err := <-firstDone; err != nil {
		t.Errorf("the dialling request failed: %v", err)
	}
}

// The dial plus handshake are bounded, so a black-holed SYN does not hold the
// credential's requests for the kernel's connect timeout.
func TestDialIsBoundedByUtlsDialTimeout(t *testing.T) {
	previous := utlsDialTimeout
	utlsDialTimeout = 200 * time.Millisecond
	t.Cleanup(func() { utlsDialTimeout = previous })
	rt := testRoundTripper(t)
	rt.dialer = &gatedDialer{gate: make(chan struct{})} // never released

	start := time.Now()
	_, err := rt.RoundTrip(strictReq(t, context.Background(), "127.0.0.1:9", "/"))
	if err == nil || !strings.Contains(err.Error(), "utls: dial upstream") {
		t.Errorf("err = %v, want a dial failure", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the dial took %s, want about utlsDialTimeout", took)
	}
}
