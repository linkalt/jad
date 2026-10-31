package anthropicadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"jad/backend-go/internal/fserr"
)

// credentialFile is the part of the claude-auth file that is read. Unknown keys are ignored
// because the tool that writes the file owns its layout. The adapter never writes the file:
// sign-in and refresh belong to claude-auth, so there is no second writer to race with.
type credentialFile struct {
	AccessToken string `json:"access_token"`
	ExpiresAt   string `json:"expires_at"`
}

// credentialError is a credential that cannot be used. Error names the file and the reason
// and never contains token text; it is for the server log. Message omits the path and is the
// only text a caller may see.
type credentialError struct {
	Path   string
	Reason string
}

func (e *credentialError) Error() string {
	return fmt.Sprintf("Claude OAuth credential %s: %s", e.Path, e.Reason)
}

func (e *credentialError) Message() string {
	return "Claude OAuth credential " + e.Reason
}

// loadCredential reads the credential file. It reads the file on every call, so a token that
// claude-auth refreshed is used by the next request without a restart. A token whose stored
// expiry is within margin of now is refused before the request is sent.
func loadCredential(path string, margin time.Duration, now time.Time) (string, *credentialError) {
	fail := func(reason string) (string, *credentialError) {
		return "", &credentialError{Path: path, Reason: reason}
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fail("file not found; sign in with claude-auth")
		}
		return fail("cannot be read: " + fserr.UnwrapPathError(err).Error())
	}
	if !info.Mode().IsRegular() {
		return fail("is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail("cannot be read: " + fserr.UnwrapPathError(err).Error())
	}
	var file credentialFile
	if err := json.Unmarshal(data, &file); err != nil {
		// The decoder's text can quote file content, so it is not repeated.
		return fail("is not valid JSON of the expected shape")
	}
	if file.AccessToken == "" {
		return fail("has no access token; sign in with claude-auth")
	}
	// Access tokens are opaque, so the expiry in the file is the only signal. A file without a
	// readable expiry is refused: claude-auth always writes one.
	expires, err := time.Parse(time.RFC3339, file.ExpiresAt)
	if err != nil {
		return fail("has no valid expires_at; sign in with claude-auth")
	}
	if !expires.After(now.Add(margin)) {
		return fail(fmt.Sprintf("access token expired at %s; refresh it with claude-auth refresh", expires.UTC().Format(time.RFC3339)))
	}
	return file.AccessToken, nil
}

// ExpandCredentialPath applies the rule model.json uses for oauth_auth_file: the path must be
// absolute or start with "~/".
func ExpandCredentialPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	switch {
	case path == "":
		return "", errors.New("must be a non-empty path")
	case strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		return filepath.Clean(filepath.Join(home, path[2:])), nil
	case filepath.IsAbs(path):
		return filepath.Clean(path), nil
	}
	return "", fmt.Errorf("must be absolute or start with ~/, got %q", path)
}

// validHeaderName reports whether name is an RFC 9110 token.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

// upstreamToken returns the credential to send upstream: the token in the credential file when
// one is configured, otherwise the one the caller sent.
func (h *Handler) upstreamToken(r *http.Request) (string, *apiError) {
	upstream := &h.config.Upstream
	if upstream.credentialPath == "" {
		token := apiKeyFrom(r)
		if token == "" {
			return "", unauthorized("send the Anthropic credential (an API key or an access token) as a Bearer token or x-api-key header")
		}
		return token, nil
	}
	token, err := loadCredential(upstream.credentialPath, time.Duration(upstream.ExpiryMarginMS)*time.Millisecond, h.now())
	if err != nil {
		h.logger.Warn("credential unusable", "error", err.Error())
		return "", &apiError{Status: http.StatusUnauthorized, Type: "authentication_error", Code: "credential_unusable", Message: err.Message()}
	}
	return token, nil
}
