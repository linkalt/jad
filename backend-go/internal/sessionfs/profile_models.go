package sessionfs

import (
	"bytes"
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
	// trial is the own file's contents a trial load of an edit reads in place of the file
	// (nil: the file is read), so a candidate own model is judged before it is written; an
	// empty candidate is judged as empty, never as the file on disk.
	trial *[]byte
	// pin is the own model as the run read it when it started (PinOwnModels); nil before.
	pin *ownModelPin
}

// ownModelKeys are the keys of an own model file.
var ownModelKeys = []string{"schema_version", "based_on", "set"}

// The reasons a key of an own model's set is refused by name (decider 3c.1b).
const (
	refusedEndpoint    = "the endpoint, the credential and the billed model come from the base; put a model with its own endpoint in the workspace's models"
	refusedRunner      = "the session runner sets it"
	refusedPrice       = "it changes the price, which the budget caps do not count"
	refusedStorage     = "it keeps data at the provider"
	refusedAccount     = "it pulls other content of the provider account into the request"
	refusedAttribution = "it changes whom the provider attributes the requests to"
	refusedCacheKey    = "the top-level prompt_cache_key sets the key"
	refusedHeaders     = "fixed header values could replace the credential or the account"
	refusedBody        = "any field passes through to the provider, a routing or fallback model list too"
	refusedUpstream    = "it holds the endpoint, the auth header, the beta flags and extra headers"
	refusedLog         = "its headers and body bytes could write the key into a log file"
	refusedModels      = "per-model profiles carry extra_body and match other models"
)

// ownKeyRule is what one value of an own model's set may hold (decider 3c.1b): a JSON kind
// and, for an object, the keys it allows (keys) or the rule of every value of a map (values).
// refused names keys refused with their reason, before the allow-list. Every other key is
// refused, keys added to model files later too. The set is merged into the base key by key,
// objects recursively (mergeSettings), so a key the own model does not name keeps the base's
// value and an own model cannot remove one.
type ownKeyRule struct {
	kind    string
	keys    map[string]ownKeyRule
	values  *ownKeyRule
	refused map[string]string
}

var (
	ownString  = ownKeyRule{kind: "string"}
	ownNumber  = ownKeyRule{kind: "number"}
	ownInteger = ownKeyRule{kind: "integer"}
	ownBoolean = ownKeyRule{kind: "boolean"}
)

// ownModelSet is the rule of an own model's set: tuning that does not change where requests
// go, which key is sent, which model is billed, what the provider keeps or whom it bills.
// It is the one table 3c.2's self-edit checks too.
var ownModelSet = ownKeyRule{kind: "object", keys: map[string]ownKeyRule{
	"name":             ownString,
	"efforts":          {kind: "array"},
	"prompt_cache_key": ownString,
	"request": {kind: "object", keys: map[string]ownKeyRule{
		"reasoning":           {kind: "object", keys: map[string]ownKeyRule{"effort": ownString, "summary": ownString}},
		"max_output_tokens":   ownInteger,
		"temperature":         ownNumber,
		"top_p":               ownNumber,
		"parallel_tool_calls": ownBoolean,
		"truncation":          ownString,
		"text":                {kind: "object", keys: map[string]ownKeyRule{"verbosity": ownString}},
	}, refused: map[string]string{
		"model": refusedRunner, "input": refusedRunner, "instructions": refusedRunner, "stream": refusedRunner, "tools": refusedRunner,
		"service_tier": refusedPrice,
		"store":        refusedStorage, "background": refusedStorage,
		"previous_response_id": refusedAccount, "conversation": refusedAccount, "prompt": refusedAccount,
		"metadata": refusedAttribution, "user": refusedAttribution, "safety_identifier": refusedAttribution,
		"prompt_cache_key": refusedCacheKey,
	}},
	"anthropic": {kind: "object", keys: map[string]ownKeyRule{
		"default_max_tokens": ownInteger,
		"cache": {kind: "object", keys: map[string]ownKeyRule{
			"automatic": ownBoolean, "tools": ownBoolean, "system": ownBoolean, "history": ownBoolean, "ttl": ownString,
		}},
		"default_profile": {kind: "object", keys: map[string]ownKeyRule{
			"effort_budget": {kind: "object", values: &ownInteger},
		}, refused: map[string]string{"extra_body": refusedBody}},
	}, refused: map[string]string{"upstream": refusedUpstream, "log": refusedLog, "models": refusedModels, "extra_headers": refusedHeaders, "extra_body": refusedBody}},
	"chat_completions": {kind: "object", keys: map[string]ownKeyRule{
		"request_timeout_ms": ownInteger,
		"system_role":        ownString,
	}, refused: map[string]string{"extra_headers": refusedHeaders, "extra_body": refusedBody, "log": refusedLog}},
}, refused: map[string]string{
	"base_url": refusedEndpoint, "api_key_env": refusedEndpoint, "provider": refusedEndpoint, "api": refusedEndpoint,
	"model": refusedEndpoint, "auth_mode": refusedEndpoint, "oauth_auth_file": refusedEndpoint,
}}

