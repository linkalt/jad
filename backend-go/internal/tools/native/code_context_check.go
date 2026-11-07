package native

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// codeContextStatus is an entry checked against its file as it is now. An entry is
// current while its lines still hold the bytes it was saved with, even when other parts
// of the file changed. When they do not, but those bytes occur exactly once elsewhere in
// the file, the code only moved and the entry is relocated there. Otherwise it is stale
// and reason says why.
type codeContextStatus struct {
	stale  bool
	reason string
	// content is the entry's bytes as read now, at start..end.
	content    []byte
	start, end int64
	// relocated: start..end is where the saved bytes are now; fromStart..fromEnd is the
	// range the entry held before.
	relocated          bool
	fromStart, fromEnd int64
	fileSHA256         string
}

// codeContextFile is one file as a check read it.
type codeContextFile struct {
	data    []byte
	sha256  string
	reason  string
	offsets []int
}

// lineOffsets is the start of every line and, last, the file's length, so line k
// (0-based) is data[offsets[k]:offsets[k+1]]. Lines are counted as codeContextLines
// counts them.
func (f *codeContextFile) lineOffsets() []int {
	if f.offsets != nil {
		return f.offsets
	}
	offsets := []int{0}
	if len(f.data) > 0 {
		for index, char := range f.data {
			if char == '\n' && index+1 < len(f.data) {
				offsets = append(offsets, index+1)
			}
		}
		offsets = append(offsets, len(f.data))
	}
	f.offsets = offsets
	return offsets
}

// codeContextChecker reads each distinct file once for all the entries it checks.
type codeContextChecker struct {
	reader codeContextReader
	files  map[string]*codeContextFile
}

func newCodeContextChecker(reader codeContextReader) *codeContextChecker {
	return &codeContextChecker{reader: reader, files: map[string]*codeContextFile{}}
}

func (c *codeContextChecker) file(path string) *codeContextFile {
	if file, read := c.files[path]; read {
		return file
	}
	data, _, failure, reason := c.reader.readFile(path)
	file := &codeContextFile{data: data, sha256: codeContextSHA256(data)}
	if failure != "" {
		file = &codeContextFile{reason: reason}
	}
	c.files[path] = file
	return file
}

func (c *codeContextChecker) check(entry codeContextEntry) codeContextStatus {
	unchanged := codeContextStatus{start: entry.Start, end: entry.End}
	file := c.file(entry.Path)
	if file.reason != "" {
		unchanged.stale, unchanged.reason = true, file.reason
		return unchanged
	}
	content, _, ok := codeContextLines(file.data, entry.Start, entry.End)
	if ok && (file.sha256 == entry.FileSHA256 || codeContextSHA256(content) == entry.ContentSHA256) {
		unchanged.content, unchanged.fileSHA256 = content, file.sha256
		return unchanged
	}
	if c.reader.settings.Relocate {
		if start, moved, found := relocateCodeContext(file, entry); found {
			return codeContextStatus{
				content: moved, start: start, end: start + entry.End - entry.Start,
				relocated: true, fromStart: entry.Start, fromEnd: entry.End, fileSHA256: file.sha256,
			}
		}
	}
	unchanged.stale, unchanged.reason = true, "the file changed and these lines differ from what was saved"
	if !ok {
		unchanged.reason = "the file changed and no longer has these lines"
	}
	return unchanged
}

func (c *codeContextChecker) checkAll(entries []codeContextEntry) map[int64]codeContextStatus {
	statuses := make(map[int64]codeContextStatus, len(entries))
	for _, entry := range entries {
		statuses[entry.ID] = c.check(entry)
	}
	return statuses
}

