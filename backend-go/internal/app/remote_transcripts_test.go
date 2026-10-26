package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/remoteapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
	"jad/backend-go/internal/tools/native"
)

func remoteUser(text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}, "__agent_session_input_batch_id": "b", "__agent_session_input_batch_index": 0})
	return string(raw) + "\n"
}

func remoteAnswer(turn int, text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "response.completed", "session_turn": turn, "session_step": 1, "response": map[string]any{"id": "r", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}}})
	return string(raw) + "\n"
}

var remoteTranscriptJSONL = remoteUser("what is the secret question") + remoteAnswer(1, "the answer is forty-two")

func transcriptSettings(allow bool, names ...string) sessionfs.NativeTranscriptSettings {
	return sessionfs.NativeTranscriptSettings{
		ResponseMaxBytes: 64 << 10, LineMaxBytes: 1 << 20, ScanMaxBytes: 16 << 20, SuggestMax: 20, TimeoutMs: 10_000,
		PreviewBytes: 200, ValueBytes: 80, MaxListEntries: 100, MaxDetailBytes: 16 << 10,
		AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true, AllowRemote: allow, Remotes: names,
	}
}

func transcriptsSettings(allow bool, names ...string) sessionfs.NativeTranscriptsSettings {
	return sessionfs.NativeTranscriptsSettings{MaxEntries: 50, ScanMaxEntries: 2000, TimeoutMs: 10_000, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true, AllowRemote: allow, Remotes: names}
}

// writeRemoteTranscript puts a messages.jsonl into the server's profile and returns its path.
func writeRemoteTranscript(t *testing.T, server *remoteServer, profile, content string) string {
	t.Helper()
	target, err := profileMessenger{resolver: server.service.resolver}.Profile(context.Background(), profileMessageOwner, "demo", profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target.Files.Messages), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Files.Messages, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return target.Files.Messages
}

type remoteReaders struct {
	transcript, transcripts tools.Tool
	client                  *remoteClient
}

func startRemoteReaders(t *testing.T, server *remoteServer, allow bool, names ...string) remoteReaders {
	t.Helper()
	client := startRemoteClient(t, server.url, writeServerCA(t, server), "")
	directory := client.hub.Directory(client.dir)
	transcript, err := native.NewTranscriptToolWithRemotes(transcriptSettings(allow, names...), native.TranscriptFiles{}, nil, nil, directory)
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := native.NewTranscriptsToolWithRemotes(transcriptsSettings(allow, names...), native.TranscriptFiles{}, nil, nil, nil, directory)
	if err != nil {
		t.Fatal(err)
	}
	return remoteReaders{transcript: transcript, transcripts: transcripts, client: client}
}

