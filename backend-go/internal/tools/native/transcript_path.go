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
const transcriptPathKinds = "profile:<category>/<name>, profile:<session>/<category>/<name>, subagent:<run id>, subagent:<category>/<name>/<run id>, subagent:<session>/<category>/<name>/<run id>, path:<file>"

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
		if !ok || (len(parts) != 1 && len(parts) != 3 && len(parts) != 4) || !transcriptRunID.MatchString(parts[len(parts)-1]) {
			return invalid("subagent needs a run id (sub_ and eight hex digits) after the profile that started it: subagent:<run id> for your profile, subagent:<category>/<name>/<run id> or subagent:<session>/<category>/<name>/<run id>; a run lives inside a profile, so subagent:<session>/<run id> is gone")
		}
		if source == "backup" {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "source backup does not apply to subagent: a sub-agent transcript has no backup")
		}
		id := parts[len(parts)-1]
		canonical := "subagent:" + id
		var dir, owner string
		var found bool
		var ids []string
		own := len(parts) == 1
		var target TranscriptTarget
		if !own {
			if t.profiles == nil {
				return TranscriptFiles{}, "", t.fail(execution, tools.StatusError, "profile_unavailable", "Reading another profile's sub-agent run is not available here")
			}
			session := ""
			if len(parts) == 4 {
				session = parts[0]
			}
			var failure string
			if target, failure = t.lookupProfile(ctx, execution, session, strings.Join(parts[len(parts)-3:len(parts)-1], "/")); failure != "" {
				return TranscriptFiles{}, "", failure
			}
			otherSession, otherProfile := target.Session != callerSession, target.Profile != callerProfile
			own = !otherSession && !otherProfile
			if refusal := t.profileRefusal(otherSession, otherProfile); refusal != nil {
				return forbid(refusal[0], refusal[1])
			}
			canonical = "subagent:" + target.Session + "/" + target.Profile + "/" + id
		}
		if own {
			canonical = "subagent:" + id
			if t.runs == nil {
				return TranscriptFiles{}, "", t.fail(execution, tools.StatusError, "subagent_unavailable", "Sub-agent runs are not available in this session")
			}
			dir, owner, found = t.runs.RunTranscript(id)
			for _, run := range t.runs.ownedRuns(execution.SessionKey) {
				ids = append(ids, run.id)
			}
		} else {
			var agent string
			dir, owner, agent, found = runOfTarget(target, id)
			if found && target.MayAgent != nil && !target.MayAgent(agent) {
				found = false
			}
			ids = subagentRunIDs(target.SubagentDir, target.MayAgent)
		}
		if !found {
			return TranscriptFiles{}, "", t.notFound(execution, "no_such_run", "No sub-agent run "+rest, id, ids)
		}
		ownerSession, _, _ := strings.Cut(owner, "\x00")
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
		files := TranscriptFiles{Messages: filepath.Join(dir, "messages.jsonl"), OtherSession: ownerSession != callerSession}
		if t.deniedFile(files.Messages, policy) {
			return forbid("path_denied", "The sub-agent transcript is denied by the read path rules")
		}
		return files, canonical, ""
	case "profile":
		parts, ok := splitTranscriptName(rest)
		if !ok || len(parts) < 2 || len(parts) > 3 {
			return invalid("profile needs <category>/<name>, optionally after the session: profile:<category>/<name> or profile:<session>/<category>/<name>")
		}
		if t.profiles == nil {
			return TranscriptFiles{}, "", t.fail(execution, tools.StatusError, "profile_unavailable", "Reading another profile's transcript is not available here")
		}
		session := ""
		if len(parts) == 3 {
			session = parts[0]
		}
		target, failure := t.lookupProfile(ctx, execution, session, strings.Join(parts[len(parts)-2:], "/"))
		if failure != "" {
			return TranscriptFiles{}, "", failure
		}
		otherSession, otherProfile := target.Session != callerSession, target.Profile != callerProfile
		if refusal := t.profileRefusal(otherSession, otherProfile); refusal != nil {
			return forbid(refusal[0], refusal[1])
		}
		if (otherSession || otherProfile) && t.deniedFile(target.Files.Messages, policy) {
			return forbid("path_denied", "The transcript is denied by the read path rules")
		}
		files := target.Files
		files.OtherSession = otherSession
		return files, "profile:" + target.Session + "/" + target.Profile, ""
	}
	return invalid("unknown prefix " + kind + ":")
}

// lookupProfile finds a profile of a session, or fails with a not-found error that names the
// nearest profiles or sessions. Without allow_other_sessions it names only the caller's profiles.
func (t *transcriptTool) lookupProfile(ctx context.Context, execution tools.ExecutionContext, session, profile string) (TranscriptTarget, string) {
	target, err := t.profiles.Profile(ctx, execution.SessionKey, session, profile)
	if err == nil {
		return target, ""
	}
	callerSession, _, _ := strings.Cut(execution.SessionKey, "\x00")
	profiles, sessions := t.profiles.Candidates(ctx, execution.SessionKey, session)
	names, query := profiles, profile
	if profiles == nil {
		names, query = sessions, session
	}
	if !t.settings.AllowOtherSessions && (profiles == nil || session != "" && session != callerSession) {
		names = nil
	}
	return TranscriptTarget{}, t.notFound(execution, "no_such_profile", err.Error(), query, names)
}

// profileRefusal is the error code and text of the switch that forbids reading a profile of
// another session or another profile, nil when both are allowed.
func (t *transcriptTool) profileRefusal(otherSession, otherProfile bool) []string {
	if otherSession && !t.settings.AllowOtherSessions {
		return []string{"transcript_session_denied", "Another session's transcript: tools.native.transcript.allow_other_sessions is false"}
	}
	if otherProfile && !t.settings.AllowOtherProfiles {
		return []string{"transcript_profile_denied", "Another profile's transcript: tools.native.transcript.allow_other_profiles is false"}
	}
	return nil
}

// runOfTarget finds a run in the sub-agent storage of another profile: the live manager when
// the profile has one, else the run's manifest on disk.
func runOfTarget(target TranscriptTarget, id string) (dir, owner, agent string, found bool) {
	if target.Runs != nil {
		if run := target.Runs.byID(id); run != nil {
			run.mu.Lock()
			defer run.mu.Unlock()
			return run.dir, run.owner, run.agent, true
		}
	}
	return readSubagentManifest(target.SubagentDir, id)
}
