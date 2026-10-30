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
}

type CompactionOptions struct {
	TurnType           string
	KeepTurns          int
	KeepToolPairs      int
	BackupMode         string
	BackupMinTurns     int
	PreserveResponseID string
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
	if s.recovering {
		return result, errors.New("turn store recovery is in progress")
	}
	if len(s.activeTurns) != 0 {
		return result, errors.New("cannot compact while a response turn is active")
	}

	// The compaction cache is read once for the history and for the
	// assistant-final items. The store lock is held and no turn is active, so
	// nothing rewrites the cache between the two uses.
	previousCache, err := s.readCompactionCache()
	if err != nil {
		return result, fmt.Errorf("read request history for compaction: %w", err)
	}
	history, err := s.readInputItemsWithCache(nil, previousCache, nil)
	if err != nil {
		return result, fmt.Errorf("read request history for compaction: %w", err)
	}
	activeTail, excluded, err := s.preservedResponseTail(options.PreserveResponseID)
	if err != nil {
		return result, err
	}
	compactableHistory := excludePreservedItems(history, excluded)
	finalAssistant, err := s.assistantFinalItems(previousCache)
	if err != nil {
		return result, fmt.Errorf("read assistant final items for compaction: %w", err)
	}
	result.CompactedItems = len(history)
	result.RetainedItems = len(history)
	result.BackupPath = s.options.MessagesBackupPath

	turns := make([][][]byte, 0)
	for _, item := range compactableHistory {
		fields, err := objectFields(item)
		if err != nil {
			return result, err
		}
		roleRaw, _ := fieldValue(fields, "role")
		role, _ := stringValue(roleRaw)
		if role == "user" || len(turns) == 0 {
			turns = append(turns, make([][]byte, 0))
		}
		turns[len(turns)-1] = append(turns[len(turns)-1], item)
	}
	selected := turns
	if options.KeepTurns == 0 {
		selected = nil
	} else if len(selected) > options.KeepTurns {
		selected = selected[len(selected)-options.KeepTurns:]
	}

	retained := make([][]byte, 0, len(history))
	keepPairs := keptToolPairs(selected, options)
	for _, turn := range selected {
		retained = append(retained, retainCompactedTurn(turn, options, finalAssistant, keepPairs)...)
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
		if err := backupTranscript(s.journal.path, s.options.MessagesBackupPath, options.BackupMode, options.BackupMinTurns, s.journal.durability); err != nil {
			return result, fmt.Errorf("backup transcript before compaction: %w", err)
		}
	}
	cache := compactionCache{
		SchemaVersion:            1,
		SourceSHA256:             sourceHash,
		SourceBytes:              sourceBytes,
		Items:                    make([]json.RawMessage, len(retained)),
		AssistantFinalItems:      make([]string, 0),
		ActiveTail:               compactedActiveTail,
		ResetLatestResponseUsage: true,
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

func excludePreservedItems(history [][]byte, excluded map[string]int) [][]byte {
	if len(excluded) == 0 {
		return history
	}
	remaining := make(map[string]int, len(excluded))
	for key, count := range excluded {
		remaining[key] = count
	}
	filtered := make([][]byte, 0, len(history))
	for _, item := range history {
		key := string(item)
		if remaining[key] > 0 {
			remaining[key]--
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
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

// countUserTurns counts the records of a transcript file that are user messages,
// the same role == "user" rule CompactHistory uses to split turns. It reads the
// file as it is on disk: no compaction overlay, and each record of an input
// batch counts, where the web UI's backup reader merges a batch into one turn.
func countUserTurns(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	turns := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			fields, err := objectFields(line)
			if err != nil {
				return 0, err
			}
			if roleRaw, ok := fieldValue(fields, "role"); ok {
				if role, _ := stringValue(roleRaw); role == "user" {
					turns++
				}
			}
		}
		if readErr == io.EOF {
			return turns, nil
		}
		if readErr != nil {
			return 0, readErr
		}
	}
}

// keepsFullerBackup reports whether a replace backup must stay as it is: the
// existing one holds at least minTurns user turns and the new snapshot fewer.
// A missing or unreadable file on either side never blocks the overwrite.
func keepsFullerBackup(source, destination string, minTurns int) bool {
	if minTurns <= 0 {
		return false
	}
	existing, err := countUserTurns(destination)
	if err != nil || existing < minTurns {
		return false
	}
	snapshot, err := countUserTurns(source)
	return err == nil && snapshot < minTurns
}

func backupTranscript(source, destination, mode string, minTurns int, durability DurabilityMode) error {
	if source == "" || destination == "" {
		return errors.New("transcript backup paths are required")
	}
	if mode == "replace" && keepsFullerBackup(source, destination, minTurns) {
		return nil
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
	if mode == "append" {
		if previous, openErr := os.Open(destination); openErr == nil {
			if _, err := io.Copy(temporary, previous); err != nil {
				_ = previous.Close()
				_ = temporary.Close()
				return err
			}
			if err := previous.Close(); err != nil {
				_ = temporary.Close()
				return err
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			_ = temporary.Close()
			return openErr
		}
	}
	input, err := os.Open(source)
	if err != nil {
		_ = temporary.Close()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	_, copyErr := io.Copy(temporary, input)
	closeErr := input.Close()
	if copyErr != nil {
		_ = temporary.Close()
		return copyErr
	}
	if closeErr != nil {
		_ = temporary.Close()
		return closeErr
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
	// The cache goes first: once the transcript is published empty, a cache that
	// survived would still be served as history.
	if err := s.removeCompactionCaches(); err != nil {
		return err
	}
	if err := s.journal.ReplaceRecords(); err != nil {
		return err
	}
	if err := s.removeDerivedFiles(true, false); err != nil {
		return err
	}
	if err := os.RemoveAll(s.journal.path + ".deleted-history"); err != nil {
		return fmt.Errorf("remove deleted-history archive: %w", err)
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
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
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
