package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"jad/backend-go/internal/sessionfs"
)

const (
	defaultTimeoutMS      int64 = 30_000
	maxTimeoutMS          int64 = 10 * 60_000
	defaultMaxResultBytes       = 1_048_576
	minMaxResultBytes           = 512
	maxMaxResultBytes           = 16 * 1024 * 1024

	// Discovery budgets. The defaults follow the phase-6a measurement: a server that
	// returned 500 tools with 16 KiB input schemas cost 158 ms of CPU and 229 MB of
	// allocation per run (about 458 KB per tool, 28x its schema, because every schema is
	// copied, marshalled, compiled, cached and re-serialized into each request). A
	// server whose tools fit the defaults still loads; one oversized definition is
	// refused before it is copied, compiled or cached, and the run fails with the
	// setting named instead of the tool silently disappearing.
	defaultMaxTools            = 256
	minMaxTools                = 1
	maxMaxTools                = 2_048
	defaultMaxSchemaBytes      = 65_536
	minMaxSchemaBytes          = 512
	maxMaxSchemaBytes          = 1_048_576
	defaultMaxDescriptionBytes = 4_096
	minMaxDescriptionBytes     = 1
	maxMaxDescriptionBytes     = 65_536

	// Resource change notifications kept per server (the newest ones).
	defaultResourceEventsMax = 100
	minResourceEventsMax     = 1
	maxResourceEventsMax     = 10_000

	// OAuth. The redirect address is where "jad mcp login" waits for the browser; a
	// stored access token is refreshed this long before it expires.
	defaultOAuthRedirectURL     = "http://127.0.0.1:47681/callback"
	defaultOAuthRefreshMarginMS = 60_000
	maxOAuthRefreshMarginMS     = 3_600_000
	maxOAuthScopes              = 64
	oauthGrantAuthorizationCode = "authorization_code"
	oauthGrantClientCredentials = "client_credentials"
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerNamePattern      = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
)

type stdioConfig struct {
	command string
	args    []string
	cwd     string
	env     map[string]string
}

type streamableHTTPConfig struct {
	endpoint string
	headers  map[string]string
	oauth    *oauthConfig
}

// oauthConfig is transport.oauth of a streamable_http server. clientSecret is the value
// of the variable client_secret_from names, resolved when the run starts.
type oauthConfig struct {
	grant         string
	clientID      string
	clientSecret  string
	scopes        []string
	redirectURL   string
	refreshMargin time.Duration
}

type transportConfig struct {
	stdio       *stdioConfig
	http        *streamableHTTPConfig
	fingerprint any
}

type loadedConfig struct {
	transport           transportConfig
	timeout             time.Duration
	maxResultBytes      int
	maxTools            int
	maxSchemaBytes      int
	maxDescriptionBytes int
	resourceEventsMax   int
	fingerprint         string
}

func normalizeConfig(raw json.RawMessage, reference sessionfs.MCPServerReference, cwd string, environment map[string]string) (loadedConfig, error) {
	return normalizeConfigFor(raw, reference, cwd, environment, true)
}

// ValidateServerFile applies the loader's rules for the contents of an MCP server file. It
// does not check what depends on the run rather than the file: that the variables named by
// env_from and headers_from are set, and that a stdio cwd exists under the session's cwd.
func ValidateServerFile(path string, raw []byte) error {
	_, err := normalizeConfigFor(raw, sessionfs.MCPServerReference{ConfigPath: path}, "", nil, false)
	return err
}

