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
	"sync"

	"jad/backend-go/internal/catalog"
)

// CatalogKind names a catalog whose files the server may read, write and delete.
type CatalogKind string

const (
	CatalogProfiles CatalogKind = "profile"
	CatalogModels   CatalogKind = "model"
	CatalogToolsets CatalogKind = "toolset"
	CatalogMCP      CatalogKind = "mcp"
	// CatalogSandboxes are the sandbox templates, flat files <name>.json with flat IDs.
	CatalogSandboxes CatalogKind = "sandbox"
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
	// Name is the display name the file sets; empty when unset.
	Name string
	// Efforts are the reasoning efforts a model file lists; nil for other kinds and when none.
	Efforts []string
	// Error is set when the file could not be read; the entry then has no ETag, so it can
	// still be listed, opened and replaced while the other files of its kind are used.
	Error string
	// scope is the profile selection of a session file, read for ProfileSessions; zero for other kinds.
	scope catalog.Selection
}

// SessionRef names a session in a listing.
type SessionRef struct {
	ID   string
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
	if directory, scoped, err := l.scopedCatalogDir(kind); scoped || err != nil {
		return directory, err
	}
	switch kind {
	case CatalogProfiles:
		return l.Profiles, nil
	case CatalogModels:
		return l.Models, nil
	case CatalogToolsets:
		return l.Tools, nil
	case CatalogMCP:
		return l.MCP, nil
	case CatalogSandboxes:
		if l.Sandbox == "" {
			return "", &CatalogError{Kind: CatalogNotFound, Message: "the sandbox template folder is taken from each session's cwd (catalogs.sandbox {cwd}/...); edit its files in the project"}
		}
		return l.Sandbox, nil
	}
	return "", &CatalogError{Kind: CatalogBadID, Message: fmt.Sprintf("unknown catalog %q", kind)}
}

