package session

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"os/exec"
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

//go:embed testdata/native-exec.messages.jsonl
var nativeExecMessagesGolden []byte

// The run time and the content fence token change on every run; the golden holds placeholders.
var (
	execDurationPattern = regexp.MustCompile(`completed in [0-9]+ ms`)
	execTokenPattern    = regexp.MustCompile(`token=[A-Za-z0-9_-]+`)
)

// An exec call and its result are written to messages.jsonl in this exact form. exec replaced
// the file, search and shell tools, so this is the golden of the tool most calls now go to.
// Change the golden only for a requested format change.
func TestRunnerNativeExecMessagesJSONLMatchesGolden(t *testing.T) {
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	workspace := t.TempDir()
	store, journal, _ := newTestStore(t, transcript.ResponseMetadataFull)
	tool, err := native.NewExecTool(sessionfs.NativeExecSettings{
		Child: sessionfs.NativeBashSettings{
			Policy: sessionfs.NativePathPolicy{Paths: []sessionfs.NativePathRule{}}, Shell: shell,
			TimeoutMs: 20_000, MaxTimeoutMs: 20_000, KillGraceMs: 100, KillProcessGroup: "always", MaxBytes: 2048,
			Stdout: sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512}, Stderr: sessionfs.NativeBashStreamBudget{HeadBytes: 512, TailBytes: 512},
			Spill:        sessionfs.NativeBashSpillSettings{Keep: "truncated_only", CapBytes: 1024, StorageDir: filepath.Join(workspace, ".exec-output")},
			MaxLineChars: 2000, StdinMaxBytes: 64 * 1024, StripANSI: true, Env: map[string]string{"NO_COLOR": "1"}, EnvDeny: []string{},
		},
		LangNames:   []string{"bash"},
		Langs:       map[string]sessionfs.NativeExecLang{"bash": {Argv: []string{"bash", "-c", "{code}", "exec"}, Mode: sessionfs.NativeExecModeArg, Ext: ".sh", Aliases: []string{}, LibEnv: "PATH"}},
		DefaultLang: "bash", CodeMaxBytes: 64 * 1024, InputMaxBytes: 4096, ArgMaxBytes: 64 * 1024, ResultMaxBytes: 1024, LineAware: true,
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	call := json.RawMessage(`{"type":"function_call","call_id":"call-exec","name":"exec","arguments":"{\"code\":\"printf 'alpha\\\\nbeta\\\\n'; echo warn >&2\"}"}`)
	finalMessage := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Printed two lines."}]}`)
	streamer := &multiRoundStreamer{rounds: [][]responses.Event{
		goldenResponseEvents("golden-exec-1", call),
		goldenResponseEvents("golden-exec-2", finalMessage),
	}}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), json.RawMessage(`"print two lines"`), RunOptions{
		TurnID:        "golden-exec",
		ToolCatalog:   catalog,
		MaxToolRounds: 1,
		ToolExecution: tools.ExecutionContext{WorkspaceRoot: workspace, SessionDir: workspace},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.TurnID != "golden-exec" || len(streamer.requests) != 2 {
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
	got = execDurationPattern.ReplaceAll(got, []byte(`completed in <ms> ms`))
	got = execTokenPattern.ReplaceAll(got, []byte(`token=<token>`))
	want := strings.ReplaceAll(string(nativeExecMessagesGolden), "\r\n", "\n")
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("messages.jsonl differs from the golden after normalizing batch IDs, run times and tokens\n got: %s\nwant: %s", got, want)
	}
}
