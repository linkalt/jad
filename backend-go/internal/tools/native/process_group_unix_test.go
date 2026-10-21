//go:build linux

package native

import (
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func startGroupLeader(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", script)
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		t.Skip("cannot start /bin/sh: " + err.Error())
	}
	return command
}

// After the group is gone its id may be handed to an unrelated process, so the pending hard
// kill must be cancelled.
func TestCancelKillIfGroupGoneStopsTheTimerOfAFinishedGroup(t *testing.T) {
	command := startGroupLeader(t, "exit 0")
	_ = command.Wait()
	var fired atomic.Bool
	timer := time.AfterFunc(80*time.Millisecond, func() { fired.Store(true) })
	cancelKillIfGroupGone(timer, command)
	time.Sleep(250 * time.Millisecond)
	if fired.Load() {
		t.Fatal("the kill timer fired for a group that no longer exists")
	}
}

func TestCancelKillIfGroupGoneKeepsTheTimerWhileAMemberLives(t *testing.T) {
	command := startGroupLeader(t, "sleep 30")
	defer func() {
		signalProcessGroup(command, true)
		_ = command.Wait()
	}()
	var fired atomic.Bool
	timer := time.AfterFunc(80*time.Millisecond, func() { fired.Store(true) })
	cancelKillIfGroupGone(timer, command)
	deadline := time.Now().Add(2 * time.Second)
	for !fired.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !fired.Load() {
		t.Fatal("the kill timer was cancelled while the group still has a member")
	}
}

func TestKillGroupAfterKillsAGroupThatSurvivedItsLeader(t *testing.T) {
	command := startGroupLeader(t, "sleep 30 & exit 0")
	_ = command.Wait()
	timer := killGroupAfter(command, 50*time.Millisecond)
	defer timer.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for processGroupAlive(command.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processGroupAlive(command.Process.Pid) {
		signalProcessGroupID(command.Process.Pid, true)
		t.Fatal("the surviving member of the group was not killed")
	}
}
