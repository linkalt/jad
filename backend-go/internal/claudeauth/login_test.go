package claudeauth

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

// browser plays the user: when the authorization URL is opened it visits the redirect URIs that
// visit returns.
type browser struct {
	t      *testing.T
	visit  func(redirect, state string) []string
	mutex  sync.Mutex
	opened *url.URL
	done   chan struct{}
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
	go func() {
		defer close(b.done)
		for _, target := range b.visit(parsed.Query().Get("redirect_uri"), parsed.Query().Get("state")) {
			response, err := http.Get(target)
			if err != nil {
				b.t.Errorf("callback request failed: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	return nil
}

func (b *browser) openedURL() *url.URL {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.opened
}

func goodVisit(code string) func(redirect, state string) []string {
	return func(redirect, state string) []string {
		return []string{redirect + "?code=" + code + "&state=" + state}
	}
}

func options(person *browser, out io.Writer) LoginOptions {
	return LoginOptions{Out: out, OpenBrowser: person.open, Now: func() time.Time { return fixedNow }}
}

func TestLoginCallbackFlow(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t, reply{200, tokenBody("acc-login", "ref-login", 28800)})
	person := newBrowser(t, goodVisit("auth-code-1"))
	var out bytes.Buffer
	if err := Login(context.Background(), store, clientFor(t, issuer), options(person, &out)); err != nil {
		t.Fatal(err)
	}
	opened := person.openedURL()
	call := issuer.Calls[0].JSON
	if call["code"] != "auth-code-1" || call["grant_type"] != "authorization_code" || call["state"] != opened.Query().Get("state") ||
		call["redirect_uri"] != opened.Query().Get("redirect_uri") ||
		codeChallenge(call["code_verifier"].(string)) != opened.Query().Get("code_challenge") {
		t.Fatalf("exchange does not match the authorization request: %v", keys(call))
	}
	if !strings.HasPrefix(opened.Query().Get("redirect_uri"), "http://localhost:") || !strings.HasSuffix(opened.Query().Get("redirect_uri"), "/callback") {
		t.Errorf("redirect_uri %s", opened.Query().Get("redirect_uri"))
	}
	file := readJSON(t, store.Path)
	if file["access_token"] != "acc-login" || file["refresh_token"] != "ref-login" || file["expires_at"] != "2026-10-08T20:00:00Z" {
		t.Fatalf("stored file wrong: %v", keys(file))
	}
	if strings.Contains(out.String(), "acc-login") || strings.Contains(out.String(), "auth-code-1") {
		t.Fatal("progress output shows a secret")
	}
}

func TestLoginIgnoresAWrongStateAndErrorRedirectEndsIt(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t, reply{200, tokenBody("acc", "ref", 60)})
	person := newBrowser(t, func(redirect, state string) []string {
		return []string{redirect + "?code=evil&state=wrong", redirect + "?code=good&state=" + state}
	})
	if err := Login(context.Background(), store, clientFor(t, issuer), options(person, nil)); err != nil {
		t.Fatal(err)
	}
	if issuer.Calls[0].JSON["code"] != "good" {
		t.Fatalf("a request with the wrong state was used: %v", issuer.Calls[0].JSON["code"])
	}

	store2 := storeIn(t)
	issuer2 := newFakeIssuer(t)
	refuse := newBrowser(t, func(redirect, state string) []string {
		return []string{redirect + "?error=access_denied&state=" + url.QueryEscape(state)}
	})
	err := Login(context.Background(), store2, clientFor(t, issuer2), options(refuse, nil))
	if err == nil || !strings.Contains(err.Error(), "access_denied") || issuer2.callCount() != 0 {
		t.Fatalf("err %v calls %d", err, issuer2.callCount())
	}
	if _, statErr := os.Stat(store2.Path); statErr == nil {
		t.Fatal("a refused sign-in wrote a file")
	}
}

func TestLoginRefusesToReplaceWithoutForce(t *testing.T) {
	store := storeIn(t)
	writeFile(t, store, oldFile(fixedNow.Add(time.Hour)))
	before, _ := os.ReadFile(store.Path)
	issuer := newFakeIssuer(t)
	person := newBrowser(t, goodVisit("c"))
	err := Login(context.Background(), store, clientFor(t, issuer), options(person, nil))
	if !errors.Is(err, ErrFileExists) || person.openedURL() != nil || issuer.callCount() != 0 {
		t.Fatalf("err %v opened %v", err, person.openedURL())
	}
	if after, _ := os.ReadFile(store.Path); string(after) != string(before) {
		t.Fatal("the file changed")
	}
}

func TestLoginForceReplacesTokensButKeepsUnknownKeys(t *testing.T) {
	store := storeIn(t)
	file := oldFile(fixedNow.Add(time.Hour))
	file["future_key"] = "keep"
	writeFile(t, store, file)
	// The issuer sends no refresh token and no scope: nothing of the old login may carry over.
	issuer := newFakeIssuer(t, reply{200, `{"access_token":"acc-other","expires_in":60}`})
	person := newBrowser(t, goodVisit("c"))
	opts := options(person, nil)
	opts.Force = true
	if err := Login(context.Background(), store, clientFor(t, issuer), opts); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, store.Path)
	if got["access_token"] != "acc-other" || got["future_key"] != "keep" {
		t.Fatalf("keys %v", keys(got))
	}
	for _, stale := range []string{"refresh_token", "scope"} {
		if _, ok := got[stale]; ok {
			t.Errorf("old %s carried over into a new sign-in", stale)
		}
	}
	if readJSON(t, store.BackupPath())["refresh_token"] != "old-refresh" {
		t.Fatal("the backup must hold the replaced login")
	}
}

func TestLoginTimesOut(t *testing.T) {
	store := storeIn(t)
	client := clientFor(t, newFakeIssuer(t))
	client.Config.LoginTimeoutMS = 50
	idle := newBrowser(t, func(string, string) []string { return nil })
	err := Login(context.Background(), store, client, options(idle, nil))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v", err)
	}
}

