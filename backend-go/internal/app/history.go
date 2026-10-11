package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/sessionfs"
	"example.com/agent-harness/backend-go/internal/transcript"
	"example.com/agent-harness/backend-go/internal/webui"
)

// historyReference points a snapshot at its history block. ETag is null when
// the session has no compaction backup, exactly as the TypeScript view reports.
type historyReference struct {
	ETag  *string `json:"etag"`
	Count int     `json:"count"`
}

type historyBlock struct {
	ETag     string
	Messages []webui.Message
}

func (r *Resolver) ReadSessionHistory(ctx context.Context, reference, profile, ifNoneMatch string) (httpapi.CatalogResponse, error) {
	if ctx == nil {
		return httpapi.CatalogResponse{}, errors.New("session history context is required")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.CatalogResponse{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.CatalogResponse{}, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return httpapi.CatalogResponse{}, mapSessionError(err)
	}
	block, err := r.readHistoryBlock(ctx, config)
	if err != nil {
		return httpapi.CatalogResponse{}, err
	}
	if block.ETag == "" {
		body, err := json.Marshal(struct {
			Messages []webui.Message `json:"messages"`
		}{Messages: []webui.Message{}})
		return httpapi.CatalogResponse{Body: body}, err
	}
	etag := `"` + block.ETag + `"`
	if httpapi.ETagMatches(ifNoneMatch, etag) {
		return httpapi.CatalogResponse{ETag: etag, NotModified: true}, nil
	}
	messages := webui.CapMessages(block.Messages, r.catalog.MaxInlinePartBytes, nil)
	if messages == nil {
		messages = []webui.Message{}
	}
	body, err := json.Marshal(struct {
		Messages []webui.Message `json:"messages"`
	}{Messages: messages})
	if err != nil {
		return httpapi.CatalogResponse{}, fmt.Errorf("encode session history: %w", err)
	}
	return httpapi.CatalogResponse{Body: body, ETag: etag}, nil
}

func (r *Resolver) ReadSessionMessage(ctx context.Context, reference, profile, messageID string, cursor *int64) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("session message context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return nil, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return nil, mapSessionError(err)
	}
	if cursor == nil {
		block, err := r.readHistoryBlock(ctx, config)
		if err != nil {
			return nil, err
		}
		for _, message := range block.Messages {
			if message.ID == messageID {
				return json.Marshal(message)
			}
		}
		return nil, &httpapi.StatusError{Code: 404, Message: "message not found"}
	}

	records, err := r.readTurnRecords(ctx, config, location.Reference, *cursor)
	if err != nil {
		return nil, err
	}
	messages, err := webui.NormalizeTranscript(records)
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if message.ID == messageID {
			if webui.HasInlineImages(message) {
				// The whole message replaces the capped one in the browser, whose inline
				// images are fetched by this cursor.
				message.Cursor = cursor
			}
			return json.Marshal(message)
		}
	}
	return nil, &httpapi.StatusError{Code: 404, Message: "message not found"}
}

// ReadMessageImage serves the index-th inline image of the tool result callID in the
// message messageID. The transcript keeps the image as a data URL; it is decoded here and
// never copied into the message. cursor is the message's turn; without it only the history
// block is searched, as for ReadSessionMessage.
func (r *Resolver) ReadMessageImage(ctx context.Context, reference, profile, messageID, callID string, index int, cursor *int64) (httpapi.SessionImage, error) {
	if ctx == nil {
		return httpapi.SessionImage{}, errors.New("session message context is required")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.SessionImage{}, err
	}
	notFound := &httpapi.StatusError{Code: 404, Message: "image not found"}
	if callID == "" || index < 0 {
		return httpapi.SessionImage{}, notFound
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.SessionImage{}, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return httpapi.SessionImage{}, mapSessionError(err)
	}
	var records []webui.TranscriptRecord
	if cursor == nil {
		records, _, err = readHistoryRecords(ctx, config)
	} else {
		records, err = r.readTurnRecords(ctx, config, location.Reference, *cursor)
	}
	if err != nil {
		return httpapi.SessionImage{}, err
	}
	if data, contentType, ok := webui.FindInlineImage(records, messageID, callID, index); ok {
		return httpapi.SessionImage{ContentType: contentType, Data: data}, nil
	}
	return httpapi.SessionImage{}, notFound
}

// readHistoryRecords returns the backup tail the history block shows; hasBackup is false
// when the session keeps no backup.
func readHistoryRecords(ctx context.Context, config sessionfs.SessionConfig) (records []webui.TranscriptRecord, hasBackup bool, err error) {
	if config.BackupTurns <= 0 {
		return nil, false, nil
	}
	info, err := os.Stat(config.MessagesBackupPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat session backup %q: %w", config.MessagesBackupPath, err)
	}
	if info.Size() <= 0 {
		return nil, false, nil
	}
	records, err = readBackupTail(ctx, config.MessagesBackupPath, config.BackupTurns)
	return records, err == nil, err
}

