package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jad/backend-go/internal/imagestore"
	"jad/backend-go/internal/responses"
	"jad/backend-go/internal/transcript"
)

// generatedRounds: round 1 carries a provider-generated image (as a bare base64 string and
// as a data URL) next to a client tool call, so round 2 re-sends the history.
func generatedRounds(bare string) [][]responses.Event {
	generated := `{"type":"openrouter:image_generation","id":"st_1","status":"completed","result":"` + bare + `","imageUrl":"data:image/png;base64,` + bare + `","prompt":"a gradient"}`
	call := `{"type":"function_call","call_id":"call-shot","name":"shot","arguments":"{}"}`
	return [][]responses.Event{
		{
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + generated + `}`)},
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":1,"item":` + call + `}`)},
			{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"r1","status":"completed","output":[` + generated + `,` + call + `]}}`)},
		},
		{
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`)},
			{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"r2","status":"completed","output":[]}}`)},
		},
	}
}

func runGeneratedTurn(t *testing.T, images *imagestore.Store) (streamer *multiRoundStreamer, bare, transcriptText string, events []string) {
	t.Helper()
	bare = strings.TrimPrefix(bigPNGURL(t), "data:image/png;base64,")
	streamer = &multiRoundStreamer{rounds: generatedRounds(bare)}
	dir := t.TempDir()
	journal, err := transcript.NewJournal(filepath.Join(dir, "messages.jsonl"), transcript.DurabilityProcess)
	if err != nil {
		t.Fatal(err)
	}
	store, err := transcript.NewTurnStore(journal, filepath.Join(dir, "staging"), transcript.TurnStoreOptions{MaxItemBytes: 4 << 20, ResponseMetadata: transcript.ResponseMetadataFull})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRunner(store, streamer, ModelSettings{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), json.RawMessage(`"draw"`), RunOptions{
		TurnID: "run-generated", MaxToolRounds: 2,
		ToolCatalog: newImageCatalog(t, "mcp", "data:image/png;base64,AAAA"),
		ImageStore:  images,
		OnEvent: func(event responses.Event) error {
			events = append(events, string(event.Raw))
			return nil
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return streamer, bare, string(stored), events
}

func newGeneratedStore(t *testing.T) (*imagestore.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "images")
	return imagestore.New(imagestore.Options{Dir: dir, MinBytes: 1000, MaxFileBytes: 4 << 20, MaxTotalBytes: 16 << 20}), dir
}

func TestRunnerStoresAGeneratedImageAndSendsItBackInFull(t *testing.T) {
	images, dir := newGeneratedStore(t)
	streamer, bare, stored, events := runGeneratedTurn(t, images)

	if strings.Contains(stored, bare[:200]) {
		t.Fatalf("messages.jsonl still holds the generated image (%d bytes)", len(stored))
	}
	if strings.Count(stored, imagestore.RawRefPrefix) == 0 || strings.Count(stored, imagestore.RefPrefix) == 0 {
		t.Fatalf("no references in the transcript: %.400s", stored)
	}
	for index, event := range events {
		if strings.Contains(event, bare[:200]) {
			t.Fatalf("event %d carries the base64 (%d bytes)", index, len(event))
		}
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("stored files = %d, want 1", len(files))
	}
	// Round 2 gets the history with the real bytes again, in both forms.
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(streamer.requests))
	}
	second := string(streamer.requests[1])
	if strings.Contains(second, "stored-image") {
		t.Fatalf("the provider received a reference: %.300s", second)
	}
	if strings.Count(second, bare) != 2 || !strings.Contains(second, `"imageUrl":"data:image/png;base64,`+bare) {
		t.Fatalf("the provider did not receive the full image twice (count %d)", strings.Count(second, bare))
	}
	// The item was stored once, with the file name in both references.
	name := files[0].Name()
	if strings.Count(stored, name) < 2 {
		t.Fatalf("file name %s not referenced twice", name)
	}
	if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || base64.StdEncoding.EncodeToString(data) != bare {
		t.Fatalf("stored file differs from the generated image: %v", err)
	}
}

func TestRunnerKeepsAGeneratedImageInTheTranscriptWhenTheStoreIsOff(t *testing.T) {
	streamer, bare, stored, events := runGeneratedTurn(t, nil)
	if strings.Count(stored, bare) < 2 {
		t.Fatalf("the transcript lost the image with the store off")
	}
	if !strings.Contains(strings.Join(events, ""), bare[:200]) {
		t.Fatal("the stream changed with the store off")
	}
	if strings.Contains(stored, "stored-image") || strings.Contains(string(streamer.requests[1]), "stored-image") {
		t.Fatal("a reference appeared with the store off")
	}
}

func TestRunnerSurvivesALostGeneratedImageFile(t *testing.T) {
	images, dir := newGeneratedStore(t)
	// Round 1 stores the image; the file is deleted before round 2 reads it back.
	bare := strings.TrimPrefix(bigPNGURL(t), "data:image/png;base64,")
	streamer := &multiRoundStreamer{rounds: generatedRounds(bare)}
	deleted := false
	wrapped := streamerFunc(func(ctx context.Context, request json.RawMessage, handle responses.EventHandler) error {
		err := streamer.Stream(ctx, request, handle)
		if len(streamer.requests) == 1 {
			// Round 1 is committed (the image is in the store); lose the file before
			// round 2 builds its request.
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if os.Remove(filepath.Join(dir, entry.Name())) == nil {
					deleted = true
				}
			}
		}
		return err
	})
	runDir := t.TempDir()
	journal, _ := transcript.NewJournal(filepath.Join(runDir, "messages.jsonl"), transcript.DurabilityProcess)
	store, _ := transcript.NewTurnStore(journal, filepath.Join(runDir, "staging"), transcript.TurnStoreOptions{MaxItemBytes: 4 << 20, ResponseMetadata: transcript.ResponseMetadataFull})
	runner, _ := NewRunner(store, wrapped, ModelSettings{Model: "gpt-test"})
	if _, err := runner.Run(context.Background(), json.RawMessage(`"draw"`), RunOptions{TurnID: "run-lost", MaxToolRounds: 2, ToolCatalog: newImageCatalog(t, "mcp", "data:image/png;base64,AAAA"), ImageStore: images}); err != nil {
		t.Fatalf("a lost image file failed the run: %v", err)
	}
	if !deleted || len(streamer.requests) != 2 || !json.Valid(streamer.requests[1]) {
		t.Fatalf("deleted=%v requests=%d", deleted, len(streamer.requests))
	}
	second := string(streamer.requests[1])
	if strings.Contains(second, "stored-image") {
		t.Fatal("a reference was sent to the provider")
	}
	if !strings.Contains(second, `"result":"`+onePixelBase64) || !strings.Contains(second, `"imageUrl":"data:image/png;base64,`+onePixelBase64) {
		t.Fatalf("a lost image was not replaced by the one-pixel image in both forms: %.400s", second)
	}
}

const onePixelBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

type streamerFunc func(context.Context, json.RawMessage, responses.EventHandler) error

func (f streamerFunc) Stream(ctx context.Context, request json.RawMessage, handle responses.EventHandler) error {
	return f(ctx, request, handle)
}