func TestLoginPortInUse(t *testing.T) {
	holder, err := (&portHolder{}).listen()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	client := clientFor(t, newFakeIssuer(t))
	client.Config.LoginPort = holder.port()
	err = Login(context.Background(), storeIn(t), client, LoginOptions{})
	if err == nil || !strings.Contains(err.Error(), "listen for the sign-in callback") {
		t.Fatalf("err %v", err)
	}
}

func TestLoginExchangeFailureWritesNothing(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t, reply{400, `{"error":"invalid_grant"}`})
	person := newBrowser(t, goodVisit("c"))
	err := Login(context.Background(), store, clientFor(t, issuer), options(person, nil))
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(store.Path); statErr == nil {
		t.Fatal("a failed exchange wrote a file")
	}
}

func TestCopyCodeFlowUsesTheCopyRedirectAndTheStateFromThePaste(t *testing.T) {
	for name, paste := range map[string]func(state string) string{
		"code#state": func(state string) string { return "pasted-code#" + state + "\n" },
		"url":        func(state string) string { return "https://x.example/cb?code=pasted-code&state=" + state + "\n" },
		"code only":  func(state string) string { return "pasted-code\n" },
	} {
		t.Run(name, func(t *testing.T) {
			store := storeIn(t)
			issuer := newFakeIssuer(t, reply{200, tokenBody("acc-cc", "ref-cc", 60)})
			// The input is built once the URL (and so the state) is known.
			reader, writer := io.Pipe()
			opts := LoginOptions{CopyCode: true, In: reader, Now: func() time.Time { return fixedNow }}
			var opened *url.URL
			opts.OpenBrowser = func(raw string) error {
				opened, _ = url.Parse(raw)
				go func() { _, _ = io.WriteString(writer, paste(opened.Query().Get("state"))) }()
				return nil
			}
			if err := Login(context.Background(), store, clientFor(t, issuer), opts); err != nil {
				t.Fatal(err)
			}
			call := issuer.Calls[0].JSON
			if opened.Query().Get("redirect_uri") != DefaultCopyCodeURI || call["redirect_uri"] != DefaultCopyCodeURI ||
				call["code"] != "pasted-code" || call["state"] != opened.Query().Get("state") {
				t.Fatalf("copy-code exchange wrong: redirect %s, %v", opened.Query().Get("redirect_uri"), keys(call))
			}
			if readJSON(t, store.Path)["access_token"] != "acc-cc" {
				t.Fatal("tokens not stored")
			}
		})
	}
}

