package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
)

// compactionPlan is what one compaction reads from the history before it rewrites it.
type compactionPlan struct {
	history        [][]byte
	activeTail     []json.RawMessage
	finalAssistant map[string]struct{}
	turns          [][][]byte
	selected       [][][]byte
}

// planCompaction reads the history, the preserved response and the assistant-final items,
// splits the compactable history into turns (a user message starts one, except a steering
// message, which continues the turn before it) and selects the kept ones. The caller holds
// s.mu.
func (s *TurnStore) planCompaction(options CompactionOptions) (compactionPlan, error) {
	plan := compactionPlan{}
	if s.recovering {
		return plan, errors.New("turn store recovery is in progress")
	}
	if len(s.activeTurns) != 0 {
		return plan, errors.New("cannot compact while a response turn is active")
	}
	// The compaction cache is read once for the history and for the
	// assistant-final items. The store lock is held and no turn is active, so
	// nothing rewrites the cache between the two uses.
	previousCache, err := s.readCompactionCache()
	if err != nil {
		return plan, fmt.Errorf("read request history for compaction: %w", err)
	}
	var continued []bool
	plan.history, err = s.readInputItemsWithCache(nil, previousCache, nil, &continued)
	if err != nil {
		return plan, fmt.Errorf("read request history for compaction: %w", err)
	}
	activeTail, excluded, err := s.preservedResponseTail(options.PreserveResponseID)
	if err != nil {
		return plan, err
	}
	plan.activeTail = activeTail
	compactableHistory, compactableContinued := excludePreservedItems(plan.history, continued, excluded)
	plan.finalAssistant, err = s.assistantFinalItems(previousCache)
	if err != nil {
		return plan, fmt.Errorf("read assistant final items for compaction: %w", err)
	}
	plan.turns = make([][][]byte, 0)
	for index, item := range compactableHistory {
		fields, err := objectFields(item)
		if err != nil {
			return plan, err
		}
		roleRaw, _ := fieldValue(fields, "role")
		role, _ := stringValue(roleRaw)
		startsTurn := role == "user" && !(index < len(compactableContinued) && compactableContinued[index])
		if startsTurn || len(plan.turns) == 0 {
			plan.turns = append(plan.turns, make([][]byte, 0))
		}
		plan.turns[len(plan.turns)-1] = append(plan.turns[len(plan.turns)-1], item)
	}
	plan.selected = plan.turns
	if options.KeepTurns == 0 {
		plan.selected = nil
	} else if len(plan.selected) > options.KeepTurns {
		plan.selected = plan.selected[len(plan.selected)-options.KeepTurns:]
	}
	if options.KeepLatestTurn && len(plan.selected) == 0 && len(plan.turns) > 0 {
		plan.selected = plan.turns[len(plan.turns)-1:]
	}
	return plan, nil
}
