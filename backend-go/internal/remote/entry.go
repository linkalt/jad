// Package remote is the client side of calling another JAD: the remote entry files, the
// HTTP client that reaches them and the token handling. It imports nothing of the
// application so the tools and the server can share it.
package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	// ControlSlots is how many control calls (stops) may run at once, on top of max_in_flight.
	ControlSlots       = 2
	MaxRetries         = 5
	DefaultPollMs      = 20_000
	MinPollMs          = 100
	MaxPollMs          = 60_000
	DefaultLostAfterMs = 60_000
	MinLostAfterMs     = 1_000
	MaxLostAfterMs     = 3_600_000
	// MinTokenBytes matches the server's minimum for its API tokens.
	MinTokenBytes = 32
	maxTokenFile  = 4096
)

var (
	// NamePattern is a remote's name: the file name without .json, one path segment.
	NamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	// envNamePattern limits token_env to the JAD_REMOTE_ prefix: a remote file names where its
	// token is read from and where it is sent, so it must not be able to name a provider key.
	envNamePattern = regexp.MustCompile(`^JAD_REMOTE_[A-Za-z0-9_]+$`)
	// SessionPattern is a session id as the remote's routes take it.
	SessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// workspacePattern is a workspace id as the remote server lists it (/api/workspaces).
	workspacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	tokenPattern     = regexp.MustCompile(`^[\x21-\x7e]+$`)
)

// Entry is one remote file: <remotes>/<name>.json. It holds where to reach the remote and
// where its token comes from, never the token.
type Entry struct {
	Name        string
	Description string
	URL         string
	// Unix is the socket path when URL is unix://; Host is the http host:port otherwise.
	Unix             string
	Host             string
	Timeout          int64
	MaxResponseBytes int64
	Retries          int
	MaxInFlight      int
	PollMs           int64
	LostAfterMs      int64
	// Workspace is the workspace of the server the binding reaches. A binding is one server and
	// one workspace: a session is sent as <Workspace>:<session>.
	Workspace string
	// KeyFile is the key file (G/auth/keys/<key>.json) the calling session's remote_keys give
	// this binding, and KeyName its name; both are set by a scoped Directory, never by the
	// binding file, and are empty for an unkeyed binding.
	KeyFile string
	KeyName string
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
	TimeoutMs        *int64   `json:"timeout_ms"`
	MaxResponseBytes *int64   `json:"max_response_bytes"`
	Retries          *int64   `json:"retries"`
	MaxInFlight      *int64   `json:"max_in_flight"`
	PollMs           *int64   `json:"poll_ms"`
	LostAfterMs      *int64   `json:"lost_after_ms"`
	Workspace        string   `json:"workspace"`
	Profiles         []string `json:"profiles"`
}

// inlineSecretKeys are keys a remote file must not carry: the message names the allowed sources.
var inlineSecretKeys = []string{"token", "password", "secret", "authorization", "api_key", "bearer", "proxy"}

// removedTLSKeys were the TLS options of the https era. They are refused by name, with the
// reason, instead of the generic unknown-key error: a file that still carries one was written
// for a connection this build no longer makes.
var removedTLSKeys = []string{"ca_file", "insecure", "insecure_skip_verify", "skip_verify", "tls"}

// ParseBinding reads one remote file: a binding, which is one server and one workspace on it. It
// has no credential of its own; the caller's key supplies it. The keys of the retired v1 layout
// are refused by name (checkRemovedBindingKeys).
func ParseBinding(name, path string, data []byte) (Entry, error) {
	if !NamePattern.MatchString(name) {
		return Entry{}, fmt.Errorf("%s: a remote name is 1 to 64 letters, digits, '_' or '-'", path)
	}
	if err := checkRemoteKeys(path, data); err != nil {
		return Entry{}, err
	}
	if err := checkRemovedBindingKeys(path, data); err != nil {
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
	entry := Entry{Name: name, Description: file.Description, Profiles: append([]string(nil), r.Profiles...)}
	if err := entry.setURL(path, r.URL); err != nil {
		return Entry{}, err
	}
	if !workspacePattern.MatchString(r.Workspace) {
		return Entry{}, fmt.Errorf("%s: remote.workspace is required: the id of the workspace on that server this binding reaches (1 to 64 letters, digits, '.', '_' or '-'); a second workspace on the same server is a second binding", path)
	}
	entry.Workspace = r.Workspace
	// The credential is the caller's key: nothing in the binding names a token source.
	for _, glob := range r.Profiles {
		if strings.TrimSpace(glob) == "" || strings.ContainsAny(glob, "\x00\\[") {
			return Entry{}, fmt.Errorf("%s: remote.profiles entries must be non-empty globs like category/name, without [classes]", path)
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

// checkRemoteKeys gives a clear message for a key that would hold a secret or asks for a TLS
// setting, before the generic unknown-key error.
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
				return fmt.Errorf("%s: %q is not allowed in a remote file: the token comes from token_env or token_file, and no proxy is used", path, key)
			}
		}
		for _, key := range removedTLSKeys {
			if _, has := level[key]; has {
				return fmt.Errorf("%s: %q was removed: a remote is reached over plain http or a unix socket, so there is no TLS to configure and no certificate to trust; delete the key", path, key)
			}
		}
	}
	return nil
}

