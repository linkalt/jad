package remote

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// HeaderHops carries the number of remote hops the request has made so far.
	HeaderHops = "X-Jad-Hops"
	// DefaultMaxHops is the hop limit when nothing configures another.
	DefaultMaxHops = 3
	// DefaultRetryBackoff is the pause before the first retry; it doubles each time.
	DefaultRetryBackoff = 200 * time.Millisecond
	errorBodyBytes      = 512
	unixHost            = "jad.invalid"
	redacted            = "[REDACTED]"
)

// ErrResponseTooLarge is a response body over the entry's max_response_bytes.
var ErrResponseTooLarge = errors.New("remote response is larger than max_response_bytes")

// ErrRedirect is any 3xx answer: redirects are never followed, so the token never leaves the
// configured host.
var ErrRedirect = errors.New("remote redirected the request; redirects are not followed")

// Error is an error answer of the remote: its status, its code and a bounded message.
type Error struct {
	Remote  string
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	text := fmt.Sprintf("remote %s answered %d", e.Remote, e.Status)
	if e.Code != "" {
		text += " " + e.Code
	}
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

// Options are what a Client takes besides the entry. The zero value is the production one.
type Options struct {
	// Environment reads token_env; nil is os.Getenv.
	Environment func(string) string
	// RetryBackoff is the pause before the first retry; zero is DefaultRetryBackoff.
	RetryBackoff time.Duration
}

// Client calls one remote. It is safe for concurrent use; Close drops its idle connections.
type Client struct {
	entry     Entry
	http      *http.Client
	transport *http.Transport
	base      string
	options   Options
	slots     chan struct{}
}

// NewClient builds the client of one entry: TLS verified against the system roots plus
// ca_file, no proxy, no redirects, a unix dialer for unix:// urls.
func NewClient(entry Entry, options Options) (*Client, error) {
	if options.Environment == nil {
		options.Environment = os.Getenv
	}
	if options.RetryBackoff <= 0 {
		options.RetryBackoff = DefaultRetryBackoff
	}
	transport := &http.Transport{
		// An explicit nil: HTTPS_PROXY and friends never apply to a remote.
		Proxy:                 nil,
		MaxIdleConns:          entry.MaxInFlight,
		MaxIdleConnsPerHost:   entry.MaxInFlight,
		IdleConnTimeout:       time.Duration(entry.Timeout) * time.Millisecond,
		TLSHandshakeTimeout:   time.Duration(entry.Timeout) * time.Millisecond,
		ResponseHeaderTimeout: 0,
		ForceAttemptHTTP2:     entry.Unix == "",
	}
	base := entry.URL
	if entry.Unix != "" {
		socket := entry.Unix
		dialer := &net.Dialer{Timeout: time.Duration(entry.Timeout) * time.Millisecond}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			info, err := os.Lstat(socket)
			if err != nil {
				return nil, fmt.Errorf("remote %s: socket %s is not reachable", entry.Name, socket)
			}
			if info.Mode()&os.ModeSocket == 0 {
				return nil, fmt.Errorf("remote %s: %s is not a unix socket", entry.Name, socket)
			}
			if err := checkSocketOwner(socket, info); err != nil {
				return nil, fmt.Errorf("remote %s: %w", entry.Name, err)
			}
			return dialer.DialContext(ctx, "unix", socket)
		}
		base = "http://" + unixHost
	} else {
		config := &tls.Config{MinVersion: tls.VersionTLS12}
		if entry.CAFile != "" {
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			pem, err := os.ReadFile(entry.CAFile)
			if err != nil {
				return nil, fmt.Errorf("remote %s: ca_file %s cannot be read", entry.Name, entry.CAFile)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("remote %s: ca_file %s holds no PEM certificate", entry.Name, entry.CAFile)
			}
			config.RootCAs = pool
		}
		transport.TLSClientConfig = config
	}
	c := &Client{
		entry:     entry,
		transport: transport,
		base:      base,
		options:   options,
		slots:     make(chan struct{}, entry.MaxInFlight),
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	return c, nil
}

// Entry is the remote the client reaches.
func (c *Client) Entry() Entry { return c.entry }

// Close drops idle connections; in-flight requests end with their context.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// Request is one call. Body is JSON-encoded when set; out receives the JSON answer.
type Request struct {
	Method string
	// Path is /api/remote/v1/... with its query.
	Path string
	Body any
	// Hops is the hop count to send, the caller's own plus one.
	Hops int
	// Wait is how long the remote may hold the request open (a status poll); it extends the
	// per-request timeout.
	Wait time.Duration
	// Retry says a failed request may be sent again: reads, and sends that carry a request id.
	Retry bool
	// Raw, when set, receives the body bytes instead of JSON decoding into out.
	Raw *[]byte
}

// Do sends the request and decodes the JSON answer into out (nil ignores it). A 2xx is
// success; any other status is an *Error. Nothing it returns contains the token.
func (c *Client) Do(ctx context.Context, request Request, out any) error {
	token, err := c.entry.Token(c.options.Environment)
	if err != nil {
		return err
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	var payload []byte
	if request.Body != nil {
		if payload, err = json.Marshal(request.Body); err != nil {
			return fmt.Errorf("remote %s: encode request: %v", c.entry.Name, err)
		}
	}
	attempts := 1
	if request.Retry {
		attempts += c.entry.Retries
	}
	backoff := c.options.RetryBackoff
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			backoff *= 2
		}
		retryable, err := c.once(ctx, token, request, payload, out)
		if err == nil {
			return nil
		}
		last = scrub(err, token)
		if !retryable || ctx.Err() != nil {
			break
		}
	}
	return last
}

