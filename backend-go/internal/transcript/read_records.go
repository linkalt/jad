package transcript

import "errors"

// ReadRecords visits complete transcript records with their physical byte
// offsets. Reads are serialized with Journal appends and recoveries.
func (s *TurnStore) ReadRecords(visit func(offset int64, line []byte) error) error {
	return s.ReadRecordsRange(0, 1<<63-1, visit)
}

// ReadRecordsRange reads complete records in the half-open byte interval
// [start, end). Bounds must fall on record boundaries.
func (s *TurnStore) ReadRecordsRange(start, end int64, visit func(offset int64, line []byte) error) error {
	if s == nil || s.journal == nil {
		return errors.New("turn store is not initialized")
	}
	s.mu.Lock()
	recovering := s.recovering
	s.mu.Unlock()
	if recovering {
		return errors.New("turn store recovery is in progress")
	}
	return s.journal.ReadRecordsRange(start, end, visit)
}

// ReadCompleteRecordsRange reads LF-terminated records without repairing an
// incomplete final record, returning the next offset that was fully consumed.
func (s *TurnStore) ReadCompleteRecordsRange(start, end int64, visit func(offset int64, line []byte) error) (int64, error) {
	if s == nil || s.journal == nil {
		return start, errors.New("turn store is not initialized")
	}
	s.mu.Lock()
	recovering := s.recovering
	s.mu.Unlock()
	if recovering {
		return start, errors.New("turn store recovery is in progress")
	}
	return s.journal.ReadCompleteRecordsRange(start, end, visit)
}
