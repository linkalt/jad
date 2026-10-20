package anthropicadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustConfig(t *testing.T, text string) *Config {
	t.Helper()
	config, err := LoadConfig(writeConfig(t, text))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func translateOK(t *testing.T, config *Config, body string) anthropicRequest {
	t.Helper()
	result, apiErr := translateRequest(config, []byte(body))
	if apiErr != nil {
		t.Fatalf("translate %s: %v (%s)", body, apiErr, apiErr.Code)
	}
	return result.Request
}

func translateFail(t *testing.T, config *Config, body string) *apiError {
	t.Helper()
	_, apiErr := translateRequest(config, []byte(body))
	if apiErr == nil {
		t.Fatalf("translate %s: expected an error", body)
	}
	return apiErr
}

func asJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestSimpleTextRequest(t *testing.T) {
	request := translateOK(t, mustConfig(t, `{}`), `{"model":"claude-x","instructions":"be brief","input":"hi","temperature":0.5}`)
	want := `{"model":"claude-x","max_tokens":8192,"system":[{"type":"text","text":"be brief"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"temperature":0.5}`
	if got := asJSON(t, request); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestMaxOutputTokensAndDefault(t *testing.T) {
	config := mustConfig(t, `{"default_max_tokens":100}`)
	if got := translateOK(t, config, `{"model":"m","input":"x"}`).MaxTokens; got != 100 {
		t.Fatalf("default max tokens = %d", got)
	}
	if got := translateOK(t, config, `{"model":"m","input":"x","max_output_tokens":7}`).MaxTokens; got != 7 {
		t.Fatalf("explicit max tokens = %d", got)
	}
	translateFail(t, config, `{"model":"m","input":"x","max_output_tokens":0}`)
}

func TestToolRoundTripGroupsResultsFirst(t *testing.T) {
	body := `{"model":"m","input":[
	  {"role":"user","content":[{"type":"input_text","text":"go"}]},
	  {"type":"function_call","call_id":"c1","name":"a","arguments":"{\"x\":1}"},
	  {"type":"function_call","call_id":"c2","name":"b","arguments":""},
	  {"type":"function_call_output","call_id":"c2","output":"two"},
	  {"type":"function_call_output","call_id":"c1","output":"one"},
	  {"role":"user","content":"next"}]}`
	request := translateOK(t, mustConfig(t, `{}`), body)
	want := `[{"role":"user","content":[{"type":"text","text":"go"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"a","input":{"x":1}},` +
		`{"type":"tool_use","id":"c2","name":"b","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":[{"type":"text","text":"two"}]},` +
		`{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"one"}]},` +
		`{"type":"text","text":"next"}]}]`
	if got := asJSON(t, request.Messages); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestMissingToolResultIsSynthesized(t *testing.T) {
	body := `{"model":"m","input":[{"role":"user","content":"go"},
	  {"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
	  {"role":"user","content":"again"}]}`
	request := translateOK(t, mustConfig(t, `{"missing_tool_result_text":"gone"}`), body)
	got := asJSON(t, request.Messages[2])
	want := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"gone"}],"is_error":true},` +
		`{"type":"text","text":"again"}]}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// A call still open at the end of the input is answered too.
	tail := translateOK(t, mustConfig(t, `{}`), `{"model":"m","input":[{"role":"user","content":"go"},
	  {"type":"function_call","call_id":"c9","name":"a","arguments":"{}"}]}`)
	if last := tail.Messages[len(tail.Messages)-1]; last.Role != "user" || last.Content[0].ToolUseID != "c9" {
		t.Fatalf("an open call at the end was not answered: %s", asJSON(t, tail.Messages))
	}
	strict := mustConfig(t, `{"synthesize_missing_tool_results":false}`)
	if apiErr := translateFail(t, strict, body); !strings.Contains(apiErr.Message, "c1") {
		t.Fatalf("strict mode error should name the call: %v", apiErr)
	}
}

func TestInvalidArgumentsAreKept(t *testing.T) {
	body := `{"model":"m","input":[{"role":"user","content":"go"},
	  {"type":"function_call","call_id":"c1","name":"a","arguments":"not json"},
	  {"type":"function_call_output","call_id":"c1","output":"x"}]}`
	request := translateOK(t, mustConfig(t, `{}`), body)
	if got := string(request.Messages[1].Content[0].Input); got != `{"_invalid_arguments":"not json"}` {
		t.Fatalf("input = %s", got)
	}
}