// once makes one attempt and says whether a failure is worth another.
func (c *Client) once(ctx context.Context, token string, request Request, payload []byte, out any) (retryable bool, err error) {
	timeout := time.Duration(c.entry.Timeout)*time.Millisecond + request.Wait
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	httpRequest, err := http.NewRequestWithContext(attemptCtx, request.Method, c.base+request.Path, body)
	if err != nil {
		return false, fmt.Errorf("remote %s: build request: %v", c.entry.Name, err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set(HeaderHops, fmt.Sprint(request.Hops))
	if payload != nil {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(httpRequest)
	if err != nil {
		return true, fmt.Errorf("remote %s: %s", c.entry.Name, networkText(err))
	}
	defer response.Body.Close()
	limit := c.entry.MaxResponseBytes
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return true, fmt.Errorf("remote %s: reading the answer failed: %s", c.entry.Name, networkText(err))
	}
	if int64(len(data)) > limit {
		return false, fmt.Errorf("remote %s: %w (%d bytes)", c.entry.Name, ErrResponseTooLarge, limit)
	}
	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return false, fmt.Errorf("remote %s: %w", c.entry.Name, ErrRedirect)
	case response.StatusCode >= 200 && response.StatusCode < 300:
	default:
		failure := errorOf(c.entry.Name, response.StatusCode, data)
		return response.StatusCode == http.StatusBadGateway || response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusGatewayTimeout, failure
	}
	if request.Raw != nil {
		*request.Raw = data
		return false, nil
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("remote %s: the answer is not the expected JSON", c.entry.Name)
	}
	return false, nil
}

// errorOf reads {"error":"...","code":"..."} of a failed answer; the text is untrusted, so it is
// cut and stripped of control characters.
func errorOf(name string, status int, data []byte) *Error {
	failure := &Error{Remote: name, Status: status}
	var parsed struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(data, &parsed) == nil {
		failure.Code = safeText(parsed.Code, 64)
		failure.Message = safeText(firstNonEmpty(parsed.Error, parsed.Message), errorBodyBytes)
	}
	return failure
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// safeText keeps printable text up to limit bytes without splitting a rune.
func safeText(text string, limit int) string {
	var builder strings.Builder
	for _, r := range text {
		if r < 0x20 || r == 0x7f {
			r = ' '
		}
		if builder.Len()+utf8.RuneLen(r) > limit {
			break
		}
		builder.WriteRune(r)
	}
	return strings.TrimSpace(builder.String())
}

// networkText names a transport failure without the url or header values.
func networkText(err error) string {
	var certificate x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "the request timed out"
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	case errors.As(err, &certificate), errors.As(err, &hostname), errors.As(err, &invalid):
		return "TLS certificate verification failed: " + safeText(err.Error(), 200)
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		return "cannot connect (" + safeText(operation.Op, 20) + ")"
	}
	var wrapped *url.Error
	if errors.As(err, &wrapped) {
		err = wrapped.Err
	}
	return "request failed: " + safeText(err.Error(), 200)
}

// scrub removes the token from an error text; the token must never leave this package.
func scrub(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	text := err.Error()
	if !strings.Contains(text, token) {
		return err
	}
	var remote *Error
	if errors.As(err, &remote) {
		copied := *remote
		copied.Message = strings.ReplaceAll(copied.Message, token, redacted)
		copied.Code = strings.ReplaceAll(copied.Code, token, redacted)
		return &copied
	}
	return errors.New(strings.ReplaceAll(text, token, redacted))
}
