package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// workingSnapshot is a commit of the session's working state on top of base: the tracked
// and untracked files that are not ignored, as they are now. It is built with a copy of the
// session's index (so only changed files are hashed) and leaves the session's index, files
// and refs alone; the commit is reachable from no ref, so git's own pruning collects it
// unless a run's branch keeps it. Without a change it is base itself.
func workingSnapshot(ctx context.Context, options Options, top, base, name string) (string, error) {
	gitDir, err := run(ctx, options, top, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	index, err := os.CreateTemp("", "jad-index-*")
	if err != nil {
		return "", fmt.Errorf("create the snapshot index: %w", err)
	}
	defer func() { _ = os.Remove(index.Name()) }()
	source, err := os.Open(filepath.Join(gitDir, "index"))
	switch {
	case err == nil:
		_, err = io.Copy(index, source)
		_ = source.Close()
	case errors.Is(err, os.ErrNotExist):
		err = nil
	}
	if closeErr := index.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("copy the session's index: %w", err)
	}
	env := []string{"GIT_INDEX_FILE=" + index.Name()}
	if _, _, err := runGit(ctx, options, gitCommand{dir: top, args: []string{"add", "--all"}, env: env}); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}
	tree, _, err := runGit(ctx, options, gitCommand{dir: top, args: []string{"write-tree"}, env: env})
	if err != nil {
		return "", fmt.Errorf("git write-tree: %w", err)
	}
	baseTree, err := run(ctx, options, top, "rev-parse", "--verify", base+"^{tree}")
	if err != nil {
		return "", err
	}
	if tree == baseTree {
		return base, nil
	}
	identity := (&Tree{options: options, common: top}).hostIdentity(ctx)
	commit, err := run(ctx, options, top, append(identity, "commit-tree", tree, "-p", base, "-m", "jad: working state for "+name)...)
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w", err)
	}
	return commit, nil
}
