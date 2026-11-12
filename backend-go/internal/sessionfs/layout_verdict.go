package sessionfs

import (
	"fmt"
	"strings"

	"jad/backend-go/internal/catalog"
)

// A profile file that fails its rules is left out of the session that lists it, and a model
// file that fails the model rules is left out of the models a profile offers, each with a
// warning, and the session loads without it. This file decides which ones; layoutCompose
// applies the verdicts. Only what the file itself says is judged (it parses, its keys and
// references hold, its toolset files parse, a model file passes the model rules). What
// composing the selected profile finds (sandbox, tool and hook settings) still fails the
// load: a profile left out is never run, so leaving it out cannot loosen anything, but
// skipping to another profile on a failed security setting would.
//
// Nothing is run in place of what was chosen. A profile the caller names must pass itself.
// A model that fails the model rules is never offered, whatever chose it: it is not in
// Models, ModelPaths or the picker, the profile's explicit model_default and the model a
// caller requests included. The session still loads (it stays listed and readable), with
// SessionConfig.ModelFault set when the model it would run is one of those, and a run stops
// with that file's error after its input is stored, instead of using another model.

// badModel is a model file a profile lists that fails the model rules.
type badModel struct {
	id  string
	err error
}

// profileVerdict is what reading one profile of the scope found.
type profileVerdict struct {
	entry catalog.Entry
	file  layoutProfileFile
	// listed are the models the profile lists, narrowed from the catalog; badModels are those
	// among them whose files fail the model rules, in catalog order.
	listed    *catalog.Catalog
	badModels []badModel
	// err is why the profile is left out; nil when it is usable.
	err error
	// composition is set when err is what applying its toolsets to this session's settings
	// found. That depends on the session, so jad check reports it with the session; the
	// other reasons are about the profile's own files, which check reports on their own.
	composition bool
	// toolsets is set when the profile is left out because the settings its toolsets add fail
	// their rules (judgeDefaultToolsetSettings).
	toolsets bool
}

// scopeJudgement is the verdict on every profile of a session's scope.
type scopeJudgement struct {
	// all is the scope as the session names it; usable leaves out the profiles that fail.
	all, usable *catalog.Catalog
	verdicts    map[string]*profileVerdict
	// models is every model in the catalog.
	models *catalog.Catalog
}

