package httpapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"jad/backend-go/internal/webui"
)

// TestToolResultImagesAreKeptUnderTheCallIDTheMessageShows: the live image route is asked
// for the call id of the message part. A result without a call_id gets one made from its
// position in the message, and its images must be kept under that same id (they were kept
// under the id for position 0, so the browser's request found nothing).
func TestToolResultImagesAreKeptUnderTheCallIDTheMessageShows(t *testing.T) {
	registry := newRunRegistry(time.Minute, 1<<30, 0)
	run, err := registry.begin("key", "turn")
	if err != nil {
		t.Fatal(err)
	}
	var published []webui.Message
	emitter := newMessageStreamEmitter(0, func(message webui.Message) error { published = append(published, message); return nil })
	streaming := webui.NewStreamingRun("turn", "now")
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 24)...)
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)

	publishToolResult(run, streaming, emitter, json.RawMessage(`{"type":"function_call_output","call_id":"first","output":"text"}`), nil, nil)
	publishToolResult(run, streaming, emitter, json.RawMessage(`{"type":"function_call_output","output":[{"type":"input_image","image_url":"`+url+`"},{"type":"input_image","image_url":"`+url+`"}]}`), nil, nil)

	if len(published) != 2 {
		t.Fatalf("published %d messages, want 2", len(published))
	}
	parts := published[1].Parts
	part := parts[len(parts)-1]
	if part.Type != "tool_result" || part.InlineImages != 2 {
		t.Fatalf("last part = %+v", part)
	}
	for index := 0; index < part.InlineImages; index++ {
		image, ok := run.liveImage(part.CallID, index)
		if !ok || !bytes.Equal(image.Data, png) || image.ContentType != "image/png" {
			t.Fatalf("image %d of %q: %v, %d bytes", index, part.CallID, ok, len(image.Data))
		}
	}
}
