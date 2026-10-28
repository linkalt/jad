package native

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"jad/backend-go/internal/tools"
)

// transcriptRun is what a read has resolved before it picks a mode.
type transcriptRun struct {
	ctx       context.Context
	execution tools.ExecutionContext
	file      *os.File
	turns     []transcriptTurn
	complete  int64
	identity  string
	selector  string
	source    string
	path      string
	label     string
}

// detailBlock is the text of one step in detail mode, for the part asked.
func detailBlock(step *transcriptStep, part string) (string, bool) {
	id := step.id.String()
	switch step.kind {
	case "reasoning":
		if part != "reasoning" {
			return "", false
		}
		if step.reasoning == "" {
			return "== " + id + " reasoning\n(no readable reasoning text)", true
		}
		return "== " + id + " reasoning\n" + step.reasoning, true
	case "user", "assistant", "message":
		if part == "result" || part == "reasoning" {
			return "", false
		}
		return "== " + id + " " + step.kind + "\n" + step.text, true
	case "call":
		if part == "reasoning" {
			return "", false
		}
		head := fmt.Sprintf("== %s call %s [%s] %s", id, step.tool, step.callID, step.status)
		if step.hasResult {
			head += fmt.Sprintf(" %dB", step.resultBytes)
		}
		block := head
		if part != "result" {
			block += "\narguments: " + step.args
		}
		if part != "call" {
			if step.hasResult {
				block += "\nresult:\n" + step.result
			} else {
				block += "\nresult: none in the file"
			}
		}
		return block, true
	case "result":
		if part == "call" || part == "reasoning" {
			return "", false
		}
		return fmt.Sprintf("== %s result [%s] %s %dB without its call\n%s", id, step.callID, step.status, step.resultBytes, step.result), true
	}
	if part == "reasoning" {
		return "", false
	}
	return "== " + id + " " + step.line, true
}

// pageOfText cuts [offset, offset+size) out of text on rune boundaries and returns the page
// and the offset of the next one (len(text) when it was the last).
func pageOfText(text string, offset, size int) (string, int) {
	offset = max(0, min(offset, len(text)))
	for offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset++
	}
	end := min(offset+size, len(text))
	for end > offset && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == offset && end < len(text) {
		_, width := utf8.DecodeRuneInString(text[offset:])
		end = offset + width
	}
	return text[offset:end], end
}

func (t *transcriptTool) detailPageBytes() int {
	return int(min(t.settings.MaxDetailBytes, t.settings.ResponseMaxBytes))
}

// detail answers a call with step: the full text of the steps from..to, one stream, paged by
// byte offset.
func (t *transcriptTool) detail(c transcriptRun, from, to transcriptStepID, part string, offset int64) string {
	first := sort.Search(len(c.turns), func(i int) bool { return c.turns[i].number >= from.turn })
	last := sort.Search(len(c.turns), func(i int) bool { return c.turns[i].number > to.turn }) - 1
	if first >= len(c.turns) || c.turns[first].number != from.turn || first > last {
		return t.fail(c.execution, tools.StatusNotFound, "no_such_step", fmt.Sprintf("The transcript has turns %d to %d; step is turn.step", c.turns[0].number, c.turns[len(c.turns)-1].number))
	}
	startOff := c.turns[first].off
	endOff := c.complete
	if last+1 < len(c.turns) {
		endOff = c.turns[last+1].off
	}
	walk := newTranscriptWalk(c.turns, first, "", int(t.settings.PreviewBytes), int(t.settings.ValueBytes))
	walk.keepFrom, walk.keepTo = &from, &to
	if err := scanTranscript(c.ctx, c.file, startOff, endOff, t.settings.LineMaxBytes, func(line transcriptLine) (bool, error) {
		walk.visit(line)
		return true, nil
	}); err != nil {
		return t.scanFailure(c.execution, err)
	}
	var blocks []string
	for _, step := range walk.steps {
		if step.id.less(from) || to.less(step.id) {
			continue
		}
		if block, ok := detailBlock(step, part); ok {
			blocks = append(blocks, block)
		}
	}
	label := c.label
	spec := from.String()
	if from != to {
		spec += "-" + to.String()
	}
	if len(blocks) == 0 {
		return t.fail(c.execution, tools.StatusNotFound, "no_such_step", fmt.Sprintf("%sNo step %s has a %s part in this transcript; the list shows the steps", label, spec, part))
	}
	text := strings.Join(blocks, "\n")
	page, next := pageOfText(text, int(offset), t.detailPageBytes())
	header := fmt.Sprintf("%sstep %s, part %s; bytes %d-%d of %d", label, spec, part, int(min(offset, int64(len(text)))), next, len(text))
	nextCursor := ""
	if next < len(text) {
		nextCursor = encodeTranscriptCursor(transcriptCursor{Selector: c.selector, Source: c.source, Identity: c.identity, From: from.String(), To: to.String(), Part: part, Bytes: int64(next)})
	}
	return t.render(c.execution, header, c.path, []string{page}, nextCursor, nextCursor != "")
}
