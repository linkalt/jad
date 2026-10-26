// Package remote is the client side of calling another JAD: the remote entry files, the
// HTTP client that reaches them and the token handling. It imports nothing of the
// application so the tools and the server can share it.
package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const (
	DefaultTimeoutMs        = 30_000
	MaxTimeoutMs            = 5 * 60 * 1000
	DefaultMaxResponseBytes = 1 << 20
	MaxMaxResponseBytes     = 64 << 20
	DefaultRetries          = 2
	DefaultMaxInFlight      = 8
	MaxMaxInFlight          = 64
	MaxRetries              = 5
	DefaultPollMs           = 20_000
	MinPollMs               = 100
	MaxPollMs               = 60_000
	DefaultLostAfterMs      = 60_000
	MinLostAfterMs          = 1_000
	MaxLostAfterMs          = 3_600_000
	// MinTokenBytes matches the server's minimum for its API tokens.
	MinTokenBytes = 32
	maxTokenFile  = 4096
)

var (
	// NamePattern is a remote's name: the file name without .json, one path segment.
	NamePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// SessionPattern is a session id as the remote's routes take it.
	SessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	tokenPattern   = regexp.MustCompile(`^[\x21-\x7e]+$`)
)

// Entry is one remote file: <remotes>/<name>.json. It holds where to reach the remote and
// where its token comes from, never the token.
type Entry struct {
	Name        string
	Description string
	URL         string
	// Unix is the socket path when URL is unix://; Host is the https host:port otherwise.
	Unix string
	Host string
	// TokenEnv or TokenFile (absolute) is the token source; exactly one is set.
	TokenEnv  string
	TokenFile string
	// CAFile is an extra PEM root file for https, absolute; empty means system roots only.
	CAFile           string
	Timeout          int64
	MaxResponseBytes int64
	Retries          int
	MaxInFlight      int
	PollMs           int64
	LostAfterMs      int64
	DefaultSession   string
	// Profiles narrows what this client lets a model reach (globs of category/name for
	// profiles and agents); empty allows everything the remote's token allows.
	Profiles []string
}

type fileEntry struct {
	SchemaVersion int         `json:"schema_version"`
	Kind          string      `json:"kind"`
	Description   string      `json:"description"`
	Remote        *fileRemote `json:"remote"`
}

type fileRemote struct {
	URL              string   `json:"url"`
	TokenEnv         string   `json:"token_env"`
	TokenFile        string   `json:"token_file"`
	CAFile           string   `json:"ca_file"`
	TimeoutMs        *int64   `json:"timeout_ms"`
	MaxResponseBytes *int64   `json:"max_response_bytes"`
	Retries          *int64   `json:"retries"`
	MaxInFlight      *int64   `json:"max_in_flight"`
	PollMs           *int64   `json:"poll_ms"`
	LostAfterMs      *int64   `json:"lost_after_ms"`
	DefaultSession   string   `json:"default_session"`
	Profiles         []string `json:"profiles"`
}

// inlineSecretKeys are keys a remote file must not carry: the message names the allowed sources.
var inlineSecretKeys = []string{"token", "password", "secret", "authorization", "api_key", "bearer", "insecure", "insecure_skip_verify", "skip_verify", "proxy"}

