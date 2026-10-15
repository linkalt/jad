package app

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

// newLockedConfigFixture is the two-profile session with every file and folder dated an
// hour ago, so the racy-stamp rule does not hide what a test checks.
func newLockedConfigFixture(t *testing.T) (*Resolver, string) {
	t.Helper()
	config, fixture := layoutConfig(t, sameLengthModelsSpec(), map[string]string{})
	resolver, err := NewResolver(config.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	backdate(t, fixture.Root)
	return resolver, fixture.Root
}

// sameLengthModelsSpec is lockSpec with two model names of one length, so a test can switch
// a profile's model_default without changing the size of its file.
func sameLengthModelsSpec() layouttest.Spec {
	spec := lockSpec()
	spec.Models = map[string]map[string]any{"openai/mod-a": {"provider": "openai", "model": "gpt-test"}, "openai/mod-b": {"provider": "openai", "model": "gpt-test"}}
	spec.Profiles = map[string]map[string]any{
		"dev/green": {"models": []any{"openai/mod-a", "openai/mod-b"}, "model_default": "openai/mod-a"},
		"dev/blue":  {"models": []any{"openai/mod-a", "openai/mod-b"}, "model_default": "openai/mod-a"},
	}
	return spec
}

func backdate(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (c *lockedConfigs) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func resolvedModelDefault(t *testing.T, resolver *Resolver) string {
	t.Helper()
	resolved, err := resolver.Resolve(context.Background(), "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Runner.(boundRunner).config.ModelDefault
}

// rewriteKeepingTime replaces the text in a file with text of the same length and puts the
// old modification time back, the one edit a stamp of size and time cannot see.
func rewriteKeepingTime(t *testing.T, path, from, to string) {
	t.Helper()
	if len(from) != len(to) {
		t.Fatal("rewriteKeepingTime needs text of the same length")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), from) {
		t.Fatalf("%s does not contain %q", path, from)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), from, to, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestResolveReusesTheConfigLockSessionLoaded(t *testing.T) {
	resolver, root := newLockedConfigFixture(t)
	release, err := resolver.LockSession(context.Background(), "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if resolver.lockedConfigs.len() != 1 {
		t.Fatalf("parked configs after LockSession = %d, want 1", resolver.lockedConfigs.len())
	}
	// This edit leaves size and time as they were, so the stamp calls the files unchanged
	// and Resolve must answer with the parked config, not a fresh load.
	rewriteKeepingTime(t, filepath.Join(root, "profiles", "dev", "green.json"), `"model_default": "openai/mod-a"`, `"model_default": "openai/mod-b"`)
	if got := resolvedModelDefault(t, resolver); got != "openai/mod-a" {
		t.Fatalf("Resolve loaded again (model_default %q); it should have used the config LockSession loaded", got)
	}
	if resolver.lockedConfigs.len() != 0 {
		t.Fatalf("parked configs after Resolve = %d, want 0 (an entry serves one Resolve)", resolver.lockedConfigs.len())
	}
}

func TestResolveLoadsAgainWhenAFileReadByTheLockedConfigChanged(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		"profile edited, size and time changed": func(t *testing.T, root string) {
			path := filepath.Join(root, "profiles", "dev", "green.json")
			rewriteKeepingTime(t, path, `"model_default": "openai/mod-a"`, `"model_default": "openai/mod-b"`)
			later := time.Now().Add(-30 * time.Minute)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		},
		"selected profile edited, only the time changed": func(t *testing.T, root string) {
			path := filepath.Join(root, "profiles", "dev", "green.json")
			later := time.Now().Add(-30 * time.Minute)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatal(err)
			}
		},
		"unselected profile edited": func(t *testing.T, root string) {
			path := filepath.Join(root, "profiles", "dev", "blue.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"profile added": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "profiles", "dev", "extra.json"), []byte(`{"models":["openai/mod-a"],"model_default":"openai/mod-a"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"profile removed": func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "profiles", "dev", "blue.json")); err != nil {
				t.Fatal(err)
			}
		},
		"model added": func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "models", "openai", "extra.json"), []byte(`{"provider":"openai","model":"gpt-test"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"session file edited": func(t *testing.T, root string) {
			path := filepath.Join(root, "sessions", "session-1.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			resolver, root := newLockedConfigFixture(t)
			release, err := resolver.LockSession(context.Background(), "session-1", "dev/green", "")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if resolver.lockedConfigs.len() != 1 {
				t.Fatalf("LockSession parked %d configs, want 1", resolver.lockedConfigs.len())
			}
			location, err := sessionfs.Resolve(resolver.roots, "session-1")
			if err != nil {
				t.Fatal(err)
			}
			_, stamp, err := sessionfs.LoadSessionSettingsStamped(location, "dev/green", "")
			if err != nil {
				t.Fatal(err)
			}
			if !stamp.Current() {
				t.Fatal("a stamp of untouched files is not current")
			}
			change(t, root)
			if stamp.Current() {
				t.Fatal("the stamp still says current after a file it covers changed")
			}
			resolved, err := resolver.Resolve(context.Background(), "session-1", "dev/green", "")
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := sessionfs.LoadSessionSettings(location, "dev/green", "")
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.Runner.(boundRunner).config; !reflect.DeepEqual(got, fresh) {
				t.Fatalf("Resolve returned a config that differs from a fresh load:\n got  %+v\n want %+v", got, fresh)
			}
			if resolver.lockedConfigs.len() != 0 {
				t.Fatalf("the rejected entry stayed parked")
			}
		})
	}
}

func TestLockedConfigIsNotUsedForFilesChangedJustBeforeTheLoad(t *testing.T) {
	config, _ := layoutConfig(t, lockSpec(), map[string]string{})
	resolver, err := NewResolver(config.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer resolver.Close()
	release, err := resolver.LockSession(context.Background(), "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for key, entry := range resolver.lockedConfigs.entries {
		if _, ok := resolver.lockedConfigs.take(key, entry.loaded); ok {
			t.Fatal("a config was reused although its files were written seconds before the stamp, so the stamp cannot see another edit")
		}
		return
	}
	t.Fatal("LockSession parked no config")
}

func TestLockedConfigIsRejectedAfterItsMaxAge(t *testing.T) {
	resolver, _ := newLockedConfigFixture(t)
	release, err := resolver.LockSession(context.Background(), "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for key, entry := range resolver.lockedConfigs.entries {
		if _, ok := resolver.lockedConfigs.take(key, entry.loaded.Add(lockedConfigMaxAge+time.Millisecond)); ok {
			t.Fatal("an old config was reused")
		}
		return
	}
	t.Fatal("LockSession parked no config")
}

func TestLockedConfigKeysDoNotShareEntries(t *testing.T) {
	resolver, _ := newLockedConfigFixture(t)
	ctx := context.Background()
	releaseGreen, err := resolver.LockSession(ctx, "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseGreen()
	releaseBlue, err := resolver.LockSession(ctx, "session-1", "dev/blue", "")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBlue()
	if resolver.lockedConfigs.len() != 2 {
		t.Fatalf("parked configs = %d, want 2 (one per profile)", resolver.lockedConfigs.len())
	}
	resolved, err := resolver.Resolve(ctx, "session-1", "dev/blue", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Runner.(boundRunner).config.Profile; got != "dev/blue" {
		t.Fatalf("Resolve for dev/blue returned the config of %q", got)
	}
	if resolver.lockedConfigs.len() != 1 {
		t.Fatalf("parked configs = %d, want 1 (the green entry stays)", resolver.lockedConfigs.len())
	}
}

func TestLockedConfigIsDroppedWhenTheLockIsReleased(t *testing.T) {
	resolver, _ := newLockedConfigFixture(t)
	release, err := resolver.LockSession(context.Background(), "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if resolver.lockedConfigs.len() != 0 {
		t.Fatalf("parked configs after release = %d, want 0 (checkpoint and mutation routes lock without resolving)", resolver.lockedConfigs.len())
	}
}

func TestLockedConfigIsNotParkedAfterWaitingForTheLock(t *testing.T) {
	resolver, _ := newLockedConfigFixture(t)
	releaseFirst, err := resolver.LockSession(context.Background(), "session-1", "dev/green", "")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		release func()
		err     error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		release, err := resolver.LockSession(ctx, "session-1", "dev/green", "")
		done <- result{release, err}
	}()
	time.Sleep(100 * time.Millisecond)
	releaseFirst()
	second := <-done
	if second.err != nil {
		t.Fatal(second.err)
	}
	defer second.release()
	if resolver.lockedConfigs.len() != 0 {
		t.Fatalf("parked configs after a waited lock = %d, want 0 (the config was read before the wait)", resolver.lockedConfigs.len())
	}
}
