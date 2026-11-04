package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"jad/backend-go/internal/fserr"
	"jad/backend-go/internal/sessionfs"
)

// journal undoes the steps of an install or removal that fails part way, in reverse order,
// and finishes them (drops the backups) once all succeeded.
type journal struct {
	undo   []func() error
	commit []func()
}

func (j *journal) rollback() error {
	var problems []error
	for index := len(j.undo) - 1; index >= 0; index-- {
		if err := j.undo[index](); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func (j *journal) finish() {
	for _, step := range j.commit {
		step()
	}
}

// fail rolls back and returns err, with any rollback failure appended so a half-undone
// state is never reported as clean.
func (j *journal) fail(err error) error {
	if undoErr := j.rollback(); undoErr != nil {
		return fmt.Errorf("%w; undoing the steps already taken also failed: %v", err, undoErr)
	}
	return err
}

// writeCatalog writes one catalog file with the loader's checks, replacing the version on
// disk when there is one, and journals how to put that version back.
func (t Target) writeCatalog(j *journal, kind sessionfs.CatalogKind, id string, data []byte) error {
	path := t.catalogPath(kind, id)
	previous, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, fserr.UnwrapPathError(err))
	}
	condition := sessionfs.WriteCondition{IfNoneMatch: "*"}
	if exists {
		condition = sessionfs.WriteCondition{IfMatch: sessionfs.CatalogETag(previous)}
	}
	if _, _, err := sessionfs.WriteCatalogFile(t.Layout, t.Roots, kind, id, data, condition, true, t.ValidateServer); err != nil {
		return fmt.Errorf("%s %s: %w", kind, id, err)
	}
	j.undo = append(j.undo, func() error {
		if !exists {
			err := os.Remove(path)
			_ = os.Remove(filepath.Dir(path))
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		return replaceFile(path, previous, 0o644)
	})
	return nil
}

// deleteCatalog removes a catalog file through the catalog's own delete, which refuses a
// file that something outside the plugin still needs.
func (t Target) deleteCatalog(j *journal, kind sessionfs.CatalogKind, id string) error {
	path := t.catalogPath(kind, id)
	previous, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, fserr.UnwrapPathError(err))
	}
	if err := sessionfs.DeleteCatalogFile(t.Layout, t.Roots, kind, id, sessionfs.CatalogETag(previous)); err != nil {
		return fmt.Errorf("%s %s: %w", kind, id, err)
	}
	j.undo = append(j.undo, func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return replaceFile(path, previous, 0o644)
	})
	return nil
}

// moveAside renames a folder to a hidden backup beside it; the backup is dropped when the
// journal finishes and renamed back when it rolls back.
func moveAside(j *journal, path string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	backup := hiddenSibling(path, "old")
	if err := os.Rename(path, backup); err != nil {
		return fmt.Errorf("move %s aside: %w", path, fserr.UnwrapPathError(err))
	}
	j.undo = append(j.undo, func() error {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		return os.Rename(backup, path)
	})
	j.commit = append(j.commit, func() { _ = os.RemoveAll(backup) })
	return nil
}

// placeTree copies a folder of the plugin to path, which must not exist: it is built under
// a hidden name and renamed into place.
func placeTree(j *journal, source, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), fserr.UnwrapPathError(err))
	}
	staging := hiddenSibling(path, "new")
	if err := copyTree(source, staging); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, path); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("place %s: %w", path, fserr.UnwrapPathError(err))
	}
	j.undo = append(j.undo, func() error { return os.RemoveAll(path) })
	return nil
}

func hiddenSibling(path, label string) string {
	random := make([]byte, 6)
	_, _ = rand.Read(random)
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+label+"-"+hex.EncodeToString(random))
}

func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(source, path)
		destination := filepath.Join(target, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, info.Mode().Perm()|0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file or folder", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The mode is kept, so a script the plugin ships stays executable.
		return os.WriteFile(destination, data, info.Mode().Perm())
	})
}

