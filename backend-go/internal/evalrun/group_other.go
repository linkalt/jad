//go:build !unix

package evalrun

import "os/exec"

// startInGroup leaves command as it is: without process groups, cancellation kills only
// the shell.
func startInGroup(*exec.Cmd) {}
