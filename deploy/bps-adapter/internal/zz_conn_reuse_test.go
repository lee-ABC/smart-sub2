//go:build !go1.27 || http2legacy

package adapter

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// h2Server is a real HTTP/2 server, so connection reuse is observed on the wire
// rather than inferred from the round tripper's own bookkeeping. It speaks h2
// without TLS: the transport hands http2 an established connection either way,
// and a plaintext one lets a test dial it without a trusted certificate. The
// TLS handshake itself is exercised against the real upstream, not here.
type h2Server struct {
	addr string

	mu       sync.Mutex
	accepted int
}

func startH2Server(t *testing.T) *h2Server {
	t.Helper()
	return startH2ServerWithStreams(t, 0)
}

// startH2ServerWithStreams caps the streams a connection may run at once
// (SETTINGS_MAX_CONCURRENT_STREAMS); 0 keeps the http2 default.
func startH2ServerWithStreams(t *testing.T, maxStreams uint32) *h2Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	server := &h2Server{addr: listener.Addr().String()}
	h2 := &http2.Server{MaxConcurrentStreams: maxStreams}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stream" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 5; i++ {
			_, _ = fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	go func() {
		for {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			server.mu.Lock()
			server.accepted++
			server.mu.Unlock()
			go h2.ServeConn(conn, &http2.ServeConnOpts{Handler: handler})
		}
	}()
	return server
}

func (s *h2Server) connectionsAccepted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// connect gives the round tripper a live connection to the server, standing in
// for the dial that createConnection performs in production.
func (s *h2Server) connect(t *testing.T, rt *utlsRoundTripper) *http2.ClientConn {
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

func (s *h2Server) request(rt *utlsRoundTripper, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+s.addr+path, nil)
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(req)
}

func (s *h2Server) get(t *testing.T, rt *utlsRoundTripper) {
	t.Helper()
	resp, err := s.request(rt, "/")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if _, err = io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err = resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
}

// eventuallyClosed waits for a retired connection to finish closing: retiring
// is graceful and asynchronous (retireConn), so it is not closed on return.
func eventuallyClosed(t *testing.T, h2Conn *http2.ClientConn) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h2Conn.State().Closed {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func (t *utlsRoundTripper) heldConnections() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

// The whole point of the change: a credential's requests ride one connection
// instead of opening a fresh one, and a fresh full handshake, every time.
func TestConnectionIsReusedAcrossRequests(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	server.connect(t, rt)

	for i := 0; i < 5; i++ {
		server.get(t, rt)
	}

	if got := server.connectionsAccepted(); got != 1 {
		t.Errorf("five requests arrived on %d connections, want 1", got)
	}
	if got := rt.heldConnections(); got != 1 {
		t.Errorf("the round tripper holds %d connections afterwards, want 1", got)
	}
}

// Closing a response body used to close the connection under it. It must now
// end only that stream.
func TestClosingTheBodyLeavesTheConnectionUsable(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	h2Conn := server.connect(t, rt)

	server.get(t, rt)
	if state := h2Conn.State(); state.Closed || state.Closing {
		t.Fatal("the connection is closed after one request completed: closing the body closed it")
	}
	server.get(t, rt)
	if got := server.connectionsAccepted(); got != 1 {
		t.Errorf("the second request opened another connection (%d in total)", got)
	}
}

// Streaming is the risky part of holding a connection: the body is read over
// time, and the connection must stay healthy for the next request afterwards.
func TestStreamingWorksAndLeavesTheConnectionUsable(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	server.connect(t, rt)

	resp, err := server.request(rt, "/stream")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	scanner := bufio.NewScanner(resp.Body)
	chunks := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			chunks++
		}
	}
	if err = scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if err = resp.Body.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	if chunks != 5 {
		t.Errorf("received %d streamed chunks, want 5", chunks)
	}

	server.get(t, rt)
	if got := server.connectionsAccepted(); got != 1 {
		t.Errorf("after streaming, the next request opened another connection (%d in total)", got)
	}
}

// A stream abandoned half way -- a cancelled client, a failover -- must reset
// only its own stream.
func TestAbandonedStreamLeavesTheConnectionUsable(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	server.connect(t, rt)

	resp, err := server.request(rt, "/stream")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	buf := make([]byte, 8)
	if _, err = resp.Body.Read(buf); err != nil {
		t.Fatalf("read first bytes: %v", err)
	}
	_ = resp.Body.Close()

	server.get(t, rt)
	if got := server.connectionsAccepted(); got != 1 {
		t.Errorf("abandoning a stream cost the connection (%d opened in total)", got)
	}
}

// Concurrent requests for one credential multiplex over the single connection
// instead of each dialling their own.
func TestConcurrentRequestsShareOneConnection(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	server.connect(t, rt)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := server.request(rt, "/")
			if err != nil {
				errs <- err
				return
			}
			if _, err = io.ReadAll(resp.Body); err != nil {
				errs <- err
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent request failed: %v", err)
	}
	if got := server.connectionsAccepted(); got != 1 {
		t.Errorf("eight concurrent requests used %d connections, want 1", got)
	}
}

