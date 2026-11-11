package app

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestReportRegisteredCatalogBuildCost reports what one registered catalog build of the assembly
// fixture costs over warm MCP managers (time and allocations per build): the owner's catalog, the
// subagent tool and every child catalog it builds at prepare. It measures and asserts nothing, so it
// runs only with JAD_REPORT_CATALOG_COST=1; it is how the R4b before and after numbers were taken.
func TestReportRegisteredCatalogBuildCost(t *testing.T) {
	if os.Getenv("JAD_REPORT_CATALOG_COST") != "1" {
		t.Skip("set JAD_REPORT_CATALOG_COST=1 to report the catalog build cost")
	}
	f, config, _ := assemblyFixture(t)
	resolver := f.service.resolver
	ctx := context.Background()
	build := func() {
		if _, _, _, err := resolver.buildRegisteredCatalog(ctx, config, nil); err != nil {
			t.Fatal(err)
		}
	}
	build()
	const runs = 200
	allocs := testing.AllocsPerRun(runs, build)
	started := time.Now()
	for range runs {
		build()
	}
	t.Logf("registered catalog build: %v per build, %.0f allocs per build", time.Since(started)/runs, allocs)
}
