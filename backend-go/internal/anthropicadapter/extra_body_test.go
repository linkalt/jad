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

const extraBodyFields = `{"cache_control":{"type":"ephemeral","ttl":"1h"},"top_k":40,"metadata":{"user_id":"u1"},"stop_sequences":["END"]}`

const defaultExtraBody = `,"default_profile":{"extra_body":` + extraBodyFields + `}`

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
	adapter := adapterWithUpstream(t, upstream.URL, "", defaultExtraBody)
	status, _, raw := post(t, adapter.URL+"/v1/responses", `{"model":"claude-x","input":"hi"}`, map[string]string{"Authorization": "Bearer k"})
	if status != 200 {
		t.Fatalf("status %d: %s", status, raw)
	}
	checkExtraBody(t, (*calls)[0].Body)
}

func TestExtraBodyReachesStreamingUpstream(t *testing.T) {
	upstream, body := captureStreamBody(t)
	adapter := adapterWithUpstream(t, upstream.URL, "", defaultExtraBody)
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
	config := mustConfig(t, `{"default_profile":{"extra_body":{"big":12345678901234567890,"f":1.10}}}`)
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
		_, err := LoadConfig(writeConfig(t, `{"models":[{"match":"^a","extra_body":{"`+key+`":1}}]}`))
		if err == nil || !strings.Contains(err.Error(), "models[0].extra_body."+key) {
			t.Errorf("owned key %s in a model profile: error %v", key, err)
		}
		_, err = LoadConfig(writeConfig(t, `{"default_profile":{"extra_body":{"`+key+`":1}}}`))
		if err == nil || !strings.Contains(err.Error(), "default_profile.extra_body."+key) {
			t.Errorf("owned key %s in default_profile: error %v", key, err)
		}
	}
	_, err := LoadConfig(writeConfig(t, `{"cache":{"automatic":true},"models":[{"match":"^a"},{"match":"^b","extra_body":{"cache_control":{"type":"ephemeral"}}}]}`))
	if err == nil || !strings.Contains(err.Error(), "models[1].extra_body.cache_control") || !strings.Contains(err.Error(), "cache.automatic") {
		t.Errorf("cache_control with cache.automatic (model): error %v", err)
	}
	_, err = LoadConfig(writeConfig(t, `{"cache":{"automatic":true},"default_profile":{"extra_body":{"cache_control":{"type":"ephemeral"}}}}`))
	if err == nil || !strings.Contains(err.Error(), "default_profile.extra_body.cache_control") {
		t.Errorf("cache_control with cache.automatic (default): error %v", err)
	}
	if _, err := LoadConfig(writeConfig(t, `{"models":[{"match":"^a","extra_body":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}]}`)); err != nil {
		t.Errorf("cache_control alone rejected: %v", err)
	}
	if _, err := LoadConfig(writeConfig(t, `{"default_profile":{"extra_body":{"top_k":"x"}}}`)); err != nil {
		t.Errorf("a plain extra field rejected: %v", err)
	}
}

func TestUpstreamExtraBodyIsRejectedAsUnknown(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, `{"upstream":{"extra_body":{"top_k":1}}}`))
	if err == nil || !strings.Contains(err.Error(), "extra_body") {
		t.Fatalf("upstream.extra_body must be an unknown key, got %v", err)
	}
}

func TestExtraBodyFollowsTheMatchedProfile(t *testing.T) {
	config := mustConfig(t, `{"models":[
		{"match":"^claude-a","extra_body":{"top_k":1}},
		{"match":"^claude-b","extra_body":{"top_k":2,"service_tier":"auto"}},
		{"match":"^claude-c"}],
		"default_profile":{"extra_body":{"top_k":9}}}`)
	body := func(model string) string {
		request := translateOK(t, config, `{"model":"`+model+`","input":"x"}`)
		payload, err := encodeRequestBody(config, &request)
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	for model, want := range map[string]string{"claude-a": `"top_k":1`, "claude-b": `"top_k":2`, "other": `"top_k":9`} {
		if got := body(model); !strings.Contains(got, want) {
			t.Errorf("%s: %s lacks %s", model, got, want)
		}
	}
	if got := body("claude-a"); strings.Contains(got, "service_tier") {
		t.Errorf("profiles must not merge: %s", got)
	}
	if got := body("claude-b"); !strings.Contains(got, `"service_tier":"auto"`) {
		t.Errorf("claude-b lost its own field: %s", got)
	}
	request := translateOK(t, config, `{"model":"claude-c","input":"x"}`)
	plain, _ := json.Marshal(&request)
	if got := body("claude-c"); got != string(plain) {
		t.Errorf("a profile without extra_body must leave the body unchanged:\n got %s\nwant %s", got, plain)
	}
}

func TestSchemaHandlesExtraBody(t *testing.T) {
	schema := compileSchema(t)
	check := func(text string) error {
		return schema.Validate(mustUnmarshalJSON(t, text))
	}
	if err := check(`{"default_profile":{"extra_body":{"cache_control":{"type":"ephemeral","ttl":"1h"},"top_k":5}},"models":[{"match":"^a","extra_body":{"top_k":5}}]}`); err != nil {
		t.Errorf("schema rejected extra_body: %v", err)
	}
	if check(`{"models":[{"match":"^a","extra_body":{"messages":[]}}]}`) == nil {
		t.Error("schema accepted an owned key in extra_body")
	}
	if check(`{"upstream":{"extra_body":{"top_k":5}}}`) == nil {
		t.Error("schema accepted upstream.extra_body")
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
