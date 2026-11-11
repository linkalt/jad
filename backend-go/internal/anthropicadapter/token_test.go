package anthropicadapter

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

const simpleRequest = `{"model":"claude-x","input":"hi"}`

func TestTheCallersTokenIsRequired(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	if status, _, _ := post(t, adapter.URL+"/v1/responses", simpleRequest, nil); status != 401 || len(*calls) != 0 {
		t.Fatalf("status %d calls %d", status, len(*calls))
	}
}

func TestExtraHeadersAreSentOnEveryRequest(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","extra_headers":{"X-App":"cli","User-Agent":"ua/1"}}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	post(t, server.URL+"/v1/responses", simpleRequest, map[string]string{"Authorization": "Bearer t"})
	call := (*calls)[len(*calls)-1]
	if call.Header.Get("X-App") != "cli" || call.Header.Get("User-Agent") != "ua/1" || call.Header.Get("Authorization") != "Bearer t" {
		t.Fatalf("headers %v", call.Header)
	}
}

const prefixSentence = "You are the support assistant of Example Corp."

func sentSystem(t *testing.T, body string) []struct {
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control"`
} {
	t.Helper()
	var sent struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	return sent.System
}

func TestSystemPrefixIsTheFirstBlockAndTheCallersInstructionsFollow(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","system_prefix":`+strconvQuote(prefixSentence)+`},"cache":{"system":true}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	post(t, server.URL+"/v1/responses", `{"model":"claude-x","instructions":"Be brief.","input":[{"role":"developer","content":"also this"},{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer t"})
	system := sentSystem(t, (*calls)[0].Body)
	if len(system) != 3 || system[0].Text != prefixSentence || system[1].Text != "Be brief." || system[2].Text != "also this" {
		t.Fatalf("system blocks wrong: %+v", system)
	}
	// The prefix is a block of its own, never merged into the caller's text.
	if strings.Contains(system[1].Text, "Example Corp") {
		t.Fatal("the prefix was merged into the instructions")
	}
	// The cache marker still sits on the last block, so the cached prefix covers all three.
	if len(system[0].CacheControl) != 0 || len(system[2].CacheControl) == 0 {
		t.Fatalf("cache marker misplaced: %+v", system)
	}
}

func TestSystemPrefixAloneWhenTheCallerSendsNoInstructions(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstream.URL+`","system_prefix":`+strconvQuote(prefixSentence)+`}}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	post(t, server.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer t"})
	if system := sentSystem(t, (*calls)[0].Body); len(system) != 1 || system[0].Text != prefixSentence {
		t.Fatalf("system blocks wrong: %+v", system)
	}
}

func TestNoSystemPrefixByDefault(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer t"})
	if strings.Contains((*calls)[0].Body, `"system"`) {
		t.Fatalf("a system field was sent without instructions or a prefix: %s", (*calls)[0].Body)
	}
}

func strconvQuote(s string) string { raw, _ := json.Marshal(s); return string(raw) }

// The adapter holds no credential, only forwards the caller's, so a browser Origin is not
// refused: the page would only spend its own key.
func TestBrowserOriginIsAccepted(t *testing.T) {
	upstream, _ := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterFor(t, upstream.URL, "")
	headers := map[string]string{"Authorization": "Bearer t", "Origin": "https://example.test"}
	if status, _, raw := post(t, adapter.URL+"/v1/responses", simpleRequest, headers); status != 200 {
		t.Fatalf("status %d body %s", status, raw)
	}
}
