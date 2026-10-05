package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/httpapi"
	"example.com/agent-harness/backend-go/internal/sessionfs"
)

func TestConditionalCatalogResponsesSkipTranscriptReads(t *testing.T) {
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

	roots := []sessionfs.Root{{ID: "primary", Label: "Primary", Directory: rootDir}}
	location, err := sessionfs.Resolve(roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.MessagesPath, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := resolver.ListSessions(ctx, "", nil); err == nil {
		t.Fatal("unconditional list unexpectedly accepted a malformed transcript")
	}
	if _, err := resolver.ReadSessionSnapshot(ctx, location.Reference, "", httpapi.SnapshotOptions{}, "", nil); err == nil {
		t.Fatal("unconditional snapshot unexpectedly accepted a malformed transcript")
	}

	emptyRuns := httpapi.SessionRunSummaries{}
	snapshotETag := sessionVersionForCatalog(location, config, resolver.catalog.MaxInlinePartBytes, emptyRuns)
	snapshot, err := resolver.ReadSessionSnapshot(ctx, location.Reference, "", httpapi.SnapshotOptions{}, snapshotETag, nil)
	if err != nil {
		t.Fatalf("conditional snapshot: %v", err)
	}
	if !snapshot.NotModified || len(snapshot.Body) != 0 || snapshot.ETag != snapshotETag {
		t.Fatalf("conditional snapshot response = %+v", snapshot)
	}

	listVersion := sessionVersionForCatalog(location, config, resolver.catalog.MaxInlinePartBytes, emptyRuns)
	listETag := makeETag(location.Reference + ":" + listVersion)
	list, err := resolver.ListSessions(ctx, listETag, nil)
	if err != nil {
		t.Fatalf("conditional list: %v", err)
	}
	if !list.NotModified || len(list.Body) != 0 || list.ETag != listETag {
		t.Fatalf("conditional list response = %+v", list)
	}
}

func TestCatalogRunSummariesAreReturnedAndVersioned(t *testing.T) {
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

	roots := []sessionfs.Root{{ID: "primary", Label: "Primary", Directory: rootDir}}
	location, err := sessionfs.Resolve(roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}

	defaultRun := httpapi.SessionRunState{TurnID: "turn-default", StartedAt: "2026-09-29T08:00:00Z", Status: "running"}
	profileRun := httpapi.SessionRunState{TurnID: "turn-review", StartedAt: "2026-09-29T08:01:00Z", Status: "completed"}
	provider := fixedCatalogRunProvider{summaries: httpapi.SessionRunSummaries{
		Default:   &defaultRun,
		ByProfile: map[string]httpapi.SessionRunState{"default": defaultRun, "review": profileRun},
	}}

	list, err := resolver.ListSessions(context.Background(), "", provider)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var listed struct {
		Sessions []sessionListRecord `json:"sessions"`
	}
	if err := json.Unmarshal(list.Body, &listed); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].Run == nil || *listed.Sessions[0].Run != defaultRun {
		t.Fatalf("session list default run = %+v", listed.Sessions)
	}
	if got := listed.Sessions[0].RunsByProfile["review"]; got != profileRun {
		t.Fatalf("session list profile run = %+v, want %+v", got, profileRun)
	}

	snapshot, err := resolver.ReadSessionSnapshot(context.Background(), location.Reference, "", httpapi.SnapshotOptions{}, "", provider)
	if err != nil {
		t.Fatalf("read session snapshot: %v", err)
	}
	var snapshotRecord sessionListRecord
	if err := json.Unmarshal(snapshot.Body, &snapshotRecord); err != nil {
		t.Fatalf("decode session snapshot: %v", err)
	}
	if snapshotRecord.Run == nil || *snapshotRecord.Run != defaultRun || snapshotRecord.RunsByProfile["review"] != profileRun {
		t.Fatalf("snapshot run summaries = %+v / %+v", snapshotRecord.Run, snapshotRecord.RunsByProfile)
	}

	changedRun := defaultRun
	changedRun.Status = "completed"
	changedProvider := fixedCatalogRunProvider{summaries: httpapi.SessionRunSummaries{
		Default:   &changedRun,
		ByProfile: map[string]httpapi.SessionRunState{"default": changedRun, "review": profileRun},
	}}
	changedList, err := resolver.ListSessions(context.Background(), list.ETag, changedProvider)
	if err != nil {
		t.Fatalf("revalidate session list: %v", err)
	}
	if changedList.NotModified || changedList.ETag == list.ETag {
		t.Fatalf("run state did not invalidate list ETag: before=%q after=%+v", list.ETag, changedList)
	}
	changedSnapshot, err := resolver.ReadSessionSnapshot(context.Background(), location.Reference, "", httpapi.SnapshotOptions{}, snapshot.ETag, changedProvider)
	if err != nil {
		t.Fatalf("revalidate session snapshot: %v", err)
	}
	if changedSnapshot.NotModified || changedSnapshot.ETag == snapshot.ETag {
		t.Fatalf("run state did not invalidate snapshot ETag: before=%q after=%+v", snapshot.ETag, changedSnapshot)
	}
}

