// Derived from CPA core at 4aabe5c3c7543e5c08741eeaa51569d792455a86.
// See CPA-MIT-LICENSE and UPSTREAM.md for provenance and bounded changes.
package adapter

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jaxson-wang/cpa-plugin-oai-basispoints/internal/proxyutil"
	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using a Chrome fingerprint for
// providers that require browser-like TLS. The HTTP version follows ALPN.
// There is one per credential (local patch, see chromeRoundTripperSelector):
// when ALPN picks h2 it holds that credential's connection open between
// requests and multiplexes over it the way a browser does; any other protocol
// keeps upstream's dedicated connection per request.
type utlsRoundTripper struct {
	dialer       proxy.Dialer
	sessionCache tls.ClientSessionCache

	// conns holds one HTTP/2 connection per host for this credential, kept
	// open between requests. See connFor for why.
	mu    sync.Mutex
	conns map[string]*http2.ClientConn
	// dialing has an entry while a connection to that host is being dialled;
	// the channel is closed when the dial ends (local patch G1-4).
	dialing map[string]chan struct{}
	// verifying holds the connections with a liveness ping in flight (G1-2).
	verifying map[*http2.ClientConn]struct{}
}

type closeConnectionBody struct {
	io.ReadCloser
	closeConnection func() error
	once            sync.Once
	err             error
}

func (b *closeConnectionBody) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		var errConnection error
		if b.closeConnection != nil {
			errConnection = b.closeConnection()
		}
		var errBody error
		if b.ReadCloser != nil {
			errBody = b.ReadCloser.Close()
		}
		b.err = errors.Join(errBody, errConnection)
	})
	return b.err
}

// chromeSessionCacheCapacity bounds one credential's TLS session cache.
//
// The server does issue tickets into it, but nothing resumes today: utls's
// HelloChrome_133 spec (what HelloChrome_Auto resolves to in v1.8.2) carries no
// pre_shared_key extension, so a ticket is never offered back. Reusing one
// connection per credential is what keeps the aggregate browser-like; this cache
// starts working the day the spec carries PSK. (local patch)
const chromeSessionCacheCapacity = 8

// utlsDialTimeout bounds the TCP dial plus TLS handshake of a new connection.
// proxy.Direct has no timeout of its own: a black-holed SYN held the
// credential's requests for the kernel's connect timeout (about 127 s). A
// variable so tests can shorten it. (local patch G1-4)
var utlsDialTimeout = 30 * time.Second

// utlsCancelPingTimeout bounds the liveness ping sent after a client abandoned
// a request on a held connection (local patch G1-2).
var utlsCancelPingTimeout = 5 * time.Second

// utlsUnprocessedRetries bounds how often a request the peer provably never
// processed is sent again; retries after the first wait utlsRetryBackoff times
// the attempt number. x/net's own Transport retries these up to 7 times with
// exponential backoff from 1s. (local patch G1-3)
const utlsUnprocessedRetries = 2

var utlsRetryBackoff = 200 * time.Millisecond

// utlsRootCAs, when set, replaces the system roots for the Chrome handshake.
// Tests only; nil (the system roots) in production. (local patch)
var utlsRootCAs *x509.CertPool

// Adaptation: selection and bounded caching live in transport_pool.go. Invalid
// proxies fail closed instead of silently becoming direct connections.
func newUtlsRoundTripper(proxyURL string) (*utlsRoundTripper, error) {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		built, mode, err := proxyutil.BuildDialer(proxyURL)
		if err != nil || mode != proxyutil.ModeProxy || built == nil {
			return nil, errors.New("invalid selected-account proxy")
		}
		dialer = built
	}
	return &utlsRoundTripper{dialer: dialer, sessionCache: tls.NewLRUClientSessionCache(chromeSessionCacheCapacity), conns: make(map[string]*http2.ClientConn)}, nil
}

func (t *utlsRoundTripper) createConnection(ctx context.Context, host, addr string) (*tls.UConn, error) {
	return t.createConnectionWithALPN(ctx, host, addr, nil)
}