func normalizeConfigFor(raw json.RawMessage, reference sessionfs.MCPServerReference, cwd string, environment map[string]string, atRun bool) (loadedConfig, error) {
	path := reference.ConfigPath
	fields, err := objectFields(raw, path, "expected a JSON object")
	if err != nil {
		return loadedConfig{}, err
	}
	if value, exists := fields["schema_version"]; exists {
		var version float64
		if json.Unmarshal(value, &version) != nil || version != 1 {
			return loadedConfig{}, configError(path, "unsupported schema_version %s", strings.TrimSpace(string(value)))
		}
	}
	if value, exists := fields["name"]; exists {
		var displayName string
		if json.Unmarshal(value, &displayName) != nil {
			return loadedConfig{}, configError(path, "name must be a string")
		}
	}
	transportFields, err := objectFields(fields["transport"], path, "transport must be an object")
	if err != nil {
		return loadedConfig{}, err
	}
	transportType, err := requiredString(transportFields["type"], path+": transport.type")
	if err != nil {
		return loadedConfig{}, err
	}
	var transport transportConfig
	switch transportType {
	case "stdio":
		config, fingerprint, err := normalizeStdio(transportFields, cwd, path, environment, atRun)
		if err != nil {
			return loadedConfig{}, err
		}
		transport = transportConfig{stdio: config, fingerprint: fingerprint}
	case "streamable_http":
		config, fingerprint, err := normalizeStreamableHTTP(transportFields, path, environment, atRun)
		if err != nil {
			return loadedConfig{}, err
		}
		transport = transportConfig{http: config, fingerprint: fingerprint}
	default:
		return loadedConfig{}, configError(path, "transport.type must be stdio or streamable_http")
	}

	timeoutMS, err := configuredInteger(fields["timeout_ms"], defaultTimeoutMS, 1, maxTimeoutMS, path, "timeout_ms")
	if err != nil {
		return loadedConfig{}, err
	}
	maxResultBytes, err := configuredInteger(fields["max_result_bytes"], defaultMaxResultBytes, minMaxResultBytes, maxMaxResultBytes, path, "max_result_bytes")
	if err != nil {
		return loadedConfig{}, err
	}
	maxTools, err := configuredInteger(fields["max_tools"], defaultMaxTools, minMaxTools, maxMaxTools, path, "max_tools")
	if err != nil {
		return loadedConfig{}, err
	}
	maxSchemaBytes, err := configuredInteger(fields["max_schema_bytes"], defaultMaxSchemaBytes, minMaxSchemaBytes, maxMaxSchemaBytes, path, "max_schema_bytes")
	if err != nil {
		return loadedConfig{}, err
	}
	maxDescriptionBytes, err := configuredInteger(fields["max_description_bytes"], defaultMaxDescriptionBytes, minMaxDescriptionBytes, maxMaxDescriptionBytes, path, "max_description_bytes")
	if err != nil {
		return loadedConfig{}, err
	}
	resourceEventsMax, err := configuredInteger(fields["resource_events_max"], defaultResourceEventsMax, minResourceEventsMax, maxResourceEventsMax, path, "resource_events_max")
	if err != nil {
		return loadedConfig{}, err
	}
	fingerprintBytes, err := json.Marshal(struct {
		Transport           any   `json:"transport"`
		TimeoutMS           int64 `json:"timeout_ms"`
		MaxResultBytes      int   `json:"max_result_bytes"`
		MaxTools            int   `json:"max_tools"`
		MaxSchemaBytes      int   `json:"max_schema_bytes"`
		MaxDescriptionBytes int   `json:"max_description_bytes"`
		ResourceEventsMax   int   `json:"resource_events_max"`
	}{Transport: transport.fingerprint, TimeoutMS: timeoutMS, MaxResultBytes: int(maxResultBytes), MaxTools: int(maxTools), MaxSchemaBytes: int(maxSchemaBytes), MaxDescriptionBytes: int(maxDescriptionBytes), ResourceEventsMax: int(resourceEventsMax)})
	if err != nil {
		return loadedConfig{}, configError(path, "could not fingerprint MCP server configuration")
	}
	digest := sha256.Sum256(fingerprintBytes)
	return loadedConfig{
		transport:           transport,
		timeout:             time.Duration(timeoutMS) * time.Millisecond,
		maxResultBytes:      int(maxResultBytes),
		maxTools:            int(maxTools),
		maxSchemaBytes:      int(maxSchemaBytes),
		maxDescriptionBytes: int(maxDescriptionBytes),
		resourceEventsMax:   int(resourceEventsMax),
		fingerprint:         hex.EncodeToString(digest[:]),
	}, nil
}