func (l Layout) catalogPath(kind CatalogKind, id string) (string, error) {
	directory, err := l.catalogDir(kind)
	if err != nil {
		return "", err
	}
	if kind == CatalogSandboxes {
		if strings.Contains(id, "/") || catalog.ValidateIdentifier(id) != nil {
			return "", &CatalogError{Kind: CatalogBadID, Message: fmt.Sprintf("sandbox template %q must be a flat <name>: letters, digits, '-', '_' and '.', starting with a letter or digit", id)}
		}
		return filepath.Join(directory, id+SandboxTemplateFileSuffix), nil
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
	if kind == CatalogSandboxes {
		var infos []CatalogFileInfo
		for _, name := range ListSandboxTemplates(directory) {
			data, err := readRegularFile(filepath.Join(directory, name+SandboxTemplateFileSuffix), maxInternalJSONFileBytes)
			if err != nil {
				continue
			}
			infos = append(infos, CatalogFileInfo{ID: name, ETag: CatalogETag(data)})
		}
		return infos, nil
	}
	entries, err := catalog.Scan(string(kind), directory)
	if err != nil {
		return nil, configError("%v", err)
	}
	infos := make([]CatalogFileInfo, 0, entries.Len())
	for _, entry := range entries.Entries() {
		data, err := readRegularFile(entry.Path, maxInternalJSONFileBytes)
		if err != nil {
			// One file that cannot be read must not hide the others, or no file could be fixed.
			infos = append(infos, CatalogFileInfo{ID: entry.ID(), Error: configError("%s: could not be read: %v", entry.Path, err).Error()})
			continue
		}
		info := CatalogFileInfo{ID: entry.ID(), ETag: CatalogETag(data), Name: fileDisplayName(data)}
		if kind == CatalogModels {
			info.Efforts = fileEfforts(data)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// fileDisplayName is the trimmed "name" a file sets. A file that does not parse, or sets a name
// that is not a string, has none, so it is still listed and can be opened and repaired.
func fileDisplayName(data []byte) string {
	var named struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(data, &named)
	return strings.TrimSpace(named.Name)
}

// fileEfforts is the efforts a model file lists, with the leniency of fileDisplayName: a list
// that is not all strings counts as none, so the file is still listed.
func fileEfforts(data []byte) []string {
	var model struct {
		Efforts []string `json:"efforts"`
	}
	if json.Unmarshal(data, &model) != nil || len(model.Efforts) == 0 {
		return nil
	}
	return model.Efforts
}

// ReadCatalogFile returns one file as it is on disk.
func ReadCatalogFile(layout Layout, kind CatalogKind, id string) (CatalogFile, error) {
	path, err := layout.catalogPath(kind, id)
	if err != nil {
		return CatalogFile{}, err
	}
	data, err := readRegularFile(path, maxInternalJSONFileBytes)
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
	if kind == CatalogProfiles {
		// The writers of a profile file share one lock (a profile's own edit included).
		unlock, err := lockProfileFile(path)
		if err != nil {
			return CatalogFileInfo{}, false, err
		}
		defer unlock()
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
	current, err := readRegularFile(path, maxInternalJSONFileBytes)
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
	if kind == CatalogProfiles {
		unlock, err := lockProfileFile(path)
		if err != nil {
			return err
		}
		defer unlock()
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
	var users []string
	for _, target := range layout.usersOf(roots) {
		found, err := catalogUsers(target.layout, target.roots, kind, id, path)
		if err != nil {
			return err
		}
		users = appendNewStrings(users, found...)
	}
	if len(users) > 0 {
		return &CatalogError{Kind: CatalogInUse, Message: inUseMessage(kind, id, users)}
	}
	if kind == CatalogSandboxes {
		return os.Remove(path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	// Removing a directory succeeds only when it is empty, so a file created meanwhile stays.
	categoryDirectoryMu.Lock()
	_ = os.Remove(filepath.Dir(path))
	categoryDirectoryMu.Unlock()
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
	case CatalogSandboxes:
		_, err := parseSandboxTemplate(path, data)
		return err
	}
	return nil
}

// validateModelFile applies the rules the loader applies to a model file read as a whole
// model: based_on is refused (loadModel).
func validateModelFile(path string, data []byte) error {
	fields, err := parseSessionObject(path, data)
	if err != nil {
		return err
	}
	_, err = modelFromFields(fields, path, "", "")
	return err
}

// modelUsers lists the profiles that stop resolving when the model file at path is removed:
// their models selection or model_default names it, or the selection loses its last member of
// a category, or one of their own models is based on it (phase 3c). Each profile is judged with
// its own catalog (Layout.profileModels). A profile that does not resolve today is not counted.
func modelUsers(layout Layout, id, path string) ([]string, error) {
	profiles, err := catalog.Scan("profile", layout.Profiles)
	if err != nil {
		return nil, configError("%v", err)
	}
	var users []string
	// One memo for every profile: the shared folders are scanned once.
	scans := NewCatalogScans()
	for _, entry := range profiles.Entries() {
		own, err := layout.profileModels(scans, entry.ID())
		if err != nil {
			return nil, configError("%v", err)
		}
		if base := own.basedOn(path); base != "" {
			users = append(users, entry.ID()+" (its own model "+base+" is based on it)")
			continue
		}
		var profile layoutProfileFile
		if err := readLayoutFile(entry.Path, &profile); err != nil {
			continue
		}
		if _, _, err := checkProfileWith(entry.Path, profile, own.catalog, own.effort); err != nil {
			continue
		}
		if _, _, err := checkProfileWith(entry.Path, profile, own.catalog.Without(id), own.effort); err != nil {
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
	// The profile is judged with its own models beside the shared ones (phase 3c).
	own, err := layout.profileModels(nil, layout.profileIDOf(path))
	if err != nil {
		return configError("%s: %v", path, err)
	}
	if _, _, err := checkProfileWith(path, profile, own.catalog, own.effort); err != nil {
		return err
	}
	if err := checkUnselectedProfile(path, profile); err != nil {
		return err
	}
	if len(profile.Toolset) > 0 {
		// Its own toolsets are looked up beside the shared ones (phase 3c.3).
		toolsets, err := layout.profileToolsets(nil, layout.profileIDOf(path))
		if err != nil {
			return configError("%s: %v", path, err)
		}
		for _, id := range profile.Toolset {
			if _, err := toolsets.catalog.Lookup(id); err != nil {
				return configError("%s: toolset: %v", path, err)
			}
		}
	}
	return nil
}

// profileUsers lists the sessions that stop loading when the profile is removed: it is in
// the session's scope and the scope no longer resolves without it.
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
				_, err = left.Lookup(session.defaultProfile(left))
			}
			if err != nil {
				users = append(users, location.Reference)
			}
		}
	}
	return users, nil
}

const createDirectoryAttempts = 32

// categoryDirectoryMu orders a creator's MkdirAll+CreateTemp against a deleter's removal of the
// then-empty category directory inside this process. Another process sharing the directory is
// not covered by it; createTempInCategory's retry is what handles that case.
var categoryDirectoryMu sync.Mutex

// createTempInCategory makes the file's directory and a temporary file in it. Once the
// temporary file exists the directory is not empty, so a delete cannot remove it. Another
// process may still remove the directory in between; each such removal needs a file that
// existed, so the retry makes progress, and the bound is a guard.
func createTempInCategory(path string) (*os.File, error) {
	for attempt := 1; ; attempt++ {
		file, err := func() (*os.File, error) {
			categoryDirectoryMu.Lock()
			defer categoryDirectoryMu.Unlock()
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
			return os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
		}()
		if err == nil {
			return file, nil
		}
		if attempt >= createDirectoryAttempts || !(errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist)) {
			return nil, err
		}
	}
}

// createFileExclusive stores data at path only if nothing is there: the contents go to a
// temporary file that is then hard-linked into place, so a concurrent creator loses.
func createFileExclusive(path string, data []byte, syncWrite bool) error {
	file, err := createTempInCategory(path)
	if err != nil {
		return err
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

// catalogUsers lists what still needs the file id of kind in one layout: the sessions for a
// profile, the profiles for a model or toolset, the toolsets for an MCP server, the
// defaults, sessions and profiles for a sandbox template.
func catalogUsers(layout Layout, roots []Root, kind CatalogKind, id, path string) ([]string, error) {
	switch kind {
	case CatalogProfiles:
		return profileUsers(layout, roots, id)
	case CatalogModels:
		return modelUsers(layout, id, path)
	case CatalogToolsets:
		return toolsetUsers(layout, id, path)
	case CatalogSandboxes:
		return sandboxUsers(layout, roots, id)
	case CatalogMCP:
		return serverUsers(layout, id)
	}
	return nil, nil
}

func inUseMessage(kind CatalogKind, id string, users []string) string {
	switch kind {
	case CatalogProfiles:
		return fmt.Sprintf("profile %q is needed by session %s", id, strings.Join(users, ", "))
	case CatalogModels:
		return fmt.Sprintf("model %q is needed by profile %s", id, strings.Join(users, ", "))
	case CatalogToolsets:
		return fmt.Sprintf("toolset %q is needed by profile %s", id, strings.Join(users, ", "))
	case CatalogSandboxes:
		return fmt.Sprintf("sandbox template %q is named by %s", id, strings.Join(users, ", "))
	default:
		return fmt.Sprintf("MCP server %q is needed by toolset %s", id, strings.Join(users, ", "))
	}
}
