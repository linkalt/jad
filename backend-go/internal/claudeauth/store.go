package claudeauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"jad/backend-go/internal/fserr"
	"jad/backend-go/internal/sessionlock"
)

// ExpiresAtLayout is the time format of expires_at and last_refresh in the file.
const ExpiresAtLayout = time.RFC3339

// File is the credential file: one flat JSON object. Keys this tool does not know are kept and
// written back. Its text forms never show a token.
//
//	{"version":1,"access_token":"…","refresh_token":"…","expires_at":"2026-10-08T05:00:00Z",
//	 "scope":"…","last_refresh":"2026-10-08T04:00:00Z"}
type File struct {
	Version      int
	AccessToken  string
	RefreshToken string
	// ExpiresAt is when the issuer said the access token ends, as written in the file.
	ExpiresAt string
	Scope     string
	// LastRefresh is the RFC 3339 time the tokens were stored, as written in the file.
	LastRefresh string

	top map[string]json.RawMessage
}

func (File) String() string   { return "claudeauth.File{redacted}" }
func (File) GoString() string { return "claudeauth.File{redacted}" }

// ErrNoFile is returned by Store.Read when the credential file does not exist.
var ErrNoFile = errors.New("credential file does not exist")

// parseFile decodes the file. The decoder's own error text can quote file content, so it is not
// repeated.
func parseFile(data []byte) (*File, error) {
	file := &File{top: map[string]json.RawMessage{}}
	if err := json.Unmarshal(data, &file.top); err != nil {
		return nil, errors.New("credential file is not a JSON object")
	}
	var version float64
	if raw, ok := file.top["version"]; ok {
		_ = json.Unmarshal(raw, &version)
	}
	file.Version = int(version)
	file.AccessToken = stringKey(file.top, "access_token")
	file.RefreshToken = stringKey(file.top, "refresh_token")
	file.ExpiresAt = stringKey(file.top, "expires_at")
	file.Scope = stringKey(file.top, "scope")
	file.LastRefresh = stringKey(file.top, "last_refresh")
	return file, nil
}

func stringKey(object map[string]json.RawMessage, key string) string {
	var value string
	if raw, ok := object[key]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}

// Apply returns a copy of the file holding the new tokens. A refresh token or scope the issuer
// did not send keeps its old value; the access token and its expiry are always replaced.
func (f *File) Apply(tokens *Tokens, now time.Time) (*File, error) {
	next := &File{top: map[string]json.RawMessage{}}
	if f != nil {
		for key, value := range f.top {
			next.top[key] = value
		}
		next.RefreshToken, next.Scope = f.RefreshToken, f.Scope
	}
	next.Version = FileVersion
	next.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		next.RefreshToken = tokens.RefreshToken
	}
	if tokens.Scope != "" {
		next.Scope = tokens.Scope
	}
	next.ExpiresAt = now.Add(time.Duration(tokens.ExpiresIn) * time.Second).UTC().Format(ExpiresAtLayout)
	next.LastRefresh = now.UTC().Format(ExpiresAtLayout)
	if err := next.validate(); err != nil {
		return nil, err
	}
	return next, nil
}

// validate applies the checks the adapter's reader applies, so a file this tool writes is one the
// adapter can load.
func (f *File) validate() error {
	if f.AccessToken == "" {
		return errors.New("no access token to store")
	}
	if _, err := time.Parse(ExpiresAtLayout, f.ExpiresAt); err != nil {
		return errors.New("no valid expiry to store")
	}
	return nil
}

func (f *File) encode() ([]byte, error) {
	top := make(map[string]json.RawMessage, len(f.top)+6)
	for key, value := range f.top {
		top[key] = value
	}
	version, _ := json.Marshal(f.Version)
	top["version"] = version
	for key, value := range map[string]string{
		"access_token": f.AccessToken, "refresh_token": f.RefreshToken, "expires_at": f.ExpiresAt,
		"scope": f.Scope, "last_refresh": f.LastRefresh,
	} {
		if value == "" {
			delete(top, key)
			continue
		}
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
		return nil, fmt.Errorf("read %s: %w", s.Path, fserr.UnwrapPathError(err))
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
	// The lock helper would create a missing directory with mode 0755; make it private first.
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.Path), fserr.UnwrapPathError(err))
	}
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
			return fmt.Errorf("read %s for the backup: %w", s.Path, fserr.UnwrapPathError(err))
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
	// complete either way.
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		dirHandle.Close()
	}
	return nil
}
