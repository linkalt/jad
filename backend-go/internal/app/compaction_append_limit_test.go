package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// seedAppBackup writes n old user turns into the transcript backup next to messagesPath and
// returns its path.
func seedAppBackup(t *testing.T, messagesPath string, n int) string {
	t.Helper()
	var builder strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&builder, `{"type":"message","role":"user","content":[{"type":"input_text","text":"seed-%d"}]}`+"\n", i)
		fmt.Fprintf(&builder, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"seed-answer-%d"}]}`+"\n", i)
	}
	path := filepath.Join(filepath.Dir(messagesPath), "messages.backup.jsonl")
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func appBackupUserTexts(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"role":"user"`) {
			continue
		}
		start := strings.Index(line, `"text":"`) + len(`"text":"`)
		texts = append(texts, line[start:start+strings.Index(line[start:], `"`)])
	}
	return strings.Join(texts, ",")
}

func compactionSettings(backupMode string, keepbackup map[string]any) map[string]any {
	compaction := map[string]any{
		"backup_mode": backupMode,
		"keepbatch":   map[string]any{"turn": map[string]any{"type": "assistant_final", "keep": 1}},
	}
	if keepbackup != nil {
		compaction["keepbackup"] = keepbackup
	}
	return map[string]any{"compaction": compaction}
}

// keepbackup.append_limit reaches the backup of a manual compaction, which is also the
// compaction profile_message's context_trigger runs. The seeded backup holds 3 turns and
// the transcript 2, so 5 are appended.
func TestManualCompactionRollsTheAppendBackupByAppendLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		keepbackup map[string]any
		want       string
	}{
		{"unlimited by default", "append", nil, "seed-1,seed-2,seed-3,old context,latest context"},
		{"limit 0", "append", map[string]any{"append_limit": 0}, "seed-1,seed-2,seed-3,old context,latest context"},
		{"limit 3", "append", map[string]any{"append_limit": 3}, "seed-3,old context,latest context"},
		{"limit 1", "append", map[string]any{"append_limit": 1}, "latest context"},
		{"replace ignores the limit", "replace", map[string]any{"append_limit": 1}, "old context,latest context"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, api, messagesPath := startPreCompactService(t, compactionSettings(tc.mode, tc.keepbackup), nil, &lockedBuffer{})
			backup := seedAppBackup(t, messagesPath, 3)
			response := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
			if body := readMutationResponse(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, `"compacted":true`) {
				t.Fatalf("compact status=%d body=%s", response.StatusCode, body)
			}
			if got := appBackupUserTexts(t, backup); got != tc.want {
				t.Fatalf("backup user turns = %q, want %q", got, tc.want)
			}
		})
	}
}

// A backup with a line that is not a JSON object stops the manual compaction with a
// clear error: nothing is compacted, and the backup and the transcript are as they were.
func TestManualCompactionWithAppendLimitStopsOnACorruptBackup(t *testing.T) {
	logs := &lockedBuffer{}
	_, api, messagesPath := startPreCompactService(t, compactionSettings("append", map[string]any{"append_limit": 2}), nil, logs)
	backup := filepath.Join(filepath.Dir(messagesPath), "messages.backup.jsonl")
	corrupt := `{"type":"message","role":"user","content":[]}` + "\n{oops\n"
	if err := os.WriteFile(backup, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript, err := os.ReadFile(messagesPath)
	if err != nil {
		t.Fatal(err)
	}
	response := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	body := readMutationResponse(t, response)
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("compact status=%d body=%s; want a server error", response.StatusCode, body)
	}
	if !strings.Contains(logs.String(), "messages.backup.jsonl: record at byte") {
		t.Fatalf("the log does not name the backup file and byte:\n%s", logs.String())
	}
	if got, _ := os.ReadFile(backup); string(got) != corrupt {
		t.Fatalf("the backup was changed: %q", got)
	}
	if got, _ := os.ReadFile(messagesPath); string(got) != string(transcript) {
		t.Fatalf("the transcript was changed: %q", got)
	}
}

