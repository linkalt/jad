package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
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
	if err := c.Do(ctx, Request{Method: http.MethodPost, Path: apiPrefix + kind, Body: payload, Hops: hops, Retry: true}, &view); err != nil {
		return View{}, err
	}
	if !runIDPattern.MatchString(view.ID) {
		return View{}, fmt.Errorf("remote %s: the answer does not name a run", c.entry.Name)
	}
	return view, nil
}

// PollRun reads a run, letting the remote hold the request open for up to wait.
func (c *Client) PollRun(ctx context.Context, id string, wait time.Duration) (View, error) {
	if !runIDPattern.MatchString(id) {
		return View{}, fmt.Errorf("remote %s: invalid run id", c.entry.Name)
	}
	path := apiPrefix + "runs/" + id
	if wait > 0 {
		path += "?wait_ms=" + strconv.FormatInt(wait.Milliseconds(), 10)
	}
	var view View
	if err := c.Do(ctx, Request{Method: http.MethodGet, Path: path, Wait: wait}, &view); err != nil {
		return View{}, err
	}
	return view, nil
}

// StopRun asks the remote to stop a run; stopping is idempotent there, so it is retried.
func (c *Client) StopRun(ctx context.Context, id string) (View, error) {
	if !runIDPattern.MatchString(id) {
		return View{}, fmt.Errorf("remote %s: invalid run id", c.entry.Name)
	}
	var view View
	if err := c.Do(ctx, Request{Method: http.MethodPost, Path: apiPrefix + "runs/" + id + "/stop", Retry: true}, &view); err != nil {
		return View{}, err
	}
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
