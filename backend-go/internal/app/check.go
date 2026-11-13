package app

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
	"jad/backend-go/internal/tools/native"
)

// CheckFiles reads every catalog and session file of the roots as the server would load it
// and writes one line per problem and a summary to w; it also starts each distinct enabled
// sandbox once. It reports whether all files are valid and every sandbox started.
func CheckFiles(roots []sessionfs.Root, w io.Writer) (bool, error) {
	report, err := sessionfs.CheckLayout(roots, mcpclient.ValidateServerFile)
	if err != nil {
		return false, err
	}
	// Files that load can still name a sandbox that does not start here; each distinct one is
	// tried once and reported against the first session that uses it.
	for _, sandbox := range report.Sandboxes {
		if err := native.ProbeSandbox(context.Background(), sandbox.Settings, sandbox.WorkingDir); err != nil {
			report.Problems = append(report.Problems, sessionfs.CheckProblem{Kind: "session", ID: sandbox.SessionID, Message: "sandbox: " + err.Error()})
		}
	}
	report.Problems = append(report.Problems, shareProblems(report.Shares, checkOverlay)...)
	for _, note := range report.Notes {
		fmt.Fprintf(w, "note: %s\n", note)
	}
	for _, note := range hostNotes(report.Sandboxes, checkOverlay, checkPlanFallback) {
		fmt.Fprintf(w, "note: %s\n", note)
	}
	for _, problem := range report.Problems {
		if problem.ID == "" {
			fmt.Fprintf(w, "%s: %s\n", problem.Kind, problem.Message)
			continue
		}
		fmt.Fprintf(w, "%s %s: %s\n", problem.Kind, problem.ID, problem.Message)
	}
	fmt.Fprintf(w, "checked %d files, %d problems\n", report.Checked, len(report.Problems))
	return len(report.Problems) == 0, nil
}

// shareProblems are the isolation settings with share_paths that jad would refuse to run here:
// share_paths need the overlay materialiser, a run never drops them, and materialize auto
// does not fall back to a checkout for them, so where the overlay trial fails (with the bwrap
// that run's sandbox names) every run of that setting fails. The trial is the one the run makes.
func shareProblems(shares []sessionfs.CheckedShares, overlay func(context.Context, string, time.Duration) error) []sessionfs.CheckProblem {
	var timeout time.Duration
	if fallback := sessionfs.PlanFallbackSandbox(nil); fallback != nil {
		timeout = time.Duration(fallback.CheckTimeoutMs) * time.Millisecond
	}
	failed := map[string]error{}
	var problems []sessionfs.CheckProblem
	for _, share := range shares {
		executable := native.HolderExecutable(share.Sandbox)
		err, tried := failed[executable]
		if !tried {
			err = overlay(context.Background(), executable, timeout)
			failed[executable] = err
		}
		if err != nil {
			problems = append(problems, sessionfs.CheckProblem{Kind: "session", ID: share.Where, Message: fmt.Sprintf("isolation.share_paths %q need the overlay materialiser, which does not work with %s here, so every such run would be refused (never run without the folders): %v", share.Paths, executable, err)})
		}
	}
	return problems
}

// The trial starts of what this host offers beyond the configured sandboxes; tests replace
// them.
var (
	checkOverlay      = native.ProbeOverlay
	checkPlanFallback = native.ProbePlanFallback
)

// hostNotes are what jad check says about this host without counting it as a problem, since
// jad falls back: the overlay materialiser of isolated runs, tried with the default bwrap and
// each configured one (materialize auto then checks out, D1, D14), and the sandbox plan-mode
// and read_only exec calls fall back to (P5). Nothing is said when both work.
func hostNotes(sandboxes []sessionfs.CheckedSandbox, overlay func(context.Context, string, time.Duration) error, planFallback func(context.Context) error) []string {
	ctx := context.Background()
	var timeout time.Duration
	if fallback := sessionfs.PlanFallbackSandbox(nil); fallback != nil {
		timeout = time.Duration(fallback.CheckTimeoutMs) * time.Millisecond
	}
	executables := []string{sessionfs.DefaultSandboxExecutable}
	for _, sandbox := range sandboxes {
		if settings := sandbox.Settings; settings != nil && settings.Backend == sessionfs.SandboxBackendBwrap && !slices.Contains(executables, settings.Executable) {
			executables = append(executables, settings.Executable)
		}
	}
	var notes []string
	for _, executable := range executables {
		if err := overlay(ctx, executable, timeout); err != nil {
			notes = append(notes, fmt.Sprintf("isolation: overlay runs do not work with %s here, so isolated runs check their files out (materialize auto) and materialize overlay, or auto with share_paths, fails the run: %v", executable, err))
		}
	}
	if err := planFallback(ctx); err != nil {
		notes = append(notes, "plan mode: the fallback sandbox does not start, so exec read_only does not count and plan mode refuses exec code where no sandbox covers exec: "+err.Error())
	}
	return notes
}

// CheckSelfBindings warns about each remote binding of the workspace layout that reaches this
// server itself (its remote API's unix socket, or its own host and port). A call through such a
// binding arrives as a remote message: the deny rules of workspace.json and global.json without
// a from apply to it, the caller-side rules with a from or a local: target do not, and the hop
// limit is what ends a loop. It never fails the check.
func CheckSelfBindings(roots []sessionfs.Root, server sessionfs.ServerSettings, w io.Writer) {
	if !server.RemoteAPI.Enabled {
		return
	}
	for _, root := range roots {
		if root.Layout == nil || root.Layout.Workspace == "" {
			continue
		}
		entries, _ := remote.ListBindings(root.Layout.Remotes)
		for _, entry := range entries {
			if entry.PointsAt(server.RemoteAPI.ListenUnix, server.Host, server.Port) {
				fmt.Fprintf(w, "warning: remote binding %s points at this server itself: a message through it is a remote message, which skips the caller-side local: deny rules and workspace allow_reset (a deny rule without from still applies), and a loop is stopped only by the hop limit\n", entry.Name)
			}
		}
		return // one remotes folder, shared by every workspace
	}
}