type fixedCatalogRunProvider struct {
	summaries httpapi.SessionRunSummaries
}

func (p fixedCatalogRunProvider) SessionRuns(string, string) httpapi.SessionRunSummaries {
	return p.summaries
}

func TestSnapshotCursorsUseTurnByteOffsetsAndFetchWholeMessage(t *testing.T) {
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
	firstUser := `{"type":"message","role":"user","content":[{"type":"input_text","text":"first question"}]}`
	firstResponse := `{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"resp-1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a long answer that is capped in the snapshot"}]}]}}`
	secondUser := `{"type":"message","role":"user","content":[{"type":"input_text","text":"second question"}]}`
	secondResponse := `{"type":"response.completed","session_turn":2,"session_step":1,"response":{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"later answer"}]}]}}`
	transcript := strings.Join([]string{firstUser, firstResponse, secondUser, secondResponse}, "\n") + "\n"
	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	if err := os.WriteFile(messagesPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := []sessionfs.Root{{ID: "primary", Label: "Primary", Directory: rootDir}}
	location, err := sessionfs.Resolve(roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 4})
	if err != nil {
		t.Fatal(err)
	}

	snapshotResponse, err := resolver.ReadSessionSnapshot(context.Background(), location.Reference, "", httpapi.SnapshotOptions{}, "", nil)
	if err != nil {
		t.Fatalf("read session snapshot: %v", err)
	}
	var snapshot struct {
		Messages []struct {
			ID     string `json:"id"`
			Sender string `json:"sender"`
			Cursor *int64 `json:"cursor"`
			Parts  []struct {
				Text      string          `json:"text"`
				Truncated json.RawMessage `json:"truncated"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(snapshotResponse.Body, &snapshot); err != nil {
		t.Fatalf("decode session snapshot: %v", err)
	}
	var firstCapped, secondAssistant *struct {
		ID     string `json:"id"`
		Sender string `json:"sender"`
		Cursor *int64 `json:"cursor"`
		Parts  []struct {
			Text      string          `json:"text"`
			Truncated json.RawMessage `json:"truncated"`
		} `json:"parts"`
	}
	for index := range snapshot.Messages {
		message := &snapshot.Messages[index]
		if message.Sender != "assistant" || message.Cursor == nil {
			continue
		}
		if len(message.Parts) > 0 && message.Parts[0].Text == "a lo" && len(message.Parts[0].Truncated) > 0 {
			firstCapped = message
		}
		if len(message.Parts) > 0 && message.Parts[0].Text == "late" {
			secondAssistant = message
		}
	}
	if firstCapped == nil || *firstCapped.Cursor != 0 {
		t.Fatalf("first capped message = %+v, want transcript byte cursor 0", firstCapped)
	}
	wantSecondCursor := int64(strings.Index(transcript, secondUser))
	if secondAssistant == nil || secondAssistant.Cursor == nil || *secondAssistant.Cursor != wantSecondCursor {
		t.Fatalf("second assistant cursor = %+v, want byte offset %d", secondAssistant, wantSecondCursor)
	}

	fullRaw, err := resolver.ReadSessionMessage(context.Background(), location.Reference, "", firstCapped.ID, firstCapped.Cursor)
	if err != nil {
		t.Fatalf("fetch capped message: %v", err)
	}
	var full struct {
		Parts []struct {
			Text      string          `json:"text"`
			Truncated json.RawMessage `json:"truncated"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(fullRaw, &full); err != nil {
		t.Fatalf("decode full message: %v", err)
	}
	if len(full.Parts) != 1 || full.Parts[0].Text != "a long answer that is capped in the snapshot" || len(full.Parts[0].Truncated) > 0 {
		t.Fatalf("full fetched parts = %+v", full.Parts)
	}
	if _, err := resolver.ReadSessionMessage(context.Background(), location.Reference, "", firstCapped.ID, nil); err == nil {
		t.Fatal("live transcript message without a cursor was unexpectedly found")
	}
	wrongCursor := wantSecondCursor
	if _, err := resolver.ReadSessionMessage(context.Background(), location.Reference, "", firstCapped.ID, &wrongCursor); err == nil {
		t.Fatal("message fetch accepted a cursor from a different turn")
	}
}
