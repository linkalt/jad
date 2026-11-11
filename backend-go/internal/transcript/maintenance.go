package transcript

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

type compactionCache struct {
	SchemaVersion            int               `json:"schema_version"`
	SourceSHA256             string            `json:"source_sha256"`
	SourceBytes              int64             `json:"source_bytes,omitempty"`
	Items                    []json.RawMessage `json:"items"`
	AssistantFinalItems      []string          `json:"assistant_final_items"`
	ActiveTail               []json.RawMessage `json:"active_tail"`
	ResetLatestResponseUsage bool              `json:"reset_latest_response_usage"`
	// ContinuedItems are the indexes in Items of user messages that continue the turn
	// before them (steering messages, SessionInputSteerField), ascending. Absent in a cache
	// written before the field existed, so every user message starts a turn there.
	ContinuedItems []int `json:"continued_items,omitempty"`
}

// continuedFlags returns one flag per item of the cache: true at ContinuedItems. An index
// out of range is ignored.
func (c *compactionCache) continuedFlags() []bool {
	flags := make([]bool, len(c.Items))
	for _, index := range c.ContinuedItems {
		if index >= 0 && index < len(flags) {
			flags[index] = true
		}
	}
	return flags
}

type CompactionOptions struct {
	TurnType      string
	KeepTurns     int
	KeepToolPairs int
	BackupMode    string
	// AppendLimit, with BackupMode append, keeps only the newest this many user turns in
	// the backup after each append (see backupTranscript); 0 keeps every turn.
	AppendLimit        int
	PreserveResponseID string
	// KeepLatestTurn keeps the newest turn (the latest user message and everything after
	// it) whole, even when KeepTurns is 0 and with every tool pair assistant_final would
	// strip. The overflow compaction of a run sets it, so the run's own tool work is not
	// dropped while it is still going.
	KeepLatestTurn bool
}

type CompactionResult struct {
	Compacted      bool   `json:"compacted"`
	CompactedItems int    `json:"compacted_items"`
	RetainedItems  int    `json:"retained_items"`
	BackupPath     string `json:"backup_path"`
}

