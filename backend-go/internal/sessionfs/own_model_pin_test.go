package sessionfs

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pinFixture is a profile whose run uses the own model openai/fast with the own openai/spare
// as its model_fallback, and offers the own openai/unused, which its run never reaches. A second
// global model, openai/other, has another key variable.
func pinFixture(t *testing.T) editFixture {
	t.Helper()
	e := newEditFixture(t, "", "", editBase+`}`)
	wsPut(t, filepath.Join(e.f.global, "models", "openai", "other.json"), `{"provider":"openai","model":"gpt-other","api_key_env":"OTHER_KEY"}`)
	wsPut(t, e.ownModel("openai/fast"), pinOwn("global:openai/default", "0.2"))
	wsPut(t, e.ownModel("openai/spare"), pinOwn("global:openai/default", "0.2"))
	wsPut(t, e.ownModel("openai/unused"), pinOwn("global:openai/default", "0.2"))
	wsPut(t, e.file, `{"models":["openai/default","profile:openai/fast","profile:openai/spare","profile:openai/unused"],"model_default":"profile:openai/fast","settings":{"model_fallback":{"models":["profile:openai/spare"]}}}`)
	return e
}

func pinOwn(basedOn, temperature string) string {
	return `{"based_on":"` + basedOn + `","set":{"request":{"temperature":` + temperature + `}}}`
}

func (e editFixture) loadRun(t *testing.T) SessionConfig {
	t.Helper()
	config, err := LoadSessionSettings(e.location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(config.ModelBases) != 3 || config.ModelFallback == nil || len(config.ModelFallback.Models) != 1 {
		t.Fatalf("own models = %v, fallback = %+v", config.ModelBases, config.ModelFallback)
	}
	return config
}

// withModelAt is config's model loaded as if the run used the model file at path.
func withModelAt(config SessionConfig, path string) (Model, error) {
	config.ModelPath, config.ReasoningEffort = path, ""
	loaded, err := config.WithModel()
	return loaded.Model, err
}

func temperatureOf(t *testing.T, model Model, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Temperature *json.Number `json:"temperature"`
	}
	if json.Unmarshal(model.Request, &request) != nil || request.Temperature == nil {
		t.Fatalf("request without a temperature: %s", model.Request)
	}
	return request.Temperature.String()
}

// A run keeps the own models it read when it started: an edit of the selected model's set, a
// changed based_on of its fallback and an edit of the base reach neither, and none of them fails
// with "load the session again". An own model the run did not reach when it started fails closed
// and is not read from the disk. Shared models still follow their files, and the next run reads
// the edit (decider 3c.2b).
func TestPinnedRunKeepsTheOwnModelsItStartedWith(t *testing.T) {
	e := pinFixture(t)
	config := e.loadRun(t)
	pinned := config.PinOwnModels()
	fast, spare, unused := e.ownModel("openai/fast"), e.ownModel("openai/spare"), e.ownModel("openai/unused")

	// The selected model's set changes through a self-edit; the fallback's base is swapped by hand.
	if _, err := e.editModels(t, "", ProfileDocumentChange{Path: "openai/fast", Content: text(pinOwn("global:openai/default", "0.9")), IfMatch: e.ownModelRevision(t, "openai/fast")}); err != nil {
		t.Fatal(err)
	}
	wsPut(t, spare, pinOwn("global:openai/other", "0.9"))
	base := filepath.Join(e.f.global, "models", "openai", "default.json")
	wsPut(t, base, `{"provider":"openai","model":"gpt-new","api_key_env":"NEW_KEY"}`)

	model, err := pinned.WithModel()
	if got := temperatureOf(t, model.Model, err); got != "0.2" || model.Model.Name != "gpt-test" || model.Model.APIKeyEnv != "FIXTURE_KEY" {
		t.Fatalf("pinned run model = %s %s %s", got, model.Model.Name, model.Model.APIKeyEnv)
	}
	again, err := withModelAt(pinned, spare)
	if got := temperatureOf(t, again, err); got != "0.2" || again.APIKeyEnv != "FIXTURE_KEY" {
		t.Fatalf("pinned fallback = %s %s", got, again.APIKeyEnv)
	}
	if _, err := withModelAt(pinned, unused); err == nil || !IsOwnModelError(err) || !strings.Contains(err.Error(), "did not read this own model when it started") {
		t.Fatalf("an own model the run did not reach = %v", err)
	}
	// A shared model is read from its file, as before.
	if shared, err := withModelAt(pinned, base); err != nil || shared.Name != "gpt-new" {
		t.Fatalf("shared model = %+v, %v", shared, err)
	}
	// A pinned config pins nothing again.
	if repinned, err := pinned.PinOwnModels().WithModel(); temperatureOf(t, repinned.Model, err) != "0.2" {
		t.Fatal("pinning again read the disk")
	}

	// Without the copy, the same loads read the edits.
	if unpinned, err := config.WithModel(); temperatureOf(t, unpinned.Model, err) != "0.9" || unpinned.Model.Name != "gpt-new" {
		t.Fatalf("unpinned = %+v", unpinned.Model)
	}
	if _, err := withModelAt(config, spare); err == nil || !strings.Contains(err.Error(), "load the session again") {
		t.Fatalf("unpinned fallback = %v", err)
	}
	if _, err := withModelAt(config, fast); err != nil {
		t.Fatal(err)
	}

	// The next run reads the edit.
	wsPut(t, spare, pinOwn("global:openai/default", "0.9"))
	next, err := e.loadRun(t).PinOwnModels().WithModel()
	if got := temperatureOf(t, next.Model, err); got != "0.9" || next.Model.Name != "gpt-new" {
		t.Fatalf("next run model = %s %s", got, next.Model.Name)
	}
}