func (r remoteReaders) run(t *testing.T, tool tools.Tool, arguments map[string]any) string {
	t.Helper()
	out, err := tool.Execute(context.Background(), arguments, tools.ExecutionContext{CallID: "call", SessionKey: r.client.owner})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var remoteCursorLine = regexp.MustCompile(`(?m)^cursor: (\S+)$`)

func TestRemoteTranscriptsListAndReadWithoutPathsOrTokens(t *testing.T) {
	server, _ := e2eServer(t)
	file := writeRemoteTranscript(t, server, "dev/coder", remoteTranscriptJSONL)
	readers := startRemoteReaders(t, server, true, "peer")

	listed := readers.run(t, readers.transcripts, map[string]any{"path": "remote:peer/session:demo"})
	if !strings.Contains(listed, "profile dev/coder") || !strings.Contains(listed, "path remote:peer/profile:demo/dev/coder") {
		t.Fatalf("listing:\n%s", listed)
	}
	for _, leak := range []string{file, filepath.Dir(file), server.f.workspace, "messages.jsonl", e2eToken, "| file "} {
		if strings.Contains(listed, leak) {
			t.Fatalf("the listing leaks %q:\n%s", leak, listed)
		}
	}

	again := readers.run(t, readers.transcripts, map[string]any{"path": "remote:peer"})
	if !strings.Contains(again, "remote:peer/profile:demo/dev/coder") {
		t.Fatalf("remote:<name> alone lists the default session:\n%s", again)
	}

	read := readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:demo/dev/coder"})
	if !strings.Contains(read, "forty-two") || !strings.Contains(read, "newest step first") {
		t.Fatalf("list:\n%s", read)
	}
	if strings.Contains(read, "transcript: ") || strings.Contains(read, file) || strings.Contains(read, "messages.jsonl") {
		t.Fatalf("a remote read must show no file:\n%s", read)
	}
	if !strings.Contains(read, `remote:peer/profile:demo/dev/coder`) {
		t.Fatalf("the header must name the path to reuse:\n%s", read)
	}

	detail := readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:demo/dev/coder", "step": "1.1"})
	if !strings.Contains(detail, "the answer is forty-two") {
		t.Fatalf("detail:\n%s", detail)
	}
	short := readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:dev/coder"})
	if !strings.Contains(short, "forty-two") {
		t.Fatalf("default_session fills the session:\n%s", short)
	}
}

func TestRemoteTranscriptCursorIsWrappedAndContinues(t *testing.T) {
	server, _ := e2eServer(t)
	var lines strings.Builder
	for i := 0; i < 70; i++ {
		lines.WriteString(remoteUser(fmt.Sprintf("question %d", i)))
		lines.WriteString(remoteAnswer(i+1, fmt.Sprintf("answer %d", i)))
	}
	writeRemoteTranscript(t, server, "dev/coder", lines.String())
	readers := startRemoteReaders(t, server, true, "peer")
	first := readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:demo/dev/coder"})
	match := remoteCursorLine.FindStringSubmatch(first)
	if match == nil || !strings.HasPrefix(match[1], "remote:peer/") {
		t.Fatalf("140 steps exceed one page, so a cursor with the remote prefix is expected:\n%s", first)
	}
	if !strings.Contains(first, "70.1 ") || !strings.Contains(first, "21.0 ") || strings.Contains(first, "20.1 ") {
		t.Fatalf("first page is the newest steps:\n%s", first)
	}
	next := readers.run(t, readers.transcript, map[string]any{"cursor": match[1]})
	if strings.Contains(next, "error_code") || !strings.Contains(next, "20.1 ") || !strings.Contains(next, "1.0 ") || strings.Contains(next, "21.0 ") {
		t.Fatalf("the cursor continues toward older steps:\n%s", next)
	}
	if out := readers.run(t, readers.transcript, map[string]any{"cursor": match[1], "path": "remote:peer/profile:demo/dev/coder"}); !strings.Contains(out, "conflicting_selectors") {
		t.Fatalf("cursor with path:\n%s", out)
	}
	if out := readers.run(t, readers.transcript, map[string]any{"cursor": "remote:peer/@@@"}); !strings.Contains(out, "error_code") {
		t.Fatalf("a forged cursor must fail on the remote, not crash:\n%s", out)
	}
	off := startRemoteReaders(t, server, false)
	if out := off.run(t, off.transcript, map[string]any{"cursor": match[1]}); !strings.Contains(out, "remote_disabled") {
		t.Fatalf("a wrapped cursor is a remote read and obeys the switch:\n%s", out)
	}
}

func TestRemoteTranscriptSwitchOffTokenDeniedAndScope(t *testing.T) {
	f := newSubagentFixture(t, map[string]any{"model": "child.json", "description": "Finds facts."})
	grant := func(extra string) string { return tokenTable(tokenRow("client", e2eToken, extra)) }
	server := startRemoteServer(t, f, grant(`"profiles":["demo/review/*"],"actions":["message","status","stop","transcripts"]`))
	writeRemoteTranscript(t, server, "dev/coder", remoteTranscriptJSONL)

	off := startRemoteReaders(t, server, false)
	out := off.run(t, off.transcript, map[string]any{"path": "remote:peer/profile:demo/dev/coder"})
	if !strings.Contains(out, "remote_disabled") {
		t.Fatalf("the local switch is off by default:\n%s", out)
	}
	if out = off.run(t, off.transcripts, map[string]any{"path": "remote:peer/session:demo"}); !strings.Contains(out, "remote_disabled") {
		t.Fatalf("transcripts too:\n%s", out)
	}

	on := startRemoteReaders(t, server, true, "peer")
	out = on.run(t, on.transcript, map[string]any{"path": "remote:other/profile:demo/dev/coder"})
	if !strings.Contains(out, "unknown_remote") {
		t.Fatalf("a remote not in the tool's list:\n%s", out)
	}
	out = on.run(t, on.transcript, map[string]any{"path": "remote:peer/path:/etc/passwd"})
	if !strings.Contains(out, "bad_remote_reference") || strings.Contains(out, "root:") {
		t.Fatalf("path: is never read on a remote:\n%s", out)
	}
	out = on.run(t, on.transcript, map[string]any{"path": "remote:peer/profile:demo/dev/coder"})
	if !strings.Contains(out, "error_code") || strings.Contains(out, "forty-two") || strings.Contains(out, "demo.json") {
		t.Fatalf("a profile outside the token's globs must not be readable:\n%s", out)
	}
	listed := on.run(t, on.transcripts, map[string]any{"path": "remote:peer/session:demo"})
	if strings.Contains(listed, "dev/coder") {
		t.Fatalf("the listing shows only what the token reaches:\n%s", listed)
	}

	noTranscripts := startRemoteServer(t, f, grant(`"profiles":["*/*/*"],"actions":["message","status","stop"]`))
	denied := startRemoteReaders(t, noTranscripts, true, "peer")
	out = denied.run(t, denied.transcript, map[string]any{"path": "remote:peer/profile:demo/dev/coder"})
	if !strings.Contains(out, "remote_forbidden") || strings.Contains(out, e2eToken) {
		t.Fatalf("a token without the transcripts action:\n%s", out)
	}
}

func TestRemoteTranscriptAPIRejectsUnknownKeysAndPaths(t *testing.T) {
	server, _ := e2eServer(t)
	writeRemoteTranscript(t, server, "dev/coder", remoteTranscriptJSONL)
	get := func(query string) (int, map[string]any) {
		return server.call(t, "GET", remoteapi.Prefix+"/transcript?"+query, e2eToken, nil)
	}
	cases := map[string]string{
		"unknown key":     "path=" + url.QueryEscape("profile:demo/dev/coder") + "&bogus=1",
		"path: forbidden": "path=" + url.QueryEscape("path:/etc/passwd"),
		"own transcript":  "turns=1",
		"no session":      "path=" + url.QueryEscape("profile:dev/coder"),
		"traversal":       "path=" + url.QueryEscape("profile:demo/../dev/coder"),
		"number expected": "path=" + url.QueryEscape("profile:demo/dev/coder") + "&context=x",
		"repeated key":    "path=a&path=b",
	}
	for name, query := range cases {
		if status, body := get(query); status != 400 {
			t.Errorf("%s: %d %v", name, status, body)
		}
	}
	if status, body := get("path=" + url.QueryEscape("profile:demo/dev/coder")); status != 200 || !strings.Contains(fmt.Sprint(body["output"]), "forty-two") {
		t.Fatalf("a valid read: %d %v", status, body)
	}
	if status, _ := server.call(t, "GET", remoteapi.Prefix+"/transcript?path=x", "", nil); status != 401 {
		t.Fatalf("no token: %d", status)
	}
}

var _ = remote.Prefix

func TestRemoteTranscriptOutageAndRedirectGiveCleanErrors(t *testing.T) {
	server, _ := e2eServer(t)
	writeRemoteTranscript(t, server, "dev/coder", remoteTranscriptJSONL)
	readers := startRemoteReaders(t, server, true, "peer")
	server.gate.cut.Store(true)
	out := readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:demo/dev/coder"})
	if !strings.Contains(out, "remote_unreachable") || strings.Contains(out, e2eToken) || strings.Contains(out, "127.0.0.1") || strings.Contains(out, "/api/remote") {
		t.Fatalf("a cut connection:\n%s", out)
	}
	server.gate.cut.Store(false)
	if out = readers.run(t, readers.transcript, map[string]any{"path": "remote:peer/profile:demo/dev/coder"}); !strings.Contains(out, "forty-two") {
		t.Fatalf("the next call works again:\n%s", out)
	}
}
