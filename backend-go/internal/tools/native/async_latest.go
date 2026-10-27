package native

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/tools"
)

// latestInfo is what the manager knows about a live handle when a driver is asked for news.
type latestInfo struct {
	state   asyncmgr.State
	started time.Time
}

// asyncLatester is implemented by the driver of a kind that can say what a live handle did
// since the last time its progress was shown. prev is the cursor the last call returned (nil
// the first time); next replaces it. A driver keeps no state of its own: the cursor lives on
// the handle in the manager.
type asyncLatester interface {
	latest(id string, info latestInfo, prev any, cfg asyncmgr.Config, now time.Time) (text string, next any)
}

// latestBlock is the news of one live handle in an async response.
type latestBlock struct {
	ID   string        `json:"id"`
	Kind asyncmgr.Kind `json:"kind"`
	Text string        `json:"text"`
}

// latestBlocks returns one block per live handle of ids (owner's), newest news only, within
// latest_total_bytes in all. A handle whose block no longer fits is left for the next call:
// its cursor does not move. A block that fits only in part is cut and its cursor moves.
func (h *AsyncHost) latestBlocks(owner string, ids []string) []latestBlock {
	cfg := h.Config()
	budget := int(cfg.LatestTotalBytes)
	now := time.Now()
	var blocks []latestBlock
	for _, id := range ids {
		if budget <= 0 {
			break
		}
		kind, state, started, live := h.manager.LiveInfo(owner, id)
		if !live {
			continue
		}
		driver, ok := h.manager.Driver(kind).(asyncLatester)
		if !ok {
			continue
		}
		text, next := driver.latest(id, latestInfo{state: state, started: started}, h.manager.Progress(id), cfg, now)
		if text == "" {
			continue
		}
		if len(text) > budget {
			text, _ = cutUTF8(text, int64(budget))
		}
		h.manager.SetProgress(id, next)
		budget -= len(text)
		blocks = append(blocks, latestBlock{ID: id, Kind: kind, Text: text})
	}
	return blocks
}

// latestText is the block of one handle, "" when it is not live or has nothing to say.
func (h *AsyncHost) latestText(owner, id string) string {
	if blocks := h.latestBlocks(owner, []string{id}); len(blocks) > 0 {
		return blocks[0].Text
	}
	return ""
}

