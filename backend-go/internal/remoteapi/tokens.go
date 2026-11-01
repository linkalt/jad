// Package remoteapi is the server side of remote calling: /api/remote/v1 with its own
// token table, separate from the UI token, so a remote token can only do what its entry allows.
package remoteapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Actions a token may be granted.
const (
	ActionMessage     = "message"
	ActionSubagent    = "subagent"
	ActionStatus      = "status"
	ActionStop        = "stop"
	ActionTranscripts = "transcripts"
	// ActionProfile lets a token ask for a profile's info (its context tokens). Compact and reset
	// are not remote actions: a remote never changes a profile.
	ActionProfile = "profile"
)

var (
	allActions     = []string{ActionMessage, ActionSubagent, ActionStatus, ActionStop, ActionTranscripts, ActionProfile}
	tokenIDRule    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	sha256HexRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	errNoToken     = errors.New("token required")
	errBadToken    = errors.New("token not accepted")
	errTableBroken = errors.New("token table is not usable")
	maxTokenFile   = int64(1 << 20)
)

// tokenFileCheckInterval is the shortest time between two looks at the table file, so a burst
// of requests costs one read; a revocation applies within this time.
const tokenFileCheckInterval = 250 * time.Millisecond

// Token is one row of the table. Only the SHA-256 of the secret is stored.
type Token struct {
	ID      string
	hash    [sha256.Size]byte
	Expires time.Time
	Revoked bool
	// Profiles are globs over "session/category/profile", Agents over
	// "session/category/profile/agent"; '*' does not cross '/'.
	Profiles []string
	Agents   []string
	Actions  map[string]bool
	// MaxHops lowers the server's hop limit for this token; zero keeps the server's.
	MaxHops int
}

type tokenFile struct {
	SchemaVersion int        `json:"schema_version"`
	Tokens        []tokenRow `json:"tokens"`
}

type tokenRow struct {
	ID       string   `json:"id"`
	SHA256   string   `json:"sha256"`
	Expires  string   `json:"expires"`
	Revoked  bool     `json:"revoked"`
	Profiles []string `json:"profiles"`
	Agents   []string `json:"agents"`
	Actions  []string `json:"actions"`
	MaxHops  int      `json:"max_hops"`
}

// ParseTokens reads a token table. Unknown keys, a duplicate id or hash, a malformed hash or
// glob, and an unknown action are errors, so a typo never widens or silently drops a grant.
func ParseTokens(data []byte) ([]Token, error) {
	var file tokenFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("token table: %v", err)
	}
	if file.SchemaVersion != 0 && file.SchemaVersion != 1 {
		return nil, fmt.Errorf("token table: unsupported schema_version %d", file.SchemaVersion)
	}
	ids, hashes := map[string]bool{}, map[string]bool{}
	tokens := make([]Token, 0, len(file.Tokens))
	for index, row := range file.Tokens {
		where := fmt.Sprintf("token table: tokens[%d]", index)
		if !tokenIDRule.MatchString(row.ID) {
			return nil, fmt.Errorf("%s: id must be 1 to 64 letters, digits, '_', '.' or '-'", where)
		}
		hash := strings.ToLower(row.SHA256)
		if !sha256HexRe.MatchString(hash) {
			return nil, fmt.Errorf("%s (%s): sha256 must be the 64 hex digits of the SHA-256 of the token", where, row.ID)
		}
		if ids[row.ID] || hashes[hash] {
			return nil, fmt.Errorf("%s (%s): id and sha256 must be unique", where, row.ID)
		}
		ids[row.ID], hashes[hash] = true, true
		token := Token{ID: row.ID, Revoked: row.Revoked, Profiles: row.Profiles, Agents: row.Agents, Actions: map[string]bool{}, MaxHops: row.MaxHops}
		raw, _ := hex.DecodeString(hash)
		copy(token.hash[:], raw)
		if row.Expires != "" {
			expires, err := time.Parse(time.RFC3339, row.Expires)
			if err != nil {
				return nil, fmt.Errorf("%s (%s): expires must be RFC 3339", where, row.ID)
			}
			token.Expires = expires
		}
		if row.MaxHops < 0 {
			return nil, fmt.Errorf("%s (%s): max_hops must not be negative", where, row.ID)
		}
		for _, glob := range append(append([]string(nil), row.Profiles...), row.Agents...) {
			if _, err := path.Match(glob, "a/b/c/d"); err != nil || strings.TrimSpace(glob) == "" || strings.Contains(glob, "[") {
				return nil, fmt.Errorf("%s (%s): %q is not a valid glob over session/category/name (no [classes])", where, row.ID, glob)
			}
		}
		for _, action := range row.Actions {
			if !contains(allActions, action) {
				return nil, fmt.Errorf("%s (%s): unknown action %q (%s)", where, row.ID, action, strings.Join(allActions, ", "))
			}
			token.Actions[action] = true
		}
		tokens = append(tokens, token)
	}
	return tokens, nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Allows reports whether the token may do action.
func (t *Token) Allows(action string) bool { return t.Actions[action] }

