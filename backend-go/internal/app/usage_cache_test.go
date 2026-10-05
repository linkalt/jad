package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func TestUsageCacheWritesAndExtendsTranscriptSummary(t *testing.T) {
	workspace := t.TempDir()
	rootDir := filepath.Join(workspace, "sessions")
	sessionDir := filepath.Join(rootDir, "session-1")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	setting := fmt.Sprintf(`{"cwd":%q,"persistence":{"durability":"process","max_item_bytes":1048576,"response_metadata":"full"}}`, workspace)
	if err := os.WriteFile(filepath.Join(sessionDir, "setting.json"), []byte(setting), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "model.json"), []byte(`{"provider":"openai","model":"gpt-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	firstUser := `{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}`
	firstResponse := `{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"usage-response-1","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"output":[]}}`
	transcriptPath := filepath.Join(sessionDir, "messages.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(firstUser+"\n"+firstResponse+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := []sessionfs.Root{{ID: "primary", Label: "Primary", Directory: rootDir}}
	location, err := sessionfs.Resolve(roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := resolver.ReadSessionSnapshot(ctx, location.Reference, "", httpapi.SnapshotOptions{}, "", nil)
	if err != nil {
		t.Fatalf("read initial snapshot: %v", err)
	}
	var firstSnapshot sessionListRecord
	if err := json.Unmarshal(first.Body, &firstSnapshot); err != nil {
		t.Fatalf("decode initial snapshot: %v", err)
	}
	if firstSnapshot.Usage == nil || firstSnapshot.Usage.CompletedResponses != 1 || firstSnapshot.Usage.TurnCount != 1 || firstSnapshot.Usage.TokenUsage.InputTokens == nil || *firstSnapshot.Usage.TokenUsage.InputTokens != 5 {
		t.Fatalf("initial usage summary = %+v", firstSnapshot.Usage)
	}
	cacheBytes, err := os.ReadFile(config.UsageCachePath)
	if err != nil {
		t.Fatalf("read usage cache: %v", err)
	}
	var cache usageCacheFile
	if err := json.Unmarshal(cacheBytes, &cache); err != nil {
		t.Fatalf("decode usage cache: %v", err)
	}
	if cache.SchemaVersion != 1 || cache.Transcript.Offset != int64(len(firstUser)+len(firstResponse)+2) || cache.LastResponseKey == "" || cache.Summary.CompletedResponses != 1 {
		t.Fatalf("initial usage cache = %+v", cache)
	}

	unchanged, err := resolver.ReadSessionSnapshot(ctx, location.Reference, "", httpapi.SnapshotOptions{}, first.ETag, nil)
	if err != nil {
		t.Fatalf("conditional unchanged snapshot: %v", err)
	}
	if !unchanged.NotModified {
		t.Fatalf("unchanged snapshot = %+v, want not modified", unchanged)
	}
	cacheAfterRevalidation, err := os.ReadFile(config.UsageCachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cacheAfterRevalidation) != string(cacheBytes) {
		t.Fatal("usage cache changed during unchanged conditional read")
	}

	secondUser := `{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}`
	secondResponse := `{"type":"response.completed","session_turn":2,"session_step":1,"response":{"id":"usage-response-2","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7},"output":[]}}`
	file, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write([]byte(secondUser + "\n" + secondResponse + "\n"))
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	appended, err := resolver.ReadSessionSnapshot(ctx, location.Reference, "", httpapi.SnapshotOptions{}, first.ETag, nil)
	if err != nil {
		t.Fatalf("read appended snapshot: %v", err)
	}
	if appended.NotModified {
		t.Fatal("appended transcript returned not modified")
	}
	var appendedSnapshot sessionListRecord
	if err := json.Unmarshal(appended.Body, &appendedSnapshot); err != nil {
		t.Fatalf("decode appended snapshot: %v", err)
	}
	usage := appendedSnapshot.Usage
	if usage == nil || usage.CompletedResponses != 2 || usage.TurnCount != 2 || usage.TokenUsage.InputTokens == nil || *usage.TokenUsage.InputTokens != 8 || usage.TokenUsage.OutputTokens == nil || *usage.TokenUsage.OutputTokens != 6 {
		t.Fatalf("appended usage summary = %+v", usage)
	}
	updatedCacheBytes, err := os.ReadFile(config.UsageCachePath)
	if err != nil {
		t.Fatalf("read updated usage cache: %v", err)
	}
	if err := json.Unmarshal(updatedCacheBytes, &cache); err != nil {
		t.Fatalf("decode updated usage cache: %v", err)
	}
	if cache.Transcript.Offset != int64(len(firstUser)+len(firstResponse)+len(secondUser)+len(secondResponse)+4) || cache.Summary.CompletedResponses != 2 || cache.LastResponseKey == "" {
		t.Fatalf("updated usage cache = %+v", cache)
	}
}
