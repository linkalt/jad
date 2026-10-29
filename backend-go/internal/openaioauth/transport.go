package openaioauth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Transport adds the OAuth credential headers to each request and passes the request
// on unchanged otherwise: the URL, the method and the body are the caller's, so the
// request is the same Responses request an API-key session sends.
type Transport struct {
	// Base sends the request; nil uses http.DefaultTransport.
	Base http.RoundTripper
	// Source supplies the credential for each request.
	Source *Source
	// BaseURL is the upstream the credential is bound to: a request whose scheme and
	// host differ (a redirect hop, for one) is refused rather than sent with it.
	BaseURL string
}

// headers this transport owns. Anything the caller set under these names is replaced,
// so the SDK's own Authorization header (built from a placeholder key) never leaves.
var ownedHeaders = []string{"Authorization", "Chatgpt-Account-Id", "Openai-Beta", "X-Openai-Fedramp"}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.Source == nil {
		closeBody(request)
		return nil, errors.New("openaioauth: transport has no credential source")
	}
	if err := t.checkOrigin(request); err != nil {
		closeBody(request)
		return nil, err
	}
	credential, err := t.Source.Load()
	if err != nil {
		// A RoundTripper closes the request body on every path, errors included.
		closeBody(request)
		return nil, err
	}
	// A RoundTripper must not modify the request it is given.
	clone := request.Clone(request.Context())
	for _, name := range ownedHeaders {
		clone.Header.Del(name)
	}
	clone.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	clone.Header.Set("chatgpt-account-id", credential.AccountID)
	if credential.FedRAMP {
		clone.Header.Set("X-OpenAI-Fedramp", "true")
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// checkOrigin fails closed: an unparsable or empty BaseURL matches nothing.
func (t *Transport) checkOrigin(request *http.Request) error {
	base, err := url.Parse(t.BaseURL)
	if err != nil || base.Host == "" || !SameOrigin(base, request.URL) {
		return fmt.Errorf("openaioauth: refusing to send the credential to %s: not the configured upstream origin", request.URL.Redacted())
	}
	return nil
}

// SameOrigin reports whether two URLs have the same scheme and host (port included).
func SameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func closeBody(request *http.Request) {
	if request.Body != nil {
		_ = request.Body.Close()
	}
}
