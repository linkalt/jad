package native

import (
	"context"
	"os"
)

// SubagentTranscriptFolder reports whether dir is a child's transcript folder directly
// under the subagent storage directory root (and not its manifest folder). A reader of a
// child's transcript uses it to refuse any path that is not one.
func SubagentTranscriptFolder(root, dir string) bool {
	return transcriptFolderUnder(root, dir)
}

// ScanTranscriptLines visits the complete lines of file[0:size) in order with their byte
// offset. A line over maxLine is skipped, and an incomplete last line is never visited, so
// a child that is still writing is only read, never changed. line is valid during visit,
// which returns false to stop.
func ScanTranscriptLines(ctx context.Context, file *os.File, size, maxLine int64, visit func(offset int64, line []byte) bool) error {
	return ScanTranscriptLinesSkipping(ctx, file, size, maxLine, visit, nil)
}

// ScanTranscriptLinesSkipping is ScanTranscriptLines that also tells where a line over
// maxLine was skipped (skipped is called with its offset, in file order between the
// visits; nil ignores them), so a reader knows its view of the file has a gap.
func ScanTranscriptLinesSkipping(ctx context.Context, file *os.File, size, maxLine int64, visit func(offset int64, line []byte) bool, skipped func(offset int64)) error {
	return ScanTranscriptRangeSkipping(ctx, file, 0, size, maxLine, visit, skipped)
}

// ScanTranscriptRangeSkipping is ScanTranscriptLinesSkipping over file[start:end): offsets
// are the file's own. The first line is whatever begins at start: a caller that starts
// inside a line gets that line's tail first and skips it. The range ends at end or at the
// last complete line before it.
func ScanTranscriptRangeSkipping(ctx context.Context, file *os.File, start, end, maxLine int64, visit func(offset int64, line []byte) bool, skipped func(offset int64)) error {
	return scanTranscript(ctx, file, start, end, maxLine, func(line transcriptLine) (bool, error) {
		if line.tooLong {
			if skipped != nil {
				skipped(line.off)
			}
			return true, nil
		}
		return visit(line.off, line.data), nil
	})
}