// replaceFile writes data to path through a temporary file and a rename.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(name, mode)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// writeRecord stores the record, or removes it when record is nil, and journals the old one.
func (t Target) writeRecord(j *journal, name string, record *Record) error {
	path := t.recordPath(name)
	previous, err := os.ReadFile(path)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, fserr.UnwrapPathError(err))
	}
	if record == nil {
		if existed {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, fserr.UnwrapPathError(err))
			}
		}
	} else {
		data, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return err
		}
		if err := replaceFile(path, append(data, '\n'), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, fserr.UnwrapPathError(err))
		}
	}
	j.undo = append(j.undo, func() error {
		if !existed {
			err := os.Remove(path)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		return replaceFile(path, previous, 0o644)
	})
	return nil
}

// InstallResult says what an install did.
type InstallResult struct {
	Record   Record
	Upgraded *Record
	Removed  []Entry
}

// Install installs the plugin in the folder source, or upgrades the installed plugin of the
// same name. A file the plugin would write that exists and is not the plugin's is refused,
// and so is one the plugin installed that was changed since, unless force is set. Every
// catalog file is checked with the loader's rules as it is written, and a skill is read the
// way the skill tool reads it; any failure undoes the whole install.
func (t Target) Install(ctx context.Context, source string, force bool) (result InstallResult, err error) {
	found, err := readBundle(source)
	if err != nil {
		return InstallResult{}, err
	}
	name := found.manifest.Name
	lock, err := t.lock(ctx, name)
	if err != nil {
		return InstallResult{}, err
	}
	defer lock.Close()
	// A failed install leaves no empty folder of its own behind.
	defer func() {
		if err != nil {
			t.removeEmptyFolders(name)
		}
	}()
	previous, err := t.ReadRecord(name)
	if err != nil {
		return InstallResult{}, err
	}
	owned := map[string]Entry{}
	if previous != nil {
		for _, entry := range previous.Entries {
			owned[entry.Path] = entry
		}
	}

	filesPath := t.filesDir(name)
	replacement := ""
	if encoded, err := json.Marshal(filesPath); err == nil {
		replacement = strings.Trim(string(encoded), `"`)
	}
	var planned []Entry
	for index := range found.catalog {
		file := &found.catalog[index]
		file.data = []byte(strings.ReplaceAll(string(file.data), FilesPlaceholder, replacement))
		planned = append(planned, Entry{Kind: string(file.kind), ID: file.id, Path: t.catalogPath(file.kind, file.id), SHA256: hashBytes(file.data)})
	}
	for _, item := range found.trees {
		if item.kind == kindSkill && t.SkillDir == "" {
			return InstallResult{}, fmt.Errorf("plugin %s has skills, but no skill folder is configured (libraries.skill_dirs)", name)
		}
		if item.kind == kindCommands && t.CommandDir == "" {
			return InstallResult{}, fmt.Errorf("plugin %s has commands, but no command folder is configured (libraries.command_dirs)", name)
		}
		planned = append(planned, Entry{Kind: item.kind, ID: item.id, Path: t.treePath(name, item), SHA256: item.hash})
	}

	// What is on disk where the plugin writes must be the plugin's own, unchanged.
	plannedPaths := map[string]bool{}
	var changed []Entry
	for _, entry := range planned {
		plannedPaths[entry.Path] = true
		hash, err := currentHash(entry)
		if err != nil {
			return InstallResult{}, err
		}
		if hash == "" {
			continue
		}
		mine, isMine := owned[entry.Path]
		if !isMine {
			return InstallResult{}, fmt.Errorf("plugin %s: %s %s exists and was not installed by this plugin: %s", name, entry.Kind, entry.ID, entry.Path)
		}
		if hash != mine.SHA256 {
			changed = append(changed, mine)
		}
	}
	var obsolete []Entry
	if previous != nil {
		for _, entry := range previous.Entries {
			if plannedPaths[entry.Path] {
				continue
			}
			obsolete = append(obsolete, entry)
			hash, err := currentHash(entry)
			if err != nil {
				return InstallResult{}, err
			}
			if hash != "" && hash != entry.SHA256 {
				changed = append(changed, entry)
			}
		}
	}
	if len(changed) > 0 && !force {
		return InstallResult{}, &ChangedError{Plugin: name, Entries: changed}
	}

	j := &journal{}
	for _, item := range found.trees {
		path := t.treePath(name, item)
		if err := moveAside(j, path); err != nil {
			return InstallResult{}, j.fail(err)
		}
		if err := placeTree(j, item.source, path); err != nil {
			return InstallResult{}, j.fail(err)
		}
		if item.kind == kindSkill && t.ReadSkill != nil {
			if err := t.ReadSkill(ctx, t.SkillDir, item.id, t.SkillMaxBytes); err != nil {
				return InstallResult{}, j.fail(fmt.Errorf("plugin %s: skill %s: %w", name, item.id, err))
			}
		}
		if item.kind == kindCommands && t.ReadCommands != nil {
			if _, err := t.ReadCommands(ctx, t.CommandDir, name, t.SkillMaxBytes); err != nil {
				return InstallResult{}, j.fail(fmt.Errorf("plugin %s: %w", name, err))
			}
		}
	}
	for _, file := range found.catalog {
		if err := t.writeCatalog(j, file.kind, file.id, file.data); err != nil {
			return InstallResult{}, j.fail(fmt.Errorf("plugin %s: %w", name, err))
		}
	}
	if err := t.removeEntries(j, obsolete); err != nil {
		return InstallResult{}, j.fail(fmt.Errorf("plugin %s: %w", name, err))
	}
	record := Record{
		SchemaVersion: recordVersion,
		Name:          name,
		Version:       found.manifest.Version,
		Description:   found.manifest.Description,
		Source:        found.source,
		InstalledAt:   t.now().UTC(),
		Entries:       planned,
	}
	if err := t.writeRecord(j, name, &record); err != nil {
		return InstallResult{}, j.fail(err)
	}
	j.finish()
	t.removeEmptyFolders(name)
	return InstallResult{Record: record, Upgraded: previous, Removed: obsolete}, nil
}