// CompactHistory retains the configured recent input turns and atomically
// replaces the transcript. The original transcript is copied to its backup
// before publication; derived Go/TypeScript caches are invalidated afterward.
func (s *TurnStore) CompactHistory(options CompactionOptions) (CompactionResult, error) {
	result := CompactionResult{}
	if s == nil || s.journal == nil {
		return result, errors.New("turn store is not initialized")
	}
	if options.TurnType != "assistant_final" && options.TurnType != "full" {
		return result, errors.New("turn type must be assistant_final or full")
	}
	if options.KeepTurns < 0 || options.KeepToolPairs < 0 {
		return result, errors.New("compaction counts must not be negative")
	}
	if options.BackupMode != "replace" && options.BackupMode != "append" {
		return result, errors.New("backup mode must be replace or append")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, err := s.planCompaction(options)
	if err != nil {
		return result, err
	}
	history, activeTail, finalAssistant, selected := plan.history, plan.activeTail, plan.finalAssistant, plan.selected

	retained := make([][]byte, 0, len(history))
	pairTurns := selected
	if options.KeepLatestTurn && len(selected) > 0 {
		// The latest turn keeps all its pairs; keep_tool_pairs counts in the turns before it.
		pairTurns = selected[:len(selected)-1]
	}
	keepPairs := keptToolPairs(pairTurns, options)
	// continuedItems are the user messages of the retained items that continue their turn:
	// every user message of a turn except the one that started it (see planCompaction).
	continuedItems := make([]int, 0)
	for index, turn := range selected {
		kept := turn
		if !options.KeepLatestTurn || index != len(selected)-1 {
			kept = retainCompactedTurn(turn, options, finalAssistant, keepPairs)
		}
		startedByUser := len(turn) > 0 && isUserItem(turn[0])
		for position, item := range kept {
			if isUserItem(item) && !(startedByUser && position == 0) {
				continuedItems = append(continuedItems, len(retained))
			}
			retained = append(retained, item)
		}
	}
	compactedActiveTail := activeTail
	if options.TurnType == "assistant_final" {
		compactedActiveTail = make([]json.RawMessage, 0, len(activeTail))
		for _, record := range activeTail {
			compacted, err := assistantFinalActiveTailRecord(record)
			if err != nil {
				return result, fmt.Errorf("compact preserved response: %w", err)
			}
			compactedActiveTail = append(compactedActiveTail, compacted)
		}
	}
	activeTailItems := make([][]byte, 0)
	for _, record := range compactedActiveTail {
		expanded, err := expandTranscriptRecord(record)
		if err != nil {
			return result, fmt.Errorf("expand preserved response: %w", err)
		}
		activeTailItems = append(activeTailItems, expanded...)
	}
	retainedWithActiveTail := append(append([][]byte(nil), retained...), activeTailItems...)
	if compactedInputBytes(retainedWithActiveTail) >= compactedInputBytes(history) {
		return result, nil
	}
	sourceHash, sourceBytes, err := hashTranscriptFile(s.journal.path)
	if err != nil {
		return result, err
	}
	if s.options.MessagesBackupPath != "" {
		if err := backupTranscript(s.journal.path, s.options.MessagesBackupPath, options.BackupMode, options.AppendLimit, s.journal.durability); err != nil {
			return result, fmt.Errorf("backup transcript before compaction: %w", err)
		}
	}
	result.CompactedItems = len(history)
	result.BackupPath = s.options.MessagesBackupPath
	cache := compactionCache{
		SchemaVersion:            1,
		SourceSHA256:             sourceHash,
		SourceBytes:              sourceBytes,
		Items:                    make([]json.RawMessage, len(retained)),
		AssistantFinalItems:      make([]string, 0),
		ActiveTail:               compactedActiveTail,
		ResetLatestResponseUsage: true,
	}
	if len(continuedItems) > 0 {
		cache.ContinuedItems = continuedItems
	}
	for index, item := range retained {
		cache.Items[index] = append(json.RawMessage(nil), item...)
		fields, parseErr := objectFields(item)
		if parseErr != nil {
			continue
		}
		typeRaw, _ := fieldValue(fields, "type")
		itemType, _ := stringValue(typeRaw)
		roleRaw, _ := fieldValue(fields, "role")
		role, _ := stringValue(roleRaw)
		if itemType == "message" && role == "assistant" {
			if _, isFinal := finalAssistant[string(item)]; isFinal {
				cache.AssistantFinalItems = append(cache.AssistantFinalItems, string(item))
			}
		}
	}
	if err := s.writeCompactionCache(cache); err != nil {
		return result, fmt.Errorf("write compaction cache: %w", err)
	}
	activeTailRecords := make([][]byte, len(compactedActiveTail))
	for index, record := range compactedActiveTail {
		activeTailRecords[index] = append([]byte(nil), record...)
	}
	if err := s.journal.ReplaceRecords(activeTailRecords...); err != nil {
		// The cache describes the transcript that was not replaced; drop it so the
		// next read serves the old transcript as it is.
		if removeErr := s.removeCompactionCaches(); removeErr != nil {
			return result, errors.Join(fmt.Errorf("replace transcript during compaction: %w", err), removeErr)
		}
		return result, fmt.Errorf("replace transcript during compaction: %w", err)
	}
	if err := s.removeDerivedFiles(false, false); err != nil {
		return result, err
	}
	result.Compacted = true
	result.RetainedItems = len(retained) + len(activeTailItems)
	return result, nil
}

func (s *TurnStore) preservedResponseTail(responseID string) ([]json.RawMessage, map[string]int, error) {
	activeTail := make([]json.RawMessage, 0)
	excluded := make(map[string]int)
	if responseID == "" {
		return activeTail, excluded, nil
	}
	if err := s.journal.ReadLines(func(line []byte) error {
		fields, err := objectFields(line)
		if err != nil {
			return err
		}
		recordTypeRaw, _ := fieldValue(fields, "type")
		recordType, _ := stringValue(recordTypeRaw)
		if recordType != "response.completed" {
			return nil
		}
		responseRaw, exists := fieldValue(fields, "response")
		if !exists {
			return nil
		}
		responseFields, err := objectFields(responseRaw)
		if err != nil {
			return err
		}
		idRaw, _ := fieldValue(responseFields, "id")
		id, ok := stringValue(idRaw)
		if !ok || id != responseID {
			return nil
		}
		canonical, err := canonicalJSON(line)
		if err != nil {
			return err
		}
		activeTail = append(activeTail, json.RawMessage(canonical))
		items, err := expandTranscriptRecord(line)
		if err != nil {
			return err
		}
		for _, item := range items {
			excluded[string(item)]++
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if len(activeTail) == 0 {
		return nil, nil, errors.New("cannot preserve a response that is missing from messages.jsonl")
	}
	return activeTail, excluded, nil
}

func excludePreservedItems(history [][]byte, continued []bool, excluded map[string]int) ([][]byte, []bool) {
	if len(excluded) == 0 {
		return history, continued
	}
	remaining := make(map[string]int, len(excluded))
	for key, count := range excluded {
		remaining[key] = count
	}
	filtered := make([][]byte, 0, len(history))
	filteredContinued := make([]bool, 0, len(history))
	for index, item := range history {
		key := string(item)
		if remaining[key] > 0 {
			remaining[key]--
			continue
		}
		filtered = append(filtered, item)
		filteredContinued = append(filteredContinued, index < len(continued) && continued[index])
	}
	return filtered, filteredContinued
}

// isUserItem reports whether a history item is a message with role user.
func isUserItem(item []byte) bool {
	fields, err := objectFields(item)
	if err != nil {
		return false
	}
	roleRaw, _ := fieldValue(fields, "role")
	role, _ := stringValue(roleRaw)
	return role == "user"
}

func assistantFinalActiveTailRecord(record []byte) (json.RawMessage, error) {
	fields, err := objectFields(record)
	if err != nil {
		return nil, err
	}
	responseRaw, exists := fieldValue(fields, "response")
	if !exists {
		canonical, err := canonicalJSON(record)
		return json.RawMessage(canonical), err
	}
	responseFields, err := objectFields(responseRaw)
	if err != nil {
		return nil, err
	}
	outputRaw, exists := fieldValue(responseFields, "output")
	if !exists || len(outputRaw) == 0 || outputRaw[0] != '[' {
		canonical, err := canonicalJSON(record)
		return json.RawMessage(canonical), err
	}
	functionCalls := make([]json.RawMessage, 0)
	var assistantFinal json.RawMessage
	if err := arrayValues(outputRaw, func(item []byte) error {
		itemFields, err := objectFields(item)
		if err != nil {
			return nil
		}
		itemTypeRaw, _ := fieldValue(itemFields, "type")
		itemType, _ := stringValue(itemTypeRaw)
		itemRoleRaw, _ := fieldValue(itemFields, "role")
		itemRole, _ := stringValue(itemRoleRaw)
		if itemType == "function_call" {
			functionCalls = append(functionCalls, append(json.RawMessage(nil), item...))
		}
		if itemType == "message" && itemRole == "assistant" {
			assistantFinal = append(json.RawMessage(nil), item...)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	selected := functionCalls
	if len(selected) == 0 && len(assistantFinal) > 0 {
		selected = []json.RawMessage{assistantFinal}
	}
	encodedOutput, err := json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	updatedResponse := make([]jsonField, 0, len(responseFields))
	for _, field := range responseFields {
		switch field.key {
		case "output":
			updatedResponse = append(updatedResponse, jsonField{key: field.key, value: encodedOutput})
		case "output_text":
			continue
		default:
			updatedResponse = append(updatedResponse, field)
		}
	}
	updatedRecord := make([]jsonField, 0, len(fields))
	for _, field := range fields {
		if field.key == "response" {
			updatedRecord = append(updatedRecord, jsonField{key: field.key, value: encodeJSONObject(updatedResponse)})
		} else {
			updatedRecord = append(updatedRecord, field)
		}
	}
	canonical, err := canonicalJSON(encodeJSONObject(updatedRecord))
	return json.RawMessage(canonical), err
}

// assistantFinalItems returns the assistant messages that end a turn, from the
// previous compaction cache (nil when there is none) and the journal.
func (s *TurnStore) assistantFinalItems(previous *compactionCache) (map[string]struct{}, error) {
	finalByTurn := make(map[int]string)
	turnIndex := -1
	if previous != nil {
		previousFinal := make(map[string]struct{}, len(previous.AssistantFinalItems))
		for _, item := range previous.AssistantFinalItems {
			previousFinal[item] = struct{}{}
		}
		for _, item := range previous.Items {
			fields, err := objectFields(item)
			if err != nil {
				continue
			}
			roleRaw, _ := fieldValue(fields, "role")
			role, _ := stringValue(roleRaw)
			if role == "user" {
				turnIndex++
			}
			if turnIndex < 0 {
				turnIndex = 0
			}
			typeRaw, _ := fieldValue(fields, "type")
			itemType, _ := stringValue(typeRaw)
			if itemType == "message" && role == "assistant" {
				if _, ok := previousFinal[string(item)]; ok {
					finalByTurn[turnIndex] = string(item)
				}
			}
		}
	}
	err := s.journal.ReadLines(func(line []byte) error {
		fields, err := objectFields(line)
		if err != nil {
			return err
		}
		roleRaw, _ := fieldValue(fields, "role")
		role, _ := stringValue(roleRaw)
		if role == "user" {
			turnIndex++
			return nil
		}
		typeRaw, _ := fieldValue(fields, "type")
		recordType, _ := stringValue(typeRaw)
		if recordType != "response.completed" {
			return nil
		}
		responseRaw, ok := fieldValue(fields, "response")
		if !ok {
			return nil
		}
		// objectFields validated the whole record, so every value inside it is
		// valid JSON and the *Valid variants skip validating the same bytes again.
		responseFields, err := objectFieldsValid(bytes.TrimSpace(responseRaw))
		if err != nil {
			return err
		}
		outputRaw, ok := fieldValue(responseFields, "output")
		if !ok {
			return nil
		}
		outputRaw = bytes.TrimSpace(outputRaw)
		if len(outputRaw) == 0 || outputRaw[0] != '[' {
			return errors.New("expected a JSON array")
		}
		hasFunctionCall := false
		var assistantFinal []byte
		if err := arrayValuesValid(outputRaw, func(item []byte) error {
			itemFields, err := objectFieldsValid(item)
			if err != nil {
				return err
			}
			itemTypeRaw, _ := fieldValue(itemFields, "type")
			itemType, _ := stringValue(itemTypeRaw)
			itemRoleRaw, _ := fieldValue(itemFields, "role")
			itemRole, _ := stringValue(itemRoleRaw)
			if itemType == "function_call" {
				hasFunctionCall = true
			}
			if itemType == "message" && itemRole == "assistant" {
				assistantFinal, err = canonicalJSONValid(item)
				return err
			}
			return nil
		}); err != nil {
			return err
		}
		turn := turnIndex
		if turn < 0 {
			turn = 0
		}
		if hasFunctionCall {
			delete(finalByTurn, turn)
			return nil
		}
		statusRaw, _ := fieldValue(responseFields, "status")
		status, _ := stringValue(statusRaw)
		if status == "completed" && len(assistantFinal) > 0 {
			finalByTurn[turn] = string(assistantFinal)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	finals := make(map[string]struct{}, len(finalByTurn))
	for _, item := range finalByTurn {
		finals[item] = struct{}{}
	}
	return finals, nil
}

// keptToolPairs picks the tool call/result pairs compaction keeps. The count is
// compaction.keepbatch.tools in total across every kept turn, newest first, as
// the TypeScript backend does; it is not a count per turn. A pair is a call
// whose output is also in the kept turns, and only assistant_final keeps any.
func keptToolPairs(turns [][][]byte, options CompactionOptions) map[string]bool {
	keepPairs := make(map[string]bool)
	if options.TurnType != "assistant_final" || options.KeepToolPairs <= 0 {
		return keepPairs
	}
	callIDs := make([]string, 0)
	callSeen := make(map[string]bool)
	outputIDs := make(map[string]bool)
	for _, turn := range turns {
		for _, item := range turn {
			fields, err := objectFields(item)
			if err != nil {
				continue
			}
			typeRaw, _ := fieldValue(fields, "type")
			itemType, _ := stringValue(typeRaw)
			if itemType != "function_call" && itemType != "function_call_output" {
				continue
			}
			id := rawCallID(fields)
			if id == "" {
				continue
			}
			if itemType == "function_call" && !callSeen[id] {
				callIDs = append(callIDs, id)
				callSeen[id] = true
			}
			if itemType == "function_call_output" {
				outputIDs[id] = true
			}
		}
	}
	for i := len(callIDs) - 1; i >= 0 && len(keepPairs) < options.KeepToolPairs; i-- {
		if outputIDs[callIDs[i]] {
			keepPairs[callIDs[i]] = true
		}
	}
	return keepPairs
}

func retainCompactedTurn(turn [][]byte, options CompactionOptions, assistantFinal map[string]struct{}, keepPairs map[string]bool) [][]byte {
	if len(turn) == 0 {
		return nil
	}
	retained := make([][]byte, 0, len(turn))
	for _, item := range turn {
		fields, err := objectFields(item)
		if err != nil {
			continue
		}
		typeRaw, _ := fieldValue(fields, "type")
		itemType, _ := stringValue(typeRaw)
		roleRaw, _ := fieldValue(fields, "role")
		role, _ := stringValue(roleRaw)
		if role == "user" {
			retained = append(retained, item)
			continue
		}
		if itemType == "function_call" || itemType == "function_call_output" {
			if keepPairs[rawCallID(fields)] {
				retained = append(retained, item)
			}
			continue
		}
		if options.TurnType == "assistant_final" {
			if _, isFinal := assistantFinal[string(item)]; isFinal {
				retained = append(retained, item)
			}
			continue
		}
		if role == "assistant" || itemType == "reasoning" {
			retained = append(retained, item)
		}
	}
	return retained
}

func rawCallID(fields []jsonField) string {
	raw, _ := fieldValue(fields, "call_id")
	id, _ := stringValue(raw)
	return id
}

func compactedInputBytes(items [][]byte) int {
	total := 2
	if len(items) > 1 {
		total += len(items) - 1
	}
	for _, item := range items {
		total += len(item)
	}
	return total
}

// backupTurnStart is where a user turn starts in a backup: the file (0 is the existing
// backup, 1 the live transcript) and the byte offset of its record.
type backupTurnStart struct {
	file   int
	offset int64
}

// backupTurnWindow remembers the starts of the newest limit user turns of the files it
// scans, in a ring of limit entries, so the memory does not grow with the backup.
type backupTurnWindow struct {
	starts []backupTurnStart
	turns  int
}

func newBackupTurnWindow(limit int) *backupTurnWindow {
	return &backupTurnWindow{starts: make([]backupTurnStart, limit)}
}

// cut returns the start of the oldest turn to keep when the files hold more turns than
// the window; ok is false when they hold no more, and nothing is cut.
func (w *backupTurnWindow) cut() (start backupTurnStart, ok bool) {
	if w.turns <= len(w.starts) {
		return backupTurnStart{}, false
	}
	return w.starts[w.turns%len(w.starts)], true
}

// backupLine is a line of the backup as it will be written: the existing backup followed
// by the transcript. A line that does not end with a newline is joined to the first line of
// whatever follows it, so it is only known once that follows. file and offset say where the
// line starts (file 0 is the existing backup, 1 the transcript); path is the file it is
// reported in, and joinedTo is the transcript when the line is a backup's unterminated last
// line joined to the transcript's first line.
type backupLine struct {
	data     []byte
	file     int
	offset   int64
	path     string
	joinedTo string
}

// observe checks that the line is a JSON object (a blank one is not a record) and, when it
// has role user and is not marked as steering (SessionInputSteerField), records in window
// that a turn starts at the line.
func (l backupLine) observe(window *backupTurnWindow) error {
	trimmed := bytes.TrimSpace(l.data)
	if len(trimmed) == 0 {
		return nil
	}
	fields, err := objectFields(trimmed)
	if err != nil {
		if l.joinedTo != "" {
			return fmt.Errorf("%s: record at byte %d has no newline, so it is joined to the first line of %s, which is not a JSON object: %w", l.path, l.offset, l.joinedTo, err)
		}
		return fmt.Errorf("%s: record at byte %d is not a JSON object: %w", l.path, l.offset, err)
	}
	if roleRaw, ok := fieldValue(fields, "role"); ok {
		steered, marked := fieldValue(fields, SessionInputSteerField)
		if role, _ := stringValue(roleRaw); role == "user" && !(marked && string(steered) == "true") {
			window.starts[window.turns%len(window.starts)] = backupTurnStart{file: l.file, offset: l.offset}
			window.turns++
		}
	}
	return nil
}

// scanBackupTurns reads the transcript file at path as it is on disk and records in window
// where each turn starts. A turn starts at a record with role user that is not marked as
// steering (SessionInputSteerField): no compaction overlay, and each record of an input
// batch counts, where the web UI's backup reader merges a batch into one turn. A steering
// record (a steering message or a stop hook's message) continues the turn before it, as
// CompactHistory does not start a turn at it; the session's turn numbers and the web UI
// still count it as a turn of its own. A missing file is empty. A line that is not a JSON
// object is an error that names the file and the byte offset.
//
// The lines are those of the backup that will be written, so the file's last line, when it
// does not end with a newline, is not read yet: it is returned as rest, for the caller to
// read once it knows what is joined to it. carry is such a line of the file scanned before:
// it is joined to the first line of this file, and that one line is read, and counted, as
// it will be in the backup. When the file has no line, rest is carry. size is the size of
// what was read.
func scanBackupTurns(path string, file int, window *backupTurnWindow, carry *backupLine) (size int64, rest *backupLine, err error) {
	input, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, carry, nil
		}
		return 0, nil, err
	}
	defer input.Close()
	reader := bufio.NewReader(input)
	rest = carry
	for first := true; ; first = false {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			current := backupLine{data: line, file: file, offset: size, path: path}
			if first && carry != nil {
				current = *carry
				current.data = append(append([]byte(nil), carry.data...), line...)
				current.joinedTo = path
			}
			size += int64(len(line))
			if readErr == io.EOF {
				// No newline: joined to what follows.
				return size, &current, nil
			}
			if err := current.observe(window); err != nil {
				return 0, nil, err
			}
			if first {
				rest = nil
			}
		}
		if readErr == io.EOF {
			return size, rest, nil
		}
		if readErr != nil {
			return 0, nil, readErr
		}
	}
}

// copyBackupPart copies the bytes of the file at path from offset to its end, or, when
// size is not negative, up to size bytes of it. A missing file reports exists false.
func copyBackupPart(destination *os.File, path string, offset, size int64) (exists bool, err error) {
	input, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer input.Close()
	var reader io.Reader = input
	if offset > 0 || size >= 0 {
		if size < 0 {
			size = math.MaxInt64 - offset
		}
		reader = io.NewSectionReader(input, offset, size)
	}
	if _, err := io.Copy(destination, reader); err != nil {
		return true, err
	}
	return true, nil
}

// backupTranscript writes the transcript backup before a compaction: the live transcript
// at source replaces destination (mode replace), or is added to the end of it (mode append).
//
// With mode append and appendLimit above 0 the backup is checked and then cut to the newest
// appendLimit user turns, each turn being counted as scanBackupTurns says, on the lines of
// the bytes that would be written: the existing backup followed by the transcript, as they
// are. An existing backup whose last line has no newline is joined to the first line of the
// transcript, as it always was, and that joined line is one record: it must be a JSON object
// (a blank part is fine), and it starts a turn, at the start of the backup's last line, when
// it is a user record. A record that is not a JSON object stops the backup before anything
// is written, wherever it is, even in a part the cut would drop: the error names the file and
// byte offset, and destination is left as it was. The cut is at the record that starts the
// oldest turn kept, so only whole turns go, steering records stay with the turn before them,
// and the records in front of the first turn go with the turns dropped. When the files hold no
// more than appendLimit turns nothing is cut, whatever comes before the first turn. No
// separator is added. appendLimit 0 appends the bytes as they are and checks nothing.
//
// The result goes to a temporary file in the directory of destination and replaces it by
// rename, with the durability the journal has. A source that does not exist leaves
// destination alone.
func backupTranscript(source, destination, mode string, appendLimit int, durability DurabilityMode) error {
	if source == "" || destination == "" {
		return errors.New("transcript backup paths are required")
	}
	// The parts of the files that go to the backup: from an offset, for a length (-1 is to
	// the end of the file).
	var existingFrom, sourceFrom int64
	existingLength, sourceLength := int64(-1), int64(-1)
	if mode == "append" && appendLimit > 0 {
		window := newBackupTurnWindow(appendLimit)
		existingSize, tail, err := scanBackupTurns(destination, 0, window, nil)
		if err != nil {
			return err
		}
		sourceSize, last, err := scanBackupTurns(source, 1, window, tail)
		if err != nil {
			return err
		}
		// The last line of the backup written, which has no newline, is read now.
		if last != nil {
			if err := last.observe(window); err != nil {
				return err
			}
		}
		// What was scanned is what is copied, even if the file grows meanwhile.
		existingLength, sourceLength = existingSize, sourceSize
		if start, trimmed := window.cut(); trimmed {
			if start.file == 0 {
				existingFrom = start.offset
				existingLength = existingSize - existingFrom
			} else {
				existingLength = 0
				sourceFrom, sourceLength = start.offset, sourceSize-start.offset
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".compact-backup-*")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	if mode == "append" && existingLength != 0 {
		if _, err := copyBackupPart(temporary, destination, existingFrom, existingLength); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	exists, err := copyBackupPart(temporary, source, sourceFrom, sourceLength)
	if err != nil || !exists {
		_ = temporary.Close()
		return err
	}
	if durability != DurabilityProcess {
		if err := temporary.Sync(); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return err
	}
	if durability != DurabilityProcess {
		return syncDirectory(filepath.Dir(destination))
	}
	return nil
}

// Reset deletes the history and everything derived from it. A *PublishedError (use errors.As)
// from it means the history is in fact gone: only the durability of the publish is unconfirmed.
// Publishing the empty transcript
// is the commit point and comes last: every file the reset removes goes first, and each removal
// ignores a file that is already gone, so a failure, or a crash, before the commit leaves the
// history in place with only caches missing, and running Reset again finishes the job. The
// other order (empty transcript first) left a failure between the two with an empty history and
// leftover caches that nothing retried: a caller that treats an empty transcript as nothing to
// reset never came back to them.
//
// What a failed Reset can have removed before the commit is data the reset was asked to delete
// anyway (the caches, the transcript backup, the deleted-history archive); the history itself
// is untouched until the one atomic publish. The compaction cache goes first: it can hold the
// retained turns of a compacted history, and once the transcript is published empty a cache that
// survived would still be served as history.
func (s *TurnStore) Reset() error {
	if s == nil || s.journal == nil {
		return errors.New("turn store is not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovering {
		return errors.New("turn store recovery is in progress")
	}
	if len(s.activeTurns) != 0 {
		return errors.New("cannot reset while response turns are active")
	}
	if err := s.removeCompactionCaches(); err != nil {
		return err
	}
	if err := s.removeDerivedFiles(true, false); err != nil {
		return err
	}
	if err := os.RemoveAll(s.journal.path + ".deleted-history"); err != nil {
		return fmt.Errorf("remove deleted-history archive: %w", err)
	}
	publishErr := s.journal.ReplaceRecords()
	var published *PublishedError
	if publishErr != nil && !errors.As(publishErr, &published) {
		return publishErr
	}
	// A reader outside the store lock (a usage scan for profile info) may have written a cache
	// for the old transcript between the removals and the publish. Such a cache no longer
	// matches the transcript and is never used; this only drops the file, so its failure is
	// not the reset's.
	s.invalidateCursorCache()
	_ = s.removeDerivedFiles(true, false)
	if published != nil {
		// The transcript is replaced and everything derived from it is gone, but the directory
		// sync failed, so a crash could bring the old file back. The error says so (it still
		// is a *PublishedError for errors.As): the caller must treat the history as reset, and
		// a retry, which finds it empty, is how a crash that undid it is noticed later.
		return fmt.Errorf("reset applied but not confirmed durable: %w", publishErr)
	}
	return nil
}

// removeDerivedFiles deletes the caches a transcript rewrite invalidates.
// keepStateContext leaves the state context snapshot alone: it holds application
// state read at the last compaction, not anything anchored to message ids.
func (s *TurnStore) removeDerivedFiles(reset, keepStateContext bool) error {
	s.invalidateCursorCache()
	if reset {
		defer s.compactionGeneration.Add(1)
	}
	// The run-error cache goes with the rest: its entries are anchored to session
	// turn numbers and message ids that a rewrite of the transcript re-bases.
	paths := []string{s.options.UsageCachePath, s.options.CursorCachePath, s.options.RunErrorCachePath}
	if !keepStateContext {
		paths = append(paths, s.options.StateContextPath)
	}
	if reset {
		paths = append(paths, s.options.MessagesBackupPath, s.options.CompactionCachePath, s.options.CompactionCachePath+".backup")
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove session cache %q: %w", path, err)
		}
	}
	return nil
}

// removeCompactionCaches deletes the compaction cache and its backup. A caller
// that changes the transcript must do it before publishing the new transcript
// when the cache would otherwise be served for the old one.
func (s *TurnStore) removeCompactionCaches() error {
	defer s.compactionGeneration.Add(1)
	if s.options.CompactionCachePath == "" {
		return nil
	}
	for _, path := range []string{s.options.CompactionCachePath, s.options.CompactionCachePath + ".backup"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove compaction cache %q: %w", path, err)
		}
	}
	return nil
}

func (s *TurnStore) readCompactionCache() (*compactionCache, error) {
	if s.options.CompactionCachePath == "" {
		return nil, nil
	}
	for _, path := range []string{s.options.CompactionCachePath, s.options.CompactionCachePath + ".backup"} {
		encoded, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var cache compactionCache
		if json.Unmarshal(encoded, &cache) != nil || cache.SchemaVersion != 1 || len(cache.SourceSHA256) != 64 || cache.SourceBytes < 0 {
			continue
		}
		if _, err := hex.DecodeString(cache.SourceSHA256); err != nil {
			continue
		}
		valid := true
		for _, item := range cache.Items {
			// json.Unmarshal validated the whole file, so each item is valid JSON.
			fields, err := objectFieldsValid(bytes.TrimSpace(item))
			if err != nil || validateInputFields(fields) != nil {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		for _, record := range cache.ActiveTail {
			if _, err := expandTranscriptRecord(record); err != nil {
				valid = false
				break
			}
		}
		if valid {
			return &cache, nil
		}
	}
	return nil, nil
}

func (s *TurnStore) writeCompactionCache(cache compactionCache) error {
	if s.options.CompactionCachePath == "" {
		return errors.New("compaction cache path is required")
	}
	// Bumped even when a write fails half way, so no memo outlives a changed file.
	defer s.compactionGeneration.Add(1)
	encoded, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	for _, path := range []string{s.options.CompactionCachePath + ".backup", s.options.CompactionCachePath} {
		if err := writeCompactionJSONAtomic(path, encoded, s.journal.durability); err != nil {
			return err
		}
	}
	return nil
}

func writeCompactionJSONAtomic(path string, encoded []byte, durability DurabilityMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".cache-write-*")
	if err != nil {
		return err
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if durability != DurabilityProcess {
		if err := temporary.Sync(); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if durability != DurabilityProcess {
		return syncDirectory(filepath.Dir(path))
	}
	return nil
}

func hashTranscriptFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		empty := sha256.Sum256(nil)
		return hex.EncodeToString(empty[:]), 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	return hashReader(file)
}

// cacheSourceIsTranscript reports whether the transcript still holds the bytes the
// compaction cache was built from. That is true only between writing the cache and
// replacing the transcript; after a finished compaction the transcript is the
// shorter active tail. A cache that recorded its source size is compared by size
// first, because a different size cannot hash equal, so the whole-file hash runs
// only when the sizes agree. A cache written before the size was recorded always hashes.
func (s *TurnStore) cacheSourceIsTranscript(cache *compactionCache) (bool, error) {
	if cache.SourceBytes > 0 {
		size, err := s.journal.byteSize()
		if err != nil {
			return false, err
		}
		if size != cache.SourceBytes {
			return false, nil
		}
	}
	hash, _, err := hashTranscriptFile(s.journal.path)
	if err != nil {
		return false, err
	}
	return hash == cache.SourceSHA256, nil
}
