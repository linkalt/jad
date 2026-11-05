// Package plugins installs plugin bundles into the catalog layout: one folder that holds
// models, MCP servers, toolsets (with their hooks), profiles (agents), skills, custom
// slash commands and files a plugin's MCP servers or hooks run. Installing copies them into the layout, records what
// was copied and its hash, and checks every catalog file with the loader's rules; removing
// takes back exactly what was recorded. A file changed after it was installed is left
// alone unless the caller forces it.
package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jad/backend-go/internal/catalog"
	"jad/backend-go/internal/fserr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionlock"
)

const (
	// ManifestName is the file that names a plugin, at the top of its folder.
	ManifestName = "plugin.json"
	// FilesPlaceholder in a plugin's catalog file is replaced by the absolute path of the
	// installed files/ folder, so an MCP server or a hook can run a script the plugin ships.
	FilesPlaceholder = "${JAD_PLUGIN_FILES}"
	// recordFolder under the state root holds one record per installed plugin and the
	// plugins' files. The leading dot keeps it apart from session folders.
	recordFolder   = ".plugins"
	recordVersion  = 1
	maxRecordBytes = 4 << 20

	// Entry kinds besides the catalog kinds (model, mcp, toolset, profile).
	kindSkill = "skill"
	kindFiles = "files"
	// kindCommands is the plugin's commands/ folder, installed whole as
	// <command folder>/<plugin name>, so its commands are /<plugin>:<command>.
	kindCommands = "commands"
)

// catalogFolders are the plugin folders that hold catalog files, in the order they are
// written: a toolset names MCP servers, a profile names models and toolsets, so what is
// named is in place, and checked, before what names it. Removal goes the other way.
var catalogFolders = []struct {
	folder string
	kind   sessionfs.CatalogKind
}{
	{"models", sessionfs.CatalogModels},
	{"mcp", sessionfs.CatalogMCP},
	{"tools", sessionfs.CatalogToolsets},
	{"profiles", sessionfs.CatalogProfiles},
}

// Manifest is plugin.json.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Version       string `json:"version,omitempty"`
	Description   string `json:"description,omitempty"`
}

// Entry is one thing an installed plugin put in place: a catalog file, a skill folder or
// its files folder, with the hash it had when installed.
type Entry struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Record is <state>/.plugins/<name>.json.
type Record struct {
	SchemaVersion int       `json:"schema_version"`
	Name          string    `json:"name"`
	Version       string    `json:"version,omitempty"`
	Description   string    `json:"description,omitempty"`
	Source        string    `json:"source"`
	InstalledAt   time.Time `json:"installed_at"`
	Entries       []Entry   `json:"entries"`
}

// Target is the layout plugins are installed into.
type Target struct {
	Layout sessionfs.Layout
	Roots  []sessionfs.Root
	// SkillDir is the skill folder skills are copied into: the first of libraries.skill_dirs,
	// so an installed skill is found before one of the same name further down the list.
	SkillDir string
	// SkillMaxBytes is libraries.max_asset_bytes, the limit a SKILL.md and a command
	// file are read with.
	SkillMaxBytes int64
	// CommandDir is the command folder a plugin's commands/ folder is copied into, as
	// <CommandDir>/<plugin name>: the first of libraries.command_dirs.
	CommandDir string
	// ReadCommands checks the commands installed in root/folder the way the "/" menu
	// reads them, and returns how many there are.
	ReadCommands   func(ctx context.Context, root, folder string, maxBytes int64) (int, error)
	ValidateServer sessionfs.ServerFileValidator
	// ReadSkill checks an installed skill folder the way the skill tool reads it.
	ReadSkill func(ctx context.Context, directory, name string, maxBytes int64) error
	Now       func() time.Time
}

func (t Target) recordPath(name string) string {
	return filepath.Join(t.Layout.State, recordFolder, name+".json")
}

func (t Target) filesDir(name string) string {
	return filepath.Join(t.Layout.State, recordFolder, name, "files")
}

// treePath is where a plugin's folder of the kind is installed.
func (t Target) treePath(plugin string, item tree) string {
	switch item.kind {
	case kindSkill:
		return filepath.Join(t.SkillDir, item.id)
	case kindCommands:
		return filepath.Join(t.CommandDir, plugin)
	default:
		return t.filesDir(plugin)
	}
}

func isTreeKind(kind string) bool {
	return kind == kindSkill || kind == kindFiles || kind == kindCommands
}

func (t Target) catalogDir(kind sessionfs.CatalogKind) string {
	switch kind {
	case sessionfs.CatalogModels:
		return t.Layout.Models
	case sessionfs.CatalogMCP:
		return t.Layout.MCP
	case sessionfs.CatalogToolsets:
		return t.Layout.Tools
	default:
		return t.Layout.Profiles
	}
}

func (t Target) catalogPath(kind sessionfs.CatalogKind, id string) string {
	category, name, _ := strings.Cut(id, "/")
	return filepath.Join(t.catalogDir(kind), category, name+".json")
}

