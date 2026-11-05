package triggers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"jad/backend-go/internal/sessionfs"
)

// Delivery states of a notification, as GET /api/triggers shows them.
const (
	NotifyPending = "pending"
	NotifySent    = "sent"
	NotifyFailed  = "failed"
)

// maxNotifyResponseBytes is how much of a notifier's response is read before the
// connection is reused; the body itself is not used.
const maxNotifyResponseBytes = 64 * 1024

// Notification is the delivery of one finished run to one notifier.
type Notification struct {
	Notifier string `json:"notifier"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
}

// notifier is a server.triggers.notify entry with its address and headers resolved from
// the environment.
type notifier struct {
	settings sessionfs.TriggerNotifier
	url      string
	headers  http.Header
	// template is settings.Template parsed; nil without one.
	template *template.Template
}

// notifyBody is the json format: the run's record, a one-line text and, when answer_bytes
// allows, the start of the final answer.
type notifyBody struct {
	Record
	Text            string `json:"text"`
	Answer          string `json:"answer,omitempty"`
	AnswerTruncated bool   `json:"answer_truncated,omitempty"`
}

// notifyTemplateData is what a notifier's template reads: the json format's fields by their
// Go names, with the cost a plain number (0 when unknown) so printf and if take it.
type notifyTemplateData struct {
	ID, Trigger, Source, Session, Profile, Status, TurnID, Error string
	StopHooksSkipped                                             string
	StartedAt, EndedAt                                           time.Time
	CostUSD                                                      float64
	DurationMS                                                   int64
	Text, Answer                                                 string
	AnswerTruncated                                              bool
}

func templateData(body notifyBody) notifyTemplateData {
	data := notifyTemplateData{
		ID: body.ID, Trigger: body.Trigger, Source: body.Source, Session: body.Session, Profile: body.Profile,
		Status: body.Status, TurnID: body.TurnID, Error: body.Error, StopHooksSkipped: body.StopHooksSkipped, StartedAt: body.StartedAt, EndedAt: body.EndedAt,
		DurationMS: body.DurationMS, Text: body.Text, Answer: body.Answer, AnswerTruncated: body.AnswerTruncated,
	}
	if body.CostUSD != nil {
		data.CostUSD = *body.CostUSD
	}
	return data
}

// newNotifiers resolves every notifier's url_env and headers_env. A variable that is unset
// or empty stops the server, as a webhook secret does, instead of failing each delivery.
func newNotifiers(settings []sessionfs.TriggerNotifier, environment map[string]string) (map[string]*notifier, error) {
	notifiers := make(map[string]*notifier, len(settings))
	for _, setting := range settings {
		resolved := &notifier{settings: setting, url: setting.URL, headers: http.Header{}}
		section := "server.triggers.notify." + setting.Name
		if setting.Template != "" {
			parsed, err := sessionfs.ParseTriggerNotifyTemplate(setting.Name, setting.Template)
			if err != nil {
				return nil, fmt.Errorf("%s.template: %v", section, err)
			}
			resolved.template = parsed
		}
		if setting.URLEnv != "" {
			resolved.url = environment[setting.URLEnv]
			if resolved.url == "" {
				return nil, fmt.Errorf("%s.url_env: the environment variable %s is not set or empty", section, setting.URLEnv)
			}
			if err := sessionfs.CheckTriggerNotifyURL(resolved.url); err != nil {
				return nil, fmt.Errorf("%s.url_env: the value of %s %v", section, setting.URLEnv, err)
			}
		}
		for name, value := range setting.Headers {
			resolved.headers.Set(name, value)
		}
		for name, variable := range setting.HeadersEnv {
			value := environment[variable]
			if value == "" || strings.ContainsAny(value, "\r\n") {
				return nil, fmt.Errorf("%s.headers_env.%s: the environment variable %s is not set, empty or not one line", section, name, variable)
			}
			resolved.headers.Set(name, value)
		}
		notifiers[setting.Name] = resolved
	}
	return notifiers, nil
}

// pendingNotifications lists the entry's notifiers that report status. m.mu need not be held.
func (m *Manager) pendingNotifications(entry sessionfs.TriggerEntry, status string) []Notification {
	var pending []Notification
	for _, name := range entry.Notify {
		if target := m.notifiers[name]; target != nil && target.settings.Wants(status) {
			pending = append(pending, Notification{Notifier: name, Status: NotifyPending})
		}
	}
	return pending
}

// notify sends record to each of its pending notifiers, each in a goroutine of its own
// counted in m.wg. The caller holds m.mu and has added record to the history, and the
// manager is not closed or a run is still counted, so the wait group is not at zero.
func (m *Manager) notify(record Record, answer string) {
	for _, pending := range record.Notifications {
		target := m.notifiers[pending.Notifier]
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			delivered := m.deliver(target, record, answer)
			m.mu.Lock()
			m.setNotification(record.ID, delivered)
			m.mu.Unlock()
			if delivered.Status == NotifyFailed {
				m.logger.Warn("trigger notification failed", slog.String("trigger", record.Trigger), slog.String("run", record.ID), slog.String("notifier", delivered.Notifier), slog.Int("attempts", delivered.Attempts), slog.String("error", delivered.Error))
			}
		}()
	}
}

// setNotification records a delivery on the run's history entry. m.mu is held. An entry
// that has left the history is not looked for.
func (m *Manager) setNotification(runID string, delivered Notification) {
	for index := len(m.history) - 1; index >= 0; index-- {
		if m.history[index].ID != runID {
			continue
		}
		notifications := append([]Notification(nil), m.history[index].Notifications...)
		for position := range notifications {
			if notifications[position].Notifier == delivered.Notifier {
				notifications[position] = delivered
			}
		}
		m.history[index].Notifications = notifications
		return
	}
}

// deliver posts the notification, retrying a failed attempt. Once the manager closes, the
// attempt in progress finishes but no retry starts.
func (m *Manager) deliver(target *notifier, record Record, answer string) Notification {
	result := Notification{Notifier: target.settings.Name}
	body, contentType, err := notificationBody(target, record, answer)
	if err != nil {
		// The same record renders the same way every time: no retry.
		result.Status, result.Error = NotifyFailed, err.Error()
		return result
	}
	for {
		result.Attempts++
		err := m.post(target, body, contentType)
		if err == nil {
			result.Status, result.Error = NotifySent, ""
			return result
		}
		result.Status, result.Error = NotifyFailed, err.Error()
		if int64(result.Attempts) > target.settings.Retries {
			return result
		}
		select {
		case <-m.ctx.Done():
			return result
		case <-m.after(time.Duration(target.settings.RetryDelayMS) * time.Millisecond):
		}
	}
}

func (m *Manager) post(target *notifier, body []byte, contentType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(target.settings.TimeoutMS)*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("the request could not be built")
	}
	request.Header = target.headers.Clone()
	if request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := m.client.Do(request)
	if err != nil {
		// A *url.Error repeats the address, which may be a secret (url_env).
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxNotifyResponseBytes))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

// notificationBody renders the notification: the notifier's template when it has one,
// else its format's own body. A template whose output a json or slack notifier cannot
// send (not JSON) is an error.
func notificationBody(target *notifier, record Record, answer string) ([]byte, string, error) {
	settings := target.settings
	record.Notifications = nil
	text := notificationText(record)
	answer, truncated := cutAnswer(strings.TrimSpace(answer), settings.AnswerBytes)
	contentType := "application/json"
	if settings.Format == sessionfs.TriggerNotifyFormatText {
		contentType = "text/plain; charset=utf-8"
	}
	data := notifyBody{Record: record, Text: text, Answer: answer, AnswerTruncated: truncated}
	if target.template != nil {
		var out bytes.Buffer
		if err := target.template.Execute(&out, templateData(data)); err != nil {
			return nil, "", fmt.Errorf("the template failed: %v", err)
		}
		if contentType == "application/json" && !json.Valid(out.Bytes()) {
			return nil, "", fmt.Errorf("the template did not produce JSON, which format %s sends; use the json function for strings", settings.Format)
		}
		return out.Bytes(), contentType, nil
	}
	switch settings.Format {
	case sessionfs.TriggerNotifyFormatText:
		return []byte(plainNotification(text, record.Error, record.StopHooksSkipped, answer, truncated) + "\n"), contentType, nil
	case sessionfs.TriggerNotifyFormatSlack:
		// A Slack incoming webhook takes {"text": ...}; &, < and > must be escaped in it.
		encoded, _ := json.Marshal(map[string]string{"text": sessionfs.SlackEscape(plainNotification(text, record.Error, record.StopHooksSkipped, answer, truncated))})
		return encoded, contentType, nil
	}
	// Every field is a string, a number or a time, so encoding cannot fail.
	encoded, _ := json.Marshal(data)
	return encoded, contentType, nil
}

// plainNotification is the text and slack body: the line, the error, the report of stop
// commands skipped at a cap, and the answer.
func plainNotification(text, runError, stopSkipped, answer string, truncated bool) string {
	var out strings.Builder
	out.WriteString(text)
	if runError != "" {
		out.WriteString("\n" + runError)
	}
	if stopSkipped != "" {
		out.WriteString("\n" + stopSkipped)
	}
	if answer != "" {
		out.WriteString("\n\n" + answer)
		if truncated {
			out.WriteString("…")
		}
	}
	return out.String()
}

// notificationText is one line: the trigger, how the run ended, and where and how long.
func notificationText(record Record) string {
	details := []string{"session " + record.Session}
	if record.Source != "" {
		details = append(details, record.Source)
	}
	if !record.EndedAt.IsZero() && !record.StartedAt.IsZero() {
		details = append(details, record.EndedAt.Sub(record.StartedAt).Round(100*time.Millisecond).String())
	}
	if record.CostUSD != nil {
		details = append(details, fmt.Sprintf("$%.4f", *record.CostUSD))
	}
	return fmt.Sprintf("jad trigger %s %s (%s)", record.Trigger, strings.ReplaceAll(record.Status, "_", " "), strings.Join(details, ", "))
}

// cutAnswer keeps at most limit bytes of answer, on a rune boundary.
func cutAnswer(answer string, limit int64) (string, bool) {
	if limit <= 0 {
		return "", false
	}
	if int64(len(answer)) <= limit {
		return answer, false
	}
	cut := int(limit)
	for cut > 0 && !utf8.RuneStart(answer[cut]) {
		cut--
	}
	return answer[:cut], true
}
