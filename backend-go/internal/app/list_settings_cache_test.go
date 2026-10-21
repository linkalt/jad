package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func listedSessionFile(t *testing.T, resolver *Resolver, id string) string {
	t.Helper()
	location, err := sessionfs.Resolve(resolver.roots, "local:"+id)
	if err != nil {
		t.Fatal(err)
	}
	return location.File
}

func deleteListedSession(t *testing.T, resolver *Resolver, reference string) error {
	t.Helper()
	file, err := resolver.ReadSessionConfig(context.Background(), reference)
	if err != nil {
		return err
	}
	return resolver.DeleteSessionConfig(context.Background(), reference, file.ETag, false)
}

// markCached swaps the cached config of a session for one with a name only the cache
// can supply, so a list that shows the name did not parse the files.
func markCached(t *testing.T, resolver *Resolver, file, name string) {
	t.Helper()
	resolver.listSettingsCache.mu.Lock()
	defer resolver.listSettingsCache.mu.Unlock()
	entry, ok := resolver.listSettingsCache.entries[file]
	if !ok {
		t.Fatalf("no cached settings for %s", file)
	}
	entry.config.SessionName = name
	resolver.listSettingsCache.entries[file] = entry
}

func TestListSessionsReusesCachedSettingsWhileTheFilesAreUnchanged(t *testing.T) {
	resolver := newManySessionsResolver(t, 3, 1)
	ctx := context.Background()
	if _, err := resolver.ListSessions(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := resolver.listSettingsCache.len(); got != 3 {
		t.Fatalf("cache holds %d sessions after a list of 3", got)
	}
	file := listedSessionFile(t, resolver, "session-0001")
	markCached(t, resolver, file, "from-the-cache")
	response, err := resolver.ListSessions(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response.Body), "from-the-cache") {
		t.Fatal("an unchanged session was parsed again; the list did not use its cached settings")
	}
}

func TestListSessionsLoadsAgainAfterTheSessionFileChanges(t *testing.T) {
	resolver := newManySessionsResolver(t, 2, 1)
	ctx := context.Background()
	first, err := resolver.ListSessions(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	file := listedSessionFile(t, resolver, "session-0001")
	markCached(t, resolver, file, "from-the-cache")

	var session map[string]any
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	session["category"] = "edited-category"
	data, err = json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	response, err := resolver.ListSessions(ctx, first.ETag, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.NotModified {
		t.Fatal("an edited session file still answered 304")
	}
	body := string(response.Body)
	if strings.Contains(body, "from-the-cache") || !strings.Contains(body, "edited-category") {
		t.Fatalf("the list did not reload the edited session: %s", body)
	}
}

func TestListSessionsLoadsAgainAfterAPromptDocumentChanges(t *testing.T) {
	resolver := newManySessionsResolver(t, 1, 1)
	ctx := context.Background()
	document := filepath.Join(t.TempDir(), "rules.md")
	if err := os.WriteFile(document, []byte("rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(resolver.roots[0].Layout.Profiles, "dev", "main.json")
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["prompt_documents"] = []string{document}
	if data, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	backdateTree(t, resolver.roots[0].GlobalJadRoot, filepath.Dir(document))

	if response, err := resolver.ListSessions(ctx, "", nil); err != nil || !strings.Contains(string(response.Body), "session_name") {
		t.Fatalf("list with the document: %v", err)
	}
	file := listedSessionFile(t, resolver, "session-0000")
	if _, ok := resolver.listSettingsCache.get(file); !ok {
		t.Fatal("the session was not cached")
	}
	if err := os.Remove(document); err != nil {
		t.Fatal(err)
	}
	response, err := resolver.ListSessions(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(response.Body), "session_name") {
		t.Fatal("a session whose prompt document is gone is still listed from the cache")
	}
}

func TestListSessionsDropsCachedSettingsOfADeletedSession(t *testing.T) {
	resolver := newManySessionsResolver(t, 3, 1)
	ctx := context.Background()
	if _, err := resolver.ListSessions(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := deleteListedSession(t, resolver, "local:session-0001"); err != nil {
		t.Fatal(err)
	}
	if got := resolver.listSettingsCache.len(); got != 2 {
		t.Fatalf("cache holds %d sessions right after a delete, want 2", got)
	}
	response, err := resolver.ListSessions(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(response.Body), "session-0001") || resolver.listSettingsCache.len() != 2 {
		t.Fatalf("deleted session still listed or cached (cached %d)", resolver.listSettingsCache.len())
	}
}

func TestListSessionsDropsCachedSettingsOfASessionRemovedByHand(t *testing.T) {
	resolver := newManySessionsResolver(t, 3, 1)
	ctx := context.Background()
	if _, err := resolver.ListSessions(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(listedSessionFile(t, resolver, "session-0002")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ListSessions(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := resolver.listSettingsCache.len(); got != 2 {
		t.Fatalf("cache holds %d sessions after one file was removed, want 2 (the cache must not grow past the sessions)", got)
	}
}

func TestListSessionsConcurrentWithEditsAndDeletes(t *testing.T) {
	resolver := newManySessionsResolver(t, 6, 1)
	ctx := context.Background()
	if _, err := resolver.ListSessions(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := resolver.ListSessions(ctx, "", nil); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			file := listedSessionFile(t, resolver, "session-0003")
			if data, err := os.ReadFile(file); err == nil {
				_ = os.WriteFile(file, append(data, ' '), 0o600)
			}
		}
		if err := deleteListedSession(t, resolver, "local:session-0004"); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	response, err := resolver.ListSessions(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(response.Body), "session-0004") {
		t.Fatal("deleted session is listed")
	}
	if got := resolver.listSettingsCache.len(); got > 5 {
		t.Fatalf("cache holds %d sessions for 5 listed", got)
	}
}
