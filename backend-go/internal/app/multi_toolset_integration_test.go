package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestLockSessionThenResolveLogsAToolsetClashWarningOnce(t *testing.T) {
	read, err := filepath.Abs(filepath.Join("..", "..", "..", "sample", "tools", "native", "read.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := layouttest.Write(t, layouttest.Spec{
		SessionID:           "one",
		ToolDefinitionPaths: []string{read},
		Profiles: map[string]map[string]any{"dev/coder": {
			"models": []any{"openai/default"}, "model_default": "openai/default",
			"toolset": []string{"extra/first", "extra/second"},
		}},
	})
	for _, id := range []string{"extra/first", "extra/second"} {
		body, _ := json.Marshal(map[string]any{"native": map[string]any{"read": map[string]any{}}})
		writeFile(t, filepath.Join(fixture.Root, "tools", filepath.FromSlash(id)+".json"), body)
	}
	config, err := LoadConfig(sessionfs.LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	logs := &lockedBuffer{}
	resolver, err := NewResolver(config.Roots, map[string]string{}, CatalogConfig{SnapshotTurns: 24, MaxPageTurns: 50}, WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.Close)
	backdate(t, fixture.Root)

	release, err := resolver.LockSession(context.Background(), "one", "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := strings.Count(logs.String(), "level=WARN"); got != 1 {
		t.Fatalf("LockSession logged %d warnings, want 1:\n%s", got, logs.String())
	}
	for _, want := range []string{"read", "extra/first", "extra/second"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the warning does not name %q:\n%s", want, logs.String())
		}
	}
	if resolver.lockedConfigs.len() != 1 {
		t.Fatalf("parked configs after LockSession = %d, want 1", resolver.lockedConfigs.len())
	}
	if _, err := resolver.Resolve(context.Background(), "one", "dev/coder", ""); err != nil {
		t.Fatal(err)
	}
	if resolver.lockedConfigs.len() != 0 {
		t.Fatal("Resolve did not take the parked config")
	}
	if got := strings.Count(logs.String(), "level=WARN"); got != 1 {
		t.Fatalf("Resolve of a reused config logged again: %d warnings:\n%s", got, logs.String())
	}
}

// The agent lists two toolsets: the first gives read, the second an MCP server. The
// session registers neither.
func TestSubagentWithTwoToolsetsConnectsTheSecondToolsetsMCPServer(t *testing.T) {
	calls := &atomic.Int32{}
	f := buildSubagentFixtureWithMCP(t, map[string]any{
		"model": "child.json", "toolset": []string{"extra/reader", "extra/lookup"}, "tools": []string{"read", "mcp:fixture:lookup"},
		"__session_without": []string{"read"},
		"__toolsets": map[string]any{
			"extra/reader": map[string]any{"native": map[string]any{"read": map[string]any{}}},
			"extra/lookup": map[string]any{
				"native": map[string]any{},
				"mcp":    map[string]any{"m/fixture": map[string]any{"deny_tools": []string{}}},
			},
		},
	}, false, nil, &mcpFixture{calls: calls, onlyFile: true})
	f.provider.childFirstCall = [2]string{mcpToolNameFor("fixture", "lookup"), `{"word":"answer"}`}

	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if calls.Load() != 1 {
		t.Errorf("the MCP tool was called %d times, want 1", calls.Load())
	}
	child := f.provider.forModel("child-model")
	if len(child) < 2 {
		t.Fatalf("child requests = %d, want at least 2", len(child))
	}
	got := append([]string(nil), child[0].Tools...)
	want := []string{"read", mcpToolNameFor("fixture", "lookup")}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("child tools = %v, want %v", got, want)
	}
	if !strings.Contains(child[1].Text, "MCP says answer is 99") {
		t.Errorf("the child did not get the MCP result:\n%s", child[1].Text)
	}
}
