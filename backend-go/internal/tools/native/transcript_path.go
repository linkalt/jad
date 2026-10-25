package native

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"jad/backend-go/internal/tools"
)

// transcriptPathKinds are the forms of path, as the error for a bad one lists them.
const transcriptPathKinds = "profile:<category>/<name>, profile:<session>/<category>/<name>, subagent:<run id>, subagent:<session>/<run id>, path:<file>"

var transcriptRunID = regexp.MustCompile("^sub_[0-9a-f]{8}$")

// splitTranscriptName splits the part of path after its prefix at '/' and refuses an empty
// segment, a dot segment, a backslash and NUL, so a name cannot climb out of what it names.
func splitTranscriptName(text string) ([]string, bool) {
	parts := strings.Split(text, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return nil, false
		}
	}
	return parts, true
}

// nearestTranscriptNames puts the names that share the longest start with query, or contain it,
// first, then the rest in order, and cuts the list at limit.
func nearestTranscriptNames(query string, names []string, limit int) []string {
	query = strings.ToLower(query)
	score := func(name string) int {
		name = strings.ToLower(name)
		if query != "" && strings.Contains(name, query) {
			return len(query) + 1
		}
		common := 0
		for common < len(name) && common < len(query) && name[common] == query[common] {
			common++
		}
		return common
	}
	sorted := append([]string(nil), names...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if a, b := score(sorted[i]), score(sorted[j]); a != b {
			return a > b
		}
		return sorted[i] < sorted[j]
	})
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	return sorted
}

// notFound is a not-found error that lists the names nearest to query.
func (t *transcriptTool) notFound(execution tools.ExecutionContext, code, summary, query string, names []string) string {
	var information []tools.Information
	if names = nearestTranscriptNames(query, names, int(t.settings.SuggestMax)); len(names) > 0 {
		information = append(information, tools.Information{Key: "available", Value: strings.Join(names, ", ")})
	}
	return t.fail(execution, tools.StatusNotFound, code, summary, information...)
}