func normalizeStdio(fields map[string]json.RawMessage, sessionCWD, path string, environment map[string]string, atRun bool) (*stdioConfig, any, error) {
	command, err := requiredString(fields["command"], path+": transport.command")
	if err != nil {
		return nil, nil, err
	}
	args, err := stringArray(fields["args"], true, path+": transport.args")
	if err != nil {
		return nil, nil, err
	}
	serverCWD := sessionCWD
	if rawCWD, exists := fields["cwd"]; exists {
		configuredCWD, err := requiredString(rawCWD, path+": transport.cwd")
		if err != nil {
			return nil, nil, err
		}
		serverCWD = resolvePath(sessionCWD, configuredCWD)
	}
	if atRun {
		cwdInfo, err := os.Stat(serverCWD)
		if err != nil {
			return nil, nil, configError(path, "stdio cwd does not exist: %s", serverCWD)
		}
		if !cwdInfo.IsDir() {
			return nil, nil, configError(path, "stdio cwd is not a directory: %s", serverCWD)
		}
	}
	configuredEnv, err := stringMap(fields["env"], environmentNamePattern, path+": transport.env", false)
	if err != nil {
		return nil, nil, err
	}
	envFrom, err := stringMap(fields["env_from"], environmentNamePattern, path+": transport.env_from", true)
	if err != nil {
		return nil, nil, err
	}
	for name := range configuredEnv {
		if _, exists := envFrom[name]; exists {
			return nil, nil, configError(path, "transport.env and transport.env_from must not set the same variable")
		}
	}
	resolvedEnv := defaultEnvironment(environment, runtime.GOOS)
	for target, source := range envFrom {
		secret, exists := environment[source]
		if atRun && (!exists || secret == "") {
			return nil, nil, configError(path, "environment variable %s required by transport.env_from.%s is not set", source, target)
		}
		resolvedEnv[target] = secret
	}
	for target, value := range configuredEnv {
		resolvedEnv[target] = value
	}
	config := &stdioConfig{command: command, args: append([]string(nil), args...), cwd: serverCWD, env: resolvedEnv}
	fingerprint := struct {
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		CWD     string            `json:"cwd"`
		Env     map[string]string `json:"env"`
	}{Type: "stdio", Command: command, Args: args, CWD: serverCWD, Env: resolvedEnv}
	return config, fingerprint, nil
}

func normalizeStreamableHTTP(fields map[string]json.RawMessage, path string, environment map[string]string, atRun bool) (*streamableHTTPConfig, any, error) {
	urlText, err := requiredString(fields["url"], path+": transport.url")
	if err != nil {
		return nil, nil, err
	}
	endpoint, err := url.Parse(urlText)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, nil, configError(path, "transport.url must use HTTP(S) and must not contain credentials or a fragment")
	}
	headersFrom, err := stringMap(fields["headers_from"], headerNamePattern, path+": transport.headers_from", true)
	if err != nil {
		return nil, nil, err
	}
	resolvedHeaders := make(map[string]string, len(headersFrom))
	for header, source := range headersFrom {
		secret, exists := environment[source]
		if atRun && (!exists || secret == "") {
			return nil, nil, configError(path, "environment variable %s required by transport.headers_from.%s is not set", source, header)
		}
		resolvedHeaders[header] = secret
	}
	oauth, err := normalizeOAuth(fields["oauth"], path, environment, atRun)
	if err != nil {
		return nil, nil, err
	}
	if oauth != nil {
		for header := range headersFrom {
			if strings.EqualFold(header, "Authorization") {
				return nil, nil, configError(path, "transport.oauth and an Authorization header in transport.headers_from must not both be set")
			}
		}
	}
	endpointText := endpoint.String()
	config := &streamableHTTPConfig{endpoint: endpointText, headers: resolvedHeaders, oauth: oauth}
	fingerprint := struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		OAuth   any               `json:"oauth,omitempty"`
	}{Type: "streamable_http", URL: endpointText, Headers: resolvedHeaders}
	if oauth != nil {
		fingerprint.OAuth = struct {
			Grant         string   `json:"grant"`
			ClientID      string   `json:"client_id"`
			ClientSecret  string   `json:"client_secret"`
			Scopes        []string `json:"scopes"`
			RedirectURL   string   `json:"redirect_url"`
			RefreshMargin int64    `json:"refresh_margin_ms"`
		}{oauth.grant, oauth.clientID, oauth.clientSecret, oauth.scopes, oauth.redirectURL, oauth.refreshMargin.Milliseconds()}
	}
	return config, fingerprint, nil
}

