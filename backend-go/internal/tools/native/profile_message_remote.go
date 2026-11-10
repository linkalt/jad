package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/tools"
)

// stopCallTimeout bounds the best-effort stop sent to a remote when the local side gives up
// (timeout, stop, shutdown).
const stopCallTimeout = 3 * time.Second

// RemoteFailedError is a remote that refused or failed a message, with its client-safe code.
type RemoteFailedError struct {
	Code    string
	Message string
}

func (e *RemoteFailedError) Error() string { return "remote call failed: " + e.Code }

// ErrRemoteLost is the outcome of a remote message that can no longer be followed.
var ErrRemoteLost = remote.ErrLost

// remoteSettings is what the profile_message tool knows about remotes: the names the session
// allows and the folder they live in.
type remoteSettings struct {
	names     []string
	directory *remote.Directory
}

func (r remoteSettings) allowed(name string) bool { return slices.Contains(r.names, name) }

// executeRemote sends the message to remote:<name>/<session>/<category>/<profile> and follows the
// run the way a local message is followed: an answer within block_ms, else a handle.
func (p *profileMessageTool) executeRemote(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext, reference, message string) (string, error) {
	callID := execution.CallID
	if p.remotes.directory == nil || len(p.remotes.names) == 0 {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "remote_unavailable",
			"This session does not enable remote profiles (tools.native.profile_message.remotes)"), nil
	}
	name, _, _ := remote.Split(reference)
	if !p.remotes.allowed(name) {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "unknown_remote",
			fmt.Sprintf("Remote %s is not available to message", name),
			tools.Information{Key: "available", Value: strings.Join(p.remotes.names, ", ")}), nil
	}
	target, err := remote.ParseProfile(reference, p.remotes.directory.Entry)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusInvalidInput, "bad_remote_reference", err.Error()), nil
	}
	entry, err := p.remotes.directory.Entry(name)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "remote_unavailable", remoteProblem(name, err)), nil
	}
	if !entry.Allows(target.Scope()) {
		return tools.ExecutionError(callID, tools.StatusForbidden, "remote_not_allowed",
			fmt.Sprintf("Profile %s is not among the profiles this session may reach on remote %s", target.Scope(), name)), nil
	}
	owner, canHandle := handleOwner(execution)
	handled := p.host != nil && canHandle
	block := p.blockFor(arguments, execution)
	plan := &remotePlan{
		directory: p.remotes.directory,
		target:    target,
		message:   message,
		hops:      execution.RemoteHops + 1,
		timeout:   time.Duration(p.settings.TimeoutMs) * time.Millisecond,
	}
	if !handled {
		return p.runInline(ctx, callID, reference, plan)
	}
	return p.runHandle(ctx, callID, reference, message, owner, plan, block)
}

// remoteProblem words a remote entry that could not be used, without any secret: the loader's
// own errors name files and keys only.
func remoteProblem(name string, err error) string {
	if errors.Is(err, remote.ErrNotFound) {
		return fmt.Sprintf("Remote %s has no file in the remotes folder", name)
	}
	return fmt.Sprintf("Remote %s cannot be used: %v", name, err)
}

// remotePlan is a message to a remote that passed every local check and has not been sent.
type remotePlan struct {
	directory *remote.Directory
	target    remote.Target
	message   string
	hops      int
	timeout   time.Duration
}

// Start sends the message. It returns an error only when the remote refused it or could not be
// reached before an answer; a message sent and then lost is the run's outcome.
func (p *remotePlan) Start(ctx context.Context) (ProfileMessageRun, error) {
	run, err := startRemoteRun(ctx, p.directory, p.target, "messages", map[string]any{
		"session": p.target.Session, "profile": p.target.Profile, "message": p.message,
	}, p.hops, p.timeout, "profile")
	if err != nil {
		return nil, err
	}
	return run, nil
}

// startRemoteRun starts a run on the remote (kind "messages" or "subagents") and follows it on
// a goroutine of the hub until it ends.
func startRemoteRun(ctx context.Context, directory *remote.Directory, target remote.Target, kind string, body map[string]any, hops int, timeout time.Duration, what string) (*remoteRun, error) {
	client, err := directory.Client(target.Remote)
	if err != nil {
		return nil, &RemoteFailedError{Code: "remote_unavailable", Message: remoteProblem(target.Remote, err)}
	}
	return startRemoteRunOn(ctx, directory, client, target, kind, body, hops, timeout, what)
}

