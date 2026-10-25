package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// providerDial is the dialer under a forced address family; tests replace it. Its timeout
// and keep-alive are those of http.DefaultTransport.
var providerDial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext

// newNetworkTransport clones the default transport (proxy, TLS and HTTP/2 stay) and dials
// with the configured network. With an HTTP(S) proxy the family applies to the dial to the
// proxy, because that is the connection this transport opens.
func newNetworkTransport(network string, dial func(ctx context.Context, network, address string) (net.Conn, error)) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, fmt.Errorf("provider_network.network is %q: %w", network, err)
		}
		return conn, nil
	}
	return transport
}
