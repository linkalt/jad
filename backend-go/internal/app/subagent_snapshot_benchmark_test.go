package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"jad/backend-go/internal/webui"
)

// benchmarkChild writes a child like a real one: rounds of a short answer followed by a
// tool result of resultBytes.
func benchmarkChild(b *testing.B, rounds, resultBytes int) (string, webui.SubagentCall) {
	b.Helper()
	root := b.TempDir()
	var transcript strings.Builder
	transcript.WriteString(childUserLine)
	result := `{"type":"function_call_output","call_id":"k","output":"` + strings.Repeat("r", resultBytes) + `"}` + "\n"
	for round := 1; round <= rounds; round++ {
		transcript.WriteString(completedLine(1, round, strings.Repeat("answer ", 40)))
		transcript.WriteString(result)
	}
	childFolderB(b, root, "bench-0a1b2c3d", transcript.String())
	return root, webui.SubagentCall{CallID: "bench"}
}

// BenchmarkSubagentSnapshotCold is a read of a child of 7 rounds with 14 KB results (about
// the size of a real one); Cached is the same call once its transcript is unchanged, which
// is what every later snapshot, history or message route costs: one stat.
func BenchmarkSubagentSnapshotCold(b *testing.B) {
	root, call := benchmarkChild(b, 7, 14<<10)
	limits := defaultSubagentSnapshotLimits
	b.ReportAllocs()
	for b.Loop() {
		subagentSnapshots.mu.Lock()
		clear(subagentSnapshots.entries)
		subagentSnapshots.mu.Unlock()
		readSubagentSnapshot(context.Background(), root, call, limits)
	}
}

func BenchmarkSubagentSnapshotCached(b *testing.B) {
	root, call := benchmarkChild(b, 7, 14<<10)
	limits := defaultSubagentSnapshotLimits
	readSubagentSnapshot(context.Background(), root, call, limits)
	b.ReportAllocs()
	for b.Loop() {
		readSubagentSnapshot(context.Background(), root, call, limits)
	}
}

// BenchmarkSubagentSnapshotHostile is a 40 MB child of 1 MB lines: the line limit skips
// them and the scan limit stops the read at 16 MB.
func BenchmarkSubagentSnapshotHostile(b *testing.B) {
	root, call := benchmarkChild(b, 40, 1<<20)
	limits := defaultSubagentSnapshotLimits
	b.ReportAllocs()
	for b.Loop() {
		subagentSnapshots.mu.Lock()
		clear(subagentSnapshots.entries)
		subagentSnapshots.mu.Unlock()
		readSubagentSnapshot(context.Background(), root, call, limits)
	}
}

func childFolderB(b *testing.B, root, name, transcript string) {
	b.Helper()
	dir := root + "/" + name
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(dir+"/messages.jsonl", []byte(transcript), 0o600); err != nil {
		b.Fatal(err)
	}
}
