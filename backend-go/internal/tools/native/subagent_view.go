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
	return scanTranscript(ctx, file, 0, size, maxLine, func(line transcriptLine) (bool, error) {
		if line.tooLong {
			return true, nil
		}
		return visit(line.off, line.data), nil
	})
}