// removedBindingKeys are the keys of a v1 remote file a binding does not have, with what to do
// instead: they are refused by name, never ignored, so a file written for the retired v1 layout
// never silently loses its credential or its default session.
var removedBindingKeys = map[string]string{
	"token_env":       "the secret moves to a key file G/auth/keys/<key>.json (its token_env, or token), the key is named in remote_keys, and the binding names no credential",
	"token_file":      "move the secret to a key file G/auth/keys/<key>.json, list the key in remote_keys, and delete the key from the binding",
	"default_session": "write the session in the address: remote:<binding>/<session>/<category>/<name>",
}

// checkRemovedBindingKeys refuses the retired v1 keys in a binding.
func checkRemovedBindingKeys(path string, data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	var inner map[string]json.RawMessage
	if raw, ok := top["remote"]; ok {
		_ = json.Unmarshal(raw, &inner)
	}
	for _, key := range []string{"token_env", "token_file", "default_session"} {
		if _, has := inner[key]; has {
			return fmt.Errorf("%s: remote.%s was removed with workspace layout v1; %s", path, key, removedBindingKeys[key])
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
	case "http":
		if parsed.Hostname() == "" || (parsed.Path != "" && parsed.Path != "/") {
			return fmt.Errorf("%s: remote.url must be http://host[:port] with no path", path)
		}
		e.Host = parsed.Host
		e.URL = "http://" + parsed.Host
	case "unix":
		if parsed.Host != "" || !filepath.IsAbs(parsed.Path) || parsed.Path == "/" {
			return fmt.Errorf("%s: remote.url for a unix socket must be unix:///absolute/path.sock", path)
		}
		e.Unix = filepath.Clean(parsed.Path)
		e.URL = "unix://" + e.Unix
	case "https":
		return fmt.Errorf("%s: remote.url must be http://host[:port] or unix:///path.sock: https was removed, and the bearer token travels in clear text over http, so reach the remote over a VPN or a unix socket", path)
	default:
		return fmt.Errorf("%s: remote.url must be http://host[:port] or unix:///path.sock", path)
	}
	return nil
}

// tokenFileChecked runs between the checks on a token file and its read; tests replace the path here.
var tokenFileChecked = func() {}

// Address is where the entry reaches its server: the socket path or the host and port. Two entries with
// the same address reach the same server whatever else they differ in (token, timeouts, limits).
func (e Entry) Address() string {
	if e.Unix != "" {
		return "unix:" + e.Unix
	}
	return "http:" + e.Host
}

// SameHost reports whether the remote's server runs on this machine: it is reached over a unix
// socket or at a loopback (or unspecified) address. Such a server may work in a folder this machine
// also works in. It is a heuristic about the address, not a statement about file systems and not
// confinement: it reads no DNS and no network, so a name that resolves to this machine, this machine's
// own LAN or public address, a tunnel or port forward, or a network file system that shares the
// folder with a server at another address all read as another machine. An address of another machine
// says nothing about its file systems, so it is not.
func (e Entry) SameHost() bool {
	if e.Unix != "" {
		return true
	}
	host := e.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// PointsAt reports whether the entry reaches the server that listens on the unix socket
// listenUnix, or, when that is empty, on host:port. It is a warning aid (jad check) and not a
// refusal: another name for the same machine (a LAN address, a tunnel) is not recognised.
func (e Entry) PointsAt(listenUnix, host string, port int) bool {
	if listenUnix != "" {
		return e.Unix != "" && e.Unix == filepath.Clean(listenUnix)
	}
	if e.Unix != "" || port == 0 {
		return false
	}
	entryHost, entryPort, err := net.SplitHostPort(e.Host)
	if err != nil || entryPort != strconv.Itoa(port) {
		return false
	}
	own := net.ParseIP(strings.Trim(host, "[]"))
	if host == "" || (own != nil && (own.IsLoopback() || own.IsUnspecified())) || strings.EqualFold(host, "localhost") {
		return e.SameHost()
	}
	return strings.EqualFold(strings.Trim(entryHost, "[]"), host)
}

// Keyed reports whether the entry can authenticate: only when the calling session's remote_keys
// give the binding a key.
func (e Entry) Keyed() bool { return e.KeyFile != "" }

// WireSession is the session as the remote server's routes take it: <workspace>:<session>.
func (e Entry) WireSession(session string) string {
	return e.Workspace + ":" + session
}

// Token reads the token from the key the calling session has for this binding. Errors name the
// source, never a value.
func (e Entry) Token(environment func(string) string) (string, error) {
	return e.keyToken(environment)
}

// Allows reports whether the client-side allow-list lets a model reach target:
// "category/name" of a profile, or "category/name/agent" of a sub-agent.
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

// LoadBinding reads <dir>/<name>.json as a binding. A missing file is ErrNotFound.
func LoadBinding(dir, name string) (Entry, error) {
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
	return ParseBinding(name, path, data)
}

// ErrNotFound is a remote name with no file.
var ErrNotFound = errors.New("unknown remote")

// ListBindings reads every <dir>/<name>.json, sorted by name. A missing dir is empty. A broken
// file is reported in problems and does not hide the others.
func ListBindings(dir string) (entries []Entry, problems []error) { return list(dir, LoadBinding) }

func list(dir string, load func(dir, name string) (Entry, error)) (entries []Entry, problems []error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			problems = append(problems, fmt.Errorf("remotes folder: %v", err))
		}
		return nil, problems
	}
	for _, item := range items {
		name, ok := strings.CutSuffix(item.Name(), ".json")
		if !ok {
			continue
		}
		entry, err := load(dir, name)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, problems
}