// A connection the peer has dropped must not be handed to the next request.
//
// The connection carries a request before it dies, which is the only shape the
// map ever holds in production and the only one this can be asserted on: http2
// deliberately reports a brand-new closed connection as usable, so that the
// request fails visibly instead of being retried elsewhere (transport.go,
// idleStateLocked).
func TestSpentConnectionIsNotReused(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	h2Conn := server.connect(t, rt)
	server.get(t, rt)
	if err := h2Conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// No replacement can be established here, because the server speaks h2
	// without TLS and createConnection insists on a handshake. The dial failure
	// is itself the proof that the dead connection was not reused.
	_, err := server.request(rt, "/")
	if err == nil {
		t.Fatal("the request succeeded, so it was sent over the closed connection")
	}
	if !strings.Contains(err.Error(), "utls:") {
		t.Errorf("request failed with %v, want a failure from dialling a replacement", err)
	}
	if got := rt.heldConnections(); got != 0 {
		t.Errorf("the dead connection is still held (%d connections)", got)
	}
}

// discard must drop the failed connection so the next request dials.
func TestDiscardRemovesTheConnection(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	h2Conn := server.connect(t, rt)

	rt.discard(server.addr, h2Conn)

	if got := rt.heldConnections(); got != 0 {
		t.Errorf("discard left %d connections behind", got)
	}
	if !eventuallyClosed(t, h2Conn) {
		t.Error("discard did not close the connection it dropped")
	}
}

// A request that fails late must not evict the replacement another request has
// already established, or one failure would knock out its successors in turn.
func TestDiscardKeepsTheReplacementConnection(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	failed := server.connect(t, rt)
	replacement := server.connect(t, rt)

	rt.discard(server.addr, failed)

	rt.mu.Lock()
	held := rt.conns[server.addr]
	rt.mu.Unlock()
	if held != replacement {
		t.Fatal("discarding the failed connection also dropped the replacement")
	}
	server.get(t, rt)
}

// An evicted credential must not leave its connection open.
func TestEvictionClosesTheConnections(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	h2Conn := server.connect(t, rt)

	rt.closeIdleConnections()

	if got := rt.heldConnections(); got != 0 {
		t.Errorf("eviction left %d connections held", got)
	}
	if !eventuallyClosed(t, h2Conn) {
		t.Error("eviction did not close the connection")
	}
}

// Eviction from the LRU is what triggers that close in production, so the cache
// has to be wired to it.
func TestEvictionFromTheCacheClosesConnections(t *testing.T) {
	server := startH2Server(t)
	evicted := testCachedTransport("conn-reuse-evict-me", "")
	h2Conn := server.connect(t, evicted)

	for i := 0; i < 256+1; i++ {
		testCachedTransport(fmt.Sprintf("conn-reuse-filler-%d", i), "")
	}

	if !eventuallyClosed(t, h2Conn) {
		t.Error("a credential evicted from the cache left its connection open")
	}
}

// readStream reads a /stream response to the end and reports the chunks seen.
func readStream(resp *http.Response) (int, error) {
	defer func() { _ = resp.Body.Close() }()
	scanner := bufio.NewScanner(resp.Body)
	chunks := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			chunks++
		}
	}
	return chunks, scanner.Err()
}

// One request failing on the shared connection must not cut the credential's
// other requests running on it. The v7 patch closed the connection in discard,
// which surfaced in production as bursts of "client connection force closed
// via ClientConn.Close" on one credential (up to 46 streams in one second).
func TestDiscardLetsTheOtherStreamsFinish(t *testing.T) {
	server := startH2Server(t)
	rt := testRoundTripper(t)
	h2Conn := server.connect(t, rt)

	resp, err := server.request(rt, "/stream")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	rt.discard(server.addr, h2Conn) // another request failed on this connection

	chunks, err := readStream(resp)
	if err != nil {
		t.Fatalf("the running stream was cut when the connection was discarded: %v", err)
	}
	if chunks != 5 {
		t.Errorf("received %d streamed chunks, want 5", chunks)
	}
	if !eventuallyClosed(t, h2Conn) {
		t.Error("the discarded connection was not closed once its streams finished")
	}
}

// A connection that can take no new request (every stream the peer allows is
// in use; the same holds after the peer's GOAWAY) is retired for the next
// request, but the streams already on it run to the end.
func TestFullConnectionIsRetiredWithoutCuttingItsStreams(t *testing.T) {
	server := startH2ServerWithStreams(t, 1)
	rt := testRoundTripper(t)
	h2Conn := server.connect(t, rt)
	server.get(t, rt) // learn the peer's SETTINGS

	resp, err := server.request(rt, "/stream")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	// The connection is full now; the next request must not reuse it. Its
	// replacement cannot be dialled here (plaintext server), which is expected.
	if _, errNext := server.request(rt, "/"); errNext == nil || !strings.Contains(errNext.Error(), "utls:") {
		t.Fatalf("second request: %v, want a failure from dialling a replacement", errNext)
	}
	if got := rt.heldConnections(); got != 0 {
		t.Errorf("the full connection is still handed out (%d held)", got)
	}
	chunks, err := readStream(resp)
	if err != nil {
		t.Fatalf("the running stream was cut when its full connection was retired: %v", err)
	}
	if chunks != 5 {
		t.Errorf("received %d streamed chunks, want 5", chunks)
	}
	if !eventuallyClosed(t, h2Conn) {
		t.Error("the retired connection was not closed once its stream finished")
	}
}
