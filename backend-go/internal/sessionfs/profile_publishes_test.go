package sessionfs

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// publishFixture is dev/coder running its own toolset coding/lean (exec timeout 2000) with
// max_tool_rounds 5; the edit it publishes sets 7 and 3000 together.
func publishFixture(t *testing.T) editFixture {
	t.Helper()
	e := newToolsetEditFixture(t)
	if _, err := e.editToolsets(t, publishProfile(5), ProfileDocumentChange{Path: "coding/lean", Content: text(ownLeanEdit)}); err != nil {
		t.Fatal(err)
	}
	return e
}

func publishProfile(rounds int) string {
	return editBase + `,"toolset":"profile:coding/lean","settings":{"max_tool_rounds":` + string(rune('0'+rounds)) + `}}`
}

const ownLeanNew = `{"schema_version":1,"based_on":"global:coding/full","omit":{"native":["skill"]},"set":{"native":{"exec":{"timeout_ms":3000}}}}`

// publishNew edits both halves: the own toolset is published first, then the profile file.
func (e editFixture) publishNew(t *testing.T) error {
	t.Helper()
	_, err := e.editToolsets(t, publishProfile(7), ProfileDocumentChange{Path: "coding/lean", Content: text(ownLeanNew), IfMatch: e.ownToolsetRevision(t, "coding/lean")})
	return err
}

// revisionOf names the revision a load read: old, new, or a mix of the two.
func revisionOf(config SessionConfig, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	switch rounds, timeout := config.MaxToolRounds, config.NativeExec.Child.TimeoutMs; {
	case rounds == 5 && timeout == 2000:
		return "old"
	case rounds == 7 && timeout == 3000:
		return "new"
	default:
		return "mixed"
	}
}

// pausePublish runs during at the named publish step and then lets the publish go on, or fails
// it with fail.
func pausePublish(t *testing.T, step string, fail error, during func()) {
	t.Helper()
	t.Cleanup(func() { editPublishFault = nil })
	editPublishFault = func(at string) error {
		if at != step {
			return nil
		}
		during()
		return fail
	}
}

// A load that runs while the own toolset is published and the profile file is not yet waits for
// the publish and reads both halves of the new revision, never the new toolset with the old
// profile file.
func TestALoadDuringAPublishReadsOneRevision(t *testing.T) {
	e := publishFixture(t)
	if got := revisionOf(mustLoadCoder(e)); got != "old" {
		t.Fatalf("before = %s", got)
	}
	var loaded sync.WaitGroup
	var config SessionConfig
	var loadErr error
	pausePublish(t, "document", nil, func() {
		loaded.Add(1)
		done := make(chan struct{})
		go func() {
			defer loaded.Done()
			defer close(done)
			config, loadErr = LoadSessionSettings(e.location, "dev/coder", "")
		}()
		select {
		case <-done:
			t.Error("the load did not wait for the publish")
		case <-time.After(50 * time.Millisecond):
		}
	})
	if err := e.publishNew(t); err != nil {
		t.Fatal(err)
	}
	loaded.Wait()
	if got := revisionOf(config, loadErr); got != "new" {
		t.Fatalf("load during the publish = %s", got)
	}
}

// A publish that fails after its first rename is undone: the load that ran meanwhile reads the
// old revision whole, and so does the next.
func TestALoadDuringAnUndonePublishReadsTheOldRevision(t *testing.T) {
	e := publishFixture(t)
	var loaded sync.WaitGroup
	var config SessionConfig
	var loadErr error
	pausePublish(t, "profile", errors.New("injected"), func() {
		loaded.Add(1)
		go func() {
			defer loaded.Done()
			config, loadErr = LoadSessionSettings(e.location, "dev/coder", "")
		}()
		time.Sleep(50 * time.Millisecond)
	})
	if err := e.publishNew(t); err == nil || !strings.Contains(err.Error(), "was undone") {
		t.Fatalf("publish = %v", err)
	}
	loaded.Wait()
	if got := revisionOf(config, loadErr); got != "old" {
		t.Fatalf("load during the undone publish = %s", got)
	}
	if got := revisionOf(mustLoadCoder(e)); got != "old" {
		t.Fatalf("next load = %s", got)
	}
	if changed, running := profilePublishes.unsettled(profilePublishes.now(), []string{e.file}); len(changed) != 0 || len(running) != 0 {
		t.Fatalf("after the undo: %v %v", changed, running)
	}
}

