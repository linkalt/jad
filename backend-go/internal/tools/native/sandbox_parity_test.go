package native

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/worktree"
)

// parityWorld is a repository with two isolated runs of one materialiser, for the probes of
// TestOverlayAndCheckoutRunsAreConfinedAlike.
type parityWorld struct {
	root        string
	mine, other *worktree.Tree
}

func newParityWorld(t *testing.T, materialize string) parityWorld {
	t.Helper()
	if materialize == sessionfs.SandboxIsolationMaterializeOverlay {
		requireOverlay(t)
	}
	root := gitRepository(t)
	open := func(name string) *worktree.Tree {
		tree, holder, err := openIsolatedTree(context.Background(), isolatedTestOptions(), materialize, nil, nil, root, name, "")
		if err != nil {
			t.Fatal(err)
		}
		registerIsolatedRun(tree)
		if holder != nil {
			attachHolder(tree, holder)
		}
		t.Cleanup(func() {
			forgetIsolatedRun(tree)
			// An overlay's work folder is unreadable once it has been mounted.
			_ = tree.Remove(context.Background())
		})
		return tree
	}
	return parityWorld{root: root, mine: open("mine"), other: open("other")}
}

// parityProbe is one attempt of a sandboxed call: a command that succeeds only when the call
// may do it. Overlay-only probes have no counterpart in a checkout run.
type parityProbe struct {
	name        string
	command     string // with $WS, $ORIGIN, $COMMON, $ADMIN, $OTHER, $OTHERADMIN, $RUNS, $BASE, $UPPER, $OVERLAY
	allowed     bool
	overlayOnly bool
}

var parityProbes = []parityProbe{
	{name: "write a new file in the workspace", command: `echo x > "$WS/new.txt"`, allowed: true},
	{name: "overwrite a tracked file in the workspace", command: `echo x > "$WS/a.txt"`, allowed: true},
	{name: "delete a tracked file in the workspace", command: `rm "$WS/keep/secret.txt"`, allowed: true},
	{name: "make a folder in the workspace", command: `mkdir "$WS/made" && echo x > "$WS/made/f"`, allowed: true},
	{name: "rewrite the workspace's .git file", command: `echo x > "$WS/.git"`, allowed: true},
	{name: "write the call's own temp folder", command: `touch "$TMPDIR/probe"`, allowed: true},
	{name: "write the run's private objects", command: `touch "$ADMIN/jad-objects/probe"`, allowed: true},
	{name: "take the run's index lock", command: `touch "$ADMIN/index.lock"`, allowed: true},
	{name: "write the session's folder", command: `echo x > "$ORIGIN/new.txt"`},
	{name: "overwrite the session's tracked file", command: `echo x > "$ORIGIN/a.txt"`},
	{name: "delete the session's tracked file", command: `rm "$ORIGIN/a.txt"`},
	{name: "plant a hook", command: `echo x > "$COMMON/hooks/post-checkout"`},
	{name: "append to the repository's config", command: `echo x >> "$COMMON/config"`},
	{name: "make a branch ref", command: `echo x > "$COMMON/refs/heads/evil"`},
	{name: "write a shared object", command: `touch "$COMMON/objects/probe"`},
	{name: "write another run's workspace", command: `echo x > "$OTHER/a.txt"`},
	{name: "write another run's admin folder", command: `touch "$OTHERADMIN/probe"`},
	{name: "write the folder of the runs", command: `touch "$RUNS/probe"`},
	{name: "write outside every writable root", command: `echo x > /etc/jad-parity-probe`},
	{name: "read the session's file", command: `cat "$ORIGIN/a.txt"`, allowed: true},
	{name: "read another run's file", command: `cat "$OTHER/a.txt"`},
	{name: "list another run's admin folder", command: `ls "$OTHERADMIN"`},
	{name: "write the base checkout", command: `echo x > "$BASE/a.txt"`, overlayOnly: true},
	{name: "write the run's upper layer", command: `echo x > "$UPPER/probe"`, overlayOnly: true},
	{name: "write the run's overlay folder", command: `touch "$OVERLAY/probe"`, overlayOnly: true},
	{name: "read the run's upper layer", command: `ls "$UPPER"`, overlayOnly: true},
	{name: "read the base checkout", command: `cat "$BASE/a.txt"`, overlayOnly: true},
}