func TestCopyCodeRejectsAForeignStateAndEmptyInput(t *testing.T) {
	for name, input := range map[string]string{
		"foreign state": "code#some-other-state\n",
		"empty":         "\n",
		"no input":      "",
	} {
		t.Run(name, func(t *testing.T) {
			store := storeIn(t)
			issuer := newFakeIssuer(t)
			opts := LoginOptions{CopyCode: true, In: strings.NewReader(input)}
			if err := Login(context.Background(), store, clientFor(t, issuer), opts); err == nil {
				t.Fatal("accepted")
			}
			if issuer.callCount() != 0 {
				t.Fatal("the issuer was called")
			}
			if _, statErr := os.Stat(store.Path); statErr == nil {
				t.Fatal("a file was written")
			}
		})
	}
}

func TestCopyCodeWithoutAnInputIsAnError(t *testing.T) {
	err := Login(context.Background(), storeIn(t), clientFor(t, newFakeIssuer(t)), LoginOptions{CopyCode: true})
	if err == nil || !strings.Contains(err.Error(), "no input") {
		t.Fatalf("err %v", err)
	}
}

func TestLoginIgnoresAStrayErrorWithoutStateAndKeepsWaiting(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t, reply{200, tokenBody("acc", "ref", 60)})
	person := newBrowser(t, func(redirect, state string) []string {
		return []string{
			redirect + "?error=access_denied",
			redirect + "?error=access_denied&state=wrong",
			redirect + "?code=good&state=" + url.QueryEscape(state),
		}
	})
	if err := Login(context.Background(), store, clientFor(t, issuer), options(person, nil)); err != nil {
		t.Fatalf("a stray ?error= ended the sign-in: %v", err)
	}
	if issuer.Calls[0].JSON["code"] != "good" {
		t.Fatalf("exchange used %v", issuer.Calls[0].JSON["code"])
	}
}

func TestLoginReportsTheIssuerErrorWhenTheStateIsRight(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t)
	person := newBrowser(t, func(redirect, state string) []string {
		return []string{redirect + "?error=access_denied&state=" + url.QueryEscape(state)}
	})
	err := Login(context.Background(), store, clientFor(t, issuer), options(person, nil))
	if err == nil || !strings.Contains(err.Error(), "access_denied") || issuer.callCount() != 0 {
		t.Fatalf("err %v calls %d", err, issuer.callCount())
	}
}

func TestLoginArmsTheStateBeforeTheBrowserOpens(t *testing.T) {
	store := storeIn(t)
	issuer := newFakeIssuer(t, reply{200, tokenBody("acc", "ref", 60)})
	var status int
	opts := LoginOptions{Now: func() time.Time { return fixedNow }}
	opts.OpenBrowser = func(raw string) error {
		parsed, err := url.Parse(raw)
		if err != nil {
			return err
		}
		response, err := http.Get(parsed.Query().Get("redirect_uri") + "?code=c&state=" + url.QueryEscape(parsed.Query().Get("state")))
		if err != nil {
			return err
		}
		response.Body.Close()
		status = response.StatusCode
		return nil
	}
	client := clientFor(t, issuer)
	client.Config.LoginTimeoutMS = 2000
	if err := Login(context.Background(), store, client, opts); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("a redirect arriving while the browser opens was answered %d, want 200", status)
	}
}
