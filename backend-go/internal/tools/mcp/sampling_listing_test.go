package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

// probeListing lists the probe server of directory again on manager (the same connection) and
// returns its probe tool; a nil sampler lists with LoadTools.
func probeListing(t *testing.T, manager *Manager, directory string, sampler Sampler) tools.Tool {
	t.Helper()
	servers := []sessionfs.MCPServerReference{{ID: "probe", ConfigPath: filepath.Join(directory, "probe.json"), Enabled: true}}
	var loaded []tools.Tool
	var err error
	if sampler == nil {
		loaded, err = manager.LoadTools(context.Background(), servers, directory)
	} else {
		loaded, err = manager.LoadToolsSampled(context.Background(), servers, directory, sampler)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range loaded {
		if tool.Definition.ID == "mcp:probe:probe" {
			return tool
		}
	}
	t.Fatal("no probe tool")
	return tools.Tool{}
}

func namedSampler(model string) Sampler {
	return func(context.Context, SamplingRequest) (string, string, error) { return "hi", model, nil }
}

// T-008: a server's sampling request is answered by the sampler of the listing whose tool the
// connection is running, not by whatever was set or listed last on the shared manager. A call
// whose sampling is in flight keeps its listing's model while another listing and SetSampler
// happen; a LoadTools listing still uses the manager's sampler, and without one is refused.
func TestSamplingIsAnsweredByTheSamplerOfTheCallsListing(t *testing.T) {
	manager, plain, directory := probeServer(t, map[string]any{"sampling": map[string]any{"enabled": true, "max_requests": 100}})
	if output := runProbe(t, plain, tools.ExecutionContext{CallID: "c0"}, "sample"); !strings.Contains(output, `\"status\":\"error\"`) || strings.Contains(output, "sampled") {
		t.Fatalf("a LoadTools listing with no manager sampler: %s, want refused", output)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	blocking := func(context.Context, SamplingRequest) (string, string, error) {
		once.Do(func() { close(entered) })
		<-release
		return "hi", "model-a", nil
	}
	first := probeListing(t, manager, directory, blocking)
	answered := make(chan string, 1)
	go func() {
		output, err := first.ExecuteOutput(context.Background(), map[string]any{"ask": []string{"sample"}}, tools.ExecutionContext{CallID: "c1"})
		if err != nil {
			answered <- err.Error()
			return
		}
		answered <- string(output)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first listing's sampling request did not arrive")
	}
	// While the first call's sampling goes on: another listing, and a sampler set on the manager.
	second := probeListing(t, manager, directory, namedSampler("model-b"))
	manager.SetSampler(namedSampler("model-manager"))
	close(release)
	if got := <-answered; !strings.Contains(got, "sampled hi by model-a") {
		t.Fatalf("the first call's sampling = %s, want its own listing's model-a", got)
	}
	for name, want := range map[string]struct {
		tool  tools.Tool
		model string
	}{"second": {second, "model-b"}, "first again": {first, "model-a"}, "plain": {plain, "model-manager"}} {
		if got := runProbe(t, want.tool, tools.ExecutionContext{CallID: name}, "sample"); !strings.Contains(got, "by "+want.model) {
			t.Fatalf("%s listing: %s, want %s", name, got, want.model)
		}
	}
}

// Concurrent calls of listings with different samplers on one connection (callMu runs them one
// at a time) are each answered by their own listing's model.
func TestConcurrentCallsOfDifferentListingsSampleWithTheirOwnModels(t *testing.T) {
	manager, _, directory := probeServer(t, map[string]any{"sampling": map[string]any{"enabled": true, "max_requests": 100}})
	manager.SetSampler(namedSampler("model-manager"))
	listings := map[string]tools.Tool{}
	for _, model := range []string{"model-a", "model-b", "model-c"} {
		listings[model] = probeListing(t, manager, directory, namedSampler(model))
	}
	var wg sync.WaitGroup
	failures := make(chan string, 30)
	for model, tool := range listings {
		for i := range 8 {
			wg.Go(func() {
				output, err := tool.ExecuteOutput(context.Background(), map[string]any{"ask": []string{"sample"}}, tools.ExecutionContext{CallID: fmt.Sprintf("%s-%d", model, i)})
				if err != nil || !strings.Contains(string(output), "sampled hi by "+model) {
					failures <- fmt.Sprintf("%s call %d: %s %v", model, i, output, err)
				}
			})
		}
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

// The request budget stays one per connection, shared by every listing of it: sampled listings
// do not get a budget each.
func TestSamplingBudgetIsPerConnectionAcrossListings(t *testing.T) {
	manager, _, directory := probeServer(t, map[string]any{"sampling": map[string]any{"enabled": true, "max_requests": 2}})
	a, b := probeListing(t, manager, directory, namedSampler("model-a")), probeListing(t, manager, directory, namedSampler("model-b"))
	if got := runProbe(t, a, tools.ExecutionContext{CallID: "1"}, "sample"); !strings.Contains(got, "by model-a") {
		t.Fatalf("first: %s", got)
	}
	if got := runProbe(t, b, tools.ExecutionContext{CallID: "2"}, "sample"); !strings.Contains(got, "by model-b") {
		t.Fatalf("second: %s", got)
	}
	if got := runProbe(t, a, tools.ExecutionContext{CallID: "3"}, "sample"); !strings.Contains(got, `\"status\":\"error\"`) || strings.Contains(got, "sampled") {
		t.Fatalf("third, over the connection's budget of 2: %s, want refused", got)
	}
}