func (t Target) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// lock serializes installs and removals of one plugin, across processes.
func (t Target) lock(ctx context.Context, name string) (io.Closer, error) {
	path := t.recordPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), fserr.UnwrapPathError(err))
	}
	lockPath, err := sessionlock.PathFor(path)
	if err != nil {
		return nil, err
	}
	return sessionlock.Acquire(ctx, lockPath)
}

// catalogFile is one catalog file of a plugin, ready to write.
type catalogFile struct {
	kind sessionfs.CatalogKind
	id   string
	data []byte
}

// tree is a skill folder, the commands folder or the files folder of a plugin.
type tree struct {
	kind   string
	id     string
	source string
	hash   string
}

// bundle is a plugin folder as read.
type bundle struct {
	manifest Manifest
	source   string
	catalog  []catalogFile
	trees    []tree
}

// ReadManifest reads and checks plugin.json in folder.
func ReadManifest(folder string) (Manifest, error) {
	path := filepath.Join(folder, ManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", path, fserr.UnwrapPathError(err))
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("%s: %v", path, err)
	}
	if manifest.SchemaVersion != 1 {
		return Manifest{}, fmt.Errorf("%s: schema_version must be 1", path)
	}
	if err := catalog.ValidateIdentifier(manifest.Name); err != nil {
		return Manifest{}, fmt.Errorf("%s: name %v; it becomes the category of the plugin's catalog files", path, err)
	}
	return manifest, nil
}

// readBundle reads a plugin folder. Only the folders a plugin can hold are read; anything
// else at the top, except hidden entries and README, LICENSE and CHANGELOG files, is refused
// so a misplaced file is not silently left out.
func readBundle(source string) (bundle, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return bundle{}, err
	}
	manifest, err := ReadManifest(source)
	if err != nil {
		return bundle{}, err
	}
	found := bundle{manifest: manifest, source: source}
	entries, err := os.ReadDir(source)
	if err != nil {
		return bundle{}, fmt.Errorf("read %s: %w", source, fserr.UnwrapPathError(err))
	}
	known := map[string]bool{ManifestName: true, "skills": true, "files": true, "commands": true}
	for _, folder := range catalogFolders {
		known[folder.folder] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		upper := strings.ToUpper(name)
		if known[name] || strings.HasPrefix(name, ".") || strings.HasPrefix(upper, "README") || strings.HasPrefix(upper, "LICENSE") || strings.HasPrefix(upper, "CHANGELOG") {
			continue
		}
		return bundle{}, fmt.Errorf("plugin %s: %s is not a plugin part; a plugin holds plugin.json, models/, mcp/, tools/, profiles/, skills/, commands/ and files/", manifest.Name, filepath.Join(source, name))
	}
	hasFiles := false
	if info, err := os.Lstat(filepath.Join(source, "files")); err == nil {
		if !info.IsDir() {
			return bundle{}, fmt.Errorf("plugin %s: files must be a folder", manifest.Name)
		}
		hash, err := treeHash(filepath.Join(source, "files"))
		if err != nil {
			return bundle{}, err
		}
		found.trees = append(found.trees, tree{kind: kindFiles, id: kindFiles, source: filepath.Join(source, "files"), hash: hash})
		hasFiles = true
	}
	for _, folder := range catalogFolders {
		files, err := readCatalogFolder(manifest.Name, filepath.Join(source, folder.folder), folder.kind)
		if err != nil {
			return bundle{}, err
		}
		for index := range files {
			if strings.Contains(string(files[index].data), FilesPlaceholder) && !hasFiles {
				return bundle{}, fmt.Errorf("plugin %s: %s %s uses %s but the plugin has no files folder", manifest.Name, files[index].kind, files[index].id, FilesPlaceholder)
			}
		}
		found.catalog = append(found.catalog, files...)
	}
	skills, err := readSkillsFolder(manifest.Name, filepath.Join(source, "skills"))
	if err != nil {
		return bundle{}, err
	}
	found.trees = append(found.trees, skills...)
	commands, err := readCommandsFolder(manifest.Name, filepath.Join(source, "commands"))
	if err != nil {
		return bundle{}, err
	}
	found.trees = append(found.trees, commands...)
	if len(found.catalog) == 0 && len(skills) == 0 && len(commands) == 0 {
		return bundle{}, fmt.Errorf("plugin %s holds no catalog file, skill or command", manifest.Name)
	}
	return found, nil
}

func readCatalogFolder(plugin, folder string, kind sessionfs.CatalogKind) ([]catalogFile, error) {
	entries, err := os.ReadDir(folder)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", folder, fserr.UnwrapPathError(err))
	}
	var files []catalogFile
	for _, entry := range entries {
		path := filepath.Join(folder, entry.Name())
		name, isJSON := strings.CutSuffix(entry.Name(), ".json")
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !entry.Type().IsRegular() || !isJSON || catalog.ValidateIdentifier(name) != nil {
			return nil, fmt.Errorf("plugin %s: %s must be a file <name>.json; it is installed as %s/%s", plugin, path, plugin, "<name>")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, fserr.UnwrapPathError(err))
		}
		files = append(files, catalogFile{kind: kind, id: plugin + "/" + name, data: data})
	}
	return files, nil
}

