package app

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// A compaction sent while a run of the profile is live waits for the run, on the HTTP compact
// route and on the native profile tool alike, as DECISIONS.md records: neither refuses it and
// neither returns before the run ends. While a compaction holds the profile's gate, a second
// mutation of the same profile is refused at once with the same 409 on both paths.
func TestToolAndHTTPCompactWaitForALiveRunAlike(t *testing.T) {
	f := newHeldResetFixture(t)
	run := f.startHeldRun()
	f.awaitRunActive()
	toolDone := make(chan error, 1)
	go func() {
		toolDone <- profileMessenger{resolver: f.service.resolver}.CompactProfile(context.Background(), "local:one", "dev/coder")
	}()
	select {
	case err := <-toolDone:
		t.Fatalf("the tool compact finished during a live run: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	compacted := make(chan [2]any, 1)
	go func() {
		status, body := f.compact()
		compacted <- [2]any{status, body}
	}()
	select {
	case got := <-compacted:
		if got[0] != http.StatusConflict {
			t.Fatalf("HTTP compact while the tool compaction holds the gate = %v %v, want 409", got[0], got[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP compact waited behind the tool compaction instead of being refused by the gate")
	}
	close(f.release)
	if _, err := io.Copy(io.Discard, run); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-toolDone:
		if err != nil {
			t.Fatalf("tool compact after the run ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tool compact did not finish after the run ended")
	}
	if status, body := f.compact(); status != http.StatusOK {
		t.Fatalf("HTTP compact after the run ended = %d %s, want 200", status, body)
	}
}

func (f *heldResetFixture) compact() (int, string) {
	f.t.Helper()
	request, _ := http.NewRequest(http.MethodPost, f.api.URL+"/api/sessions/local:one/compact?profile=dev/coder", nil)
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}
