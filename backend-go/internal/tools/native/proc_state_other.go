//go:build !linux && !windows

package native

// processIsZombie is not read on this system: kill(pid, 0) alone decides liveness.
func processIsZombie(int) bool { return false }

// groupHasLiveMember is not read on this system: kill(-pgid, 0) alone decides liveness,
// so a group whose only members are unreaped zombies still reads as alive.
func groupHasLiveMember(int) (live, known bool) { return false, false }
