package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RecoveryReport summarizes startup repair and staged-turn recovery.
type RecoveryReport struct {
	RepairedMessageBytes int64    `json:"repaired_message_bytes"`
	InspectedTurns       int      `json:"inspected_turns"`
	RetainedTurns        int      `json:"retained_turns"`
	CommittedTurns       int      `json:"committed_turns"`
	RemovedTurns         int      `json:"removed_turns"`
	Errors               []string `json:"errors"`
}

type turnRecoveryOutcome string

const (
	turnRetained  turnRecoveryOutcome = "retained"
	turnCommitted turnRecoveryOutcome = "committed"
	turnRemoved   turnRecoveryOutcome = "removed"
)

// Recover repairs the transcript tail, removes partial item/index records, and
// finishes commits that already have a complete final.jsonl. Unfinished turns
// without a final record are retained for diagnostics or a future resume path.
func (s *TurnStore) Recover() (RecoveryReport, error) {
	if s == nil || s.journal == nil {
		return RecoveryReport{}, errors.New("turn store is not initialized")
	}
	s.mu.Lock()
	if len(s.activeTurns) > 0 {
		s.mu.Unlock()
		return RecoveryReport{}, errors.New("close active turns before recovery")
	}
	if s.recovering {
		s.mu.Unlock()
		return RecoveryReport{}, errors.New("turn store recovery is already in progress")
	}
	s.recovering = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.recovering = false
		s.mu.Unlock()
	}()

	repairedBytes, err := s.journal.RepairTail()
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{
		RepairedMessageBytes: repairedBytes,
		Errors:               make([]string, 0),
	}
	entries, err := os.ReadDir(s.stagingDir)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return RecoveryReport{}, fmt.Errorf("read staging directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		report.InspectedTurns++
		outcome, recoverErr := s.recoverTurn(filepath.Join(s.stagingDir, entry.Name()))
		if recoverErr != nil {
			report.RetainedTurns++
			report.Errors = append(report.Errors, entry.Name()+": "+recoverErr.Error())
			continue
		}
		switch outcome {
		case turnCommitted:
			report.CommittedTurns++
			report.RemovedTurns++
		case turnRemoved:
			report.RemovedTurns++
		default:
			report.RetainedTurns++
		}
	}
	return report, nil
}

func (s *TurnStore) recoverTurn(turnDir string) (turnRecoveryOutcome, error) {
	manifestPath := filepath.Join(turnDir, manifestFilename)
	manifest, err := readTurnManifest(manifestPath)
	if err != nil {
		return turnRetained, err
	}
	itemsPath := filepath.Join(turnDir, itemsFilename)
	indexPath := filepath.Join(turnDir, indexFilename)
	if err := repairJSONLPath(itemsPath); err != nil {
		return turnRetained, fmt.Errorf("repair staged items: %w", err)
	}
	if err := repairJSONLPath(indexPath); err != nil {
		return turnRetained, fmt.Errorf("repair staged item index: %w", err)
	}

	finalPath := filepath.Join(turnDir, finalFilename)
	finalExists, err := fileExists(finalPath)
	if err != nil {
		return turnRetained, fmt.Errorf("inspect staged final record: %w", err)
	}
	if finalExists {
		terminated, err := fileEndsWithNewline(finalPath)
		if err != nil {
			return turnRetained, fmt.Errorf("inspect staged final record terminator: %w", err)
		}
		if !terminated {
			return turnRetained, errors.New("staged final record is incomplete")
		}
		actualHash, err := hashFile(finalPath)
		if err != nil {
			return turnRetained, fmt.Errorf("hash staged final record: %w", err)
		}
		if manifest.CanonicalLineSHA256 != "" && manifest.CanonicalLineSHA256 != actualHash {
			return turnRetained, errors.New("staged final record hash does not match manifest")
		}
		if _, err := s.journal.appendFileIfAbsent(finalPath); err != nil {
			return turnRetained, fmt.Errorf("append recovered final record: %w", err)
		}
		manifest.CanonicalLineSHA256 = actualHash
		manifest.Status = "committed"
		if err := writeRecoveryManifest(manifestPath, manifest, s.journal.durability == DurabilityStrict); err != nil {
			return turnRetained, fmt.Errorf("persist recovered turn manifest: %w", err)
		}
		if err := os.RemoveAll(turnDir); err != nil {
			return turnRetained, fmt.Errorf("remove recovered turn directory: %w", err)
		}
		return turnCommitted, nil
	}

	if manifest.Status == "committed" && manifest.CanonicalLineSHA256 != "" {
		present, err := s.journal.containsHashText(manifest.CanonicalLineSHA256)
		if err != nil {
			return turnRetained, fmt.Errorf("verify committed transcript record: %w", err)
		}
		if present {
			if err := os.RemoveAll(turnDir); err != nil {
				return turnRetained, fmt.Errorf("remove committed turn directory: %w", err)
			}
			return turnRemoved, nil
		}
		return turnRetained, errors.New("committed manifest has no matching messages.jsonl record")
	}
	return turnRetained, nil
}

