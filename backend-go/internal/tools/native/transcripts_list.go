package native

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jad/backend-go/internal/tools"
)

// transcriptsLegend explains the fields of an item line; it is printed once above the items.
const transcriptsLegend = "each line: kind name | last activity (UTC) | size in bytes | path = what to give the transcript tool | file = the full messages.jsonl path"

// transcriptsItem is one listed thing, with its line already formatted. Items are ordered by
// activity (Unix nanoseconds, newest first), then key.
type transcriptsItem struct {
	key      string
	activity int64
	line     string
}

// transcriptsListing is what one level found, before paging.
type transcriptsListing struct {
	what    string // "profiles of session demo"
	items   []transcriptsItem
	hidden  int      // left out because the read path rules deny the file
	skipped int      // could not be read at all
	notes   []string // lines for the header
}

func (t *transcriptsTool) list(ctx context.Context, request transcriptsRequest, execution tools.ExecutionContext) string {
	var listing transcriptsListing
	var failure string
	switch request.level.kind {
	case "sessions":
		listing, failure = t.listSessions(ctx, request, execution)
	case "profiles":
		listing, failure = t.listProfiles(ctx, request, execution)
	default:
		listing, failure = t.listRuns(ctx, request, execution)
	}
	if failure != "" {
		return failure
	}
	if err := ctx.Err(); err != nil {
		return t.contextFailure(execution, err)
	}
	return t.render(execution, request, listing)
}

func (t *transcriptsTool) contextFailure(execution tools.ExecutionContext, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return t.fail(execution, tools.StatusError, "transcripts_timeout", "Listing took longer than tools.native.transcripts.timeout_ms")
	}
	return t.fail(execution, tools.StatusError, "transcripts_cancelled", "The call was cancelled")
}

// tooMany refuses a listing that would examine more items than scan_max_entries.
func (t *transcriptsTool) tooMany(execution tools.ExecutionContext, count int) string {
	return t.fail(execution, tools.StatusInvalidInput, "too_many", "More items match than tools.native.transcripts.scan_max_entries allows: add filters",
		tools.Information{Key: "matched", Value: count}, tools.Information{Key: "scan_max_entries", Value: t.settings.ScanMaxEntries})
}

// matchesFilters is true when every filter matches the name or the description.
func matchesFilters(filters []toolSmithFilter, name, description string) bool {
	return toolSmithMatchesFilters(toolSmithListEntry{name: name, description: description}, filters)
}

// transcriptsFile is what a stat of a transcript file found.
type transcriptsFile struct {
	exists     bool
	unreadable bool
	size       int64
	modified   time.Time
}

// statTranscriptsFile looks at path without opening it. A file that is not there is not an
// error (a profile that never ran has none). A symlink is followed only while its target stays
// inside the folder the link is in, so a link cannot point the listing at a foreign file.
func statTranscriptsFile(path string) transcriptsFile {
	link, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return transcriptsFile{}
		}
		return transcriptsFile{exists: true, unreadable: true}
	}
	info := link
	if link.Mode()&os.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return transcriptsFile{exists: true, unreadable: true}
		}
		realDir, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil || !insideDirectory(realDir, real) {
			return transcriptsFile{exists: true, unreadable: true}
		}
		if info, err = os.Stat(real); err != nil {
			return transcriptsFile{exists: true, unreadable: true}
		}
	}
	if !info.Mode().IsRegular() {
		return transcriptsFile{exists: true, unreadable: true}
	}
	return transcriptsFile{exists: true, size: info.Size(), modified: info.ModTime()}
}

func transcriptsTime(value time.Time) string { return value.UTC().Format(time.RFC3339) }

// transcriptsFileDenied applies the read path rules to a transcript as it is on disk.
func (t *transcriptsTool) fileDenied(path string, execution tools.ExecutionContext) bool {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	return transcriptDenied(scopedPolicy(t.settings.Policy, execution), real)
}

func (t *transcriptsTool) listSessions(ctx context.Context, request transcriptsRequest, execution tools.ExecutionContext) (transcriptsListing, string) {
	callerSession, _, _ := strings.Cut(execution.SessionKey, "\x00")
	var names []string
	if t.sessions != nil && t.settings.AllowOtherSessions {
		names = t.sessions.Sessions(ctx)
	}
	if !t.settings.AllowOtherSessions || t.sessions == nil {
		names = []string{callerSession}
	}
	listing := transcriptsListing{what: "sessions"}
	for _, name := range names {
		if name == "" || !matchesFilters(request.filters, strings.ToLower(name), "") {
			continue
		}
		listing.items = append(listing.items, transcriptsItem{key: name, line: "session " + name + " | path session:" + name})
	}
	if len(listing.items) > int(t.settings.ScanMaxEntries) {
		return listing, t.tooMany(execution, len(listing.items))
	}
	if !t.settings.AllowOtherSessions {
		listing.notes = append(listing.notes, "only your session: tools.native.transcripts.allow_other_sessions is false")
	}
	return listing, ""
}

