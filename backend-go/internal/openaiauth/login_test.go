package openaiauth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// loginClient is a client for the fake issuer with a free callback port and a short wait.
func loginClient(t *testing.T, issuer *fakeIssuer, edit func(*Config)) *Client {
	t.Helper()
	client := clientFor(t, issuer)
	client.Config.LoginPort = 0
	client.Config.LoginTimeoutMS = 3000
	if edit != nil {
		edit(client.Config)
	}
	return client
}

// browser plays the user: it reads the authorization URL and calls the redirect URI with the
// parameters chosen by visit. It records the status of the callback answer.
type browser struct {
	t      *testing.T
	mutex  sync.Mutex
	opened *url.URL
	status []int
	visit  func(redirect, state string) []string // URLs to call, in order
	done   chan struct{}                         // closed when every visit has been answered
}

// finished waits for the visits to end and returns the callback answers. Login can return
// before the last answer is recorded, so a test must not read status without this.
func (b *browser) finished() []int {
	select {
	case <-b.done:
	case <-time.After(5 * time.Second):
		b.t.Fatal("the browser visits did not finish")
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return append([]int(nil), b.status...)
}

func (b *browser) openedURL() *url.URL {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.opened
}

func newBrowser(t *testing.T, visit func(redirect, state string) []string) *browser {
	return &browser{t: t, visit: visit, done: make(chan struct{})}
}

func (b *browser) open(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	b.mutex.Lock()
	b.opened = parsed
	b.mutex.Unlock()
	redirect, state := parsed.Query().Get("redirect_uri"), parsed.Query().Get("state")
	go func() {
		defer close(b.done)
		for _, target := range b.visit(redirect, state) {
			// A request that cannot connect is recorded as status 0, not as a test failure: Login
			// closes the callback server after the first redirect, so a later request can
			// legitimately find the port closed. A test that needs an answer checks the status.
			code := 0
			if response, err := http.Get(target); err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				code = response.StatusCode
			}
			b.mutex.Lock()
			b.status = append(b.status, code)
			b.mutex.Unlock()
		}
	}()
	return nil
}

func goodVisit(code string) func(redirect, state string) []string {
	return func(redirect, state string) []string {
		return []string{redirect + "?code=" + code + "&state=" + url.QueryEscape(state)}
	}
}

func TestLoginStoresTheTokens(t *testing.T) {
	store := storeIn(t)
	access := accessJWT(t, fixedNow, 10*24*time.Hour, "acct-login")
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "id-1", "refresh-1")})
	person := newBrowser(t, goodVisit("auth-code-1"))
	var out bytes.Buffer

	err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{Out: &out, OpenBrowser: person.open, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	opened := person.openedURL()
	// The exchange used the code from the redirect and the verifier whose challenge was in the URL.
	form := issuer.Calls[0].Form
	if form["code"][0] != "auth-code-1" || form["grant_type"][0] != "authorization_code" ||
		form["redirect_uri"][0] != opened.Query().Get("redirect_uri") ||
		codeChallenge(form["code_verifier"][0]) != opened.Query().Get("code_challenge") {
		t.Fatalf("exchange does not match the authorization request: %v", form)
	}
	file := readJSON(t, store.Path)
	tokens := tokensOf(file)
	if file["auth_mode"] != "chatgpt" || tokens["access_token"] != access || tokens["refresh_token"] != "refresh-1" ||
		tokens["id_token"] != "id-1" || tokens["account_id"] != "acct-login" || file["last_refresh"] != "2026-10-08T12:00:00.000Z" {
		t.Fatalf("stored file wrong: top keys %v token keys %v", keysOf(file), keysOf(tokens))
	}
	if info, _ := os.Stat(store.Path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(out.String(), opened.String()) {
		t.Fatal("the authorization URL must be printed so it works without a browser")
	}
	for _, secret := range []string{access, "refresh-1", "id-1", "auth-code-1"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("the progress output contains secret text: %q", secret)
		}
	}
	if answers := person.finished(); len(answers) != 1 || answers[0] != 200 {
		t.Fatalf("callback answers %v, want [200]", answers)
	}
}