func (t *utlsRoundTripper) createConnectionWithALPN(ctx context.Context, host, addr string, protocols []string) (*tls.UConn, error) {
	contextDialer, ok := t.dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("utls: dialer does not support context cancellation")
	}
	conn, errDial := contextDialer.DialContext(ctx, "tcp", addr)
	if errDial != nil {
		return nil, fmt.Errorf("utls: dial upstream: %w", errDial)
	}

	tlsConfig := &tls.Config{ServerName: host, ClientSessionCache: t.sessionCache, RootCAs: utlsRootCAs}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloChrome_Auto)
	if len(protocols) > 0 {
		if err := tlsConn.BuildHandshakeState(); err != nil {
			_ = conn.Close()
			return nil, errors.New("cannot prepare browser TLS handshake")
		}
		for _, extension := range tlsConn.Extensions {
			if alpn, ok := extension.(*tls.ALPNExtension); ok {
				alpn.AlpnProtocols = append([]string(nil), protocols...)
			}
		}

	}

	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		if errors.Is(errHandshake, context.Canceled) || errors.Is(errHandshake, context.DeadlineExceeded) {
			_ = conn.Close()
			return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
		}
		if errClose := conn.Close(); errClose != nil {
			return nil, fmt.Errorf("utls: TLS handshake: %w; close connection: %v", errHandshake, errClose)
		}
		return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
	}

	return tlsConn, nil
}

// connFor returns either the credential's held HTTP/2 connection to addr with a
// stream reserved on it, or -- when a fresh handshake did not negotiate h2 -- the
// handshaken TLS connection for upstream's one-shot roundTripUtlsConnection.
//
// A credential's concurrent requests wait for the one connection being dialled
// instead of racing to open several, which is also what a browser does. The
// dial itself runs outside t.mu (local patch G1-4): a waiting request whose
// context ends leaves at once, eviction (closeIdleConnections) is never held up
// by a slow dial, and the dial plus handshake are bounded by utlsDialTimeout.
// ReserveNewRequest both reports whether the connection can take another
// request (false after the peer's GOAWAY, or while every stream the peer allows
// is in use) and claims a stream. A connection that can take no more is
// retired, not closed: the streams still running on it are finished first (see
// retireConn). (local patch)
func (t *utlsRoundTripper) connFor(ctx context.Context, host, addr string) (*http2.ClientConn, *tls.UConn, error) {
	for {
		t.mu.Lock()
		if t.conns == nil {
			t.conns = make(map[string]*http2.ClientConn)
		}
		if existing := t.conns[addr]; existing != nil {
			if existing.ReserveNewRequest() {
				t.mu.Unlock()
				return existing, nil, nil
			}
			delete(t.conns, addr)
			retireConn(existing)
		}
		if wait := t.dialing[addr]; wait != nil {
			t.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, nil, fmt.Errorf("utls: waiting for the connection to %s: %w", addr, ctx.Err())
			}
		}
		if t.dialing == nil {
			t.dialing = make(map[string]chan struct{})
		}
		done := make(chan struct{})
		t.dialing[addr] = done
		t.mu.Unlock()

		h2Conn, tlsConn, err := t.dial(ctx, host, addr)

		t.mu.Lock()
		delete(t.dialing, addr)
		close(done)
		if h2Conn != nil {
			t.conns[addr] = h2Conn
		}
		t.mu.Unlock()
		return h2Conn, tlsConn, err
	}
}

// dial opens a new connection to addr. An h2 one comes back with a stream
// reserved; any other protocol comes back as the bare TLS connection.
func (t *utlsRoundTripper) dial(ctx context.Context, host, addr string) (*http2.ClientConn, *tls.UConn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, utlsDialTimeout)
	defer cancel()
	tlsConn, err := t.createConnection(dialCtx, host, addr)
	if err != nil {
		return nil, nil, err
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		return nil, tlsConn, nil
	}
	h2Conn, errClientConn := (&http2.Transport{}).NewClientConn(tlsConn)
	if errClientConn != nil {
		_ = tlsConn.Close()
		return nil, nil, fmt.Errorf("utls: initialize HTTP/2 connection: %w", errClientConn)
	}
	if !h2Conn.ReserveNewRequest() {
		_ = h2Conn.Close()
		return nil, nil, fmt.Errorf("utls: fresh connection to %s cannot take a request", addr)
	}
	return h2Conn, nil, nil
}

