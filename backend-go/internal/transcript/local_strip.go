package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// LocalStripCounts counts what StripLocalFields found: the records it blanks and, among
// them, the steer markers and the MetaField members.
type LocalStripCounts struct {
	Records int
	Steer   int
	Meta    int
}

type localSpan struct {
	offset int64
	length int
}

// StripLocalFields finds the trailing JAD-only members of every record of the messages.jsonl
// at path (SessionInputSteerField and MetaField, see locateLocalTail) and, with write,
// overwrites them with spaces in place, so an older build can read the transcript (it would
// send the members to the provider). With metaOnly only MetaField is blanked (for a build that
// knows the steer marker); otherwise both are (for a build from before the marker). The batch
// fields stay: every build reads them. The file keeps its size, its inode and every record's
// offset, which message ids and the transcript caches rely on; a cache that hashed the old
// bytes is rebuilt, as after any change it did not make. A line that is not a JSON object is
// left alone. A record that carries either member anywhere else than at the end, or a steer
// marker other than true, is an error, and nothing is written. The caller holds the
// transcript's lock.
func StripLocalFields(path string, write, metaOnly bool) (LocalStripCounts, error) {
	spans, counts, err := localFieldSpans(path, metaOnly)
	if err != nil || !write || len(spans) == 0 {
		return counts, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return LocalStripCounts{}, fmt.Errorf("open %s: %w", path, err)
	}
	var blank []byte
	for _, span := range spans {
		if len(blank) < span.length {
			blank = bytes.Repeat([]byte{' '}, span.length)
		}
		if _, err := file.WriteAt(blank[:span.length], span.offset); err != nil {
			_ = file.Close()
			return LocalStripCounts{}, fmt.Errorf("write %s: %w", path, err)
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return LocalStripCounts{}, fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return LocalStripCounts{}, fmt.Errorf("close %s: %w", path, err)
	}
	return counts, nil
}

func localFieldSpans(path string, metaOnly bool) ([]localSpan, LocalStripCounts, error) {
	var counts LocalStripCounts
	file, err := os.Open(path)
	if err != nil {
		return nil, counts, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	var spans []localSpan
	var start int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			record := bytes.TrimRight(line, "\r\n")
			tail, err := locateLocalTail(record)
			switch {
			case errors.Is(err, errNotRecord):
			case err != nil:
				return nil, LocalStripCounts{}, fmt.Errorf("%s: the record at offset %d: %v; nothing was changed", path, start, err)
			default:
				from := tail.meta
				if tail.meta >= 0 {
					counts.Meta++
				}
				if tail.steer >= 0 && !metaOnly {
					from = tail.steer
					counts.Steer++
				}
				if from >= 0 {
					counts.Records++
					spans = append(spans, localSpan{offset: start + int64(from), length: tail.end - from})
				}
			}
			start += int64(len(line))
		}
		if errors.Is(readErr, io.EOF) {
			return spans, counts, nil
		}
		if readErr != nil {
			return nil, LocalStripCounts{}, fmt.Errorf("read %s: %w", path, readErr)
		}
	}
}
