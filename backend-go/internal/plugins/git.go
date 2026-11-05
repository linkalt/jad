package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// GitSource is a plugin in a git repository: the repository's URL, the branch, tag or
// commit to install (the default branch when empty) and the plugin's folder in it (the
// repository's top when empty).
type GitSource struct {
	URL    string
	Ref    string
	Subdir string
	// Timeout bounds the clone and checkout; zero means no limit beyond ctx.
	Timeout time.Duration
	// Git is the git program; empty is "git" from PATH.
	Git string
}

var (
	// scpLikeGitURL is git's user@host:path form, as in git@github.com:owner/repo.git.
	scpLikeGitURL = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^/\\]`)
	// commitRef is a full or abbreviated commit name, checked out after a full clone.
	commitRef  = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	gitSchemes = []string{"https://", "http://", "ssh://", "git://", "file://"}
)

// IsGitURL reports whether source names a git repository rather than a folder: a URL of
// a scheme git fetches over, or git's user@host:path form.
func IsGitURL(source string) bool {
	lower := strings.ToLower(source)
	for _, scheme := range gitSchemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return scpLikeGitURL.MatchString(source)
}

// RedactGitURL is the URL with any user name and password left out, as the record and the
// messages show it: an https URL may carry a token.
func RedactGitURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.User == nil {
		return raw
	}
	if _, hasPassword := parsed.User.Password(); !hasPassword && parsed.Scheme == "ssh" {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}

// InstallGit clones the repository into a temporary folder, installs the plugin in its
// Subdir as Install does, and removes the clone. The record's source is the URL without
// credentials, the ref when one was asked for, the folder and the commit installed.
func (t Target) InstallGit(ctx context.Context, source GitSource, force bool) (InstallResult, error) {
	if !IsGitURL(source.URL) {
		return InstallResult{}, fmt.Errorf("%s is not a git URL (https://, http://, ssh://, git://, file:// or user@host:path)", RedactGitURL(source.URL))
	}
	if source.Ref != "" && (strings.HasPrefix(source.Ref, "-") || strings.ContainsAny(source.Ref, " \t\r\n~^:?*[\\") || strings.Contains(source.Ref, "..")) {
		return InstallResult{}, fmt.Errorf("--ref %q is not a branch, tag or commit name", source.Ref)
	}
	subdir := filepath.Clean(filepath.FromSlash(source.Subdir))
	if source.Subdir != "" && (!filepath.IsLocal(subdir)) {
		return InstallResult{}, fmt.Errorf("--subdir %q must be a relative folder inside the repository", source.Subdir)
	}
	workspace, err := os.MkdirTemp("", "jad-plugin-git-")
	if err != nil {
		return InstallResult{}, err
	}
	defer os.RemoveAll(workspace)
	cloneCtx := ctx
	if source.Timeout > 0 {
		var cancel context.CancelFunc
		cloneCtx, cancel = context.WithTimeout(ctx, source.Timeout)
		defer cancel()
	}
	checkout := filepath.Join(workspace, "repository")
	commit, err := cloneRef(cloneCtx, source, checkout)
	if err != nil {
		if cloneCtx.Err() != nil && ctx.Err() == nil {
			return InstallResult{}, fmt.Errorf("git clone of %s did not finish within %s", RedactGitURL(source.URL), source.Timeout)
		}
		return InstallResult{}, err
	}
	folder := checkout
	if source.Subdir != "" {
		folder = filepath.Join(checkout, subdir)
		// A symbolic link in the repository must not lead the install out of the clone.
		resolved, err := filepath.EvalSymlinks(folder)
		if err != nil {
			return InstallResult{}, fmt.Errorf("--subdir %s is not a folder of the repository", source.Subdir)
		}
		top, err := filepath.EvalSymlinks(checkout)
		if err != nil {
			return InstallResult{}, err
		}
		if relative, err := filepath.Rel(top, resolved); err != nil || !filepath.IsLocal(relative) {
			return InstallResult{}, fmt.Errorf("--subdir %s leads out of the repository", source.Subdir)
		}
		folder = resolved
	}
	label := RedactGitURL(source.URL)
	if source.Ref != "" {
		label += "#" + source.Ref
	}
	if source.Subdir != "" {
		label += " " + filepath.ToSlash(subdir)
	}
	label += " @ " + commit
	return t.install(ctx, folder, label, force)
}

// cloneRef clones the repository into dir at the ref and returns the commit checked out. A
// branch or tag is a shallow clone of that ref alone; a commit needs the history, so the
// clone is full and the commit is checked out after it.
func cloneRef(ctx context.Context, source GitSource, dir string) (string, error) {
	program := source.Git
	if program == "" {
		program = "git"
	}
	var steps [][]string
	switch {
	case source.Ref == "":
		steps = [][]string{{"clone", "--depth", "1", "--no-tags", "--", source.URL, dir}}
	case commitRef.MatchString(source.Ref):
		steps = [][]string{{"clone", "--no-checkout", "--", source.URL, dir}, {"-C", dir, "checkout", "--detach", source.Ref}}
	default:
		steps = [][]string{{"clone", "--depth", "1", "--no-tags", "--branch", source.Ref, "--", source.URL, dir}}
	}
	for _, step := range steps {
		if _, err := runGit(ctx, program, source.URL, step...); err != nil {
			return "", err
		}
	}
	commit, err := runGit(ctx, program, source.URL, "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(commit), nil
}

// runGit runs one git command that never asks for a password (a missing credential fails
// at once) and does not use the ext transport. Its error is git's own message with the
// URL's credentials left out.
func runGit(ctx context.Context, program, rawURL string, args ...string) (string, error) {
	full := append([]string{"-c", "protocol.ext.allow=never"}, args...)
	command := exec.CommandContext(ctx, program, full...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		var missing *exec.Error
		if errors.As(err, &missing) {
			return "", fmt.Errorf("installing from a git URL needs git: %w", err)
		}
		message := strings.TrimSpace(strings.ReplaceAll(stderr.String(), rawURL, RedactGitURL(rawURL)))
		if message == "" {
			message = err.Error()
		}
		verb := args[0]
		if verb == "-C" && len(args) > 2 {
			verb = args[2]
		}
		return "", fmt.Errorf("git %s: %s", verb, message)
	}
	return stdout.String(), nil
}
