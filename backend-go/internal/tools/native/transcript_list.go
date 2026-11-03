package native

import (
	"context"
	"os"
	"sort"
)

// transcriptListWindow is the first stretch of a transcript, counted back from the end of the
// listed range, that a list without search reads; it doubles while the page is not decided. Only
// tests change it.
var transcriptListWindow int64 = 1 << 20

// transcriptListBackward is off only in the tests that compare the backward read with a read of
// the whole range.
var transcriptListBackward = true

// transcriptList is one list call: the turns first..last (indexes into turns), the lines of
// [turns[first].off, endOff), the filters and the step a cursor resumes at.
type transcriptList struct {
	tool        *transcriptTool
	ctx         context.Context
	file        *os.File
	turns       []transcriptTurn
	first, last int
	endOff      int64
	search      string
	role        string
	toolFilter  *compiledGlob
	context     int
	resume      transcriptStepID
	hasResume   bool
}

// steps returns the selected steps a page is cut from, oldest first, ending with the newest
// one the page may show, and the index of the newest turn of the range. A search reads the whole
// range, since a match can be anywhere. Without one the page is the newest selected steps, so the
// read starts near the end and goes back only while it is not decided (see backward).
func (l transcriptList) steps() ([]*transcriptStep, int, error) {
	if l.search != "" || !transcriptListBackward {
		selected, _, lastTurn, err := l.scan(l.first, l.endOff)
		return selected, lastTurn, err
	}
	return l.backward()
}

// scan walks [turns[lo].off, end) and selects its steps, up to the cursor's step. It also returns
// every step the walk made, listed or not.
func (l transcriptList) scan(lo int, end int64) ([]*transcriptStep, []*transcriptStep, int, error) {
	t := l.tool
	walk := newTranscriptWalk(l.turns, lo, l.search, int(t.settings.PreviewBytes), int(t.settings.ValueBytes))
	err := scanTranscript(l.ctx, l.file, l.turns[lo].off, end, t.settings.LineMaxBytes, func(line transcriptLine) (bool, error) {
		walk.visit(line)
		return true, nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	selected := selectTranscriptSteps(walk.steps, l.role, l.toolFilter, l.search != "", l.context)
	if l.hasResume {
		selected = selected[:sort.Search(len(selected), func(i int) bool { return l.resume.less(selected[i].id) })]
	}
	return selected, walk.steps, walk.turnIndex, nil
}

// backward reads whole turns from the end of the range (or from the cursor's turn) and widens the
// read until the page is decided, so a page costs about what it shows instead of the whole range.
// A page is the newest max_list_entries+1 selected steps (the extra one is where the next cursor
// points). The read is the one the whole range gives when it reaches the first turn, or when it
// holds a full page none of whose steps the rest of the range can change. The walk pairs a result
// with its call across turns, so two kinds of step can differ: a result without its call (its
// call may be in an older turn, where it would add no step) in a turn the page shows, listed or
// not, since it moves the numbers of the steps after it, widens the read back; a call on the page
// without a result (its result may be in a newer turn past the cursor's) widens it to the end of
// the range.
func (l transcriptList) backward() ([]*transcriptStep, int, error) {
	need := int(l.tool.settings.MaxListEntries) + 1
	hi, end := l.last, l.endOff
	if l.hasResume {
		for index := l.last; index >= l.first; index-- {
			if l.turns[index].number == l.resume.turn {
				hi = index
				if hi < l.last {
					end = l.turns[hi+1].off
				}
				break
			}
		}
	}
	window := transcriptListWindow
	for {
		lo := sort.Search(len(l.turns), func(i int) bool { return l.turns[i].off > end-window }) - 1
		lo = min(max(lo, l.first), hi)
		selected, walked, _, err := l.scan(lo, end)
		if err != nil {
			return nil, 0, err
		}
		page := selected[max(len(selected)-need, 0):]
		decided := lo == l.first || len(selected) >= need && !transcriptPageHas(walked, func(step *transcriptStep) bool {
			return step.kind == "result" && step.id.turn >= page[0].id.turn
		})
		switch {
		case !decided:
			window *= 2
		case end < l.endOff && transcriptPageHas(page, func(step *transcriptStep) bool { return step.kind == "call" && !step.hasResult }):
			end = l.endOff
		default:
			return selected, l.last, nil
		}
	}
}

func transcriptPageHas(page []*transcriptStep, test func(*transcriptStep) bool) bool {
	for _, step := range page {
		if test(step) {
			return true
		}
	}
	return false
}
