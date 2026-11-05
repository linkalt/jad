package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// steerMarkSuffix is how a marked record ends: persistInputItem appends SessionInputSteerField
// last, and encodeJSONObject writes no space.
var steerMarkSuffix = []byte(`,"` + SessionInputSteerField + `":true}`)

// StripSteerMarks finds every record of the messages.jsonl at path that carries
// SessionInputSteerField and, with write, overwrites the field with spaces in place, so a build
// from before the field can read the transcript (it would send the field to the provider). The
// file keeps its size, its inode and every record's offset, which message ids and the
// transcript caches rely on; a cache that hashed the old bytes is rebuilt, as after any
// change it did not make. It returns the number of marked records. A record that carries the
// field anywhere else than last, or with another value than true, is an error, and nothing
// is written. The caller holds the transcript's lock.
func StripSteerMarks(path string, write bool) (int, error) {
	offsets, err := steerMarkOffsets(path)
	if err != nil || !write || len(offsets) == 0 {
		return len(offsets), err
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	blank := bytes.Repeat([]byte{' '}, len(steerMarkSuffix)-1)
	for _, offset := range offsets {
		if _, err := file.WriteAt(blank, offset); err != nil {
			_ = file.Close()
			return 0, fmt.Errorf("write %s: %w", path, err)
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return 0, fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return 0, fmt.Errorf("close %s: %w", path, err)
	}
	return len(offsets), nil
}

// steerMarkOffsets returns the offset of the comma that starts the marker of each marked
// record: the bytes from there to the record's closing brace are the field.
func steerMarkOffsets(path string) ([]int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	key := []byte(`"` + SessionInputSteerField + `"`)
	var offsets []int64
	var start int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			record := bytes.TrimRight(line, "\r\n")
			if bytes.Contains(record, key) {
				if fields, err := objectFields(record); err == nil {
					if value, marked := fieldValue(fields, SessionInputSteerField); marked {
						if string(value) != "true" || !bytes.HasSuffix(record, steerMarkSuffix) {
							return nil, fmt.Errorf("%s: the record at offset %d carries %s in a form this command does not rewrite; nothing was changed", path, start, SessionInputSteerField)
						}
						offsets = append(offsets, start+int64(len(record)-len(steerMarkSuffix)))
					}
				}
			}
			start += int64(len(line))
		}
		if errors.Is(readErr, io.EOF) {
			return offsets, nil
		}
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", path, readErr)
		}
	}
}
