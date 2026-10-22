package sessionfs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSubagentPersistRunsDefaultsToOnAndCanBeTurnedOff(t *testing.T) {
	on, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || !on.PersistRuns {
		t.Fatalf("default persist_runs = %v, %v; want true", on != nil && on.PersistRuns, err)
	}
	off, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"persist_runs":false,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || off.PersistRuns {
		t.Fatalf("persist_runs false = %v, %v", off != nil && off.PersistRuns, err)
	}
	explicit, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"persist_runs":null,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || !explicit.PersistRuns {
		t.Fatalf("persist_runs null = %v, %v; want the default true", explicit != nil && explicit.PersistRuns, err)
	}
}

func TestSubagentPersistRunsRejectsANonBoolean(t *testing.T) {
	for _, value := range []string{`"yes"`, `1`, `{}`} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"persist_runs":`+value+`,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.subagent.persist_runs must be a boolean") {
			t.Errorf("persist_runs %s: err = %v", value, err)
		}
	}
}

func TestSubagentStateContextAcceptsTheLostState(t *testing.T) {
	got, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"statecontext":{"states":["running","lost"]},"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || strings.Join(got.StateContext.States, ",") != "running,lost" {
		t.Fatalf("states = %+v, %v", got, err)
	}
}

func TestSubagentDeleteTranscriptsDefaultsToOffAndCanBeTurnedOn(t *testing.T) {
	off, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || off.DeleteTranscripts {
		t.Fatalf("default delete_transcripts = %v, %v; want false", off != nil && off.DeleteTranscripts, err)
	}
	on, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"delete_transcripts":true,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || !on.DeleteTranscripts {
		t.Fatalf("delete_transcripts true = %v, %v", on != nil && on.DeleteTranscripts, err)
	}
	null, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"delete_transcripts":null,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || null.DeleteTranscripts {
		t.Fatalf("delete_transcripts null = %v, %v; want the default false", null != nil && null.DeleteTranscripts, err)
	}
}

func TestSubagentDeleteTranscriptsRejectsANonBoolean(t *testing.T) {
	for _, value := range []string{`"yes"`, `1`, `{}`} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"delete_transcripts":`+value+`,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.subagent.delete_transcripts must be a boolean") {
			t.Errorf("delete_transcripts %s: err = %v", value, err)
		}
	}
}

func TestSubagentAllowWriteDefaultsToOffAndNeedsABoolean(t *testing.T) {
	off, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || off.AllowWrite {
		t.Fatalf("default allow_write = %v, %v; want false", off != nil && off.AllowWrite, err)
	}
	null, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"allow_write":null,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || null.AllowWrite {
		t.Fatalf("allow_write null = %v, %v; want false", null != nil && null.AllowWrite, err)
	}
	on, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"allow_write":true,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
	if err != nil || !on.AllowWrite {
		t.Fatalf("allow_write true = %v, %v", on != nil && on.AllowWrite, err)
	}
	for _, value := range []string{`"yes"`, `1`, `{}`} {
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"allow_write":`+value+`,"agents":{"a":{}}}`), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), "tools.native.subagent.allow_write must be a boolean") {
			t.Errorf("allow_write %s: err = %v", value, err)
		}
	}
}

func TestSubagentAgentListingAChangeToolNeedsAllowWrite(t *testing.T) {
	for _, tool := range []string{"write", "edit", "bash", "exec"} {
		setting := `{"agents":{"fixer":{"tools":["read","` + tool + `"]}}}`
		_, err := normalizeNativeSubagentSettings(subagentSettingsFor(setting), "/s/setting.json", "/w", "")
		if err == nil || !strings.Contains(err.Error(), "agent fixer lists "+tool) || !strings.Contains(err.Error(), "allow_write") {
			t.Errorf("without allow_write, %s: err = %v", tool, err)
		}
		withFlag := `{"allow_write":true,"agents":{"fixer":{"tools":["read","` + tool + `"]}}}`
		got, err := normalizeNativeSubagentSettings(subagentSettingsFor(withFlag), "/s/setting.json", "/w", "")
		if err != nil || !got.Agents[0].ChangesFiles() {
			t.Errorf("with allow_write, %s: %v, %v", tool, got, err)
		}
	}
	// Several change tools are all named, in list order.
	_, err := normalizeNativeSubagentSettings(subagentSettingsFor(`{"agents":{"fixer":{"tools":["bash","read","edit"]}}}`), "/s/setting.json", "/w", "")
	if err == nil || !strings.Contains(err.Error(), "lists bash, edit in tools") {
		t.Errorf("err = %v", err)
	}
	// Read-only lists and no list are fine without the flag.
	for _, ok := range []string{`{"agents":{"a":{"tools":["read","grep"]}}}`, `{"agents":{"a":{}}}`} {
		if _, err := normalizeNativeSubagentSettings(subagentSettingsFor(ok), "/s/setting.json", "/w", ""); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func TestSubagentChangeToolsOfAnAgentFolderAreCheckedToo(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "fixer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixer", "agent.json"), []byte(`{"schema_version":1,"tools":["read","write"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	setting := `{"agents_dir":[` + strconv.Quote(dir) + `]}`
	if _, err := normalizeNativeSubagentSettings(subagentSettingsFor(setting), "/s/setting.json", "/w", ""); err == nil || !strings.Contains(err.Error(), "agent fixer lists write") {
		t.Fatalf("a folder agent got past the check: %v", err)
	}
	withFlag := `{"allow_write":true,"agents_dir":[` + strconv.Quote(dir) + `]}`
	if got, err := normalizeNativeSubagentSettings(subagentSettingsFor(withFlag), "/s/setting.json", "/w", ""); err != nil || !got.Agents[0].ChangesFiles() {
		t.Fatalf("folder agent with allow_write: %v, %v", got, err)
	}
}
