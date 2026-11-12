package sessionfs

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"jad/backend-go/internal/catalog"
)

// A profile's own models (phase 3c.1, docs/design_workspace_layout.md section 17): files in
// W/.jad/profile-own/<category>/<name>/models/<model category>/<model name>.json. Such a file is
// not a whole model: it names a visible model with its scope (based_on) and the few tuning
// keys it changes (set). The endpoint, the credential and the billed model always come from
// the base, so they are never written in profile-own and follow every edit of the base.

const profileModelsFolder = "models"

// ModelBase is the model file one of a profile's own models is based on.
type ModelBase struct {
	// BasedOn is the reference the own file names, with its scope prefix ("global:openai/x").
	BasedOn string
	// Path is the base's model file.
	Path string
}

// ownModelKeys are the keys of an own model file.
var ownModelKeys = []string{"schema_version", "based_on", "set"}

// ownModelSetKeys are the model keys an own model may set (decider 3c C1): tuning that does
// not change where requests go, which key is sent or which model is billed. Each replaces the
// base's value as a whole. Every other key is refused, keys added to model files later too.
var ownModelSetKeys = []string{"name", "request", "efforts", "prompt_cache_key"}

// ownModelRefusedKeys are refused by name, also under profile_self_edit full: the endpoint,
// the credential, what decides them, and the adapters' blocks that are parsed with base_url.
var ownModelRefusedKeys = []string{"base_url", "api_key_env", "provider", "api", "model", "anthropic", "chat_completions", "auth_mode", "oauth_auth_file"}

// ownModelFile is an own model file as read.
type ownModelFile struct {
	BasedOn string
	Set     map[string]json.RawMessage
}

// parseOwnModel applies the own model file rules to contents.
func parseOwnModel(path string, contents []byte) (ownModelFile, error) {
	fields, err := parseSessionObject(path, contents)
	if err != nil {
		return ownModelFile{}, err
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(ownModelKeys, key) {
			return ownModelFile{}, configError("%s: %s is not a key of a profile's own model (keys: %s); every other value comes from the model it is based on", path, key, strings.Join(ownModelKeys, ", "))
		}
	}
	if err := validateSchemaVersion(fields, path); err != nil {
		return ownModelFile{}, err
	}
	var file ownModelFile
	if err := json.Unmarshal(fields["based_on"], &file.BasedOn); err != nil || file.BasedOn == "" {
		return ownModelFile{}, configError("%s: based_on must name the model it is based on, with its scope, for example global:openai/default", path)
	}
	scope, _, _ := strings.Cut(file.BasedOn, ":")
	if scope != catalog.ScopeGlobal && scope != catalog.ScopeWorkspace {
		return ownModelFile{}, configError("%s: based_on %q must start with global: or workspace:; a profile's own model is based on a shared one, never on another own model", path, file.BasedOn)
	}
	if raw, ok := fields["set"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &file.Set); err != nil || file.Set == nil {
			return ownModelFile{}, configError("%s: set must be a JSON object", path)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(file.Set)) {
		if slices.Contains(ownModelRefusedKeys, key) {
			return ownModelFile{}, configError("%s: set.%s cannot be changed by a profile's own model: the endpoint, the credential and the billed model come from the base; put a model with its own endpoint in the workspace's models", path, key)
		}
		if !slices.Contains(ownModelSetKeys, key) {
			return ownModelFile{}, configError("%s: set.%s is not one a profile's own model may change (allowed: %s)", path, key, strings.Join(ownModelSetKeys, ", "))
		}
	}
	return file, nil
}

// readOwnModelFile reads an own model file, which must be a regular file and not a link.
func readOwnModelFile(path string) (ownModelFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ownModelFile{}, configError("%s: could not be read: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return ownModelFile{}, configError("%s: a profile's own model must be a regular file, not a link", path)
	}
	contents, err := readRegularFile(path, maxInternalJSONFileBytes)
	if err != nil {
		return ownModelFile{}, configError("%s: could not be read: %v", path, err)
	}
	return parseOwnModel(path, contents)
}

