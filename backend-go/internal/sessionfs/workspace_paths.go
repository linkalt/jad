package sessionfs

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// In the workspace layout a relative path in a settings file resolves from the folder the
// file came from (decider ruling on phase 1a): W/.jad for the files of a workspace (its
// sessions, profiles, toolsets and models) and G for the global ones. The settings of a
// session are composed into one object and read with one base, so the base is W/.jad
// (Root.pathBase) and the relative paths of a global toolset are made absolute from G before
// they are merged in (absoluteGlobalToolsetPaths). A v1 root keeps its setting folder.

// workspacePlaceholder starts a prompt document path that always means the workspace W, also
// for a session or profile working in a worktree or another cwd (design D1, D2).
const workspacePlaceholder = "{workspace}"

// pathBase is the folder relative paths of a session's settings resolve from: the root
// setting's folder in v1, W/.jad in the workspace layout.
func (r Root) pathBase() string {
	if r.Layout != nil && r.Layout.Workspace != "" {
		return filepath.Join(r.Layout.Workspace, ".jad")
	}
	return r.GlobalJadRoot
}

// globalToolset reports whether the toolset file at path is a global one of the workspace
// layout, whose relative paths resolve from G.
func (l Layout) globalToolset(path string) bool {
	return l.Workspace != "" && l.GlobalDir != "" && l.Tools != "" && PathWithin(l.Tools, path)
}

// policyPathTools are the native tools whose paths key is a path policy read with the root
// folder as its base (normalizeNativePathPolicy).
var policyPathTools = []string{"exec", "read_image", "lsp", "code_context"}

// absoluteGlobalToolsetPaths makes the relative paths of a global toolset absolute from G:
// env_file of exec and tool_smith and of hooks, and the path rules of the tools in
// policyPathTools. Each value becomes what the normalizer would have made of it with G as
// the base; a value of the wrong shape is left for the normalizer to refuse.
func absoluteGlobalToolsetPaths(toolset layoutToolsetFile, global string) layoutToolsetFile {
	if len(toolset.Native) > 0 {
		native := make(map[string]json.RawMessage, len(toolset.Native))
		for name, raw := range toolset.Native {
			native[name] = raw
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil || fields == nil {
				continue
			}
			changed := false
			if containsString(envFileTools, name) {
				changed = absoluteEnvFile(fields, global) || changed
			}
			if containsString(policyPathTools, name) {
				if paths, ok := fields["paths"]; ok {
					if rewritten, did := absolutePolicyPaths(paths, global); did {
						fields["paths"], changed = rewritten, true
					}
				}
			}
			if changed {
				native[name], _ = json.Marshal(fields)
			}
		}
		toolset.Native = native
	}
	if len(toolset.Hooks) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(toolset.Hooks, &fields) == nil && fields != nil && absoluteEnvFile(fields, global) {
			toolset.Hooks, _ = json.Marshal(fields)
		}
	}
	return toolset
}

// absoluteEnvFile replaces a relative env_file string with its path from base, as
// loadConfiguredEnvFile resolves it.
func absoluteEnvFile(fields map[string]json.RawMessage, base string) bool {
	var value string
	if raw, ok := fields["env_file"]; !ok || json.Unmarshal(raw, &value) != nil || strings.TrimSpace(value) == "" {
		return false
	}
	value = strings.TrimSpace(value)
	if filepath.IsAbs(value) {
		return false
	}
	fields["env_file"] = mustJSON(resolveFrom(base, value))
	return true
}

// absolutePolicyPaths rewrites the path strings of a path policy (an array of {action, path}
// rules, or {allow, deny} lists of strings or {path} objects) to what resolveNativeRulePath
// makes of them from base.
func absolutePolicyPaths(raw json.RawMessage, base string) (json.RawMessage, bool) {
	rewrite := func(value string) (string, bool) {
		if strings.TrimSpace(value) == "" {
			return value, false
		}
		resolved := resolveNativeRulePath(value, base)
		return resolved, resolved != value
	}
	// pathOf rewrites one entry: a string, or an object with a path.
	pathOf := func(entry json.RawMessage) (json.RawMessage, bool) {
		var text string
		if json.Unmarshal(entry, &text) == nil {
			if resolved, did := rewrite(text); did {
				return mustJSON(resolved), true
			}
			return entry, false
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil || fields == nil || json.Unmarshal(fields["path"], &text) != nil {
			return entry, false
		}
		resolved, did := rewrite(text)
		if !did {
			return entry, false
		}
		fields["path"] = mustJSON(resolved)
		encoded, _ := json.Marshal(fields)
		return encoded, true
	}
	list := func(raw json.RawMessage) (json.RawMessage, bool) {
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) != nil || entries == nil {
			return pathOf(raw)
		}
		changed := false
		for index, entry := range entries {
			var did bool
			if entries[index], did = pathOf(entry); did {
				changed = true
			}
		}
		if !changed {
			return raw, false
		}
		encoded, _ := json.Marshal(entries)
		return encoded, true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		changed := false
		for _, key := range []string{"allow", "deny"} {
			if value, ok := object[key]; ok {
				var did bool
				if object[key], did = list(value); did {
					changed = true
				}
			}
		}
		if !changed {
			return raw, false
		}
		encoded, _ := json.Marshal(object)
		return encoded, true
	}
	return list(raw)
}

// hideFixedPaths gives the path rules of every in-process reader the layout's fixed hidden
// paths (decider fix C): code_context, lsp, read_image, the transcript, transcripts and profile
// tools, the UI file list and the cwd checks of exec and tool_smith answer a path in G/auth or
// another workspace's .jad as missing, links followed, with or without a sandbox. Nothing in v1.
func (c *SessionConfig) hideFixedPaths(hidden []string) {
	if len(hidden) == 0 {
		return
	}
	hide := func(policy *NativePathPolicy) {
		policy.Hidden = appendNewStrings(append([]string(nil), policy.Hidden...), hidden...)
	}
	hide(&c.FilePolicy)
	if c.NativeReadImage != nil {
		hide(&c.NativeReadImage.Policy)
	}
	if c.NativeLSP != nil {
		hide(&c.NativeLSP.Policy)
	}
	if c.NativeCodeContext != nil {
		hide(&c.NativeCodeContext.Policy)
	}
	if c.NativeExec != nil {
		hide(&c.NativeExec.Child.Policy)
	}
	if c.NativeToolSmith != nil {
		hide(&c.NativeToolSmith.Child.Policy)
	}
	if c.NativeTranscript != nil {
		hide(&c.NativeTranscript.Policy)
	}
	if c.NativeTranscripts != nil {
		hide(&c.NativeTranscripts.Policy)
	}
	if c.NativeProfile != nil {
		hide(&c.NativeProfile.Policy)
	}
}
