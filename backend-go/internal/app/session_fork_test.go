package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (f *heldResetFixture) call(method, path, body string) (int, string) {
	f.t.Helper()
	request, _ := http.NewRequest(method, f.api.URL+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

// runTurn runs one turn on session and reads the stream to its end.
func (f *heldResetFixture) runTurn(session, input string) {
	f.t.Helper()
	request, _ := http.NewRequest(http.MethodPost, f.api.URL+"/api/sessions/"+session+"/run?profile=dev/coder", strings.NewReader(`{"input":"`+input+`"}`))
	request.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		f.t.Fatalf("run %s: %d %s", input, response.StatusCode, data)
	}
	// The run goes on in the background; the stream ends with it.
	stream, _ := http.NewRequest(http.MethodGet, f.api.URL+"/api/sessions/"+session+"/run/stream?profile=dev/coder", nil)
	stream.Header.Set("Authorization", "Bearer "+integrationAPIToken)
	streamed, err := http.DefaultClient.Do(stream)
	if err != nil {
		f.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, streamed.Body)
	streamed.Body.Close()
}

// userMessages returns the ids and texts of the user messages of the session snapshot.
func (f *heldResetFixture) userMessages(session string) (ids, texts []string) {
	f.t.Helper()
	status, body := f.call(http.MethodGet, "/api/sessions/"+session+"?profile=dev/coder", "")
	if status != http.StatusOK {
		f.t.Fatalf("snapshot %s: %d %s", session, status, body)
	}
	var snapshot struct {
		Messages []struct {
			ID     string `json:"id"`
			Sender string `json:"sender"`
			Text   string `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		f.t.Fatal(err)
	}
	for _, message := range snapshot.Messages {
		if message.Sender == "user" {
			ids, texts = append(ids, message.ID), append(texts, message.Text)
		}
	}
	return ids, texts
}

func (f *heldResetFixture) transcript(session string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.fixture.StateDir, session, "dev", "coder", "messages.jsonl"))
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func TestForkCopiesTheTranscriptUpToATurn(t *testing.T) {
	f := newHeldResetFixture(t)
	close(f.release)
	for _, input := range []string{"first", "second", "third"} {
		f.runTurn("local:one", input)
	}
	ids, texts := f.userMessages("local:one")
	if strings.Join(texts, ",") != "first,second,third" {
		t.Fatalf("source user messages = %v", texts)
	}
	source := f.transcript("one")

	// Before the second turn: the fork can ask the second question again.
	status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:retry","message_id":"`+ids[1]+`","keep_turn":false}`)
	if status != http.StatusCreated || !strings.Contains(body, `"turns":1`) || !strings.Contains(body, `"profile":"dev/coder"`) {
		t.Fatalf("fork before the second turn: %d %s", status, body)
	}
	if _, texts := f.userMessages("local:retry"); strings.Join(texts, ",") != "first" {
		t.Fatalf("fork before the second turn holds %v", texts)
	}
	forked := f.transcript("retry")
	if !bytes.HasPrefix(source, forked) || len(forked) == 0 || len(forked) >= len(source) {
		t.Fatalf("the fork is not a prefix of the source transcript: %d of %d bytes", len(forked), len(source))
	}

	// Through the second turn, named by its message id; then the fork goes on by itself.
	status, body = f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:branch","message_id":"`+ids[1]+`"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"turns":2`) {
		t.Fatalf("fork through the second turn: %d %s", status, body)
	}
	f.runTurn("local:branch", "other")
	if _, texts := f.userMessages("local:branch"); strings.Join(texts, ",") != "first,second,other" {
		t.Fatalf("branch after a run holds %v", texts)
	}
	if !bytes.Equal(f.transcript("one"), source) {
		t.Fatal("the source transcript changed")
	}
	if config, err := os.ReadFile(filepath.Join(f.fixture.SessionsDir, "branch.json")); err != nil || len(config) == 0 {
		t.Fatalf("the fork's session file: %v", err)
	}

	// Without a fork point, the whole transcript.
	status, body = f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:copy"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"turns":3`) || !bytes.Equal(f.transcript("copy"), source) {
		t.Fatalf("whole fork: %d %s", status, body)
	}

	for name, test := range map[string]struct {
		body   string
		status int
	}{
		"existing target":    {`{"session":"local:copy"}`, http.StatusPreconditionFailed},
		"same session":       {`{"session":"local:one"}`, http.StatusBadRequest},
		"unknown message":    {`{"session":"local:x1","message_id":"session-user-o999999"}`, http.StatusNotFound},
		"unknown cursor":     {`{"session":"local:x2","cursor":7}`, http.StatusNotFound},
		"both fork points":   {`{"session":"local:x3","cursor":0,"message_id":"m"}`, http.StatusBadRequest},
		"keep without point": {`{"session":"local:x4","keep_turn":false}`, http.StatusBadRequest},
		"no session":         {`{}`, http.StatusBadRequest},
		"unknown field":      {`{"session":"local:x5","turn":1}`, http.StatusBadRequest},
		"other root":         {`{"session":"elsewhere:x6"}`, http.StatusNotFound},
	} {
		status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", test.body)
		if status != test.status {
			t.Errorf("%s: %d %s, want %d", name, status, body, test.status)
		}
	}
	for _, refused := range []string{"x1", "x2", "x3", "x4", "x5", "x6"} {
		if _, err := os.Stat(filepath.Join(f.fixture.SessionsDir, refused+".json")); !os.IsNotExist(err) {
			t.Errorf("a refused fork left the session file %s: %v", refused, err)
		}
	}
}

