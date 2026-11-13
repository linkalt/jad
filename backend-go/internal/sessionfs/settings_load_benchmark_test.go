package sessionfs

import (
	"path/filepath"
	"testing"
)

// BenchmarkLoadWorkspaceSessionConfig is the workspace layout (v2) load of a profile that runs
// one own model and one own toolset, each based on a global one (decider 3c.3b, for the record).
func BenchmarkLoadWorkspaceSessionConfig(b *testing.B) {
	f := newWSFixture(b)
	wsPut(b, filepath.Join(f.global, "tools", "coding", "full.json"), ownBaseToolset)
	wsPut(b, filepath.Join(f.global, "env", "exec.env"), "A=1\n")
	wsPut(b, filepath.Join(f.global, "mcp", "srv", "one.json"), `{"command":"true"}`)
	w := f.workspace(b, "proj", `{}`)
	wsPut(b, filepath.Join(w, ".jad", "profiles", "dev", "coder.json"), `{"models":["openai/default","profile:openai/fast"],"model_default":"profile:openai/fast","toolset":"profile:coding/lean"}`)
	own := filepath.Join(w, ".jad", "profile-own", "dev", "coder")
	wsPut(b, filepath.Join(own, "models", "openai", "fast.json"), `{"based_on":"global:openai/default","set":{"request":{"temperature":0.2}}}`)
	wsPut(b, filepath.Join(own, "tools", "coding", "lean.json"), ownLean)
	location, err := Resolve(wsMustLoad(b, f, w).Roots, "s1")
	if err != nil {
		b.Fatal(err)
	}
	if config, err := LoadSessionConfig(location, "", ""); err != nil || len(config.ModelBases) != 1 || config.NativeExec == nil || config.NativeExec.Child.TimeoutMs != 2000 {
		b.Fatalf("fixture: %v", err)
	}
	b.Run("settings", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			if _, err := LoadSessionSettings(location, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("settings-and-model", func(b *testing.B) {
		b.ReportAllocs()
		for iteration := 0; iteration < b.N; iteration++ {
			if _, err := LoadSessionConfig(location, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}