func readTurnManifest(path string) (TurnManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TurnManifest{}, fmt.Errorf("read turn manifest: %w", err)
	}
	fields, err := objectFields(data)
	if err != nil {
		return TurnManifest{}, fmt.Errorf("parse turn manifest: %w", err)
	}
	schemaRaw, hasSchema := fieldValue(fields, "schema_version")
	schemaVersion, schemaValid := safeInteger(schemaRaw, false)
	turnIDRaw, hasTurnID := fieldValue(fields, "turn_id")
	turnID, turnIDValid := stringValue(turnIDRaw)
	statusRaw, hasStatus := fieldValue(fields, "status")
	status, statusValid := stringValue(statusRaw)
	itemCountRaw, hasItemCount := fieldValue(fields, "item_count")
	itemCount, itemCountValid := safeInteger(itemCountRaw, false)
	nextSequenceRaw, hasNextSequence := fieldValue(fields, "next_sequence")
	nextSequence, nextSequenceValid := safeInteger(nextSequenceRaw, false)
	if !hasSchema || !schemaValid || schemaVersion != 1 || !hasTurnID || !turnIDValid ||
		!hasStatus || !statusValid || !hasItemCount || !itemCountValid ||
		!hasNextSequence || !nextSequenceValid || itemCount > int64(int(^uint(0)>>1)) {
		return TurnManifest{}, errors.New("invalid turn manifest")
	}
	switch status {
	case "streaming", "ready", "committing", "committed", "failed":
	default:
		return TurnManifest{}, fmt.Errorf("invalid turn status in manifest %q", status)
	}
	manifest := TurnManifest{
		SchemaVersion: int(schemaVersion),
		TurnID:        turnID,
		Status:        status,
		ItemCount:     int(itemCount),
		NextSequence:  nextSequence,
	}
	if responseIDRaw, exists := fieldValue(fields, "response_id"); exists {
		responseID, valid := stringValue(responseIDRaw)
		if !valid {
			return TurnManifest{}, errors.New("invalid response_id in manifest")
		}
		manifest.ResponseID = responseID
	}
	if hashRaw, exists := fieldValue(fields, "canonical_line_sha256"); exists {
		lineHash, valid := stringValue(hashRaw)
		if !valid || len(lineHash) != sha256.Size*2 || strings.ToLower(lineHash) != lineHash {
			return TurnManifest{}, errors.New("invalid canonical line hash in manifest")
		}
		if _, err := hex.DecodeString(lineHash); err != nil {
			return TurnManifest{}, errors.New("invalid canonical line hash in manifest")
		}
		manifest.CanonicalLineSHA256 = lineHash
	}
	return manifest, nil
}

func writeRecoveryManifest(path string, manifest TurnManifest, sync bool) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, append(data, '\n'), sync)
}

func repairJSONLPath(path string) (result error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); result == nil && closeErr != nil {
			result = closeErr
		}
	}()
	if _, err := repairTrailingPartialLine(file); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func fileEndsWithNewline(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	var lastByte [1]byte
	if _, err := file.ReadAt(lastByte[:], info.Size()-1); err != nil {
		return false, err
	}
	return lastByte[0] == '\n', nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash, _, err := hashReader(file)
	return hash, err
}

// hashReader returns the hex SHA-256 of everything the reader holds, and how many bytes that was.
func hashReader(reader io.Reader) (string, int64, error) {
	digest := sha256.New()
	size, err := io.CopyBuffer(digest, reader, make([]byte, 64*1024))
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}
