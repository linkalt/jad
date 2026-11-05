package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"

	"jad/backend-go/internal/sessionfs"
)

// Metrics: server.metrics records counts, durations and token usage of runs, model requests
// and tool calls at the same points the spans start and end, and pushes them over
// OTLP/HTTP JSON every export_interval_ms, cumulative. Attributes are model names, tool
// names and outcomes only, so the number of series stays small: no session, profile or
// turn ids, prompts, answers, arguments or output.

const (
	metricsPath           = "/v1/metrics"
	envMetricsEndpoint    = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envMetricsHeaders     = "OTEL_EXPORTER_OTLP_METRICS_HEADERS"
	metricRuns            = "jad.runs"
	metricRunDuration     = "jad.run.duration"
	metricRunsActive      = "jad.runs.active"
	metricModelDuration   = "gen_ai.client.operation.duration"
	metricTokenUsage      = "gen_ai.client.token.usage"
	metricToolCalls       = "jad.tool.calls"
	metricToolDuration    = "jad.tool.duration"
	metricKindSum         = "sum"
	metricKindUpDown      = "updown"
	metricKindHistogram   = "histogram"
	temporalityCumulative = 2
)

// metricDescriptions are the description and unit of each instrument.
var metricDescriptions = map[string][2]string{
	metricRuns:          {"Runs that ended, by status and source.", "{run}"},
	metricRunDuration:   {"How long runs took, by status.", "s"},
	metricRunsActive:    {"Runs in progress.", "{run}"},
	metricModelDuration: {"How long model requests took.", "s"},
	metricTokenUsage:    {"Tokens used by model requests, by token type.", "{token}"},
	metricToolCalls:     {"Tool calls that ended, by tool and result status.", "{call}"},
	metricToolDuration:  {"How long tool calls took, by tool.", "s"},
}

// meter is the recorder Setup installs; nil while metrics are off.
var meter atomic.Pointer[metricsRecorder]

// metricsRecorder holds every series in memory.
type metricsRecorder struct {
	mu              sync.Mutex
	start           time.Time
	durationBuckets []float64
	tokenBuckets    []float64
	series          map[string]*metricSeries
}

// metricSeries is one instrument with one attribute set.
type metricSeries struct {
	name       string
	kind       string
	attributes [][2]string
	value      float64
	buckets    []float64
	counts     []uint64
	count      uint64
	sum        float64
	min, max   float64
}

func newMetricsRecorder(settings sessionfs.MetricsSettings, now time.Time) *metricsRecorder {
	return &metricsRecorder{
		start:           now,
		durationBuckets: slices.Clone(settings.DurationBuckets),
		tokenBuckets:    slices.Clone(settings.TokenBuckets),
		series:          map[string]*metricSeries{},
	}
}

