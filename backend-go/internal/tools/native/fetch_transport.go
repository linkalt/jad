package native

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// fetchTransportKey identifies one pinned destination. The validated address is
// part of the key, so a pooled connection is only reused for the exact address
// that passed validateFetchURL on the current call.
type fetchTransportKey struct {
	scheme string
	host   string
	port   string
	ip     string
}

type fetchTransportEntry struct {
	transport *http.Transport
	lastUsed  time.Time
}

// fetchTransportPool keeps one keep-alive transport per pinned destination so
// repeated fetches from one host skip the TCP and TLS handshake. The zero value
// is ready to use.
type fetchTransportPool struct {
	mu      sync.Mutex
	entries map[fetchTransportKey]*fetchTransportEntry
	closed  bool
}

// get returns the transport for key, creating it when absent. Entries unused for
// longer than idle are dropped, and the least recently used entry is evicted when
// the pool already holds maxHosts (at least 1). A closed pool returns nil.
// Dropped transports are closed after the lock is released, because closing a TLS
// connection can block on a slow peer.
func (p *fetchTransportPool) get(key fetchTransportKey, idle time.Duration, maxHosts int, create func() *http.Transport) *http.Transport {
	if maxHosts < 1 {
		maxHosts = 1
	}
	var dropped []*http.Transport
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	now := time.Now()
	for existing, entry := range p.entries {
		if now.Sub(entry.lastUsed) > idle {
			dropped = append(dropped, entry.transport)
			delete(p.entries, existing)
		}
	}
	entry := p.entries[key]
	if entry != nil {
		entry.lastUsed = now
	} else {
		if p.entries == nil {
			p.entries = make(map[fetchTransportKey]*fetchTransportEntry)
		}
		for len(p.entries) >= maxHosts {
			var oldestKey fetchTransportKey
			var oldest *fetchTransportEntry
			for existing, candidate := range p.entries {
				if oldest == nil || candidate.lastUsed.Before(oldest.lastUsed) {
					oldestKey, oldest = existing, candidate
				}
			}
			dropped = append(dropped, oldest.transport)
			delete(p.entries, oldestKey)
		}
		entry = &fetchTransportEntry{transport: create(), lastUsed: now}
		p.entries[key] = entry
	}
	transport := entry.transport
	p.mu.Unlock()
	for _, old := range dropped {
		old.CloseIdleConnections()
	}
	return transport
}

// close drops every pooled transport and refuses new ones.
func (p *fetchTransportPool) close() {
	p.mu.Lock()
	p.closed = true
	dropped := make([]*http.Transport, 0, len(p.entries))
	for key, entry := range p.entries {
		dropped = append(dropped, entry.transport)
		delete(p.entries, key)
	}
	p.mu.Unlock()
	for _, old := range dropped {
		old.CloseIdleConnections()
	}
}

// newPinnedFetchTransport dials only target.Address and verifies TLS against the
// URL hostname. keepAlive false disables connection reuse.
func newPinnedFetchTransport(target validatedFetchURL, port string, keepAlive time.Duration) *http.Transport {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DisableKeepAlives:  keepAlive <= 0,
		TLSClientConfig:    &tls.Config{ServerName: target.URL.Hostname()},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if target.Address.IP.To4() != nil {
				network = "tcp4"
			} else {
				network = "tcp6"
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(target.Address.IP.String(), port))
		},
	}
	if keepAlive > 0 {
		transport.IdleConnTimeout = keepAlive
		transport.MaxIdleConns = 1
		transport.MaxIdleConnsPerHost = 1
	}
	return transport
}
