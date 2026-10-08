package anthropicadapter

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// The shapes of the harness's own tools: a top-level anyOf (fetch) and a top-level allOf with an
// if/then that itself holds an anyOf (task). Anthropic answers 400 for both at the top level.
const (
	fetchLikeSchema = `{"type":"object","properties":{"url":{"type":"string"},"cursor":{"type":"string"}},
	  "additionalProperties":false,"anyOf":[{"properties":{"url":{}},"required":["url"]},{"properties":{"cursor":{}},"required":["cursor"]}]}`
	taskLikeSchema = `{"type":"object","properties":{"action":{"type":"string"},
	  "tags":{"anyOf":[{"type":"string"},{"type":"array"}]}},
	  "allOf":[{"if":{"properties":{"action":{"const":"update"}}},"then":{"anyOf":[{"required":["title"]},{"required":["status"]}]}}]}`
)

func toolsRequest(parameters ...string) string {
	var tools []string
	names := []string{"fetch", "task", "plain"}
	for index, parameter := range parameters {
		tools = append(tools, `{"type":"function","name":"`+names[index]+`","description":"d","parameters":`+parameter+`}`)
	}
	return `{"model":"claude-x","input":"hi","tools":[` + strings.Join(tools, ",") + `]}`
}

func schemaOf(t *testing.T, tool anthropicTool) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(tool.InputSchema, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestTopLevelCombinatorsAreRemovedAndNestedOnesKept(t *testing.T) {
	config := mustConfig(t, `{}`)
	result, apiErr := translateRequest(config, []byte(toolsRequest(fetchLikeSchema, taskLikeSchema)))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	fetch, task := schemaOf(t, result.Request.Tools[0]), schemaOf(t, result.Request.Tools[1])
	for _, keyword := range []string{"anyOf", "allOf", "oneOf"} {
		if _, ok := fetch[keyword]; ok {
			t.Errorf("fetch still has top-level %s", keyword)
		}
		if _, ok := task[keyword]; ok {
			t.Errorf("task still has top-level %s", keyword)
		}
	}
	for _, keep := range []string{"type", "properties", "additionalProperties"} {
		if _, ok := fetch[keep]; !ok {
			t.Errorf("fetch lost %s", keep)
		}
	}
	// A property's own anyOf is allowed by Anthropic and describes a real constraint: it stays.
	if !strings.Contains(string(task["properties"]), `"anyOf"`) {
		t.Errorf("a nested anyOf was removed: %s", task["properties"])
	}
}

func TestRemovalsAreReportedAndASchemaWithoutThemIsUntouched(t *testing.T) {
	config := mustConfig(t, `{}`)
	plain := `{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"string"}},"required":["a"]}`
	result, apiErr := translateRequest(config, []byte(toolsRequest(fetchLikeSchema, taskLikeSchema, plain)))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if got := strings.Join(result.Ignored, ","); got != "tools.fetch.anyOf,tools.task.allOf" {
		t.Errorf("ignored = %q", got)
	}
	// Byte for byte: a schema with nothing to strip must not be re-encoded (key order, spacing).
	if string(result.Request.Tools[2].InputSchema) != plain {
		t.Errorf("a schema with no combinator was changed: %s", result.Request.Tools[2].InputSchema)
	}
}

func TestStrippedSchemaIsStableAcrossRequests(t *testing.T) {
	config := mustConfig(t, `{}`)
	first, _ := translateRequest(config, []byte(toolsRequest(fetchLikeSchema)))
	second, _ := translateRequest(config, []byte(toolsRequest(fetchLikeSchema)))
	if string(first.Request.Tools[0].InputSchema) != string(second.Request.Tools[0].InputSchema) {
		t.Fatal("the same tool gave two different schemas: a prompt-cache prefix would miss")
	}
}

func TestStripKeywordsAreConfigurable(t *testing.T) {
	empty := mustConfig(t, `{"strip_schema_keywords":[]}`)
	result, _ := translateRequest(empty, []byte(toolsRequest(fetchLikeSchema)))
	if !strings.Contains(string(result.Request.Tools[0].InputSchema), `"anyOf"`) || len(result.Ignored) != 0 {
		t.Errorf("an empty list must send schemas unchanged: %s / %v", result.Request.Tools[0].InputSchema, result.Ignored)
	}
	custom := mustConfig(t, `{"strip_schema_keywords":["additionalProperties"]}`)
	result, _ = translateRequest(custom, []byte(toolsRequest(fetchLikeSchema)))
	schema := schemaOf(t, result.Request.Tools[0])
	if _, ok := schema["additionalProperties"]; ok {
		t.Error("a configured keyword was not removed")
	}
	if _, ok := schema["anyOf"]; !ok {
		t.Error("a keyword not in the list was removed")
	}
}

func TestStripSchemaKeywordsRejectsEmptyEntry(t *testing.T) {
	if _, err := LoadConfig(writeConfig(t, `{"strip_schema_keywords":[" "]}`)); err == nil || !strings.Contains(err.Error(), "strip_schema_keywords") {
		t.Fatalf("err %v", err)
	}
}

func TestStripOnlyChangesWhatItIsToldTo(t *testing.T) {
	if got, removed := stripTopLevelKeywords(json.RawMessage(`not json`), []string{"anyOf"}); string(got) != "not json" || removed != nil {
		t.Errorf("invalid JSON must pass through: %s %v", got, removed)
	}
	if got, removed := stripTopLevelKeywords(json.RawMessage(`[1]`), []string{"anyOf"}); string(got) != "[1]" || removed != nil {
		t.Errorf("a non-object must pass through: %s %v", got, removed)
	}
}

// The adapter reports the removal to the caller in a response header.
func TestRemovalIsVisibleInTheResponseHeader(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	server := httptest.NewServer(NewHandler(mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`"}}`), nil))
	t.Cleanup(server.Close)
	status, header, raw := post(t, server.URL+"/v1/responses", toolsRequest(fetchLikeSchema), map[string]string{"Authorization": "Bearer t"})
	if status != 200 {
		t.Fatalf("status %d body %s", status, raw)
	}
	if header.Get("X-Adapter-Ignored-Fields") != "tools.fetch.anyOf" {
		t.Errorf("header %q", header.Get("X-Adapter-Ignored-Fields"))
	}
	var sent struct {
		Tools []struct {
			InputSchema map[string]json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte((*calls)[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent.Tools[0].InputSchema["anyOf"]; ok {
		t.Fatalf("the upstream request still has a top-level anyOf: %s", (*calls)[0].Body)
	}
}
