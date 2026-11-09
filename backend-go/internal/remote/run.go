package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"jad/backend-go/internal/asyncmgr"
)

const (
	apiPrefix = "/api/remote/v1/"
	// minPollGap stops a remote that ignores wait_ms from being hammered.
	minPollGap  = MinPollMs * time.Millisecond
	maxRetryGap = 2 * time.Second
)

var runIDPattern = regexp.MustCompile(`^rr_[0-9a-f]{16}$`)

// ErrLost is a remote run that can no longer be followed: the remote was unreachable for
// lost_after_ms, or it no longer knows the run. The run may still be going over there.
var ErrLost = errors.New("remote run was lost")

// View is the remote's report of one run.
type View struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Kind      string `json:"kind"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Text      string `json:"text"`
	Cut       bool   `json:"cut"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Reason    string `json:"reason"`
	// ResumedFrom and ConfigNote are set for a run that resumed another, and only then.
	ResumedFrom string `json:"resumed_from,omitempty"`
	ConfigNote  string `json:"config_note,omitempty"`
	// Worktree are the worktree_* lines of a run that worked in a git worktree of its own on
	// the remote's repository; empty for another run, or from a remote that does not send them.
	Worktree []WorktreeLine `json:"worktree,omitempty"`
}

// WorktreeLine is one worktree_* line of a remote run's view.
type WorktreeLine struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// worktreeKeyPattern is the form of a worktree line's key; a line with another key is dropped,
// so a remote cannot add lines of another kind to the caller's result.
var worktreeKeyPattern = regexp.MustCompile(`^worktree_[a-z_]{1,32}$`)

const (
	// worktreeLinesMax and worktreeValueMaxBytes bound what a remote's worktree lines add to
	// the caller's result.
	worktreeLinesMax      = 16
	worktreeValueMaxBytes = 2048
)

// clean bounds the remote's short texts and strips control characters: they reach the
// model's context and the async records, so a remote must not be able to forge lines there.
func (v *View) clean() {
	v.State = safeText(v.State, 32)
	v.Kind = safeText(v.Kind, 32)
	v.Code = safeText(v.Code, 64)
	v.Message = safeText(v.Message, 1024)
	v.Reason = safeText(v.Reason, 128)
	v.ResumedFrom = safeText(v.ResumedFrom, 64)
	v.ConfigNote = safeText(v.ConfigNote, asyncmgr.ConfigNoteMaxBytes)
	var lines []WorktreeLine
	for _, line := range v.Worktree {
		if len(lines) == worktreeLinesMax {
			break
		}
		if !worktreeKeyPattern.MatchString(line.Key) {
			continue
		}
		lines = append(lines, WorktreeLine{Key: line.Key, Value: safeText(line.Value, worktreeValueMaxBytes)})
	}
	v.Worktree = lines
}

// Terminal reports whether the run has ended.
func (v View) Terminal() bool {
	switch v.State {
	case "done", "failed", "stopped", "timed_out":
		return true
	}
	return false
}

func newRequestID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create request id: %w", err)
	}
	return "r" + hex.EncodeToString(random), nil
}

// StartRun asks the remote to start a message ("messages") or a sub-agent ("subagents"). The
// request id is made here and sent with every retry, so a lost answer never starts the work
// twice. A failure that is not a *Error means the remote may or may not have started it.
func (c *Client) StartRun(ctx context.Context, kind string, body map[string]any, hops int) (View, error) {
	requestID, err := newRequestID()
	if err != nil {
		return View{}, err
	}
	payload := make(map[string]any, len(body)+1)
	for key, value := range body {
		payload[key] = value
	}
	payload["request_id"] = requestID
	var view View
	if err := c.Do(ctx, Request{Method: http.MethodPost, Path: apiPrefix + kind + c.textMaxQuery(), Body: payload, Hops: hops, Retry: true}, &view); err != nil {
		return View{}, err
	}
	if !runIDPattern.MatchString(view.ID) {
		return View{}, fmt.Errorf("remote %s: the answer does not name a run", c.entry.Name)
	}
	view.clean()
	return view, nil
}

// PollRun reads a run, letting the remote hold the request open for up to wait.
func (c *Client) PollRun(ctx context.Context, id string, wait time.Duration) (View, error) {
	if !runIDPattern.MatchString(id) {
		return View{}, fmt.Errorf("remote %s: invalid run id", c.entry.Name)
	}
	path := apiPrefix + "runs/" + id + c.textMaxQuery()
	if wait > 0 {
		path += "&wait_ms=" + strconv.FormatInt(wait.Milliseconds(), 10)
	}
	var view View
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: path, Wait: wait}, &view); err != nil {
		return View{}, err
	}
	view.clean()
	return view, nil
}

const (
	// viewEnvelopeBytes is what a run view may hold besides its text (messages, worktree lines),
	// capped at half the response limit; jsonGrowth is how many bytes JSON may make of one byte
	// of text (\u003c for "<").
	viewEnvelopeBytes = 64 << 10
	jsonGrowth        = 6
)

// textMaxQuery is the query that asks the remote for no more answer text in a view than fits
// this entry's max_response_bytes whatever the text is, so a long answer never makes the poll
// that carries it too large. The rest of the answer is read with FetchResult.
func (c *Client) textMaxQuery() string {
	limit := c.entry.MaxResponseBytes
	return "?text_max_bytes=" + strconv.FormatInt(max(0, (limit-min(viewEnvelopeBytes, limit/2))/jsonGrowth), 10)
}

