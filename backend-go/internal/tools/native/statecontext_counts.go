package native

// StateContextCount is how many records a summary section holds in one state.
type StateContextCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// countStates counts how many of states occur in values, in the order of states, and
// leaves out a state that has none.
func countStates(states []string, values []string) []StateContextCount {
	counts := make([]StateContextCount, 0, len(states))
	for _, state := range states {
		count := 0
		for _, value := range values {
			if value == state {
				count++
			}
		}
		if count > 0 {
			counts = append(counts, StateContextCount{State: state, Count: count})
		}
	}
	return counts
}

func stateContextItemCount[Item any](items []Item, counts []StateContextCount) int {
	return len(items) + len(counts)
}

// trimStateContextItem drops the last detail item, or when none are left the last
// count, and records the omission.
func trimStateContextItem[Item any](items *[]Item, omittedItems *int, counts *[]StateContextCount, omittedCounts *int) bool {
	if len(*items) > 0 {
		*items = (*items)[:len(*items)-1]
		*omittedItems++
		return true
	}
	if len(*counts) > 0 {
		*counts = (*counts)[:len(*counts)-1]
		*omittedCounts++
		return true
	}
	return false
}