// A memo made before a publish holds what it read before: a load through it after the publish
// reads again from a new memo, so it never pairs the memo's old own files with the new profile
// file.
func TestAMemoOlderThanAPublishIsReadAgain(t *testing.T) {
	e := publishFixture(t)
	scans := NewCatalogScans().recorder()
	if got := revisionOf(LoadSessionSettingsScoped(e.location, "dev/coder", "", scans)); got != "old" {
		t.Fatalf("first = %s", got)
	}
	if err := e.publishNew(t); err != nil {
		t.Fatal(err)
	}
	if got := revisionOf(LoadSessionSettingsScoped(e.location, "dev/coder", "", scans)); got != "new" {
		t.Fatalf("through the old memo = %s", got)
	}
	if len(scans.stamp().files) == 0 {
		t.Fatal("the recording view lost its stamps")
	}
}

// A profile that keeps publishing makes the load give up after profileLoadAttempts loads with a
// retry error, not return a config.
func TestALoadGivesUpOnAProfileThatKeepsPublishing(t *testing.T) {
	e := publishFixture(t)
	end := profilePublishes.begin(e.file)
	result := make(chan error, 1)
	go func() {
		_, err := LoadSessionSettings(e.location, "dev/coder", "")
		result <- err
	}()
	for attempt := 1; attempt < profileLoadAttempts; attempt++ {
		time.Sleep(50 * time.Millisecond)
		end()
		end = profilePublishes.begin(e.file)
	}
	err := <-result
	end()
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "is being edited; try again") {
		t.Fatalf("load = %v", err)
	}
	if got := revisionOf(mustLoadCoder(e)); got != "old" {
		t.Fatalf("after = %s", got)
	}
}

// The trial load of an edit reads under the writer's lock and is not held up by a publish.
func TestATrialLoadIsNotHeldByAPublish(t *testing.T) {
	e := publishFixture(t)
	end := profilePublishes.begin(e.file)
	defer end()
	done := make(chan error, 1)
	go func() {
		_, err := loadLayoutSessionSettings(e.location, *e.location.Root.Layout, "dev/coder", "", newTrialScans(e.file, []byte(publishProfile(7))))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the trial load waited for the publish")
	}
}

// Loads and edits at once: every load reads one revision or reports the retry error (-race).
func TestLoadsAndPublishesAtOnceNeverMix(t *testing.T) {
	e := publishFixture(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				config, err := LoadSessionSettings(e.location, "dev/coder", "")
				var conflict *ConflictError
				if errors.As(err, &conflict) {
					continue
				}
				if err != nil {
					t.Errorf("load = %v", err)
					return
				}
				if got := revisionOf(config, nil); got == "mixed" {
					t.Error("a load read two revisions")
					return
				}
			}
		}()
	}
	for round := 0; round < 10; round++ {
		rounds, content := 7, ownLeanNew
		if round%2 == 1 {
			rounds, content = 5, ownLeanEdit
		}
		if _, err := e.editToolsets(t, publishProfile(rounds), ProfileDocumentChange{Path: "coding/lean", Content: text(content), IfMatch: e.ownToolsetRevision(t, "coding/lean")}); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	wg.Wait()
}

func mustLoadCoder(e editFixture) (SessionConfig, error) {
	return LoadSessionSettings(e.location, "dev/coder", "")
}