// run executes the probes as one sandboxed call of the run, and returns what each did.
func (w parityWorld) run(t *testing.T, overlay bool, sandbox *sessionfs.SandboxSettings, planMode ...bool) map[string]bool {
	t.Helper()
	common := w.mine.Common()
	paths := map[string]string{
		"WS": w.mine.Workspace, "ORIGIN": w.root, "COMMON": common, "ADMIN": w.mine.Admin,
		"OTHER": w.other.Workspace, "OTHERADMIN": w.other.Admin, "RUNS": filepath.Dir(w.mine.Path),
	}
	if layers, ok := w.mine.Layers(); ok {
		paths["BASE"], paths["UPPER"], paths["OVERLAY"] = layers.Lower[0], layers.Upper, filepath.Dir(layers.Upper)
	}
	var script strings.Builder
	for name, path := range paths {
		fmt.Fprintf(&script, "%s='%s'\n", name, path)
	}
	var names []string
	for index, probe := range parityProbes {
		if probe.overlayOnly && !overlay {
			continue
		}
		names = append(names, probe.name)
		fmt.Fprintf(&script, "if ( %s ) >/dev/null 2>&1; then echo 'probe %d allowed'; else echo 'probe %d blocked'; fi\n", probe.command, index, index)
	}
	output := holderBash(t, w.mine, sandbox, len(planMode) > 0 && planMode[0], script.String())
	results := map[string]bool{}
	for index, probe := range parityProbes {
		switch {
		case strings.Contains(output, fmt.Sprintf("probe %d allowed\n", index)):
			results[probe.name] = true
		case strings.Contains(output, fmt.Sprintf("probe %d blocked\n", index)):
			results[probe.name] = false
		case probe.overlayOnly && !overlay:
		default:
			t.Fatalf("probe %q gave no answer; output:\n%s", probe.name, output)
		}
	}
	return results
}

// F6: one profile, both materialisers: every write and read a sandboxed call of a checkout run may
// or may not do, the call of an overlay run may or may not do the same. The overlay run's own
// layers and the base checkout it mounts are blocked on top, since a checkout run has no such
// folders to protect. Each side is also checked against the expected sets, so a probe that both get
// wrong does not pass as parity.
func TestOverlayAndCheckoutRunsAreConfinedAlike(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	checkout := newParityWorld(t, sessionfs.SandboxIsolationMaterializeCheckout).run(t, false, bwrapSandbox())
	overlay := newParityWorld(t, sessionfs.SandboxIsolationMaterializeOverlay).run(t, true, bwrapSandbox())
	var differ []string
	for _, probe := range parityProbes {
		if probe.overlayOnly {
			if overlay[probe.name] {
				t.Errorf("the overlay run may %s: the layers and the base checkout are the run's to keep apart", probe.name)
			}
			continue
		}
		if checkout[probe.name] != probe.allowed {
			t.Errorf("checkout run, %s: allowed = %v, want %v", probe.name, checkout[probe.name], probe.allowed)
		}
		if overlay[probe.name] != checkout[probe.name] {
			differ = append(differ, fmt.Sprintf("%s (checkout %v, overlay %v)", probe.name, checkout[probe.name], overlay[probe.name]))
		}
	}
	sort.Strings(differ)
	if len(differ) > 0 {
		t.Fatalf("the two materialisers confine a run differently:\n%s", strings.Join(differ, "\n"))
	}
}

// F6, the same profile without a sandbox: a call of either run is unconfined, so what one may do
// the other may too (the overlay run's layers and base checkout are then reachable by path, as
// every other folder of the host is to a checkout run; accepted in docs/isolation-decisions.md).
func TestUnsandboxedOverlayAndCheckoutRunsAreUnconfinedAlike(t *testing.T) {
	useOwnTempDir(t)
	checkout := newParityWorld(t, sessionfs.SandboxIsolationMaterializeCheckout).run(t, false, nil)
	overlay := newParityWorld(t, sessionfs.SandboxIsolationMaterializeOverlay).run(t, true, nil)
	for _, probe := range parityProbes {
		if probe.overlayOnly {
			continue
		}
		if overlay[probe.name] != checkout[probe.name] {
			t.Errorf("%s: checkout %v, overlay %v", probe.name, checkout[probe.name], overlay[probe.name])
		}
	}
	if !checkout["write the session's folder"] || !overlay["write the session's folder"] {
		t.Fatalf("a call without a sandbox could not write the session's folder: probes do not reach it")
	}
}

// F6 in plan mode, where the sandbox keeps a call to its plan paths: the two materialisers agree
// again, and a plan-mode call writes nothing in the workspace of either.
func TestOverlayAndCheckoutRunsAreConfinedAlikeInPlanMode(t *testing.T) {
	requireBwrap(t)
	useOwnTempDir(t)
	checkout := newParityWorld(t, sessionfs.SandboxIsolationMaterializeCheckout).run(t, false, bwrapSandbox(), true)
	overlay := newParityWorld(t, sessionfs.SandboxIsolationMaterializeOverlay).run(t, true, bwrapSandbox(), true)
	for _, probe := range parityProbes {
		if probe.overlayOnly {
			if overlay[probe.name] {
				t.Errorf("the overlay run may %s in plan mode", probe.name)
			}
			continue
		}
		if overlay[probe.name] != checkout[probe.name] {
			t.Errorf("%s in plan mode: checkout %v, overlay %v", probe.name, checkout[probe.name], overlay[probe.name])
		}
	}
	for _, name := range []string{"write a new file in the workspace", "overwrite a tracked file in the workspace", "write the session's folder"} {
		if checkout[name] || overlay[name] {
			t.Errorf("%s was allowed in plan mode", name)
		}
	}
}
