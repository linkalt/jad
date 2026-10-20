package anthropicadapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const extraBodyConfig = `,"extra_body":{"cache_control":{"type":"ephemeral","ttl":"1h"},"top_k":40,"metadata":{"user_id":"u1"},"stop_sequences":["END"]}`

func captureStreamBody(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var first string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		if first == "" {
			first = string(raw)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
	}))
	t.Cleanup(server.Close)
	return server, func() string { mu.Lock(); defer mu.Unlock(); return first }
}

func adapterWithUpstream(t *testing.T, upstreamURL, upstreamExtra, top string) *httptest.Server {
	t.Helper()
	config := mustConfig(t, `{"upstream":{"base_url":"`+upstreamURL+`"`+upstreamExtra+`}`+top+`}`)
	server := httptest.NewServer(NewHandler(config, nil))
	t.Cleanup(server.Close)
	return server
}

func checkExtraBody(t *testing.T, body string) {
	t.Helper()
	var sent map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("outbound body is not JSON: %v: %s", err, body)
	}
	want := map[string]string{
		"cache_control":  `{"type":"ephemeral","ttl":"1h"}`,
		"top_k":          `40`,
		"metadata":       `{"user_id":"u1"}`,
		"stop_sequences": `["END"]`,
	}
	for key, value := range want {
		if string(sent[key]) != value {
			t.Errorf("%s = %s, want %s (body %s)", key, sent[key], value, body)
		}
	}
	if string(sent["model"]) != `"claude-x"` || sent["messages"] == nil {
		t.Errorf("adapter-built fields lost: %s", body)
	}
}

func TestExtraBodyReachesNonStreamingUpstream(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterWithUpstream(t, upstream.URL, extraBodyConfig, "")
	status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer k"})
	if status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	checkExtraBody(t, (*calls)[0].Body)
}

func TestExtraBodyReachesStreamingUpstream(t *testing.T) {
	upstream, body := captureStreamBody(t)
	adapter := adapterWithUpstream(t, upstream.URL, extraBodyConfig, "")
	post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi","stream":true}`, map[string]string{"Authorization": "Bearer k"})
	sent := body()
	checkExtraBody(t, sent)
	if !strings.Contains(sent, `"stream":true`) {
		t.Errorf("stream flag lost: %s", sent)
	}
}

func TestWithoutExtraBodyTheOutboundBodyIsUnchanged(t *testing.T) {
	upstream, calls := fakeAnthropic(t, 200, messageBody, nil)
	adapter := adapterWithUpstream(t, upstream.URL, "", "")
	post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer k"})
	config := mustConfig(t, `{}`)
	request := translateOK(t, config, `{"model":"claude-x","input":"hi"}`)
	want, _ := json.Marshal(&request)
	if (*calls)[0].Body != string(want) {
		t.Fatalf("body changed without extra_body:\n got %s\nwant %s", (*calls)[0].Body, want)
	}
}

func TestExtraBodyKeepsRawNumbers(t *testing.T) {
	config := mustConfig(t, `{"upstream":{"extra_body":{"big":12345678901234567890,"f":1.10}}}`)
	request := translateOK(t, config, `{"model":"m","input":"x"}`)
	payload, err := encodeRequestBody(config, &request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"big":12345678901234567890`) || !strings.Contains(string(payload), `"f":1.10`) {
		t.Fatalf("raw values changed: %s", payload)
	}
}

func TestExtraBodyConfigValidation(t *testing.T) {
	for _, key := range ownedRequestFields {
		_, err := LoadConfig(writeConfig(t, `{"upstream":{"extra_body":{"`+key+`":1}}}`))
		if err == nil || !strings.Contains(err.Error(), "extra_body."+key) {
			t.Errorf("owned key %s: error %v", key, err)
		}
	}
	_, err := LoadConfig(writeConfig(t, `{"cache":{"automatic":true},"upstream":{"extra_body":{"cache_control":{"type":"ephemeral"}}}}`))
	if err == nil || !strings.Contains(err.Error(), "cache.automatic") {
		t.Errorf("cache_control with cache.automatic: error %v", err)
	}
	if _, err := LoadConfig(writeConfig(t, `{"upstream":{"extra_body":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}}`)); err != nil {
		t.Errorf("cache_control alone rejected: %v", err)
	}
	if _, err := LoadConfig(writeConfig(t, `{"upstream":{"extra_body":{"top_k":"x"}}}`)); err != nil {
		t.Errorf("a plain extra field rejected: %v", err)
	}
}

func TestSchemaHandlesExtraBody(t *testing.T) {
	schema := compileSchema(t)
	check := func(text string) error {
		instance := mustUnmarshalJSON(t, text)
		return schema.Validate(instance)
	}
	if err := check(`{"upstream":{"extra_body":{"cache_control":{"type":"ephemeral","ttl":"1h"},"top_k":5}}}`); err != nil {
		t.Errorf("schema rejected extra_body: %v", err)
	}
	if check(`{"upstream":{"extra_body":{"messages":[]}}}`) == nil {
		t.Error("schema accepted an owned key in extra_body")
	}
}

func mustUnmarshalJSON(t *testing.T, text string) any {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return instance
}
