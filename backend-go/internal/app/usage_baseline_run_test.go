package app

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/webui"
)

// runCompaction is the compaction setting of a run that compacts after its first response
// (automatic) or only when the provider reports an overflow.
func runCompactionSettings(overflow bool, hooks map[string]any) map[string]any {
	compaction := map[string]any{"auto_enabled": true, "trigger_tokens": 1, "keepbatch": map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}}}
	if overflow {
		compaction["trigger_tokens"] = 1000000
		compaction["overflow"] = map[string]any{"enabled": true}
	}
	settings := map[string]any{"max_tool_rounds": 1, "compaction": compaction}
	if hooks != nil {
		settings["hooks"] = hooks
	}
	return settings
}

func hookSessionUsage(t *testing.T, apiURL string) webui.UsageSummary {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, apiURL+"/api/sessions/local:one", nil)
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var snapshot struct {
		Usage *webui.UsageSummary `json:"usage"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &snapshot) != nil {
		t.Fatalf("snapshot: %d %s", response.StatusCode, body)
	}
	if snapshot.Usage == nil {
		return webui.UsageSummary{}
	}
	return *snapshot.Usage
}

func inputOf(usage webui.UsageSummary) int64 {
	if usage.InputTokens == nil {
		return 0
	}
	return *usage.InputTokens
}

// blockBaseline makes the usage baseline impossible to store: its path is a directory.
func blockBaseline(t *testing.T, messagesPath string) (unblock func()) {
	t.Helper()
	path := filepath.Join(filepath.Dir(messagesPath), "staging", "usage-cache.json.baseline")
	if err := os.MkdirAll(filepath.Join(path, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
}

// runFailedLine is the log line of the failed run: the full error the client is not shown.
func runFailedLine(t *testing.T, logs *lockedBuffer) string {
	t.Helper()
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="run failed"`) {
			return line
		}
	}
	t.Fatalf("no run failed line in the log:\n%s", logs.String())
	return ""
}

func noCompactionFiles(t *testing.T, messagesPath string) {
	t.Helper()
	dir := filepath.Dir(messagesPath)
	for _, path := range []string{filepath.Join(dir, "messages.backup.jsonl"), filepath.Join(dir, "staging", "compaction-cache.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after a compaction that failed: %v", path, err)
		}
	}
}

// A compaction inside a run whose usage baseline cannot be stored fails the run, and the error
// names the usage baseline (the log carries the path). Nothing was removed: the transcript still
// begins with the old history, no backup or compaction cache was written, the totals are the
// ones before plus the response that was received, and once the baseline can be stored again
// the next run compacts and the totals keep growing.
func TestAutomaticCompactionFailsTheRunWhenTheBaselineCannotBeStored(t *testing.T) {
	logs := &lockedBuffer{}
	api, messagesPath := startHookRunService(t, runCompactionSettings(false, nil), nil, &recordingProvider{}, logs)
	before := hookSessionUsage(t, api.URL)
	unblock := blockBaseline(t, messagesPath)

	body := postHookRun(t, api.URL, "continue")
	if !strings.Contains(body, "event: error") || strings.Contains(body, "event: completed") {
		t.Fatalf("the run did not fail:\n%s", body)
	}
	if line := runFailedLine(t, logs); !strings.Contains(line, "compact session transcript") || !strings.Contains(line, "usage baseline") || !strings.Contains(line, "usage-cache.json.baseline") {
		t.Errorf("the failed run's error does not identify the usage baseline and its path:\n%s", line)
	}
	transcript := readTranscript(t, messagesPath)
	if !strings.HasPrefix(transcript, preCompactTranscript) || !strings.Contains(transcript, "provider answer") {
		t.Fatalf("the transcript lost history or the received response:\n%s", transcript)
	}
	noCompactionFiles(t, messagesPath)
	after := hookSessionUsage(t, api.URL)
	if after.CompletedResponses != before.CompletedResponses+1 || inputOf(after) != inputOf(before)+100 || after.HistoryPartial {
		t.Fatalf("usage after the failed run = %+v, want %+v plus the one response of 100 input tokens", after, before)
	}
	if again := hookSessionUsage(t, api.URL); again.CompletedResponses != after.CompletedResponses || inputOf(again) != inputOf(after) {
		t.Fatalf("a second read differs: %+v and %+v", again, after)
	}

	unblock()
	if body := postHookRun(t, api.URL, "again"); !strings.Contains(body, "event: completed") {
		t.Fatalf("the run after the baseline was repaired failed:\n%s", body)
	}
	if strings.Contains(readTranscript(t, messagesPath), "old context") {
		t.Error("the repaired run did not compact")
	}
	final := hookSessionUsage(t, api.URL)
	if final.CompletedResponses != after.CompletedResponses+1 || inputOf(final) != inputOf(after)+100 || final.HistoryPartial {
		t.Fatalf("usage after the compacting run = %+v, want %+v plus one response", final, after)
	}
}

