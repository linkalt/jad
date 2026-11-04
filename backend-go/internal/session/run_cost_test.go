package session

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestCostMeterAddsIntoItsParent(t *testing.T) {
	root := NewCostMeter(nil)
	ctx := WithCostMeter(context.Background(), root)
	run := NewCostMeter(CostMeterFrom(ctx))
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() { defer wait.Done(); run.Add(0.01) }()
	}
	wait.Wait()
	if total, priced := run.Total(); priced != 100 || total < 0.999 || total > 1.001 {
		t.Fatalf("run = %v, %d", total, priced)
	}
	if total, priced := root.Total(); priced != 100 || total < 0.999 || total > 1.001 {
		t.Fatalf("root = %v, %d", total, priced)
	}
	if CostMeterFrom(context.Background()) != nil {
		t.Fatal("a plain context has a meter")
	}
	var none *CostMeter
	if total, priced := none.Total(); total != 0 || priced != 0 {
		t.Fatal("a nil meter has a total")
	}
	if text := (&RunCostLimitError{Used: 0.0057, Limit: 0.005}).Error(); !strings.Contains(text, "$0.0057") || !strings.Contains(text, "max_run_cost_usd limit of $0.0050") {
		t.Fatalf("error text = %q", text)
	}
}