// retireConn stops a held connection from being handed out without cutting
// the requests still running on it, and without sending anything.
//
// Closing it outright (what the v7 patch did) interrupted every other in-flight
// stream of the credential with "http2: client connection force closed via
// ClientConn.Close". ClientConn.Shutdown (the first v8 version) sends a client
// GOAWAY whose last_stream_id is the next client stream ID, an odd number:
// RFC 9113 6.8 wants the highest server-initiated stream processed (0 without
// push), and nghttp2-based servers answer it with PROTOCOL_ERROR "GOAWAY:
// invalid last_stream_id" and drop the whole connection, siblings included. A
// browser does not send that frame either. SetDoNotReuse writes no frame: x/net
// closes the connection when its last stream ends (forgetStreamID, closeOnIdle),
// and an idle one is closed here. It runs in its own goroutine because State
// and Close can wait on a connection whose writes are stuck. (local patch G1)
func retireConn(h2Conn *http2.ClientConn) {
	if h2Conn == nil {
		return
	}
	go func() {
		h2Conn.SetDoNotReuse()
		if st := h2Conn.State(); st.StreamsActive == 0 && st.StreamsReserved == 0 && st.StreamsPending == 0 {
			_ = h2Conn.Close()
		}
	}()
}

// discard stops handing out a connection that can carry no more requests, so
// the next request dials a new one. Other requests already running on it are
// left to finish. (local patch)
func (t *utlsRoundTripper) discard(addr string, h2Conn *http2.ClientConn) {
	t.mu.Lock()
	if t.conns[addr] == h2Conn {
		delete(t.conns, addr)
	}
	t.mu.Unlock()
	retireConn(h2Conn)
}

// closeIdleConnections retires every connection this credential holds. It runs
// when the round tripper is evicted from chromeRoundTripperCache. (local patch)
func (t *utlsRoundTripper) closeIdleConnections() {
	t.mu.Lock()
	conns := make([]*http2.ClientConn, 0, len(t.conns))
	for addr, h2Conn := range t.conns {
		conns = append(conns, h2Conn)
		delete(t.conns, addr)
	}
	t.mu.Unlock()
	for _, h2Conn := range conns {
		retireConn(h2Conn)
	}
}

// afterRequestError decides what one failed request says about its connection
// (local patch G1-2). Only a connection that can carry no more requests is
// discarded: the read loop ended, a write failed, a GOAWAY arrived, or it was
// marked not reusable -- x/net reports all of those as Closed or Closing. A
// stream-level failure (RST_STREAM from the peer, REFUSED_STREAM) leaves a
// healthy connection that the credential's other requests are still using. A
// request its client abandoned (Codex ESC, sub2api giving up, a failover) says
// nothing either; the connection is kept unless a ping goes unanswered.
func (t *utlsRoundTripper) afterRequestError(req *http.Request, addr string, h2Conn *http2.ClientConn) {
	if st := h2Conn.State(); st.Closed || st.Closing {
		t.discard(addr, h2Conn)
		return
	}
	if req.Context().Err() != nil {
		t.verifyAfterCancel(addr, h2Conn)
	}
}

// verifyAfterCancel keeps a connection whose request was abandoned by its
// client unless the peer stops answering on it. One ping per connection at a
// time, however many requests are abandoned at once. (local patch G1-2)
func (t *utlsRoundTripper) verifyAfterCancel(addr string, h2Conn *http2.ClientConn) {
	t.mu.Lock()
	if t.verifying == nil {
		t.verifying = make(map[*http2.ClientConn]struct{})
	}
	if _, busy := t.verifying[h2Conn]; busy {
		t.mu.Unlock()
		return
	}
	t.verifying[h2Conn] = struct{}{}
	t.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), utlsCancelPingTimeout)
		err := h2Conn.Ping(ctx)
		cancel()
		t.mu.Lock()
		delete(t.verifying, h2Conn)
		t.mu.Unlock()
		if err != nil {
			t.discard(addr, h2Conn)
		}
	}()
}

// testHookAfterReserve runs between connFor and ClientConn.RoundTrip (tests only).
var testHookAfterReserve = func(addr string, h2Conn *http2.ClientConn) {}

// x/net reports these three as unexported sentinel errors; they are matched by
// text, pinned by TestRequestNotProcessedMatchesXNetErrors against the x/net
// version in go.mod.
const (
	errTextClientConnUnusable  = "http2: client conn not usable"
	errTextClientConnGotGoAway = "http2: Transport received Server's graceful shutdown GOAWAY"
	errTextClientConnNotOpened = "http2: client conn could not be established"
)