// MayProfile and MayAgent check "session/category/name" against the token's globs. A token
// with no globs reaches nothing.
func (t *Token) MayProfile(target string) bool { return matchAny(t.Profiles, target) }
func (t *Token) MayAgent(target string) bool   { return matchAny(t.Agents, target) }

func matchAny(globs []string, target string) bool {
	for _, segment := range strings.Split(target, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	for _, glob := range globs {
		if ok, err := path.Match(glob, target); err == nil && ok {
			return true
		}
	}
	return false
}

// TokenTable authenticates a bearer secret. With a file it re-reads the file when its content
// or mode changes (looking at most every tokenFileCheckInterval), so a revoked flag, a removed
// row or a new expiry applies to the next request after that; if the file becomes unreadable,
// invalid or open to group or others after it loaded, every request is refused until it is fixed.
type TokenTable struct {
	file     string
	logger   *slog.Logger
	now      func() time.Time
	interval time.Duration

	lastCheck atomic.Int64
	mu        sync.RWMutex
	tokens    []Token
	state     fileState
	broken    bool
}

// fileState identifies the table file version that was loaded: its content hash and mode.
type fileState struct {
	sum  [sha256.Size]byte
	perm os.FileMode
}

// NewTokenTable loads the file once and fails if it is not valid.
func NewTokenTable(file string, logger *slog.Logger, now func() time.Time) (*TokenTable, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	table := &TokenTable{file: file, logger: logger, now: now, interval: tokenFileCheckInterval}
	tokens, state, err := table.read()
	if err != nil {
		return nil, err
	}
	table.tokens, table.state = tokens, state
	table.lastCheck.Store(time.Now().UnixNano())
	return table, nil
}

// StaticTokenTable is a table that never reloads (tests, embedders).
func StaticTokenTable(tokens []Token, now func() time.Time) *TokenTable {
	if now == nil {
		now = time.Now
	}
	return &TokenTable{tokens: tokens, now: now, logger: slog.New(slog.DiscardHandler)}
}

// read loads and parses the file. The state is returned even when the content is invalid, so
// an unchanged invalid file is not parsed and logged again.
func (t *TokenTable) read() ([]Token, fileState, error) {
	info, err := os.Stat(t.file)
	if err != nil {
		return nil, fileState{}, fmt.Errorf("token table %s cannot be read", t.file)
	}
	if !info.Mode().IsRegular() || info.Size() > maxTokenFile {
		return nil, fileState{}, fmt.Errorf("token table %s must be a regular file under %d bytes", t.file, maxTokenFile)
	}
	data, err := os.ReadFile(t.file)
	if err != nil {
		return nil, fileState{}, fmt.Errorf("token table %s cannot be read", t.file)
	}
	state := fileState{sum: sha256.Sum256(data), perm: info.Mode().Perm()}
	if runtime.GOOS != "windows" && state.perm&0o077 != 0 {
		return nil, state, fmt.Errorf("token table %s must not be accessible by group or others (chmod 600)", t.file)
	}
	tokens, err := ParseTokens(data)
	if err != nil {
		return nil, state, fmt.Errorf("%s: %w", t.file, err)
	}
	return tokens, state, nil
}

// refresh looks at the file when the check interval has passed.
func (t *TokenTable) refresh() {
	if t.file == "" {
		return
	}
	last := t.lastCheck.Load()
	if time.Duration(time.Now().UnixNano()-last) < t.interval {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastCheck.Load() != last {
		return
	}
	t.lastCheck.Store(time.Now().UnixNano())
	tokens, state, err := t.read()
	if err != nil {
		if !t.broken || state != t.state {
			t.logger.Error("remote api token table is not usable; refusing every request", "error", err.Error())
		}
		t.tokens, t.state, t.broken = nil, state, true
		return
	}
	if state == t.state && !t.broken {
		return
	}
	t.tokens, t.state, t.broken = tokens, state, false
	t.logger.Info("remote api token table reloaded", "tokens", len(tokens))
}

// Authenticate finds the token for an Authorization header value. The returned token is a
// copy. Revoked and expired tokens are refused with the same error as unknown ones; the
// reason is logged with the token id (never the secret).
func (t *TokenTable) Authenticate(header string) (Token, error) {
	secret, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || strings.TrimSpace(secret) == "" {
		return Token{}, errNoToken
	}
	candidate := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	t.refresh()
	t.mu.RLock()
	tokens, broken := t.tokens, t.broken
	t.mu.RUnlock()
	if broken {
		return Token{}, errTableBroken
	}
	var found *Token
	for index := range tokens {
		if subtle.ConstantTimeCompare(candidate[:], tokens[index].hash[:]) == 1 {
			found = &tokens[index]
		}
	}
	if found == nil {
		return Token{}, errBadToken
	}
	switch {
	case found.Revoked:
		t.logger.Warn("remote api: revoked token used", "token", found.ID)
		return Token{}, errBadToken
	case !found.Expires.IsZero() && !t.now().Before(found.Expires):
		t.logger.Warn("remote api: expired token used", "token", found.ID)
		return Token{}, errBadToken
	}
	return *found, nil
}
