//go:build !unix

package app

func umask(mask int) int { return 0 }
