package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/webui"
)

// forkUsageFixture is the layout session "demo" (profile dev/coder) whose provider answers every
// request with a response of 5 input and 1 output tokens, so n responses are 5n and n.
type forkUsageFixture struct {
	*layoutFixture
	t *testing.T
}

func newForkUsageFixture(t *testing.T) forkUsageFixture {
	t.Helper()
	f := newLayoutFixture(t)
	f.usage = `"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6},`
	return forkUsageFixture{layoutFixture: f, t: t}
}

func (f forkUsageFixture) route(reference, profile, action string) string {
	return "/api/sessions/" + reference + action + "?profile=" + url.QueryEscape(profile)
}

func (f forkUsageFixture) run(reference, profile, text string) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"input": text})
	if status, out := f.do(f.t, http.MethodPost, f.route(reference, profile, "/run"), string(body)); status != http.StatusOK || !strings.Contains(out, "event: completed") {
		f.t.Fatalf("run %q on %s: %d %s", text, reference, status, out)
	}
}

func (f forkUsageFixture) runs(reference, profile string, from, to int) {
	f.t.Helper()
	for turn := from; turn <= to; turn++ {
		f.run(reference, profile, fmt.Sprintf("turn %d", turn))
	}
}

func (f forkUsageFixture) post(reference, profile, action, body string) (int, string) {
	f.t.Helper()
	return f.do(f.t, http.MethodPost, f.route(reference, profile, action), body)
}

