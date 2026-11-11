package sessionfs

import (
	"fmt"

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
// Nothing is run in place of what was chosen. A profile the caller names must pass itself,
// and a model the profile names as its model_default, or the caller requests, stays offered
// even when its file fails: the session is listed and the run stops with that file's error,
// after its input is stored, instead of using another model.

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
func judgeScope(layout Layout, session layoutSessionFile, sessionPath string, scans *CatalogScans) (scopeJudgement, error) {
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
			verdict.badModels = badModelsOf(scans, narrowed)
		}
		if verdict.err != nil {
			judgement.usable = judgement.usable.Without(entry.ID())
		}
	}
	return judgement, nil
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

// offer returns the models the profile offers and a warning for each that fails. A model that
// fails is left out unless it is the profile's model_default or the model the caller
// requests (keep), or every listed model fails: the run then says what is wrong with it.
func (v *profileVerdict) offer(sessionPath, profileID, keep string) (*catalog.Catalog, []string) {
	if len(v.badModels) == 0 {
		return v.listed, nil
	}
	chosen := map[string]string{}
	if v.file.ModelDefault != "" {
		id, _, _ := SplitModelEffort(v.file.ModelDefault)
		chosen[id] = "its model_default"
	}
	if keep != "" {
		chosen[keep] = "the requested model"
	}
	offered := v.listed
	var dropped []badModel
	var notes []string
	for _, item := range v.badModels {
		if why, kept := chosen[item.id]; kept {
			notes = append(notes, fmt.Sprintf("%s: model %s of profile %s cannot be used and is still offered as %s, so a run with it fails: %v", sessionPath, item.id, profileID, why, item.err))
			continue
		}
		dropped = append(dropped, item)
		offered = offered.Without(item.id)
	}
	if offered.Len() == 0 {
		// Nothing usable is listed: keep them all, so that a run reports the file's error.
		for _, item := range dropped {
			notes = append(notes, fmt.Sprintf("%s: model %s of profile %s cannot be used and no model of the profile can: %v", sessionPath, item.id, profileID, item.err))
		}
		return v.listed, notes
	}
	for _, item := range dropped {
		notes = append(notes, fmt.Sprintf("%s: model %s left out of profile %s: %v", sessionPath, item.id, profileID, item.err))
	}
	return offered, notes
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
		if id == wouldBeDefault && defaultID != "" {
			note += fmt.Sprintf("; the default profile is %s instead", defaultID)
		}
		notes = append(notes, note)
	}
	return notes
}
