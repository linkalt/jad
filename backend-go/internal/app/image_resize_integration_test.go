package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// gradientPNG is a 400x400 png: bigger than the 100 pixel limit the tests set.
func gradientPNG(t *testing.T) []byte {
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
	return buffer.Bytes()
}

// imageSeenBy returns the width of the image in the function_call_output of the model's
// last request, and the data URL itself.
func imageSeenBy(t *testing.T, f subagentFixture, model string) (int, string) {
	t.Helper()
	requests := f.provider.forModel(model)
	if len(requests) < 2 {
		t.Fatalf("%s got %d requests, want at least 2", model, len(requests))
	}
	for _, raw := range requests[len(requests)-1].Input {
		var item struct {
			Type   string `json:"type"`
			Output []struct {
				Type     string `json:"type"`
				ImageURL string `json:"image_url"`
			} `json:"output"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type != "function_call_output" {
			continue
		}
		for _, part := range item.Output {
			if part.Type != "input_image" {
				continue
			}
			_, data, _ := strings.Cut(part.ImageURL, ",")
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatal(err)
			}
			config, _, err := image.DecodeConfig(bytes.NewReader(decoded))
			if err != nil {
				t.Fatal(err)
			}
			return config.Width, part.ImageURL
		}
	}
	t.Fatalf("%s: no image in its last request", model)
	return 0, ""
}

func imageFixture(t *testing.T, imageResize any, childUsesMCP bool) subagentFixture {
	t.Helper()
	agent := map[string]any{"model": "child.json"}
	if childUsesMCP {
		agent["tools"] = []string{"mcp:fixture:lookup"}
	}
	if imageResize != nil {
		agent["__session"] = map[string]any{"image_resize": imageResize}
	}
	f := buildSubagentFixtureWithMCP(t, agent, false, nil, &mcpFixture{calls: &atomic.Int32{}, image: gradientPNG(t)})
	return f
}

func TestParentRunSendsAResizedMCPImageAndKeepsTheOriginalInTheTranscript(t *testing.T) {
	f := imageFixture(t, map[string]any{"enabled": true, "max_edge_px": 100}, false)
	f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	width, _ := imageSeenBy(t, f, "parent-model")
	if width != 100 {
		t.Fatalf("the provider got an image %d pixels wide, want 100", width)
	}
	stored, err := os.ReadFile(filepath.Join(f.sessionDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	original := base64.StdEncoding.EncodeToString(gradientPNG(t))
	if !strings.Contains(string(stored), original) {
		t.Fatalf("messages.jsonl (%d bytes) does not hold the original 400 pixel image", len(stored))
	}
}

func TestParentRunSendsTheOriginalImageWhenImageResizeIsOffOrAbsent(t *testing.T) {
	for name, setting := range map[string]any{
		"absent":    nil,
		"disabled":  map[string]any{"enabled": false, "max_edge_px": 100},
		"no source": map[string]any{"enabled": true, "max_edge_px": 100, "applies_to": []string{}},
		"user only": map[string]any{"enabled": true, "max_edge_px": 100, "applies_to": []string{"user"}},
	} {
		f := imageFixture(t, setting, false)
		f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
		f.run(t)
		if width, _ := imageSeenBy(t, f, "parent-model"); width != 400 {
			t.Errorf("%s: the provider got a %d pixel image, want the original 400", name, width)
		}
	}
}

func TestSubagentSendsAResizedMCPImageToo(t *testing.T) {
	f := imageFixture(t, map[string]any{"enabled": true, "max_edge_px": 100}, true)
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if width, _ := imageSeenBy(t, f, "child-model"); width != 100 {
		t.Fatalf("the child's provider got an image %d pixels wide, want 100", width)
	}
}

func TestSubagentSendsTheOriginalImageWhenImageResizeIsOff(t *testing.T) {
	f := imageFixture(t, nil, true)
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	f.run(t)
	if width, _ := imageSeenBy(t, f, "child-model"); width != 400 {
		t.Fatalf("the child's provider got a %d pixel image, want the original 400", width)
	}
}
