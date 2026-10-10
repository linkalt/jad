package rootsetting

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCandidatesOrder(t *testing.T) {
	got := Candidates("/work/app", "/home/me")
	want := []string{
		"/work/app/.jad/setting.json",
		"/home/me/.jad/setting.json",
		"/work/app/setting.json",
		"/work/setting.json",
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
}

func TestCandidatesWithoutAHomeSkipTheHomeFile(t *testing.T) {
	for _, path := range Candidates("/work/app", "") {
		if path == ".jad/setting.json" || path == "/.jad/setting.json" {
			t.Fatalf("a home file was invented: %v", path)
		}
	}
	if len(Candidates("/work/app", "")) != 3 {
		t.Fatalf("candidates = %v", Candidates("/work/app", ""))
	}
}

func TestFindPrefersTheProjectFileThenHomeThenTheOldPlaces(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	write(t, filepath.Join(cwd, "setting.json"))
	got, err := Find(cwd, home)
	if err != nil || got != filepath.Join(cwd, "setting.json") {
		t.Fatalf("old place: %q, %v", got, err)
	}
	write(t, filepath.Join(home, ".jad", "setting.json"))
	if got, _ = Find(cwd, home); got != filepath.Join(home, ".jad", "setting.json") {
		t.Fatalf("home must win over the old place: %q", got)
	}
	write(t, filepath.Join(cwd, ".jad", "setting.json"))
	if got, _ = Find(cwd, home); got != filepath.Join(cwd, ".jad", "setting.json") {
		t.Fatalf("the project file must win: %q", got)
	}
}

func TestFindReturnsNothingWhenThereIsNoFile(t *testing.T) {
	got, err := Find(t.TempDir(), t.TempDir())
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindSkipsADirectoryNamedLikeTheFile(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".jad", "setting.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(cwd, "setting.json"))
	got, err := Find(cwd, "")
	if err != nil || got != filepath.Join(cwd, "setting.json") {
		t.Fatalf("got %q, %v", got, err)
	}
}
