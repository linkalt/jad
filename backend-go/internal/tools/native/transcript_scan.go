package native

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// transcriptHead is the part of a transcript record the scanner needs to tell where a turn
// starts and which turn a record belongs to.
type transcriptHead struct {
	Role        string `json:"role"`
	SessionTurn *int64 `json:"session_turn"`
	BatchIndex  *int64 `json:"__agent_session_input_batch_index"`
}

func (h transcriptHead) startsUserTurn() bool {
	return h.Role == "user" && (h.BatchIndex == nil || *h.BatchIndex == 0)
}

type transcriptTurn struct {
	off    int64
	number int64
}

type transcriptLine struct {
	off     int64
	length  int64
	data    []byte
	tooLong bool
}

// scanTranscript visits the complete LF-terminated lines of file in [start, end). The file is
// only read: an incomplete last line is neither visited nor repaired, so a transcript a run
// is writing is never changed by a reader. A line longer than maxLine is reported with
// tooLong and no data. data is valid only during visit, which returns false to stop.
func scanTranscript(ctx context.Context, file *os.File, start, end, maxLine int64, visit func(transcriptLine) (bool, error)) error {
	if end <= start {
		return nil
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(file, start, end-start), 256<<10)
	off := start
	var buffer []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		buffer = buffer[:0]
		tooLong := false
		var length int64
		for {
			chunk, err := reader.ReadSlice('\n')
			length += int64(len(chunk))
			if !tooLong {
				if int64(len(buffer)+len(chunk)) > maxLine+1 {
					tooLong = true
					buffer = buffer[:0]
				} else {
					buffer = append(buffer, chunk...)
				}
			}
			if err == nil {
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line := transcriptLine{off: off, length: length, tooLong: tooLong}
		if !tooLong {
			line.data = buffer[:len(buffer)-1]
		}
		off += length
		more, err := visit(line)
		if err != nil || !more {
			return err
		}
	}
}

// indexTranscriptTurns returns where every turn of file[0:size) starts and where the last
// complete line ends. A user message
// starts a turn; the session_turn of a completed response names it, which wins over the count,
// so a compacted transcript starts above 1.
func indexTranscriptTurns(ctx context.Context, file *os.File, size, maxLine int64) ([]transcriptTurn, int64, error) {
	var turns []transcriptTurn
	var count, end int64
	err := scanTranscript(ctx, file, 0, size, maxLine, func(line transcriptLine) (bool, error) {
		end = line.off + line.length
		var head transcriptHead
		if !line.tooLong {
			_ = json.Unmarshal(line.data, &head)
		}
		if len(turns) == 0 || head.startsUserTurn() {
			count++
			turns = append(turns, transcriptTurn{off: line.off, number: count})
		}
		if head.SessionTurn != nil && *head.SessionTurn > 0 {
			count = *head.SessionTurn
			turns[len(turns)-1].number = count
		}
		return true, nil
	})
	return turns, end, err
}

// transcriptIdentity names the file a cursor was made for: a hash of its first line (at most
// 4 KiB). Appends do not change it; a compaction or a reset does.
func transcriptIdentity(file *os.File, size int64) (string, error) {
	head := make([]byte, min(size, 4096))
	if _, err := file.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	sum := sha256.Sum256(head)
	return hex.EncodeToString(sum[:8]), nil
}