// Settings written for the removed compaction.summary and keepbackup.min_turns still load:
// they are ignored. The compaction sends no summary request and a replace backup is
// overwritten whatever min_turns says.
func TestRemovedSummaryAndMinTurnsSettingsAreIgnored(t *testing.T) {
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer provider.Close()
	settings := compactionSettings("replace", map[string]any{"min_turns": 100})
	settings["compaction"].(map[string]any)["summary"] = map[string]any{"enabled": true, "prompt": "SUMMARIZE", "on_failure": "skip", "unknown": 1}
	_, api, messagesPath := startPreCompactService(t, settings, provider, &lockedBuffer{})
	backup := seedAppBackup(t, messagesPath, 5)
	response := postEmptyMutation(t, api.URL+"/api/sessions/local:one/compact", integrationAPIToken)
	if body := readMutationResponse(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, `"compacted":true`) {
		t.Fatalf("compact status=%d body=%s", response.StatusCode, body)
	}
	if got := providerCalls.Load(); got != 0 {
		t.Fatalf("provider requests = %d, want none: the summary setting was removed", got)
	}
	if got := appBackupUserTexts(t, backup); got != "old context,latest context" {
		t.Fatalf("replace backup user turns = %q, want the transcript only: min_turns was removed", got)
	}
	cache := ""
	_ = filepath.Walk(filepath.Dir(messagesPath), func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.Contains(filepath.Base(path), "compaction") && strings.HasSuffix(path, ".json") {
			data, _ := os.ReadFile(path)
			cache += string(data)
		}
		return nil
	})
	if strings.Contains(cache, "Summary of the earlier conversation") || strings.Contains(cache, "SUMMARIZE") {
		t.Fatalf("a summary was stored: %s", cache)
	}
}

// The run's automatic compaction (bound_runner) takes append_limit from the session.
func TestAutomaticCompactionInARunRollsTheAppendBackupByAppendLimit(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`,
			`{"type":"response.completed","response":{"id":"resp-auto","status":"completed","model":"gpt-test","usage":{"input_tokens":100},"output":[]}}`,
			"[DONE]",
		} {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	defer provider.Close()
	settings := compactionSettings("append", map[string]any{"append_limit": 2})
	settings["max_tool_rounds"] = 1
	settings["compaction"].(map[string]any)["auto_enabled"] = true
	settings["compaction"].(map[string]any)["trigger_tokens"] = 1
	_, api, messagesPath := startPreCompactService(t, settings, provider, &lockedBuffer{})
	backup := seedAppBackup(t, messagesPath, 4)

	request, err := http.NewRequest(http.MethodPost, api.URL+"/api/sessions/local:one/run", strings.NewReader(`{"input":"continue"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("run status=%d body=%s", response.StatusCode, body)
	}
	got := appBackupUserTexts(t, backup)
	if n := len(strings.Split(got, ",")); n != 2 || strings.Contains(got, "seed-") {
		t.Fatalf("backup user turns = %q, want the newest 2 and none of the seeded ones", got)
	}
}

// A sub-agent's automatic compaction carries the session's append_limit like its other
// backup settings. A sub-agent's transcript store keeps no backup (see subagent.go), so
// today the value has no file to act on; this pins that the settings agree.
func TestSubagentCompactionSettingsCarryTheAppendLimit(t *testing.T) {
	base := sessionfs.SessionConfig{Compaction: sessionfs.Compaction{TurnType: "assistant_final", KeepTurns: 1, BackupMode: "append", AppendLimit: 7}}
	settings := compactionFor(base, sessionfs.NativeSubagentAgent{})
	if settings.AppendLimit != 7 || settings.BackupMode != "append" {
		t.Fatalf("sub-agent compaction settings = %+v", settings)
	}
}