func readSkillsFolder(plugin, folder string) ([]tree, error) {
	entries, err := os.ReadDir(folder)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", folder, fserr.UnwrapPathError(err))
	}
	var skills []tree
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(folder, entry.Name())
		if !entry.IsDir() {
			return nil, fmt.Errorf("plugin %s: %s must be a skill folder holding SKILL.md", plugin, path)
		}
		if info, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("plugin %s: skill folder %s has no SKILL.md", plugin, path)
		}
		hash, err := treeHash(path)
		if err != nil {
			return nil, err
		}
		skills = append(skills, tree{kind: kindSkill, id: entry.Name(), source: path, hash: hash})
	}
	return skills, nil
}

// readCommandsFolder reads the commands/ folder: Markdown files, in subfolders or not,
// which become /<plugin>:<path> commands. Another file is refused, so nothing the plugin
// ships is silently left out of the menu.
func readCommandsFolder(plugin, folder string) ([]tree, error) {
	info, err := os.Lstat(folder)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", folder, fserr.UnwrapPathError(err))
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plugin %s: commands must be a folder", plugin)
	}
	count := 0
	err = filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return fmt.Errorf("plugin %s: %s is not a command; the commands folder holds Markdown files (.md)", plugin, path)
		}
		count++
		return nil
	})
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, fmt.Errorf("plugin %s: the commands folder holds no .md file", plugin)
	}
	hash, err := treeHash(folder)
	if err != nil {
		return nil, err
	}
	return []tree{{kind: kindCommands, id: plugin, source: folder, hash: hash}}, nil
}

// treeHash is the hash of a folder's regular files, their relative paths and contents. A
// link or a special file is refused: copying it would put something other than the plugin's
// own bytes in place.
func treeHash(root string) (string, error) {
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file or folder", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		lines = append(lines, filepath.ToSlash(relative)+"\x00"+hashBytes(data))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("read %s: %w", root, fserr.UnwrapPathError(err))
	}
	sort.Strings(lines)
	return hashBytes([]byte(strings.Join(lines, "\n"))), nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// currentHash is the hash an entry's path has now, or "" when it is gone.
func currentHash(entry Entry) (string, error) {
	if isTreeKind(entry.Kind) {
		if _, err := os.Lstat(entry.Path); errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return treeHash(entry.Path)
	}
	data, err := os.ReadFile(entry.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", entry.Path, fserr.UnwrapPathError(err))
	}
	return hashBytes(data), nil
}

// ReadRecord returns the record of an installed plugin, or nil when it is not installed.
func (t Target) ReadRecord(name string) (*Record, error) {
	if catalog.ValidateIdentifier(name) != nil {
		return nil, fmt.Errorf("%q is not a plugin name", name)
	}
	path := t.recordPath(name)
	handle, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, fserr.UnwrapPathError(err))
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, maxRecordBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, fserr.UnwrapPathError(err))
	}
	var record Record
	if len(data) > maxRecordBytes || json.Unmarshal(data, &record) != nil || record.SchemaVersion != recordVersion || record.Name != name {
		return nil, fmt.Errorf("%s is not a valid plugin record", path)
	}
	return &record, nil
}

// Status is an installed plugin and the entries changed or removed since it was installed.
type Status struct {
	Record   Record
	Modified []Entry
}

// List returns the installed plugins by name.
func (t Target) List() ([]Status, error) {
	folder := filepath.Join(t.Layout.State, recordFolder)
	entries, err := os.ReadDir(folder)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", folder, fserr.UnwrapPathError(err))
	}
	var statuses []Status
	for _, entry := range entries {
		name, isRecord := strings.CutSuffix(entry.Name(), ".json")
		if !isRecord || !entry.Type().IsRegular() || strings.HasPrefix(name, ".") {
			continue
		}
		record, err := t.ReadRecord(name)
		if err != nil {
			return nil, err
		}
		if record == nil {
			continue
		}
		modified, err := modifiedEntries(record.Entries)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, Status{Record: *record, Modified: modified})
	}
	return statuses, nil
}

func modifiedEntries(entries []Entry) ([]Entry, error) {
	var modified []Entry
	for _, entry := range entries {
		hash, err := currentHash(entry)
		if err != nil {
			return nil, err
		}
		if hash != entry.SHA256 {
			modified = append(modified, entry)
		}
	}
	return modified, nil
}

// ChangedError is an install or removal refused because files the plugin installed were
// changed or removed by hand since; Force replaces or removes them anyway.
type ChangedError struct {
	Plugin  string
	Entries []Entry
}

func (e *ChangedError) Error() string {
	names := make([]string, 0, len(e.Entries))
	for _, entry := range e.Entries {
		names = append(names, entry.Kind+" "+entry.ID)
	}
	return fmt.Sprintf("plugin %s: changed since it was installed: %s; pass --force to overwrite or remove them", e.Plugin, strings.Join(names, ", "))
}
