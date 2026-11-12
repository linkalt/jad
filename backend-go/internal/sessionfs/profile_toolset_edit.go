package sessionfs

import (
	"fmt"
	"strings"
)

// Self-edit of a profile's own toolsets (phase 3c.3, docs/design_workspace_layout.md §21):
// edit's toolsets create, replace or delete files in {profile_own}/tools in the same revision as
// the profile file, the documents and the own models, under the profile file's lock, with the
// same trial, history, publish order and revert. An own toolset holds only based_on, omit and
// set (the ownToolsetTools table) at every profile_self_edit level; the hooks, MCP connections,
// paths, commands, environments, endpoints and limits always come from the base.

// checkOwnToolsetSteps applies the own toolset rules to each toolset an edit or revert writes,
// before the trial: the file rules and a name no shared toolset has (decider 3c C2: self-edit
// never makes a clash a bare ID would hit). A written file whose base is missing, or that does
// not compose with it, is refused by the trial (judgeTrialToolsets).
func checkOwnToolsetSteps(layout Layout, steps []documentStep) error {
	if len(steps) == 0 {
		return nil
	}
	scopes, err := NewCatalogScans().scanScopeList("toolset", layout.Tools, layout.WorkspaceTools)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if step.delete {
			continue
		}
		id := step.record.Path
		for _, scope := range scopes {
			if _, found := scope.Catalog.Get(id); found {
				return &ClientError{Message: fmt.Sprintf("toolsets %s: %s:%s is a shared toolset; an own toolset takes a name no shared toolset has, so a bare %s keeps meaning one toolset", id, scope.Name, id, id)}
			}
		}
		if _, err := parseOwnToolset("toolsets "+id, step.content); err != nil {
			return &ClientError{Message: err.Error()}
		}
	}
	return nil
}

// judgeTrialToolsets is the toolset part of an edit's trial: each own toolset the change writes
// must find its base, compose with it and pass the toolset rules, whether the profile names it
// yet or not. A toolset the profile names is also applied by the trial load itself.
func judgeTrialToolsets(layout Layout, root Root, scans *CatalogScans, profile string, steps []documentStep) error {
	var written []string
	for _, step := range steps {
		if step.kind == ownToolsetKind && !step.delete {
			written = append(written, step.record.Path)
		}
	}
	if len(written) == 0 {
		return nil
	}
	own, err := layout.profileToolsets(scans, profile)
	if err != nil {
		return err
	}
	found := map[string]string{}
	for path, id := range own.ids {
		found[id] = path
	}
	for _, id := range written {
		path, ok := found[id]
		if !ok {
			return fmt.Errorf("own toolset %s is left out: %s", id, strings.Join(own.notes, "; "))
		}
		toolset, err := own.strictToolset(path)
		if err == nil {
			err = validateLayoutToolset(layout, []Root{root}, path, toolset)
		}
		if err != nil {
			return fmt.Errorf("own toolset %s: %w", id, err)
		}
	}
	return nil
}