// startRemoteRunOn is startRemoteRun with the client already chosen.
func startRemoteRunOn(ctx context.Context, directory *remote.Directory, client *remote.Client, target remote.Target, kind string, body map[string]any, hops int, timeout time.Duration, what string) (*remoteRun, error) {
	view, err := client.StartRun(ctx, kind, body, hops)
	if err != nil {
		return nil, startError(target.Remote, err)
	}
	run := &remoteRun{client: client, target: target, what: what, id: view.ID, began: time.Now(), start: make(chan struct{}), done: make(chan struct{}), state: view.State}
	close(run.start)
	if !directory.Go(func(hubCtx context.Context) { run.follow(hubCtx, timeout) }) {
		stopCtx, cancel := context.WithTimeout(context.Background(), stopCallTimeout)
		defer cancel()
		_, _ = client.StopRun(stopCtx, view.ID)
		return nil, &RemoteFailedError{Code: "remote_unavailable", Message: "the server is shutting down; the remote run was asked to stop"}
	}
	return run, nil
}

// startError turns a failed start into the error the tool renders. A *remote.Error carries the
// remote's own code; anything else means the message may or may not have reached it.
func startError(name string, err error) error {
	var failure *remote.Error
	if errors.As(err, &failure) {
		switch {
		case failure.Code == "profile_busy" || failure.Status == http.StatusConflict && failure.Code == "":
			return &ProfileBusyError{}
		case failure.Code == "profile_cycle":
			return ErrProfileCycle
		}
		return &RemoteFailedError{Code: remoteCode(failure), Message: remoteMessage(name, failure)}
	}
	if errors.Is(err, remote.ErrRedirect) || errors.Is(err, remote.ErrResponseTooLarge) {
		return &RemoteFailedError{Code: "remote_failed", Message: err.Error()}
	}
	return &RemoteFailedError{Code: "remote_unreachable", Message: err.Error() + ". The message may not have been sent; check the remote before sending it again"}
}

func remoteCode(failure *remote.Error) string {
	switch failure.Code {
	case "remote_hop_limit", "forbidden", "unauthorized", "no_session", "unknown_target", "too_many_runs", "unavailable":
		return "remote_" + strings.TrimPrefix(failure.Code, "remote_")
	}
	if failure.Status == http.StatusUnauthorized {
		return "remote_unauthorized"
	}
	return "remote_failed"
}

func remoteMessage(name string, failure *remote.Error) string {
	switch failure.Code {
	case "remote_hop_limit":
		return fmt.Sprintf("Remote %s refused the message: it has passed through too many JAD servers", name)
	case "forbidden":
		return fmt.Sprintf("Remote %s refused the message: its token may not reach that profile", name)
	case "unauthorized":
		return fmt.Sprintf("Remote %s did not accept this server's token", name)
	case "no_session":
		return fmt.Sprintf("Remote %s has no such session", name)
	case "unknown_target":
		return fmt.Sprintf("Remote %s has no such profile in that session", name)
	}
	return failure.Error()
}

// remoteRun is a remote message being followed. It implements ProfileMessageRun, so the async
// tool's wait, status and stop act on it like on a local message.
type remoteRun struct {
	client *remote.Client
	target remote.Target
	// what is "profile" or "agent", for the status text.
	what  string
	id    string
	began time.Time
	start chan struct{}
	done  chan struct{}

	mu         sync.Mutex
	state      string
	stopReason string
	outcome    ProfileMessageOutcome
	stopOnce   sync.Once
}

func (r *remoteRun) Queued() bool             { return false }
func (r *remoteRun) Started() <-chan struct{} { return r.start }
func (r *remoteRun) Done() <-chan struct{}    { return r.done }

// Reference is the remote run as "<name>/rr_…", the form remote_run names it by. It is set when
// the run starts, so it is the same while the run goes and after it ends.
func (r *remoteRun) Reference() string { return r.target.Remote + "/" + r.id }

func (r *remoteRun) Outcome() ProfileMessageOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcome
}

func (r *remoteRun) Status() string {
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	return fmt.Sprintf("remote %s %s is %s: %ds used", r.what, r.target.String(), state, int64(time.Since(r.began).Seconds()))
}