// snapshot reads the session snapshot: the usage block and the ids of the user messages.
func (f forkUsageFixture) snapshot(reference, profile string) (webui.UsageSummary, []string, int) {
	f.t.Helper()
	status, body := f.do(f.t, http.MethodGet, f.route(reference, profile, ""), "")
	if status != http.StatusOK {
		return webui.UsageSummary{}, nil, status
	}
	var snapshot struct {
		Usage    *webui.UsageSummary `json:"usage"`
		Messages []struct {
			ID     string `json:"id"`
			Sender string `json:"sender"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		f.t.Fatalf("snapshot of %s: %v\n%s", reference, err, body)
	}
	var usage webui.UsageSummary
	if snapshot.Usage != nil {
		usage = *snapshot.Usage
	}
	var ids []string
	for _, message := range snapshot.Messages {
		if message.Sender == "user" {
			ids = append(ids, message.ID)
		}
	}
	return usage, ids, status
}

func (f forkUsageFixture) usageOf(reference, profile string) webui.UsageSummary {
	f.t.Helper()
	usage, _, status := f.snapshot(reference, profile)
	if status != http.StatusOK {
		f.t.Fatalf("snapshot of %s: %d", reference, status)
	}
	return usage
}

func (f forkUsageFixture) userIDs(reference, profile string) []string {
	f.t.Helper()
	_, ids, status := f.snapshot(reference, profile)
	if status != http.StatusOK {
		f.t.Fatalf("snapshot of %s: %d", reference, status)
	}
	return ids
}

// stateDir is the profile state folder of a session of this fixture.
func (f forkUsageFixture) stateDir(session, profile string) string {
	return filepath.Join(f.cwd, ".jad", "state", session, filepath.FromSlash(profile))
}

func (f forkUsageFixture) baselinePath(session, profile string) string {
	return filepath.Join(f.stateDir(session, profile), "staging", "usage-cache.json.baseline")
}

func (f forkUsageFixture) cachePath(session, profile string) string {
	return filepath.Join(f.stateDir(session, profile), "staging", "usage-cache.json")
}

func (f forkUsageFixture) baseline(session, profile string) []usageBaselineEntry {
	f.t.Helper()
	return f.baselineAt(f.stateDir(session, profile))
}

func (f forkUsageFixture) baselineAt(state string) []usageBaselineEntry {
	f.t.Helper()
	file, err := readUsageBaselineFile(filepath.Join(state, "staging", "usage-cache.json.baseline"))
	if err != nil {
		f.t.Fatal(err)
	}
	return file.Entries
}

func (f forkUsageFixture) transcript(session, profile string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.stateDir(session, profile), "messages.jsonl"))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// wantResponses fails unless usage counts n responses of 5 input and 1 output tokens, n turns,
// and history that is not partial.
func wantResponses(t *testing.T, label string, usage webui.UsageSummary, n int64) {
	t.Helper()
	input, output := int64(-1), int64(-1)
	if usage.InputTokens != nil {
		input = *usage.InputTokens
	}
	if usage.OutputTokens != nil {
		output = *usage.OutputTokens
	}
	if n == 0 {
		input, output = 0, 0
	}
	if usage.CompletedResponses != n || input != 5*n || output != n || usage.TurnCount != n || usage.HistoryPartial {
		t.Fatalf("%s: usage = %+v (input %d, output %d), want %d responses of 5 input tokens, %d turns, not partial", label, usage, input, output, n, n)
	}
}

// forkTarget is one way to make a fork: into a new session, or into a new profile of the source.
type forkTarget struct {
	name string
	// make forks demo/dev/coder and returns the fork's reference, profile and state folder.
	make func(f forkUsageFixture, name, body string) (status int, reference, profile, state string)
}

var forkTargets = []forkTarget{
	{"new session", func(f forkUsageFixture, name, body string) (int, string, string, string) {
		status, out := f.post("local:demo", "dev/coder", "/fork", `{"session":"local:`+name+`"`+body+`}`)
		_ = out
		return status, "local:" + name, "dev/coder", f.stateDir(name, "dev/coder")
	}},
	{"new profile", func(f forkUsageFixture, name, body string) (int, string, string, string) {
		status, out := f.post("local:demo", "dev/coder", "/fork", `{"profile":"`+name+`"`+body+`}`)
		_ = out
		return status, "local:demo", "dev/" + name, f.stateDir("demo", "dev/"+name)
	}},
}

func (f forkUsageFixture) fork(target forkTarget, name, body string) (reference, profile, state string) {
	f.t.Helper()
	status, reference, profile, state := target.make(f, name, body)
	if status != http.StatusCreated {
		f.t.Fatalf("%s fork %s %s: %d", target.name, name, body, status)
	}
	return reference, profile, state
}

// A fork of a session whose history a compaction removed carries the removed history's spend:
// its totals are the source's, the next message adds to them, and the baseline file is the only
// carrier (the fork's cache is deleted before it is read). Cutting the fork earlier counts the
// turns it keeps on top of the removed history; a fork that copies nothing has no spend.
func TestForkOfACompactedSessionInheritsTheRemovedHistoryUsage(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newForkUsageFixture(t)
			f.runs("local:demo", "dev/coder", 1, 3)
			if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
				t.Fatalf("compact: %d %s", status, out)
			}
			f.runs("local:demo", "dev/coder", 4, 6)
			sourceBefore := f.transcript("demo", "dev/coder")
			entries := f.baseline("demo", "dev/coder")
			if len(entries) != 1 || entries[0].Summary.CompletedResponses != 3 {
				t.Fatalf("source baseline = %+v, want one entry with the 3 compacted responses", entries)
			}
			wantResponses(t, "source", f.usageOf("local:demo", "dev/coder"), 6)
			ids := f.userIDs("local:demo", "dev/coder")
			if len(ids) != 3 {
				t.Fatalf("source user messages after the compaction = %v", ids)
			}

			// The whole transcript.
			reference, profile, state := f.fork(target, "whole", "")
			wantResponses(t, "whole fork", f.usageOf(reference, profile), 6)
			forkEntries := f.baselineAt(state)
			if len(forkEntries) != 1 || forkEntries[0].Offset != entries[0].Offset || forkEntries[0].SHA256 != entries[0].SHA256 ||
				forkEntries[0].Summary.CompletedResponses != 3 || forkEntries[0].LastResponseKey != entries[0].LastResponseKey {
				t.Fatalf("fork baseline = %+v, want the source's %+v", forkEntries, entries)
			}
			// Only the baseline carries the removed spend: the fork's cache is gone.
			if err := os.Remove(filepath.Join(state, "staging", "usage-cache.json")); err != nil {
				t.Fatalf("the fork's first read left no usage cache: %v", err)
			}
			wantResponses(t, "whole fork, no cache", f.usageOf(reference, profile), 6)

			// The next message adds to the carried totals and numbers its turn after them.
			f.run(reference, profile, "turn 7")
			wantResponses(t, "whole fork after the next message", f.usageOf(reference, profile), 7)
			if !strings.Contains(f.transcriptOf(target, "whole"), `"session_turn":7,`) {
				t.Fatalf("the fork's next response is not turn 7:\n%s", f.transcriptOf(target, "whole"))
			}
			if err := os.Remove(filepath.Join(state, "staging", "usage-cache.json")); err != nil {
				t.Fatal(err)
			}
			wantResponses(t, "whole fork after the next message, no cache", f.usageOf(reference, profile), 7)

			// Through turn 5, and before turn 5.
			reference, profile, _ = f.fork(target, "through5", `,"message_id":"`+ids[1]+`"`)
			wantResponses(t, "fork through turn 5", f.usageOf(reference, profile), 5)
			reference, profile, _ = f.fork(target, "before5", `,"message_id":"`+ids[1]+`","keep_turn":false`)
			wantResponses(t, "fork before turn 5", f.usageOf(reference, profile), 4)

			// Before turn 4: nothing is copied, so no history and no spend come along.
			reference, profile, state = f.fork(target, "before4", `,"message_id":"`+ids[0]+`","keep_turn":false`)
			if usage := f.usageOf(reference, profile); usage.CompletedResponses != 0 || usage.HistoryPartial {
				t.Fatalf("fork before the first kept turn: usage = %+v, want none", usage)
			}
			if _, err := os.Stat(filepath.Join(state, "staging", "usage-cache.json.baseline")); !os.IsNotExist(err) {
				t.Fatalf("an empty fork got a baseline: %v", err)
			}

			// The source did not change, and its spend is still its own.
			if f.transcript("demo", "dev/coder") != sourceBefore {
				t.Fatal("the source transcript changed")
			}
			wantResponses(t, "source after the forks", f.usageOf("local:demo", "dev/coder"), 6)
		})
	}
}

func (f forkUsageFixture) transcriptOf(target forkTarget, name string) string {
	if target.name == "new profile" {
		return f.transcript("demo", "dev/"+name)
	}
	return f.transcript(name, "dev/coder")
}

// A front compaction keeps a prefix of the transcript and removes what follows. Its entry covers
// that prefix, so a fork that keeps all of it carries the entry; a fork that cuts inside it
// cannot tell the covered prefix from the removed tail, which came after the cut, and counts only
// the records it copied.
func TestForkOfAFrontCompactedSessionCarriesTheEntryOnlyWhenItKeepsTheCoveredPrefix(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newForkUsageFixture(t)
			f.runs("local:demo", "dev/coder", 1, 4)
			ids := f.userIDs("local:demo", "dev/coder")
			if status, out := f.post("local:demo", "dev/coder", "/front-compact", `{"message_id":"`+ids[2]+`"}`); status != http.StatusOK {
				t.Fatalf("front-compact: %d %s", status, out)
			}
			f.run("local:demo", "dev/coder", "turn 5")
			entries := f.baseline("demo", "dev/coder")
			if len(entries) != 1 || entries[0].Offset <= 0 || entries[0].Summary.CompletedResponses != 4 {
				t.Fatalf("source baseline = %+v, want one entry over a kept prefix with 4 responses", entries)
			}
			wantResponses(t, "source", f.usageOf("local:demo", "dev/coder"), 5)
			ids = f.userIDs("local:demo", "dev/coder")
			if len(ids) != 3 {
				t.Fatalf("source user messages = %v, want turns 1, 2 and 5", ids)
			}

			// All of it: the prefix, the removed tail's spend and turn 5.
			reference, profile, _ := f.fork(target, "whole", "")
			wantResponses(t, "whole fork", f.usageOf(reference, profile), 5)
			f.run(reference, profile, "turn 6")
			wantResponses(t, "whole fork after the next message", f.usageOf(reference, profile), 6)

			// Exactly the covered prefix (turns 1 and 2): the entry's own summary.
			reference, profile, _ = f.fork(target, "prefix", `,"message_id":"`+ids[1]+`"`)
			wantResponses(t, "fork of exactly the covered prefix", f.usageOf(reference, profile), 4)

			// Inside the covered prefix: the removed turns 3 and 4 came after this cut, and are not
			// carried; the fork counts turn 1 only. Nothing was removed from before the kept bytes
			// (the entry says so), so that is exact, and the fork's baseline file records it: an
			// exact marker, which keeps the history from being called partial once the cache is
			// gone.
			reference, profile, state := f.fork(target, "inside", `,"message_id":"`+ids[1]+`","keep_turn":false`)
			wantResponses(t, "fork inside the covered prefix", f.usageOf(reference, profile), 1)
			forkEntries := f.baselineAt(state)
			if len(forkEntries) != 1 || forkEntries[0].Offset != 0 || !forkEntries[0].PrefixExact || forkEntries[0].Summary.HistoryPartial {
				t.Fatalf("fork inside the covered prefix: baseline %+v, want one exact marker", forkEntries)
			}
			if err := os.Remove(filepath.Join(state, "staging", "usage-cache.json")); err != nil {
				t.Fatal(err)
			}
			wantResponses(t, "fork inside the covered prefix, no cache", f.usageOf(reference, profile), 1)
			f.run(reference, profile, "turn 3")
			wantResponses(t, "fork inside the covered prefix after the next message", f.usageOf(reference, profile), 2)
		})
	}
}

func staleBaselineFile(t *testing.T) []byte {
	t.Helper()
	count := func(n int64) *int64 { return &n }
	empty := sha256.Sum256(nil)
	summary := webui.UsageSummary{CompletedResponses: 99, ResponsesWithUsage: 99, TurnCount: 99}
	summary.InputTokens, summary.OutputTokens, summary.TotalTokens = count(495), count(99), count(594)
	data, err := json.Marshal(usageBaselineFile{SchemaVersion: usageBaselineSchemaVersion, Entries: []usageBaselineEntry{{
		SHA256: hex.EncodeToString(empty[:]), ReplacesSize: 1, ReplacesSHA256: "not a transcript", Summary: summary,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// plantStaleUsageState leaves what an earlier session of that name could have left in the
// target's state folder: a baseline whose offset-0 entry fits every file, and a cache.
func (f forkUsageFixture) plantStaleUsageState(state string) {
	f.t.Helper()
	staging := filepath.Join(state, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "usage-cache.json.baseline"), staleBaselineFile(f.t), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "usage-cache.json"), []byte("{stale"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f forkUsageFixture) addPlainSession(name string) {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "sessions", "demo.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "sessions", name+".json"), data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// state none starts empty, and neither it nor a copy picks up a baseline or cache that an earlier
// session of the target's name left behind.
func TestForkIgnoresAndReplacesAStaleTargetBaseline(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newForkUsageFixture(t)
			f.runs("local:demo", "dev/coder", 1, 3)
			if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
				t.Fatalf("compact: %d %s", status, out)
			}
			f.run("local:demo", "dev/coder", "turn 4")
			stale := func(name string) {
				if target.name == "new profile" {
					f.plantStaleUsageState(f.stateDir("demo", "dev/"+name))
				} else {
					f.plantStaleUsageState(f.stateDir(name, "dev/coder"))
				}
			}

			stale("none")
			reference, profile, state := f.fork(target, "none", `,"state":"none"`)
			if usage := f.usageOf(reference, profile); usage.CompletedResponses != 0 || usage.TurnCount != 0 {
				t.Fatalf("state none took the stale baseline: %+v", usage)
			}
			for _, name := range []string{"usage-cache.json", "usage-cache.json.baseline"} {
				if _, err := os.Stat(filepath.Join(state, "staging", name)); err == nil && name == "usage-cache.json.baseline" {
					t.Fatalf("state none left a baseline in the new session")
				}
			}

			stale("copy")
			reference, profile, state = f.fork(target, "copy", "")
			wantResponses(t, "copy over a stale baseline", f.usageOf(reference, profile), 4)
			entries := f.baselineAt(state)
			if len(entries) != 1 || entries[0].Summary.CompletedResponses != 3 {
				t.Fatalf("the copy's baseline = %+v, want the source's single entry", entries)
			}
		})
	}
}

// A source that never had a baseline forks as before: the copied records are counted and nothing
// is written. A source compacted before baselines existed (cache lost, no baseline) forks to the
// same partial totals it shows itself, and the fork's baseline file keeps the flag.
func TestForkOfASourceWithoutABaselineIsUnchanged(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newForkUsageFixture(t)
			f.runs("local:demo", "dev/coder", 1, 3)
			ids := f.userIDs("local:demo", "dev/coder")
			reference, profile, state := f.fork(target, "plain", `,"message_id":"`+ids[1]+`"`)
			wantResponses(t, "fork of an uncompacted source", f.usageOf(reference, profile), 2)
			if _, err := os.Stat(filepath.Join(state, "staging", "usage-cache.json.baseline")); !os.IsNotExist(err) {
				t.Fatalf("a fork of a source without a baseline got one: %v", err)
			}

			// Compacted by a build without baselines: the baseline and the cache are gone.
			if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
				t.Fatalf("compact: %d %s", status, out)
			}
			f.run("local:demo", "dev/coder", "turn 4")
			for _, path := range []string{f.baselinePath("demo", "dev/coder"), f.cachePath("demo", "dev/coder")} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			source := f.usageOf("local:demo", "dev/coder")
			if !source.HistoryPartial || source.CompletedResponses != 1 {
				t.Fatalf("source without a baseline = %+v, want the kept record only, flagged partial", source)
			}
			// The source's own read wrote its cache; the baseline is still missing.
			reference, profile, state = f.fork(target, "old", "")
			usage := f.usageOf(reference, profile)
			if !usage.HistoryPartial || usage.CompletedResponses != source.CompletedResponses {
				t.Fatalf("fork of a source without a baseline = %+v, want the source's %+v", usage, source)
			}
			// The fork says in its own baseline file that it is partial, so the flag does not
			// depend on its cache or on the history block that came with it.
			forkEntries := f.baselineAt(state)
			if len(forkEntries) != 1 || forkEntries[0].Offset != 0 || !forkEntries[0].Summary.HistoryPartial {
				t.Fatalf("fork of a partial source: baseline %+v, want one partial marker", forkEntries)
			}
			for _, path := range []string{filepath.Join(state, "staging", "usage-cache.json"), filepath.Join(state, "messages.backup.jsonl")} {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			usage = f.usageOf(reference, profile)
			if !usage.HistoryPartial || usage.CompletedResponses != source.CompletedResponses {
				t.Fatalf("fork of a partial source without its cache or history block = %+v, want partial with the source's %d responses", usage, source.CompletedResponses)
			}
			f.run(reference, profile, "turn 5")
			if usage := f.usageOf(reference, profile); !usage.HistoryPartial || usage.CompletedResponses != source.CompletedResponses+1 {
				t.Fatalf("fork of a partial source after the next message = %+v", usage)
			}
		})
	}
}

// A fork that fails after its transcript was written removes what it wrote, so the next fork to
// that name is not a 409 and no baseline is left without its transcript. A stale baseline that
// cannot be removed stops the fork before it writes anything.
func TestFailedForkRemovesWhatItWrote(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newForkUsageFixture(t)
			f.runs("local:demo", "dev/coder", 1, 3)
			if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
				t.Fatalf("compact: %d %s", status, out)
			}
			f.run("local:demo", "dev/coder", "turn 4")

			var state string
			switch target.name {
			case "new profile":
				state = f.stateDir("demo", "dev/blocked")
			default:
				state = f.stateDir("blocked", "dev/coder")
			}
			// The history block cannot be written: its path is a directory with something in it.
			block := filepath.Join(state, "messages.backup.jsonl")
			if err := os.MkdirAll(filepath.Join(block, "keep"), 0o755); err != nil {
				t.Fatal(err)
			}
			if status, _, _, _ := target.make(f, "blocked", ""); status == http.StatusCreated {
				t.Fatal("a fork whose history block cannot be written succeeded")
			}
			for _, path := range []string{filepath.Join(state, "messages.jsonl"), filepath.Join(state, "staging", "usage-cache.json.baseline")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("the failed fork left %s: %v", path, err)
				}
			}
			// The session file (new session) or the profile file (new profile) goes again too.
			if target.name == "new session" {
				if _, err := os.Stat(filepath.Join(f.root, "sessions", "blocked.json")); !os.IsNotExist(err) {
					t.Fatalf("the failed fork left its session file: %v", err)
				}
			} else if status, _ := f.do(t, http.MethodGet, "/api/catalog/profiles/dev/blocked", ""); status != http.StatusNotFound {
				t.Fatalf("the failed fork left its profile file: %d", status)
			}
			if err := os.RemoveAll(block); err != nil {
				t.Fatal(err)
			}
			reference, profile, _ := f.fork(target, "blocked", "")
			wantResponses(t, "retried fork", f.usageOf(reference, profile), 4)

			// A stale baseline that cannot be removed.
			if target.name == "new profile" {
				state = f.stateDir("demo", "dev/stuck")
			} else {
				state = f.stateDir("stuck", "dev/coder")
			}
			stuck := filepath.Join(state, "staging", "usage-cache.json.baseline")
			if err := os.MkdirAll(filepath.Join(stuck, "keep"), 0o755); err != nil {
				t.Fatal(err)
			}
			if status, _, _, _ := target.make(f, "stuck", ""); status == http.StatusCreated {
				t.Fatal("a fork whose target baseline cannot be removed succeeded")
			}
			if _, err := os.Stat(filepath.Join(state, "messages.jsonl")); !os.IsNotExist(err) {
				t.Fatalf("the refused fork left a transcript: %v", err)
			}
		})
	}
}

// Forks of a session read by concurrent readers: every read of the source is its totals, and a
// read of the fork is either nothing yet or the whole of it, never the removed history alone.
func TestForkWithConcurrentReaders(t *testing.T) {
	f := newForkUsageFixture(t)
	f.runs("local:demo", "dev/coder", 1, 3)
	if status, out := f.post("local:demo", "dev/coder", "/compact", ""); status != http.StatusOK {
		t.Fatalf("compact: %d %s", status, out)
	}
	f.runs("local:demo", "dev/coder", 4, 5)

	const forks = 6
	stop := make(chan struct{})
	var readers sync.WaitGroup
	problems := make(chan string, 64)
	report := func(format string, args ...any) {
		select {
		case problems <- fmt.Sprintf(format, args...):
		default:
		}
	}
	for reader := 0; reader < 4; reader++ {
		readers.Add(1)
		go func(reader int) {
			defer readers.Done()
			for round := 0; ; round++ {
				select {
				case <-stop:
					return
				default:
				}
				if usage, _, status := f.snapshot("local:demo", "dev/coder"); status != http.StatusOK || usage.CompletedResponses != 5 {
					report("source read: %d %+v", status, usage)
				}
				name := fmt.Sprintf("race%d", (reader+round)%forks)
				if usage, _, status := f.snapshot("local:"+name, "dev/coder"); status == http.StatusOK && usage.CompletedResponses != 0 && usage.CompletedResponses != 5 {
					report("fork %s read: %+v", name, usage)
				}
			}
		}(reader)
	}
	for index := 0; index < forks; index++ {
		if status, out := f.post("local:demo", "dev/coder", "/fork", fmt.Sprintf(`{"session":"local:race%d"}`, index)); status != http.StatusCreated {
			t.Errorf("fork %d: %d %s", index, status, out)
		}
	}
	close(stop)
	readers.Wait()
	close(problems)
	for problem := range problems {
		t.Error(problem)
	}
	for index := 0; index < forks; index++ {
		wantResponses(t, fmt.Sprintf("fork race%d", index), f.usageOf(fmt.Sprintf("local:race%d", index), "dev/coder"), 5)
	}
}

// The fork writes each source line and a newline, so a blank line in the covered prefix is not
// copied and the fork's bytes are no longer the ones the entry's hash covers. The fork proves the
// source bytes its entry covers (blank lines are the only bytes it can reconstruct), and binds the
// entry to its own bytes: the totals are the source's, the fork's offset and hash are the fork's.
func TestForkWhoseCopyDiffersFromTheCoveredBytesCarriesTheEntryBoundToItsBytes(t *testing.T) {
	for _, target := range forkTargets {
		t.Run(target.name, func(t *testing.T) {
			f := newForkUsageFixture(t)
			f.runs("local:demo", "dev/coder", 1, 2)
			appendToTranscript(t, filepath.Join(f.stateDir("demo", "dev/coder"), "messages.jsonl"), "\n")
			f.runs("local:demo", "dev/coder", 3, 4)
			ids := f.userIDs("local:demo", "dev/coder")
			if status, out := f.post("local:demo", "dev/coder", "/front-compact", `{"message_id":"`+ids[2]+`"}`); status != http.StatusOK {
				t.Fatalf("front-compact: %d %s", status, out)
			}
			f.run("local:demo", "dev/coder", "turn 5")
			wantResponses(t, "source", f.usageOf("local:demo", "dev/coder"), 5)
			entries := f.baseline("demo", "dev/coder")
			if len(entries) != 1 || entries[0].Offset <= 0 {
				t.Fatalf("source baseline = %+v", entries)
			}

			reference, profile, state := f.fork(target, "blank", "")
			wantResponses(t, "fork", f.usageOf(reference, profile), 5)
			forkEntries := f.baselineAt(state)
			if len(forkEntries) != 1 || forkEntries[0].Summary.CompletedResponses != 4 || forkEntries[0].Offset != entries[0].Offset-1 {
				t.Fatalf("fork baseline = %+v, want the 4-response entry at the source's offset %d less the dropped blank line", forkEntries, entries[0].Offset)
			}
			// The entry fits the fork's bytes, so the cache can go and the next message adds on.
			if err := os.Remove(filepath.Join(state, "staging", "usage-cache.json")); err != nil {
				t.Fatal(err)
			}
			wantResponses(t, "fork, no cache", f.usageOf(reference, profile), 5)
			f.run(reference, profile, "turn 6")
			wantResponses(t, "fork after the next message", f.usageOf(reference, profile), 6)
		})
	}
}

func appendToTranscript(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}