func TestForkIsRefusedWhileARunIsActive(t *testing.T) {
	f := newHeldResetFixture(t)
	run := f.startHeldRun()
	f.awaitRunActive()
	status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:during"}`)
	if status != http.StatusConflict || !strings.Contains(body, "while a run is active") {
		t.Fatalf("fork during a run: %d %s", status, body)
	}
	close(f.release)
	if _, err := io.Copy(io.Discard, run); err != nil {
		t.Fatal(err)
	}
	if status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:during"}`); status != http.StatusCreated {
		t.Fatalf("fork after the run: %d %s", status, body)
	}
}

func TestCopyStoredImagesCopiesOnlyNamedFilesOnce(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "images")
	name := strings.Repeat("ab", 32) + ".png"
	if err := os.WriteFile(filepath.Join(from, name), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "other.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := strings.Repeat("cd", 32) + ".png"
	copied, err := copyStoredImages(from, to, []string{name, name, missing, "../escape.png"})
	if err != nil || copied != 1 {
		t.Fatalf("copied %d, %v", copied, err)
	}
	if data, err := os.ReadFile(filepath.Join(to, name)); err != nil || string(data) != "png" {
		t.Fatalf("copied image = %q, %v", data, err)
	}
	if copied, err := copyStoredImages(from, to, []string{name}); err != nil || copied != 0 {
		t.Fatalf("a second copy wrote %d, %v", copied, err)
	}
	if copied, err := copyStoredImages(from, from, []string{name}); err != nil || copied != 0 {
		t.Fatalf("the same directory copied %d, %v", copied, err)
	}
}