// seriesFor returns the series of name and attributes, creating it. attributes are
// name/value pairs; empty values are left out. Callers hold mu.
func (m *metricsRecorder) seriesFor(name, kind string, buckets []float64, attributes ...string) *metricSeries {
	var pairs [][2]string
	for index := 0; index+1 < len(attributes); index += 2 {
		if attributes[index+1] != "" {
			pairs = append(pairs, [2]string{attributes[index], attributes[index+1]})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	var key strings.Builder
	key.WriteString(name)
	for _, pair := range pairs {
		key.WriteString("\x00" + pair[0] + "=" + pair[1])
	}
	series, exists := m.series[key.String()]
	if !exists {
		series = &metricSeries{name: name, kind: kind, attributes: pairs, buckets: buckets}
		if kind == metricKindHistogram {
			series.counts = make([]uint64, len(buckets)+1)
		}
		m.series[key.String()] = series
	}
	return series
}

func (m *metricsRecorder) add(name, kind string, delta float64, attributes ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seriesFor(name, kind, nil, attributes...).value += delta
}

func (m *metricsRecorder) observe(name string, buckets []float64, value float64, attributes ...string) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	series := m.seriesFor(name, metricKindHistogram, buckets, attributes...)
	index := sort.SearchFloat64s(buckets, value)
	series.counts[index]++
	if series.count == 0 || value < series.min {
		series.min = value
	}
	if series.count == 0 || value > series.max {
		series.max = value
	}
	series.count++
	series.sum += value
}

// metricSpan is the span the Start functions return while metrics are on: the trace
// span (possibly the inactive one) plus what the End function records.
type metricSpan struct {
	trace.Span
	recorder *metricsRecorder
	started  time.Time
	model    string
	tool     string
	source   string
}

// withMetrics wraps span when metrics are on.
func withMetrics(span trace.Span, fill func(*metricSpan)) trace.Span {
	recorder := meter.Load()
	if recorder == nil {
		return span
	}
	wrapped := &metricSpan{Span: span, recorder: recorder, started: time.Now()}
	if fill != nil {
		fill(wrapped)
	}
	return wrapped
}

func metricsOf(span trace.Span) *metricSpan {
	wrapped, _ := span.(*metricSpan)
	return wrapped
}

func (s *metricSpan) seconds() float64 { return time.Since(s.started).Seconds() }

func recordRunStart(span trace.Span) {
	if wrapped := metricsOf(span); wrapped != nil {
		wrapped.recorder.add(metricRunsActive, metricKindUpDown, 1)
	}
}

func recordRunEnd(span trace.Span, status string) {
	wrapped := metricsOf(span)
	if wrapped == nil {
		return
	}
	recorder := wrapped.recorder
	recorder.add(metricRunsActive, metricKindUpDown, -1)
	recorder.add(metricRuns, metricKindSum, 1, "jad.run.status", status, "jad.run.source", wrapped.source)
	recorder.observe(metricRunDuration, recorder.durationBuckets, wrapped.seconds(), "jad.run.status", status)
}

func recordModelRequest(span trace.Span, event json.RawMessage, err error) {
	wrapped := metricsOf(span)
	if wrapped == nil {
		return
	}
	recorder := wrapped.recorder
	errorType := ""
	if err != nil {
		errorType = errorKind(err)
	}
	recorder.observe(metricModelDuration, recorder.durationBuckets, wrapped.seconds(), "gen_ai.operation.name", "chat", "gen_ai.request.model", wrapped.model, "error.type", errorType)
	if err != nil {
		return
	}
	var payload struct {
		Response struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(event, &payload) != nil || payload.Response.Usage == nil {
		return
	}
	usage := payload.Response.Usage
	recorder.observe(metricTokenUsage, recorder.tokenBuckets, float64(usage.InputTokens), "gen_ai.operation.name", "chat", "gen_ai.request.model", wrapped.model, "gen_ai.token.type", "input")
	recorder.observe(metricTokenUsage, recorder.tokenBuckets, float64(usage.OutputTokens), "gen_ai.operation.name", "chat", "gen_ai.request.model", wrapped.model, "gen_ai.token.type", "output")
}

func recordToolCall(span trace.Span, output json.RawMessage, err error) {
	wrapped := metricsOf(span)
	if wrapped == nil {
		return
	}
	status := "error"
	if err == nil {
		status = toolResultStatus(output)
	}
	wrapped.recorder.add(metricToolCalls, metricKindSum, 1, "gen_ai.tool.name", wrapped.tool, "jad.tool.status", status)
	wrapped.recorder.observe(metricToolDuration, wrapped.recorder.durationBuckets, wrapped.seconds(), "gen_ai.tool.name", wrapped.tool)
}

// toolResultStatus reads the status of a native tool result (the first line of the output
// text is {"id","status","summary"}); other output counts as ok.
func toolResultStatus(output json.RawMessage) string {
	var item struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(output, &item) != nil {
		return "ok"
	}
	line, _, _ := strings.Cut(item.Output, "\n")
	var header struct {
		Status string `json:"status"`
	}
	if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &header) != nil || header.Status == "" {
		return "ok"
	}
	return header.Status
}

// SetupMetrics installs the metrics recorder for settings and starts pushing every
// export_interval_ms. The returned function stops the pushing and sends the last values;
// call it before the process exits. With metrics disabled it does nothing.
func SetupMetrics(settings sessionfs.MetricsSettings, logger *slog.Logger) (func(context.Context) error, error) {
	if !settings.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := newMetricsExporter(settings, os.Getenv)
	if err != nil {
		return nil, err
	}
	recorder := newMetricsRecorder(settings, time.Now())
	restore := InstallMetrics(recorder)
	stop := make(chan struct{})
	done := make(chan struct{})
	export := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, time.Duration(settings.ExportTimeoutMS)*time.Millisecond)
		defer cancel()
		return exporter.export(ctx, recorder.snapshot(settings.ServiceName, time.Now()))
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Duration(settings.ExportIntervalMS) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := export(context.Background()); err != nil && logger != nil {
					logger.Warn("metrics export failed", slog.Any("error", err))
				}
			}
		}
	}()
	var once sync.Once
	return func(ctx context.Context) error {
		var err error
		once.Do(func() {
			close(stop)
			<-done
			err = export(ctx)
			restore()
		})
		return err
	}, nil
}

