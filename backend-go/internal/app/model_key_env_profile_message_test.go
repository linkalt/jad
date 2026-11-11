package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jad/backend-go/internal/remoteapi"
)

// T-009 review: a local profile_message target's run hides its caller's model keys as a local
// sub-agent run does. The caller dev/coder runs openai/small, whose key variable here is
// CALLER_PROVIDER_KEY; the target review/reviewer runs openai/large (FIXTURE_KEY). The target's
// exec env and hook env set both names, so a name the filter misses shows its value.

// callerKeyEnv is the configured env of the target's exec and of its hooks.
var callerKeyEnv = map[string]string{
	"CALLER_PROVIDER_KEY": "caller-in-env",
	"FIXTURE_KEY":         "fixture-in-env",
	"SPARE_PROVIDER_KEY":  "spare-in-env",
	"KEY_TEST_VISIBLE":    "visible-in-env",
}

// callerKeyProbe prints what a child process sees of the variables of callerKeyEnv.
const callerKeyProbe = `echo "C=[$CALLER_PROVIDER_KEY] F=[$FIXTURE_KEY] S=[$SPARE_PROVIDER_KEY] V=[$KEY_TEST_VISIBLE]"`

// callerKeyFixture is the layout fixture with openai/small on CALLER_PROVIDER_KEY (set in the
// loaded environment) and openai/spare (SPARE_PROVIDER_KEY), and review/reviewer given exec, a
// sub-agent finder and the hooks of hooks. dev/coder has profile_message.
func callerKeyFixture(t *testing.T, settings map[string]any) *layoutFixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	f := newLayoutFixtureWithBudget(t, nil, nil, func(config *Config) {
		config.Environment["CALLER_PROVIDER_KEY"] = "caller-provider-key"
	})
	var large map[string]any
	data, err := os.ReadFile(filepath.Join(f.root, "models", "openai", "large.json"))
	if err != nil || json.Unmarshal(data, &large) != nil {
		t.Fatalf("read the large model: %v", err)
	}
	small := map[string]any{}
	spare := map[string]any{}
	for key, value := range large {
		small[key], spare[key] = value, value
	}
	small["model"], small["api_key_env"] = "model-small", "CALLER_PROVIDER_KEY"
	spare["model"], spare["api_key_env"] = "model-spare", "SPARE_PROVIDER_KEY"
	files := map[string]any{
		"models/openai/small.json": small,
		"models/openai/spare.json": spare,
		"tools/coding/none.json":   map[string]any{"native": map[string]any{"profile_message": map[string]any{}}, "mcp": map[string]any{}},
		"tools/review/keys.json": map[string]any{"native": map[string]any{
			"exec":     map[string]any{"spill": map[string]any{"enabled": false}, "env": callerKeyEnv},
			"async":    map[string]any{},
			"subagent": map[string]any{"agents": map[string]any{"finder": map[string]any{"description": "Runs a command."}}},
		}, "mcp": map[string]any{}},
		"profiles/review/reviewer.json": map[string]any{"models": []string{"openai/large"}, "toolset": "review/keys", "settings": settings},
	}
	for rel, value := range files {
		body, _ := json.Marshal(value)
		writeFile(t, filepath.Join(f.root, rel), body)
	}
	return f
}

// probeTo is exec code that writes the probe line to path.
func probeTo(path string) string {
	return jsonCode(callerKeyProbe + " > " + path)
}

func execCall(callID, code string) string {
	var arguments map[string]any
	_ = json.Unmarshal([]byte(code), &arguments)
	return layoutCall(callID, "exec", arguments)
}

func readProbe(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no probe at %s: %v", filepath.Base(path), err)
	}
	return strings.TrimSpace(string(data))
}

