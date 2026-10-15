package adapter

import (
	"container/list"
	"errors"
	"net/http"
	"net/url"
	"sync"
)

// CPA keeps one browser TLS/HTTP2 session per credential and proxy. This
// bounded equivalent is local to one private adapter and never keys by token.
type transportPool struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List
	closed  bool
}
type transportEntry struct {
	key       string
	transport *utlsRoundTripper
}

func newTransportPool() *transportPool {
	return &transportPool{entries: map[string]*list.Element{}, order: list.New()}
}
func (p *transportPool) get(identity, proxyURL string) (http.RoundTripper, error) {
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil || u.Hostname() == "" || u.Fragment != "" || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("invalid selected-account proxy")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("adapter is shutting down")
	}
	key := identity + "\x00" + proxyURL
	if existing := p.entries[key]; existing != nil {
		p.order.MoveToFront(existing)
		return existing.Value.(transportEntry).transport, nil
	}
	transport, err := newUtlsRoundTripper(proxyURL)
	if err != nil {
		return nil, err
	}
	p.entries[key] = p.order.PushFront(transportEntry{key, transport})
	if p.order.Len() > 256 {
		last := p.order.Back()
		entry := last.Value.(transportEntry)
		delete(p.entries, entry.key)
		p.order.Remove(last)
		entry.transport.closeIdleConnections()
	}
	return transport, nil
}
func (p *transportPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, entry := range p.entries {
		entry.Value.(transportEntry).transport.closeIdleConnections()
	}
	p.entries = map[string]*list.Element{}
	p.order.Init()
}