// ownModelFields are the model fields of the own model at path: the base's file with the
// own set applied. The own file must still name the base it named when the session loaded.
func ownModelFields(path string, base ModelBase) (map[string]json.RawMessage, error) {
	file, err := readOwnModelFile(path)
	if err != nil {
		return nil, err
	}
	if file.BasedOn != base.BasedOn {
		return nil, configError("%s: based_on is now %q, not %q as when the session was loaded; load the session again", path, file.BasedOn, base.BasedOn)
	}
	fields, err := readSessionObject(base.Path)
	if err != nil {
		return nil, configError("%s: its base %s: %v", path, base.BasedOn, err)
	}
	if _, own := fields["based_on"]; own {
		return nil, configError("%s: its base %s (%s) sets based_on, so it is not a whole model file", path, base.BasedOn, base.Path)
	}
	composed := maps.Clone(fields)
	maps.Copy(composed, file.Set)
	return composed, nil
}

// loadModelOf is loadModel for a model that may be one of a profile's own: a file in bases is
// composed from its base. A file outside bases is read as a whole model, so an own model file
// reached by any other way fails (modelFromFields refuses based_on).
func loadModelOf(path string, bases map[string]ModelBase, promptRole, promptText string) (Model, error) {
	base, own := bases[path]
	if !own {
		return loadModel(path, promptRole, promptText)
	}
	fields, err := ownModelFields(path, base)
	if err != nil {
		return Model{}, err
	}
	return modelFromFields(fields, path, promptRole, promptText)
}

// ownModels is the model catalog of one profile's load: the shared catalog with the
// profile's own models as the "profile" scope.
type ownModels struct {
	catalog *catalog.Catalog
	// bases are the own model files whose base resolves, by file.
	bases map[string]ModelBase
	// faults are the own model files that cannot be used, by file, and why.
	faults map[string]error
	// ids are the IDs of the own model files, by file.
	ids map[string]string
	// notes are warnings about own model files left out for their names or a link.
	notes []string
}

// profileModels is the model catalog the profile id loads with: in the workspace layout the
// shared models and its own (decider 3c C2); elsewhere, and without own models, the shared
// catalog. An own file that fails stays in the catalog and fails like a model file that
// fails the model rules: it is left out of what the profile offers, with a warning.
func (l Layout) profileModels(scans *CatalogScans, id string) (ownModels, error) {
	scopes, err := scans.scanScopeList("model", l.Models, l.WorkspaceModels)
	if err != nil {
		return ownModels{}, err
	}
	shared := catalog.MergeScopes("model", scopes...)
	own := ownModels{catalog: shared}
	if l.Workspace == "" || l.ProfileOwnDir(id) == "" {
		return own, nil
	}
	dir, err := ownFolder(l.Workspace, id, []string{profileModelsFolder}, false)
	if err != nil {
		own.notes = append(own.notes, fmt.Sprintf("profile %s: its own models are left out: %v", id, err))
		return own, nil
	}
	if dir == "" {
		return own, nil
	}
	scanned, err := scans.Scan("model", dir)
	if err != nil {
		own.notes = append(own.notes, fmt.Sprintf("profile %s: its own models are left out: %v", id, err))
		return own, nil
	}
	for _, skipped := range scanned.Skipped() {
		own.notes = append(own.notes, fmt.Sprintf("profile %s: own model left out: %s", id, skipped))
	}
	if scanned.Len() == 0 {
		return own, nil
	}
	own.bases, own.faults, own.ids = make(map[string]ModelBase, scanned.Len()), map[string]error{}, make(map[string]string, scanned.Len())
	for _, entry := range scanned.Entries() {
		own.ids[entry.Path] = entry.ID()
		scans.noteFile(entry.Path)
		base, err := resolveOwnModel(entry.Path, shared)
		if err != nil {
			own.faults[entry.Path] = err
			continue
		}
		scans.noteFile(base.Path)
		own.bases[entry.Path] = base
	}
	own.catalog = catalog.MergeScopes("model", append(scopes, catalog.Scope{Name: catalog.ScopeProfile, Catalog: scanned})...)
	for _, skipped := range own.catalog.Skipped() {
		if PathWithin(dir, skipped.Path) {
			own.notes = append(own.notes, fmt.Sprintf("profile %s: own model left out: %s", id, skipped))
		}
	}
	return own, nil
}

