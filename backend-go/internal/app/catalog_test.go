package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestListingANeverRunSessionCreatesNoMessagesDirectory(t *testing.T) {
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	location, err := sessionfs.Resolve(loaded.Roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(loaded.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ListSessions(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(config.MessagesPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("listing created the messages directory: stat err = %v", err)
	}
}

func TestConditionalCatalogResponsesSkipTranscriptReads(t *testing.T) {
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	roots := loaded.Roots

	location, err := sessionfs.Resolve(roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(config.MessagesPath), 0o755); err != nil {
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
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	roots := loaded.Roots

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
	loaded, _ := layoutConfig(t, catalogSpec(), map[string]string{})
	roots := loaded.Roots
	firstUser := `{"type":"message","role":"user","content":[{"type":"input_text","text":"first question"}]}`
	firstResponse := `{"type":"response.completed","session_turn":1,"session_step":1,"response":{"id":"resp-1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a long answer that is capped in the snapshot"}]}]}}`
	secondUser := `{"type":"message","role":"user","content":[{"type":"input_text","text":"second question"}]}`
	secondResponse := `{"type":"response.completed","session_turn":2,"session_step":1,"response":{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"later answer"}]}]}}`
	transcript := strings.Join([]string{firstUser, firstResponse, secondUser, secondResponse}, "\n") + "\n"
	location, err := sessionfs.Resolve(roots, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	sessionConfig, err := sessionfs.LoadSessionConfig(location, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sessionConfig.MessagesPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionConfig.MessagesPath, []byte(transcript), 0o600); err != nil {
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

// TestSessionListNamesTheModelFileTheSettingsSelect checks that the session list reports
// the provider and model of the model the session's active profile selects (its
// model_default, or the first model it lists), not of another model the profile also lists.
func TestSessionListNamesTheModelFileTheSettingsSelect(t *testing.T) {
	models := map[string]map[string]any{
		"m/plain":  {"provider": "plain", "model": "plain-model"},
		"m/other":  {"provider": "  selected  ", "model": " selected-model "},
		"m/second": {"provider": "listed", "model": "listed-model"},
		"m/odd":    {"provider": 7, "model": "   "},
		"m/zeta":   {"provider": "zeta", "model": "zeta-model"},
	}
	profile := func(listed []string, modelDefault string) map[string]any {
		entry := map[string]any{"models": listed}
		if modelDefault != "" {
			entry["model_default"] = modelDefault
		}
		return entry
	}
	loaded, fixture := layoutConfig(t, layouttest.Spec{
		SessionID: "profiled", Session: map[string]any{"scope": map[string]any{"profiles": []string{"dev/profiled"}}}, Models: models,
		Profiles: map[string]map[string]any{
			// profiled: the profile's default model is m/other, the plain model is also listed.
			"dev/profiled": profile([]string{"m/other", "m/plain"}, "m/other"),
			// first: no model_default, so the first model in sorted order is the default.
			"dev/first": profile([]string{"m/zeta", "m/second"}, ""),
			"dev/plain": profile([]string{"m/plain"}, ""),
			// partial: provider is not a string, model is blank.
			"dev/partial": profile([]string{"m/odd"}, ""),
		},
	}, map[string]string{})
	fixture.AddSessionOn(t, "first", fixture.CWD, "dev/first")
	fixture.AddSessionOn(t, "plain", fixture.CWD, "dev/plain")
	fixture.AddSessionOn(t, "partial", fixture.CWD, "dev/partial")

	resolver, err := NewResolver(loaded.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	list, err := resolver.ListSessions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var listed struct {
		Sessions []struct {
			SessionID string       `json:"session_id"`
			Model     modelSummary `json:"model"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(list.Body, &listed); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	got := map[string]modelSummary{}
	for _, session := range listed.Sessions {
		got[session.SessionID] = session.Model
	}
	want := map[string]modelSummary{
		"profiled": {Provider: "selected", Model: "selected-model"},
		"first":    {Provider: "listed", Model: "listed-model"},
		"plain":    {Provider: "plain", Model: "plain-model"},
		"partial":  {Provider: "unknown", Model: "partial"},
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for session, expected := range want {
		if got[session] != expected {
			t.Errorf("session %q model = %+v, want %+v", session, got[session], expected)
		}
	}
}

func TestSessionListAndSnapshotCarryModelOptions(t *testing.T) {
	loaded, fixture := layoutConfig(t, layouttest.Spec{
		SessionID: "options",
		Models: map[string]map[string]any{
			"m/a": {"provider": "alpha", "model": "alpha-model", "api_key_env": "FIXTURE_KEY", "efforts": []string{"low", "high"}, "request": map[string]any{"reasoning": map[string]any{"effort": "high"}}},
			"m/b": {"provider": "beta", "model": "beta-model", "api_key_env": "FIXTURE_KEY"},
			"m/c": {"provider": "gamma", "model": "gamma-model", "api_key_env": "FIXTURE_KEY"},
		},
		Profiles: map[string]map[string]any{"dev/options": {"models": []string{"m/a", "m/b", "m/c"}, "model_default": "m/a"}},
		Session:  map[string]any{"scope": map[string]any{"profiles": []string{"dev/options"}}},
	}, map[string]string{"FIXTURE_KEY": "key"})
	modelFile := func(id string) string { return filepath.Join(fixture.Root, "models", filepath.FromSlash(id)+".json") }
	if err := os.WriteFile(modelFile("m/c"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(loaded.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	want := []modelOption{
		{ID: "m/a", Provider: "alpha", Efforts: []string{"low", "high"}, DefaultEffort: "high"},
		{ID: "m/b", Provider: "beta"},
		{ID: "m/c", Provider: "unknown"},
	}

	list, err := resolver.ListSessions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var listed struct {
		Sessions []sessionListRecord `json:"sessions"`
	}
	if err := json.Unmarshal(list.Body, &listed); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	if len(listed.Sessions) != 1 || !reflect.DeepEqual(listed.Sessions[0].ModelOptions, want) {
		t.Fatalf("list model_options = %+v, want %+v", listed.Sessions, want)
	}
	if strings.Contains(string(list.Body), `"efforts":null`) || strings.Contains(string(list.Body), `"default_effort":""`) {
		t.Fatalf("empty efforts or default_effort were not omitted: %s", list.Body)
	}

	snapshot, err := resolver.ReadSessionSnapshot(context.Background(), listed.Sessions[0].ID, "", httpapi.SnapshotOptions{}, "", nil)
	if err != nil {
		t.Fatalf("read session snapshot: %v", err)
	}
	var snapshotRecord sessionListRecord
	if err := json.Unmarshal(snapshot.Body, &snapshotRecord); err != nil {
		t.Fatalf("decode session snapshot: %v", err)
	}
	if !reflect.DeepEqual(snapshotRecord.ModelOptions, want) {
		t.Fatalf("snapshot model_options = %+v, want %+v", snapshotRecord.ModelOptions, want)
	}

	// m/b is listed but not the active model: changing its file must still change the list ETag.
	if err := os.WriteFile(modelFile("m/b"), []byte(`{"provider":"beta","model":"beta-model","api_key_env":"FIXTURE_KEY","efforts":["low"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := resolver.ListSessions(context.Background(), list.ETag, nil)
	if err != nil {
		t.Fatalf("revalidate session list: %v", err)
	}
	if changed.NotModified || changed.ETag == list.ETag {
		t.Fatalf("a listed model file change did not invalidate the list ETag: %+v", changed)
	}
}

func TestListingModelOptionsReadsEachFieldIndependently(t *testing.T) {
	loaded, fixture := layoutConfig(t, layouttest.Spec{
		SessionID: "lenient",
		Models: map[string]map[string]any{
			"m/gone":    {"provider": "alpha", "model": "alpha-model", "api_key_env": "FIXTURE_KEY"},
			"m/request": {"provider": "beta", "model": "beta-model", "api_key_env": "FIXTURE_KEY"},
			"m/efforts": {"provider": "gamma", "model": "gamma-model", "api_key_env": "FIXTURE_KEY"},
			"m/plain":   {"provider": "delta", "model": "delta-model", "api_key_env": "FIXTURE_KEY"},
		},
		Profiles: map[string]map[string]any{"dev/lenient": {"models": []string{"m/gone", "m/request", "m/efforts", "m/plain"}, "model_default": "m/plain"}},
		Session:  map[string]any{"scope": map[string]any{"profiles": []string{"dev/lenient"}}},
	}, map[string]string{"FIXTURE_KEY": "key"})
	modelFile := func(id string) string { return filepath.Join(fixture.Root, "models", filepath.FromSlash(id)+".json") }
	if err := os.WriteFile(modelFile("m/gone"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelFile("m/request"), []byte(`{"provider":"beta","model":"beta-model","efforts":["low"],"request":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelFile("m/efforts"), []byte(`{"provider":"gamma","model":"gamma-model","efforts":"high","request":{"reasoning":{"effort":"low"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewResolver(loaded.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50, MaxInlinePartBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	list, err := resolver.ListSessions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var listed struct {
		Sessions []sessionListRecord `json:"sessions"`
	}
	if err := json.Unmarshal(list.Body, &listed); err != nil {
		t.Fatalf("decode session list: %v", err)
	}
	want := []modelOption{
		{ID: "m/efforts", Provider: "gamma", DefaultEffort: "low"},
		{ID: "m/gone", Provider: "unknown"},
		{ID: "m/plain", Provider: "delta"},
		{ID: "m/request", Provider: "beta", Efforts: []string{"low"}},
	}
	if len(listed.Sessions) != 1 || !reflect.DeepEqual(listed.Sessions[0].ModelOptions, want) {
		t.Fatalf("list model_options = %+v, want %+v", listed.Sessions, want)
	}
	ids := make([]string, 0, len(want))
	for _, option := range listed.Sessions[0].ModelOptions {
		ids = append(ids, option.ID)
	}
	if models := listed.Sessions[0].Models; models == nil || !reflect.DeepEqual(*models, ids) {
		t.Fatalf("list models = %v, want the model_options order %v", models, ids)
	}
}

// catalogSpec is the session the catalog tests read: process durability and full response
// metadata, in session-1.
func catalogSpec() layouttest.Spec {
	return layouttest.Spec{
		SessionID: "session-1",
		Settings:  map[string]any{"persistence": map[string]any{"durability": "process", "max_item_bytes": 1048576, "response_metadata": "full"}},
	}
}
