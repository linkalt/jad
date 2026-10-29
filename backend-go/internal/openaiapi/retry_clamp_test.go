package openaiapi

import (
	"crypto/x509"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestIsRetryableErrorPermanentTransportFailures(t *testing.T) {
	permanent := map[string]error{
		"unknown authority":  &url.Error{Op: "Post", URL: "https://x/", Err: x509.UnknownAuthorityError{}},
		"invalid cert":       &url.Error{Op: "Post", URL: "https://x/", Err: x509.CertificateInvalidError{Reason: x509.Expired}},
		"hostname":           &url.Error{Op: "Post", URL: "https://x/", Err: x509.HostnameError{Host: "x"}},
		"unsupported scheme": &url.Error{Op: "Post", URL: "ftp://x/", Err: errString("unsupported protocol scheme \"ftp\"")},
	}
	for name, err := range permanent {
		if isRetryableError(err) {
			t.Errorf("%s must not be retried", name)
		}
	}
	transient := map[string]error{
		"dial":    &url.Error{Op: "Post", URL: "https://x/", Err: &net.OpError{Op: "dial", Err: errString("connection refused")}},
		"timeout": &url.Error{Op: "Post", URL: "https://x/", Err: &netTimeout{}},
	}
	for name, err := range transient {
		if !isRetryableError(err) {
			t.Errorf("%s must still be retried", name)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestRetryAfterClampsHugeValuesWithoutOverflow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, header := range []map[string]string{
		{"Retry-After-Ms": "1e300"}, {"Retry-After-Ms": "1e18"},
		{"Retry-After": "1e300"}, {"Retry-After": "1e18"}, {"Retry-After": "9223372036"},
	} {
		h := http.Header{}
		for k, v := range header {
			h.Set(k, v)
		}
		got, ok := retryAfter(h, now)
		if !ok || got < time.Hour {
			t.Errorf("%v: got (%v, %v), want a large non-negative wait", header, got, ok)
		}
		delay := retryDelay(RetryConfig{MaxRetries: 3, InitialDelay: time.Second, MaxDelay: 30 * time.Second}, 0, 429, h, now, func() float64 { return 0 })
		if delay != 30*time.Second {
			t.Errorf("%v: delay = %v, want the 30s cap", header, delay)
		}
	}
	h := http.Header{}
	h.Set("Retry-After-Ms", "250")
	if got, _ := retryAfter(h, now); got != 250*time.Millisecond {
		t.Errorf("small value changed: %v", got)
	}
}
