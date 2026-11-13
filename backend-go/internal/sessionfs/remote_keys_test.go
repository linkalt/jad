package sessionfs

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const keySecret = "KEYSECRET_0123456789abcdef0123456789abcdef"

// keysFixture is a workspace "proj" with bindings boxa and boxb in G/remotes and one key file for
// each (ka, kb), plus kc for boxa again. Its workspace.json, session s1 and profile dev/coder are
// given by the test.
type keysFixture struct {
	f wsFixture
	w string
}

func newKeysFixture(t *testing.T, workspaceJSON, sessionJSON, profileJSON string) keysFixture {
	t.Helper()
	f := newWSFixture(t)
	for _, name := range []string{"boxa", "boxb"} {
		wsPut(t, filepath.Join(f.global, "remotes", name+".json"), `{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj"}}`)
	}
	for key, binding := range map[string]string{"ka": "boxa", "kb": "boxb", "kc": "boxa"} {
		wsPut(t, filepath.Join(f.global, "auth", "keys", key+".json"), `{"kind":"remote_key","binding":"`+binding+`","token":"`+keySecret+`"}`)
	}
	w := f.workspace(t, "proj", workspaceJSON)
	if sessionJSON != "" {
		wsPut(t, filepath.Join(w, ".jad", "sessions", "s1.json"), sessionJSON)
	}
	if profileJSON != "" {
		wsPut(t, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), profileJSON)
	}
	return keysFixture{f: f, w: w}
}

func (k keysFixture) load(t *testing.T) (SessionConfig, error) {
	t.Helper()
	config, err := k.f.load(k.w)
	if err != nil {
		return SessionConfig{}, err
	}
	location, err := Resolve(config.Roots, "s1")
	if err != nil {
		t.Fatal(err)
	}
	return LoadLayoutSessionSettings(location, *location.Root.Layout, "dev/coder", "")
}

func (k keysFixture) names(t *testing.T) []string {
	t.Helper()
	config, err := k.load(t)
	if err != nil {
		t.Fatal(err)
	}
	if config.RemoteKeyFault != nil {
		t.Fatalf("fault: %v", config.RemoteKeyFault)
	}
	names := []string{}
	for binding, key := range config.RemoteKeys {
		if key.File != filepath.Join(k.f.global, "auth", "keys", key.Name+".json") {
			t.Errorf("the key file of %s is %s", key.Name, key.File)
		}
		names = append(names, binding+"="+key.Name)
	}
	slices.Sort(names)
	return names
}

const profileBase = `"models":["openai/default"],"model_default":"openai/default"`

// remote_keys: the nearest level that sets it decides (profile, session, workspace); absent
// inherits, [] means none, a list replaces the levels above.
func TestRemoteKeysNearestLevelWins(t *testing.T) {
	for name, c := range map[string]struct {
		workspace, session, profile string
		want                        []string
	}{
		"none anywhere":              {`{}`, ``, ``, []string{}},
		"workspace only":             {`{"remote_keys":["ka"]}`, ``, ``, []string{"boxa=ka"}},
		"workspace with two":         {`{"remote_keys":["ka","kb"]}`, ``, ``, []string{"boxa=ka", "boxb=kb"}},
		"session replaces":           {`{"remote_keys":["ka"]}`, `{"schema_version":1,"remote_keys":["kb"]}`, ``, []string{"boxb=kb"}},
		"empty session clears":       {`{"remote_keys":["ka"]}`, `{"schema_version":1,"remote_keys":[]}`, ``, []string{}},
		"profile replaces session":   {`{"remote_keys":["ka"]}`, `{"schema_version":1,"remote_keys":["kb"]}`, `{` + profileBase + `,"remote_keys":["kc"]}`, []string{"boxa=kc"}},
		"empty profile clears":       {`{"remote_keys":["ka","kb"]}`, ``, `{` + profileBase + `,"remote_keys":[]}`, []string{}},
		"profile alone":              {`{}`, ``, `{` + profileBase + `,"remote_keys":["kb"]}`, []string{"boxb=kb"}},
		"session inherits workspace": {`{"remote_keys":["kb"]}`, `{"schema_version":1}`, ``, []string{"boxb=kb"}},
	} {
		t.Run(name, func(t *testing.T) {
			k := newKeysFixture(t, c.workspace, c.session, c.profile)
			if got := k.names(t); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("keys = %v, want %v", got, c.want)
			}
		})
	}
}

