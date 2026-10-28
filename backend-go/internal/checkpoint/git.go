// Package checkpoint records the workspace before a run changes it and when the
// run ends, so the user can undo the run. It is the Go port of the TypeScript
// checkpoint.ts and checkpoint-git.ts; design_checkpoint.md describes both.
//
// This file is the lowest layer: it knows git and nothing about sessions or runs.
// The workspace is the work tree of a bare repository that lives elsewhere, and
// every command names its own index file, so the user's repository (its index,
// HEAD, refs and stash) is never read or written, and a workspace that is not a
// git repository works the same way.
package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Git failure kinds. "unavailable" means the binary cannot be run at all.
const (
	GitUnavailable = "unavailable"
	GitTimeout     = "timeout"
	GitFailed      = "failed"
)

// GitError is a failed git command.
type GitError struct {
	Failure string
	Message string
}

func (e *GitError) Error() string { return e.Message }

// TreeChange is one path that differs between two trees. Status A and D are
// relative to the first tree.
type TreeChange struct {
	Path    string
	Status  string
	OldMode string
	NewMode string
	OldSHA  string
	NewSHA  string
}

type shadowGit struct {
	gitDir  string
	workDir string
	binary  string
	timeout time.Duration
	// maxOutput is the largest output one git command may return before it is a failure.
	maxOutput int64
}

const (
	snapshotIndex = "snapshot.index"
	restoreIndex  = "restore.index"
	// maxStderrBytes bounds the error text kept from a failed command.
	maxStderrBytes = 4096
	// killWait bounds how long Wait lingers for a killed git's children to close the pipes.
	killWait = 5 * time.Second
)

// fixedConfig keeps a snapshot independent of the user's and the machine's git setup.
var fixedConfig = []string{
	"core.autocrlf=false",
	"core.safecrlf=false",
	"core.fileMode=true",
	"core.symlinks=true",
	"core.fsmonitor=false",
	"core.sharedRepository=0600",
	"core.hooksPath=/dev/null",
	"gc.auto=0",
	"maintenance.auto=false",
}

var inheritedGitVariables = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_NAMESPACE": true,
}

func (g *shadowGit) env(indexFile string, bound bool) []string {
	env := make([]string, 0, len(os.Environ())+12)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !inheritedGitVariables[name] {
			env = append(env, entry)
		}
	}
	// `git init` refuses a work tree it was not asked for, so it runs unbound.
	if bound {
		env = append(env, "GIT_DIR="+g.gitDir, "GIT_WORK_TREE="+g.workDir)
	}
	if indexFile != "" {
		env = append(env, "GIT_INDEX_FILE="+filepath.Join(g.gitDir, indexFile))
	}
	return append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=agent-checkpoint",
		"GIT_AUTHOR_EMAIL=checkpoint@localhost",
		"GIT_COMMITTER_NAME=agent-checkpoint",
		"GIT_COMMITTER_EMAIL=checkpoint@localhost",
	)
}

// limitedBuffer keeps at most max bytes and cancels the command once it is exceeded.
type limitedBuffer struct {
	buffer   bytes.Buffer
	max      int64
	overflow bool
	cancel   context.CancelFunc
	// stopAfter, when positive, cancels the command once more than stopAfter NUL
	// separators were written; stopped records that.
	stopAfter int64
	nuls      int64
	stopped   bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.overflow || b.stopped {
		return len(p), nil
	}
	if b.stopAfter > 0 {
		b.nuls += int64(bytes.Count(p, []byte{0}))
		if b.nuls > b.stopAfter {
			b.stopped = true
			b.cancel()
		}
	}
	if int64(b.buffer.Len())+int64(len(p)) > b.max {
		b.overflow = true
		if b.cancel != nil {
			b.cancel()
		}
		return len(p), nil
	}
	return b.buffer.Write(p)
}

type runOptions struct {
	index   string
	input   []byte
	unbound bool
	// stopAfter ends the command early, without error, once its output holds more
	// than this many NUL-terminated entries.
	stopAfter int64
}

