package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/transcript"
)

// A sub-agent's child transcript inherits the parent session's persistence.meta: its task, a
// user message, gets the stored time when the setting is on (the default) and nothing when off.
func TestSubagentChildTranscriptInheritsTheUserTimestamp(t *testing.T) {
	for _, on := range []bool{true, false} {
		agent := map[string]any{"model": "child.json", "__subagent_settings": map[string]any{
			"agents": map[string]any{
				"explore": map[string]any{"model": "openai/child", "description": "Finds facts.", "tools": []string{"skill"}},
			},
		}}
		if !on {
			agent["__session"] = map[string]any{"persistence": map[string]any{"meta": map[string]any{"user_timestamp": false}}}
		}
		f := buildSubagentFixture(t, agent, false, nil)
		f.provider.parentFirstCall = [2]string{"subagent", `{"task":"look around","agent":"explore"}`}
		f.provider.childFirstCall = [2]string{"skill", notesCall}
		if body := f.run(t); !strings.Contains(body, "event: completed") {
			t.Fatalf("on=%v: run did not complete:\n%s", on, body)
		}
		children := 0
		err := filepath.WalkDir(f.toolDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || entry.Name() != "messages.jsonl" {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !strings.Contains(line, "look around") || strings.Contains(line, "response.completed") {
					continue
				}
				children++
				_, stamped := transcript.MetaValue([]byte(line), transcript.MetaStoredAt, 0)
				if stamped != on {
					t.Errorf("on=%v: child task record stamped=%v: %s", on, stamped, line)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if children != 1 {
			t.Fatalf("on=%v: child task records = %d", on, children)
		}
		parent, err := os.ReadFile(filepath.Join(f.stateDir, "messages.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(parent), transcript.MetaField) != on {
			t.Fatalf("on=%v: parent transcript:\n%s", on, parent)
		}
	}
}
