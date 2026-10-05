package app

import (
	"encoding/json"
	"errors"

	"example.com/agent-harness/backend-go/internal/transcript"
)

type turnStartsCache struct {
	generation string
	size       int64
	consumed   int
	lastRun    *transcript.CursorRun
	scanOffset int64
	previous   *previousTurnRecord
	starts     []int64
}

type previousTurnRecord struct {
	offset          int64
	isUserTurn      bool
	batchID         string
	batchIndex      int64
	hasBatchIndex   bool
	firstUserOffset *int64
}

// readTurnStarts maps the persistent response cursor index to the byte offset
// that opens each user turn. The in-memory projection scans only records added
// since the last request and resets when the transcript generation changes.
func (shared *sharedSession) readTurnStarts(generation string) ([]int64, error) {
	if shared == nil || shared.store == nil {
		return nil, errors.New("shared transcript store is not initialized")
	}
	index, err := shared.store.ReadCursorIndex()
	if err != nil {
		return nil, err
	}

	shared.turnStartsMu.Lock()
	defer shared.turnStartsMu.Unlock()
	cache := shared.turnStarts
	if cache == nil || cache.generation != generation || cache.size > index.Size || cache.scanOffset > index.Size || cache.consumed > len(index.Runs) || (cache.consumed < len(index.Runs) && index.Runs[cache.consumed].Offset < cache.scanOffset) || !sameCursorRun(cache.lastRun, cursorRunBefore(index.Runs, cache.consumed)) {
		cache = &turnStartsCache{
			generation: generation,
			size:       index.Size,
			starts:     []int64{0},
		}
	}

	consumed := cache.consumed
	lastRun := cloneCursorRun(cursorRunBefore(index.Runs, consumed))
	previous := clonePreviousTurnRecord(cache.previous)
	appendedStarts := make([]int64, 0)
	scannedTo, err := shared.store.ReadCompleteRecordsRange(cache.scanOffset, index.Size, func(offset int64, line []byte) error {
		fields := make(map[string]json.RawMessage)
		if json.Unmarshal(line, &fields) != nil || fields == nil {
			previous = nil
			return nil
		}
		if run, ok := cursorRunAt(index.Runs, consumed, offset); ok {
			runCopy := run
			lastRun = &runCopy
			consumed++
			if run.Step == 1 && run.Offset > 0 {
				start := run.Offset
				if previous != nil {
					if previous.firstUserOffset != nil {
						start = *previous.firstUserOffset
					} else if previous.isUserTurn {
						start = previous.offset
					}
				}
				lastStart := cache.starts[len(cache.starts)-1]
				if len(appendedStarts) > 0 {
					lastStart = appendedStarts[len(appendedStarts)-1]
				}
				if start > lastStart {
					appendedStarts = append(appendedStarts, start)
				}
			}
		}
		prior := previous
		current := nextPreviousTurnRecord(fields, offset)
		if current != nil && current.batchID != "" && current.hasBatchIndex && prior != nil && prior.batchID == current.batchID && prior.hasBatchIndex && prior.batchIndex+1 == current.batchIndex && prior.firstUserOffset != nil {
			first := *prior.firstUserOffset
			current.firstUserOffset = &first
		}
		previous = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	starts := cache.starts
	if len(appendedStarts) > 0 {
		starts = append(append([]int64(nil), cache.starts...), appendedStarts...)
	}
	cache.generation = generation
	cache.size = index.Size
	cache.consumed = consumed
	cache.lastRun = lastRun
	cache.scanOffset = scannedTo
	cache.previous = previous
	cache.starts = starts
	shared.turnStarts = cache
	return append([]int64(nil), cache.starts...), nil
}

func nextPreviousTurnRecord(fields map[string]json.RawMessage, offset int64) *previousTurnRecord {
	var role string
	_ = json.Unmarshal(fields["role"], &role)
	record := &previousTurnRecord{offset: offset, isUserTurn: role == "user"}
	var batchID string
	if raw, exists := fields[transcript.SessionInputBatchIDField]; !exists || json.Unmarshal(raw, &batchID) != nil || batchID == "" {
		return record
	}
	var batchIndex int64
	if raw, exists := fields[transcript.SessionInputBatchIndexField]; !exists || json.Unmarshal(raw, &batchIndex) != nil || batchIndex < 0 || batchIndex > (1<<53)-1 {
		return record
	}
	record.batchID = batchID
	record.batchIndex = batchIndex
	record.hasBatchIndex = true
	if record.isUserTurn {
		first := offset
		record.firstUserOffset = &first
	}
	return record
}

func cursorRunAt(runs []transcript.CursorRun, index int, offset int64) (transcript.CursorRun, bool) {
	if index >= 0 && index < len(runs) && runs[index].Offset == offset {
		return runs[index], true
	}
	return transcript.CursorRun{}, false
}

func cursorRunBefore(runs []transcript.CursorRun, consumed int) *transcript.CursorRun {
	if consumed <= 0 || consumed > len(runs) {
		return nil
	}
	return &runs[consumed-1]
}

func cloneCursorRun(run *transcript.CursorRun) *transcript.CursorRun {
	if run == nil {
		return nil
	}
	copy := *run
	return &copy
}

func sameCursorRun(left, right *transcript.CursorRun) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Offset == right.Offset && left.Turn == right.Turn && left.Step == right.Step
}

func clonePreviousTurnRecord(record *previousTurnRecord) *previousTurnRecord {
	if record == nil {
		return nil
	}
	copy := *record
	if record.firstUserOffset != nil {
		first := *record.firstUserOffset
		copy.firstUserOffset = &first
	}
	return &copy
}

func (shared *sharedSession) invalidateTurnStarts() {
	if shared == nil {
		return
	}
	shared.turnStartsMu.Lock()
	shared.turnStarts = nil
	shared.turnStartsMu.Unlock()
}
