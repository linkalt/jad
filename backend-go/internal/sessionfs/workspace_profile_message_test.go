package sessionfs

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// profileMessageWorkspaces lists a and b, a's dev/coder using the toolset coding/talk with the
// given profile_message section.
func profileMessageWorkspaces(t *testing.T, section string, listA bool) (wsFixture, Config) {
	t.Helper()
	f, _ := twoMessageWorkspaces(t, listA, true, `{}`, `{}`)
	wsPut(t, filepath.Join(f.global, "tools", "coding", "talk.json"), `{"native":{"profile_message":`+section+`},"mcp":{}}`)
	a := filepath.Join(f.base, "a")
	wsPut(t, filepath.Join(a, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default"],"model_default":"openai/default","toolset":"coding/talk"}`)
	return f, wsMustLoad(t, f, a)
}

// A session of the workspace layout hands its profile_message the caller it writes addresses as
// and the other workspaces it may name; an unlisted workspace reaches none.
func TestWorkspaceSessionKnowsTheCallerOfItsProfileMessage(t *testing.T) {
	_, config := profileMessageWorkspaces(t, `{}`, true)
	session, err := wsSession(t, config, "a:s1", "")
	if err != nil {
		t.Fatal(err)
	}
	settings := session.NativeProfileMessage
	if settings == nil || settings.Caller != (MessageCaller{Workspace: "a", Session: "s1", Profile: "dev/coder"}) {
		t.Fatalf("settings = %+v", settings)
	}
	if !slices.Equal(settings.Workspaces, []string{"b"}) {
		t.Fatalf("workspaces = %q", settings.Workspaces)
	}
	// Not listed: it is not reachable by others and reaches no other workspace.
	_, config = profileMessageWorkspaces(t, `{}`, false)
	session, err = wsSession(t, config, "a:s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if session.NativeProfileMessage.Caller.Workspace != "a" || len(session.NativeProfileMessage.Workspaces) != 0 {
		t.Fatalf("unlisted caller: %+v", session.NativeProfileMessage)
	}
}

// sessions is a v1 key: in the workspace layout it is refused by name, with what to do, instead
// of being read as something else or ignored.
func TestWorkspaceLayoutRefusesTheSessionsKeyOfProfileMessage(t *testing.T) {
	_, config := profileMessageWorkspaces(t, `{"sessions":["*"]}`, true)
	_, err := wsSession(t, config, "a:s1", "")
	if err == nil || !strings.Contains(err.Error(), "profile_message.sessions was removed with workspace layout v1") || !strings.Contains(err.Error(), "delete the key") {
		t.Fatalf("err = %v", err)
	}
}

// The sessions key of the retired v1 layout is an error that says what replaces it.
func TestTheV1SessionsKeyOfProfileMessageIsRemoved(t *testing.T) {
	_, err := normalizeNativeProfileMessageSettings(decideSystemoneSettingsFor(`{"profile_message":{"sessions":["a-*"]}}`), "setting.json", MessageCaller{Workspace: "a"})
	if err == nil || !strings.Contains(err.Error(), "sessions was removed") || !strings.Contains(err.Error(), "use deny") {
		t.Fatalf("sessions: %v", err)
	}
}