// check applies the rule to the value at (a dotted name such as set.request.store).
func (r ownKeyRule) check(path, at string, raw json.RawMessage) error {
	value := bytes.TrimSpace(raw)
	if !jsonKindIs(value, r.kind) {
		return configError("%s: %s must be a JSON %s", path, at, r.kind)
	}
	if r.kind != "object" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil {
		return configError("%s: %s must be a JSON object", path, at)
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		name := at + "." + key
		if reason, refused := r.refused[key]; refused {
			return configError("%s: %s cannot be changed by a profile's own model: %s", path, name, reason)
		}
		inner, allowed := r.keys[key]
		if !allowed && r.values != nil {
			inner, allowed = *r.values, true
		}
		if !allowed {
			return configError("%s: %s is not one a profile's own model may change (allowed: %s)", path, name, strings.Join(slices.Sorted(maps.Keys(r.keys)), ", "))
		}
		if err := inner.check(path, name, fields[key]); err != nil {
			return err
		}
	}
	return nil
}

// jsonKindIs reports whether the trimmed JSON value is of kind; null is no kind.
func jsonKindIs(value []byte, kind string) bool {
	if len(value) == 0 {
		return false
	}
	switch kind {
	case "string":
		return value[0] == '"'
	case "object":
		return value[0] == '{'
	case "array":
		return value[0] == '['
	case "boolean":
		return string(value) == "true" || string(value) == "false"
	case "number", "integer":
		// json.Number also takes a quoted number; a number is never a string here.
		var number json.Number
		if value[0] == '"' || json.Unmarshal(value, &number) != nil {
			return false
		}
		if kind == "integer" {
			_, err := number.Int64()
			return err == nil
		}
		_, err := number.Float64()
		return err == nil
	}
	return false
}

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
		if err := ownModelSet.check(path, "set", raw); err != nil {
			return ownModelFile{}, err
		}
		if err := json.Unmarshal(raw, &file.Set); err != nil || file.Set == nil {
			return ownModelFile{}, configError("%s: set must be a JSON object", path)
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
	if base.pin != nil {
		// A started run never reads the own file or its base again (decider 3c.2b).
		if base.pin.err != nil {
			return nil, base.pin.err
		}
		return maps.Clone(base.pin.fields), nil
	}
	var file ownModelFile
	var err error
	if base.trial != nil {
		file, err = parseOwnModel(path, *base.trial)
	} else {
		file, err = readOwnModelFile(path)
	}
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
	if _, err := modelFromFields(fields, base.Path, "", ""); err != nil {
		// A base that fails the model rules is not repaired by an own model's set.
		return nil, configError("%s: its base %s: %v", path, base.BasedOn, err)
	}
	// The set is merged key by key, objects recursively: a request, anthropic or
	// chat_completions key the own model does not name keeps the base's value (decider 3c.1b),
	// so a base's store: false stays. The composed fields pass the whole model rules after.
	return mergeSettings(fields, file.Set), nil
}

