package remote

import "path"

// matchGlob matches target against a segment-wise glob: '*' never crosses a '/'.
func matchGlob(glob, target string) (bool, error) { return path.Match(glob, target) }

// MatchGlobs reports whether any glob matches target.
func MatchGlobs(globs []string, target string) bool {
	for _, glob := range globs {
		if ok, err := path.Match(glob, target); err == nil && ok {
			return true
		}
	}
	return false
}
