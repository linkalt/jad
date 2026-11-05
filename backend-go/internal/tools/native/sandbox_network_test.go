package native

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// proxyClient is an HTTP client whose proxy is a forwarder like the one inside the
// sandbox: a loopback listener that hands each connection to the proxy socket.
func proxyClient(t *testing.T, socket string, transport *http.Transport) *http.Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go forwardSandboxConnections(listener, socket)
	proxyURL, _ := url.Parse("http://" + listener.Addr().String())
	if transport == nil {
		transport = &http.Transport{}
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	return &http.Client{Transport: transport}
}

func TestSandboxProxyLetsThroughOnlyTheAllowedHosts(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached "+r.URL.Path)
	}))
	defer target.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tls reached")
	}))
	defer secure.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))
	_, securePort, _ := net.SplitHostPort(strings.TrimPrefix(secure.URL, "https://"))

	socket, err := sandboxNetworkProxy([]string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := sandboxNetworkProxy([]string{"localhost"})
	if err != nil || again != socket {
		t.Fatalf("the same allow list started a second proxy: %s %s %v", socket, again, err)
	}

	client := proxyClient(t, socket, nil)
	response, err := client.Get("http://localhost:" + port + "/page")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "reached /page" {
		t.Fatalf("allowed host: %d %s", response.StatusCode, body)
	}

	response, err = client.Get("http://127.0.0.1:" + port + "/page")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "not in sandbox.network_allow") {
		t.Fatalf("refused host: %d %s", response.StatusCode, body)
	}

	// HTTPS goes through a CONNECT tunnel, end to end encrypted.
	// The test certificate names example.com, not localhost.
	tlsTransport := secure.Client().Transport.(*http.Transport).Clone()
	tlsTransport.TLSClientConfig.ServerName = "example.com"
	tlsClient := proxyClient(t, socket, tlsTransport)
	response, err = tlsClient.Get("https://localhost:" + securePort + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "tls reached" {
		t.Fatalf("tunnel: %d %s", response.StatusCode, body)
	}
	if _, err := tlsClient.Get("https://127.0.0.1:" + securePort + "/"); err == nil {
		t.Fatal("a CONNECT to a host outside the list went through")
	}
}

func TestSandboxHostAllowedMatchesSubdomainsOnly(t *testing.T) {
	allowed := []string{"example.com", "pypi.org"}
	for host, want := range map[string]bool{
		"example.com": true, "api.example.com:443": true, "EXAMPLE.com.": true, "files.pypi.org": true,
		"badexample.com": false, "example.com.evil.net": false, "127.0.0.1:80": false, "": false,
	} {
		if got := sandboxHostAllowed(host, allowed); got != want {
			t.Errorf("sandboxHostAllowed(%q) = %t, want %t", host, got, want)
		}
	}
}

func TestSandboxNetForwardRunsTheChildAndKeepsItsStatus(t *testing.T) {
	socket, err := sandboxNetworkProxy([]string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	listener.Close()
	var stderr bytes.Buffer
	if status := RunSandboxNetForward([]string{"--socket", socket, "--port", port, "--argv0", "named", "--", "/bin/sh", "-c", `[ "$0" = named ] && exit 7`}, &stderr); status != 7 {
		t.Fatalf("status = %d, stderr = %s", status, stderr.String())
	}
	if status := RunSandboxNetForward([]string{"--port", port, "--", "/bin/true"}, &stderr); status != 126 {
		t.Fatalf("a forwarder without a socket ran: %d", status)
	}
}

func TestBwrapArgumentsWithNetworkAllowCutTheNetworkAndStartTheForwarder(t *testing.T) {
	sandbox := &sessionfs.SandboxSettings{Backend: sessionfs.SandboxBackendBwrap, Executable: "bwrap", NetworkAllow: []string{"pypi.org"}, ProxyPort: 3128}
	place := sandboxPlace{workspace: t.TempDir(), cwd: t.TempDir()}
	got := bwrapArguments(sandbox, place, sandboxExpander(place), &sandboxNetwork{socket: "/tmp/jad-netproxy-x/proxy.sock", forwarder: "/usr/local/bin/jad"}, "bash", "/usr/bin/bash", []string{"-c", "pip install x"})
	joined := strings.Join(got, " ")
	if slices.Contains(got, "--share-net") {
		t.Fatalf("network_allow kept the network: %s", joined)
	}
	for _, want := range []string{
		"--bind /tmp/jad-netproxy-x/proxy.sock /tmp/jad-netproxy-x/proxy.sock",
		"--setenv HTTPS_PROXY http://127.0.0.1:3128", "--setenv http_proxy http://127.0.0.1:3128", "--unsetenv NO_PROXY",
		"-- /usr/local/bin/jad " + SandboxNetForwardCommand + " --socket /tmp/jad-netproxy-x/proxy.sock --port 3128 --argv0 bash -- /usr/bin/bash -c pip install x",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("arguments miss %q:\n%s", want, joined)
		}
	}
	if strings.Index(joined, "--setenv") > strings.Index(joined, " -- ") {
		t.Errorf("an environment option follows the command:\n%s", joined)
	}
}