// normalizeOAuth reads transport.oauth. authorization_code (the default) uses the tokens
// "jad mcp login" stored, registering a client dynamically when no client_id is given;
// client_credentials needs a client_id and secret and no sign-in.
func normalizeOAuth(raw json.RawMessage, path string, environment map[string]string, atRun bool) (*oauthConfig, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	fields, err := objectFields(raw, path, "transport.oauth must be an object")
	if err != nil {
		return nil, err
	}
	for key := range fields {
		switch key {
		case "grant", "client_id", "client_secret_from", "scopes", "redirect_url", "refresh_margin_ms":
		default:
			return nil, configError(path, "transport.oauth.%s is not supported", key)
		}
	}
	config := &oauthConfig{grant: oauthGrantAuthorizationCode, redirectURL: defaultOAuthRedirectURL}
	if value, exists := fields["grant"]; exists {
		if config.grant, err = requiredString(value, path+": transport.oauth.grant"); err != nil {
			return nil, err
		}
		if config.grant != oauthGrantAuthorizationCode && config.grant != oauthGrantClientCredentials {
			return nil, configError(path, "transport.oauth.grant must be authorization_code or client_credentials")
		}
	}
	if value, exists := fields["client_id"]; exists {
		if config.clientID, err = requiredString(value, path+": transport.oauth.client_id"); err != nil {
			return nil, err
		}
	}
	if value, exists := fields["client_secret_from"]; exists {
		source, err := requiredString(value, path+": transport.oauth.client_secret_from")
		if err != nil {
			return nil, err
		}
		if !environmentNamePattern.MatchString(source) {
			return nil, configError(path, "transport.oauth.client_secret_from must be an environment variable name")
		}
		if config.clientID == "" {
			return nil, configError(path, "transport.oauth.client_secret_from needs transport.oauth.client_id")
		}
		secret, exists := environment[source]
		if atRun && (!exists || secret == "") {
			return nil, configError(path, "environment variable %s required by transport.oauth.client_secret_from is not set", source)
		}
		config.clientSecret = secret
		// A file check has no environment: a placeholder stands for the secret, so the
		// rules that need one still apply.
		if !atRun {
			config.clientSecret = "$" + source
		}
	}
	if config.grant == oauthGrantClientCredentials && (config.clientID == "" || config.clientSecret == "") {
		return nil, configError(path, "transport.oauth.grant client_credentials needs client_id and client_secret_from")
	}
	if value, exists := fields["scopes"]; exists {
		scopes, err := stringArray(value, false, path+": transport.oauth.scopes")
		if err != nil {
			return nil, err
		}
		if len(scopes) == 0 || len(scopes) > maxOAuthScopes {
			return nil, configError(path, "transport.oauth.scopes must list 1 to %d scopes", maxOAuthScopes)
		}
		seen := make(map[string]struct{}, len(scopes))
		for _, scope := range scopes {
			if scope == "" || strings.ContainsAny(scope, " \t\r\n\"\\") {
				return nil, configError(path, "transport.oauth.scopes must be scope tokens without spaces or quotes")
			}
			if _, repeated := seen[scope]; repeated {
				return nil, configError(path, "transport.oauth.scopes lists %s twice", scope)
			}
			seen[scope] = struct{}{}
		}
		config.scopes = scopes
	}
	if value, exists := fields["redirect_url"]; exists {
		if config.redirectURL, err = requiredString(value, path+": transport.oauth.redirect_url"); err != nil {
			return nil, err
		}
	}
	if _, err := loginRedirect(config.redirectURL); err != nil {
		return nil, configError(path, "transport.oauth.redirect_url %v", err)
	}
	margin, err := configuredInteger(fields["refresh_margin_ms"], defaultOAuthRefreshMarginMS, 0, maxOAuthRefreshMarginMS, path, "transport.oauth.refresh_margin_ms")
	if err != nil {
		return nil, err
	}
	config.refreshMargin = time.Duration(margin) * time.Millisecond
	if config.grant == oauthGrantClientCredentials {
		for _, key := range []string{"scopes", "redirect_url", "refresh_margin_ms"} {
			if _, set := fields[key]; set {
				return nil, configError(path, "transport.oauth.%s applies only to grant authorization_code", key)
			}
		}
	}
	return config, nil
}

