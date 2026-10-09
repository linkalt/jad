package app

import (
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func storeFixture(t *testing.T, imageStore, imageResize any, childUsesMCP bool) subagentFixture {
	t.Helper()
	agent := map[string]any{"model": "child.json"}
	if childUsesMCP {
		agent["tools"] = []string{"mcp:fixture:lookup"}
	}
	session := map[string]any{}
	if imageStore != nil {
		session["image_store"] = imageStore
	}
	if imageResize != nil {
		session["image_resize"] = imageResize
	}
	agent["__session"] = session
	return buildSubagentFixtureWithMCP(t, agent, false, nil, &mcpFixture{calls: &atomic.Int32{}, image: gradientPNG(t)})
}

func getWithToken(t *testing.T, url string, token string) (int, http.Header, []byte) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, response.Header, body
}

func TestImageStoreKeepsTheTranscriptSmallAndTheProviderSeesTheImage(t *testing.T) {
	f := storeFixture(t, map[string]any{"dir": "images", "min_bytes": 100}, nil, false)
	f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	original := gradientPNG(t)
	transcript, err := os.ReadFile(filepath.Join(f.sessionDir, "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake provider echoes what it saw in its reply, so only the tool output line counts.
	var outputLine string
	for _, line := range strings.Split(string(transcript), "\n") {
		if strings.Contains(line, `"type":"function_call_output"`) {
			outputLine = line
		}
	}
	if outputLine == "" || strings.Contains(outputLine, "base64") || !strings.Contains(outputLine, "stored-image://") || len(outputLine) > 600 {
		t.Fatalf("the tool output in messages.jsonl must hold a reference and no base64 (%d bytes): %.700s", len(outputLine), outputLine)
	}
	// The model still gets the whole image, byte for byte.
	width, url := imageSeenBy(t, f, "parent-model")
	if width != 400 || !strings.HasSuffix(url, base64.StdEncoding.EncodeToString(original)) {
		t.Fatalf("the provider got width %d and a different image", width)
	}
	entries, err := os.ReadDir(filepath.Join(f.sessionDir, "images"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("image directory: %v, %d entries", err, len(entries))
	}
	// The file is served by name, behind the token.
	route := f.apiURL + "/api/sessions/local:demo/images/" + entries[0].Name()
	if status, _, _ := getWithToken(t, route, ""); status != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", status)
	}
	status, header, body := getWithToken(t, route, integrationAPIToken)
	if status != http.StatusOK || header.Get("Content-Type") != "image/png" || header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Content-Security-Policy") != "sandbox" || string(body) != string(original) {
		t.Fatalf("status %d type %q nosniff %q, %d bytes", status, header.Get("Content-Type"), header.Get("X-Content-Type-Options"), len(body))
	}
	for _, name := range []string{"..%2Fsetting.json", "nothing.png", strings.Repeat("0", 64) + ".png", strings.Repeat("0", 64) + ".svg"} {
		if status, _, _ := getWithToken(t, f.apiURL+"/api/sessions/local:demo/images/"+name, integrationAPIToken); status != http.StatusBadRequest && status != http.StatusNotFound {
			t.Errorf("%s: status %d, want 400 or 404", name, status)
		}
	}
}

func TestImageStoreAndImageResizeTogetherSendTheResizedImage(t *testing.T) {
	f := storeFixture(t, map[string]any{"dir": "images", "min_bytes": 100}, map[string]any{"enabled": true, "max_edge_px": 100}, false)
	f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	f.run(t)
	if width, _ := imageSeenBy(t, f, "parent-model"); width != 100 {
		t.Fatalf("the provider got a %d pixel image, want the stored image resized to 100", width)
	}
}

func TestImageStoreOffLeavesTheTranscriptAsBefore(t *testing.T) {
	for name, setting := range map[string]any{"absent": nil, "below min_bytes": map[string]any{"dir": "images", "min_bytes": 10 << 20}} {
		f := storeFixture(t, setting, nil, false)
		f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
		f.run(t)
		transcript, _ := os.ReadFile(filepath.Join(f.sessionDir, "messages.jsonl"))
		if !strings.Contains(string(transcript), base64.StdEncoding.EncodeToString(gradientPNG(t))) || strings.Contains(string(transcript), "stored-image://") {
			t.Errorf("%s: the image must stay inline in messages.jsonl", name)
		}
		if status, _, _ := getWithToken(t, f.apiURL+"/api/sessions/local:demo/images/"+strings.Repeat("a", 64)+".png", integrationAPIToken); status != http.StatusNotFound {
			t.Errorf("%s: route status %d, want 404", name, status)
		}
	}
}

func TestSubagentStoresItsMCPImageToo(t *testing.T) {
	f := storeFixture(t, map[string]any{"dir": "images", "min_bytes": 100}, nil, true)
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if width, _ := imageSeenBy(t, f, "child-model"); width != 400 {
		t.Fatalf("the child's provider got a %d pixel image, want the original 400", width)
	}
	entries, _ := os.ReadDir(filepath.Join(f.sessionDir, "images"))
	if len(entries) != 1 {
		t.Fatalf("%d stored images, want 1 (the child's)", len(entries))
	}
}