// caller -> target -> child: the target's exec, its child's exec, its pre_tool hook for both, its
// run_start command and its subagent_stop command hide the caller's key beside the target's own.
// The target's own run afterwards (no caller) shows the caller's key, which no model of that run
// uses: the narrowing is the message run's only, and the target's settings are unchanged.
func TestAProfileMessageTargetsToolsAndHooksHideTheCallersModelKey(t *testing.T) {
	dir := t.TempDir()
	hookEnv := map[string]any{}
	for name, value := range callerKeyEnv {
		hookEnv[name] = value
	}
	f := callerKeyFixture(t, map[string]any{"hooks": map[string]any{"enabled": true, "env": hookEnv,
		"pre_tool": []any{map[string]any{"name": "pre", "match": "exec", "argv": []string{"sh", "-c",
			`id=$(grep -o 'call-[a-z]*-exec' | head -1); echo "$id $(` + callerKeyProbe + `)" >> ` + filepath.Join(dir, "pre")}}},
		"run_start":     []any{map[string]any{"name": "start", "argv": []string{"sh", "-c", callerKeyProbe + " >> " + filepath.Join(dir, "start")}}},
		"subagent_stop": []any{map[string]any{"name": "stop", "argv": []string{"sh", "-c", callerKeyProbe + " > " + filepath.Join(dir, "stop")}}},
	}})
	f.script = func(model, body string) string {
		last := lastInputItem(t, body)
		switch model {
		case "model-small":
			if !strings.Contains(body, "function_call_output") {
				return layoutCall("call-pm", "profile_message", map[string]any{"profile": "review/reviewer", "message": "pm-run"})
			}
		case "model-large":
			switch {
			case strings.Contains(last, "function_call_output"):
				if strings.Contains(last, `"call-target-exec"`) {
					return layoutCall("call-sub", "subagent", map[string]any{"agent": "finder", "task": "child-go"})
				}
			case strings.Contains(last, "child-go"):
				return execCall("call-child-exec", probeTo(filepath.Join(dir, "child")))
			case strings.Contains(last, "pm-run"):
				return execCall("call-target-exec", probeTo(filepath.Join(dir, "target")))
			case strings.Contains(last, "direct-run"):
				return execCall("call-direct-exec", probeTo(filepath.Join(dir, "direct")))
			}
		}
		return ""
	}
	if body := runMixedChain(t, f); !strings.Contains(body, "event: completed") {
		t.Fatalf("caller run did not complete:\n%s", body)
	}
	awaitTargetSessionFree(t, f)
	if status, body := f.do(t, http.MethodPost, "/api/sessions/local:demo/run?profile="+url.QueryEscape("review/reviewer"), `{"input":"direct-run"}`); status != http.StatusOK || !strings.Contains(body, "event: completed") {
		t.Fatalf("direct reviewer run: %d %s", status, body)
	}

	const narrowed = "C=[] F=[] S=[spare-in-env] V=[visible-in-env]"
	const own = "C=[caller-in-env] F=[] S=[spare-in-env] V=[visible-in-env]"
	wantProbe(t, "the target's exec", readProbe(t, filepath.Join(dir, "target")), narrowed)
	wantProbe(t, "the target's child's exec", readProbe(t, filepath.Join(dir, "child")), narrowed)
	wantProbe(t, "the target's subagent_stop command", readProbe(t, filepath.Join(dir, "stop")), narrowed)
	wantProbe(t, "the target's own run's exec", readProbe(t, filepath.Join(dir, "direct")), own)
	pre := strings.Split(readProbe(t, filepath.Join(dir, "pre")), "\n")
	want := []string{"call-target-exec " + narrowed, "call-child-exec " + narrowed, "call-direct-exec " + own}
	if strings.Join(pre, "\n") != strings.Join(want, "\n") {
		t.Errorf("pre_tool lines:\n%s\nwant:\n%s", strings.Join(pre, "\n"), strings.Join(want, "\n"))
	}
	starts := strings.Split(readProbe(t, filepath.Join(dir, "start")), "\n")
	if len(starts) != 2 || starts[0] != narrowed || starts[1] != own {
		t.Errorf("run_start lines = %q, want the message run's narrowed, then the own run's", starts)
	}
}

// A child the remote API starts has no local caller, so only runEphemeral names its models: its
// exec hides the key of the child's model_fallback model (openai/spare) as well as its own.
func TestARemoteStartedChildHidesItsFallbackModelsKey(t *testing.T) {
	dir := t.TempDir()
	f := callerKeyFixture(t, map[string]any{"model_fallback": map[string]any{"models": []string{"openai/spare"}}})
	f.script = func(model, body string) string {
		last := lastInputItem(t, body)
		if model == "model-large" && strings.Contains(last, "remote-go") && !strings.Contains(last, "function_call_output") {
			return execCall("call-remote-exec", probeTo(filepath.Join(dir, "remote")))
		}
		return ""
	}
	backend := remoteBackend{resolver: f.service.resolver, runTimeout: time.Minute, runs: newRemoteRunIndex()}
	run, err := backend.StartSubagent(context.Background(), remoteapi.SubagentRequest{Session: "demo", Profile: "review/reviewer", Agent: "finder", Task: "remote-go", RunID: "rr_keys", Caller: "remote:peer"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Done():
	case <-time.After(time.Minute):
		t.Fatal("the remote sub-agent did not end")
	}
	wantProbe(t, "the remote child's exec", readProbe(t, filepath.Join(dir, "remote")), "C=[caller-in-env] F=[] S=[] V=[visible-in-env]")
}
