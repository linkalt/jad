package sessionfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/catalog"
)

// CatalogKind names a catalog whose files the server may read, write and delete.
type CatalogKind string

const (
	CatalogProfiles CatalogKind = "profile"
	CatalogModels   CatalogKind = "model"
	CatalogToolsets CatalogKind = "toolset"
	CatalogMCP      CatalogKind = "mcp"
)

// ServerFileValidator checks the contents of an MCP server file. The loader for those files
// lives in a package that imports this one, so the caller supplies it.
type ServerFileValidator func(path string, data []byte) error

// CatalogErrorKind says which precondition a catalog file operation failed.
type CatalogErrorKind int

const (
	CatalogNotFound CatalogErrorKind = iota
	CatalogBadID
	CatalogPreconditionFailed
	CatalogPreconditionRequired
	CatalogInUse
)

// CatalogError is a refused catalog file operation that the client can fix.
type CatalogError struct {
	Kind    CatalogErrorKind
	Message string
}

func (e *CatalogError) Error() string { return e.Message }

// CatalogFileInfo identifies one catalog file and the version of its bytes.
type CatalogFileInfo struct {
	ID   string
	ETag string
	// Name is the display name a session or profile file sets; empty for every other kind and when unset.
	Name string
}

// CatalogFile is one catalog file with its bytes.
type CatalogFile struct {
	CatalogFileInfo
	Data []byte
}

// WriteCondition is the request's precondition: IfMatch for a file that exists,
// IfNoneMatch "*" to create one. A write with neither is refused.
type WriteCondition struct {
	IfMatch     string
	IfNoneMatch string
}

