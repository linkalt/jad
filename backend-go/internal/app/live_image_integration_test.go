package app

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

const liveImageRoute = "/api/sessions/local:demo/run/images/"

func TestLiveRunImageIsServedWithoutATranscriptRecord(t *testing.T) {
	for name, setting := range map[string]any{"image store off": nil, "image store on": map[string]any{"dir": "images", "min_bytes": 100}} {
		f := storeFixture(t, setting, nil, false)
		f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
		if body := f.run(t); !strings.Contains(body, "event: completed") {
			t.Fatalf("%s: run did not complete:\n%s", name, body)
		}
		status, header, body := getWithToken(t, f.apiURL+liveImageRoute+"call-parent-first/0", integrationAPIToken)
		if status != http.StatusOK || header.Get("Content-Type") != "image/png" || !bytes.Equal(body, gradientPNG(t)) {
			t.Errorf("%s: status %d type %q, %d bytes (want the %d byte fixture png)", name, status, header.Get("Content-Type"), len(body), len(gradientPNG(t)))
		}
		if header.Get("X-Content-Type-Options") != "nosniff" || header.Get("Content-Security-Policy") != "sandbox" || !strings.Contains(header.Get("Cache-Control"), "no-store") {
			t.Errorf("%s: headers = %v", name, header)
		}
	}
}

func TestLiveRunImageRouteRefusesWhatIsNotThere(t *testing.T) {
	f := storeFixture(t, nil, nil, false)
	f.provider.parentFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"x"}`}
	f.run(t)
	tests := []struct {
		name string
		path string
		want int
	}{
		{"index past the last image", "call-parent-first/1", http.StatusNotFound},
		{"unknown call id", "no-such-call/0", http.StatusNotFound},
		{"index is not a number", "call-parent-first/x", http.StatusBadRequest},
		{"index has a sign", "call-parent-first/+0", http.StatusBadRequest},
		{"index is negative", "call-parent-first/-1", http.StatusBadRequest},
	}
	for _, test := range tests {
		if status, _, body := getWithToken(t, f.apiURL+liveImageRoute+test.path, integrationAPIToken); status != test.want {
			t.Errorf("%s: status %d, want %d (%s)", test.name, status, test.want, body)
		}
	}
	if status, _, _ := getWithToken(t, f.apiURL+liveImageRoute+"call-parent-first/0", ""); status != http.StatusUnauthorized {
		t.Errorf("without a token: status %d, want 401", status)
	}
}

func TestLiveRunImageRouteHasNothingForASessionWithoutARun(t *testing.T) {
	f := storeFixture(t, nil, nil, false)
	if status, _, body := getWithToken(t, f.apiURL+liveImageRoute+"call-parent-first/0", integrationAPIToken); status != http.StatusNotFound {
		t.Errorf("status %d, want 404 (%s)", status, body)
	}
}
