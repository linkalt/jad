package app

import (
	"errors"
	"net/http"
	"time"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/openaioauth"
	"jad/backend-go/internal/sessionfs"
)

// oauthPlaceholderKey is given to the SDK in OAuth mode. It only has to be non-empty so
// the SDK does not look for OPENAI_API_KEY; the transport replaces the Authorization
// header it produces.
const oauthPlaceholderKey = "openai-oauth"

// oauthSource builds the credential reader for a model file in openai_oauth mode.
func oauthSource(config sessionfs.SessionConfig) *openaioauth.Source {
	return &openaioauth.Source{
		Path:         config.Model.OAuthAuthFile,
		ExpiryMargin: time.Duration(config.OpenAIOAuth.ExpiryMarginMS) * time.Millisecond,
	}
}

// checkOAuthCredential fails a run before it starts when the credential file cannot be
// used, the way an unset API key environment variable does. The request path checks
// again on every request, so a token that expires mid-run still produces a clear error.
func checkOAuthCredential(config sessionfs.SessionConfig) error {
	if _, err := oauthSource(config).Load(); err != nil {
		var credentialError *openaioauth.CredentialError
		if errors.As(err, &credentialError) {
			return &httpapi.StatusError{Code: http.StatusUnprocessableEntity, Message: credentialError.Error()}
		}
		return err
	}
	return nil
}

// oauthTransport sends requests with the credential headers. It is the innermost
// transport: the HTTP debug logger wraps it and so only sees the SDK's placeholder
// Authorization header, never the access token or the account id.
func oauthTransport(config sessionfs.SessionConfig, base http.RoundTripper) http.RoundTripper {
	return &openaioauth.Transport{Base: base, Source: oauthSource(config)}
}