// A message id found in two turns, here the provider's repeated response id, names no
// fork point: the fork is refused rather than cut at the first of them.
func TestForkRefusesAMessageIDOfSeveralTurns(t *testing.T) {
	f := newHeldResetFixture(t)
	close(f.release)
	for _, input := range []string{"first", "second"} {
		f.runTurn("local:one", input)
	}
	status, body := f.call(http.MethodGet, "/api/sessions/local:one?profile=dev/coder", "")
	var snapshot struct {
		Messages []struct {
			ID     string `json:"id"`
			Sender string `json:"sender"`
		} `json:"messages"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(body), &snapshot) != nil {
		t.Fatalf("snapshot: %d %s", status, body)
	}
	var answers []string
	for _, message := range snapshot.Messages {
		if message.Sender == "assistant" {
			answers = append(answers, message.ID)
		}
	}
	if len(answers) != 2 || answers[0] != answers[1] {
		t.Fatalf("the fixture's answers should share an id: %v", answers)
	}
	status, body = f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"session":"local:after","message_id":"`+answers[1]+`","keep_turn":true}`)
	if status != http.StatusConflict || !strings.Contains(body, "several turns") {
		t.Fatalf("fork at a repeated id: %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(f.fixture.SessionsDir, "after.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused fork left a session file: %v", err)
	}
}

func TestForkIntoANewProfileOfTheSameSession(t *testing.T) {
	f := newHeldResetFixture(t)
	close(f.release)
	for _, input := range []string{"first", "second"} {
		f.runTurn("local:one", input)
	}
	ids, _ := f.userMessages("local:one")
	source := f.transcript("one")
	sessionFile := filepath.Join(f.fixture.SessionsDir, "one.json")
	before, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}

	status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"profile":"coder-try","title":"Coder (try)","message_id":"`+ids[1]+`","keep_turn":false}`)
	if status != http.StatusCreated || !strings.Contains(body, `"session":"local:one"`) || !strings.Contains(body, `"profile":"dev/coder-try"`) || !strings.Contains(body, `"turns":1`) {
		t.Fatalf("profile fork: %d %s", status, body)
	}
	forked, err := os.ReadFile(filepath.Join(f.fixture.StateDir, "one", "dev", "coder-try", "messages.jsonl"))
	if err != nil || len(forked) == 0 || !bytes.HasPrefix(source, forked) || len(forked) >= len(source) {
		t.Fatalf("the new profile's transcript is not a shorter prefix: %d of %d bytes, %v", len(forked), len(source), err)
	}
	if !bytes.Equal(f.transcript("one"), source) {
		t.Fatal("the source transcript changed")
	}
	status, profileFile := f.call(http.MethodGet, "/api/catalog/profiles/dev/coder-try", "")
	if status != http.StatusOK || !strings.Contains(profileFile, `"name": "Coder (try)"`) {
		t.Fatalf("new profile file: %d %s", status, profileFile)
	}
	// The session has no scope, so it selects every profile and its file is left alone.
	if after, _ := os.ReadFile(sessionFile); !bytes.Equal(before, after) {
		t.Fatalf("the session file changed:\n%s", after)
	}
	status, snapshot := f.call(http.MethodGet, "/api/sessions/local:one?profile=dev/coder-try", "")
	if status != http.StatusOK || !strings.Contains(snapshot, `"text":"first"`) || strings.Contains(snapshot, `"text":"second"`) {
		t.Fatalf("new profile snapshot: %d %s", status, snapshot)
	}

	// With a scope list, the new profile is added to it; state none copies nothing.
	scoped := strings.Replace(string(before), `"schema_version": 1,`, `"schema_version": 1, "scope": {"profiles": ["dev/coder", "dev/coder-try"]},`, 1)
	if scoped == string(before) {
		t.Fatal("the scope edit did not apply")
	}
	if err := os.WriteFile(sessionFile, []byte(scoped), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body = f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", `{"profile":"coder-blank","state":"none"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"turns":0`) {
		t.Fatalf("empty profile fork: %d %s", status, body)
	}
	if data, err := os.ReadFile(filepath.Join(f.fixture.StateDir, "one", "dev", "coder-blank", "messages.jsonl")); err != nil || len(data) != 0 {
		t.Fatalf("empty fork transcript: %q %v", data, err)
	}
	if after, _ := os.ReadFile(sessionFile); !strings.Contains(string(after), `"dev/coder-blank"`) || !strings.Contains(string(after), `"cwd"`) {
		t.Fatalf("the scope list did not get the second profile:\n%s", after)
	}

	for name, test := range map[string]struct {
		body   string
		status int
	}{
		"existing profile":      {`{"profile":"coder-try"}`, http.StatusPreconditionFailed},
		"same profile":          {`{"profile":"coder"}`, http.StatusBadRequest},
		"bad name":              {`{"profile":"../x"}`, http.StatusBadRequest},
		"both targets":          {`{"profile":"p1","session":"local:p1"}`, http.StatusBadRequest},
		"title without profile": {`{"session":"local:p2","title":"x"}`, http.StatusBadRequest},
		"bad state":             {`{"profile":"p3","state":"half"}`, http.StatusBadRequest},
		"none with a point":     {`{"profile":"p4","state":"none","cursor":0}`, http.StatusBadRequest},
		"unknown message":       {`{"profile":"p5","message_id":"session-user-o999999"}`, http.StatusNotFound},
	} {
		if status, body := f.call(http.MethodPost, "/api/sessions/local:one/fork?profile=dev/coder", test.body); status != test.status {
			t.Errorf("%s: %d %s, want %d", name, status, body, test.status)
		}
	}
	for _, refused := range []string{"p1", "p3", "p4", "p5"} {
		if status, _ := f.call(http.MethodGet, "/api/catalog/profiles/dev/"+refused, ""); status != http.StatusNotFound {
			t.Errorf("a refused fork left the profile dev/%s: %d", refused, status)
		}
	}
}
