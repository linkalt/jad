package native

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// selfChange runs edit, revert and history: actions on the caller's own profile file only
// (docs/design_workspace_layout.md §4.7, decider S2). They are refused to a sub-agent whatever
// allow_children says and to another profile whatever allow_other_profiles says; edit and
// revert also in plan mode (whatever plan_mode.allow keeps) and in a run that another run, a
// profile_message or a remote server started.
func (t *profileTool) selfChange(ctx context.Context, action, path string, arguments map[string]any, execution tools.ExecutionContext) string {
	if execution.AgentDepth > 0 {
		return t.fail(execution, tools.StatusForbidden, "profile_children_denied", "A sub-agent may not "+action+" a profile: "+action+" is for a profile's own runs")
	}
	callerSession, callerProfile, hasCaller := strings.Cut(execution.SessionKey, "\x00")
	if !hasCaller || callerSession == "" || callerProfile == "" {
		return t.fail(execution, tools.StatusForbidden, "profile_caller_unknown", "The caller's own profile is not known here: "+action+" needs a session and a profile")
	}
	if path != "" && path != "self" {
		session, profile, ok := parseProfilePath(path)
		if !ok {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_path", "path is not a form this tool accepts", tools.Information{Key: "valid", Value: "self"})
		}
		if profile != callerProfile || (session != "" && session != callerSession) {
			return t.fail(execution, tools.StatusForbidden, "profile_self_only", action+" acts only on your own profile: omit path, or give self")
		}
	}
	if action != "history" {
		if execution.PlanMode || execution.PlanGuarded {
			return t.fail(execution, tools.StatusForbidden, "profile_plan_mode_denied", "Plan mode changes nothing: "+action+" is refused in plan mode")
		}
		if execution.ChainDepth > 0 || execution.RunNode != "" || execution.RemoteHops > 0 {
			return t.fail(execution, tools.StatusForbidden, "profile_started_run_denied", "This run was started by another run, a profile_message or a remote server: only the profile's own top-level runs may "+action+" it")
		}
	}
	if t.actions == nil {
		return t.fail(execution, tools.StatusError, "profile_unavailable", "Profiles are not available to this tool")
	}
	if action == "history" {
		limit := int(t.settings.MaxEntries)
		if value, ok := safeInteger(arguments["limit"]); ok {
			limit = int(min(value, t.settings.MaxEntries))
		}
		history, err := t.actions.ProfileHistory(ctx, execution.SessionKey, limit)
		if err != nil {
			return t.fail(execution, tools.StatusError, "profile_history_failed", err.Error())
		}
		return indentedJSON(execution, t, history)
	}
	request := sessionfs.ProfileEditRequest{Action: action, IfMatch: stringArgument(arguments, "if_match"),
		Audit: sessionfs.ProfileEditAudit{CallID: execution.CallID, Origin: "the profile's own top-level run"}}
	if strings.TrimSpace(request.IfMatch) == "" {
		return t.fail(execution, tools.StatusInvalidInput, "if_match_required", action+" needs if_match: the revision config showed")
	}
	if action == "edit" {
		contents, ok := rawArgument(execution.RawArguments, arguments, "contents")
		if !ok {
			return t.fail(execution, tools.StatusInvalidInput, "contents_required", "edit needs contents: the whole new profile file as a JSON object")
		}
		request.Contents = contents
	} else {
		request.HistoryID = stringArgument(arguments, "history_id")
		if strings.TrimSpace(request.HistoryID) == "" {
			return t.fail(execution, tools.StatusInvalidInput, "history_id_required", "revert needs history_id: an entry the history action lists")
		}
	}
	result, err := t.actions.EditProfile(ctx, execution.SessionKey, request)
	if err != nil {
		return t.fail(execution, editFailureStatus(err), editFailureCode(err), err.Error())
	}
	return indentedJSON(execution, t, result)
}

// rawArgument is the JSON object of one argument as the model wrote it, so the keys of a
// profile file keep their order. The raw text is used only when it says what the parsed
// arguments say; otherwise the parsed value is encoded.
func rawArgument(raw json.RawMessage, arguments map[string]any, key string) (json.RawMessage, bool) {
	value, found := arguments[key].(map[string]any)
	if !found {
		return nil, false
	}
	if len(raw) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			var decoded any
			if text, found := fields[key]; found && json.Unmarshal(text, &decoded) == nil && reflect.DeepEqual(decoded, any(value)) {
				return text, true
			}
		}
	}
	encoded, err := json.Marshal(value)
	return encoded, err == nil
}

// editFailureCode names why an edit or revert was refused.
func editFailureCode(err error) string {
	var catalogErr *sessionfs.CatalogError
	var conflict *sessionfs.ConflictError
	var client *sessionfs.ClientError
	switch {
	case errors.As(err, &catalogErr) && catalogErr.Kind == sessionfs.CatalogPreconditionFailed:
		return "profile_revision_changed"
	case errors.As(err, &catalogErr) && catalogErr.Kind == sessionfs.CatalogPreconditionRequired:
		return "if_match_required"
	case errors.As(err, &conflict):
		return "profile_edit_conflict"
	case errors.As(err, &client):
		return "profile_edit_refused"
	}
	return "profile_edit_failed"
}

func editFailureStatus(err error) tools.Status {
	var client *sessionfs.ClientError
	if errors.As(err, &client) {
		return tools.StatusInvalidInput
	}
	return tools.StatusError
}

func indentedJSON(execution tools.ExecutionContext, t *profileTool, value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return t.fail(execution, tools.StatusError, "profile_encode_failed", err.Error())
	}
	return string(encoded)
}
