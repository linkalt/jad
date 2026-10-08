package claudeauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Config is the configuration of the claude-auth command. Every key is optional.
type Config struct {
	SchemaVersion int `json:"schema_version"`
	// AuthFile is the credential file: absolute or starting with "~/".
	AuthFile     string `json:"auth_file"`
	AuthorizeURL string `json:"authorize_url"`
	TokenURL     string `json:"token_url"`
	ClientID     string `json:"client_id"`
	Scope        string `json:"scope"`
	// AuthorizeParams are extra query parameters on the authorize URL. A value given here
	// replaces the default set, it is not merged into it.
	AuthorizeParams map[string]string `json:"authorize_params"`
	// RedirectHost, LoginPort and CallbackPath form the loopback redirect URI of the login.
	RedirectHost string `json:"redirect_host"`
	LoginPort    int    `json:"login_port"`
	CallbackPath string `json:"callback_path"`
	// CopyCodeURI is the redirect URI of login --copy-code, a page that shows the code.
	CopyCodeURI    string `json:"copy_code_uri"`
	LoginTimeoutMS int    `json:"login_timeout_ms"`
	OpenBrowser    bool   `json:"open_browser"`
	HTTPTimeoutMS  int    `json:"http_timeout_ms"`
	// ExpiryMarginMS is how long before its expiry an access token counts as expired in
	// status. It should match the adapter's upstream.expiry_margin_ms.
	ExpiryMarginMS int `json:"expiry_margin_ms"`
	// KeepBackup keeps the previous credential file as <file>.bak before each rewrite.
	KeepBackup bool `json:"keep_backup"`
}

// DefaultConfig returns the configuration used when no file is given.
func DefaultConfig() Config {
	return Config{
		SchemaVersion:   1,
		AuthFile:        DefaultAuthFile,
		AuthorizeURL:    DefaultAuthorizeURL,
		TokenURL:        DefaultTokenURL,
		ClientID:        DefaultClientID,
		Scope:           DefaultScope,
		AuthorizeParams: nil, // nil means "use the default set"; see finish.
		RedirectHost:    DefaultRedirectHost,
		LoginPort:       DefaultLoginPort,
		CallbackPath:    DefaultCallbackPath,
		CopyCodeURI:     DefaultCopyCodeURI,
		LoginTimeoutMS:  DefaultLoginTimeoutMS,
		OpenBrowser:     DefaultOpenBrowser,
		HTTPTimeoutMS:   DefaultHTTPTimeoutMS,
		ExpiryMarginMS:  DefaultExpiryMarginMS,
		KeepBackup:      DefaultKeepBackup,
	}
}

// LoadConfig reads the file at path, or returns the defaults when path is empty.
// Unknown keys are rejected so a misspelled key cannot fall back to a default silently.
func LoadConfig(path string) (*Config, error) {
	config := DefaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		if decoder.More() {
			return nil, errors.New("parse config: trailing data after the JSON object")
		}
	}
	if err := config.finish(); err != nil {
		return nil, err
	}
	return &config, nil
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$|^\[[0-9A-Fa-f:]+\]$`)

// checkURL accepts an http or https URL without a fragment. Authorize and token URLs carry
// their path in the value; the authorize URL may not hold a query because the tool adds one.
func checkURL(name, value string, allowQuery bool) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.Fragment != "" || (!allowQuery && parsed.RawQuery != "") {
		return fmt.Errorf("config: %s must be an http or https URL without query or fragment, got %q", name, value)
	}
	return nil
}

func (c *Config) finish() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("config: schema_version must be 1, got %d", c.SchemaVersion)
	}
	if _, err := ExpandAuthPath(c.AuthFile); err != nil {
		return fmt.Errorf("config: auth_file: %w", err)
	}
	for name, value := range map[string]string{
		"authorize_url": c.AuthorizeURL, "token_url": c.TokenURL, "copy_code_uri": c.CopyCodeURI,
	} {
		if err := checkURL(name, value, false); err != nil {
			return err
		}
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return errors.New("config: client_id must be a non-empty string")
	}
	if strings.TrimSpace(c.Scope) == "" {
		return errors.New("config: scope must be a non-empty string")
	}
	if !hostPattern.MatchString(c.RedirectHost) {
		return fmt.Errorf("config: redirect_host must be a bare host name or address, got %q", c.RedirectHost)
	}
	if !strings.HasPrefix(c.CallbackPath, "/") || strings.ContainsAny(c.CallbackPath, "?#") {
		return fmt.Errorf("config: callback_path must start with / and hold no query or fragment, got %q", c.CallbackPath)
	}
	if c.LoginPort < 0 || c.LoginPort > 65535 {
		return fmt.Errorf("config: login_port must be 0..65535 (0 picks a free port), got %d", c.LoginPort)
	}
	for _, field := range []struct {
		name  string
		value int
		min   int
	}{{"login_timeout_ms", c.LoginTimeoutMS, 1}, {"http_timeout_ms", c.HTTPTimeoutMS, 1}, {"expiry_margin_ms", c.ExpiryMarginMS, 0}} {
		if field.value < field.min || field.value > MaxTimeoutMS {
			return fmt.Errorf("config: %s must be %d..%d, got %d", field.name, field.min, MaxTimeoutMS, field.value)
		}
	}
	if c.AuthorizeParams == nil {
		c.AuthorizeParams = defaultAuthorizeParams()
	}
	for key := range c.AuthorizeParams {
		if strings.TrimSpace(key) == "" {
			return errors.New("config: authorize_params keys must be non-empty")
		}
		if slices.Contains(reservedAuthorizeParams, key) {
			return fmt.Errorf("config: authorize_params.%s is set by the tool and cannot be overridden", key)
		}
	}
	return nil
}

// AuthFilePath returns the credential file with "~/" expanded.
func (c *Config) AuthFilePath() (string, error) { return ExpandAuthPath(c.AuthFile) }

// HTTPTimeout is the deadline of one token request.
func (c *Config) HTTPTimeout() time.Duration {
	return time.Duration(c.HTTPTimeoutMS) * time.Millisecond
}

// ExpiryMargin is the margin status applies to the access token expiry.
func (c *Config) ExpiryMargin() time.Duration {
	return time.Duration(c.ExpiryMarginMS) * time.Millisecond
}

// ExpandAuthPath applies the rule model.json uses for oauth_auth_file: the path must be
// absolute or start with "~/".
func ExpandAuthPath(path string) (string, error) {
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