// readTurnRecords returns the records of the transcript turn that starts at cursor.
func (r *Resolver) readTurnRecords(ctx context.Context, config sessionfs.SessionConfig, reference string, cursor int64) ([]webui.TranscriptRecord, error) {
	shared, err := r.sharedStore(config)
	if err != nil {
		return nil, err
	}
	index, err := scanTranscriptIndex(shared, config)
	if err != nil {
		return nil, fmt.Errorf("read session transcript %q: %w", reference, err)
	}
	turn := -1
	for candidate, start := range index.starts {
		if start == cursor {
			turn = candidate
			break
		}
	}
	if turn < 0 {
		return nil, &httpapi.StatusError{Code: 404, Message: "message not found"}
	}
	start, stop := index.starts[turn], index.size
	if turn+1 < len(index.starts) {
		stop = index.starts[turn+1]
	}
	records := make([]webui.TranscriptRecord, 0)
	err = shared.store.ReadRecordsRange(start, stop, func(offset int64, line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		records = append(records, webui.TranscriptRecord{
			Offset: offset,
			Prefix: "o",
			Raw:    append(json.RawMessage(nil), line...),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read session transcript %q: %w", reference, err)
	}
	return records, nil
}

func (r *Resolver) readHistoryBlock(ctx context.Context, config sessionfs.SessionConfig) (historyBlock, error) {
	records, hasBackup, err := readHistoryRecords(ctx, config)
	if err != nil || !hasBackup {
		return historyBlock{}, err
	}
	messages, err := webui.NormalizeTranscript(records)
	if err != nil {
		return historyBlock{}, err
	}
	transcriptInfo, err := os.Stat(config.MessagesPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return historyBlock{}, fmt.Errorf("stat session transcript %q: %w", config.MessagesPath, err)
	}
	generation := "0:0:0"
	if err == nil {
		generation = transcriptGeneration(config.MessagesPath, transcriptInfo)
	}
	key := fmt.Sprintf("%s|%s|%d", fileStamp(config.MessagesBackupPath), generation, config.BackupTurns)
	etag := strings.Trim(makeETag(key), `"`)
	return historyBlock{ETag: etag, Messages: messages}, nil
}

func readBackupTail(ctx context.Context, path string, limit int) ([]webui.TranscriptRecord, error) {
	if limit <= 0 {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open session backup %q: %w", path, err)
	}
	defer file.Close()

	tail := make([]webui.TranscriptRecord, 0)
	turnStarts := make([]int, 0)
	seenBatches := make(map[string]struct{})
	reader := bufio.NewReader(file)
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := reader.ReadBytes('\n')
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		if readErr != nil && readErr != io.EOF {
			return nil, fmt.Errorf("read session backup %q: %w", path, readErr)
		}
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		if !complete && readErr == io.EOF {
			// TypeScript's backup reader exposes only complete JSONL records.
			break
		}
		raw := strings.TrimSuffix(string(line), "\n")
		raw = strings.TrimSuffix(raw, "\r")
		if raw != "" {
			entry := webui.TranscriptRecord{Offset: offset, Prefix: "b", Raw: json.RawMessage(raw)}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(entry.Raw, &fields); err != nil || fields == nil {
				return nil, fmt.Errorf("backup record at byte %d is not a JSON object", offset)
			}
			var role, typeName, batchID string
			_ = json.Unmarshal(fields["role"], &role)
			_ = json.Unmarshal(fields["type"], &typeName)
			if typeName == "message" && role == "user" {
				newTurn := true
				if rawBatch, ok := fields[transcript.SessionInputBatchIDField]; ok && json.Unmarshal(rawBatch, &batchID) == nil && batchID != "" {
					if _, seen := seenBatches[batchID]; seen {
						newTurn = false
					} else {
						seenBatches[batchID] = struct{}{}
					}
				}
				if newTurn {
					turnStarts = append(turnStarts, len(tail))
				}
			}
			tail = append(tail, entry)
			if len(turnStarts) > limit {
				drop := turnStarts[1]
				tail = append([]webui.TranscriptRecord(nil), tail[drop:]...)
				remaining := make([]int, 0, len(turnStarts)-1)
				for _, start := range turnStarts[1:] {
					remaining = append(remaining, start-drop)
				}
				turnStarts = remaining
			}
		}
		offset += int64(len(line))
		if readErr == io.EOF {
			break
		}
	}
	return tail, nil
}

func stringFieldRaw(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

var _ = transcript.TurnStore{}
