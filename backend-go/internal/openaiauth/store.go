package openaiauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"jad/backend-go/internal/sessionlock"
)

// File is the credential file. Keys this tool does not know are kept and written back, because
// the Codex CLI and the openai-oauth library write the same layout and may add keys.
// Its text forms never show a token.
type File struct {
	AuthMode     string
	AccessToken  string
	IDToken      string
	RefreshToken string
	AccountID    string
	// LastRefresh is the RFC 3339 time the tokens were stored, as written in the file.
	LastRefresh string

	top    map[string]json.RawMessage
	tokens map[string]json.RawMessage
}

func (File) String() string   { return "openaiauth.File{redacted}" }
func (File) GoString() string { return "openaiauth.File{redacted}" }

// ErrNoFile is returned by Store.Read when the credential file does not exist.
var ErrNoFile = errors.New("credential file does not exist")

// parseFile decodes the file. The decoder's own error text can quote file content, so it is not
// repeated.
func parseFile(data []byte) (*File, error) {
	file := &File{top: map[string]json.RawMessage{}, tokens: map[string]json.RawMessage{}}
	if err := json.Unmarshal(data, &file.top); err != nil {
		return nil, errors.New("credential file is not a JSON object")
	}
	if raw, ok := file.top["tokens"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &file.tokens); err != nil {
			return nil, errors.New("credential file: tokens is not a JSON object")
		}
	}
	file.AuthMode = stringKey(file.top, "auth_mode")
	file.LastRefresh = stringKey(file.top, "last_refresh")
	file.AccessToken = stringKey(file.tokens, "access_token")
	file.IDToken = stringKey(file.tokens, "id_token")
	file.RefreshToken = stringKey(file.tokens, "refresh_token")
	file.AccountID = stringKey(file.tokens, "account_id")
	return file, nil
}

func stringKey(object map[string]json.RawMessage, key string) string {
	var value string
	if raw, ok := object[key]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}

// Apply returns a copy of the file holding the new tokens. A token the issuer did not send keeps
// its old value, except the access token which is always replaced. now is the stored time.
func (f *File) Apply(tokens *Tokens, now time.Time) (*File, error) {
	next := &File{top: map[string]json.RawMessage{}, tokens: map[string]json.RawMessage{}}
	if f != nil {
		for key, value := range f.top {
			next.top[key] = value
		}
		for key, value := range f.tokens {
			next.tokens[key] = value
		}
		next.IDToken, next.RefreshToken, next.AccountID = f.IDToken, f.RefreshToken, f.AccountID
	}
	next.AuthMode = AuthModeChatGPT
	next.AccessToken = tokens.AccessToken
	if tokens.IDToken != "" {
		next.IDToken = tokens.IDToken
	}
	if tokens.RefreshToken != "" {
		next.RefreshToken = tokens.RefreshToken
	}
	if tokens.AccountID != "" {
		next.AccountID = tokens.AccountID
	}
	if next.AccountID == "" {
		next.AccountID = accountIDFrom(next.IDToken, next.AccessToken)
	}
	next.LastRefresh = now.UTC().Format("2006-01-02T15:04:05.000Z")
	if err := next.validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// validate applies the checks the backend reader applies, so a file this tool writes is one the
// backend can load.
func (f *File) validate() error {
	if f.AccessToken == "" {
		return errors.New("no access token to store")
	}
	if f.AccountID == "" {
		return errors.New("no account id: the response and the tokens carry none")
	}
	return nil
}

func (f *File) encode() ([]byte, error) {
	top := make(map[string]json.RawMessage, len(f.top)+3)
	for key, value := range f.top {
		top[key] = value
	}
	tokens := make(map[string]json.RawMessage, len(f.tokens)+4)
	for key, value := range f.tokens {
		tokens[key] = value
	}
	for key, value := range map[string]string{
		"id_token": f.IDToken, "access_token": f.AccessToken, "refresh_token": f.RefreshToken, "account_id": f.AccountID,
	} {
		if value == "" {
			delete(tokens, key)
			continue
		}
		raw, _ := json.Marshal(value)
		tokens[key] = raw
	}
	rawTokens, err := json.Marshal(tokens)
	if err != nil {
		return nil, err
	}
	top["tokens"] = rawTokens
	for key, value := range map[string]string{"auth_mode": f.AuthMode, "last_refresh": f.LastRefresh} {
		raw, _ := json.Marshal(value)
		top[key] = raw
	}
	return json.MarshalIndent(top, "", "  ")
}

// Store reads and writes the credential file.
type Store struct {
	Path string
	// KeepBackup keeps the previous file as <Path>.bak before each rewrite.
	KeepBackup bool
}

// BackupPath is where the previous file is kept.
func (s *Store) BackupPath() string { return s.Path + ".bak" }

// Read returns the file, or an error wrapping ErrNoFile when it does not exist.
func (s *Store) Read() (*File, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", s.Path, ErrNoFile)
		}
		return nil, fmt.Errorf("read %s: %w", s.Path, errors.Unwrap(err))
	}
	file, err := parseFile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	return file, nil
}

// Update runs change under an exclusive lock, so two runs of the tool cannot spend the same
// rotating refresh token twice. change receives the current file, or nil when there is none, and
// returns the file to store. The result is checked and written atomically, and the previous file
// is kept as a backup when KeepBackup is set. When change returns an error nothing is written.
func (s *Store) Update(ctx context.Context, change func(current *File) (*File, error)) error {
	lockPath, err := sessionlock.PathFor(s.Path)
	if err != nil {
		return err
	}
	lock, err := sessionlock.Acquire(ctx, lockPath)
	if err != nil {
		return fmt.Errorf("lock %s: %w", lockPath, err)
	}
	defer lock.Close()

	current, err := s.Read()
	if err != nil {
		if !errors.Is(err, ErrNoFile) {
			return err
		}
		current = nil
	}
	next, err := change(current)
	if err != nil {
		return err
	}
	if err := next.validate(); err != nil {
		return err
	}
	data, err := next.encode()
	if err != nil {
		return fmt.Errorf("encode credential file: %w", err)
	}
	if current != nil && s.KeepBackup {
		previous, err := os.ReadFile(s.Path)
		if err != nil {
			return fmt.Errorf("read %s for the backup: %w", s.Path, errors.Unwrap(err))
		}
		if err := writeAtomic(s.BackupPath(), previous); err != nil {
			return fmt.Errorf("write backup %s: %w", s.BackupPath(), err)
		}
	}
	if err := writeAtomic(s.Path, data); err != nil {
		return fmt.Errorf("write %s: %w", s.Path, err)
	}
	return nil
}

// writeAtomic writes data to a private temporary file in the same directory, syncs it and renames
// it over path, so a reader sees either the old file or the new one, never a part.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := func() { _ = os.Remove(tempPath) }
	// os.CreateTemp creates the file with mode 0600, so the credential is never readable by others.
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		cleanup()
		return err
	}
	// Syncing the directory makes the rename durable. Some platforms cannot do it; the file is
	// already replaced, so a failure here is not an error.
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}
