package transcript

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	SessionInputBatchIDField    = "__agent_session_input_batch_id"
	SessionInputBatchIndexField = "__agent_session_input_batch_index"
	// SessionInputSteerField marks an input record that a steering message stored in the
	// middle of a run (AppendSteerInputItemsAt). It is true when present. Compaction keeps
	// such a user message in the turn before it instead of starting a turn at it. Like the
	// batch fields and MetaField (every reservedFieldPrefix key) it is stripped before the
	// item is sent anywhere.
	SessionInputSteerField = "__agent_session_input_steer"
)

// AppendInputItems persists one atomic-in-process batch of Responses API input
// items. Batch metadata is stored in messages.jsonl for turn-boundary behavior
// and removed again by ReadInputItems.
func (s *TurnStore) AppendInputItems(items ...[]byte) error {
	_, err := s.AppendInputItemsAt(items...)
	return err
}

// InputAppendResult says where an appended input batch landed in the transcript.
type InputAppendResult struct {
	// MessageOffset is the byte offset of the last record of the batch that the
	// web-ui projection shows as a user message, which is what its message id
	// (session-user-o<offset>) is built from. HasMessage is false when none of
	// the records has visible text.
	MessageOffset int64
	HasMessage    bool
}

// AppendInputItemsAt is AppendInputItems that also reports the offset of the
// batch's visible user message.
func (s *TurnStore) AppendInputItemsAt(items ...[]byte) (InputAppendResult, error) {
	return s.appendInputItems(false, items)
}

// AppendSteerInputItemsAt is AppendInputItemsAt for a steering message of a running run:
// each record also carries SessionInputSteerField, so compaction keeps it in the run's turn.
func (s *TurnStore) AppendSteerInputItemsAt(items ...[]byte) (InputAppendResult, error) {
	return s.appendInputItems(true, items)
}

