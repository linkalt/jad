package triggers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/sessionfs"
)

// notifySink is an HTTP server that records every notification and answers with the
// next status of codes (200 once they run out).
type notifySink struct {
	mu       sync.Mutex
	server   *httptest.Server
	requests []*http.Request
	bodies   []string
	codes    []int
}

func newNotifySink(t *testing.T, codes ...int) *notifySink {
	t.Helper()
	sink := &notifySink{codes: codes}
	sink.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sink.mu.Lock()
		sink.requests = append(sink.requests, r)
		sink.bodies = append(sink.bodies, string(body))
		code := http.StatusOK
		if len(sink.codes) > 0 {
			code, sink.codes = sink.codes[0], sink.codes[1:]
		}
		sink.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(sink.server.Close)
	return sink
}

func (s *notifySink) received() ([]*http.Request, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...), append([]string(nil), s.bodies...)
}

func notifyManager(t *testing.T, runner Runner, notifiers []sessionfs.TriggerNotifier, environment map[string]string, entries ...sessionfs.TriggerEntry) *Manager {
	t.Helper()
	settings := settings(entries...)
	settings.Notifiers = notifiers
	if environment == nil {
		environment = map[string]string{}
	}
	environment["CI_SECRET"] = "s3cret"
	manager, err := New(Options{Settings: settings, Environment: environment, Runner: runner, APIToken: "api-token",
		Now: func() time.Time { return time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager
}

// waitNotified waits until the newest run's notifications have all ended.
func waitNotified(t *testing.T, handler http.Handler) Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, runs := list(t, handler)
		if len(runs) > 0 {
			done := true
			for _, notification := range runs[0].Notifications {
				done = done && notification.Status != NotifyPending
			}
			if done {
				return runs[0]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications did not end: %+v", runs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testNotifier(name, url string) sessionfs.TriggerNotifier {
	return sessionfs.TriggerNotifier{Name: name, URL: url, Format: sessionfs.TriggerNotifyFormatJSON, On: []string{"completed", "failed", "timed_out", "aborted", "refused"}, TimeoutMS: 2000, Retries: 0, RetryDelayMS: 1}
}

func TestFinishedRunIsSentToItsNotifiers(t *testing.T) {
	jsonSink, textSink, failedOnly := newNotifySink(t), newNotifySink(t), newNotifySink(t)
	ops := testNotifier("ops", "")
	ops.URLEnv = "OPS_URL"
	ops.Headers = map[string]string{"X-Tag": "nightly"}
	ops.HeadersEnv = map[string]string{"Authorization": "OPS_TOKEN"}
	phone := testNotifier("phone", textSink.server.URL+"/topic")
	phone.Format, phone.AnswerBytes = sessionfs.TriggerNotifyFormatText, 6
	pager := testNotifier("pager", failedOnly.server.URL)
	pager.On = []string{"failed"}
	runner := newFakeRunner()
	cost := 0.0123
	runner.result = headless.Result{Status: headless.StatusCompleted, TurnID: "t1", Text: "héllo wörld, all green", CostUSD: &cost}
	entry := webhookEntry()
	entry.Notify = []string{"ops", "pager", "phone"}
	manager := notifyManager(t, runner, []sessionfs.TriggerNotifier{ops, pager, phone},
		map[string]string{"OPS_URL": jsonSink.server.URL + "/hook", "OPS_TOKEN": "Bearer t0ken"}, entry)
	handler := manager.Wrap(http.NotFoundHandler())
	body := `{"x":1}`
	if response := post(handler, "/api/triggers/ci", body, sign("s3cret", body)); response.Code != http.StatusAccepted {
		t.Fatalf("webhook: %d %s", response.Code, response.Body)
	}
	close(runner.release)
	run := waitNotified(t, handler)
	if len(run.Notifications) != 2 || run.Notifications[0].Notifier != "ops" || run.Notifications[1].Notifier != "phone" {
		t.Fatalf("a completed run should go to ops and phone only: %+v", run.Notifications)
	}
	for _, notification := range run.Notifications {
		if notification.Status != NotifySent || notification.Attempts != 1 || notification.Error != "" {
			t.Fatalf("notification = %+v", notification)
		}
	}

	requests, bodies := jsonSink.received()
	if len(requests) != 1 || requests[0].URL.Path != "/hook" || requests[0].Header.Get("Authorization") != "Bearer t0ken" || requests[0].Header.Get("X-Tag") != "nightly" || requests[0].Header.Get("Content-Type") != "application/json" {
		t.Fatalf("json notification request = %+v", requests)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["id"] != run.ID || sent["trigger"] != "ci" || sent["status"] != "completed" || sent["source"] != "webhook" || sent["turn_id"] != "t1" || sent["cost_usd"] != 0.0123 ||
		sent["text"] != "jad trigger ci completed (session demo, webhook, 0s, $0.0123)" {
		t.Fatalf("json notification = %s", bodies[0])
	}
	for _, absent := range []string{"answer", "notifications"} {
		if _, found := sent[absent]; found {
			t.Fatalf("json notification has %s: %s", absent, bodies[0])
		}
	}

	requests, bodies = textSink.received()
	if len(requests) != 1 || requests[0].URL.Path != "/topic" || !strings.HasPrefix(requests[0].Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("text notification request = %+v", requests)
	}
	// Six bytes cut inside "ö" keep "héllo" (6 bytes) and mark the cut.
	if bodies[0] != "jad trigger ci completed (session demo, webhook, 0s, $0.0123)\n\nhéllo…\n" {
		t.Fatalf("text notification = %q", bodies[0])
	}
	if requests, _ := failedOnly.received(); len(requests) != 0 {
		t.Fatalf("a failed-only notifier got a completed run: %d", len(requests))
	}
	triggers, _ := list(t, handler)
	if notify, _ := triggers[0]["notify"].([]any); len(notify) != 3 {
		t.Fatalf("trigger list notify = %v", triggers[0]["notify"])
	}
}

func TestNotificationRetriesAndKeepsTheAddressOutOfErrors(t *testing.T) {
	flaky := newNotifySink(t, http.StatusBadGateway)
	broken := newNotifySink(t, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError)
	retried := testNotifier("retried", flaky.server.URL)
	retried.Retries = 1
	failing := testNotifier("failing", broken.server.URL)
	failing.Retries = 2
	// Nothing listens on a closed server's port, so the request itself fails.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unreachable := testNotifier("unreachable", "")
	unreachable.URLEnv = "SECRET_URL"
	runner := newFakeRunner()
	entry := webhookEntry()
	entry.Notify = []string{"failing", "retried", "unreachable"}
	manager := notifyManager(t, runner, []sessionfs.TriggerNotifier{failing, retried, unreachable},
		map[string]string{"SECRET_URL": closed.URL + "/s3cret-path-token"}, entry)
	handler := manager.Wrap(http.NotFoundHandler())
	if _, err := manager.Fire("ci", SourceSchedule, nil); err != nil {
		t.Fatal(err)
	}
	close(runner.release)
	run := waitNotified(t, handler)
	byName := map[string]Notification{}
	for _, notification := range run.Notifications {
		byName[notification.Notifier] = notification
	}
	if got := byName["retried"]; got.Status != NotifySent || got.Attempts != 2 {
		t.Fatalf("a 502 then a 200 = %+v", got)
	}
	if got := byName["failing"]; got.Status != NotifyFailed || got.Attempts != 3 || got.Error != "HTTP 500" {
		t.Fatalf("three 500s = %+v", got)
	}
	if got := byName["unreachable"]; got.Status != NotifyFailed || got.Error == "" || strings.Contains(got.Error, "s3cret-path-token") || strings.Contains(got.Error, closed.URL) {
		t.Fatalf("an unreachable notifier = %+v", got)
	}
}

func TestSkippedRunIsSentOnlyWhenAsked(t *testing.T) {
	sink := newNotifySink(t)
	everything := testNotifier("everything", sink.server.URL)
	everything.On = append(everything.On, "skipped")
	runner := newFakeRunner()
	entry := webhookEntry()
	entry.Notify = []string{"everything"}
	manager := notifyManager(t, runner, []sessionfs.TriggerNotifier{everything}, nil, entry)
	handler := manager.Wrap(http.NotFoundHandler())
	if _, err := manager.Fire("ci", SourceSchedule, nil); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	if _, err := manager.Fire("ci", SourceSchedule, nil); err != ErrBusy {
		t.Fatalf("second firing = %v", err)
	}
	if run := waitNotified(t, handler); run.Status != StatusSkipped || len(run.Notifications) != 1 || run.Notifications[0].Status != NotifySent {
		t.Fatalf("skipped run = %+v", run)
	}
	close(runner.release)
	waitRuns(t, handler, 2)
	if run := waitNotified(t, handler); run.Status != headless.StatusCompleted {
		t.Fatalf("finished run = %+v", run)
	}
	if requests, bodies := sink.received(); len(requests) != 2 || !strings.Contains(bodies[0], `"status":"skipped"`) {
		t.Fatalf("notifications = %v", bodies)
	}
}

func TestCloseStopsNotificationRetries(t *testing.T) {
	sink := newNotifySink(t, http.StatusInternalServerError, http.StatusInternalServerError)
	slow := testNotifier("slow", sink.server.URL)
	slow.Retries, slow.RetryDelayMS = 5, 60_000
	runner := newFakeRunner()
	entry := webhookEntry()
	entry.Notify = []string{"slow"}
	settings := settings(entry)
	settings.Notifiers = []sessionfs.TriggerNotifier{slow}
	manager, err := New(Options{Settings: settings, Environment: map[string]string{"CI_SECRET": "s3cret"}, Runner: runner, APIToken: "api-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Fire("ci", SourceSchedule, nil); err != nil {
		t.Fatal(err)
	}
	close(runner.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if requests, _ := sink.received(); len(requests) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first attempt was not made")
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	manager.Close()
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("Close waited %v for a retry delay", waited)
	}
	if len(manager.history) != 1 || manager.history[0].Notifications[0].Status != NotifyFailed || manager.history[0].Notifications[0].Attempts != 1 {
		t.Fatalf("history after close = %+v", manager.history)
	}
}

func TestNewNeedsEveryNotifierVariable(t *testing.T) {
	byURL := testNotifier("by-url", "")
	byURL.URLEnv = "HOOK_URL"
	withToken := testNotifier("with-token", "https://example.test/x")
	withToken.HeadersEnv = map[string]string{"Authorization": "HOOK_TOKEN"}
	for name, test := range map[string]struct {
		notifier    sessionfs.TriggerNotifier
		environment map[string]string
		want        string
	}{
		"url unset":        {byURL, map[string]string{}, "HOOK_URL is not set or empty"},
		"url not http":     {byURL, map[string]string{"HOOK_URL": "ftp://example.test/secret"}, "absolute http or https URL"},
		"header unset":     {withToken, map[string]string{}, "HOOK_TOKEN is not set"},
		"header two lines": {withToken, map[string]string{"HOOK_TOKEN": "a\r\nX-Evil: 1"}, "not one line"},
	} {
		settings := settings()
		settings.Notifiers = []sessionfs.TriggerNotifier{test.notifier}
		_, err := New(Options{Settings: settings, Environment: test.environment, Runner: newFakeRunner()})
		if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

// A slack notifier posts {"text": ...} with &, < and > escaped; a template renders any other
// body, reading the record's fields; a template whose output is not JSON fails without a
// retry and nothing is sent.
func TestSlackFormatAndTemplates(t *testing.T) {
	slackSink, discordSink, textSink, brokenSink := newNotifySink(t), newNotifySink(t), newNotifySink(t), newNotifySink(t)
	slack := testNotifier("slack", slackSink.server.URL)
	slack.Format, slack.AnswerBytes = sessionfs.TriggerNotifyFormatSlack, 100
	discord := testNotifier("discord", discordSink.server.URL)
	discord.Template = `{"content": {{json .Text}}, "status": {{json .Status}}, "answer": {{json .Answer}}}`
	discord.AnswerBytes = 100
	text := testNotifier("text", textSink.server.URL)
	text.Format, text.Template = sessionfs.TriggerNotifyFormatText, `{{.Trigger}} is {{.Status}}{{if .CostUSD}} for ${{printf "%.2f" .CostUSD}}{{end}}: {{slack .Answer}}`
	text.AnswerBytes = 100
	broken := testNotifier("broken", brokenSink.server.URL)
	broken.Template, broken.Retries = `status={{.Status}}`, 3
	runner := newFakeRunner()
	cost := 1.5
	runner.result = headless.Result{Status: headless.StatusCompleted, TurnID: "t1", Text: "a < b & \"c\"", CostUSD: &cost}
	entry := webhookEntry()
	entry.Notify = []string{"broken", "discord", "slack", "text"}
	manager := notifyManager(t, runner, []sessionfs.TriggerNotifier{broken, discord, slack, text}, nil, entry)
	handler := manager.Wrap(http.NotFoundHandler())
	body := `{"x":1}`
	if response := post(handler, "/api/triggers/ci", body, sign("s3cret", body)); response.Code != http.StatusAccepted {
		t.Fatalf("webhook: %d %s", response.Code, response.Body)
	}
	close(runner.release)
	run := waitNotified(t, handler)
	byName := map[string]Notification{}
	for _, notification := range run.Notifications {
		byName[notification.Notifier] = notification
	}
	if got := byName["broken"]; got.Status != NotifyFailed || got.Attempts != 0 || !strings.Contains(got.Error, "did not produce JSON") {
		t.Fatalf("broken template notification = %+v", got)
	}
	if requests, _ := brokenSink.received(); len(requests) != 0 {
		t.Fatalf("a broken template sent %d requests", len(requests))
	}
	for _, name := range []string{"discord", "slack", "text"} {
		if byName[name].Status != NotifySent {
			t.Fatalf("%s notification = %+v", name, byName[name])
		}
	}

	_, bodies := slackSink.received()
	var slackBody map[string]string
	if err := json.Unmarshal([]byte(bodies[0]), &slackBody); err != nil || len(slackBody) != 1 ||
		slackBody["text"] != "jad trigger ci completed (session demo, webhook, 0s, $1.5000)\n\na &lt; b &amp; \"c\"" {
		t.Fatalf("slack body = %s", bodies[0])
	}
	_, bodies = discordSink.received()
	var discordBody map[string]string
	if err := json.Unmarshal([]byte(bodies[0]), &discordBody); err != nil ||
		discordBody["content"] != "jad trigger ci completed (session demo, webhook, 0s, $1.5000)" || discordBody["status"] != "completed" || discordBody["answer"] != `a < b & "c"` {
		t.Fatalf("template body = %s", bodies[0])
	}
	requests, bodies := textSink.received()
	if bodies[0] != `ci is completed for $1.50: a &lt; b &amp; "c"` || !strings.HasPrefix(requests[0].Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("text template body = %q", bodies[0])
	}
}

// A run whose stop commands were skipped at a cap keeps the report in its record, and the
// notifications carry it: as stop_hooks_skipped in json, as a line in text, and as
// .StopHooksSkipped in a template.
func TestSkippedStopHooksReachTheRecordAndTheNotifications(t *testing.T) {
	jsonSink, textSink := newNotifySink(t), newNotifySink(t)
	text := testNotifier("phone", textSink.server.URL)
	text.Format = sessionfs.TriggerNotifyFormatText
	runner := newFakeRunner()
	skipped := "[stop hooks] not run: it used 7 tokens, which reached the max_run_tokens limit of 5."
	runner.result = headless.Result{Status: headless.StatusCompleted, TurnID: "t1", StopHooksSkipped: skipped}
	entry := webhookEntry()
	entry.Notify = []string{"ops", "phone"}
	manager := notifyManager(t, runner, []sessionfs.TriggerNotifier{testNotifier("ops", jsonSink.server.URL), text}, nil, entry)
	handler := manager.Wrap(http.NotFoundHandler())
	body := `{"x":1}`
	if response := post(handler, "/api/triggers/ci", body, sign("s3cret", body)); response.Code != http.StatusAccepted {
		t.Fatalf("webhook: %d %s", response.Code, response.Body)
	}
	close(runner.release)
	if run := waitNotified(t, handler); run.StopHooksSkipped != skipped {
		t.Fatalf("record = %+v", run)
	}
	_, bodies := jsonSink.received()
	var sent map[string]any
	if len(bodies) != 1 || json.Unmarshal([]byte(bodies[0]), &sent) != nil || sent["stop_hooks_skipped"] != skipped {
		t.Fatalf("json notification = %v", bodies)
	}
	if _, bodies := textSink.received(); len(bodies) != 1 || !strings.Contains(bodies[0], ")\n"+skipped+"\n") {
		t.Fatalf("text notification = %q", bodies)
	}
	templated := testNotifier("chat", "http://127.0.0.1")
	templated.Format, templated.Template = sessionfs.TriggerNotifyFormatText, `{{.Status}}{{if .StopHooksSkipped}}: {{.StopHooksSkipped}}{{end}}`
	parsed, err := sessionfs.ParseTriggerNotifyTemplate(templated.Name, templated.Template)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _, err := notificationBody(&notifier{settings: templated, template: parsed}, Record{Status: "completed", StopHooksSkipped: skipped}, "")
	if err != nil || string(rendered) != "completed: "+skipped {
		t.Fatalf("template = %q, %v", rendered, err)
	}
}