func (t *transcriptsTool) listProfiles(ctx context.Context, request transcriptsRequest, execution tools.ExecutionContext) (transcriptsListing, string) {
	callerSession, callerProfile, _ := strings.Cut(execution.SessionKey, "\x00")
	session := request.level.session
	if session == "" {
		session = callerSession
	}
	listing := transcriptsListing{what: "profiles of session " + session}
	var names []string
	if t.profiles != nil {
		var sessions []string
		if names, sessions = t.profiles.Candidates(ctx, execution.SessionKey, request.level.session); names == nil && request.level.session != "" && !containsName(sessions, request.level.session) {
			return listing, t.notFound(execution, "no_such_session", "No session "+request.level.session, request.level.session, sessions)
		}
	} else if request.level.session == "" || request.level.session == callerSession {
		names = []string{callerProfile}
	}
	var candidates []string
	for _, name := range names {
		if !t.settings.AllowOtherProfiles && name != callerProfile {
			continue
		}
		if matchesFilters(request.filters, strings.ToLower(name), "") {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) > int(t.settings.ScanMaxEntries) {
		return listing, t.tooMany(execution, len(candidates))
	}
	if !t.settings.AllowOtherProfiles {
		listing.notes = append(listing.notes, "only your profile: tools.native.transcripts.allow_other_profiles is false")
	}
	for _, name := range candidates {
		if err := ctx.Err(); err != nil {
			return listing, t.contextFailure(execution, err)
		}
		files, canonicalSession, canonicalProfile := t.own, callerSession, callerProfile
		if t.profiles != nil {
			target, err := t.profiles.Profile(ctx, execution.SessionKey, request.level.session, name)
			if err != nil {
				listing.skipped++
				continue
			}
			files, canonicalSession, canonicalProfile = target.Files, target.Session, target.Profile
			if (canonicalSession != callerSession || canonicalProfile != callerProfile) && t.fileDenied(files.Messages, execution) {
				listing.hidden++
				continue
			}
		}
		listing.what = "profiles of session " + canonicalSession
		listing.items = append(listing.items, t.profileItem(canonicalProfile, "profile:"+canonicalSession+"/"+canonicalProfile, files, canonicalSession == callerSession && canonicalProfile == callerProfile))
	}
	return listing, ""
}

// profileItem formats one profile: when its transcript last changed, how large it is and
// whether a compaction backup exists, then the path for transcript and the file.
func (t *transcriptsTool) profileItem(name, path string, files TranscriptFiles, own bool) transcriptsItem {
	label := "profile " + name
	if own {
		label += " (yours)"
	}
	file := statTranscriptsFile(files.Messages)
	item := transcriptsItem{key: name}
	switch {
	case file.unreadable:
		item.line = label + " | unreadable: messages.jsonl is not a regular file inside its folder"
		return item
	case !file.exists:
		item.line = label + " | no transcript yet | path " + path + " | file " + files.Messages
		return item
	}
	item.activity = file.modified.UnixNano()
	backup := "no"
	if files.Backup != "" {
		if info := statTranscriptsFile(files.Backup); info.exists && !info.unreadable {
			backup = "yes"
		}
	}
	item.line = fmt.Sprintf("%s | active %s | %d bytes | backup %s | path %s | file %s", label, transcriptsTime(file.modified), file.size, backup, path, files.Messages)
	return item
}

// page orders the items newest first and cuts the page after the cursor.
func (t *transcriptsTool) page(items []transcriptsItem, request transcriptsRequest) (shown []transcriptsItem, more bool) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].activity != items[j].activity {
			return items[i].activity > items[j].activity
		}
		return items[i].key < items[j].key
	})
	if request.after != nil {
		start := sort.Search(len(items), func(i int) bool {
			if items[i].activity != request.after.Activity {
				return items[i].activity < request.after.Activity
			}
			return items[i].key > request.after.Key
		})
		items = items[start:]
	}
	if len(items) > request.limit {
		return items[:request.limit], true
	}
	return items, false
}