// FetchResult writes the whole text of an ended run to dst, reading it from the remote in parts
// of at most max_response_bytes, and returns its size. It is how an answer longer than a view
// can hold is read; the view's cut says when to. The parts are checked against the size the
// remote reports, so a text that changed or ended early is an error, never a short copy that
// looks whole. The parts are retried; what was written to dst before a failure is not undone.
func (c *Client) FetchResult(ctx context.Context, id string, dst io.Writer) (int64, error) {
	if !runIDPattern.MatchString(id) {
		return 0, fmt.Errorf("remote %s: invalid run id", c.entry.Name)
	}
	var offset, total int64
	for first := true; ; first = false {
		var part []byte
		var header http.Header
		path := apiPrefix + "runs/" + id + "/result?offset=" + strconv.FormatInt(offset, 10) + "&limit=" + strconv.FormatInt(c.entry.MaxResponseBytes, 10)
		if err := c.Do(ctx, Request{Method: http.MethodGet, Path: path, Retry: true, Raw: &part, Header: &header}, nil); err != nil {
			return offset, err
		}
		size, err := strconv.ParseInt(header.Get("X-Result-Bytes"), 10, 64)
		if err != nil || size < 0 {
			return offset, fmt.Errorf("remote %s: the result answer does not give the size of the result", c.entry.Name)
		}
		if !first && size != total {
			return offset, fmt.Errorf("remote %s: the result changed while it was read", c.entry.Name)
		}
		total = size
		if offset+int64(len(part)) > total || (len(part) == 0 && offset < total) {
			return offset, fmt.Errorf("remote %s: the result answer does not match the size of the result", c.entry.Name)
		}
		if _, err := dst.Write(part); err != nil {
			return offset, fmt.Errorf("remote %s: save the result: %w", c.entry.Name, err)
		}
		offset += int64(len(part))
		if offset == total {
			return total, nil
		}
	}
}

// StopRun asks the remote to stop a run; stopping is idempotent there, so it is retried.
func (c *Client) StopRun(ctx context.Context, id string) (View, error) {
	if !runIDPattern.MatchString(id) {
		return View{}, fmt.Errorf("remote %s: invalid run id", c.entry.Name)
	}
	var view View
	if err := c.Do(ctx, Request{Method: http.MethodPost, Path: apiPrefix + "runs/" + id + "/stop" + c.textMaxQuery(), Retry: true, Control: true}, &view); err != nil {
		return View{}, err
	}
	view.clean()
	return view, nil
}

// Follow polls a run until it ends. Transient failures are retried until the remote has been
// unreachable for lost_after_ms (then ErrLost); an answer that says the run is unknown is
// ErrLost at once; a permanent refusal (token, scope) is returned as it is.
func (c *Client) Follow(ctx context.Context, id string) (View, error) {
	lastGood := time.Now()
	gap := 100 * time.Millisecond
	for {
		began := time.Now()
		view, err := c.PollRun(ctx, id, time.Duration(c.entry.PollMs)*time.Millisecond)
		if err == nil {
			lastGood, gap = time.Now(), 100*time.Millisecond
			if view.Terminal() {
				return view, nil
			}
			if spent := time.Since(began); spent < minPollGap {
				if !sleep(ctx, minPollGap-spent) {
					return View{}, ctx.Err()
				}
			}
			continue
		}
		if ctx.Err() != nil {
			return View{}, ctx.Err()
		}
		var failure *Error
		if errors.As(err, &failure) {
			if failure.Status == http.StatusNotFound && failure.Code == "no_run" {
				return View{}, fmt.Errorf("remote %s: %w (the remote does not know the run)", c.entry.Name, ErrLost)
			}
			if failure.Status < 500 {
				return View{}, err
			}
		} else if errors.Is(err, ErrRedirect) || errors.Is(err, ErrResponseTooLarge) {
			return View{}, err
		}
		if time.Since(lastGood) > time.Duration(c.entry.LostAfterMs)*time.Millisecond {
			return View{}, fmt.Errorf("remote %s: %w (unreachable for %d ms)", c.entry.Name, ErrLost, c.entry.LostAfterMs)
		}
		if !sleep(ctx, gap) {
			return View{}, ctx.Err()
		}
		gap = min(gap*2, maxRetryGap)
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// ProfileInfo asks the remote for a profile's info (its context tokens) and returns the rendered
// text. It is the only profile action a remote takes: compaction and reset are not sent, so a
// remote can never change a profile. The read is retried.
func (c *Client) ProfileInfo(ctx context.Context, session, profile string, hops int) (string, error) {
	var answer struct {
		Output string `json:"output"`
	}
	query := url.Values{"session": {session}, "profile": {profile}}
	path := apiPrefix + "profile?" + query.Encode()
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: path, Hops: hops, Retry: true}, &answer); err != nil {
		return "", err
	}
	return answer.Output, nil
}

// ReadTranscript asks the remote to run its transcript or transcripts tool (tool is one of the
// two) with query as the tool's input, and returns the rendered result. Reads are retried.
func (c *Client) ReadTranscript(ctx context.Context, tool string, query url.Values, hops int) (string, error) {
	if tool != "transcript" && tool != "transcripts" {
		return "", fmt.Errorf("remote %s: %q is not a readable tool", c.entry.Name, tool)
	}
	var answer struct {
		Output string `json:"output"`
	}
	path := apiPrefix + tool
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: path, Hops: hops, Retry: true}, &answer); err != nil {
		return "", err
	}
	return answer.Output, nil
}