// A broken key does not stop the session from loading and listing, but a run (WithModel) stops
// with the key's name, and the session has no keys at all while it is broken. Messages never
// repeat the secret or a path of the key folder's parent.
func TestBrokenRemoteKeysFailTheRunAndNameTheKey(t *testing.T) {
	for name, c := range map[string]struct {
		workspace, want string
		change          func(k keysFixture)
	}{
		"unknown key":       {`{"remote_keys":["nokey"]}`, `key nokey: the key file cannot be read`, nil},
		"binding missing":   {`{"remote_keys":["ka"]}`, `key ka is for binding boxa, which cannot be used`, func(k keysFixture) { _ = os.Remove(filepath.Join(k.f.global, "remotes", "boxa.json")) }},
		"two for a binding": {`{"remote_keys":["ka","kc"]}`, `keys ka and kc are both for binding boxa`, nil},
		"open key file": {`{"remote_keys":["ka"]}`, `key ka: the key file must not be readable by group or others`, func(k keysFixture) {
			_ = os.Chmod(filepath.Join(k.f.global, "auth", "keys", "ka.json"), 0o644)
		}},
		"binding with v1 keys": {`{"remote_keys":["ka"]}`, `remote.token_env was removed with workspace layout v1`, func(k keysFixture) {
			wsPut(t, filepath.Join(k.f.global, "remotes", "boxa.json"), `{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj","token_env":"JAD_REMOTE_X"}}`)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			k := newKeysFixture(t, c.workspace, "", "")
			if c.change != nil {
				c.change(k)
			}
			config, err := k.load(t)
			if err != nil {
				t.Fatalf("the session must still load: %v", err)
			}
			if config.RemoteKeyFault == nil || !strings.Contains(config.RemoteKeyFault.Error(), c.want) || len(config.RemoteKeys) != 0 {
				t.Fatalf("fault %v keys %v, want %q", config.RemoteKeyFault, config.RemoteKeys, c.want)
			}
			if _, err := config.WithModel(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("WithModel: %v", err)
			}
			if strings.Contains(config.RemoteKeyFault.Error(), keySecret) {
				t.Fatalf("the secret is in the message: %v", config.RemoteKeyFault)
			}
		})
	}
}

// Names and types are checked where the file is read: a bad workspace.json fails the start, a
// bad session or profile file fails the session that reads it.
func TestRemoteKeysNamesAreValidated(t *testing.T) {
	for name, c := range map[string]struct{ workspace, session, profile, want string }{
		"workspace path name": {`{"remote_keys":["../ka"]}`, ``, ``, "is not a key name"},
		"workspace duplicate": {`{"remote_keys":["ka","ka"]}`, ``, ``, `lists key "ka" more than once`},
		"workspace string":    {`{"remote_keys":"ka"}`, ``, ``, "must be an array of key names"},
		"workspace null":      {`{"remote_keys":null}`, ``, ``, "must be an array of key names"},
		"session bad name":    {`{}`, `{"schema_version":1,"remote_keys":["a b"]}`, ``, "is not a key name"},
		"profile bad name":    {`{}`, ``, `{` + profileBase + `,"remote_keys":["a/b"]}`, "is not a key name"},
	} {
		t.Run(name, func(t *testing.T) {
			k := newKeysFixture(t, c.workspace, c.session, c.profile)
			_, err := k.load(t)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
		})
	}
}

// A toolset never has remote_keys: it would let a toolset choose a credential.
func TestRemoteKeysAreRefusedInToolsets(t *testing.T) {
	k := newKeysFixture(t, `{}`, ``, `{`+profileBase+`,"toolset":"coding/x"}`)
	wsPut(t, filepath.Join(k.f.global, "tools", "coding", "x.json"), `{"native":{},"mcp":{},"remote_keys":["ka"]}`)
	if config, err := k.load(t); err == nil || !strings.Contains(err.Error(), "remote_keys") {
		t.Fatalf("a toolset with remote_keys loaded: %v %v", config.RemoteKeys, err)
	}
}

// A profile cannot change its remote_keys itself, under any self-edit level, and the file it
// already has them in is not touched by an edit that leaves them alone.
func TestSelfEditCannotChangeRemoteKeys(t *testing.T) {
	for _, level := range []string{ProfileSelfEditOn, ProfileSelfEditFull} {
		e := newEditFixture(t, `{"profile_self_edit":"full"}`, `{"profile_self_edit":"`+level+`","remote_keys":["ka"]}`, `{`+editBase[1:]+`,"remote_keys":["ka"]}`)
		wsPut(t, filepath.Join(e.f.global, "remotes", "boxa.json"), `{"kind":"remote","remote":{"url":"http://10.0.0.2:8787","workspace":"proj"}}`)
		wsPut(t, filepath.Join(e.f.global, "auth", "keys", "ka.json"), `{"kind":"remote_key","binding":"boxa","token":"`+keySecret+`"}`)
		if _, err := e.edit(t, editBase+`,"remote_keys":["ka","kb"]}`); err == nil || !strings.Contains(err.Error(), "remote_keys is not a key of a profile file") {
			t.Errorf("%s: adding a key: %v", level, err)
		}
		if _, err := e.edit(t, editBase+`}`); err == nil || !strings.Contains(err.Error(), "remote_keys is not a key of a profile file") {
			t.Errorf("%s: removing the keys: %v", level, err)
		}
		if _, err := e.edit(t, editBase+`,"remote_keys":["ka"],"instructions":"be brief"}`); err != nil {
			t.Errorf("%s: an edit that leaves remote_keys as they are: %v", level, err)
		}
	}
}
