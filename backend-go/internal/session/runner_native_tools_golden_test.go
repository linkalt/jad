package session

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

//go:embed testdata/native-write-edit.messages.jsonl
var nativeWriteEditMessagesGolden []byte

//go:embed testdata/native-request-draft.messages.jsonl
var nativeRequestDraftMessagesGolden []byte

var transcriptBatchIDPattern = regexp.MustCompile(`"__agent_session_input_batch_id":"[0-9a-f]{32}"`)

func TestRunnerNativeWriteEditMessagesJSONLMatchesNodeGolden(t *testing.T) {
	store, journal, stagingDir := newTestStore(t, transcript.ResponseMetadataFull)
	workspace := filepath.Dir(stagingDir)
	backupDir := filepath.Join(workspace, "backups")
	mutationSettings := sessionfs.NativeMutationSettings{
		BackupDir:           &backupDir,
		BackupMaxCount:      5,
		BackupMaxTotalBytes: 10 * 1024 * 1024,
		Fsync:               true,
	}
	policy := sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}
	locks := native.NewPathLocks()
	writeTool, err := native.NewWriteTool(sessionfs.NativeWriteSettings{
		Policy: policy, MaxBytes: 10 * 1024 * 1024, NativeMutationSettings: mutationSettings,
	}, locks)
	if err != nil {
		t.Fatal(err)
	}
	editTool, err := native.NewEditTool(sessionfs.NativeEditSettings{
		Policy: policy, MaxBytes: 1024 * 1024, NativeMutationSettings: mutationSettings,
	}, locks)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{writeTool, editTool})
	if err != nil {
		t.Fatal(err)
	}

	writeCall := json.RawMessage(`{"type":"function_call","call_id":"call-write","name":"write","arguments":"{\"path\":\"golden.txt\",\"content\":\"before\\n\"}"}`)
	editCall := json.RawMessage(`{"type":"function_call","call_id":"call-edit","name":"edit","arguments":"{\"path\":\"golden.txt\",\"oldText\":\"before\\n\",\"newText\":\"before\\n\"}"}`)
	finalMessage := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}`)
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{
		goldenResponseEvents("golden-response-1", writeCall),
		goldenResponseEvents("golden-response-2", editCall),
		goldenResponseEvents("golden-response-3", finalMessage),
	}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), json.RawMessage(`"write and edit"`), RunOptions{
		TurnID:        "golden-native-tools",
		ToolCatalog:   catalog,
		ToolExecution: tools.ExecutionContext{WorkspaceRoot: workspace, SessionDir: workspace},
		MaxToolRounds: 2,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.TurnID != "golden-native-tools" || len(streamer.requests) != 3 {
		t.Fatalf("turn=%q provider requests=%d", result.TurnID, len(streamer.requests))
	}
	content, err := os.ReadFile(filepath.Join(workspace, "golden.txt"))
	if err != nil || string(content) != "before\n" {
		t.Fatalf("golden file = %q, error = %v", content, err)
	}

	var transcriptBytes bytes.Buffer
	if err := journal.ReadLines(func(line []byte) error {
		transcriptBytes.Write(line)
		transcriptBytes.WriteByte('\n')
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := transcriptBatchIDPattern.ReplaceAll(transcriptBytes.Bytes(), []byte(`"__agent_session_input_batch_id":"<batch-id>"`))
	want := strings.ReplaceAll(string(nativeWriteEditMessagesGolden), "\r\n", "\n")
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("messages.jsonl differs from Node golden after normalizing generated batch IDs\n got: %s\nwant: %s", got, want)
	}
}

func TestRunnerNativeRequestDraftMessagesJSONLMatchesNodeGolden(t *testing.T) {
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataFull)
	tool, err := native.NewRequestDraftTool(sessionfs.NativeRequestDraftSettings{
		TimeoutMS: 5_000, HeadlessAutoAcceptTimeoutMS: 1_000, MaxTimeoutMS: 10_000, ExtendMS: 2_000,
		MaxDraftBytes: 32 * 1024, MaxDraftDepth: 8, MaxTitleChars: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	call := json.RawMessage(`{"type":"function_call","call_id":"call-request-draft","name":"request_draft","arguments":"{\"title\":\"Clarify intent\",\"draft\":{\"goal\":\"implement\",\"scope\":[\"native.request_draft\"]}}"}`)
	finalMessage := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Proceed with the clarified request."}]}`)
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{
		goldenResponseEvents("golden-request-draft-1", call),
		goldenResponseEvents("golden-request-draft-2", finalMessage),
	}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), json.RawMessage(`"clarify ambiguous task"`), RunOptions{
		TurnID:        "golden-request-draft",
		ToolCatalog:   catalog,
		MaxToolRounds: 1,
		ToolExecution: tools.ExecutionContext{
			RequestInteraction: func(_ context.Context, identity tools.InteractionIdentity, question tools.InteractionQuestion) (tools.InteractionAnswer, error) {
				if identity.Tool != "request_draft" || identity.ToolCallID != "call-request-draft" || question.Title != "Clarify intent" || string(question.Draft) != `{"goal":"implement","scope":["native.request_draft"]}` {
					t.Errorf("interaction identity=%+v question=%+v", identity, question)
				}
				return tools.InteractionAnswer{
					Decision: tools.InteractionUpdate,
					Outcome:  tools.InteractionAnswered,
					Source:   tools.InteractionSourceUser,
					Draft:    json.RawMessage(`{"goal":"implement request_draft","scope":["native.request_draft","interaction API"]}`),
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.TurnID != "golden-request-draft" || len(streamer.requests) != 2 {
		t.Fatalf("turn=%q provider requests=%d", result.TurnID, len(streamer.requests))
	}

	var transcriptBytes bytes.Buffer
	if err := journal.ReadLines(func(line []byte) error {
		transcriptBytes.Write(line)
		transcriptBytes.WriteByte('\n')
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := transcriptBatchIDPattern.ReplaceAll(transcriptBytes.Bytes(), []byte(`"__agent_session_input_batch_id":"<batch-id>"`))
	want := strings.ReplaceAll(string(nativeRequestDraftMessagesGolden), "\r\n", "\n")
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("messages.jsonl differs from Node golden after normalizing generated batch IDs\n got: %s\nwant: %s", got, want)
	}
}

func goldenResponseEvents(id string, item json.RawMessage) []responses.Event {
	return []responses.Event{
		{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + string(item) + `}`)},
		{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"` + id + `","object":"response","status":"completed","model":"gpt-test","output":[` + string(item) + `]}}`)},
	}
}