// CatalogETag is the strong entity tag of a file's bytes.
func CatalogETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// IfMatchSatisfied reports whether an If-Match header names etag. Only the strong form
// matches; "*" matches any file that exists.
func IfMatchSatisfied(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

func (l Layout) catalogDir(kind CatalogKind) (string, error) {
	switch kind {
	case CatalogProfiles:
		return l.Profiles, nil
	case CatalogModels:
		return l.Models, nil
	case CatalogToolsets:
		return l.Tools, nil
	case CatalogMCP:
		return l.MCP, nil
	}
	return "", &CatalogError{Kind: CatalogBadID, Message: fmt.Sprintf("unknown catalog %q", kind)}
}

func (l Layout) catalogPath(kind CatalogKind, id string) (string, error) {
	directory, err := l.catalogDir(kind)
	if err != nil {
		return "", err
	}
	category, name, ok := strings.Cut(id, "/")
	if !ok || catalog.ValidateIdentifier(category) != nil || catalog.ValidateIdentifier(name) != nil {
		return "", &CatalogError{Kind: CatalogBadID, Message: fmt.Sprintf("%s %q must be written <category>/<name>", kind, id)}
	}
	return filepath.Join(directory, category, name+".json"), nil
}

// ListCatalogFiles lists the files of one catalog with the version of each.
func ListCatalogFiles(layout Layout, kind CatalogKind) ([]CatalogFileInfo, error) {
	directory, err := layout.catalogDir(kind)
	if err != nil {
		return nil, err
	}
	entries, err := catalog.Scan(string(kind), directory)
	if err != nil {
		return nil, configError("%v", err)
	}
	infos := make([]CatalogFileInfo, 0, entries.Len())
	for _, entry := range entries.Entries() {
		data, err := os.ReadFile(entry.Path)
		if err != nil {
			return nil, configError("%s: could not be read: %v", entry.Path, err)
		}
		info := CatalogFileInfo{ID: entry.ID(), ETag: CatalogETag(data)}
		if kind == CatalogProfiles {
			info.Name = fileDisplayName(data)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// fileDisplayName is the "name" a file sets. A file that does not parse, or sets a name
// that is not a string, has none, so it is still listed and can be opened and repaired.
func fileDisplayName(data []byte) string {
	var named struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(data, &named)
	return named.Name
}

// ReadCatalogFile returns one file as it is on disk.
func ReadCatalogFile(layout Layout, kind CatalogKind, id string) (CatalogFile, error) {
	path, err := layout.catalogPath(kind, id)
	if err != nil {
		return CatalogFile{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return CatalogFile{}, &CatalogError{Kind: CatalogNotFound, Message: fmt.Sprintf("%s %q does not exist", kind, id)}
	}
	if err != nil {
		return CatalogFile{}, configError("%s: could not be read: %v", path, err)
	}
	return CatalogFile{CatalogFileInfo: CatalogFileInfo{ID: id, ETag: CatalogETag(data)}, Data: data}, nil
}

// WriteCatalogFile checks data with the loader's own rules for the kind and stores it. The
// condition must hold: IfNoneMatch "*" creates a file that does not exist, IfMatch replaces
// the version it names. The replacement is a temporary file and a rename, and it is refused
// when the file changed while it was being written. The result says whether the file is new.
func WriteCatalogFile(layout Layout, roots []Root, kind CatalogKind, id string, data []byte, condition WriteCondition, syncWrite bool, validateServer ServerFileValidator) (CatalogFileInfo, bool, error) {
	path, err := layout.catalogPath(kind, id)
	if err != nil {
		return CatalogFileInfo{}, false, err
	}
	exists, before, err := checkWritePrecondition(path, string(kind), id, condition)
	if err != nil {
		return CatalogFileInfo{}, false, err
	}
	if err := validateCatalogFile(layout, roots, kind, path, data, validateServer); err != nil {
		return CatalogFileInfo{}, false, err
	}
	if exists {
		if err := writeFileIfUnchanged(path, data, before, syncWrite); err != nil {
			return CatalogFileInfo{}, false, err
		}
	} else if err := createFileExclusive(path, data, syncWrite); err != nil {
		return CatalogFileInfo{}, false, err
	}
	return CatalogFileInfo{ID: id, ETag: CatalogETag(data)}, !exists, nil
}

// checkWritePrecondition applies the request's precondition to the file at path: a file that
// exists needs the IfMatch version, one that does not needs IfNoneMatch "*". It returns
// whether the file exists and, if it does, what it looked like, for writeFileIfUnchanged.
func checkWritePrecondition(path, kind, id string, condition WriteCondition) (bool, os.FileInfo, error) {
	before, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return false, nil, configError("%s: could not be read: %v", path, statErr)
	}
	if statErr != nil {
		if condition.IfMatch != "" {
			return false, nil, &CatalogError{Kind: CatalogPreconditionFailed, Message: fmt.Sprintf("%s %q does not exist", kind, id)}
		}
		if condition.IfNoneMatch != "*" {
			return false, nil, &CatalogError{Kind: CatalogPreconditionRequired, Message: `If-None-Match: * is required to create a file`}
		}
		return false, nil, nil
	}
	if condition.IfNoneMatch != "" {
		return false, nil, &CatalogError{Kind: CatalogPreconditionFailed, Message: fmt.Sprintf("%s %q already exists", kind, id)}
	}
	if condition.IfMatch == "" {
		return false, nil, &CatalogError{Kind: CatalogPreconditionRequired, Message: "If-Match is required to replace a file"}
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return false, nil, configError("%s: could not be read: %v", path, err)
	}
	if !IfMatchSatisfied(condition.IfMatch, CatalogETag(current)) {
		return false, nil, &CatalogError{Kind: CatalogPreconditionFailed, Message: fmt.Sprintf("%s %q changed since it was read; read it again", kind, id)}
	}
	return true, before, nil
}

// DeleteCatalogFile removes the version of a file that IfMatch names. A profile that a
// session needs is refused with the sessions listed, and so is a model that a profile
// needs, with the profiles listed.
func DeleteCatalogFile(layout Layout, roots []Root, kind CatalogKind, id, ifMatch string) error {
	path, err := layout.catalogPath(kind, id)
	if err != nil {
		return err
	}
	current, err := ReadCatalogFile(layout, kind, id)
	if err != nil {
		return err
	}
	if ifMatch == "" {
		return &CatalogError{Kind: CatalogPreconditionRequired, Message: "If-Match is required to delete a file"}
	}
	if !IfMatchSatisfied(ifMatch, current.ETag) {
		return &CatalogError{Kind: CatalogPreconditionFailed, Message: fmt.Sprintf("%s %q changed since it was read; read it again", kind, id)}
	}
	if kind == CatalogProfiles {
		users, err := profileUsers(layout, roots, id)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &CatalogError{Kind: CatalogInUse, Message: fmt.Sprintf("profile %q is needed by session %s", id, strings.Join(users, ", "))}
		}
	}
	if kind == CatalogModels {
		users, err := modelUsers(layout, id)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &CatalogError{Kind: CatalogInUse, Message: fmt.Sprintf("model %q is needed by profile %s", id, strings.Join(users, ", "))}
		}
	}
	if kind == CatalogToolsets {
		users, err := toolsetUsers(layout, id)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &CatalogError{Kind: CatalogInUse, Message: fmt.Sprintf("toolset %q is needed by profile %s", id, strings.Join(users, ", "))}
		}
	}
	if kind == CatalogMCP {
		users, err := serverUsers(layout, id)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return &CatalogError{Kind: CatalogInUse, Message: fmt.Sprintf("MCP server %q is needed by toolset %s", id, strings.Join(users, ", "))}
		}
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	// Removing a directory succeeds only when it is empty, so a file created meanwhile stays.
	_ = os.Remove(filepath.Dir(path))
	return nil
}

func validateCatalogFile(layout Layout, roots []Root, kind CatalogKind, path string, data []byte, validateServer ServerFileValidator) error {
	switch kind {
	case CatalogProfiles:
		return validateProfileFile(layout, path, data)
	case CatalogModels:
		return validateModelFile(path, data)
	case CatalogToolsets:
		return validateToolsetFile(layout, roots, path, data)
	case CatalogMCP:
		if validateServer == nil {
			return configError("%s: no MCP server file validator is configured", path)
		}
		return validateServer(path, data)
	}
	return nil
}

// validateModelFile applies the rules the loader applies to a model file the run uses.
func validateModelFile(path string, data []byte) error {
	fields, err := parseSessionObject(path, data)
	if err != nil {
		return err
	}
	_, err = modelFromFields(fields, path, "", "")
	return err
}

// modelUsers lists the profiles that stop resolving when the model is removed: their models
// selection or model_default names it, or the selection loses its last member of a category.
// A profile that does not resolve today is not counted.
func modelUsers(layout Layout, id string) ([]string, error) {
	all, err := catalog.Scan("model", layout.Models)
	if err != nil {
		return nil, configError("%v", err)
	}
	remaining := all.Without(id)
	profiles, err := catalog.Scan("profile", layout.Profiles)
	if err != nil {
		return nil, configError("%v", err)
	}
	var users []string
	for _, entry := range profiles.Entries() {
		var profile layoutProfileFile
		if err := readLayoutFile(entry.Path, &profile); err != nil {
			continue
		}
		if _, _, err := checkProfile(entry.Path, profile, all); err != nil {
			continue
		}
		if _, _, err := checkProfile(entry.Path, profile, remaining); err != nil {
			users = append(users, entry.ID())
		}
	}
	return users, nil
}

// validateProfileFile applies what the loader applies to every profile of a session's scope,
// and checks that the toolset it names exists.
func validateProfileFile(layout Layout, path string, data []byte) error {
	var profile layoutProfileFile
	if err := parseLayoutFile(path, data, &profile); err != nil {
		return err
	}
	models, err := catalog.Scan("model", layout.Models)
	if err != nil {
		return configError("%s: %v", path, err)
	}
	if _, _, err := checkProfile(path, profile, models); err != nil {
		return err
	}
	if err := checkUnselectedProfile(path, profile); err != nil {
		return err
	}
	if profile.Toolset != "" {
		toolsets, err := catalog.Scan("toolset", layout.Tools)
		if err != nil {
			return configError("%s: %v", path, err)
		}
		if _, err := toolsets.Lookup(profile.Toolset); err != nil {
			return configError("%s: toolset: %v", path, err)
		}
	}
	return nil
}

// profileUsers lists the sessions that stop loading when the profile is removed: it is in
// the session's scope and is its active profile, or the scope no longer resolves without it.
// A session that does not load today is not counted.
func profileUsers(layout Layout, roots []Root, id string) ([]string, error) {
	all, err := catalog.Scan("profile", layout.Profiles)
	if err != nil {
		return nil, configError("%v", err)
	}
	remaining := all.Without(id)
	var users []string
	for _, root := range roots {
		entries, err := os.ReadDir(root.Directory)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read session root %q: %w", root.ID, err)
		}
		for _, entry := range entries {
			sessionID, ok := SessionFileID(entry)
			if !ok {
				continue
			}
			location, err := Resolve(roots, root.ID+":"+sessionID)
			if err != nil || !location.UsesLayout() {
				continue
			}
			session, err := readLayoutSession(location)
			if err != nil {
				continue
			}
			scoped, err := all.Narrow(session.Scope.Profiles)
			if err != nil {
				continue
			}
			if _, inScope := scoped.Get(id); !inScope {
				continue
			}
			left, err := remaining.Narrow(session.Scope.Profiles)
			if err == nil {
				_, err = left.Lookup(session.ActiveProfile)
			}
			if err != nil {
				users = append(users, location.Reference)
			}
		}
	}
	return users, nil
}

const createDirectoryAttempts = 32

// createFileExclusive stores data at path only if nothing is there: the contents go to a
// temporary file that is then hard-linked into place, so a concurrent creator loses.
func createFileExclusive(path string, data []byte, syncWrite bool) error {
	var file *os.File
	// A delete may remove the then-empty category directory between MkdirAll and CreateTemp.
	// Each such removal needs a file that existed, so the loop makes progress; the bound is a guard.
	for attempt := 1; file == nil; attempt++ {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		created, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
		if err == nil {
			file = created
		} else if attempt >= createDirectoryAttempts || !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	temporaryPath := file.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return err
	}
	if syncWrite {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return &CatalogError{Kind: CatalogPreconditionFailed, Message: fmt.Sprintf("%s already exists", path)}
		}
		return err
	}
	return nil
}
