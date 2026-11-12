package sessionfs

import (
	"errors"
	"os"
)

// errWorkspaceSessionNotFound is what a tool gets for a session outside its workspace and for
// one that does not exist: the same error, so the answer does not tell that a session of
// another workspace exists (design Q6).
var errWorkspaceSessionNotFound = &ResolveError{Kind: ResolveSessionNotFound, Message: "session not found"}

// WorkspaceLayout reports whether the roots are the workspaces of the workspace layout (v2).
func WorkspaceLayout(roots []Root) bool {
	for _, root := range roots {
		if root.Layout != nil && root.Layout.Workspace != "" {
			return true
		}
	}
	return false
}

// ResolveFrom resolves reference as a tool of the session caller sees it (transcript,
// transcripts, profile, profile_message and the sub-agent runs they reach). In the workspace
// layout only the caller's own workspace is visible: an unqualified ID is looked up there, a
// session of another workspace (even named <workspace>:<id>) and a session that does not
// exist are both "session not found". Outside the workspace layout it is Resolve.
func ResolveFrom(roots []Root, caller, reference string) (Location, error) {
	if !WorkspaceLayout(roots) {
		return Resolve(roots, reference)
	}
	from, err := Resolve(roots, caller)
	if err != nil {
		return Location{}, errWorkspaceSessionNotFound
	}
	rootID, sessionID, qualified, err := parseReference(reference)
	if err != nil {
		return Location{}, err
	}
	if qualified && rootID != from.Root.ID {
		return Location{}, errWorkspaceSessionNotFound
	}
	location := makeLocation(from.Root, sessionID, len(roots))
	info, err := os.Stat(location.File)
	if err != nil || !info.Mode().IsRegular() {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Location{}, err
		}
		return Location{}, errWorkspaceSessionNotFound
	}
	return location, nil
}

// VisibleFrom keeps the session references (as ListSessionFiles gives them) a tool of the
// session caller may see: in the workspace layout those of its own workspace, else all.
func VisibleFrom(roots []Root, caller string, references []string) []string {
	if !WorkspaceLayout(roots) {
		return references
	}
	from, err := Resolve(roots, caller)
	if err != nil {
		return nil
	}
	kept := references[:0:0]
	for _, reference := range references {
		if location, err := Resolve(roots, reference); err == nil && location.Root.ID == from.Root.ID {
			kept = append(kept, reference)
		}
	}
	return kept
}