func (g *shadowGit) run(parent context.Context, args []string, options runOptions) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, g.timeout)
	defer cancel()
	full := make([]string, 0, len(fixedConfig)*2+len(args))
	for _, entry := range fixedConfig {
		full = append(full, "-c", entry)
	}
	full = append(full, args...)
	command := exec.CommandContext(ctx, g.binary, full...)
	command.Dir = g.workDir
	command.Env = g.env(options.index, !options.unbound)
	command.WaitDelay = killWait
	if options.input != nil {
		command.Stdin = bytes.NewReader(options.input)
	}
	stdout := &limitedBuffer{max: g.maxOutput, cancel: cancel, stopAfter: options.stopAfter}
	stderr := &limitedBuffer{max: maxStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if stdout.stopped {
		return stdout.buffer.Bytes(), nil
	}
	if stdout.overflow {
		return nil, &GitError{Failure: GitFailed, Message: fmt.Sprintf("git %s returned more than %d bytes", args[0], g.maxOutput)}
	}
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, &GitError{Failure: GitTimeout, Message: fmt.Sprintf("git %s did not finish within %d ms", args[0], g.timeout.Milliseconds())}
	}
	if err == nil {
		return stdout.buffer.Bytes(), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		detail := strings.TrimSpace(stderr.buffer.String())
		if len(detail) > 500 {
			detail = detail[:500]
		}
		if detail != "" {
			detail = ": " + detail
		}
		return nil, &GitError{Failure: GitFailed, Message: fmt.Sprintf("git %s exited with %d%s", args[0], exit.ExitCode(), detail)}
	}
	failure := GitFailed
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		failure = GitUnavailable
	}
	return nil, &GitError{Failure: failure, Message: fmt.Sprintf("cannot run %s: %v", g.binary, err)}
}