// ParseEntry reads one remote file. path is used for messages and to resolve relative paths.
func ParseEntry(name, path string, data []byte) (Entry, error) {
	if !NamePattern.MatchString(name) {
		return Entry{}, fmt.Errorf("%s: a remote name is 1 to 64 letters, digits, '_' or '-'", path)
	}
	if err := checkRemoteKeys(path, data); err != nil {
		return Entry{}, err
	}
	var file fileEntry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return Entry{}, fmt.Errorf("%s: %v", path, err)
	}
	if file.SchemaVersion != 0 && file.SchemaVersion != 1 {
		return Entry{}, fmt.Errorf("%s: unsupported schema_version %d", path, file.SchemaVersion)
	}
	if file.Kind != "remote" {
		return Entry{}, fmt.Errorf(`%s: kind must be "remote"`, path)
	}
	if file.Remote == nil {
		return Entry{}, fmt.Errorf("%s: remote is required", path)
	}
	r := file.Remote
	base := filepath.Dir(path)
	entry := Entry{Name: name, Description: file.Description, TokenEnv: r.TokenEnv, DefaultSession: r.DefaultSession, Profiles: append([]string(nil), r.Profiles...)}
	if err := entry.setURL(path, r.URL); err != nil {
		return Entry{}, err
	}
	switch {
	case r.TokenEnv != "" && r.TokenFile != "":
		return Entry{}, fmt.Errorf("%s: remote.token_env and remote.token_file are alternatives: set one", path)
	case r.TokenEnv == "" && r.TokenFile == "":
		return Entry{}, fmt.Errorf("%s: remote needs token_env or token_file; the token itself is never written in the file", path)
	case r.TokenEnv != "" && !envNamePattern.MatchString(r.TokenEnv):
		return Entry{}, fmt.Errorf("%s: remote.token_env must be an environment variable name", path)
	}
	if r.TokenFile != "" {
		entry.TokenFile = resolveFrom(base, r.TokenFile)
	}
	if r.CAFile != "" {
		if entry.Unix != "" {
			return Entry{}, fmt.Errorf("%s: remote.ca_file only applies to an https url", path)
		}
		entry.CAFile = resolveFrom(base, r.CAFile)
	}
	if r.DefaultSession != "" && !SessionPattern.MatchString(r.DefaultSession) {
		return Entry{}, fmt.Errorf("%s: remote.default_session is not a valid session id", path)
	}
	for _, glob := range r.Profiles {
		if strings.TrimSpace(glob) == "" || strings.ContainsAny(glob, "\x00\\") {
			return Entry{}, fmt.Errorf("%s: remote.profiles entries must be non-empty globs like category/name", path)
		}
	}
	for _, field := range []struct {
		target             *int64
		value              *int64
		name               string
		fallback, min, max int64
	}{
		{&entry.Timeout, r.TimeoutMs, "timeout_ms", DefaultTimeoutMs, 1, MaxTimeoutMs},
		{&entry.MaxResponseBytes, r.MaxResponseBytes, "max_response_bytes", DefaultMaxResponseBytes, 1, MaxMaxResponseBytes},
		{&entry.PollMs, r.PollMs, "poll_ms", DefaultPollMs, MinPollMs, MaxPollMs},
		{&entry.LostAfterMs, r.LostAfterMs, "lost_after_ms", DefaultLostAfterMs, MinLostAfterMs, MaxLostAfterMs},
	} {
		*field.target = field.fallback
		if field.value != nil {
			if *field.value < field.min || *field.value > field.max {
				return Entry{}, fmt.Errorf("%s: remote.%s must be between %d and %d", path, field.name, field.min, field.max)
			}
			*field.target = *field.value
		}
	}
	entry.Retries = DefaultRetries
	if r.Retries != nil {
		if *r.Retries < 0 || *r.Retries > MaxRetries {
			return Entry{}, fmt.Errorf("%s: remote.retries must be between 0 and %d", path, MaxRetries)
		}
		entry.Retries = int(*r.Retries)
	}
	entry.MaxInFlight = DefaultMaxInFlight
	if r.MaxInFlight != nil {
		if *r.MaxInFlight < 1 || *r.MaxInFlight > MaxMaxInFlight {
			return Entry{}, fmt.Errorf("%s: remote.max_in_flight must be between 1 and %d", path, MaxMaxInFlight)
		}
		entry.MaxInFlight = int(*r.MaxInFlight)
	}
	return entry, nil
}

// checkRemoteKeys gives a clear message for the keys that would hold a secret or weaken
// TLS, before the generic unknown-key error.
func checkRemoteKeys(path string, data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	var inner map[string]json.RawMessage
	if raw, ok := top["remote"]; ok {
		_ = json.Unmarshal(raw, &inner)
	}
	for _, level := range []map[string]json.RawMessage{top, inner} {
		for _, key := range inlineSecretKeys {
			if _, has := level[key]; has {
				return fmt.Errorf("%s: %q is not allowed in a remote file: the token comes from token_env or token_file, and TLS is always verified and no proxy is used", path, key)
			}
		}
	}
	return nil
}