// loadModelOf is loadModel for a model that may be one of a profile's own: a file in bases is
// composed from its base. A file outside bases is read as a whole model, so an own model file
// reached by any other way fails in the workspace layout (modelFromFields refuses based_on).
func loadModelOf(path string, bases map[string]ModelBase, workspace bool, promptRole, promptText string) (Model, error) {
	base, own := bases[path]
	if !own {
		return loadWholeModel(path, workspace, promptRole, promptText)
	}
	fields, err := ownModelFields(path, base)
	if err != nil {
		return Model{}, ownModelError(err)
	}
	model, err := modelFromFields(fields, path, promptRole, promptText)
	return model, ownModelError(err)
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
	// workspace is set in the workspace layout, where a model file read as a whole model may
	// not set based_on (loadWholeModel).
	workspace bool
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
	own := ownModels{catalog: shared, workspace: l.Workspace != ""}
	if l.Workspace == "" || l.ProfileOwnDir(id) == "" {
		return own, nil
	}
	overlay := scans.ownModelOverlay(id)
	dir, err := ownFolder(l.Workspace, id, []string{profileModelsFolder}, false)
	if err == nil && dir == "" && len(overlay) > 0 {
		// A trial of an edit that makes the first own model: the folder is made when the edit
		// is published, where the walk from the real W/.jad would make it.
		dir, err = ownModelsFolder(l.Workspace, id)
	}
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
	for _, modelID := range slices.Sorted(maps.Keys(overlay)) {
		if overlay[modelID] == nil {
			scanned = scanned.Without(modelID)
			continue
		}
		category, name, _ := strings.Cut(modelID, "/")
		scanned = scanned.With(catalog.Entry{Category: category, Name: name, Path: filepath.Join(dir, category, name+".json")})
	}
	if scanned.Len() == 0 {
		return own, nil
	}
	own.bases, own.faults, own.ids = make(map[string]ModelBase, scanned.Len()), map[string]error{}, make(map[string]string, scanned.Len())
	for _, entry := range scanned.Entries() {
		own.ids[entry.Path] = entry.ID()
		scans.noteFile(entry.Path)
		base, err := resolveOwnModel(entry.Path, shared, overlay[entry.ID()])
		if err != nil {
			own.faults[entry.Path] = ownModelError(err)
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
// A trial passes the candidate contents (trial, nil otherwise): they are judged in place of the
// file, whose category folder an edit checks (and makes) when it publishes.
func resolveOwnModel(path string, shared *catalog.Catalog, trial *[]byte) (ModelBase, error) {
	var file ownModelFile
	var err error
	if trial != nil {
		file, err = parseOwnModel(path, *trial)
	} else {
		if info, err := os.Lstat(filepath.Dir(path)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ModelBase{}, configError("%s: its category folder is a link or not a folder", path)
		}
		file, err = readOwnModelFile(path)
	}
	if err != nil {
		return ModelBase{}, err
	}
	base, err := shared.Lookup(file.BasedOn)
	if err != nil {
		return ModelBase{}, configError("%s: based_on: %v", path, err)
	}
	return ModelBase{BasedOn: file.BasedOn, Path: base.Path, trial: trial}, nil
}

// ownModelsFolder is where the profile's own models folder is, below the real W/.jad, whether
// it exists or not (ownFolder walks it).
func ownModelsFolder(workspace, profile string) (string, error) {
	jad, err := filepath.EvalSymlinks(filepath.Join(workspace, ".jad"))
	if err != nil {
		return "", err
	}
	return filepath.Join(jad, profileOwnFolder, filepath.FromSlash(profile), profileModelsFolder), nil
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
	_, err := loadModelOf(path, o.bases, o.workspace, "", "")
	return err
}

// check is badModelsOf's check of one listed model: own files are judged composed with
// their base, once per memo like shared ones.
func (o ownModels) check(scans *CatalogScans, path string) error {
	if o.own(path) {
		return scans.check("own model", path, o.problem)
	}
	return scans.check("model", path, func(path string) error { return modelFileProblem(path, o.workspace) })
}

// effort refuses an effort the model at path does not offer; an empty effort is not checked.
func (o ownModels) effort(path, effort string) error {
	return checkModelEffortOf(path, o.bases, o.workspace, effort)
}

// effortOnly is effort for a model that may fail the model rules (checkModelEffortOnly).
func (o ownModels) effortOnly(path, effort string) error {
	return checkModelEffortOnlyOf(path, o.bases, o.workspace, effort)
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

// WithAgentModel is the configuration with the model file a sub-agent names (its ID resolved
// from the shared catalog, or a path) and its effort. That file is read as a whole model, never
// composed as one of the profile's own (decider 3c C2): an own model file named by its path
// fails like everywhere else outside the profile's own selection.
func (c SessionConfig) WithAgentModel(path, effort string) SessionConfig {
	if _, own := c.ModelBases[path]; own {
		c.ModelBases = maps.Clone(c.ModelBases)
		delete(c.ModelBases, path)
	}
	c.ModelPath, c.ReasoningEffort = path, effort
	return c
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

// loadWholeModel is loadModel under the layout's based_on rule (decider 3c.1b): the workspace
// layout refuses based_on in a file read as a whole model, so an own model file reached another
// way never runs without its base; v1, which has no own models, ignores the key as it ignored
// it before phase 3c.
func loadWholeModel(path string, workspace bool, promptRole, promptText string) (Model, error) {
	if workspace {
		return loadModel(path, promptRole, promptText)
	}
	fields, err := readSessionObject(path)
	if err != nil {
		return Model{}, err
	}
	delete(fields, "based_on")
	return modelFromFields(fields, path, promptRole, promptText)
}