func TestLoginRefusesToReplaceWithoutForce(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldTokens(t), nil)
	before, _ := os.ReadFile(store.Path)
	issuer := newFakeIssuer(t)
	opened := false
	err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{OpenBrowser: func(string) error { opened = true; return nil }})
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("want ErrFileExists, got %v", err)
	}
	if opened || issuer.callCount() != 0 {
		t.Fatal("a refused login must not open the browser or contact the issuer")
	}
	if after, _ := os.ReadFile(store.Path); string(after) != string(before) {
		t.Fatal("the file changed")
	}
}

func TestLoginForceReplacesTokensButKeepsOtherKeys(t *testing.T) {
	store := storeIn(t)
	tokens := oldTokens(t)
	tokens["future_key"] = "stale"
	writeFile(t, store, tokens, map[string]any{"OPENAI_API_KEY": "sk-keep"})
	access := accessJWT(t, fixedNow, time.Hour, "acct-other")
	// The response carries no refresh and no id token. A new sign-in may be another account, so
	// nothing from the old tokens may carry over.
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "", "")})
	person := newBrowser(t, goodVisit("c"))
	if err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{Force: true, OpenBrowser: person.open, Now: func() time.Time { return fixedNow }}); err != nil {
		t.Fatal(err)
	}
	file := readJSON(t, store.Path)
	got := tokensOf(file)
	if got["access_token"] != access || got["account_id"] != "acct-other" {
		t.Fatalf("new tokens not stored: %v", keysOf(got))
	}
	for _, stale := range []string{"refresh_token", "id_token", "future_key"} {
		if _, ok := got[stale]; ok {
			t.Errorf("old tokens.%s carried over into a new sign-in", stale)
		}
	}
	if file["OPENAI_API_KEY"] != "sk-keep" {
		t.Fatal("unknown top-level keys must be kept")
	}
	backup := readJSON(t, store.BackupPath())
	if tokensOf(backup)["refresh_token"] != "old-refresh" {
		t.Fatal("the backup must hold the replaced login")
	}
}

func TestLoginRejectsAWrongStateAndKeepsWaiting(t *testing.T) {
	store := storeIn(t)
	access := accessJWT(t, fixedNow, time.Hour, "a")
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "", "r")})
	person := newBrowser(t, func(redirect, state string) []string {
		return []string{
			redirect + "?code=evil&state=wrong",
			redirect + "?code=evil",
			redirect + "?state=" + url.QueryEscape(state), // no code
			redirect + "?code=good&state=" + url.QueryEscape(state),
		}
	})
	if err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{OpenBrowser: person.open}); err != nil {
		t.Fatal(err)
	}
	if got := issuer.Calls[0].Form["code"][0]; got != "good" {
		t.Fatalf("the exchange used code %q; only a request with the right state may be accepted", got)
	}
	if answers := person.finished(); len(answers) != 4 || answers[0] != 400 || answers[1] != 400 || answers[2] != 400 || answers[3] != 200 {
		t.Fatalf("callback answers %v, want [400 400 400 200]", answers)
	}
}

func TestLoginSecondRedirectIsRefusedAfterTheFirst(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, accessJWT(t, fixedNow, time.Hour, "a"), "", "r")})
	person := newBrowser(t, func(redirect, state string) []string {
		first := redirect + "?code=one&state=" + url.QueryEscape(state)
		return []string{first, first}
	})
	if err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{OpenBrowser: person.open}); err != nil {
		t.Fatal(err)
	}
	if issuer.callCount() != 1 {
		t.Fatalf("%d token requests; one redirect must give one exchange", issuer.callCount())
	}
	// Login closes the callback server once the first redirect is handled, so the second request
	// may be answered 409 or may find the port closed. It must never be answered 200.
	for _, status := range person.finished()[1:] {
		if status == 200 {
			t.Fatal("a second redirect was accepted")
		}
	}
}

func TestLoginIssuerErrorPage(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t)
	person := newBrowser(t, func(redirect, state string) []string {
		return []string{redirect + "?error=access_denied&state=" + url.QueryEscape(state)}
	})
	err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{OpenBrowser: person.open})
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("want the issuer's error, got %v", err)
	}
	if issuer.callCount() != 0 {
		t.Fatal("no exchange after a refused sign-in")
	}
	if _, err := os.Stat(store.Path); err == nil {
		t.Fatal("a failed sign-in must not create a file")
	}
}