func TestOrphanResultBecomesText(t *testing.T) {
	request := translateOK(t, mustConfig(t, `{}`), `{"model":"m","input":[
	  {"role":"user","content":"go"},{"type":"function_call_output","call_id":"zz","output":"late"}]}`)
	got := asJSON(t, request.Messages)
	if strings.Contains(got, "tool_result") || !strings.Contains(got, "Result of tool call zz:") || !strings.Contains(got, "late") {
		t.Fatalf("orphan result mishandled: %s", got)
	}
}

// Every tool_result must come before any other block in its user message, even
// when an unmatched result shows up while another call is still open.
func TestToolResultsLeadTheirMessage(t *testing.T) {
	body := `{"model":"m","input":[{"role":"user","content":"go"},
	  {"type":"function_call","call_id":"c1","name":"a","arguments":"{}"},
	  {"type":"function_call_output","call_id":"orphan","output":"stray"},
	  {"type":"function_call_output","call_id":"c1","output":"one"}]}`
	request := translateOK(t, mustConfig(t, `{}`), body)
	for index, message := range request.Messages {
		seenOther := false
		for _, block := range message.Content {
			if block.Type != "tool_result" {
				seenOther = true
			} else if seenOther {
				t.Fatalf("message %d has a tool_result after other content: %s", index, asJSON(t, request.Messages))
			}
		}
	}
	// Every tool_use is answered in the message right after it.
	for index, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type != "tool_use" {
				continue
			}
			if index+1 >= len(request.Messages) || request.Messages[index+1].Content[0].ToolUseID != block.ID {
				t.Fatalf("tool_use %s not answered next: %s", block.ID, asJSON(t, request.Messages))
			}
		}
	}
}

func TestSystemMessagePlacement(t *testing.T) {
	body := `{"model":"m","input":[
	  {"role":"developer","content":"first"},
	  {"role":"user","content":"u"},
	  {"role":"assistant","content":[{"type":"output_text","text":"a"}]},
	  {"role":"developer","content":"state"}]}`
	plain := translateOK(t, mustConfig(t, `{}`), body)
	if asJSON(t, plain.System) != `[{"type":"text","text":"first"}]` ||
		asJSON(t, plain.Messages[2]) != `{"role":"user","content":[{"type":"text","text":"state"}]}` {
		t.Fatalf("default placement wrong: system=%s messages=%s", asJSON(t, plain.System), asJSON(t, plain.Messages))
	}
	mid := translateOK(t, mustConfig(t, `{"default_profile":{"mid_conversation_system":true}}`), body)
	if asJSON(t, mid.Messages[2]) != `{"role":"system","content":[{"type":"text","text":"state"}]}` {
		t.Fatalf("mid-conversation system wrong: %s", asJSON(t, mid.Messages))
	}
}

func TestImagesAndFiles(t *testing.T) {
	body := `{"model":"m","input":[{"role":"user","content":[
	  {"type":"input_text","text":"look"},
	  {"type":"input_image","image_url":"data:image/png;base64,AAAA"},
	  {"type":"input_image","image_url":"https://x.test/a.png"},
	  {"type":"input_file","file_data":"data:application/pdf;base64,JVBERi0=","filename":"a.pdf"},
	  {"type":"input_file","file_data":"aGVsbG8=","filename":"n.txt"}]}]}`
	got := asJSON(t, translateOK(t, mustConfig(t, `{}`), body).Messages[0].Content)
	for _, want := range []string{
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`,
		`{"type":"image","source":{"type":"url","url":"https://x.test/a.png"}}`,
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="},"title":"a.pdf"}`,
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"hello"},"title":"n.txt"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	config := mustConfig(t, `{}`)
	translateFail(t, config, `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","file_id":"f"}]}]}`)
	translateFail(t, config, `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_data":"data:application/zip;base64,AA=="}]}]}`)
}

