package native

import (
	"strings"
	"testing"
)

// The profile-listing paths of transcripts answer with a one-line redirect to profile: no
// listing, no paths. Its run list still lists.
func TestTranscriptsProfileListingRedirectsToProfile(t *testing.T) {
	f := newListFixture(t)
	for _, arguments := range []string{`{}`, `{"path":"session:demo"}`} {
		out := f.call(t, nil, arguments)
		if !strings.Contains(out, "profiles_moved") || !strings.Contains(out, "profile tool") {
			t.Errorf("%s did not redirect: %s", arguments, out)
		}
		if strings.Contains(out, "dev/coder") || strings.Contains(out, "| path ") {
			t.Errorf("%s still lists profiles: %s", arguments, out)
		}
	}
}