// resolve finds the files of the transcript that selector (the path input) names and checks
// that the caller may read them. An empty selector is the caller's own transcript, or the
// caller's own run when it is a sub-agent. canonical is the exact path to give again, empty for
// the own transcript.
func (t *transcriptTool) resolve(ctx context.Context, selector, source string, execution tools.ExecutionContext) (TranscriptFiles, string, string) {
	forbid := func(code, summary string) (TranscriptFiles, string, string) {
		return TranscriptFiles{}, "", t.fail(execution, tools.StatusForbidden, code, summary)
	}
	invalid := func(summary string) (TranscriptFiles, string, string) {
		return TranscriptFiles{}, "", t.fail(execution, tools.StatusInvalidInput, "invalid_path", summary, tools.Information{Key: "valid", Value: transcriptPathKinds})
	}
	policy := scopedPolicy(t.settings.Policy, execution)
	callerSession, callerProfile, _ := strings.Cut(execution.SessionKey, "\x00")
	if selector == "" {
		if execution.AgentDepth == 0 || execution.ProcessOwner == "" {
			return t.own, "", ""
		}
		selector = "subagent:" + execution.ProcessOwner
	}
	kind, rest, found := strings.Cut(selector, ":")
	if !found || rest == "" {
		return invalid("path must start with a prefix and name something after it")
	}
	switch kind {
	case "path":
		if source == "backup" {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "source backup does not apply to path: name the backup file itself")
		}
		if !t.settings.AllowPath {
			return forbid("transcript_path_disabled", "Reading a transcript by path is off: set tools.native.transcript.allow_path to true")
		}
		target, failure := resolveExistingTarget(rest, execution.WorkspaceRoot, policy, execution.CallID)
		if failure != "" {
			return TranscriptFiles{}, "", failure
		}
		if !looksLikeTranscript(target.realPath, t.settings.LineMaxBytes) {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusInvalidInput, "not_a_transcript", "The file is not a messages.jsonl transcript", tools.Information{Key: "path", Value: target.displayPath})
		}
		return TranscriptFiles{Messages: target.realPath}, "path:" + target.displayPath, ""
	case "subagent":
		parts, ok := splitTranscriptName(rest)
		if !ok || len(parts) > 2 || !transcriptRunID.MatchString(parts[len(parts)-1]) {
			return invalid("subagent needs a run id (sub_ and eight hex digits), optionally after the session: subagent:<run id> or subagent:<session>/<run id>")
		}
		if source == "backup" {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "source backup does not apply to subagent: a sub-agent transcript has no backup")
		}
		if t.runs == nil {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusError, "subagent_unavailable", "Sub-agent runs are not available in this session")
		}
		id := parts[len(parts)-1]
		dir, owner, found := t.runs.RunTranscript(id)
		ownerSession, _, _ := strings.Cut(owner, "\x00")
		if found && len(parts) == 2 && parts[0] != ownerSession && !strings.HasSuffix(ownerSession, ":"+parts[0]) {
			found = false
		}
		if !found {
			var ids []string
			for _, run := range t.runs.ownedRuns(execution.SessionKey) {
				ids = append(ids, run.id)
			}
			return TranscriptFiles{}, "", t.notFound(execution, "no_such_run", "No sub-agent run "+rest, id, ids)
		}
		if owner != execution.SessionKey {
			if !t.settings.AllowChildren {
				return forbid("transcript_subagent_denied", "This run was not started by this session: tools.native.transcript.allow_children is false")
			}
			if ownerSession != callerSession && !t.settings.AllowOtherSessions {
				return forbid("transcript_session_denied", "This run belongs to another session: tools.native.transcript.allow_other_sessions is false")
			}
		}
		if dir == "" {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusNotFound, "no_transcript_yet", "Sub-agent run "+id+" has no transcript yet")
		}
		files := TranscriptFiles{Messages: filepath.Join(dir, "messages.jsonl")}
		if t.deniedFile(files.Messages, policy) {
			return forbid("path_denied", "The sub-agent transcript is denied by the read path rules")
		}
		return files, "subagent:" + id, ""
	case "profile":
		parts, ok := splitTranscriptName(rest)
		if !ok || len(parts) < 2 || len(parts) > 3 {
			return invalid("profile needs <category>/<name>, optionally after the session: profile:<category>/<name> or profile:<session>/<category>/<name>")
		}
		if t.profiles == nil {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusError, "profile_unavailable", "Reading another profile's transcript is not available here")
		}
		session, profile := "", strings.Join(parts[len(parts)-2:], "/")
		if len(parts) == 3 {
			session = parts[0]
		}
		target, err := t.profiles.Profile(ctx, execution.SessionKey, session, profile)
		if err != nil {
			profiles, sessions := t.profiles.Candidates(ctx, execution.SessionKey, session)
			names, query := profiles, profile
			if profiles == nil {
				names, query = sessions, session
			}
			return TranscriptFiles{}, "", t.notFound(execution, "no_such_profile", err.Error(), query, names)
		}
		otherSession, otherProfile := target.Session != callerSession, target.Profile != callerProfile
		if otherSession && !t.settings.AllowOtherSessions {
			return forbid("transcript_session_denied", "Another session's transcript: tools.native.transcript.allow_other_sessions is false")
		}
		if otherProfile && !t.settings.AllowOtherProfiles {
			return forbid("transcript_profile_denied", "Another profile's transcript: tools.native.transcript.allow_other_profiles is false")
		}
		if (otherSession || otherProfile) && t.deniedFile(target.Files.Messages, policy) {
			return forbid("path_denied", "The transcript is denied by the read path rules")
		}
		return target.Files, "profile:" + target.Session + "/" + target.Profile, ""
	}
	return invalid("unknown prefix " + kind + ":")
}