func TestToolsAndChoice(t *testing.T) {
	config := mustConfig(t, `{}`)
	base := `{"model":"m","input":"x","tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}},"strict":true}]`
	request := translateOK(t, config, base+`,"tool_choice":"required","parallel_tool_calls":false}`)
	if asJSON(t, request.Tools) != `[{"name":"f","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}},"strict":true}]` {
		t.Fatalf("tools = %s", asJSON(t, request.Tools))
	}
	if asJSON(t, request.ToolChoice) != `{"type":"any","disable_parallel_tool_use":true}` {
		t.Fatalf("choice = %s", asJSON(t, request.ToolChoice))
	}
	named := translateOK(t, config, base+`,"tool_choice":{"type":"function","name":"f"}}`)
	if asJSON(t, named.ToolChoice) != `{"type":"tool","name":"f"}` {
		t.Fatalf("named choice = %s", asJSON(t, named.ToolChoice))
	}
	none := translateOK(t, config, base+`,"tool_choice":"none","parallel_tool_calls":false}`)
	if asJSON(t, none.ToolChoice) != `{"type":"none"}` {
		t.Fatalf("none choice = %s", asJSON(t, none.ToolChoice))
	}
	if noTools := translateOK(t, config, `{"model":"m","input":"x","tool_choice":"auto"}`); noTools.ToolChoice != nil {
		t.Fatal("tool_choice without tools must be dropped")
	}
	translateFail(t, config, `{"model":"m","input":"x","tools":[{"type":"web_search"}]}`)
}

func TestThinkingModes(t *testing.T) {
	config := mustConfig(t, `{"models":[
	  {"match":"^adaptive","thinking":"adaptive","display":"summarized","default_effort":"medium"},
	  {"match":"^budget","thinking":"budget"}]}`)
	adaptive := translateOK(t, config, `{"model":"adaptive-1","input":"x","reasoning":{"effort":"xhigh"}}`)
	if asJSON(t, adaptive.Thinking) != `{"type":"adaptive","display":"summarized"}` ||
		asJSON(t, adaptive.OutputConfig) != `{"effort":"xhigh"}` {
		t.Fatalf("adaptive = %s %s", asJSON(t, adaptive.Thinking), asJSON(t, adaptive.OutputConfig))
	}
	fallback := translateOK(t, config, `{"model":"adaptive-1","input":"x"}`)
	if asJSON(t, fallback.OutputConfig) != `{"effort":"medium"}` {
		t.Fatalf("default effort not used: %s", asJSON(t, fallback.OutputConfig))
	}
	off := translateOK(t, config, `{"model":"adaptive-1","input":"x","reasoning":{"effort":"none"}}`)
	if off.Thinking != nil || off.OutputConfig != nil {
		t.Fatal("effort none must send no thinking")
	}
	budget := translateOK(t, config, `{"model":"budget-1","input":"x","max_output_tokens":4000,"reasoning":{"effort":"high","summary":"auto"}}`)
	if asJSON(t, budget.Thinking) != `{"type":"enabled","budget_tokens":3999,"display":"summarized"}` {
		t.Fatalf("budget = %s", asJSON(t, budget.Thinking))
	}
	if noEffort := translateOK(t, config, `{"model":"budget-1","input":"x"}`); noEffort.Thinking != nil {
		t.Fatal("budget mode with no effort must not enable thinking")
	}
	forced := translateOK(t, config, `{"model":"budget-1","input":"x","reasoning":{"effort":"low"},
	  "tools":[{"type":"function","name":"f","parameters":{}}],"tool_choice":"required"}`)
	if forced.Thinking != nil {
		t.Fatal("manual thinking must be skipped when tool use is forced")
	}
	tiny := translateOK(t, config, `{"model":"budget-1","input":"x","max_output_tokens":1024,"reasoning":{"effort":"high"}}`)
	if tiny.Thinking != nil {
		t.Fatal("a budget below the minimum must not be sent")
	}
	if plain := translateOK(t, config, `{"model":"other","input":"x","reasoning":{"effort":"high"}}`); plain.Thinking != nil {
		t.Fatal("thinking none profile must send nothing")
	}
}