// InstallMetrics makes recorder the one the spans record into; restore puts the previous
// one back. Tests install a recorder of their own.
func InstallMetrics(recorder *metricsRecorder) (restore func()) {
	previous := meter.Swap(recorder)
	return func() { meter.Store(previous) }
}

// ShutdownMetrics calls shutdown with a deadline of export_timeout_ms.
func ShutdownMetrics(shutdown func(context.Context) error, settings sessionfs.MetricsSettings) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(settings.ExportTimeoutMS)*time.Millisecond)
	defer cancel()
	return shutdown(ctx)
}

// metricsExporter posts OTLP/JSON metrics.
type metricsExporter struct {
	url     string
	headers http.Header
	client  *http.Client
}

func newMetricsExporter(settings sessionfs.MetricsSettings, lookup func(string) string) (*metricsExporter, error) {
	target := metricsURL(settings.Endpoint, lookup)
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("server.metrics: the OTLP metrics endpoint %q is not an http(s) URL", target)
	}
	headers := http.Header{}
	for _, name := range []string{envHeaders, envMetricsHeaders} {
		if err := parseHeaders(lookup(name), headers); err != nil {
			return nil, fmt.Errorf("server.metrics: %s: %w", name, err)
		}
	}
	headers.Set("Content-Type", "application/json")
	return &metricsExporter{url: target, headers: headers, client: &http.Client{}}, nil
}

func metricsURL(configured string, lookup func(string) string) string {
	if configured != "" {
		return strings.TrimRight(configured, "/") + metricsPath
	}
	if value := strings.TrimSpace(lookup(envMetricsEndpoint)); value != "" {
		return value
	}
	if value := strings.TrimSpace(lookup(envEndpoint)); value != "" {
		return strings.TrimRight(value, "/") + metricsPath
	}
	return defaultEndpoint + metricsPath
}