func (e *Entry) setURL(path, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || raw == "" {
		return fmt.Errorf("%s: remote.url is not a valid url", path)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s: remote.url must not carry credentials, a query or a fragment", path)
	}
	switch parsed.Scheme {
	case "https":
		if parsed.Hostname() == "" || (parsed.Path != "" && parsed.Path != "/") {
			return fmt.Errorf("%s: remote.url must be https://host[:port] with no path", path)
		}
		e.Host = parsed.Host
		e.URL = "https://" + parsed.Host
	case "unix":
		if parsed.Host != "" || !filepath.IsAbs(parsed.Path) || parsed.Path == "/" {
			return fmt.Errorf("%s: remote.url for a unix socket must be unix:///absolute/path.sock", path)
		}
		e.Unix = filepath.Clean(parsed.Path)
		e.URL = "unix://" + e.Unix
	case "http":
		return fmt.Errorf("%s: remote.url must be https or unix: plain http is not allowed", path)
	default:
		return fmt.Errorf("%s: remote.url must be https://host[:port] or unix:///path.sock", path)
	}
	return nil
}

func resolveFrom(base, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(base, value)
}

// Token reads the token from its source. Errors name the source, never a value.
func (e Entry) Token(environment func(string) string) (string, error) {
	var token string
	switch {
	case e.TokenEnv != "":
		if environment == nil {
			environment = os.Getenv
		}
		token = strings.TrimSpace(environment(e.TokenEnv))
		if token == "" {
			return "", fmt.Errorf("remote %s: environment variable %s is not set", e.Name, e.TokenEnv)
		}
	case e.TokenFile != "":
		info, err := os.Stat(e.TokenFile)
		if err != nil {
			return "", fmt.Errorf("remote %s: token file %s cannot be read", e.Name, e.TokenFile)
		}
		if !info.Mode().IsRegular() || info.Size() > maxTokenFile {
			return "", fmt.Errorf("remote %s: token file %s must be a regular file under %d bytes", e.Name, e.TokenFile, maxTokenFile)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("remote %s: token file %s must not be readable by group or others (chmod 600)", e.Name, e.TokenFile)
		}
		data, err := os.ReadFile(e.TokenFile)
		if err != nil {
			return "", fmt.Errorf("remote %s: token file %s cannot be read", e.Name, e.TokenFile)
		}
		token = strings.TrimSpace(string(data))
	default:
		return "", fmt.Errorf("remote %s has no token source", e.Name)
	}
	if len(token) < MinTokenBytes || !tokenPattern.MatchString(token) {
		return "", fmt.Errorf("remote %s: the token must be at least %d printable characters without spaces", e.Name, MinTokenBytes)
	}
	return token, nil
}

// Allows reports whether the client-side allow-list lets a model reach target
// ("category/name" of a profile or agent).
func (e Entry) Allows(target string) bool {
	if len(e.Profiles) == 0 {
		return true
	}
	for _, glob := range e.Profiles {
		if ok, err := matchGlob(glob, target); err == nil && ok {
			return true
		}
	}
	return false
}

// Load reads <dir>/<name>.json. A missing file is ErrNotFound.
func Load(dir, name string) (Entry, error) {
	if !NamePattern.MatchString(name) {
		return Entry{}, ErrNotFound
	}
	path := filepath.Join(dir, name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, fmt.Errorf("remote %s: %v", name, err)
	}
	return ParseEntry(name, path, data)
}

// ErrNotFound is a remote name with no file.
var ErrNotFound = errors.New("unknown remote")

// List reads every <dir>/<name>.json, sorted by name. A missing dir is empty. A broken
// file is reported in problems and does not hide the others.
func List(dir string) (entries []Entry, problems []error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			problems = append(problems, fmt.Errorf("remotes folder: %v", err))
		}
		return nil, problems
	}
	for _, item := range items {
		name, ok := strings.CutSuffix(item.Name(), ".json")
		if !ok || !item.Type().IsRegular() {
			continue
		}
		entry, err := Load(dir, name)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, problems
}