// loginRedirect checks a redirect address and reports whether "jad mcp login" can wait for
// it itself: an http address on the loopback interface with a port. Any https address is
// accepted too; the browser cannot reach the login there, so the address it was sent to
// is pasted instead.
func loginRedirect(raw string) (listen bool, err error) {
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return false, fmt.Errorf("must be an absolute http(s) address without credentials, query or fragment")
	}
	switch parsed.Scheme {
	case "https":
		return false, nil
	case "http":
		host := parsed.Hostname()
		if host != "127.0.0.1" && host != "::1" && host != "localhost" {
			return false, fmt.Errorf("must be https, or http on 127.0.0.1, ::1 or localhost")
		}
		if parsed.Port() == "" {
			return false, fmt.Errorf("on the loopback interface must name a port")
		}
		return true, nil
	default:
		return false, fmt.Errorf("must be an absolute http(s) address without credentials, query or fragment")
	}
}

func objectFields(raw json.RawMessage, path, message string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, configError(path, "%s", message)
	}
	return fields, nil
}

func requiredString(raw json.RawMessage, fieldPath string) (string, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
		return "", configError(fieldPath, "must be a non-empty string")
	}
	return strings.TrimSpace(value), nil
}

func stringArray(raw json.RawMessage, nullAsEmpty bool, fieldPath string) ([]string, error) {
	if len(raw) == 0 || (nullAsEmpty && strings.TrimSpace(string(raw)) == "null") {
		return []string{}, nil
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, configError(fieldPath, "must be an array of strings")
	}
	result := make([]string, 0, len(values))
	for _, rawValue := range values {
		var value string
		if json.Unmarshal(rawValue, &value) != nil {
			return nil, configError(fieldPath, "must be an array of strings")
		}
		result = append(result, value)
	}
	return result, nil
}

func stringMap(raw json.RawMessage, targetPattern *regexp.Regexp, fieldPath string, sourceNames bool) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		if sourceNames {
			return nil, configError(fieldPath, "must be an object mapping target names to environment variable names")
		}
		return nil, configError(fieldPath, "must be an object mapping environment variable names to values")
	}
	result := make(map[string]string, len(values))
	for target, rawValue := range values {
		var value string
		if json.Unmarshal(rawValue, &value) != nil || targetPattern != nil && !targetPattern.MatchString(target) {
			if sourceNames {
				return nil, configError(fieldPath, "keys and values must be environment variable names")
			}
			return nil, configError(fieldPath, "must contain environment variable names and string values")
		}
		if sourceNames {
			value = strings.TrimSpace(value)
			if value == "" || !environmentNamePattern.MatchString(value) {
				return nil, configError(fieldPath, "keys and values must be environment variable names")
			}
		}
		result[target] = value
	}
	return result, nil
}

func configuredInteger(raw json.RawMessage, fallback, minimum, maximum int64, path, field string) (int64, error) {
	if len(raw) == 0 {
		return fallback, nil
	}
	var value float64
	if json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value || value < float64(minimum) || value > float64(maximum) {
		return 0, configError(path, "%s must be an integer from %d to %d", field, minimum, maximum)
	}
	return int64(value), nil
}

func resolvePath(base, configured string) string {
	if filepath.IsAbs(configured) {
		return filepath.Clean(configured)
	}
	absolute, err := filepath.Abs(filepath.Join(base, configured))
	if err != nil {
		return filepath.Clean(filepath.Join(base, configured))
	}
	return absolute
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}

func configError(path, format string, args ...any) error {
	return &sessionfs.ConfigError{Message: path + ": " + fmt.Sprintf(format, args...)}
}
