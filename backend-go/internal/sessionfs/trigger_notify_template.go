package sessionfs

import (
	"encoding/json"
	"strings"
	"text/template"
)

// slackEscaper escapes the three characters Slack message text gives a meaning to.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// SlackEscape returns text with &, < and > escaped as Slack message text needs them.
func SlackEscape(text string) string {
	return slackEscaper.Replace(text)
}

// triggerNotifyTemplateFuncs are the functions a notifier template may call besides the
// built-in ones: json writes a value as JSON (a string with its quotes), slack escapes
// text for a Slack message.
var triggerNotifyTemplateFuncs = template.FuncMap{
	"json": func(value any) (string, error) {
		encoded, err := json.Marshal(value)
		return string(encoded), err
	},
	"slack": SlackEscape,
}

// ParseTriggerNotifyTemplate parses a notifier's template with the notifier functions. A
// missing field is an error when the template runs, not an empty value.
func ParseTriggerNotifyTemplate(name, text string) (*template.Template, error) {
	return template.New(name).Funcs(triggerNotifyTemplateFuncs).Option("missingkey=error").Parse(text)
}
