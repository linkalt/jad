package sessionfs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Self-edit of a profile's own models (docs/design_workspace_layout.md §19, decider 3c C1 to
// C3): edit's models create, replace or delete files in {profile_own}/models in the same
// revision as the profile file and the documents, under the profile file's lock, with the same
// trial, history, publish order and revert. An own model holds only based_on and set (the
// ownModelSet table); the endpoint, the credential and the billed model always come from the
// base, at every profile_self_edit level.

// indentedOwnModels checks that each model an edit gives is a JSON object and writes it with
// two spaces of indent and a final newline, so the file and its revision do not depend on how
// the tool call spelled it.
func indentedOwnModels(changes []ProfileDocumentChange) ([]ProfileDocumentChange, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	indented := make([]ProfileDocumentChange, len(changes))
	for index, change := range changes {
		indented[index] = change
		if change.Delete || change.Content == nil {
			continue
		}
		trimmed := bytes.TrimSpace(change.Content)
		var fields map[string]json.RawMessage
		if !jsonKindIs(trimmed, "object") || json.Unmarshal(trimmed, &fields) != nil {
			return nil, &ClientError{Message: fmt.Sprintf("models[%d] %s: content must be the own model file as a JSON object", index, change.Path)}
		}
		var out bytes.Buffer
		if err := json.Indent(&out, trimmed, "", "  "); err != nil {
			return nil, &ClientError{Message: fmt.Sprintf("models[%d] %s: %v", index, change.Path, err)}
		}
		out.WriteByte('\n')
		indented[index].Content = out.Bytes()
	}
	return indented, nil
}

// checkOwnModelSteps applies the own model rules to each model an edit or revert writes, before
// the trial: the file rules (keys, schema_version, based_on with a shared scope, set through
// the ownModelSet allow-list) and a name that no shared model has (decider 3c C2: self-edit
// never makes a clash a bare ID would hit). A written file whose base is missing or fails is
// refused by the trial (judgeTrialModels).
func checkOwnModelSteps(layout Layout, steps []documentStep) error {
	if len(steps) == 0 {
		return nil
	}
	scopes, err := NewCatalogScans().scanScopeList("model", layout.Models, layout.WorkspaceModels)
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
				return &ClientError{Message: fmt.Sprintf("models %s: %s:%s is a shared model; an own model takes a name no shared model has, so a bare %s keeps meaning one model", id, scope.Name, id, id)}
			}
		}
		if _, err := parseOwnModel("models "+id, step.content); err != nil {
			return &ClientError{Message: err.Error()}
		}
	}
	return nil
}

// judgeTrialModels is the model part of an edit's trial (decider 3c C3): each own model the
// change writes must be usable (its base found and the composed model passing the model
// rules), and the model a run of the profile would use must load with its effort. A selected
// model that fails only the way it already failed before the change does not block it, so a
// profile can still change other things while a base it uses is broken.
func judgeTrialModels(layout Layout, scans *CatalogScans, loaded, trial SessionConfig, steps []documentStep) error {
	written := map[string]bool{}
	for _, step := range steps {
		if step.kind == ownModelKind && !step.delete {
			written[step.record.Path] = true
		}
	}
	if len(written) > 0 {
		own, err := layout.profileModels(scans, loaded.Profile)
		if err != nil {
			return err
		}
		found := map[string]string{}
		for path, id := range own.ids {
			found[id] = path
		}
		for id := range written {
			path, ok := found[id]
			if !ok {
				return fmt.Errorf("own model %s is left out: %s", id, strings.Join(own.notes, "; "))
			}
			if err := own.problem(path); err != nil {
				return fmt.Errorf("own model %s: %w", id, err)
			}
		}
	}
	after := selectedModelProblem(trial)
	if after == nil {
		return nil
	}
	if before := selectedModelProblem(loaded); before != nil && before.Error() == after.Error() {
		return nil
	}
	return fmt.Errorf("the model a run of the profile would use: %w", after)
}

// selectedModelProblem is why the model a run of config would use cannot run: the load's
// ModelFault, or the error of reading it with its effort (WithModel); nil when it can.
func selectedModelProblem(config SessionConfig) error {
	if config.ModelFault != nil {
		return config.ModelFault
	}
	_, err := config.WithModel()
	return err
}
