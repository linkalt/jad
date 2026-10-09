package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/agent-harness/backend-go/internal/imageresize"
	"example.com/agent-harness/backend-go/internal/responses"
	"example.com/agent-harness/backend-go/internal/tools"
	"example.com/agent-harness/backend-go/internal/transcript"
)

// bigPNGURL is a 400x400 gradient: large enough to be resized to 100 pixels.
func bigPNGURL(t *testing.T) string {
	t.Helper()
	picture := image.NewNRGBA(image.Rect(0, 0, 400, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 400; x++ {
			picture.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buffer.Bytes())
}

func newImageCatalog(t *testing.T, source, imageURL string) *tools.Catalog {
	t.Helper()
	schema := json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	validator, err := tools.CompileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal([]map[string]any{{"type": "input_image", "image_url": imageURL, "detail": "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{{
		Definition: tools.Definition{ID: source + ":shot", Source: source, Name: "shot", Description: "Take a screenshot.", InputSchema: schema},
		Validator:  validator,
		ExecuteOutput: func(context.Context, map[string]any, tools.ExecutionContext) (json.RawMessage, error) {
			return output, nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func imageRounds() [][]responses.Event {
	call := `{"type":"function_call","call_id":"call-shot","name":"shot","arguments":"{}"}`
	return [][]responses.Event{
		{
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":` + call + `}`)},
			{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"r1","status":"completed","output":[]}}`)},
		},
		{
			{Type: "response.output_item.done", Raw: json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"seen"}]}}`)},
			{Type: "response.completed", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"r2","status":"completed","output":[]}}`)},
		},
	}
}

// secondRoundImage returns the image URL the provider received in round 2.
func secondRoundImage(t *testing.T, streamer *multiRoundStreamer) string {
	t.Helper()
	if len(streamer.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(streamer.requests))
	}
	var request struct {
		Input []struct {
			Type   string `json:"type"`
			Output []struct {
				Type     string `json:"type"`
				ImageURL string `json:"image_url"`
			} `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(streamer.requests[1], &request); err != nil {
		t.Fatal(err)
	}
	for _, item := range request.Input {
		if item.Type == "function_call_output" && len(item.Output) == 1 {
			return item.Output[0].ImageURL
		}
	}
	t.Fatalf("no image tool output in round 2: %s", streamer.requests[1])
	return ""
}

func runImageTurn(t *testing.T, source string, resizer *imageresize.Resizer) (*multiRoundStreamer, string, string) {
	t.Helper()
	original := bigPNGURL(t)
	streamer := &multiRoundStreamer{rounds: imageRounds()}
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
	if _, err := runner.Run(context.Background(), json.RawMessage(`"take a screenshot"`), RunOptions{
		TurnID: "run-image", ToolCatalog: newImageCatalog(t, source, original), MaxToolRounds: 2, ImageResizer: resizer,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return streamer, original, string(stored)
}

func resizerFor(appliesTo ...string) *imageresize.Resizer {
	return imageresize.New(imageresize.Options{MaxEdgePx: 100, MaxBytes: 1 << 20, MaxPixels: 4_000_000, JPEGQuality: 85, CacheEntries: 4}, appliesTo)
}

func TestRunnerSendsAResizedImageButStoresTheOriginal(t *testing.T) {
	streamer, original, stored := runImageTurn(t, "mcp", resizerFor("mcp"))
	sent := secondRoundImage(t, streamer)
	if sent == original || len(sent) >= len(original) {
		t.Fatalf("the provider got %d bytes of image, the original is %d: it was not resized", len(sent), len(original))
	}
	_, data, ok := strings.Cut(sent, ",")
	if !ok {
		t.Fatalf("not a data URL: %.40s", sent)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	if config, _, err := image.DecodeConfig(bytes.NewReader(raw)); err != nil || config.Width != 100 || config.Height != 100 {
		t.Fatalf("sent image = %+v (%v), want 100x100", config, err)
	}
	if !strings.Contains(stored, strings.TrimPrefix(original, "data:image/png;base64,")) {
		t.Fatalf("messages.jsonl does not hold the original image (stored %d bytes)", len(stored))
	}
	if strings.Contains(stored, data[:200]) && sent != original {
		t.Fatalf("messages.jsonl holds the resized image; the transcript must keep the original")
	}
}

func TestRunnerLeavesTheRequestAloneWhenTheSourceIsNotListed(t *testing.T) {
	for name, resizer := range map[string]*imageresize.Resizer{
		"nil resizer":        nil,
		"user only":          resizerFor("user"),
		"native only vs mcp": resizerFor("native"),
	} {
		streamer, original, _ := runImageTurn(t, "mcp", resizer)
		if sent := secondRoundImage(t, streamer); sent != original {
			t.Errorf("%s: the image was changed (%d bytes, original %d)", name, len(sent), len(original))
		}
	}
}

func TestRunnerResizesANativeToolImageOnlyWhenNativeIsListed(t *testing.T) {
	streamer, original, _ := runImageTurn(t, "native", resizerFor("mcp"))
	if sent := secondRoundImage(t, streamer); sent != original {
		t.Fatalf("a native tool image was resized although only mcp is listed")
	}
	streamer, original, _ = runImageTurn(t, "native", resizerFor("native"))
	if sent := secondRoundImage(t, streamer); sent == original {
		t.Fatalf("a native tool image was not resized although native is listed")
	}
}
