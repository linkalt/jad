package native

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeGrepTruncatesLongLinesAndEnforcesContentByteBudget(t *testing.T) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep is not installed")
	}
	root := t.TempDir()
	longLine := strings.Repeat("x", 300) + " HIT\n"
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(longLine), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := nativeGrepSettings(binary)
	settings.MaxLineChars = 40
	catalog := newGrepTestCatalog(t, settings)
	clipped := executeGrepTestCall(t, catalog, root, "grep-long-line", map[string]any{"pattern": "HIT"})
	if !strings.Contains(clipped, "[... truncated line]") || !strings.Contains(clipped, "1:xxxx") || strings.Contains(clipped, strings.Repeat("x", 60)) {
		t.Fatalf("long line was not clipped: %s", clipped)
	}

	if err := os.WriteFile(filepath.Join(root, "budget.txt"), []byte("TODO first\nTODO second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	budgetSettings := nativeGrepSettings(binary)
	budgetSettings.MaxBytes = 30
	budgetCatalog := newGrepTestCatalog(t, budgetSettings)
	budget := executeGrepTestCall(t, budgetCatalog, root, "grep-byte-budget", map[string]any{"pattern": "TODO", "path": "budget.txt"})
	if !strings.Contains(budget, "matches: 1") || !strings.Contains(budget, "truncated: true") || !strings.Contains(budget, "truncation_reason: max_content_bytes") {
		t.Fatalf("content byte budget was not reported: %s", budget)
	}
	if !strings.Contains(budget, "1:TODO first") || strings.Contains(budget, "2:TODO second") {
		t.Fatalf("content byte budget retained incorrect lines: %s", budget)
	}
}

func BenchmarkRunRipgrep(b *testing.B) {
	binary, err := exec.LookPath("rg")
	if err != nil {
		b.Skip("ripgrep is not installed")
	}
	root := b.TempDir()
	corpus := strings.Repeat("ordinary source line without a match\n", 2_000) + "needle at the end\n"
	if err := os.WriteFile(filepath.Join(root, "corpus.txt"), []byte(corpus), 0o600); err != nil {
		b.Fatal(err)
	}
	settings := nativeGrepSettings(binary)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result := runRipgrep(context.Background(), settings, nil, "needle", []string{"."}, root)
		if result.failure != nil || result.matches != 1 {
			b.Fatalf("runRipgrep() result = %+v", result)
		}
	}
}