func (t *transcriptsTool) render(execution tools.ExecutionContext, request transcriptsRequest, listing transcriptsListing) string {
	total := len(listing.items)
	shown, more := t.page(listing.items, request)
	summary := fmt.Sprintf("%d %s", total, listing.what)
	if request.after != nil || more {
		summary = fmt.Sprintf("%d of %d %s", len(shown), total, listing.what)
	}
	var lines []string
	if len(shown) > 0 {
		lines = append(lines, transcriptsLegend)
		for _, item := range shown {
			lines = append(lines, item.line)
		}
	}
	options := tools.ResultOptions{ID: execution.CallID, Status: tools.StatusOK, Summary: summary}
	for _, note := range listing.notes {
		options.Information = append(options.Information, tools.Information{Key: "note", Value: note})
	}
	if listing.hidden > 0 {
		options.Information = append(options.Information, tools.Information{Key: "hidden", Value: fmt.Sprintf("%d not readable by this caller (read path rules, or allow_* switches)", listing.hidden)})
	}
	if listing.skipped > 0 {
		options.Information = append(options.Information, tools.Information{Key: "skipped", Value: fmt.Sprintf("%d could not be read", listing.skipped)})
	}
	if more {
		last := shown[len(shown)-1]
		options.Information = append(options.Information,
			tools.Information{Key: "truncated", Value: true},
			tools.Information{Key: "cursor", Value: encodeTranscriptsCursor(transcriptsCursor{Level: request.level.text(), Filters: request.texts, Status: request.status, Activity: last.activity, Key: last.key})})
	}
	if len(lines) > 0 {
		options.Content = &tools.Content{Text: strings.Join(lines, "\n")}
	}
	result, err := tools.RenderToolResult(options)
	if err != nil {
		return t.fail(execution, tools.StatusError, "list_failed", "The result could not be rendered", tools.Information{Key: "system_error", Value: err.Error()})
	}
	return result
}

// transcriptsRun is a sub-agent run as the manifest on disk and the live manager know it.
type transcriptsRun struct {
	id, owner, agent, state string
	dir                     string
	startedAt, endedAt      time.Time
}

// callOf is the tool call that started a run: its folder is <call id>-<8 hex digits>.
func (r transcriptsRun) callOf() string {
	name := filepath.Base(r.dir)
	if r.dir == "" || len(name) < 10 || name[len(name)-9] != '-' {
		return ""
	}
	return name[:len(name)-9]
}

func (t *transcriptsTool) listRuns(ctx context.Context, request transcriptsRequest, execution tools.ExecutionContext) (transcriptsListing, string) {
	callerSession, callerProfile, _ := strings.Cut(execution.SessionKey, "\x00")
	profile := request.level.profile
	listing := transcriptsListing{what: "sub-agent runs of profile:" + callerSession + "/" + profile}
	var target TranscriptTarget
	own := profile == callerProfile && (request.level.session == "" || request.level.session == callerSession)
	switch {
	case t.profiles != nil:
		var err error
		if target, err = t.profiles.Profile(ctx, execution.SessionKey, request.level.session, profile); err != nil {
			profiles, sessions := t.profiles.Candidates(ctx, execution.SessionKey, request.level.session)
			names, query := profiles, profile
			if profiles == nil {
				names, query = sessions, request.level.session
			}
			return listing, t.notFound(execution, "no_such_profile", err.Error(), query, names)
		}
		own = target.Session == callerSession && target.Profile == callerProfile
		if target.Session != callerSession && !t.settings.AllowOtherSessions {
			return listing, t.fail(execution, tools.StatusForbidden, "transcripts_session_denied", "Another session: tools.native.transcripts.allow_other_sessions is false")
		}
		if !own && target.Profile != callerProfile && !t.settings.AllowOtherProfiles {
			return listing, t.fail(execution, tools.StatusForbidden, "transcripts_profile_denied", "Another profile: tools.native.transcripts.allow_other_profiles is false")
		}
	case own:
		target = TranscriptTarget{Session: callerSession, Profile: callerProfile, Runs: t.runs}
	default:
		return listing, t.fail(execution, tools.StatusError, "profile_unavailable", "Listing another profile's sub-agent runs is not available here")
	}
	listing.what = "sub-agent runs of profile:" + target.Session + "/" + target.Profile
	prefix := "subagent:"
	if !own {
		prefix += target.Session + "/" + target.Profile + "/"
	}
	if target.SubagentDir == "" && target.Runs == nil {
		listing.notes = append(listing.notes, "this profile has no sub-agent storage")
		return listing, ""
	}
	if !target.PersistRuns {
		listing.notes = append(listing.notes, "persist_runs is off for this profile: a finished run has no manifest, so only runs the running server still holds are listed")
	}
	runs := map[string]transcriptsRun{}
	if target.SubagentDir != "" {
		for _, manifest := range newSubagentStore(target.SubagentDir, nil).load() {
			runs[manifest.ID] = transcriptsRun{id: manifest.ID, owner: manifest.Owner, agent: manifest.Agent, state: manifest.State, dir: manifest.Dir, startedAt: manifest.StartedAt, endedAt: manifest.EndedAt}
		}
	}
	if target.Runs != nil {
		for _, run := range target.Runs.allRuns() {
			view := run.view()
			runs[run.id] = transcriptsRun{id: run.id, owner: run.owner, agent: view.agent, state: view.state, dir: view.dir, startedAt: view.startedAt, endedAt: view.endedAt}
		}
	}
	if len(runs) > int(t.settings.ScanMaxEntries) {
		return listing, t.tooMany(execution, len(runs))
	}
	realStorage := ""
	if target.SubagentDir != "" {
		realStorage, _ = filepath.EvalSymlinks(target.SubagentDir)
	}
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return listing, t.contextFailure(execution, err)
		}
		if request.status != "" && run.state != request.status || !matchesFilters(request.filters, strings.ToLower(run.id), strings.ToLower(run.agent+" "+run.callOf())) {
			continue
		}
		if !t.mayReadRun(run, execution, callerSession) {
			listing.hidden++
			continue
		}
		item, shown := t.runItem(run, prefix, target.SubagentDir, realStorage, execution)
		if !shown {
			listing.hidden++
			continue
		}
		listing.items = append(listing.items, item)
	}
	return listing, ""
}