// liveHandleIDs are the live handles of owner inside ids (all of them when ids is empty),
// at most list_max_limit.
func (h *AsyncHost) liveHandleIDs(owner string, ids []string) []string {
	if len(ids) == 0 {
		list, err := h.manager.List(asyncmgr.ListQuery{Owner: owner, State: "active", Limit: h.Config().ListMaxLimit})
		if err != nil {
			return nil
		}
		for _, item := range list.Items {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func latestAge(d time.Duration) string {
	return max(d, 0).Round(time.Second).String()
}

// outputProgress is the cursor of an output handle: how many bytes of each stream were shown.
type outputProgress struct {
	stdout, stderr int64
	shownAt        time.Time
}

// outputLatest is the news of a running child: the new stdout and stderr since the last
// block, each cut at the front to latest_bytes with a marker for what was left out.
func outputLatest(collector *bashOutputCollector, info latestInfo, prev any, cfg asyncmgr.Config, now time.Time) (string, any) {
	if collector == nil {
		return fmt.Sprintf("%s %s, no output yet", info.state, latestAge(now.Sub(info.started))), prev
	}
	var cursor outputProgress
	if shown, ok := prev.(*outputProgress); ok {
		cursor = *shown
	} else {
		cursor.shownAt = info.started
	}
	stdout, stdoutProduced, stdoutLost := collector.stdout.since(cursor.stdout)
	stderr, stderrProduced, stderrLost := collector.stderr.since(cursor.stderr)
	head := fmt.Sprintf("%s %s", info.state, latestAge(now.Sub(info.started)))
	if last := collector.lastOutputAt(); last.IsZero() {
		head += ", no output yet"
	} else {
		head += ", last output " + latestAge(now.Sub(last)) + " ago"
	}
	var sections []string
	for _, stream := range []struct {
		name string
		text string
		lost int64
	}{{"stdout", stdout, stdoutLost}, {"stderr", stderr, stderrLost}} {
		text := sanitizeBashOutput(stream.text, true)
		if text == "" {
			continue
		}
		shown, cut := tailUTF8(text, int(cfg.LatestBytes))
		section := "[" + stream.name + "]\n"
		if earlier := int64(len(text)-len(shown)) + stream.lost; cut || stream.lost > 0 {
			section += fmt.Sprintf("…+%d bytes earlier\n", earlier)
		}
		sections = append(sections, section+shown)
	}
	next := &outputProgress{stdout: stdoutProduced, stderr: stderrProduced, shownAt: now}
	if len(sections) == 0 {
		return head + "\nno new output since " + cursor.shownAt.UTC().Format(time.RFC3339), next
	}
	return head + "\n" + strings.Join(sections, "\n"), next
}

// outputWatch hands the output collector of a child that starts later to the handle that
// asks for its news.
type outputWatch struct{ collector chan *bashOutputCollector }

type outputWatchKey struct{}

func newOutputWatch() *outputWatch {
	return &outputWatch{collector: make(chan *bashOutputCollector, 1)}
}

func (w *outputWatch) get() *bashOutputCollector {
	select {
	case c := <-w.collector:
		w.collector <- c
		return c
	default:
		return nil
	}
}

func (w *outputWatch) set(c *bashOutputCollector) {
	select {
	case w.collector <- c:
	default:
	}
}

func withOutputWatch(ctx context.Context, w *outputWatch) context.Context {
	return context.WithValue(ctx, outputWatchKey{}, w)
}

func outputWatchFrom(ctx context.Context) *outputWatch {
	w, _ := ctx.Value(outputWatchKey{}).(*outputWatch)
	return w
}

// activityProgress is the cursor of an agent handle: the transcript offset up to which its
// steps were shown.
type activityProgress struct {
	path    string
	off     int64
	shownAt time.Time
}

// seedActivity starts the cursor of a handle at the end of its transcript as it is now, so
// the first block shows only what the run did.
func (h *AsyncHost) seedActivity(id, path string) {
	if path == "" {
		return
	}
	cursor := &activityProgress{path: path, shownAt: time.Now()}
	if info, err := os.Stat(path); err == nil {
		cursor.off = info.Size()
	}
	h.manager.SetProgress(id, cursor)
}

// activityLatest is the news of a running agent: its newest tool calls and replies since the
// last block (never its reasoning), how long ago the last event was, and where the
// transcript is so the transcript tool can open it.
func activityLatest(transcript string, info latestInfo, prev any, cfg asyncmgr.Config, now time.Time) (string, any) {
	if shown, ok := prev.(*activityProgress); ok && transcript == "" {
		transcript = shown.path
	}
	head := fmt.Sprintf("%s %s", info.state, latestAge(now.Sub(info.started)))
	if info.state == asyncmgr.StateQueued {
		return head + ": waiting for its turn", prev
	}
	if transcript == "" {
		return head + "; no transcript yet", prev
	}
	file, err := os.Open(transcript)
	if err != nil {
		return head + "; no transcript yet", prev
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return head + "; transcript unreadable", prev
	}
	if stat.Size() > cfg.LatestScanMaxBytes {
		return head + "\ntranscript too large to summarize (latest_scan_max_bytes); transcript: " + transcript, prev
	}
	var cursor activityProgress
	if shown, ok := prev.(*activityProgress); ok {
		cursor = *shown
	} else {
		cursor.path, cursor.shownAt = transcript, info.started
	}
	ctx := context.Background()
	turns, complete, err := indexTranscriptTurns(ctx, file, stat.Size(), cfg.LatestScanMaxBytes)
	if err != nil || len(turns) == 0 {
		return head + "; no steps yet\ntranscript: " + transcript, prev
	}
	first := max(sort.Search(len(turns), func(i int) bool { return turns[i].off > cursor.off })-1, 0)
	walk := newTranscriptWalk(turns, first, "", int(cfg.LatestBytes), int(cfg.LatestValueBytes))
	if err := scanTranscript(ctx, file, turns[first].off, complete, cfg.LatestScanMaxBytes, func(line transcriptLine) (bool, error) {
		walk.visit(line)
		return true, nil
	}); err != nil {
		return head + "; transcript unreadable", prev
	}
	var news []*transcriptStep
	for _, step := range walk.steps {
		if step.off >= cursor.off && (step.kind == "call" || step.kind == "assistant") {
			news = append(news, step)
		}
	}
	reference := "transcript: " + transcript
	if len(walk.steps) > 0 {
		reference += " (last step " + walk.steps[len(walk.steps)-1].id.String() + ")"
	}
	next := &activityProgress{path: cursor.path, off: complete, shownAt: now}
	lastEvent := "last event " + latestAge(now.Sub(stat.ModTime())) + " ago"
	if len(news) == 0 {
		return head + ", " + lastEvent + "\nno new activity since " + cursor.shownAt.UTC().Format(time.RFC3339) + "\n" + reference, next
	}
	lines := []string{fmt.Sprintf("%s, %s; %d new step(s)", head, lastEvent, len(news))}
	for _, step := range news[max(len(news)-int(cfg.LatestLines), 0):] {
		line, _ := cutUTF8(sanitizeBashOutput(stepLine(step), true), cfg.LatestBytes)
		lines = append(lines, line)
	}
	return strings.Join(append(lines, reference), "\n"), next
}

// transcriptOf is the messages.jsonl inside a run's transcript folder, "" before it exists.
func transcriptOf(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "messages.jsonl")
}

// remoteLatest is all a caller gets of a run on another server: its id and age. Nothing of
// the remote's files or steps is shown.
func remoteLatest(r *remoteRun, info latestInfo, now time.Time) string {
	return fmt.Sprintf("remote %s run %s %s %s; activity of a remote run is not shown", r.what, r.id, info.state, latestAge(now.Sub(info.started)))
}

// latestInformation is the "latest" line a start tool adds to its still-running result.
func (h *AsyncHost) latestInformation(owner, id string) []tools.Information {
	if text := h.latestText(owner, id); text != "" {
		return []tools.Information{{Key: "latest", Value: text}}
	}
	return nil
}
