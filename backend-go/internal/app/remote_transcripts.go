package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

const remoteTranscriptCaller = "remote-api\x00reader"

var errRemoteProfileHidden = errors.New("profile not found")

// tokenProfiles lets the transcript tools see only the profiles a token's globs reach. A
// profile outside them answers like one that does not exist, and no session is ever named.
type tokenProfiles struct {
	inner profileMessenger
	may   func(target string) bool
	// mayAgent limits sub-agent runs to the agents the token's globs reach.
	mayAgent func(target string) bool
}

func (p tokenProfiles) Profile(ctx context.Context, ownerKey, session, profile string) (native.TranscriptTarget, error) {
	// Before anything is looked up, a cheap filter: the session as written, or its bare id (a
	// cursor names the canonical <workspace>:<session> of a read that began with the bare id).
	// The decision is the one after the lookup.
	if !p.mayAnyOf(p.sessionNames(session), p.may, profile) {
		return native.TranscriptTarget{}, errRemoteProfileHidden
	}
	target, err := p.inner.Profile(ctx, ownerKey, session, profile)
	if err != nil {
		return native.TranscriptTarget{}, errRemoteProfileHidden
	}
	names := p.sessionNames(target.Session)
	if !p.mayAnyOf(names, p.may, target.Profile) {
		return native.TranscriptTarget{}, errRemoteProfileHidden
	}
	target.MayAgent = func(agent string) bool { return p.mayAnyOf(names, p.mayAgent, target.Profile+"/"+agent) }
	return target, nil
}

// Candidates names only profiles the token reaches and never other sessions, so a missing
// session, a missing profile and one outside the token look the same.
func (p tokenProfiles) Candidates(ctx context.Context, ownerKey, session string) ([]string, []string) {
	profiles, _ := p.inner.Candidates(ctx, ownerKey, session)
	visible := make([]string, 0, len(profiles))
	for _, name := range profiles {
		if p.may(session+"/"+name) || p.mayAnyOf(p.sessionNames(canonicalOr(p.inner, session)), p.may, name) {
			visible = append(visible, name)
		}
	}
	if len(visible) == 0 {
		return nil, nil
	}
	return visible, nil
}

// sessionNames are the names a token's globs may be tried against for the session reference
// (the canonical name of a session a read resolved): the reference itself, and for a
// <workspace>:<session> one also its bare id, but only while that bare id resolves to exactly
// this session: the lookup a start request makes for the bare id. A token written for bare ids
// can start a run on the session, so it may read what the run wrote; a bare id that more than one
// workspace holds (or that fails to resolve) gives no second name, so access fails closed.
func (p tokenProfiles) sessionNames(reference string) []string {
	names := []string{reference}
	if _, bare, qualified := strings.Cut(reference, ":"); qualified {
		if location, err := sessionfs.Resolve(p.inner.resolver.roots, bare); err == nil && location.Reference == reference {
			names = append(names, bare)
		}
	}
	return names
}

// mayAnyOf tells whether may holds for rest ("category/name" or "category/name/agent") under
// any of the session names (see sessionNames).
func (p tokenProfiles) mayAnyOf(names []string, may func(string) bool, rest string) bool {
	for _, name := range names {
		if may(name + "/" + rest) {
			return true
		}
	}
	return false
}

func canonicalOr(m profileMessenger, session string) string {
	if location, err := sessionfs.Resolve(m.resolver.roots, session); err == nil {
		return location.Reference
	}
	return session
}

// remoteTranscriptPath checks that a path names an explicit session and a profile, the only
// forms the remote API serves: there is no "own" transcript for a remote caller.
func remoteTranscriptPath(tool, path string) bool {
	kind, rest, found := strings.Cut(path, ":")
	if !found || rest == "" {
		return false
	}
	parts := strings.Split(rest, "/")
	switch {
	case tool == "transcript" && kind == "profile":
		return len(parts) == 3
	case tool == "transcript" && kind == "subagent":
		return len(parts) == 4
	case tool == "transcripts" && kind == "profile":
		return len(parts) == 3
	case tool == "transcripts" && kind == "session":
		return len(parts) == 1
	}
	return false
}

// Transcript implements remoteapi.TranscriptBackend: it runs the real transcript or
// transcripts tool for the token, with the token's profile globs as the only view, no file
// paths in the result and no path: reads.
func (b remoteBackend) Transcript(ctx context.Context, request remoteapi.TranscriptRequest) (string, error) {
	path, _ := request.Arguments["path"].(string)
	_, hasCursor := request.Arguments["cursor"]
	if !hasCursor && !remoteTranscriptPath(request.Tool, path) {
		return "", &remoteapi.Refusal{Code: "bad_path", Message: "path must name a session and a profile: profile:<session>/<category>/<name>, subagent:<session>/<category>/<name>/<run id> or, for transcripts, session:<id>"}
	}
	if hasCursor && path != "" && !remoteTranscriptPath(request.Tool, path) {
		return "", &remoteapi.Refusal{Code: "bad_path", Message: "a cursor already names its transcript; do not give path with it"}
	}
	transcriptSettings, transcriptsSettings, err := sessionfs.RemoteAPITranscriptSettings()
	if err != nil {
		return "", err
	}
	profiles := tokenProfiles{inner: profileMessenger{resolver: b.resolver}, may: request.MayProfile, mayAgent: request.MayAgent}
	var tool tools.Tool
	if request.Tool == "transcript" {
		tool, err = native.NewTranscriptTool(transcriptSettings, native.TranscriptFiles{}, profiles, nil)
	} else {
		tool, err = native.NewTranscriptsTool(transcriptsSettings, native.TranscriptFiles{}, profiles, nil, nil)
	}
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(request.Arguments)
	if err != nil {
		return "", err
	}
	issues, err := tool.Validator.Validate(encoded)
	if err != nil {
		return "", err
	}
	if len(issues) > 0 {
		return "", &remoteapi.Refusal{Code: "bad_request", Message: "the arguments do not fit the " + request.Tool + " tool"}
	}
	return tool.Execute(ctx, request.Arguments, tools.ExecutionContext{CallID: "remote", SessionKey: remoteTranscriptCaller})
}
