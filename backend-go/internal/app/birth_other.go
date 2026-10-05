//go:build !linux

package app

// fileBirthMillis is unavailable without statx; the generation then relies on
// device and inode alone.
func fileBirthMillis(string) int64 { return 0 }
