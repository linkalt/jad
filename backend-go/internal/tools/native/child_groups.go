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
	groups map[int]time.Duration
}

// WithChildGroups scopes the groups of the calls made under the returned context to one
// run. The returned function stops what is still alive: SIGTERM now, SIGKILL after each
// call's kill grace. Call it once, when the run ends.
func WithChildGroups(ctx context.Context) (context.Context, func()) {
	groups := &ChildGroups{groups: make(map[int]time.Duration)}
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
	groups.groups[pgid] = grace
	groups.mu.Unlock()
}

func (groups *ChildGroups) sweep() {
	groups.mu.Lock()
	pending := groups.groups
	groups.groups = make(map[int]time.Duration)
	groups.mu.Unlock()
	for pgid, grace := range pending {
		if !processGroupAlive(pgid) {
			continue
		}
		signalProcessGroupID(pgid, false)
		time.AfterFunc(grace, func() {
			if processGroupAlive(pgid) {
				signalProcessGroupID(pgid, true)
			}
		})
	}
}
