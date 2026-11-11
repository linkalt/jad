package app

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/tools/native"
)

// T-007 Q3: a stop, reset or delete of the caller reaches what its chain left going and the run
// tree gives every place back. The chain is dev/coder → explore → review/reviewer, whose run starts
// finder as a handle and ends: its message node stays as an ended parent with finder live below
// it. The fan-out stops finder through its lineage; finder's node goes at its end and the ended
// message node with it (its lineage drained). The reviewer's own finder, a root of its own, stays.
func TestFanOutsReleaseTheEndedParentAndItsLiveDescendant(t *testing.T) {
	cases := []struct {
		name, target, session, reason string
		fanOut                        func(t *testing.T, c *lineageChain)
	}{
		{"stop", "review/reviewer", "demo", "user_stop", func(t *testing.T, c *lineageChain) {
			c.post(t, "/api/sessions/local:demo/run/stop?profile="+url.QueryEscape("dev/coder"))
		}},
		{"reset", "review/reviewer", "demo", "session_reset", func(t *testing.T, c *lineageChain) {
			c.post(t, "/api/sessions/local:demo/reset?profile="+url.QueryEscape("dev/coder"))
		}},
		{"delete", "other/review/reviewer", "other", "session_delete", func(t *testing.T, c *lineageChain) {
			current := c.f.catalogCall(t, http.MethodGet, "/api/catalog/sessions/demo", "", nil)
			if reply := c.f.catalogCall(t, http.MethodDelete, "/api/catalog/sessions/demo", "", map[string]string{"If-Match": current.etag}); reply.status != http.StatusNoContent {
				t.Fatalf("delete: %d %s", reply.status, reply.body)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newLineageChain(t, lineageOptions{target: tc.target})
			r := c.f.service.resolver
			c.startOwnWork(t, tc.session)
			c.startChain(t)
			var own, message, finder native.RunNodeInfo
			pollFor(t, "the ended message node with finder below it", func() bool {
				own, message, finder = native.RunNodeInfo{}, native.RunNodeInfo{}, native.RunNodeInfo{}
				nodes := r.runTree.Nodes()
				for _, node := range nodes {
					switch {
					case node.Kind == native.RunNodeProfileMessage:
						message = node
					case node.Parent == "":
						own = node
					default:
						finder = node
					}
				}
				return len(nodes) == 3 && message.State == native.RunNodeEnded && finder.Parent == message.ID
			})
			if own.Kind != native.RunNodeSubagent || finder.Root != message.Root || finder.Session != own.Session {
				t.Fatalf("tree before the fan-out: own %+v, message %+v, finder %+v", own, message, finder)
			}
			if got := r.runTree.SessionRuns(own.Session); got != 2 {
				t.Fatalf("the reviewer's session counts %d runs, want its own finder and the chain's", got)
			}
			tc.fanOut(t, c)
			c.expect(t, "look around", "stopped", tc.reason)
			pollFor(t, "only the reviewer's own finder to be left", func() bool {
				nodes := r.runTree.Nodes()
				return len(nodes) == 1 && nodes[0].ID == own.ID
			})
			if got := r.runTree.SessionRuns(own.Session); got != 1 {
				t.Fatalf("after the fan-out the reviewer's session counts %d runs, want 1", got)
			}
			r.nodeDrains.mu.Lock()
			pending := len(r.nodeDrains.pending)
			r.nodeDrains.mu.Unlock()
			if pending != 0 {
				t.Fatalf("%d ended nodes still wait for a drain", pending)
			}
			c.expect(t, "watch the logs", "running", "")
		})
	}
}

// A queued profile_message (its target's transcript held) and a running one each hold a node;
// a stop, reset or delete of the caller's session ends the message and gives its node back, so
// the places it held in its target's session are free again.
func TestFanOutsReleaseAQueuedOrRunningMessagesNode(t *testing.T) {
	fanOuts := map[string]func(r *Resolver, reference string) int{
		"stop":   func(r *Resolver, reference string) int { return r.CancelSessionChildren(reference, "dev/coder") },
		"reset":  func(r *Resolver, reference string) int { return r.ResetSessionChildren(reference, "dev/coder") },
		"delete": func(r *Resolver, reference string) int { return r.CancelAllSessionChildren(reference) },
	}
	for name, fanOut := range fanOuts {
		t.Run(name+"/queued", func(t *testing.T) {
			p := newPMFixture(t, nil)
			r := p.f.service.resolver
			hold := p.holdTranscript(t)
			defer hold()
			id := p.handle(t, p.send(t, "alpha-1"))
			pollFor(t, "the queued message's node", func() bool {
				nodes := pmNodes(p)
				return len(nodes) == 1 && nodes[0].State == native.RunNodeQueued
			})
			if n := fanOut(r, sessionReference(t, p.f)); n == 0 {
				t.Fatal("the fan-out found nothing to stop")
			}
			// A reset or delete drops the caller's records of the handle; a stop keeps them.
			if name == "stop" {
				if got := p.wait(t, id); !strings.Contains(got, `"state":"stopped"`) {
					t.Fatalf("the queued message did not stop:\n%s", got)
				}
			}
			awaitEmptyRunTree(t, r)
		})
		t.Run(name+"/running", func(t *testing.T) {
			p := newPMFixture(t, nil)
			r := p.f.service.resolver
			reference := sessionReference(t, p.f)
			running := make(chan struct{})
			answer := p.f.script
			var once sync.Once
			p.f.script = func(model, body string) string {
				if strings.Contains(body, "alpha-1") {
					once.Do(func() {
						// The target's run is waiting for its model: the message's node is
						// running when the fan-out comes.
						if nodes := pmNodes(p); len(nodes) != 1 || nodes[0].State != native.RunNodeRunning {
							t.Errorf("the message's node while its run goes: %+v", nodes)
						}
						fanOut(r, reference)
						// The stop reaches the run while its model call is on its way: the call
						// is answered only once the message has ended (or after 5 s), so the
						// answer cannot win against the stop.
						deadline := time.Now().Add(5 * time.Second)
						for len(pmNodes(p)) != 0 && time.Now().Before(deadline) {
							time.Sleep(5 * time.Millisecond)
						}
						close(running)
					})
				}
				return answer(model, body)
			}
			id := p.handle(t, p.send(t, "alpha-1"))
			awaitSignal(t, running, "the fan-out during the target's run")
			if name == "stop" {
				if got := p.wait(t, id); !strings.Contains(got, `"state":"stopped"`) {
					t.Fatalf("the running message did not stop:\n%s", got)
				}
			}
			awaitEmptyRunTree(t, r)
		})
	}
}
