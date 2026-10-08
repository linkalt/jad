package claudeauth

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ErrFileExists is returned by Login when a credential file exists and Force is not set.
var ErrFileExists = errors.New("a credential file already exists; pass --force to replace it")

// LoginOptions control one sign-in.
type LoginOptions struct {
	// Force replaces an existing credential file.
	Force bool
	// CopyCode signs in without a local callback: the issuer's page shows a code that the user
	// pastes into In. Use it when the browser is on another machine.
	CopyCode bool
	// In is where the pasted code is read from in copy-code mode.
	In io.Reader
	// Out receives the authorization URL and progress lines; nil discards them.
	Out io.Writer
	// OpenBrowser opens the authorization URL; nil only prints it.
	OpenBrowser func(url string) error
	// Random is the source of the state and the verifier; nil uses crypto/rand.
	Random io.Reader
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// Login runs the authorization-code flow with PKCE and stores the tokens. By default it listens on
// the loopback callback address and waits for the redirect; with CopyCode it reads the code the
// user pastes. Tokens already in the file are replaced, unknown keys are kept.
func Login(ctx context.Context, store *Store, client *Client, options LoginOptions) error {
	config := client.Config
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	out := options.Out
	if out == nil {
		out = io.Discard
	}
	// Refuse before the browser opens, so a sign-in is not wasted. Update checks again under the
	// lock for the case that the file appears while the user is signing in.
	if !options.Force {
		if _, err := store.Read(); err == nil {
			return fmt.Errorf("%s: %w", store.Path, ErrFileExists)
		} else if !errors.Is(err, ErrNoFile) {
			return err
		}
	}

	var (
		callback    *callbackServer
		redirectURI = config.CopyCodeURI
	)
	if !options.CopyCode {
		var err error
		if callback, err = listenForCallback(config); err != nil {
			return err
		}
		defer callback.close()
		redirectURI = callback.redirectURI
	}
	request, err := NewAuthRequest(config, redirectURI, options.Random)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Open this URL to sign in:\n%s\n", request.URL)
	if options.OpenBrowser != nil {
		if err := options.OpenBrowser(request.URL); err != nil {
			fmt.Fprintf(out, "Could not open a browser (%v); open the URL above yourself.\n", err)
		}
	}

	waitContext, cancel := context.WithTimeout(ctx, time.Duration(config.LoginTimeoutMS)*time.Millisecond)
	defer cancel()
	var code, state string
	if options.CopyCode {
		fmt.Fprintln(out, "After you sign in, copy the code the page shows and paste it here:")
		code, state, err = readPasted(waitContext, options.In)
	} else {
		callback.expectState(request.State)
		fmt.Fprintf(out, "Waiting for the sign-in on %s ...\n", callback.redirectURI)
		code, err = callback.wait(waitContext)
		state = request.State
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("sign-in timed out after %d ms", config.LoginTimeoutMS)
		}
		return err
	}
	if state == "" {
		state = request.State
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(request.State)) != 1 {
		return errors.New("the pasted code belongs to a different sign-in (state mismatch)")
	}
	exchangeContext, cancelExchange := context.WithTimeout(ctx, config.HTTPTimeout())
	defer cancelExchange()
	tokens, err := client.Exchange(exchangeContext, code, state, request.Verifier, request.RedirectURI)
	if err != nil {
		return fmt.Errorf("exchange the authorization code: %w", err)
	}
	return store.Update(ctx, func(current *File) (*File, error) {
		if current != nil && !options.Force {
			return nil, fmt.Errorf("%s: %w", store.Path, ErrFileExists)
		}
		// A new sign-in may be another account, so no old token carries over; other keys do.
		var base *File
		if current != nil {
			base = &File{top: current.top}
		}
		return base.Apply(tokens, now())
	})
}

// readPasted reads one line from in and parses it. The read runs in a goroutine so the timeout
// still applies; the goroutine ends with the process or when the line arrives.
func readPasted(ctx context.Context, in io.Reader) (code, state string, err error) {
	if in == nil {
		return "", "", errors.New("no input to read the pasted code from")
	}
	type line struct {
		text string
		err  error
	}
	result := make(chan line, 1)
	go func() {
		text, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && text == "" {
			result <- line{err: errors.New("no code was pasted")}
			return
		}
		result <- line{text: text}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			return "", "", got.err
		}
		code, state = ParsePasted(got.text)
		if code == "" {
			return "", "", errors.New("the pasted text holds no authorization code")
		}
		return code, state, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// callbackServer receives the single redirect of the sign-in.
type callbackServer struct {
	redirectURI string
	servers     []*http.Server

	mutex    sync.Mutex
	state    string
	finished bool
	result   chan callbackResult
}

type callbackResult struct {
	code string
	err  error
}

func (c *callbackServer) expectState(state string) {
	c.mutex.Lock()
	c.state = state
	c.mutex.Unlock()
}

// listenForCallback listens on the loopback addresses. The browser resolves the redirect host to
// IPv4 or IPv6, so both are tried; one is enough.
func listenForCallback(config *Config) (*callbackServer, error) {
	callback := &callbackServer{result: make(chan callbackResult, 1)}
	var listeners []net.Listener
	port := config.LoginPort
	for _, host := range []string{"127.0.0.1", "::1"} {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			if len(listeners) == 0 && host == "127.0.0.1" {
				return nil, fmt.Errorf("listen for the sign-in callback on port %d: %w (is another sign-in, such as Claude Code, running?)", port, unwrapOp(err))
			}
			continue
		}
		if port == 0 {
			port = listener.Addr().(*net.TCPAddr).Port
		}
		listeners = append(listeners, listener)
	}
	callback.redirectURI = "http://" + net.JoinHostPort(config.RedirectHost, strconv.Itoa(port)) + config.CallbackPath
	mux := http.NewServeMux()
	mux.HandleFunc(config.CallbackPath, callback.handle)
	for _, listener := range listeners {
		server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		callback.servers = append(callback.servers, server)
		go func() { _ = server.Serve(listener) }()
	}
	return callback, nil
}

func unwrapOp(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Err
	}
	return err
}

// handle accepts the first valid redirect. The pages never echo request values.
func (c *callbackServer) handle(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.finished {
		writePage(w, http.StatusConflict, "The sign-in already finished. You can close this window.")
		return
	}
	if provided := query.Get("error"); provided != "" {
		c.finished = true
		c.result <- callbackResult{err: fmt.Errorf("the sign-in was refused by the issuer: %s", truncate(provided, maxDescription))}
		writePage(w, http.StatusBadRequest, "The sign-in failed. You can close this window and return to the terminal.")
		return
	}
	code := query.Get("code")
	if code == "" || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(c.state)) != 1 || c.state == "" {
		// A request with a wrong state does not end the sign-in: it may be a stray request.
		writePage(w, http.StatusBadRequest, "This sign-in request is not valid.")
		return
	}
	c.finished = true
	c.result <- callbackResult{code: code}
	writePage(w, http.StatusOK, "Signed in. You can close this window and return to the terminal.")
}

func writePage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}

func (c *callbackServer) wait(ctx context.Context) (string, error) {
	select {
	case result := <-c.result:
		return result.code, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *callbackServer) close() {
	for _, server := range c.servers {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.Shutdown(shutdown)
		cancel()
	}
}
