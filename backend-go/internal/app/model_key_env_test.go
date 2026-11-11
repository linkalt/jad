package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// T-009: a run's tools and hooks hide the API key variable of the model that answers that run,
// not only the one of the model the tools were built with (the owner run's). Every fixture here
// sets all three key variables in the configured env of the tool or hook, so a name the filter
// misses shows its value.

// modelKeyEnv is the configured env of exec and of the hooks: the three models' key variables and
// one ordinary variable that must stay visible.
var modelKeyEnv = map[string]any{
	"PARENT_PROVIDER_KEY": "parent-in-env",
	"CHILD_PROVIDER_KEY":  "child-in-env",
	"THIRD_PROVIDER_KEY":  "third-in-env",
	"KEY_TEST_VISIBLE":    "visible-in-env",
}

// modelKeyProbe prints what a child process sees of the variables of modelKeyEnv.
const modelKeyProbe = `echo "P=[$PARENT_PROVIDER_KEY] C=[$CHILD_PROVIDER_KEY] T=[$THIRD_PROVIDER_KEY] V=[$KEY_TEST_VISIBLE]"`

var modelKeyProbeLine = regexp.MustCompile(`P=\[[^\]$]*\] C=\[[^\]$]*\] T=\[[^\]$]*\] V=\[[^\]$]*\]`)

// modelKeyFixture is the split fixture (parent-model with PARENT_PROVIDER_KEY on one server,
// child-model with CHILD_PROVIDER_KEY on another) plus openai/third (third-model,
// THIRD_PROVIDER_KEY) on the parent's server, all three listed by the profile. setting is the
// agent's.
func modelKeyFixture(t *testing.T, setting map[string]any) subagentFixture {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	setting["__extra_models"] = map[string]map[string]any{"openai/third": {"model": "third-model", "api_key_env": "THIRD_PROVIDER_KEY"}}
	setting["__environment"] = map[string]string{"THIRD_PROVIDER_KEY": "third-provider-key"}
	setting["__profile_models"] = []string{"openai/parent", "openai/child", "openai/third"}
	return newSplitSubagentFixture(t, setting)
}

// probeOutput is the probe line in the second request of the first child run of model, the one
// that carries its exec result.
func probeOutput(t *testing.T, provider *subagentProvider, model string) string {
	t.Helper()
	for _, request := range provider.forModel(model) {
		if !strings.Contains(request.Text, "function_call_output") {
			continue
		}
		if line := modelKeyProbeLine.FindString(strings.ReplaceAll(request.Text, `\"`, `"`)); line != "" {
			return line
		}
	}
	t.Fatalf("no request of %s carries the probe's output", model)
	return ""
}

func wantProbe(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s sees %s, want %s", what, got, want)
	}
}

// A plain agent with its own model (child-model) runs the owner's exec: the child's shell hides
// the child model's key as well as the owner's, and shows the third model's key, which no run
// here uses, and the ordinary variable.
func TestAChildsExecHidesTheKeyOfTheModelThatAnswersTheChild(t *testing.T) {
	f := modelKeyFixture(t, map[string]any{"model": "child.json", "tools": []string{"exec"}, "__exec": map[string]any{"env": modelKeyEnv}})
	f.childProvider.childFirstCall = [2]string{"exec", jsonCode(modelKeyProbe)}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	wantProbe(t, "the plain agent's exec", probeOutput(t, f.childProvider, "child-model"), "P=[] C=[] T=[third-in-env] V=[visible-in-env]")
}

// The same for an agent whose exec comes from its own toolset, built with the owner's config.
func TestAToolsetChildsExecHidesTheKeyOfTheModelThatAnswersTheChild(t *testing.T) {
	f := modelKeyFixture(t, map[string]any{
		"model": "child.json", "toolset": "extra/shell", "tools": []string{"exec"},
		"__exec":            map[string]any{},
		"__session_without": []string{"exec"},
		"__toolsets": map[string]any{"extra/shell": map[string]any{"native": map[string]any{
			"exec": map[string]any{"spill": map[string]any{"enabled": false}, "env": modelKeyEnv},
		}}},
	})
	f.childProvider.childFirstCall = [2]string{"exec", jsonCode(modelKeyProbe)}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	wantProbe(t, "the toolset agent's exec", probeOutput(t, f.childProvider, "child-model"), "P=[] C=[] T=[third-in-env] V=[visible-in-env]")
}

// A child's pre_tool and post_tool hooks run with the hooks' own env and hide the key of the
// child's model, as its exec does; so does the caller's subagent_stop command for that child.
func TestAChildsToolHooksHideTheKeyOfTheModelThatAnswersTheChild(t *testing.T) {
	dir := t.TempDir()
	hook := func(name string) map[string]any {
		return map[string]any{"name": name, "match": "exec", "argv": []string{"sh", "-c", modelKeyProbe + " > " + filepath.Join(dir, name)}}
	}
	f := modelKeyFixture(t, map[string]any{
		"model": "child.json", "tools": []string{"exec"}, "__exec": map[string]any{},
		"__session": map[string]any{"hooks": map[string]any{"enabled": true, "env": modelKeyEnv, "pre_tool": []any{hook("pre")}, "post_tool": []any{hook("post")},
			"subagent_stop": []any{map[string]any{"name": "stop", "argv": []string{"sh", "-c", modelKeyProbe + " > " + filepath.Join(dir, "stop")}}}}},
	})
	f.childProvider.childFirstCall = [2]string{"exec", jsonCode("true")}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	for _, name := range []string{"pre", "post", "stop"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("the %s hook did not run: %v", name, err)
		}
		wantProbe(t, "the child's "+name+" hook", strings.TrimSpace(string(data)), "P=[] C=[] T=[third-in-env] V=[visible-in-env]")
	}
}

