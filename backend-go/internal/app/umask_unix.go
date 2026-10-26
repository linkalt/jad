//go:build unix

package app

import "syscall"

func umask(mask int) int { return syscall.Umask(mask) }