func TestUnsupportedFieldPolicy(t *testing.T) {
	ignore := mustConfig(t, `{}`)
	result, apiErr := translateRequest(ignore, []byte(`{"model":"m","input":"x","top_logprobs":3,"store":true,"service_tier":"auto","metadata":{"a":"b"}}`))
	if apiErr != nil || len(result.Ignored) != 1 || result.Ignored[0] != "top_logprobs" {
		t.Fatalf("ignore policy: ignored=%v err=%v", result, apiErr)
	}
	reject := mustConfig(t, `{"unsupported_fields":"reject"}`)
	if apiErr := translateFail(t, reject, `{"model":"m","input":"x","top_logprobs":3}`); apiErr.Code != "unsupported_parameter" || apiErr.Param != "top_logprobs" {
		t.Fatalf("reject policy: %+v", apiErr)
	}
	for _, field := range []string{`"previous_response_id":"r"`, `"conversation":"c"`, `"background":true`} {
		if apiErr := translateFail(t, ignore, `{"model":"m","input":"x",`+field+`}`); apiErr.Status != 501 {
			t.Errorf("%s should be 501, got %d", field, apiErr.Status)
		}
	}
	translateOK(t, ignore, `{"model":"m","input":"x","background":false,"previous_response_id":null,"text":{"format":{"type":"text"}}}`)
	result, apiErr = translateRequest(ignore, []byte(`{"model":"m","input":"x","text":{"format":{"type":"json_schema","name":"n","schema":{}}}}`))
	if apiErr != nil || len(result.Ignored) != 1 || result.Ignored[0] != "text" {
		t.Fatalf("structured output must be reported as ignored: %v %v", result, apiErr)
	}
}

func TestBadRequests(t *testing.T) {
	config := mustConfig(t, `{}`)
	for name, body := range map[string]string{
		"not json":       `[`,
		"no model":       `{"input":"x"}`,
		"no input":       `{"model":"m"}`,
		"empty input":    `{"model":"m","input":[]}`,
		"bad item":       `{"model":"m","input":[{"type":"web_search_call"}]}`,
		"bad role":       `{"model":"m","input":[{"role":"robot","content":"x"}]}`,
		"call w/o id":    `{"model":"m","input":[{"type":"function_call","name":"a"}]}`,
		"bad part":       `{"model":"m","input":[{"role":"user","content":[{"type":"input_audio"}]}]}`,
		"system image":   `{"model":"m","input":[{"role":"system","content":[{"type":"input_image","image_url":"https://x/a.png"}]}]}`,
		"input a number": `{"model":"m","input":5}`,
	} {
		if apiErr := translateFail(t, config, body); apiErr.Status != 400 && apiErr.Status != 501 {
			t.Errorf("%s: status %d", name, apiErr.Status)
		}
	}
}

func TestThinkingDropsSamplingTheUpstreamRejects(t *testing.T) {
	config := mustConfig(t, `{"models":[
	  {"match":"^adaptive","thinking":"adaptive"},
	  {"match":"^budget","thinking":"budget"}]}`)
	for _, model := range []string{"adaptive-1", "budget-1"} {
		body := `{"model":"` + model + `","input":"x","temperature":0.5,"top_p":0.5,"reasoning":{"effort":"high"}}`
		result, apiErr := translateRequest(config, []byte(body))
		if apiErr != nil {
			t.Fatal(apiErr)
		}
		if result.Request.Thinking == nil || result.Request.Temperature != nil || result.Request.TopP != nil {
			t.Fatalf("%s: thinking=%v temperature=%v top_p=%v", model, result.Request.Thinking, result.Request.Temperature, result.Request.TopP)
		}
		if got := strings.Join(result.Ignored, ","); got != "temperature,top_p" {
			t.Fatalf("%s: ignored = %q", model, got)
		}
	}
	allowed := translateOK(t, config, `{"model":"budget-1","input":"x","top_p":0.97,"reasoning":{"effort":"high"}}`)
	if allowed.TopP == nil || *allowed.TopP != 0.97 {
		t.Fatal("top_p within 0.95..1 must be kept")
	}
	plain := translateOK(t, config, `{"model":"budget-1","input":"x","temperature":0.5,"top_p":0.5}`)
	if plain.Temperature == nil || plain.TopP == nil {
		t.Fatal("sampling must be kept when thinking is off")
	}
}

func TestBudgetThinkingSkippedWhenToolTurnHasNoThinkingBlock(t *testing.T) {
	config := mustConfig(t, `{"models":[
	  {"match":"^adaptive","thinking":"adaptive"},
	  {"match":"^budget","thinking":"budget"}]}`)
	loop := func(model, extra string) string {
		return `{"model":"` + model + `","reasoning":{"effort":"high"},"input":[
		  {"type":"message","role":"user","content":"go"},` + extra + `
		  {"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		  {"type":"function_call_output","call_id":"c1","output":"ok"}]}`
	}
	result, apiErr := translateRequest(config, []byte(loop("budget-1", "")))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if result.Request.Thinking != nil || strings.Join(result.Ignored, ",") != "reasoning" {
		t.Fatalf("thinking=%v ignored=%v", result.Request.Thinking, result.Ignored)
	}
	if adaptive := translateOK(t, config, loop("adaptive-1", "")); adaptive.Thinking == nil {
		t.Fatal("adaptive thinking has no leading-block requirement")
	}
	quoted, _ := json.Marshal(encodeThinkingBlocks([]anthropicBlock{{Type: "redacted_thinking", Data: "opaque"}}))
	replayed := translateOK(t, config, loop("budget-1", `{"type":"reasoning","summary":[],"encrypted_content":`+string(quoted)+`},`))
	if replayed.Thinking == nil {
		t.Fatal("a replayed thinking block must keep thinking on")
	}
	if noTools := translateOK(t, config, `{"model":"budget-1","input":"hi","reasoning":{"effort":"high"}}`); noTools.Thinking == nil {
		t.Fatal("a request without a tool turn must keep thinking")
	}
}

