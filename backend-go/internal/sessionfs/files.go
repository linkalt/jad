package sessionfs

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"jad/backend-go/internal/sessionlock"
)

// SessionFileListing is the safe, bounded @path completion response.
type SessionFileListing struct {
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// CollectSessionFiles enumerates regular files under the configured workspace.
// It never follows directory symlinks, bounds work by settings, and filters
// entries denied by the native read policy.
func CollectSessionFiles(root string, settings SessionFileListingSettings, policy NativePathPolicy) SessionFileListing {
	listing := SessionFileListing{Files: []string{}}
	workspaceRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return listing
	}
	workspaceRoot = filepath.Clean(workspaceRoot)
	rootInfo, err := os.Stat(workspaceRoot)
	if err != nil || !rootInfo.IsDir() {
		return listing
	}

	ignored := make(map[string]struct{}, len(settings.IgnoreDirectories))
	for _, name := range settings.IgnoreDirectories {
		ignored[name] = struct{}{}
	}
	level := []string{""}
	visited := 0
	stopped := false
	for depth := 1; depth <= settings.MaxDepth && len(level) > 0; depth++ {
		nextLevel := make([]string, 0)
		for _, relativeDirectory := range level {
			if stopped {
				break
			}
			directoryPath := root
			if relativeDirectory != "" {
				directoryPath = filepath.Join(root, filepath.FromSlash(relativeDirectory))
			}
			resolvedBefore, contained := resolveContainedDirectory(directoryPath, workspaceRoot, relativeDirectory == "")
			if !contained {
				listing.Truncated = true
				continue
			}
			remaining := settings.MaxEntries - visited
			if remaining < 0 {
				remaining = 0
			}
			entries, hasMore, readErr := readLimitedDirectory(directoryPath, remaining)
			if readErr != nil {
				listing.Truncated = true
				continue
			}
			resolvedAfter, contained := resolveContainedDirectory(directoryPath, workspaceRoot, relativeDirectory == "")
			if !contained || resolvedAfter != resolvedBefore {
				listing.Truncated = true
				continue
			}
			visited += len(entries)
			if hasMore {
				visited++
			}

			for _, entry := range entries {
				if len(listing.Files) >= settings.MaxResults {
					listing.Truncated = true
					stopped = true
					break
				}
				name := entry.Name()
				if sessionlock.IsLockFileName(name) {
					continue
				}
				if !settings.IncludeHidden && strings.HasPrefix(name, ".") {
					continue
				}
				if entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				relativePath := name
				if relativeDirectory != "" {
					relativePath = filepath.ToSlash(filepath.Join(filepath.FromSlash(relativeDirectory), name))
				}
				if entry.IsDir() {
					if _, skip := ignored[name]; !skip {
						nextLevel = append(nextLevel, relativePath)
					}
					continue
				}
				info, infoErr := entry.Info()
				if infoErr != nil {
					listing.Truncated = true
					continue
				}
				if !info.Mode().IsRegular() {
					continue
				}
				filePath := filepath.Join(root, filepath.FromSlash(relativePath))
				resolvedFile, regular := containedRegularFile(filePath, workspaceRoot)
				if !regular {
					listing.Truncated = true
					continue
				}
				if !allowedByNativeReadPolicy(filePath, root, resolvedFile, workspaceRoot, policy) {
					continue
				}
				listing.Files = append(listing.Files, relativePath)
			}
			if hasMore {
				listing.Truncated = true
				stopped = true
			}
		}
		level = nextLevel
	}
	if !stopped && len(level) > 0 {
		listing.Truncated = true
	}
	return listing
}

func readLimitedDirectory(path string, limit int) ([]os.DirEntry, bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(limit + 1)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := strings.ToLower(entries[i].Name()), strings.ToLower(entries[j].Name())
		if left == right {
			return entries[i].Name() < entries[j].Name()
		}
		return left < right
	})
	return entries, hasMore, nil
}

func resolveContainedDirectory(path, workspaceRoot string, isRoot bool) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	resolved = filepath.Clean(resolved)
	if !isPathWithin(workspaceRoot, resolved) || (isRoot && resolved != workspaceRoot) {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return resolved, true
}

func containedRegularFile(path, workspaceRoot string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	resolved = filepath.Clean(resolved)
	if !isPathWithin(workspaceRoot, resolved) {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return resolved, true
}

func allowedByNativeReadPolicy(path, root, resolvedPath, workspaceRoot string, policy NativePathPolicy) bool {
	for _, rule := range policy.Paths {
		if rule.Action == "deny" && isPathWithin(rule.Path, path) {
			return false
		}
	}
	if policy.UnrestrictedPaths || isPathWithin(root, path) || isPathWithin(workspaceRoot, resolvedPath) {
		return true
	}
	for _, rule := range policy.Paths {
		if rule.Action == "allow" && isPathWithin(rule.Path, path) {
			return true
		}
	}
	return false
}

func isPathWithin(root, target string) bool {
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