func (e *metricsExporter) export(ctx context.Context, request otlpMetricsRequest) error {
	if len(request.ResourceMetrics) == 0 || len(request.ResourceMetrics[0].ScopeMetrics[0].Metrics) == 0 {
		return nil
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	post.Header = e.headers.Clone()
	response, err := e.client.Do(post)
	if err != nil {
		return fmt.Errorf("export metrics: %w", err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyBytes))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("export metrics: the collector answered HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}

// The OTLP/JSON metric shapes. 64-bit integers are decimal strings, as the OTLP JSON
// mapping requires.
type otlpMetricsRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}

type otlpMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	Sum         *otlpSum       `json:"sum,omitempty"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
}

type otlpSum struct {
	DataPoints             []otlpNumberPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

type otlpNumberPoint struct {
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	AsDouble          float64        `json:"asDouble"`
}

type otlpHistogram struct {
	DataPoints             []otlpHistogramPoint `json:"dataPoints"`
	AggregationTemporality int                  `json:"aggregationTemporality"`
}

type otlpHistogramPoint struct {
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	Count             string         `json:"count"`
	Sum               float64        `json:"sum"`
	BucketCounts      []string       `json:"bucketCounts"`
	ExplicitBounds    []float64      `json:"explicitBounds"`
	Min               float64        `json:"min"`
	Max               float64        `json:"max"`
}

// snapshot is every series as one OTLP request, metrics in name order.
func (m *metricsRecorder) snapshot(serviceName string, now time.Time) otlpMetricsRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	start, at := strconv.FormatInt(m.start.UnixNano(), 10), strconv.FormatInt(now.UnixNano(), 10)
	byName := map[string]*otlpMetric{}
	var names []string
	for _, series := range m.series {
		metric, exists := byName[series.name]
		if !exists {
			description := metricDescriptions[series.name]
			metric = &otlpMetric{Name: series.name, Description: description[0], Unit: description[1]}
			byName[series.name] = metric
			names = append(names, series.name)
		}
		attributes := make([]otlpKeyValue, 0, len(series.attributes))
		for _, pair := range series.attributes {
			value := pair[1]
			attributes = append(attributes, otlpKeyValue{Key: pair[0], Value: otlpValue{StringValue: &value}})
		}
		switch series.kind {
		case metricKindHistogram:
			if metric.Histogram == nil {
				metric.Histogram = &otlpHistogram{AggregationTemporality: temporalityCumulative}
			}
			counts := make([]string, len(series.counts))
			for index, count := range series.counts {
				counts[index] = strconv.FormatUint(count, 10)
			}
			metric.Histogram.DataPoints = append(metric.Histogram.DataPoints, otlpHistogramPoint{
				Attributes: attributes, StartTimeUnixNano: start, TimeUnixNano: at,
				Count: strconv.FormatUint(series.count, 10), Sum: series.sum, BucketCounts: counts,
				ExplicitBounds: slices.Clone(series.buckets), Min: series.min, Max: series.max,
			})
		default:
			if metric.Sum == nil {
				metric.Sum = &otlpSum{AggregationTemporality: temporalityCumulative, IsMonotonic: series.kind == metricKindSum}
			}
			metric.Sum.DataPoints = append(metric.Sum.DataPoints, otlpNumberPoint{Attributes: attributes, StartTimeUnixNano: start, TimeUnixNano: at, AsDouble: series.value})
		}
	}
	sort.Strings(names)
	metrics := make([]otlpMetric, 0, len(names))
	for _, name := range names {
		metric := byName[name]
		if metric.Sum != nil {
			sort.Slice(metric.Sum.DataPoints, func(i, j int) bool {
				return attributeKey(metric.Sum.DataPoints[i].Attributes) < attributeKey(metric.Sum.DataPoints[j].Attributes)
			})
		}
		if metric.Histogram != nil {
			sort.Slice(metric.Histogram.DataPoints, func(i, j int) bool {
				return attributeKey(metric.Histogram.DataPoints[i].Attributes) < attributeKey(metric.Histogram.DataPoints[j].Attributes)
			})
		}
		metrics = append(metrics, *metric)
	}
	service := serviceName
	return otlpMetricsRequest{ResourceMetrics: []otlpResourceMetrics{{
		Resource:     otlpResource{Attributes: []otlpKeyValue{{Key: "service.name", Value: otlpValue{StringValue: &service}}}},
		ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: instrumentationName}, Metrics: metrics}},
	}}}
}

func attributeKey(attributes []otlpKeyValue) string {
	var key strings.Builder
	for _, attribute := range attributes {
		key.WriteString(attribute.Key + "=")
		if attribute.Value.StringValue != nil {
			key.WriteString(*attribute.Value.StringValue)
		}
		key.WriteString("\x00")
	}
	return key.String()
}
