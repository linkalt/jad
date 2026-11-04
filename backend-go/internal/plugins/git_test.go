package plugins

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRepository makes a repository from versions of a plugin, one commit each, tagging
// commit i as v<i+1>; the plugin sits under prefix. It returns the file:// URL and the
// commits.
func gitRepository(t *testing.T, prefix string, versions ...map[string]string) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	var commits []string
	for index, files := range versions {
		if prefix != "" {
			_ = os.RemoveAll(filepath.Join(repo, filepath.FromSlash(prefix)))
		}
		for path, contents := range files {
			full := filepath.Join(repo, filepath.FromSlash(prefix), filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		git("add", "-A")
		git("commit", "-q", "-m", "v"+string(rune('1'+index)))
		git("tag", "v"+string(rune('1'+index)))
		commits = append(commits, git("rev-parse", "HEAD"))
	}
	return "file://" + filepath.ToSlash(repo), commits
}

func TestInstallFromGitAtTheDefaultBranchATagAndACommit(t *testing.T) {
	target, _ := testTarget(t)
	url, commits := gitRepository(t, "", acmePlugin("1.0.0"), acmePlugin("2.0.0"))
	ctx := context.Background()

	result, err := target.InstallGit(ctx, GitSource{URL: url, Timeout: time.Minute}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Version != "2.0.0" || result.Record.Source != url+" @ "+commits[1] {
		t.Fatalf("default branch record = %+v", result.Record)
	}
	result, err = target.InstallGit(ctx, GitSource{URL: url, Ref: "v1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Version != "1.0.0" || result.Upgraded == nil || result.Record.Source != url+"#v1 @ "+commits[0] {
		t.Fatalf("tag record = %+v", result.Record)
	}
	result, err = target.InstallGit(ctx, GitSource{URL: url, Ref: commits[1][:12]}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Version != "2.0.0" || !strings.HasSuffix(result.Record.Source, " @ "+commits[1]) {
		t.Fatalf("commit record = %+v", result.Record)
	}
	// The clone is gone: only the installed files remain.
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "jad-plugin-git-*"))
	for _, match := range matches {
		if info, err := os.Stat(match); err == nil && time.Since(info.ModTime()) < time.Minute {
			t.Fatalf("a clone was left behind: %s", match)
		}
	}
}

func TestInstallFromGitSubdirAndRefusals(t *testing.T) {
	target, _ := testTarget(t)
	url, commits := gitRepository(t, "plugins/acme", acmePlugin("3.0.0"))
	ctx := context.Background()
	result, err := target.InstallGit(ctx, GitSource{URL: url, Subdir: "plugins/acme"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Version != "3.0.0" || result.Record.Source != url+" plugins/acme @ "+commits[0] {
		t.Fatalf("subdir record = %+v", result.Record)
	}
	for name, test := range map[string]struct {
		source GitSource
		want   string
	}{
		"top is no plugin": {GitSource{URL: url}, "plugin.json"},
		"subdir escapes":   {GitSource{URL: url, Subdir: "../x"}, "must be a relative folder inside the repository"},
		"subdir absolute":  {GitSource{URL: url, Subdir: "/etc"}, "must be a relative folder"},
		"subdir missing":   {GitSource{URL: url, Subdir: "plugins/none"}, "is not a folder of the repository"},
		"ref option":       {GitSource{URL: url, Ref: "--upload-pack=x"}, "is not a branch, tag or commit name"},
		"ref with dots":    {GitSource{URL: url, Ref: "a..b"}, "is not a branch, tag or commit name"},
		"unknown branch":   {GitSource{URL: url, Ref: "nope"}, "git clone:"},
		"not a URL":        {GitSource{URL: "plugins/acme"}, "is not a git URL"},
		"no such repo":     {GitSource{URL: "file:///nonexistent/jad-repo"}, "git clone:"},
		"token kept out":   {GitSource{URL: "https://user:s3cret@127.0.0.1:1/r.git", Timeout: 20 * time.Second}, "git clone:"},
	} {
		_, err := target.InstallGit(ctx, test.source, false)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v, want %q", name, err, test.want)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: the error repeats the password: %v", name, err)
		}
	}
}

func TestInstallFromGitSubdirMayNotLinkOut(t *testing.T) {
	target, _ := testTarget(t)
	outside := writePlugin(t, acmePlugin("9.0.0"))
	url, _ := gitRepository(t, "", map[string]string{"README.md": "x"})
	repo := strings.TrimPrefix(url, "file://")
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Skip("no symbolic links here")
	}
	for _, args := range [][]string{{"add", "link"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "link"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	_, err := target.InstallGit(context.Background(), GitSource{URL: url, Subdir: "link"}, false)
	if err == nil || !strings.Contains(err.Error(), "leads out of the repository") {
		t.Fatalf("error = %v", err)
	}
}

func TestIsGitURLAndRedaction(t *testing.T) {
	for source, want := range map[string]bool{
		"https://github.com/o/r.git": true, "HTTP://h/r": true, "ssh://git@h/r": true, "git://h/r": true,
		"file:///srv/r": true, "git@github.com:o/r.git": true,
		"./plugins/acme": false, "plugins/acme": false, "/srv/acme": false, "C:\\acme": false, "acme": false,
	} {
		if got := IsGitURL(source); got != want {
			t.Errorf("IsGitURL(%q) = %v, want %v", source, got, want)
		}
	}
	for raw, want := range map[string]string{
		"https://user:tok@h/r.git": "https://h/r.git",
		"https://tok@h/r.git":      "https://h/r.git",
		"ssh://git@h/r":            "ssh://git@h/r",
		"git@h:o/r.git":            "git@h:o/r.git",
	} {
		if got := RedactGitURL(raw); got != want {
			t.Errorf("RedactGitURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