func TestLoginTimeoutAndCancel(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t)
	client := loginClient(t, issuer, func(c *Config) { c.LoginTimeoutMS = 150 })
	start := time.Now()
	err := Login(context.Background(), store, client, LoginOptions{})
	if err == nil || !strings.Contains(err.Error(), "timed out after 150 ms") || time.Since(start) > 3*time.Second {
		t.Fatalf("want a timeout error, got %v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	client = loginClient(t, issuer, func(c *Config) { c.LoginTimeoutMS = 60_000 })
	if err := Login(ctx, store, client, LoginOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if _, err := os.Stat(store.Path); err == nil {
		t.Fatal("no file may be written")
	}
}

func TestLoginExchangeFailureWritesNothing(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t, reply{400, `{"error":"invalid_grant","error_description":"code expired"}`})
	person := newBrowser(t, goodVisit("c"))
	err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{OpenBrowser: person.open})
	var tokenErr *TokenError
	if !errors.As(err, &tokenErr) || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("want the token error, got %v", err)
	}
	if _, err := os.Stat(store.Path); err == nil {
		t.Fatal("no file may be written after a failed exchange")
	}
}

// The file can appear while the user signs in (another login finished first).
func TestLoginRechecksUnderTheLock(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, accessJWT(t, fixedNow, time.Hour, "a"), "", "r")})
	person := newBrowser(t, goodVisit("c"))
	open := func(raw string) error {
		writeFile(t, store, oldTokens(t), nil) // the other login lands first
		return person.open(raw)
	}
	err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{OpenBrowser: open})
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("want ErrFileExists, got %v", err)
	}
	if tokensOf(readJSON(t, store.Path))["refresh_token"] != "old-refresh" {
		t.Fatal("the file written by the other login was overwritten")
	}
}

func TestLoginPortInUseAndBrowserFailure(t *testing.T) {
	issuer := newFakeIssuer(t)
	holder, err := (&portHolder{}).listen()
	if err != nil {
		t.Skip("cannot listen on loopback:", err)
	}
	defer holder.Close()
	client := loginClient(t, issuer, func(c *Config) { c.LoginPort = holder.port() })
	if err := Login(context.Background(), storeIn(t), client, LoginOptions{}); err == nil || !strings.Contains(err.Error(), "listen for the sign-in callback") {
		t.Fatalf("want a port-in-use error, got %v", err)
	}
	// A browser that cannot open is not fatal: the URL was printed.
	var out bytes.Buffer
	client = loginClient(t, issuer, func(c *Config) { c.LoginTimeoutMS = 100 })
	err = Login(context.Background(), storeIn(t), client, LoginOptions{Out: &out, OpenBrowser: func(string) error { return errors.New("no display") }})
	if err == nil || !strings.Contains(out.String(), "no display") || !strings.Contains(out.String(), "/oauth/authorize?") {
		t.Fatalf("browser failure handling wrong: err=%v out=%q", err, out.String())
	}
}

func TestLoginIgnoresAStrayErrorWithoutStateAndKeepsWaiting(t *testing.T) {
	store := storeIn(t)
	access := accessJWT(t, fixedNow, time.Hour, "a")
	issuer := newFakeIssuer(t, reply{200, tokenBody(t, access, "", "r")})
	person := newBrowser(t, func(redirect, state string) []string {
		return []string{
			redirect + "?error=access_denied",
			redirect + "?error=access_denied&state=wrong",
			redirect + "?code=good&state=" + url.QueryEscape(state),
		}
	})
	if err := Login(context.Background(), store, loginClient(t, issuer, nil), LoginOptions{OpenBrowser: person.open}); err != nil {
		t.Fatalf("a stray ?error= ended the sign-in: %v", err)
	}
	if answers := person.finished(); len(answers) != 3 || answers[0] != 400 || answers[1] != 400 || answers[2] != 200 {
		t.Fatalf("callback answers %v, want [400 400 200]", answers)
	}
}
