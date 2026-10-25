package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"jad/backend-go/internal/openaioauth"
	"jad/backend-go/internal/sessionfs"
)

func TestNetworkTransportDialsTheConfiguredFamily(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		var got, address string
		transport := newNetworkTransport(network, func(_ context.Context, n, a string) (net.Conn, error) {
			got, address = n, a
			return nil, errors.New("refused")
		})
		_, err := transport.DialContext(context.Background(), "tcp", "provider.example:443")
		if got != network || address != "provider.example:443" {
			t.Errorf("%s: dialed %q %q", network, got, address)
		}
		if err == nil || !strings.Contains(err.Error(), "provider_network.network") || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: error %v must name provider_network and keep the cause", network, err)
		}
	}
}

func TestNetworkTransportKeepsTheDefaultTransportSettings(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport)
	clone := newNetworkTransport("tcp4", nil)
	if !clone.ForceAttemptHTTP2 || clone.Proxy == nil || clone.TLSHandshakeTimeout != base.TLSHandshakeTimeout || clone.IdleConnTimeout != base.IdleConnTimeout {
		t.Fatalf("clone lost default settings: %+v", clone)
	}
	if clone == base {
		t.Fatal("the default transport must not be modified")
	}
}

func TestUnsetProviderNetworkLeavesTheDefaultClientUntouched(t *testing.T) {
	for _, network := range []string{"", "tcp"} {
		config := sessionfs.SessionConfig{ProviderNetwork: sessionfs.ProviderNetworkSettings{Network: network}}
		if client := providerHTTPClient(config, nil); client != nil {
			t.Fatalf("network %q: client = %+v, want nil", network, client)
		}
	}
	forced := providerHTTPClient(sessionfs.SessionConfig{ProviderNetwork: sessionfs.ProviderNetworkSettings{Network: "tcp6"}}, nil)
	if forced == nil || forced.Transport == http.DefaultTransport {
		t.Fatal("tcp6 must build its own transport")
	}
}

func TestDebugWrapperDialsThroughTheNetworkTransport(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(key, "")
	}
	var dialed []string
	previous := providerDial
	providerDial = func(_ context.Context, network, _ string) (net.Conn, error) {
		dialed = append(dialed, network)
		return nil, errors.New("refused")
	}
	t.Cleanup(func() { providerDial = previous })
	config := sessionfs.SessionConfig{ProviderNetwork: sessionfs.ProviderNetworkSettings{Network: "tcp6"}}
	config.HTTPDebug.Enabled = true
	config.HTTPDebug.MaxBodyBytes = 1024
	config.HTTPDebug.MaxTotalBytes = 1 << 20
	// The debug logger writes in the background, so t.TempDir's strict cleanup would race it.
	logDir, err := os.MkdirTemp("", "provider-network-debug")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	config.StoragePaths.HTTPDebug = logDir
	client := providerHTTPClient(config, nil)
	if _, ok := client.Transport.(*http.Transport); ok {
		t.Fatalf("the debug logger must wrap the network transport, got %T", client.Transport)
	}
	_, err = client.Get("http://provider.example/")
	if len(dialed) != 1 || dialed[0] != "tcp6" || err == nil || !strings.Contains(err.Error(), "provider_network.network") {
		t.Fatalf("dialed %v, err %v", dialed, err)
	}
}

func TestOAuthTransportUsesTheNetworkTransportAsBase(t *testing.T) {
	config := sessionfs.SessionConfig{ProviderNetwork: sessionfs.ProviderNetworkSettings{Network: "tcp4"}}
	config.Model.AuthMode = "openai_oauth"
	client := providerHTTPClient(config, nil)
	oauth, ok := client.Transport.(*openaioauth.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.Transport)
	}
	base, ok := oauth.Base.(*http.Transport)
	if !ok || base == http.DefaultTransport {
		t.Fatalf("oauth base = %T, want the forced-family transport", oauth.Base)
	}
}
