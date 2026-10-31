package remote

import "path"

// matchGlob matches target against a segment-wise glob: '*' never crosses a '/'.
func matchGlob(glob, target string) (bool, error) { return path.Match(glob, target) }