// Stop asks the remote to stop the run; the follow loop then sees it end. It does not wait.
func (r *remoteRun) Stop(reason string) {
	r.mu.Lock()
	if r.stopReason == "" {
		r.stopReason = reason
	}
	r.mu.Unlock()
	r.stopOnce.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), stopCallTimeout)
			defer cancel()
			_, _ = r.client.StopRun(ctx, r.id)
		}()
	})
}

func (r *remoteRun) stopped() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopReason, r.stopReason != ""
}

// follow polls the remote until the run ends, the local timeout passes, the remote is lost or
// the process shuts down, then records the outcome once.
func (r *remoteRun) follow(hubCtx context.Context, timeout time.Duration) {
	ctx, cancel := hubCtx, context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(hubCtx, timeout)
	}
	defer cancel()
	view, err := r.client.Follow(ctx, r.id)
	outcome := r.conclude(hubCtx, ctx, view, err)
	r.mu.Lock()
	r.outcome = outcome
	r.state = "ended"
	r.mu.Unlock()
	close(r.done)
}

func (r *remoteRun) conclude(hubCtx, ctx context.Context, view remote.View, err error) ProfileMessageOutcome {
	trace := remote.Prefix + r.target.Remote + "/" + r.id
	reason, stopRequested := r.stopped()
	switch {
	case err == nil:
		outcome := ProfileMessageOutcome{Text: view.Text, Cut: view.Cut, Reason: view.Reason, Trace: trace, ResumedFrom: view.ResumedFrom, ConfigNote: view.ConfigNote,
			Worktree: remoteWorktree(r.target.Remote, view.Worktree)}
		if view.Cut {
			outcome.Full = &FullAnswer{Fetch: func(w io.Writer) (int64, error) { return r.client.FetchResult(hubCtx, r.id, w) }}
		}
		switch view.State {
		case "done":
			if strings.TrimSpace(view.Text) == "" {
				outcome.Err = ErrProfileNoAnswer
			}
		case "stopped":
			outcome.Err = ErrProfileStopped
			if outcome.Reason == "" {
				outcome.Reason = reason
			}
		case "timed_out":
			outcome.Err = ErrProfileTimeout
		default:
			outcome.Err = &ProfileFailedError{Message: safeRemoteText(view.Message)}
		}
		return outcome
	case hubCtx.Err() != nil:
		r.stopRemote()
		return ProfileMessageOutcome{Err: ErrProfileStopped, Reason: "server_shutdown", Trace: trace}
	case ctx.Err() != nil:
		r.stopRemote()
		return ProfileMessageOutcome{Err: ErrProfileTimeout, Trace: trace}
	case errors.Is(err, remote.ErrLost) && stopRequested:
		return ProfileMessageOutcome{Err: ErrProfileStopped, Reason: reason, Trace: trace}
	case errors.Is(err, remote.ErrLost):
		return ProfileMessageOutcome{Err: err, Trace: trace}
	}
	var failure *remote.Error
	if errors.As(err, &failure) {
		return ProfileMessageOutcome{Err: &RemoteFailedError{Code: remoteCode(failure), Message: remoteMessage(r.target.Remote, failure)}, Trace: trace}
	}
	return ProfileMessageOutcome{Err: &RemoteFailedError{Code: "remote_failed", Message: err.Error()}, Trace: trace}
}

// remoteWorktree is the worktree of a remote run as its view reports it, with a line saying
// that the branch is in the remote's repository; nil without worktree lines.
func remoteWorktree(remoteName string, lines []remote.WorktreeLine) *DelegatedWorktreeResult {
	if len(lines) == 0 {
		return nil
	}
	where := fmt.Sprintf("remote %s: the branch and the commands below are in that server's repository, not in this session's", remoteName)
	result := &DelegatedWorktreeResult{Information: []tools.Information{{Key: "worktree_remote", Value: where}}}
	parts := []string{"worktree on " + where}
	for _, line := range lines {
		result.Information = append(result.Information, tools.Information{Key: line.Key, Value: line.Value})
		parts = append(parts, line.Key+": "+line.Value)
	}
	result.Text = strings.Join(parts, "; ")
	return result
}

// stopRemote sends one best-effort stop, from the follow goroutine, so the hub waits for it.
func (r *remoteRun) stopRemote() {
	ctx, cancel := context.WithTimeout(context.Background(), stopCallTimeout)
	defer cancel()
	_, _ = r.client.StopRun(ctx, r.id)
}

func safeRemoteText(text string) string {
	cut, _ := cutUTF8(strings.Join(strings.Fields(text), " "), 400)
	return cut
}
