package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// pointsOf returns the data points of metric name in a pushed request, each as its
// attributes (key=value, sorted) and the point itself.
func pointsOf(t *testing.T, request otlpMetricsRequest, name string) map[string]any {
	t.Helper()
	points := map[string]any{}
	for _, metric := range request.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		if metric.Name != name {
			continue
		}
		if metric.Sum != nil {
			for _, point := range metric.Sum.DataPoints {
				points[attributeKey(point.Attributes)] = point
			}
		}
		if metric.Histogram != nil {
			for _, point := range metric.Histogram.DataPoints {
				points[attributeKey(point.Attributes)] = point
			}
		}
	}
	return points
}

func TestMetricsRecordRunsModelRequestsAndToolCalls(t *testing.T) {
	settings := sessionfs.DefaultMetricsSettings()
	recorder := newMetricsRecorder(settings, time.Now())
	restore := InstallMetrics(recorder)
	defer restore()

	ctx, run := StartRun(context.Background(), "session-1", "dev", "turn-1", "")
	_, model := StartModelRequest(ctx, "gpt-test", 0)
	EndModelRequest(model, json.RawMessage(`{"response":{"usage":{"input_tokens":300,"output_tokens":20}}}`), nil)
	_, failed := StartModelRequest(ctx, "gpt-test", 1)
	EndModelRequest(failed, nil, context.DeadlineExceeded)
	_, read := StartToolCall(ctx, json.RawMessage(`{"name":"read","call_id":"c1"}`))
	EndToolCall(read, json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":"{\"id\":\"c1\",\"status\":\"not_found\",\"summary\":\"no file\"}\n"}`), nil)
	_, bash := StartToolCall(ctx, json.RawMessage(`{"name":"bash","call_id":"c2"}`))
	EndToolCall(bash, nil, errors.New("boom"))
	_, mcp := StartToolCall(ctx, json.RawMessage(`{"name":"mcp__docs__search","call_id":"c3"}`))
	EndToolCall(mcp, json.RawMessage(`{"type":"function_call_output","call_id":"c3","output":"plain text"}`), nil)

	active := pointsOf(t, recorder.snapshot("jad", time.Now()), metricRunsActive)
	if point := active[""].(otlpNumberPoint); point.AsDouble != 1 {
		t.Fatalf("active runs during the run = %v", point.AsDouble)
	}
	EndRun(run, "completed", nil)
	_, scheduled := StartRun(context.Background(), "session-2", "", "turn-2", "trigger")
	EndRun(scheduled, "failed", errors.New("x"))

	request := recorder.snapshot("jad-test", time.Now())
	if attributes := request.ResourceMetrics[0].Resource.Attributes; *attributes[0].Value.StringValue != "jad-test" {
		t.Fatalf("resource = %+v", attributes)
	}
	runs := pointsOf(t, request, metricRuns)
	if len(runs) != 2 || runs["jad.run.source=web\x00jad.run.status=completed\x00"].(otlpNumberPoint).AsDouble != 1 || runs["jad.run.source=trigger\x00jad.run.status=failed\x00"] == nil {
		t.Fatalf("runs = %+v", runs)
	}
	if point := pointsOf(t, request, metricRunsActive)[""].(otlpNumberPoint); point.AsDouble != 0 {
		t.Fatalf("active runs after = %v", point.AsDouble)
	}
	if len(pointsOf(t, request, metricRunDuration)) != 2 {
		t.Fatal("run durations missing")
	}
	durations := pointsOf(t, request, metricModelDuration)
	if durations["error.type=deadline_exceeded\x00gen_ai.operation.name=chat\x00gen_ai.request.model=gpt-test\x00"] == nil || durations["gen_ai.operation.name=chat\x00gen_ai.request.model=gpt-test\x00"] == nil {
		t.Fatalf("model durations = %+v", durations)
	}
	tokens := pointsOf(t, request, metricTokenUsage)
	input := tokens["gen_ai.operation.name=chat\x00gen_ai.request.model=gpt-test\x00gen_ai.token.type=input\x00"].(otlpHistogramPoint)
	if input.Count != "1" || input.Sum != 300 || input.Min != 300 || input.Max != 300 || len(input.BucketCounts) != len(settings.TokenBuckets)+1 {
		t.Fatalf("input tokens = %+v", input)
	}
	// 300 tokens fall in the (256, 1024] bucket, the sixth.
	if input.BucketCounts[5] != "1" {
		t.Fatalf("input token buckets = %v", input.BucketCounts)
	}
	calls := pointsOf(t, request, metricToolCalls)
	for _, key := range []string{"gen_ai.tool.name=read\x00jad.tool.status=not_found\x00", "gen_ai.tool.name=bash\x00jad.tool.status=error\x00", "gen_ai.tool.name=mcp__docs__search\x00jad.tool.status=ok\x00"} {
		if calls[key] == nil {
			t.Errorf("tool calls miss %q: %v", key, calls)
		}
	}
	encoded, _ := json.Marshal(request)
	for _, private := range []string{"session-1", "turn-1", "dev", "no file"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("the metrics carry %q: %s", private, encoded)
		}
	}
}

func TestSetupMetricsPushesOverOTLPJSONAndAtShutdown(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	var headers []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.URL.Path+" "+string(body))
		headers = append(headers, r.Header.Get("Content-Type")+" "+r.Header.Get("X-Key"))
		mu.Unlock()
	}))
	defer collector.Close()
	t.Setenv(envMetricsHeaders, "X-Key=secret")
	settings := sessionfs.DefaultMetricsSettings()
	settings.Enabled, settings.Endpoint, settings.ExportIntervalMS, settings.ServiceName = true, collector.URL, 1000, "jad-metrics"
	shutdown, err := SetupMetrics(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meter.Load() == nil {
		t.Fatal("no recorder installed")
	}
	_, run := StartRun(context.Background(), "s", "p", "t", "cli")
	EndRun(run, "completed", nil)
	if err := ShutdownMetrics(shutdown, settings); err != nil {
		t.Fatal(err)
	}
	if meter.Load() != nil {
		t.Fatal("shutdown left the recorder installed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("nothing was pushed")
	}
	last := bodies[len(bodies)-1]
	if !strings.HasPrefix(last, "/v1/metrics ") || !strings.Contains(last, `"name":"jad.runs"`) || !strings.Contains(last, `"stringValue":"jad-metrics"`) || !strings.Contains(last, `"aggregationTemporality":2`) || !strings.Contains(last, `"isMonotonic":true`) {
		t.Fatalf("push = %s", last)
	}
	if headers[len(headers)-1] != "application/json secret" {
		t.Fatalf("headers = %v", headers)
	}
}

func TestSetupMetricsDisabledInstallsNothing(t *testing.T) {
	shutdown, err := SetupMetrics(sessionfs.MetricsSettings{}, nil)
	if err != nil || meter.Load() != nil {
		t.Fatalf("disabled metrics: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := SetupMetrics(sessionfs.MetricsSettings{Enabled: true, Endpoint: "ftp://x"}, nil); err == nil {
		t.Fatal("a bad endpoint was accepted")
	}
}

func TestMetricsURLFollowsTheOTLPVariables(t *testing.T) {
	lookup := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	for _, tc := range []struct {
		configured string
		env        map[string]string
		want       string
	}{
		{"http://a:4318/", nil, "http://a:4318/v1/metrics"},
		{"", map[string]string{envMetricsEndpoint: "http://m/custom", envEndpoint: "http://b"}, "http://m/custom"},
		{"", map[string]string{envEndpoint: "http://b/"}, "http://b/v1/metrics"},
		{"", nil, "http://localhost:4318/v1/metrics"},
	} {
		if got := metricsURL(tc.configured, lookup(tc.env)); got != tc.want {
			t.Errorf("metricsURL(%q, %v) = %q, want %q", tc.configured, tc.env, got, tc.want)
		}
	}
}