func (s *TurnStore) appendInputItems(steer bool, items [][]byte) (InputAppendResult, error) {
	if s == nil || s.journal == nil {
		return InputAppendResult{}, errors.New("turn store is not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovering {
		return InputAppendResult{}, errors.New("turn store recovery is in progress")
	}
	if len(s.activeTurns) > 0 {
		return InputAppendResult{}, errors.New("cannot append input while a response turn is active")
	}
	if len(items) == 0 {
		return InputAppendResult{}, nil
	}

	canonicalItems := make([][]byte, len(items))
	for index, item := range items {
		if err := ValidateInputItem(index, item); err != nil {
			return InputAppendResult{}, err
		}
		var err error
		canonicalItems[index], err = canonicalJSON(item)
		if err != nil {
			return InputAppendResult{}, fmt.Errorf("serialize input item %d: %w", index, err)
		}
	}

	var batchBytes [16]byte
	if _, err := rand.Read(batchBytes[:]); err != nil {
		return InputAppendResult{}, fmt.Errorf("create input batch ID: %w", err)
	}
	batchID := hex.EncodeToString(batchBytes[:])
	persisted := make([][]byte, len(canonicalItems))
	for index, item := range canonicalItems {
		persistedItem, err := persistInputItem(item, batchID, index, steer)
		if err != nil {
			return InputAppendResult{}, fmt.Errorf("persist input item %d: %w", index, err)
		}
		persisted[index] = persistedItem
	}
	offsets, err := s.journal.AppendAt(persisted...)
	if err != nil {
		return InputAppendResult{}, fmt.Errorf("append input batch: %w", err)
	}
	var result InputAppendResult
	for index, item := range canonicalItems {
		if isVisibleUserInput(item) {
			result.MessageOffset, result.HasMessage = offsets[index], true
		}
	}
	return result, nil
}

// isVisibleUserInput reports whether the web-ui projection turns this input
// record into a user message: a user message with some non-empty input_text, an image or a file.
func isVisibleUserInput(item []byte) bool {
	var input struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(item, &input) != nil || input.Type != "message" || input.Role != "user" {
		return false
	}
	for _, part := range input.Content {
		// An image or a file makes the message visible too (see webui.UserContent).
		if (part.Type == "input_text" && part.Text != "") || part.Type == "input_image" || part.Type == "input_file" {
			return true
		}
	}
	return false
}

func persistInputItem(canonicalItem []byte, batchID string, index int, steer bool) ([]byte, error) {
	fields, err := objectFields(canonicalItem)
	if err != nil {
		return nil, fmt.Errorf("read canonical input item: %w", err)
	}
	fields = append(fields,
		jsonField{key: SessionInputBatchIDField, value: appendJSONString(nil, batchID)},
		jsonField{key: SessionInputBatchIndexField, value: []byte(strconv.Itoa(index))},
	)
	if steer {
		fields = append(fields, jsonField{key: SessionInputSteerField, value: []byte("true")})
	}
	return encodeJSONObject(fields), nil
}

// ReadInputItems reconstructs Responses API request history from messages.jsonl
// without reading or applying compaction/history caches.
func (s *TurnStore) ReadInputItems() ([][]byte, error) {
	if s == nil || s.journal == nil {
		return nil, errors.New("turn store is not initialized")
	}
	return s.readInputItems(nil)
}

// readInputItems implements ReadInputItems. A non-nil handle lets the reads
// reuse the compaction cache and transcript expansion an earlier read of the
// same handle produced; its items are then shared instead of copied.
func (s *TurnStore) readInputItems(handle *InputHistory) ([][]byte, error) {
	var cache *compactionCache
	var prefix [][]byte
	var err error
	if handle != nil {
		cache, prefix, err = s.readCompactionCacheMemo(&handle.compaction)
	} else {
		cache, err = s.readCompactionCache()
	}
	if err != nil {
		return nil, err
	}
	return s.readInputItemsWithCache(handle, cache, prefix, nil)
}

// readInputItemsWithCache is readInputItems after the compaction cache was
// read. A caller that already holds the cache (a compaction, which also needs
// it for the assistant-final items) passes it in so the cache file is parsed
// and validated once. prefix is the cache's items as a handle shares them and
// is used only with a handle. continued, when not nil (only without a handle), gets one
// flag per returned item: true for a user message that continues the turn before it (a
// steering message, from the cache's continued_items or the record's SessionInputSteerField).
func (s *TurnStore) readInputItemsWithCache(handle *InputHistory, cache *compactionCache, prefix [][]byte, continued *[]bool) ([][]byte, error) {
	var err error
	items := make([][]byte, 0)
	var flags []bool
	if continued != nil {
		defer func() { *continued = flags }()
	}
	if cache != nil {
		if handle != nil {
			items = append(items, prefix...)
		} else {
			for _, item := range cache.Items {
				items = append(items, append([]byte(nil), item...))
			}
			if continued != nil {
				flags = cache.continuedFlags()
			}
		}
		sourceIsTranscript, err := s.cacheSourceIsTranscript(cache)
		if err != nil {
			return nil, err
		}
		if sourceIsTranscript {
			if handle != nil {
				tail, err := handle.compaction.activeTailItems(cache)
				if err != nil {
					return nil, err
				}
				return append(items, tail...), nil
			}
			for _, record := range cache.ActiveTail {
				expanded, err := expandTranscriptRecord(record)
				if err != nil {
					return nil, err
				}
				items = append(items, expanded...)
				if continued != nil {
					flags = append(flags, make([]bool, len(expanded))...)
				}
			}
			return items, nil
		}
	}
	if handle != nil {
		tail, err := s.readTranscriptItemsMemo(&handle.memo)
		if err != nil {
			return nil, err
		}
		return append(items, tail...), nil
	}
	err = s.journal.ReadLines(func(line []byte) error {
		expanded, err := expandTranscriptRecord(line)
		if err != nil {
			return fmt.Errorf("expand transcript record: %w", err)
		}
		items = append(items, expanded...)
		if continued != nil {
			steered := recordIsSteered(line)
			for range expanded {
				flags = append(flags, steered)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// ValidateInputItem reports whether input item index may be appended. It is the check that
// AppendInputItemsAt runs before writing anything, exported so that a caller can run it
// before a side effect (an image written for the item) that a refusal must not leave behind.
func ValidateInputItem(index int, item []byte) error {
	fields, err := objectFields(item)
	if err != nil {
		return fmt.Errorf("input item %d must be an object", index)
	}
	if err := validateInputFields(fields); err != nil {
		return fmt.Errorf("input item %d: %w", index, err)
	}
	for _, field := range fields {
		if isReservedField(field.key) {
			return fmt.Errorf("input item %d: input items may not use reserved %s fields", index, reservedFieldPrefix)
		}
	}
	return nil
}

func validateInputFields(fields []jsonField) error {
	typeRaw, exists := fieldValue(fields, "type")
	itemType, isString := stringValue(typeRaw)
	if !exists || !isString || itemType == "" {
		return errors.New("input item must contain a non-empty string type")
	}
	if itemType == "response.completed" || strings.HasPrefix(itemType, "response.") {
		return errors.New("response events cannot be appended as input items")
	}
	return nil
}

func expandTranscriptRecord(raw []byte) ([][]byte, error) {
	fields, err := objectFields(raw)
	if err != nil {
		return nil, fmt.Errorf("messages.jsonl record must be an object: %w", err)
	}
	if rawType, exists := fieldValue(fields, "type"); exists {
		if recordType, isString := stringValue(rawType); isString && recordType == "response.completed" {
			return expandCompletedRecord(fields)
		}
		if recordType, isString := stringValue(rawType); isString && strings.HasPrefix(recordType, "response.") {
			return nil, fmt.Errorf("unsupported response event in messages.jsonl: %s", recordType)
		}
	}

	fields = removeReservedFields(fields)
	if err := validateInputFields(fields); err != nil {
		return nil, err
	}
	canonical, err := canonicalJSONFields(fields)
	if err != nil {
		return nil, err
	}
	return [][]byte{canonical}, nil
}

func expandCompletedRecord(eventFields []jsonField) ([][]byte, error) {
	for _, metadataField := range []string{"session_turn", "session_step"} {
		if raw, exists := fieldValue(eventFields, metadataField); exists {
			if _, valid := safeInteger(raw, true); !valid {
				return nil, fmt.Errorf("%s must be a positive safe integer", metadataField)
			}
		}
	}
	responseRaw, exists := fieldValue(eventFields, "response")
	if !exists {
		return nil, errors.New("completed response must be an object")
	}
	// responseRaw is a sub-value of a record objectFields already validated, so
	// the *Valid variants skip re-validating the same bytes at every level.
	responseFields, err := objectFieldsValid(bytes.TrimSpace(responseRaw))
	if err != nil {
		return nil, errors.New("completed response must be an object")
	}
	outputRaw, exists := fieldValue(responseFields, "output")
	if !exists {
		return nil, errors.New("committed response has no output array")
	}
	outputRaw = bytes.TrimSpace(outputRaw)
	if len(outputRaw) == 0 || outputRaw[0] != '[' {
		return nil, fmt.Errorf("committed response output must be an array: %w", errors.New("expected a JSON array"))
	}
	items := make([][]byte, 0, 4)
	err = arrayValuesValid(outputRaw, func(item []byte) error {
		itemFields, err := objectFieldsValid(item)
		if err != nil {
			return errors.New("output item must be an object")
		}
		if err := validateOutputItemType(itemFields); err != nil {
			return err
		}
		canonical, err := canonicalJSONValid(item)
		if err != nil {
			return err
		}
		items = append(items, canonical)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("committed response output must be an array: %w", err)
	}
	return items, nil
}

func canonicalJSONFields(fields []jsonField) ([]byte, error) {
	canonicalFields := make([]jsonField, len(fields))
	for index, field := range fields {
		// fields come from objectFields, so each value is already valid JSON.
		value, err := canonicalJSONValid(field.value)
		if err != nil {
			return nil, fmt.Errorf("canonicalize input field %q: %w", field.key, err)
		}
		canonicalFields[index] = jsonField{key: field.key, value: value}
	}
	return encodeJSONObject(canonicalFields), nil
}

// removeReservedFields drops every top-level reservedFieldPrefix key of an input record (the
// batch fields, the steer marker, MetaField and any later one), so none reaches a provider.
// It returns fields itself when there is none.
func removeReservedFields(fields []jsonField) []jsonField {
	first := -1
	for index, field := range fields {
		if isReservedField(field.key) {
			first = index
			break
		}
	}
	if first < 0 {
		return fields
	}
	filtered := make([]jsonField, first, len(fields))
	copy(filtered, fields[:first])
	for _, field := range fields[first+1:] {
		if !isReservedField(field.key) {
			filtered = append(filtered, field)
		}
	}
	return filtered
}

// recordIsSteered reports whether a messages.jsonl record carries SessionInputSteerField
// set to true.
func recordIsSteered(record []byte) bool {
	fields, err := objectFields(record)
	if err != nil {
		return false
	}
	raw, exists := fieldValue(fields, SessionInputSteerField)
	return exists && string(raw) == "true"
}