// The same for the compaction a provider's overflow starts: the run fails with the baseline
// named, the provider is not asked again, and nothing was removed or counted.
func TestOverflowCompactionFailsTheRunWhenTheBaselineCannotBeStored(t *testing.T) {
	logs := &lockedBuffer{}
	provider := &recordingProvider{overflowFirst: true}
	api, messagesPath := startHookRunService(t, runCompactionSettings(true, nil), nil, provider, logs)
	before := hookSessionUsage(t, api.URL)
	blockBaseline(t, messagesPath)

	body := postHookRun(t, api.URL, "continue")
	if !strings.Contains(body, "event: error") || strings.Contains(body, "event: completed") {
		t.Fatalf("the run did not fail:\n%s", body)
	}
	if line := runFailedLine(t, logs); !strings.Contains(line, "compact session transcript") || !strings.Contains(line, "usage baseline") || !strings.Contains(line, "usage-cache.json.baseline") {
		t.Errorf("the failed run's error does not identify the usage baseline and its path:\n%s", line)
	}
	if got := len(provider.requests()); got != 1 {
		t.Errorf("provider requests = %d, want 1 (no retry after the failed compaction)", got)
	}
	if transcript := readTranscript(t, messagesPath); !strings.HasPrefix(transcript, preCompactTranscript) {
		t.Fatalf("the transcript lost history:\n%s", transcript)
	}
	noCompactionFiles(t, messagesPath)
	if after := hookSessionUsage(t, api.URL); after.CompletedResponses != before.CompletedResponses || inputOf(after) != inputOf(before) {
		t.Fatalf("usage changed by a failed overflow compaction: %+v, was %+v", after, before)
	}
}

// A pre_compact hook that blocks stops the compaction before the baseline is touched: with the
// baseline impossible to store the automatic compaction is skipped, the run completes, and the
// overflow compaction is skipped, so the provider's overflow error stands without a baseline
// error beside it.
func TestBlockedPreCompactNeverReachesTheBaseline(t *testing.T) {
	gate := runHookEntry("keep", "exit 1")
	gate["blocking"] = true
	hooks := map[string]any{"enabled": true, "pre_compact": []any{gate}}

	t.Run("automatic", func(t *testing.T) {
		logs := &lockedBuffer{}
		api, messagesPath := startHookRunService(t, runCompactionSettings(false, hooks), nil, &recordingProvider{}, logs)
		before := hookSessionUsage(t, api.URL)
		blockBaseline(t, messagesPath)
		if body := postHookRun(t, api.URL, "continue"); !strings.Contains(body, "event: completed") || strings.Contains(body, "event: error") {
			t.Fatalf("the run did not complete:\n%s", body)
		}
		if strings.Contains(logs.String(), "usage baseline") {
			t.Errorf("the baseline was touched:\n%s", logs.String())
		}
		if !strings.Contains(readTranscript(t, messagesPath), "old context") {
			t.Error("a blocked compaction removed history")
		}
		if after := hookSessionUsage(t, api.URL); after.CompletedResponses != before.CompletedResponses+1 || inputOf(after) != inputOf(before)+100 {
			t.Fatalf("usage = %+v, want %+v plus one response", after, before)
		}
	})
	t.Run("overflow", func(t *testing.T) {
		logs := &lockedBuffer{}
		provider := &recordingProvider{overflowFirst: true}
		api, messagesPath := startHookRunService(t, runCompactionSettings(true, hooks), nil, provider, logs)
		before := hookSessionUsage(t, api.URL)
		blockBaseline(t, messagesPath)
		body := postHookRun(t, api.URL, "continue")
		if !strings.Contains(body, "event: error") || strings.Contains(body, "event: completed") {
			t.Fatalf("the provider's overflow did not fail the run:\n%s", body)
		}
		if strings.Contains(logs.String(), "usage baseline") || strings.Contains(body, "usage baseline") {
			t.Errorf("the baseline was touched:\n%s\n%s", body, logs.String())
		}
		if got := len(provider.requests()); got != 1 {
			t.Errorf("provider requests = %d, want 1", got)
		}
		if transcript := readTranscript(t, messagesPath); !strings.HasPrefix(transcript, preCompactTranscript) {
			t.Fatalf("a blocked compaction removed history:\n%s", transcript)
		}
		noCompactionFiles(t, messagesPath)
		if after := hookSessionUsage(t, api.URL); after.CompletedResponses != before.CompletedResponses || inputOf(after) != inputOf(before) {
			t.Fatalf("usage changed: %+v, was %+v", after, before)
		}
	})
}
