package native

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// sandbox.network_allow: the sandbox keeps no network of its own (bwrap --unshare-net). jad
// runs one filtering proxy per allow list on a Unix socket outside the sandbox. Inside it,
// jad's own binary (SandboxNetForwardCommand) listens on loopback proxy_port, hands every
// connection to that socket, and starts the child with HTTP_PROXY and HTTPS_PROXY naming the
// port. The proxy lets through CONNECT and plain HTTP requests to the allowed hosts and their
// subdomains and answers 403 to anything else, so the allowed hosts are the only way out.

// SandboxNetForwardCommand is the hidden jad subcommand that runs inside the sandbox.
const SandboxNetForwardCommand = "__sandbox-net-forward"

// sandboxProxyDialTimeout bounds the proxy's connection to an allowed host.
const sandboxProxyDialTimeout = 30 * time.Second

var sandboxProxies = struct {
	sync.Mutex
	sockets map[string]string
}{sockets: map[string]string{}}

// sandboxNetworkProxy returns the socket of the proxy for hosts, starting it on first use.
// It runs for the life of the process.
func sandboxNetworkProxy(hosts []string) (string, error) {
	key := strings.Join(hosts, ",")
	sandboxProxies.Lock()
	defer sandboxProxies.Unlock()
	if socket, running := sandboxProxies.sockets[key]; running {
		return socket, nil
	}
	digest := sha256.Sum256([]byte(key))
	directory, err := os.MkdirTemp("", "jad-netproxy-"+hex.EncodeToString(digest[:4])+"-")
	if err != nil {
		return "", fmt.Errorf("make the sandbox proxy folder: %w", err)
	}
	socket := filepath.Join(directory, "proxy.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return "", fmt.Errorf("start the sandbox proxy: %w", err)
	}
	allowed := append([]string(nil), hosts...)
	server := &http.Server{Handler: sandboxProxyHandler(allowed), ReadHeaderTimeout: sandboxProxyDialTimeout}
	go func() { _ = server.Serve(listener) }()
	sandboxProxies.sockets[key] = socket
	return socket, nil
}

// sandboxHostAllowed reports whether host (a name or name:port) is one of allowed or a
// subdomain of one.
func sandboxHostAllowed(host string, allowed []string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, domain := range allowed {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func sandboxProxyHandler(allowed []string) http.Handler {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: sandboxProxyDialTimeout}).DialContext}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if r.Method != http.MethodConnect && r.URL.Host != "" {
			host = r.URL.Host
		}
		if !sandboxHostAllowed(host, allowed) {
			http.Error(w, "jad sandbox: "+host+" is not in sandbox.network_allow", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodConnect {
			sandboxProxyTunnel(w, r)
			return
		}
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			http.Error(w, "jad sandbox: the proxy takes CONNECT or absolute http:// requests", http.StatusBadRequest)
			return
		}
		outgoing := r.Clone(r.Context())
		outgoing.RequestURI = ""
		outgoing.Header.Del("Proxy-Connection")
		outgoing.Header.Del("Proxy-Authorization")
		response, err := transport.RoundTrip(outgoing)
		if err != nil {
			http.Error(w, "jad sandbox: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	})
}

func sandboxProxyTunnel(w http.ResponseWriter, r *http.Request) {
	upstream, err := net.DialTimeout("tcp", r.Host, sandboxProxyDialTimeout)
	if err != nil {
		http.Error(w, "jad sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "jad sandbox: the proxy cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		// Bytes the client sent after the CONNECT line are already in the buffer.
		if buffered != nil && buffered.Reader.Buffered() > 0 {
			pending, _ := buffered.Reader.Peek(buffered.Reader.Buffered())
			_, _ = upstream.Write(pending)
		}
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	_, _ = io.Copy(client, upstream)
	client.Close()
	upstream.Close()
}

func closeWrite(connection net.Conn) {
	if tcp, ok := connection.(interface{ CloseWrite() error }); ok {
		_ = tcp.CloseWrite()
		return
	}
	_ = connection.Close()
}

// sandboxProxyEnvironment is what bwrap sets for the child: every common spelling of the
// proxy variables, and no exceptions.
func sandboxProxyEnvironment(port int64) []string {
	address := "http://127.0.0.1:" + strconv.FormatInt(port, 10)
	var arguments []string
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		arguments = append(arguments, "--setenv", name, address)
	}
	return append(arguments, "--unsetenv", "NO_PROXY", "--unsetenv", "no_proxy")
}

// RunSandboxNetForward is SandboxNetForwardCommand: it listens on 127.0.0.1:port, forwards
// each connection to the proxy socket, runs the child (argv after "--", argv0 as its name)
// with the standard streams, and returns the child's exit status (128 + signal for a
// signal). It runs inside the sandbox, where loopback is the sandbox's own.
func RunSandboxNetForward(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet(SandboxNetForwardCommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "proxy socket")
	port := flags.Int("port", 0, "loopback port")
	argv0 := flags.String("argv0", "", "the child's name")
	if err := flags.Parse(args); err != nil {
		return 126
	}
	child := flags.Args()
	if *socket == "" || *port <= 0 || len(child) == 0 {
		fmt.Fprintln(stderr, "jad sandbox: the network forwarder needs --socket, --port and a program")
		return 126
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(*port))
	if err != nil {
		fmt.Fprintf(stderr, "jad sandbox: cannot listen on the proxy port %d (setting: sandbox.proxy_port): %v\n", *port, err)
		return 126
	}
	defer listener.Close()
	go forwardSandboxConnections(listener, *socket)
	command := exec.Command(child[0], child[1:]...)
	if *argv0 != "" {
		command.Args[0] = *argv0
	}
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, stderr
	if err := command.Start(); err != nil {
		fmt.Fprintf(stderr, "jad sandbox: %v\n", err)
		return 127
	}
	// Signals sent to the forwarder (a stop of the call) reach the child.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for received := range signals {
			_ = command.Process.Signal(received)
		}
	}()
	err = command.Wait()
	signal.Stop(signals)
	close(signals)
	return childExitStatus(command, err)
}

func forwardSandboxConnections(listener net.Listener, socket string) {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer connection.Close()
			upstream, err := (&net.Dialer{Timeout: sandboxProxyDialTimeout}).DialContext(context.Background(), "unix", socket)
			if err != nil {
				_, _ = connection.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
				return
			}
			defer upstream.Close()
			go func() {
				_, _ = io.Copy(upstream, connection)
				closeWrite(upstream)
			}()
			_, _ = io.Copy(connection, bufio.NewReader(upstream))
		}()
	}
}

func childExitStatus(command *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	return 1
}