// A run that starts while an edit publishes waits for it: an edit that fails halfway and is
// undone is never seen, and one that finishes is seen whole.
func TestPinWaitsForAnEditBeingPublished(t *testing.T) {
	e := pinFixture(t)
	config := e.loadRun(t)
	t.Cleanup(func() { editPublishFault = nil })
	var started sync.WaitGroup
	var pinned SessionConfig
	editPublishFault = func(at string) error {
		if at != "profile" {
			return nil
		}
		// The own model is written, the profile file is next: the publish holds the lock.
		if written, _ := os.ReadFile(e.ownModel("openai/fast")); !strings.Contains(string(written), "0.9") {
			t.Errorf("the model is not published yet: %s", written)
		}
		started.Add(1)
		go func() {
			defer started.Done()
			pinned = config.PinOwnModels()
		}()
		time.Sleep(20 * time.Millisecond)
		return errors.New("injected at " + at)
	}
	_, err := e.editModels(t, `{"models":["openai/default","profile:openai/fast","profile:openai/spare"],"model_default":"profile:openai/fast","settings":{"model_fallback":{"models":["profile:openai/spare"]}}}`,
		ProfileDocumentChange{Path: "openai/fast", Content: text(pinOwn("global:openai/default", "0.9")), IfMatch: e.ownModelRevision(t, "openai/fast")})
	editPublishFault = nil
	if err == nil || !strings.Contains(err.Error(), "was undone") {
		t.Fatalf("edit = %v", err)
	}
	started.Wait()
	if model, err := pinned.WithModel(); temperatureOf(t, model.Model, err) != "0.2" {
		t.Fatal("the run read an edit that was undone")
	}

	// A writer holding the lock: the run waits, then reads what the writer left.
	unlock, err := lockProfileFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan SessionConfig, 1)
	go func() { done <- config.PinOwnModels() }()
	select {
	case <-done:
		t.Fatal("the run did not wait for the writer")
	case <-time.After(50 * time.Millisecond):
	}
	wsPut(t, e.ownModel("openai/fast"), pinOwn("global:openai/default", "0.7"))
	unlock()
	if model, err := (<-done).WithModel(); temperatureOf(t, model.Model, err) != "0.7" {
		t.Fatal("the run did not read what the writer left")
	}
}

// Runs that start while edits change both of their own models read one revision: never one
// model edited and the other not.
func TestPinnedModelsAreOfOneRevision(t *testing.T) {
	e := pinFixture(t)
	config := e.loadRun(t)
	spare := e.ownModel("openai/spare")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			value := []string{"0.2", "0.9"}[i%2]
			model := text(pinOwn("global:openai/default", value))
			if _, err := e.editModels(t, "",
				ProfileDocumentChange{Path: "openai/fast", Content: model, IfMatch: e.ownModelRevision(t, "openai/fast")},
				ProfileDocumentChange{Path: "openai/spare", Content: model, IfMatch: e.ownModelRevision(t, "openai/spare")}); err != nil {
				var conflict *ConflictError
				if !errors.As(err, &conflict) {
					t.Error(err)
					return
				}
			}
		}
	}()
	for range 50 {
		pinned := config.PinOwnModels()
		selected, err := pinned.WithModel()
		first := temperatureOf(t, selected.Model, err)
		fallback, err := withModelAt(pinned, spare)
		if second := temperatureOf(t, fallback, err); first != second {
			t.Errorf("one run read %s and %s", first, second)
		}
	}
	close(stop)
	wg.Wait()
}

// A config with no own model is returned as it is, without the profile file's lock: v1 and a
// profile that runs shared models only.
func TestPinOwnModelsLeavesAConfigWithoutOwnModels(t *testing.T) {
	e := newEditFixture(t, "", "", editBase+`}`)
	config, err := LoadSessionSettings(e.location, "dev/coder", "")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockProfileFile(e.file)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if pinned := config.PinOwnModels(); pinned.ModelBases != nil || pinned.ModelPath != config.ModelPath {
		t.Fatalf("pinned = %+v", pinned.ModelBases)
	}
	if pinned := (SessionConfig{}).PinOwnModels(); pinned.ModelBases != nil {
		t.Fatal("an empty config changed")
	}
}
