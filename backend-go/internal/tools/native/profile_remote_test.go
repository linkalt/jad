package native

import (
	"context"
	"strings"
	"testing"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// A remote compact or reset is refused with no directory at all: if it reached a request, it would
// fail on the nil directory instead, so the refusal shows that nothing was sent.
func TestProfileRemoteCompactAndResetAreRefusedBeforeAnyRequest(t *testing.T) {
	settings := sessionfs.NativeProfileSettings{MaxEntries: 5, ScanMaxEntries: 5, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true, AllowRemote: true, Remotes: []string{"peer"}}
	actions := &fakeProfileActions{}
	tool := &profileTool{settings: settings, actions: actions, remotes: remoteSettings{names: []string{"peer"}}}
	for _, action := range []string{"compact", "reset"} {
		out := tool.executeRemote(context.Background(), action, "remote:peer/profile:demo/dev/coder", tools.ExecutionContext{SessionKey: "demo\x00dev/reviewer"})
		if !strings.Contains(out, "profile_remote_denied") {
			t.Fatalf("%s on a remote: %s", action, out)
		}
	}
	if len(actions.calls) != 0 {
		t.Fatalf("a local action ran for a remote path: %v", actions.calls)
	}
}

// Info of a remote profile is one request to the peer, and the answer carries no path.
func TestProfileRemoteInfoReachesThePeerAndShowsNoPath(t *testing.T) {
	p := startPeer(t, peerGrant)
	hub := remote.NewHub(func(name string) string {
		if name == "JAD_REMOTE_PEER" {
			return remoteSecret
		}
		return ""
	})
	t.Cleanup(hub.Close)
	settings := sessionfs.NativeProfileSettings{MaxEntries: 5, ScanMaxEntries: 5, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true, AllowRemote: true, Remotes: []string{"peer"}}
	tool, err := NewProfileToolWithRemotes(settings, TranscriptFiles{}, nil, nil, []string{"peer"}, hub.Directory(p.dir))
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), map[string]any{"action": "info", "path": "remote:peer/profile:main/review/reviewer"}, tools.ExecutionContext{CallID: "call"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "profile review/reviewer | context tokens: 42") || strings.Contains(out, "messages.jsonl") || strings.Contains(out, "| file ") {
		t.Fatalf("remote info:\n%s", out)
	}
	if len(p.backend.infos) != 1 || p.backend.infos[0].Session != "main" || p.backend.infos[0].Profile != "review/reviewer" {
		t.Fatalf("peer requests: %#v", p.backend.infos)
	}
}

// Remote info is refused when allow_remote is off, before any request.
func TestProfileRemoteInfoNeedsAllowRemote(t *testing.T) {
	settings := sessionfs.NativeProfileSettings{MaxEntries: 5, ScanMaxEntries: 5, AllowOtherSessions: true, AllowOtherProfiles: true, AllowChildren: true}
	tool := &profileTool{settings: settings, remotes: remoteSettings{names: []string{"peer"}}}
	out := tool.executeRemote(context.Background(), "info", "remote:peer/profile:demo/dev/coder", tools.ExecutionContext{})
	if !strings.Contains(out, "remote_disabled") {
		t.Fatalf("remote info without allow_remote: %s", out)
	}
}
