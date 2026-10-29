package native

import (
	"context"
	"sync"
	"time"
)

type childGroupsKey struct{}

// ChildGroups holds the process groups that bash, exec and tool_smith calls of one run
// left alive, so the run's end can stop them. Processes of the process tool are not here.
type ChildGroups struct {
	mu     sync.Mutex
	groups map[int]childGroup
}

// childGroup is one group a finished call left behind. The group id alone is not an
// identity: the system hands the number out again once the whole group is gone, and the
// sweep runs at the end of the run, which can be hours after the call. Where the system
// reports it, the leader's start time is kept too, so the sweep can tell a recycled id from
// the group it registered.
type childGroup struct {
	grace time.Duration
	// start is the leader's start time (clock ticks since boot on Linux); zero means the
	// system does not report one, and only liveness is checked.
	start uint64
}

// alive reports whether pgid is still the group this call registered: still alive, and —
// where a start time was recorded and can be read — still led by the same process.
func (group childGroup) alive(pgid int) bool {
	if !processGroupAlive(pgid) {
		return false
	}
	if group.start == 0 {
		return true
	}
	current := processStartTime(pgid)
	return current == 0 || current == group.start
}

// WithChildGroups scopes the groups of the calls made under the returned context to one
// run. The returned function stops what is still alive: SIGTERM now, SIGKILL after each
// call's kill grace. Call it once, when the run ends.
func WithChildGroups(ctx context.Context) (context.Context, func()) {
	groups := &ChildGroups{groups: make(map[int]childGroup)}
	return context.WithValue(ctx, childGroupsKey{}, groups), groups.sweep
}

func childGroupsFrom(ctx context.Context) *ChildGroups {
	groups, _ := ctx.Value(childGroupsKey{}).(*ChildGroups)
	return groups
}

// add registers a group at the end of its call, only while a member is still alive, so a
// finished group's id is never signalled later.
func (groups *ChildGroups) add(pgid int, grace time.Duration) {
	if !processGroupAlive(pgid) {
		return
	}
	groups.mu.Lock()
	groups.dropDeadLocked()
	groups.groups[pgid] = childGroup{grace: grace, start: processStartTime(pgid)}
	groups.mu.Unlock()
}

// dropDeadLocked forgets the groups that are gone, so a later call does not record them
// again and the sweep has fewer ids to look at.
func (groups *ChildGroups) dropDeadLocked() {
	for pgid, group := range groups.groups {
		if !group.alive(pgid) {
			delete(groups.groups, pgid)
		}
	}
}

func (groups *ChildGroups) sweep() {
	groups.mu.Lock()
	pending := groups.groups
	groups.groups = make(map[int]childGroup)
	groups.mu.Unlock()
	for pgid, group := range pending {
		if !group.alive(pgid) {
			continue
		}
		signalProcessGroupID(pgid, false)
		time.AfterFunc(group.grace, func() {
			if group.alive(pgid) {
				signalProcessGroupID(pgid, true)
			}
		})
	}
}