// leftOut returns the IDs of the profiles that fail, in scope order.
func (j scopeJudgement) leftOut() []string {
	var ids []string
	for _, id := range j.all.IDs() {
		if j.verdicts[id].err != nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// judgeScope reads the profile files of the session's scope and judges each. A nil scans
// shares its reads of model and toolset files within this call only.
func judgeScope(layout Layout, location Location, session layoutSessionFile, sessionPath string, scans *CatalogScans) (scopeJudgement, error) {
	if scans == nil {
		scans = NewCatalogScans()
	}
	all, err := scans.Scan("profile", layout.Profiles)
	if err != nil {
		return scopeJudgement{}, configError("%s: %v", sessionPath, err)
	}
	if all, err = all.Narrow(session.Scope.Profiles); err != nil {
		return scopeJudgement{}, configError("%s: scope.profiles: %v", sessionPath, err)
	}
	models, err := scans.Scan("model", layout.Models)
	if err != nil {
		return scopeJudgement{}, configError("%s: %v", sessionPath, err)
	}
	judgement := scopeJudgement{all: all, usable: all, verdicts: make(map[string]*profileVerdict, all.Len()), models: models}
	var toolsets *catalog.Catalog
	var toolsetsErr error
	for _, entry := range all.Entries() {
		scans.noteFile(entry.Path)
		verdict := &profileVerdict{entry: entry}
		judgement.verdicts[entry.ID()] = verdict
		file, narrowed, err := readProfileOf(entry.Path, models)
		if err == nil {
			err = checkUnselectedProfile(entry.Path, file)
		}
		if err != nil {
			verdict.err = err
			judgement.usable = judgement.usable.Without(entry.ID())
			continue
		}
		verdict.file, verdict.listed = file, narrowed
		if len(file.Toolset) > 0 && toolsets == nil && toolsetsErr == nil {
			toolsets, toolsetsErr = scans.Scan("toolset", layout.Tools)
		}
		verdict.err = checkProfileToolsets(scans, entry.Path, file, toolsets, toolsetsErr)
		if verdict.err == nil {
			verdict.err = checkToolsetComposition(layout, session, sessionPath, entry.Path, file, scans)
			verdict.composition = verdict.err != nil
		}
		if verdict.err == nil {
			verdict.badModels = badModelsOf(scans, narrowed)
		}
		if verdict.err != nil {
			judgement.usable = judgement.usable.Without(entry.ID())
		}
	}
	judgement.judgeDefaultToolsetSettings(layout, location, session, sessionPath, scans)
	return judgement, nil
}

// judgeDefaultToolsetSettings leaves out the profile that would be the default when the native
// tool settings its toolsets add (a misspelt key, a value out of range) make the settings it
// would run with fail their rules, so that one toolset file does not fail the session while
// another profile can run. It repeats for the profile that becomes the default, and stops at
// the first that is fine. Knowing means composing and normalising the settings as a load does,
// so this is done for the default only, not for every profile: that was measured at about four
// times the cost of the judgement of five profiles, on every list and profile lookup. Every
// caller reads the default from this judgement, so the load, the profile list and the model and
// profile-message routes agree on it. A profile that is not the default stays offered, and
// selecting it fails the load of that profile with the setting's error; nothing takes its place.
//
// The profile is left out only when its settings fail with its toolsets and load without them.
// A failure that stays without them belongs to the shared settings or to the profile's own
// (sandbox, hooks, tool settings, storage), and the profile is kept so that the load that runs
// it fails with that error, as it always did: leaving it out would let another profile run in
// its place. A left-out profile is never run, so nothing is loosened. The toolsets an agent
// names are not judged: a failure of those still fails the load that reads them.
func (j *scopeJudgement) judgeDefaultToolsetSettings(layout Layout, location Location, session layoutSessionFile, sessionPath string, scans *CatalogScans) {
	keys, err := profileKeys(j.all, sessionPath)
	if err != nil {
		return
	}
	for {
		id := session.defaultProfile(j.usable)
		if id == "" {
			return
		}
		verdict := j.verdicts[id]
		if !toolsetsAddNativeSettings(j.toolsetPaths(verdict.file, scans, layout)) {
			return
		}
		attempt := func(withoutToolsets bool) error {
			c := &layoutCompose{layout: layout, location: location, sessionPath: sessionPath, session: session, profiles: j.usable, judged: *j, keys: keys, scans: scans, withoutToolsets: withoutToolsets}
			setting, _, err := c.compose(verdict.entry, nil)
			if err != nil {
				return err
			}
			_, err = loadSessionSettingsFrom(location, setting, sessionPath, location.Root.GlobalJadRoot, keys[id], "")
			return err
		}
		err := attempt(false)
		if err == nil || attempt(true) != nil {
			return
		}
		verdict.err = err
		verdict.composition = true
		verdict.toolsets = true
		j.usable = j.usable.Without(id)
	}
}

// toolsetPaths returns the files of the toolsets the profile names, empty when they cannot be
// found (a profile whose toolset is missing was already left out).
func (j *scopeJudgement) toolsetPaths(file layoutProfileFile, scans *CatalogScans, layout Layout) []string {
	if len(file.Toolset) == 0 {
		return nil
	}
	toolsets, err := scans.Scan("toolset", layout.Tools)
	if err != nil {
		return nil
	}
	var paths []string
	for _, id := range file.Toolset {
		if found, err := toolsets.Lookup(id); err == nil {
			paths = append(paths, found.Path)
		}
	}
	return paths
}

// toolsetsAddNativeSettings reports whether any of the toolset files gives native or extension
// tool settings, the only ones whose rules a toolset can fail beyond what composing found.
func toolsetsAddNativeSettings(paths []string) bool {
	for _, path := range paths {
		var toolset layoutToolsetFile
		if readLayoutFile(path, &toolset) == nil && (len(toolset.Native) > 0 || len(toolset.Extension) > 0) {
			return true
		}
	}
	return false
}

// checkProfileToolsets requires each toolset the profile names to exist and to parse. What the
// toolset asks for (its tools, hooks and servers) is checked when the profile is composed.
func checkProfileToolsets(scans *CatalogScans, profilePath string, file layoutProfileFile, toolsets *catalog.Catalog, scanErr error) error {
	if len(file.Toolset) == 0 {
		return nil
	}
	if scanErr != nil {
		return configError("%s: %v", profilePath, scanErr)
	}
	for _, id := range file.Toolset {
		found, err := toolsets.Lookup(id)
		if err != nil {
			return configError("%s: toolset: %v", profilePath, err)
		}
		if err := scans.check("toolset", found.Path, parseToolsetFile); err != nil {
			return err
		}
	}
	return nil
}

func parseToolsetFile(path string) error {
	var toolset layoutToolsetFile
	return readLayoutFile(path, &toolset)
}

// badModelsOf returns the models among listed whose files fail the model rules.
func badModelsOf(scans *CatalogScans, listed *catalog.Catalog) []badModel {
	var bad []badModel
	for _, item := range listed.Entries() {
		if err := scans.check("model", item.Path, modelFileProblem); err != nil {
			bad = append(bad, badModel{id: item.ID(), err: err})
		}
	}
	return bad
}

// checkToolsetComposition applies the profile's toolsets to the settings the profile would run
// with, as compose does, and returns what that finds: hooks given both in the settings and a
// toolset, an MCP server that does not exist or is given twice, a malformed mcp object, a
// removed key. Only what the toolsets add is judged here, so a profile left out for it is one
// whose toolset files are what is wrong. A profile's own settings (sandbox, hooks, tool
// settings) are not. A native tool setting of a toolset is judged only for the profile that
// would be the default (judgeDefaultToolsetSettings); for another profile it fails the load
// of that profile when it is selected.
func checkToolsetComposition(layout Layout, session layoutSessionFile, sessionPath, profilePath string, file layoutProfileFile, scans *CatalogScans) error {
	if len(file.Toolset) == 0 {
		return nil
	}
	setting := mergeSettings(mergeSettings(layout.Defaults, mergeSettings(nil, session.Settings)), file.Settings)
	scratch := &layoutCompose{layout: layout, sessionPath: sessionPath, scans: scans}
	return scratch.addToolsets(setting, file.Toolset, profilePath, setting["env_file"])
}

// bad returns the error of a listed model whose file fails the model rules.
func (v *profileVerdict) bad(id string) error {
	for _, item := range v.badModels {
		if item.id == id {
			return item.err
		}
	}
	return nil
}

// offer returns the models the profile offers, which are those of its list whose files pass,
// and a warning for each that does not and for what that means for the model a run uses.
func (v *profileVerdict) offer(sessionPath, profileID string) (*catalog.Catalog, []string) {
	if len(v.badModels) == 0 {
		return v.listed, nil
	}
	defaultID := ""
	if v.file.ModelDefault != "" {
		defaultID, _, _ = SplitModelEffort(v.file.ModelDefault)
	}
	offered := v.listed
	var notes []string
	for _, item := range v.badModels {
		offered = offered.Without(item.id)
		note := fmt.Sprintf("%s: model %s left out of profile %s: %v", sessionPath, item.id, profileID, item.err)
		if item.id == defaultID {
			note += "; it is the profile's model_default, so a run that names no model stops with this error and no other model is used"
		}
		notes = append(notes, note)
	}
	if offered.Len() == 0 {
		notes = append(notes, fmt.Sprintf("%s: no model of profile %s can be used, so a run with it stops with the errors above", sessionPath, profileID))
	}
	return offered, notes
}

// selectionFault is the error a run gets instead of a model when the model it would use is
// one that is left out: the model the caller requests, else the profile's explicit
// model_default, else (no default named) the first of the list when none of the list passes.
// It is nil when that model is usable, so a model that is left out never changes which one
// is used.
func (v *profileVerdict) selectionFault(requested string) error {
	if len(v.badModels) == 0 {
		return nil
	}
	if requested != "" {
		id, _, err := SplitModelEffort(requested)
		if err != nil {
			return nil
		}
		return v.bad(id)
	}
	if v.file.ModelDefault != "" {
		id, _, _ := SplitModelEffort(v.file.ModelDefault)
		return v.bad(id)
	}
	if len(v.badModels) < v.listed.Len() {
		return nil
	}
	reasons := make([]string, 0, len(v.badModels))
	for _, item := range v.badModels {
		reasons = append(reasons, item.err.Error())
	}
	return configError("%s: no model it lists can be used: %s", v.entry.Path, strings.Join(reasons, "; "))
}

// modelFileProblem is nil when the file at path passes the model rules.
func modelFileProblem(path string) error {
	_, err := loadModel(path, "", "")
	return err
}

// skippedNotes returns one warning per profile left out of the session. A profile that
// would have been the default says which one is.
func (j scopeJudgement) skippedNotes(sessionPath, wouldBeDefault, defaultID string) []string {
	var notes []string
	for _, id := range j.leftOut() {
		note := fmt.Sprintf("%s: profile %s left out: %v", sessionPath, id, j.verdicts[id].err)
		if j.verdicts[id].toolsets {
			note += fmt.Sprintf("; its settings fail with the toolsets it names (%s) and load without them", strings.Join(j.verdicts[id].file.Toolset, ", "))
		}
		if id == wouldBeDefault && defaultID != "" {
			note += fmt.Sprintf("; the default profile is %s instead", defaultID)
		}
		notes = append(notes, note)
	}
	return notes
}
