//go:build !windows

package native

import "syscall"

const toolSmithOpenNoFollow = syscall.O_NOFOLLOW
