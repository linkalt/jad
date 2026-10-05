package memorytrace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTraceRecordsOverlappingRunsAndPeakMemory(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	memory := Snapshot{RSSBytes: 10, HeapTotalBytes: 20, HeapUsedBytes: 5}
	lines := make([]string, 0)
	trace := New(Options{
		Enabled:        true,
		SampleInterval: time.Hour,
		ReadMemory:     func() Snapshot { return memory },
		WriteLine:      func(line string) { lines = append(lines, line) },
		Now:            func() time.Time { return now },
	})

	first := trace.BeginRun()
	memory = Snapshot{RSSBytes: 30, HeapTotalBytes: 40, HeapUsedBytes: 25}
	first.Mark("request-built", map[string]any{"history_items": 2})
	second := trace.BeginRun()
	memory = Snapshot{RSSBytes: 50, HeapTotalBytes: 60, HeapUsedBytes: 35}
	trace.SampleNow()
	now = now.Add(17 * time.Millisecond)
	first.Stop()
	second.Stop()
	first.Stop()

	if len(lines) != 6 {
		t.Fatalf("trace line count = %d, want 6: %v", len(lines), lines)
	}
	var start traceRecord
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], tracePrefix)), &start); err != nil {
		t.Fatal(err)
	}
	if start.Event != "start" || start.ActiveRuns != 1 || start.Memory == nil || start.Memory.RSSBytes != 10 {
		t.Fatalf("first start record = %+v", start)
	}
	var phase traceRecord
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], tracePrefix)), &phase); err != nil {
		t.Fatal(err)
	}
	if phase.Event != "phase" || phase.Phase != "request-built" || phase.Details["history_items"] != float64(2) {
		t.Fatalf("phase record = %+v", phase)
	}
	var sample traceRecord
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[3], tracePrefix)), &sample); err != nil {
		t.Fatal(err)
	}
	if sample.Event != "sample" || sample.ActiveRuns != 2 || len(sample.RunIDs) != 2 {
		t.Fatalf("sample record = %+v", sample)
	}
	var end traceRecord
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[4], tracePrefix)), &end); err != nil {
		t.Fatal(err)
	}
	if end.Event != "end" || end.ActiveRuns != 1 || end.ElapsedMS != 17 || end.Peak == nil || end.Peak.RSSBytes != 50 {
		t.Fatalf("first end record = %+v", end)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, tracePrefix) || strings.Contains(line, "prompt text") {
			t.Fatalf("unexpected trace line: %q", line)
		}
	}
}

func TestDisabledTraceIsNoOp(t *testing.T) {
	lines := 0
	trace := New(Options{WriteLine: func(string) { lines++ }})
	handle := trace.BeginRun()
	handle.Mark("request-built", map[string]any{"history_items": 1})
	trace.SampleNow()
	handle.Stop()
	if lines != 0 {
		t.Fatalf("disabled trace emitted %d lines", lines)
	}
}

func TestFromEnvironmentUsesSafeSamplingInterval(t *testing.T) {
	t.Setenv(enableEnvironmentVariable, "1")
	t.Setenv(intervalEnvironmentVariable, "17")
	trace := FromEnvironment()
	if !trace.options.Enabled || trace.options.SampleInterval != 17*time.Millisecond {
		t.Fatalf("configured trace = enabled %v, interval %v", trace.options.Enabled, trace.options.SampleInterval)
	}

	t.Setenv(intervalEnvironmentVariable, "9223372036854775807")
	trace = FromEnvironment()
	if want := New(Options{}).options.SampleInterval; trace.options.SampleInterval != want {
		t.Fatalf("unsafe interval was not replaced by default: got %v, want %v", trace.options.SampleInterval, want)
	}
}
