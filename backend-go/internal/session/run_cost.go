package session

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CostMeter adds up what a run's model responses cost, in US dollars, from the pricing of
// the model that answered each one. A run's meter also adds into the meter of the context
// the run started in: the HTTP executor puts one there to report the run's cost. Sub-agent
// runs start on their manager's context, not the calling run's, so they are not counted.
type CostMeter struct {
	parent *CostMeter
	mu     sync.Mutex
	total  float64
	priced int64
}

// NewCostMeter returns a meter that also adds into parent; parent may be nil.
func NewCostMeter(parent *CostMeter) *CostMeter { return &CostMeter{parent: parent} }

// Add counts the cost of one priced response.
func (m *CostMeter) Add(usd float64) {
	for meter := m; meter != nil; meter = meter.parent {
		meter.mu.Lock()
		meter.total += usd
		meter.priced++
		meter.mu.Unlock()
	}
}

// Total is what the priced responses cost so far, and how many there were.
func (m *CostMeter) Total() (float64, int64) {
	if m == nil {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total, m.priced
}

type costMeterKey struct{}

// WithCostMeter returns ctx carrying meter; a provider client that knows its model's
// pricing adds each response's cost to the meter of the context it streams with.
func WithCostMeter(ctx context.Context, meter *CostMeter) context.Context {
	return context.WithValue(ctx, costMeterKey{}, meter)
}

// CostMeterFrom returns the meter of ctx, or nil.
func CostMeterFrom(ctx context.Context) *CostMeter {
	meter, _ := ctx.Value(costMeterKey{}).(*CostMeter)
	return meter
}

// RunCostLimitError is returned when a run has cost max_run_cost_usd and the model asked
// for another round. Its text holds only numbers the harness counted, so a client may read it.
type RunCostLimitError struct {
	Used  float64
	Limit float64
}

func (e *RunCostLimitError) Error() string {
	return fmt.Sprintf("The run stopped before its next round: it cost $%.4f, which reached the max_run_cost_usd limit of $%.4f.", e.Used, e.Limit)
}

// RunDurationLimitError is returned when a run took max_run_duration_ms and was stopped.
// Its text holds only the configured limit, so a client may read it.
type RunDurationLimitError struct {
	Limit time.Duration
}

func (e *RunDurationLimitError) Error() string {
	return fmt.Sprintf("The run was stopped: it ran for the max_run_duration_ms limit of %s.", e.Limit)
}

// ClientMessage is the error's text, which names only the configured limit.
func (e *RunDurationLimitError) ClientMessage() string { return e.Error() }