// removeEntries takes back catalog files, profiles first, then toolsets, MCP servers and
// models, so nothing the plugin still holds names what goes; then the skill, commands and
// files folders.
func (t Target) removeEntries(j *journal, entries []Entry) error {
	for index := len(catalogFolders) - 1; index >= 0; index-- {
		kind := catalogFolders[index].kind
		for _, entry := range entries {
			if entry.Kind != string(kind) {
				continue
			}
			if err := t.deleteCatalog(j, kind, entry.ID); err != nil {
				return err
			}
		}
	}
	for _, entry := range entries {
		if isTreeKind(entry.Kind) {
			if err := moveAside(j, entry.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// Remove takes back what the plugin installed and its record. A file changed since it was
// installed is refused unless force is set; a catalog file something outside the plugin
// still needs (a session's profile, another toolset's MCP server) is always refused, and
// nothing is removed then.
func (t Target) Remove(ctx context.Context, name string, force bool) (Record, error) {
	lock, err := t.lock(ctx, name)
	if err != nil {
		return Record{}, err
	}
	defer lock.Close()
	record, err := t.ReadRecord(name)
	if err != nil {
		return Record{}, err
	}
	if record == nil {
		return Record{}, fmt.Errorf("plugin %s is not installed", name)
	}
	changed, err := modifiedEntries(record.Entries)
	if err != nil {
		return Record{}, err
	}
	var edited []Entry
	for _, entry := range changed {
		if hash, _ := currentHash(entry); hash != "" {
			edited = append(edited, entry)
		}
	}
	if len(edited) > 0 && !force {
		return Record{}, &ChangedError{Plugin: name, Entries: edited}
	}
	j := &journal{}
	if err := t.removeEntries(j, record.Entries); err != nil {
		return Record{}, j.fail(fmt.Errorf("plugin %s: %w", name, err))
	}
	if err := t.writeRecord(j, name, nil); err != nil {
		return Record{}, j.fail(err)
	}
	j.finish()
	t.removeEmptyFolders(name)
	return *record, nil
}

// removeEmptyFolders drops the plugin's category folders and its folder under .plugins when
// nothing is left in them. Removing a folder only succeeds when it is empty.
func (t Target) removeEmptyFolders(name string) {
	for _, folder := range catalogFolders {
		_ = os.Remove(filepath.Join(t.catalogDir(folder.kind), name))
	}
	_ = os.Remove(filepath.Join(t.Layout.State, recordFolder, name))
}