// resolveOwnModel reads the own model file at path and finds its base among the shared models.
func resolveOwnModel(path string, shared *catalog.Catalog) (ModelBase, error) {
	if info, err := os.Lstat(filepath.Dir(path)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ModelBase{}, configError("%s: its category folder is a link or not a folder", path)
	}
	file, err := readOwnModelFile(path)
	if err != nil {
		return ModelBase{}, err
	}
	base, err := shared.Lookup(file.BasedOn)
	if err != nil {
		return ModelBase{}, configError("%s: based_on: %v", path, err)
	}
	return ModelBase{BasedOn: file.BasedOn, Path: base.Path}, nil
}

// own reports whether path is one of the profile's own model files.
func (o ownModels) own(path string) bool {
	if _, found := o.bases[path]; found {
		return true
	}
	_, found := o.faults[path]
	return found
}

// problem is nil when the model file at path, own or shared, passes the model rules.
func (o ownModels) problem(path string) error {
	if err, failed := o.faults[path]; failed {
		return err
	}
	_, err := loadModelOf(path, o.bases, "", "")
	return err
}

// check is badModelsOf's check of one listed model: own files are judged composed with
// their base, once per memo like shared ones.
func (o ownModels) check(scans *CatalogScans, path string) error {
	if o.own(path) {
		return scans.check("own model", path, o.problem)
	}
	return scans.check("model", path, modelFileProblem)
}

// effort refuses an effort the model at path does not offer; an empty effort is not checked.
func (o ownModels) effort(path, effort string) error {
	return checkModelEffortOf(path, o.bases, effort)
}

// effortOnly is effort for a model that may fail the model rules (checkModelEffortOnly).
func (o ownModels) effortOnly(path, effort string) error {
	return checkModelEffortOnlyOf(path, o.bases, effort)
}

// ownReference reports whether a model or toolset reference names a profile's own entry.
func ownReference(reference string) bool {
	return strings.HasPrefix(reference, catalog.ScopeProfile+":")
}

// OwnModel reports whether path is one of the selected profile's own model files.
func (c SessionConfig) OwnModel(path string) bool {
	_, own := c.ModelBases[path]
	return own
}

// ModelFields are the fields of the model file at path as the session's runs read them: an
// own model composed with its base, any other file as it is. They are not checked.
func (c SessionConfig) ModelFields(path string) (map[string]json.RawMessage, error) {
	if base, own := c.ModelBases[path]; own {
		return ownModelFields(path, base)
	}
	return readSessionObject(path)
}

// ModelSourceFiles are the files the model at path is read from: an own model's file and its
// base, any other the file itself.
func (c SessionConfig) ModelSourceFiles(path string) []string {
	if base, own := c.ModelBases[path]; own {
		return []string{path, base.Path}
	}
	return []string{path}
}

// basedOn is the reference of the first own model (by file) based on the model file at path,
// an own model whose ID is also a shared one included; "" when none is.
func (o ownModels) basedOn(path string) string {
	path = filepath.Clean(path)
	for _, file := range slices.Sorted(maps.Keys(o.bases)) {
		if filepath.Clean(o.bases[file].Path) == path {
			return catalog.ScopeProfile + ":" + o.ids[file]
		}
	}
	return ""
}

// profileIDOf is the ID of the profile file at path in the layout's profiles folder; "" when
// it is not one.
func (l Layout) profileIDOf(path string) string {
	if l.Profiles == "" {
		return ""
	}
	relative, err := filepath.Rel(l.Profiles, path)
	if err != nil || !filepath.IsLocal(relative) {
		return ""
	}
	id, found := strings.CutSuffix(filepath.ToSlash(relative), ".json")
	if !found || strings.Count(id, "/") != 1 {
		return ""
	}
	return id
}

// OwnModelScope is the scope name of a profile's own models, as a listing marks them.
const OwnModelScope = catalog.ScopeProfile