// A call's model argument picks the child's model (child-model) over the agent's (third-model):
// the child's shell hides the key of the model that answers it, and shows the agent's own
// model's, which does not answer this run.
func TestACallsModelChoiceIsTheKeyTheChildsExecHides(t *testing.T) {
	f := modelKeyFixture(t, map[string]any{"model": "openai/third", "tools": []string{"exec"}, "__exec": map[string]any{"env": modelKeyEnv}, "__subagent_settings": map[string]any{"model_choice": true}})
	f.provider.parentFirstCall = [2]string{"subagent", `{"task":"find the answer","model":"openai/child"}`}
	f.childProvider.childFirstCall = [2]string{"exec", jsonCode(modelKeyProbe)}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	if len(f.provider.forModel("third-model")) != 0 {
		t.Fatal("the agent's own model answered although the call chose another")
	}
	wantProbe(t, "the child of a call that chose openai/child", probeOutput(t, f.childProvider, "child-model"), "P=[] C=[] T=[third-in-env] V=[visible-in-env]")
}

// Two children of one response, one on the agent's model (third-model) and one on the model its
// call chose (child-model), share the owner's exec tool: each hides its own model's key and only
// that one besides the owner's.
func TestSiblingChildrenOnDifferentModelsHideEachTheirOwnKey(t *testing.T) {
	f := modelKeyFixture(t, map[string]any{"model": "openai/third", "tools": []string{"exec"}, "__exec": map[string]any{"env": modelKeyEnv}, "__subagent_settings": map[string]any{"model_choice": true}})
	f.provider.parentSiblings = []string{`{"task":"the agent's model"}`, `{"task":"the call's model","model":"openai/child"}`}
	f.provider.childFirstCall = [2]string{"exec", jsonCode(modelKeyProbe)}
	f.childProvider.childFirstCall = [2]string{"exec", jsonCode(modelKeyProbe)}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	wantProbe(t, "the child on the agent's model", probeOutput(t, f.provider, "third-model"), "P=[] C=[child-in-env] T=[] V=[visible-in-env]")
	wantProbe(t, "the child on the call's model", probeOutput(t, f.childProvider, "child-model"), "P=[] C=[] T=[third-in-env] V=[visible-in-env]")
}

// The owner's own exec, and a profile env_deny, are as before: the owner's key and a denied name
// are hidden from the owner's shell; the other models' keys are not.
func TestTheOwnersExecStillHidesOnlyItsOwnKeyAndTheDeniedNames(t *testing.T) {
	f := modelKeyFixture(t, map[string]any{"model": "child.json", "__exec": map[string]any{"env": modelKeyEnv, "env_deny": []string{"KEY_TEST_VISIBLE"}}})
	f.provider.parentFirstCall = [2]string{"exec", jsonCode(modelKeyProbe)}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	wantProbe(t, "the owner's exec", probeOutput(t, f.provider, "parent-model"), "P=[] C=[child-in-env] T=[third-in-env] V=[]")
}

func jsonCode(code string) string {
	quoted := strings.ReplaceAll(strings.ReplaceAll(code, `\`, `\\`), `"`, `\"`)
	return `{"code":"` + quoted + `"}`
}

// A model_fallback model may answer the run's requests, so the run's tools and hooks hide its key
// too: the owner's exec, a child's and the run_start command all hide THIRD_PROVIDER_KEY when
// openai/third is the fallback.
func TestTheRunsFallbackModelsKeyIsHiddenToo(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "start")
	f := modelKeyFixture(t, map[string]any{"model": "child.json", "tools": []string{"exec"}, "__exec": map[string]any{"env": modelKeyEnv},
		"__session": map[string]any{"model_fallback": map[string]any{"models": []string{"openai/third"}},
			"hooks": map[string]any{"enabled": true, "env": modelKeyEnv, "run_start": []any{map[string]any{"name": "start", "argv": []string{"sh", "-c", modelKeyProbe + " > " + marker}}}}}})
	f.provider.parentSteps = []subagentStep{{"exec", jsonCode(modelKeyProbe)}, {"subagent", `{"task":"find the answer"}`}}
	f.childProvider.childFirstCall = [2]string{"exec", jsonCode(modelKeyProbe)}
	if body := f.run(t); !strings.Contains(body, "event: completed") {
		t.Fatalf("run did not complete:\n%s", body)
	}
	wantProbe(t, "the owner's exec", probeOutput(t, f.provider, "parent-model"), "P=[] C=[child-in-env] T=[] V=[visible-in-env]")
	wantProbe(t, "the child's exec", probeOutput(t, f.childProvider, "child-model"), "P=[] C=[] T=[] V=[visible-in-env]")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the run_start command did not run: %v", err)
	}
	wantProbe(t, "the run_start command", strings.TrimSpace(string(data)), "P=[] C=[child-in-env] T=[] V=[visible-in-env]")
}
