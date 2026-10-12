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

func containsState(states []string, wanted string) bool {
	for _, state := range states {
		if state == wanted {
			return true
		}
	}
	return false
}