// mayReadRun applies to a listed run the rules transcript applies to reading it.
func (t *transcriptsTool) mayReadRun(run transcriptsRun, execution tools.ExecutionContext, callerSession string) bool {
	if run.owner == execution.SessionKey {
		return true
	}
	ownerSession, _, _ := strings.Cut(run.owner, "\x00")
	return t.settings.AllowChildren && (ownerSession == callerSession || t.settings.AllowOtherSessions)
}

// runItem formats one run; shown is false when the read path rules deny its transcript.
func (t *transcriptsTool) runItem(run transcriptsRun, prefix, storage, realStorage string, execution tools.ExecutionContext) (transcriptsItem, bool) {
	line := fmt.Sprintf("run %s | %s | agent %s | started %s", run.id, run.state, run.agent, transcriptsTime(run.startedAt))
	if !run.endedAt.IsZero() {
		line += " | ended " + transcriptsTime(run.endedAt)
	}
	if call := run.callOf(); call != "" {
		line += " | call " + call
	}
	item := transcriptsItem{key: run.id, activity: run.startedAt.UnixNano()}
	if !run.endedAt.IsZero() {
		item.activity = run.endedAt.UnixNano()
	}
	path := prefix + run.id
	if run.dir == "" {
		item.line = line + " | no transcript yet | path " + path
		return item, true
	}
	file := filepath.Join(run.dir, "messages.jsonl")
	if realDir, err := filepath.EvalSymlinks(run.dir); storage != "" && (err != nil || !insideDirectory(storage, run.dir) || realStorage == "" || !insideDirectory(realStorage, realDir)) {
		item.line = line + " | unreadable: the transcript folder is outside the sub-agent storage | path " + path
		return item, true
	}
	if t.fileDenied(file, execution) {
		return transcriptsItem{}, false
	}
	info := statTranscriptsFile(file)
	switch {
	case info.unreadable:
		item.line = line + " | unreadable: messages.jsonl is not a regular file inside its folder | path " + path
	case !info.exists:
		item.line = line + " | no transcript yet | path " + path + " | file " + file
	default:
		item.activity = info.modified.UnixNano()
		item.line = fmt.Sprintf("%s | active %s | %d bytes | path %s | file %s", line, transcriptsTime(info.modified), info.size, path, file)
	}
	return item, true
}

// notFound is a not-found error that lists the names nearest to query, at most max_entries.
func (t *transcriptsTool) notFound(execution tools.ExecutionContext, code, summary, query string, names []string) string {
	var information []tools.Information
	if names = nearestTranscriptNames(query, names, int(t.settings.MaxEntries)); len(names) > 0 {
		information = append(information, tools.Information{Key: "available", Value: strings.Join(names, ", ")})
	}
	return t.fail(execution, tools.StatusNotFound, code, summary, information...)
}
