//go:build linux

package native

import (
	"syscall"
	"testing"
	"time"
)

// waitForZombie polls until pid has exited and is not reaped yet (nobody calls Wait on it).
func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if stat, ok := readProcStat(pid); ok && procStateDead(stat.state) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d never became a zombie", pid)
		}
	}
}

// A group whose only member has exited but is not reaped still answers kill(-pgid, 0). In a
// container whose init does not reap orphans such a group stays forever, and treating it as
// alive made every group stop wait its whole kill grace (audit B1).
func TestProcessGroupOfOnlyZombiesIsNotAlive(t *testing.T) {
	command := startGroupLeader(t, "exit 0")
	defer command.Wait()
	pid := command.Process.Pid
	waitForZombie(t, pid)
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Skipf("kill(-pgid, 0) does not see the zombie group here: %v", err)
	}
	if processGroupAlive(pid) {
		t.Fatal("a group whose only member is a zombie reads as alive")
	}
	if processExists(pid) {
		t.Fatal("a zombie process reads as existing")
	}
}

func TestProcessGroupWithALiveMemberIsAlive(t *testing.T) {
	// The leader exits at once and stays a zombie; the background sleep keeps the group alive.
	command := startGroupLeader(t, "sleep 30 & exit 0")
	pid := command.Process.Pid
	defer func() {
		signalProcessGroupID(pid, true)
		_ = command.Wait()
	}()
	waitForZombie(t, pid)
	if !processGroupAlive(pid) {
		t.Fatal("a group with a running member reads as gone")
	}
	if processExists(pid) {
		t.Fatal("the zombie leader reads as existing")
	}
}

func TestParseProcStatReadsStateAndGroupAfterTheLastParenthesis(t *testing.T) {
	stat, ok := parseProcStat("42 (a) b) (c) Z 1 40 40 0 -1 4194560 0 0")
	if !ok || stat.state != 'Z' || stat.pgrp != 40 {
		t.Fatalf("stat = %+v, ok = %v", stat, ok)
	}
	if _, ok := parseProcStat("42 no-parenthesis"); ok {
		t.Fatal("a line without a command name parsed")
	}
}

// A leader that already exited (a zombie nobody reaps) can leave a member that ignores
// SIGTERM. stopOrphanGroups must still hard-kill it, and report the group gone afterwards.
func TestStopOrphanGroupsHardKillsAMemberOfAZombieLeader(t *testing.T) {
	command := startGroupLeader(t, "trap '' TERM; sleep 120 & exit 0")
	pid := command.Process.Pid
	defer func() {
		signalProcessGroupID(pid, true)
		_ = command.Wait()
	}()
	waitForZombie(t, pid)
	if !processGroupAlive(pid) {
		t.Fatal("the background member is not running")
	}
	gone := stopOrphanGroups([]int{pid}, 200*time.Millisecond)
	if processGroupAlive(pid) {
		t.Fatal("the member that ignores SIGTERM survived the stop")
	}
	if !gone[pid] {
		t.Fatal("the stopped group is not reported gone")
	}
}