func TestFixedSamplingProfileDropsTemperatureAndTopP(t *testing.T) {
	config := mustConfig(t, `{"models":[{"match":"^fixed","thinking":"none","fixed_sampling":true}]}`)
	result, apiErr := translateRequest(config, []byte(`{"model":"fixed-1","input":"x","temperature":1,"top_p":0.99}`))
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if result.Request.Temperature != nil || result.Request.TopP != nil || strings.Join(result.Ignored, ",") != "temperature,top_p" {
		t.Fatalf("temperature=%v top_p=%v ignored=%v", result.Request.Temperature, result.Request.TopP, result.Ignored)
	}
	other := translateOK(t, config, `{"model":"other","input":"x","temperature":0.3,"top_p":0.5}`)
	if other.Temperature == nil || other.TopP == nil {
		t.Fatal("a profile without fixed_sampling must keep the values")
	}
}

func TestToolStrictIsPassedThrough(t *testing.T) {
	config := mustConfig(t, `{}`)
	body := func(strict string) string {
		return `{"model":"m","input":"x","tools":[{"type":"function","name":"f","parameters":{"type":"object"}` + strict + `}]}`
	}
	if got := asJSON(t, translateOK(t, config, body(`,"strict":true`)).Tools); !strings.Contains(got, `"strict":true`) {
		t.Fatalf("strict true lost: %s", got)
	}
	if got := asJSON(t, translateOK(t, config, body(`,"strict":false`)).Tools); !strings.Contains(got, `"strict":false`) {
		t.Fatalf("strict false lost: %s", got)
	}
	if got := asJSON(t, translateOK(t, config, body("")).Tools); strings.Contains(got, "strict") {
		t.Fatalf("strict must be absent when the tool has none: %s", got)
	}
}

func TestUserBecomesMetadataUserID(t *testing.T) {
	config := mustConfig(t, `{}`)
	got := asJSON(t, translateOK(t, config, `{"model":"m","input":"x","user":"u-123"}`).Metadata)
	if got != `{"user_id":"u-123"}` {
		t.Fatalf("metadata = %s", got)
	}
	if translateOK(t, config, `{"model":"m","input":"x"}`).Metadata != nil {
		t.Fatal("no user, no metadata")
	}
	long := strings.Repeat("a", MaxMetadataUserIDBytes+1)
	result, apiErr := translateRequest(config, []byte(`{"model":"m","input":"x","user":"`+long+`"}`))
	if apiErr != nil || result.Request.Metadata != nil || strings.Join(result.Ignored, ",") != "user" {
		t.Fatalf("an over-long user must be dropped and reported: %v %v", apiErr, result)
	}
}

func TestExtraBodyMetadataWinsOverUser(t *testing.T) {
	config := mustConfig(t, `{"default_profile":{"extra_body":{"metadata":{"user_id":"fixed"}}}}`)
	request := translateOK(t, config, `{"model":"m","input":"x","user":"from-client"}`)
	body, err := encodeRequestBody(config, &request)
	if err != nil || !strings.Contains(string(body), `"metadata":{"user_id":"fixed"}`) || strings.Contains(string(body), "from-client") {
		t.Fatalf("extra_body.metadata must win: %s %v", body, err)
	}
}

func TestServerToolRejectionNamesTheType(t *testing.T) {
	apiErr := translateFail(t, mustConfig(t, `{}`), `{"model":"m","input":"x","tools":[{"type":"web_search"}]}`)
	for _, want := range []string{"web_search", "server tools", "only function tools"} {
		if !strings.Contains(apiErr.Message, want) {
			t.Fatalf("message %q lacks %q", apiErr.Message, want)
		}
	}
}