// requestNotProcessed reports the errors that guarantee the peer never
// processed the request: the stream was never opened on a connection that was
// already retired or closed, the peer's GOAWAY did not cover it, or the peer
// refused it (REFUSED_STREAM). x/net's own Transport retries exactly these
// (canRetryError). (local patch G1-3)
func requestNotProcessed(err error) bool {
	if err == nil {
		return false
	}
	var streamErr http2.StreamError
	if errors.As(err, &streamErr) && streamErr.Code == http2.ErrCodeRefusedStream {
		return true
	}
	switch err.Error() {
	case errTextClientConnUnusable, errTextClientConnGotGoAway, errTextClientConnNotOpened:
		return true
	}
	return false
}

// rewindRequest returns a request that can be sent again, or false when its
// body cannot be replayed.
func rewindRequest(req *http.Request) (*http.Request, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	again := req.Clone(req.Context())
	again.Body = body
	return again, true
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	for attempt := 0; ; attempt++ {
		h2Conn, tlsConn, err := t.connFor(req.Context(), hostname, addr)
		if err != nil {
			return nil, err
		}
		if h2Conn == nil {
			// Not h2 (HTTP/1.1 or empty ALPN, e.g. behind a TLS-inspecting proxy):
			// upstream's dedicated connection, closed with the response body.
			return roundTripUtlsConnection(req, tlsConn)
		}
		testHookAfterReserve(addr, h2Conn)
		resp, err := h2Conn.RoundTrip(req)
		if err == nil {
			if resp == nil {
				t.discard(addr, h2Conn)
				return nil, fmt.Errorf("utls: upstream returned an empty response")
			}
			if resp.Body == nil {
				resp.Body = http.NoBody
			}
			// Closing the body ends this stream only; the connection stays open.
			return resp, nil
		}
		t.afterRequestError(req, addr, h2Conn)
		// The peer provably never saw it: send it again (local patch G1-3).
		if attempt < utlsUnprocessedRetries && req.Context().Err() == nil && requestNotProcessed(err) {
			if again, ok := rewindRequest(req); ok {
				if attempt > 0 && !sleepContext(req.Context(), time.Duration(attempt)*utlsRetryBackoff) {
					return nil, err
				}
				req = again
				continue
			}
		}
		return nil, err
	}
}

// sleepContext waits d unless ctx ends first; it reports whether the full wait elapsed.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// roundTripUtlsConnection selects the HTTP protocol before sending the request.
// Empty ALPN is HTTP/1.1, including when a TLS-inspecting proxy omits ALPN.
func roundTripUtlsConnection(req *http.Request, tlsConn *tls.UConn) (*http.Response, error) {
	closeConnection := func() error {
		// The HTTP/1.1 transport may already have closed its non-pooled
		// connection after reading the response or canceling the request.
		if errClose := tlsConn.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			return errClose
		}
		return nil
	}
	var resp *http.Response
	var err error
	switch protocol := tlsConn.ConnectionState().NegotiatedProtocol; protocol {
	case "h2":
		h2Conn, errClientConn := (&http2.Transport{}).NewClientConn(tlsConn)
		if errClientConn != nil {
			err = fmt.Errorf("utls: initialize HTTP/2 connection: %w", errClientConn)
			break
		}
		closeConnection = h2Conn.Close
		resp, err = h2Conn.RoundTrip(req)
	case "", "http/1.1":
		// Reuse the already-handshaken uTLS connection. A fresh, non-pooling
		// transport retains net/http's cancellation and request-body handling
		// without changing the TLS fingerprint or redialing through another path.
		transport := &http.Transport{
			DisableKeepAlives: true,
			DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				return tlsConn, nil
			},
		}
		resp, err = transport.RoundTrip(req)
		transport.CloseIdleConnections()
	default:
		err = fmt.Errorf("utls: unsupported negotiated protocol %q", protocol)
	}
	if err != nil {
		_ = closeConnection()
		return nil, err
	}
	if resp == nil {
		_ = closeConnection()
		return nil, fmt.Errorf("utls: upstream returned an empty response")
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &closeConnectionBody{
		ReadCloser:      resp.Body,
		closeConnection: closeConnection,
	}
	return resp, nil
}
