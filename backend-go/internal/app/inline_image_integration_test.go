package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

type inlineImageTarget struct {
	messageID string
	callID    string
	count     int
	cursor    *int64
}

func inlineImageSnapshot(t *testing.T, f subagentFixture) inlineImageTarget {
	t.Helper()
	status, _, body := getWithToken(t, f.apiURL+"/api/sessions/local:demo", integrationAPIToken)
	if status != http.StatusOK {
		t.Fatalf("snapshot status %d: %s", status, body)
	}
	var snapshot struct {
		Messages []struct {
			ID     string `json:"id"`
			Cursor *int64 `json:"cursor"`
			Parts  []struct {
				Type         string `json:"type"`
				CallID       string `json:"callId"`
				Output       string `json:"output"`
				InlineImages int    `json:"inlineImages"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, message := range snapshot.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.InlineImages > 0 {
				if strings.Contains(part.Output, "base64") {
					t.Fatalf("the image bytes reached the message: %.80s", part.Output)
				}
				return inlineImageTarget{messageID: message.ID, callID: part.CallID, count: part.InlineImages, cursor: message.Cursor}
			}
		}
	}
	t.Fatalf("no tool result with inlineImages in the snapshot: %s", body)
	return inlineImageTarget{}
}

func (target inlineImageTarget) path(index string) string {
	path := "/api/sessions/local:demo/messages/" + url.PathEscape(target.messageID) + "/images/" + url.PathEscape(target.callID) + "/" + index
	if target.cursor != nil {
		path += "?cursor=" + strconv.FormatInt(*target.cursor, 10)
	}
	return path
}

func TestInlineImageOfAStoreLessSessionIsServedThroughTheMessageRoute(t *testing.T) {
	f := storeFixture(t, nil, nil, false)
	f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	target := inlineImageSnapshot(t, f)
	if target.count != 1 || target.cursor == nil {
		t.Fatalf("target = %+v, want one image and a cursor", target)
	}

	status, header, body := getWithToken(t, f.apiURL+target.path("0"), integrationAPIToken)
	if status != http.StatusOK || header.Get("Content-Type") != "image/png" || !bytes.Equal(body, gradientPNG(t)) {
		t.Fatalf("status %d type %q, %d bytes (want the %d byte fixture png)", status, header.Get("Content-Type"), len(body), len(gradientPNG(t)))
	}
	if header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Content-Security-Policy") != "sandbox" || !strings.Contains(header.Get("Cache-Control"), "no-store") {
		t.Fatalf("headers = %v", header)
	}
}

func TestMessageImageRouteRefusesWhatIsNotThere(t *testing.T) {
	f := storeFixture(t, nil, nil, false)
	f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	f.run(t)
	target := inlineImageSnapshot(t, f)

	wrongCall := target
	wrongCall.callID = "no-such-call"
	wrongMessage := target
	wrongMessage.messageID = "no-such-message"
	noCursor := target
	noCursor.cursor = nil
	wrongCursor := target
	moved := *target.cursor + 1
	wrongCursor.cursor = &moved

	tests := []struct {
		name string
		path string
		want int
	}{
		{"index past the last image", target.path("1"), http.StatusNotFound},
		{"unknown call id", wrongCall.path("0"), http.StatusNotFound},
		{"unknown message", wrongMessage.path("0"), http.StatusNotFound},
		{"no cursor searches only the history block", noCursor.path("0"), http.StatusNotFound},
		{"cursor that starts no turn", wrongCursor.path("0"), http.StatusNotFound},
		{"index is not a number", target.path("x"), http.StatusBadRequest},
		{"index has a sign", target.path("+0"), http.StatusBadRequest},
		{"index is negative", target.path("-1"), http.StatusBadRequest},
	}
	for _, test := range tests {
		if status, _, body := getWithToken(t, f.apiURL+test.path, integrationAPIToken); status != test.want {
			t.Errorf("%s: status %d, want %d (%s)", test.name, status, test.want, body)
		}
	}
	if status, _, _ := getWithToken(t, f.apiURL+target.path("0"), ""); status != http.StatusUnauthorized {
		t.Errorf("without a token: status %d, want 401", status)
	}
}

func TestStoredImageStillShowsAsAStoredNameNotAnInlineOne(t *testing.T) {
	f := storeFixture(t, map[string]any{"dir": "images", "min_bytes": 100}, nil, false)
	f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	f.run(t)
	status, _, body := getWithToken(t, f.apiURL+"/api/sessions/local:demo", integrationAPIToken)
	if status != http.StatusOK || !strings.Contains(string(body), `"images":["`) || strings.Contains(string(body), "inlineImages") {
		t.Fatalf("status %d, want stored names and no inlineImages: %s", status, body)
	}
}

func TestLoadingTheWholeMessageKeepsTheCursorThatTheInlineImagesNeed(t *testing.T) {
	f := storeFixture(t, nil, nil, false)
	f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	f.run(t)
	target := inlineImageSnapshot(t, f)

	status, _, body := getWithToken(t, f.apiURL+"/api/sessions/local:demo/messages/"+url.PathEscape(target.messageID)+"?cursor="+strconv.FormatInt(*target.cursor, 10), integrationAPIToken)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var reply struct {
		Message struct {
			Cursor *int64 `json:"cursor"`
			Parts  []struct {
				Type         string `json:"type"`
				Output       string `json:"output"`
				InlineImages int    `json:"inlineImages"`
			} `json:"parts"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Message.Cursor == nil || *reply.Message.Cursor != *target.cursor {
		t.Fatalf("cursor = %v, want %d: the browser would lose the images after Load full", reply.Message.Cursor, *target.cursor)
	}
	for _, part := range reply.Message.Parts {
		if part.Type == "tool_result" && strings.Contains(part.Output, "base64") {
			t.Fatalf("the image bytes reached the tool result: %.120s", part.Output)
		}
	}
}