// relocateCodeContext finds the entry's saved bytes elsewhere in the file: as many lines,
// the same length, the same first line and the same SHA-256. Only a single occurrence is
// a move; none means the code changed, several that the tool cannot tell which is meant.
// The new range is where identical bytes are, never one computed from the edits.
func relocateCodeContext(file *codeContextFile, entry codeContextEntry) (int64, []byte, bool) {
	if entry.ContentBytes <= 0 || entry.ContentSHA256 == "" {
		return 0, nil, false
	}
	offsets := file.lineOffsets()
	lines := int(entry.End - entry.Start + 1)
	found := -1
	for index := 0; index+lines < len(offsets); index++ {
		from, to := offsets[index], offsets[index+lines]
		if int64(to-from) != entry.ContentBytes {
			continue
		}
		if entry.HeadSHA256 != "" && codeContextSHA256(file.data[from:offsets[index+1]]) != entry.HeadSHA256 {
			continue
		}
		if codeContextSHA256(file.data[from:to]) != entry.ContentSHA256 {
			continue
		}
		if found >= 0 {
			return 0, nil, false
		}
		found = index
	}
	if found < 0 {
		return 0, nil, false
	}
	return int64(found + 1), file.data[offsets[found]:offsets[found+lines]], true
}

// codeContextHead is the first line of a range's bytes, newline included.
func codeContextHead(content []byte) []byte {
	if index := bytes.IndexByte(content, '\n'); index >= 0 {
		return content[:index+1]
	}
	return content
}

// settleCodeContextRelocations keeps a relocation only when no other entry of the task
// reads the range the code moved to: such an entry would be a duplicate, so it is stale.
func settleCodeContextRelocations(document codeContextDocument, statuses map[int64]codeContextStatus) {
	taken := make(map[string]int64, len(document.Entries))
	for _, entry := range document.Entries {
		if status, checked := statuses[entry.ID]; !checked || !status.relocated {
			taken[codeContextRangeKey(entry.Path, entry.Start, entry.End)] = entry.ID
		}
	}
	for _, entry := range document.Entries {
		status, checked := statuses[entry.ID]
		if !checked || !status.relocated {
			continue
		}
		key := codeContextRangeKey(entry.Path, status.start, status.end)
		if other, exists := taken[key]; exists && other != entry.ID {
			statuses[entry.ID] = codeContextStatus{
				stale: true, start: entry.Start, end: entry.End,
				reason: fmt.Sprintf("the code moved to lines %d-%d, which entry #%d reads; drop this entry", status.start, status.end, other),
			}
			continue
		}
		taken[key] = entry.ID
	}
}

func codeContextRangeKey(path string, start, end int64) string {
	return fmt.Sprintf("%s\x00%d\x00%d", path, start, end)
}

// saveCodeContextRelocations writes the relocated ranges, each only while its entry
// still reads the range that was checked. It answers the document as written, how many
// entries moved, and a note when they could not be saved.
func saveCodeContextRelocations(store codeContextStore, task, by string, maxBytes int64, document codeContextDocument, statuses map[int64]codeContextStatus) (codeContextDocument, int, string) {
	checked := make(map[int64]codeContextEntry, len(document.Entries))
	pending := false
	for _, entry := range document.Entries {
		checked[entry.ID] = entry
		if statuses[entry.ID].relocated {
			pending = true
		}
	}
	if !pending {
		return document, 0, ""
	}
	moved := 0
	written, err := store.mutate(task, by, maxBytes, func(current *codeContextDocument) error {
		for index, entry := range current.Entries {
			status := statuses[entry.ID]
			before, known := checked[entry.ID]
			if !status.relocated || !known || before.Path != entry.Path || before.Start != entry.Start || before.End != entry.End || before.ContentSHA256 != entry.ContentSHA256 {
				continue
			}
			entry.Start, entry.End, entry.FileSHA256, entry.Generation = status.start, status.end, status.fileSHA256, current.Generation
			current.Entries[index] = entry
			moved++
		}
		if moved == 0 {
			return errCodeContextNoChange
		}
		return nil
	})
	if errors.Is(err, errCodeContextNoChange) {
		return document, 0, ""
	}
	if err != nil {
		return document, 0, "the relocated ranges could not be saved (" + oneLine(err.Error()) + "); they are shown where the code is now"
	}
	return written, moved, ""
}

// codeContextFingerprint names what a caller holds of an entry once it was printed: its
// path, its code and its description. A relocation keeps it, so code that only moved is
// not printed again.
func codeContextFingerprint(entry codeContextEntry) string {
	sum := sha256.Sum256([]byte(entry.Path + "\x00" + entry.ContentSHA256 + "\x00" + entry.Description))
	return hex.EncodeToString(sum[:8])
}
