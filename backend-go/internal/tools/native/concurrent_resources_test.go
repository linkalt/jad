package native

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Tool calls of one provider response run concurrently, so the shared stores
// must keep their capacity checks atomic with the write that follows them.

func TestFetchStoreConcurrentCreatesCannotOvershootStorageCapacity(t *testing.T) {
	store := NewFetchStore(filepath.Join(t.TempDir(), "fetch"))
	policy := fetchTestSettings("")
	// Room for one stored document (content plus metadata) but not two.
	policy.MaxStorageBytes = 800
	document := FetchDocument{
		URL: "https://example.test/", FinalURL: "https://example.test/",
		ContentType: "text/html", Status: 200, Markdown: strings.Repeat("x", 400),
	}
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for index := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[index] = store.Create(document, policy)
		}()
	}
	wg.Wait()
	stored := 0
	for _, err := range errs {
		if err == nil {
			stored++
		}
	}
	if stored != 1 {
		t.Fatalf("stored documents = %d, want exactly 1 (errors: %v)", stored, errs)
	}
}

var startedProcessLine = regexp.MustCompile(`^proc_`)

func TestProcessManagerConcurrentStartsCannotExceedMaxRunningAndRefusalsFreeTheirSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed process groups require a POSIX platform")
	}
	directory := t.TempDir()
	manager := NewProcessManager(filepath.Join(directory, "processes"))
	defer manager.Close()

	request := processTestRequest(directory, "sleep 30", 1024)
	request.Policy.MaxRunning = 2

	type result struct {
		snapshot ProcessSnapshot
		err      error
	}
	results := make([]result, 6)
	var wg sync.WaitGroup
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[index].snapshot, results[index].err = manager.Start(request)
		}()
	}
	wg.Wait()

	var started []ProcessSnapshot
	for _, outcome := range results {
		if outcome.err == nil {
			started = append(started, outcome.snapshot)
		}
	}
	if len(started) != 2 {
		t.Fatalf("started processes = %d, want exactly max_running (2)", len(started))
	}
	for _, process := range started {
		if !startedProcessLine.MatchString(process.ID) {
			t.Fatalf("unexpected process id %q", process.ID)
		}
	}
	if running := manager.List("running"); len(running) != 2 {
		t.Fatalf("running processes = %d, want 2", len(running))
	}

	// The four refused starts must not have leaked a reserved slot: once a
	// process ends, a new one is accepted again.
	if _, found := manager.Stop(started[0].ID, "stopped_by_agent"); !found {
		t.Fatal("Stop() did not find the first process")
	}
	again, err := manager.Start(request)
	if err != nil {
		t.Fatalf("Start() after a slot was freed: %v", err)
	}
	if again.ID == "" {
		t.Fatal("Start() returned no process id")
	}
}
