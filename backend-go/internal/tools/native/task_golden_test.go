package native

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"jad/backend-go/internal/tools"
)

func TestNativeTaskOutputMatchesNodeMessagesJSONLGolden(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	tool, err := NewTaskTool(settings, NewTaskStore(settings.StorageDir))
	if err != nil {
		t.Fatal(err)
	}
	arguments := tool.NormalizeArguments(map[string]any{"title": "Plan the next change"})
	output, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "call-task-add"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}{Type: "function_call_output", CallID: "call-task-add", Output: output})
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "messages-native-task-add.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(line, want) {
		t.Fatalf("native.task messages.jsonl output differs from Node-generated golden\n got: %s\nwant: %s", line, want)
	}
}

// The two fixtures below were Node-generated schema errors (the allOf rules of the old
// schema). The redesign dropped allOf, so the same calls now fail in Execute with their
// own error codes, and the fixtures were rewritten by hand to that output; the add golden
// above is still the Node one. Node byte parity ends here (decided with the redesign).
func TestNativeTaskSchemaErrorMatchesNodeMessagesJSONLGolden(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	tool, err := NewTaskTool(settings, NewTaskStore(settings.StorageDir))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(struct {
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{CallID: "call-task-invalid", Name: "task", Arguments: `{"action":"add"}`})
	if err != nil {
		t.Fatal(err)
	}
	line, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "messages-native-task-invalid-add.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(line, want) {
		t.Fatalf("native.task schema-error messages.jsonl output differs from Node-generated golden\n got: %s\nwant: %s", line, want)
	}
}

func TestNativeTaskNestedSchemaErrorMatchesNodeMessagesJSONLGolden(t *testing.T) {
	settings := taskTestSettings(filepath.Join(t.TempDir(), "tasks"))
	tool, err := NewTaskTool(settings, NewTaskStore(settings.StorageDir))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(struct {
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{CallID: "call-task-update-invalid", Name: "task", Arguments: `{"action":"update","id":"T-001"}`})
	if err != nil {
		t.Fatal(err)
	}
	line, err := catalog.ExecuteFunctionCall(context.Background(), call, tools.ExecutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "messages-native-task-invalid-update.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(line, want) {
		t.Fatalf("native.task nested schema-error output differs from Node-generated golden\\n got: %s\\nwant: %s", line, want)
	}
}
