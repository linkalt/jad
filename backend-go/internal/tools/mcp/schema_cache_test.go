package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

func countingCache(max int) (*schemaCache, *atomic.Int64) {
	var compiles atomic.Int64
	cache := newSchemaCache(max)
	cache.compile = func(raw []byte) (*tools.SchemaValidator, error) {
		compiles.Add(1)
		return tools.CompileSchema(raw)
	}
	return cache, &compiles
}

func TestLoadToolsCompilesEachSchemaOnce(t *testing.T) {
	reference, _ := newListServer(t, "one", listServerOptions{}, nil)
	manager := NewManager(nil)
	t.Cleanup(func() { _ = manager.Close() })
	var compiles atomic.Int64
	manager.schemas.compile = func(raw []byte) (*tools.SchemaValidator, error) {
		compiles.Add(1)
		return tools.CompileSchema(raw)
	}
	for run := 0; run < 5; run++ {
		loaded, err := manager.LoadTools(context.Background(), []sessionfs.MCPServerReference{reference}, t.TempDir())
		if err != nil || len(loaded) != 1 {
			t.Fatalf("run %d: %d tools, err=%v", run, len(loaded), err)
		}
	}
	if compiles.Load() != 1 {
		t.Fatalf("compilations = %d, want 1 across 5 loads", compiles.Load())
	}
}

func TestSchemaCacheRecompilesAChangedSchema(t *testing.T) {
	cache, compiles := countingCache(8)
	first, err := cache.get([]byte(`{"type":"object","properties":{"a":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := cache.get([]byte(`{"type":"object","properties":{"a":{"type":"number"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if first == changed || compiles.Load() != 2 {
		t.Fatalf("compilations = %d, same validator = %v", compiles.Load(), first == changed)
	}
	issues, err := changed.Validate([]byte(`{"a":"text"}`))
	if err != nil || len(issues) == 0 {
		t.Fatalf("changed schema must reject a string: issues=%v err=%v", issues, err)
	}
}

func TestSchemaCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	cache, compiles := countingCache(3)
	schema := func(index int) []byte { return []byte(fmt.Sprintf(`{"type":"object","description":"s%d"}`, index)) }
	for index := 0; index < 3; index++ {
		if _, err := cache.get(schema(index)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cache.get(schema(0)); err != nil { // schema 1 is now the oldest
		t.Fatal(err)
	}
	if _, err := cache.get(schema(3)); err != nil {
		t.Fatal(err)
	}
	if cache.len() != 3 {
		t.Fatalf("len = %d, want the bound 3", cache.len())
	}
	before := compiles.Load()
	for _, index := range []int{0, 2, 3} {
		if _, err := cache.get(schema(index)); err != nil {
			t.Fatal(err)
		}
	}
	if compiles.Load() != before {
		t.Fatal("recently used schemas were evicted")
	}
	if _, err := cache.get(schema(1)); err != nil || compiles.Load() != before+1 {
		t.Fatalf("evicted schema must recompile: compiles=%d err=%v", compiles.Load(), err)
	}
	for index := 10; index < 1010; index++ {
		if _, err := cache.get(schema(index)); err != nil {
			t.Fatal(err)
		}
	}
	if cache.len() != 3 {
		t.Fatalf("len after 1000 distinct schemas = %d, want 3", cache.len())
	}
}

func TestSchemaCacheDoesNotCacheInvalidSchemas(t *testing.T) {
	cache, compiles := countingCache(8)
	invalid := []byte(`{"type":"object","properties":{"a":{"type":"nonsense"}}}`)
	_, direct := tools.CompileSchema(invalid)
	if direct == nil {
		t.Fatal("test schema must be invalid")
	}
	for attempt := 0; attempt < 2; attempt++ {
		_, err := cache.get(invalid)
		if err == nil || err.Error() != direct.Error() {
			t.Fatalf("error = %v, want %v", err, direct)
		}
	}
	if compiles.Load() != 2 || cache.len() != 0 {
		t.Fatalf("compiles=%d len=%d, want 2 and 0", compiles.Load(), cache.len())
	}
	if _, err := cache.get([]byte("  ")); err == nil || !strings.Contains(err.Error(), "JSON Schema is required") {
		t.Fatalf("empty schema error = %v", err)
	}
}

func TestSchemaCacheSharesOneValidatorAcrossConcurrentUse(t *testing.T) {
	cache, _ := countingCache(4)
	schema := []byte(`{"type":"object","properties":{"n":{"type":"integer"},"s":{"type":"string"}},"required":["n"],"additionalProperties":false}`)
	var wait sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for round := 0; round < 100; round++ {
				validator, err := cache.get(schema)
				if err != nil {
					t.Error(err)
					return
				}
				if issues, err := validator.Validate([]byte(`{"n":1}`)); err != nil || len(issues) != 0 {
					t.Errorf("valid instance rejected: %v %v", issues, err)
				}
				if issues, err := validator.Validate([]byte(`{"n":"x","extra":1}`)); err != nil || len(issues) == 0 {
					t.Errorf("invalid instance accepted: %v %v", issues, err)
				}
				if _, err := cache.get([]byte(fmt.Sprintf(`{"type":"object","title":"w%d-%d"}`, worker, round%6))); err != nil {
					t.Error(err)
				}
			}
		}(worker)
	}
	wait.Wait()
	if cache.len() > 4 {
		t.Fatalf("len = %d exceeds the bound", cache.len())
	}
}