// prepare creates the private repository on first use and writes the exclusion list.
func (g *shadowGit) prepare(ctx context.Context, excludePatterns []string) error {
	if _, err := os.Stat(filepath.Join(g.gitDir, "HEAD")); err != nil {
		if err := privateDir(g.gitDir); err != nil {
			return err
		}
		if _, err := g.run(ctx, []string{"init", "--bare", "--quiet", g.gitDir}, runOptions{unbound: true}); err != nil {
			return err
		}
	}
	if err := privateDir(filepath.Dir(g.gitDir)); err != nil {
		return err
	}
	if err := privateDir(g.gitDir); err != nil {
		return err
	}
	if err := privateDir(filepath.Join(g.gitDir, "info")); err != nil {
		return err
	}
	// A git killed by a cancelled context or the timeout leaves its index lock behind. Callers hold the
	// staging mutex and no git of this service runs between commands, so a lock here is stale.
	for _, index := range []string{snapshotIndex, restoreIndex} {
		if err := os.Remove(filepath.Join(g.gitDir, index+".lock")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(g.gitDir, "info", "attributes"), []byte(neutralAttributes), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(g.gitDir, "info", "exclude"), []byte(strings.Join(excludePatterns, "\n")+"\n"), 0o600)
}

// neutralAttributes sits in info/attributes, which outranks every workspace
// .gitattributes, so no eol, ident, filter or encoding conversion alters bytes.
const neutralAttributes = "* -text -eol -ident -filter -working-tree-encoding\n"

// privateDir creates dir owner-only and tightens one that already exists.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// exceedsFiles reports whether the workspace holds more than limit files that
// are not yet in the snapshot index and not excluded. Staging adds every one of
// them, so the answer is a lower bound on the snapshot's file count; the listing
// stops as soon as the limit is passed and writes no object. It also returns the
// nested git repositories (listed as "dir/") that the snapshot leaves out.
func (g *shadowGit) exceedsFiles(ctx context.Context, limit int64) (bool, []string, error) {
	listing, err := g.run(ctx, []string{"ls-files", "-z", "--others", "--exclude-standard"}, runOptions{index: snapshotIndex, stopAfter: limit})
	if err != nil {
		return false, nil, err
	}
	var nested []string
	for _, entry := range strings.Split(string(listing), "\x00") {
		if strings.HasSuffix(entry, "/") {
			nested = append(nested, strings.TrimSuffix(entry, "/"))
		}
	}
	return int64(bytes.Count(listing, []byte{0})) > limit, nested, nil
}

// snapshot stages the whole workspace in the private index and returns the tree
// and its file count. Nested git repositories are not staged: git would record one
// as a single gitlink that checkout cannot restore, and fails the whole add for one
// without a commit.
func (g *shadowGit) snapshot(ctx context.Context, nested []string) (string, int, error) {
	add := []string{"add", "--all", "--", "."}
	for _, dir := range nested {
		add = append(add, ":(exclude,literal)"+dir)
	}
	if _, err := g.run(ctx, add, runOptions{index: snapshotIndex}); err != nil {
		return "", 0, err
	}
	tree, err := g.run(ctx, []string{"write-tree"}, runOptions{index: snapshotIndex})
	if err != nil {
		return "", 0, err
	}
	listing, err := g.run(ctx, []string{"ls-files", "-z"}, runOptions{index: snapshotIndex})
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(string(tree)), bytes.Count(listing, []byte{0}), nil
}

func (g *shadowGit) commit(ctx context.Context, tree, message string) (string, error) {
	output, err := g.run(ctx, []string{"commit-tree", tree, "-m", message}, runOptions{})
	return strings.TrimSpace(string(output)), err
}

func (g *shadowGit) setRef(ctx context.Context, ref, commit string) error {
	_, err := g.run(ctx, []string{"update-ref", ref, commit}, runOptions{})
	return err
}

func (g *shadowGit) deleteRef(ctx context.Context, ref string) error {
	_, err := g.run(ctx, []string{"update-ref", "-d", ref}, runOptions{})
	return err
}

// gc reclaims what deleteRef only unreferenced: --prune=now drops the objects the
// deleted refs kept alive at once, where a plain gc would keep them in a cruft
// pack for gc.pruneExpire (two weeks by default). The repository is bare and its
// refs carry no reflog, so nothing can bring a pruned object back.
func (g *shadowGit) gc(ctx context.Context) error {
	_, err := g.run(ctx, []string{"gc", "--prune=now", "--quiet"}, runOptions{})
	return err
}

var objectSizeLine = regexp.MustCompile(`^(size|size-pack|size-garbage):\s+(\d+)`)

// repositoryBytes is what the private repository occupies: loose objects plus packs.
func (g *shadowGit) repositoryBytes(ctx context.Context) (int64, error) {
	output, err := g.run(ctx, []string{"count-objects", "-v"}, runOptions{})
	if err != nil {
		return 0, err
	}
	var kibibytes int64
	for _, line := range strings.Split(string(output), "\n") {
		if match := objectSizeLine.FindStringSubmatch(line); match != nil {
			value, _ := strconv.ParseInt(match[2], 10, 64)
			kibibytes += value
		}
	}
	return kibibytes * 1024, nil
}

// diff lists the paths that differ between two trees, with the mode and blob of each side.
func (g *shadowGit) diff(ctx context.Context, from, to string) ([]TreeChange, error) {
	if from == to {
		return nil, nil
	}
	raw, err := g.run(ctx, []string{"diff-tree", "-r", "-z", "--no-renames", "--raw", "--no-abbrev", from, to}, runOptions{})
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(raw), "\x00")
	var changes []TreeChange
	for index := 0; index+1 < len(parts); index += 2 {
		meta, path := parts[index], parts[index+1]
		if !strings.HasPrefix(meta, ":") {
			continue
		}
		fields := strings.Split(meta[1:], " ")
		for len(fields) < 5 {
			fields = append(fields, "")
		}
		letter := ""
		if fields[4] != "" {
			letter = fields[4][:1]
		}
		if letter != "A" && letter != "D" && letter != "M" && letter != "T" {
			continue
		}
		changes = append(changes, TreeChange{Path: path, Status: letter, OldMode: fields[0], NewMode: fields[1], OldSHA: fields[2], NewSHA: fields[3]})
	}
	return changes, nil
}

// checkout writes paths of tree into the workspace, replacing what is there.
func (g *shadowGit) checkout(ctx context.Context, tree string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if _, err := g.run(ctx, []string{"read-tree", tree}, runOptions{index: restoreIndex}); err != nil {
		return err
	}
	_, err := g.run(ctx, []string{"checkout-index", "--force", "-z", "--stdin"}, runOptions{
		index: restoreIndex,
		input: []byte(strings.Join(paths, "\x00") + "\x00"),
	})
	return err
}
