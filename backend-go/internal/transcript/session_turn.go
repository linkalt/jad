package transcript

import (
	"errors"
	"fmt"
)

// NextSessionTurn returns the next one-based session turn number using the
// authoritative transcript records. Input items in the same persisted batch
// count as one user submission, matching the TypeScript usage summary logic.
// History caches and compaction overlays are intentionally not applied here.
func (s *TurnStore) NextSessionTurn() (int64, error) {
	if s == nil || s.journal == nil {
		return 0, errors.New("turn store is not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovering {
		return 0, errors.New("turn store recovery is in progress")
	}
	if len(s.activeTurns) > 0 {
		return 0, errors.New("cannot read session turn while a response turn is active")
	}

	var turnCount int64
	seenBatches := make(map[string]struct{})
	err := s.journal.ReadLines(func(line []byte) error {
		fields, err := objectFields(line)
		if err != nil {
			return fmt.Errorf("read session turn transcript record: %w", err)
		}

		if roleRaw, exists := fieldValue(fields, "role"); exists {
			if role, isString := stringValue(roleRaw); isString && role == "user" {
				batchRaw, hasBatch := fieldValue(fields, SessionInputBatchIDField)
				batchID, batchIsString := stringValue(batchRaw)
				if !hasBatch || !batchIsString {
					if turnCount < maxJavaScriptSafeInteger {
						turnCount++
					}
				} else if _, seen := seenBatches[batchID]; !seen {
					seenBatches[batchID] = struct{}{}
					if turnCount < maxJavaScriptSafeInteger {
						turnCount++
					}
				}
			}
		}

		typeRaw, exists := fieldValue(fields, "type")
		recordType, isString := stringValue(typeRaw)
		if !exists || !isString || recordType != "response.completed" {
			return nil
		}
		if explicitRaw, exists := fieldValue(fields, "session_turn"); exists {
			explicitTurn, valid := safeInteger(explicitRaw, true)
			if !valid {
				return errors.New("session_turn must be a positive safe integer")
			}
			turnCount = explicitTurn
		} else if turnCount < 1 {
			turnCount = 1
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if turnCount >= maxJavaScriptSafeInteger {
		return 0, errors.New("session turn counter is exhausted")
	}
	return turnCount + 1, nil
}
