package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrCompactionSkipped is returned by a CompactionOptions.Summarize callback that wants the
// compaction not to happen at all (compaction.summary.on_failure "skip"). CompactHistory
// then returns an empty result and no error, as when there was nothing to compact.
var ErrCompactionSkipped = errors.New("compaction skipped")

// compactionPlan is what one compaction reads from the history before it rewrites it.
type compactionPlan struct {
	history        [][]byte
	activeTail     []json.RawMessage
	finalAssistant map[string]struct{}
	turns          [][][]byte
	selected       [][][]byte
}

// dropped returns the items of the turns the compaction does not keep, oldest first. The
// tool pairs that assistant_final removes from a kept turn are not part of it.
func (p compactionPlan) dropped() [][]byte {
	items := make([][]byte, 0)
	for _, turn := range p.turns[:len(p.turns)-len(p.selected)] {
		items = append(items, turn...)
	}
	return items
}

// planCompaction reads the history, the preserved response and the assistant-final items,
// splits the compactable history into turns (a user message starts one) and selects the
// kept ones. The caller holds s.mu.
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
	plan.history, err = s.readInputItemsWithCache(nil, previousCache, nil)
	if err != nil {
		return plan, fmt.Errorf("read request history for compaction: %w", err)
	}
	activeTail, excluded, err := s.preservedResponseTail(options.PreserveResponseID)
	if err != nil {
		return plan, err
	}
	plan.activeTail = activeTail
	compactableHistory := excludePreservedItems(plan.history, excluded)
	plan.finalAssistant, err = s.assistantFinalItems(previousCache)
	if err != nil {
		return plan, fmt.Errorf("read assistant final items for compaction: %w", err)
	}
	plan.turns = make([][][]byte, 0)
	for _, item := range compactableHistory {
		fields, err := objectFields(item)
		if err != nil {
			return plan, err
		}
		roleRaw, _ := fieldValue(fields, "role")
		role, _ := stringValue(roleRaw)
		if role == "user" || len(plan.turns) == 0 {
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

// summarizeDroppedTurns hands the turns a compaction would drop to options.Summarize, with
// the store lock released while it runs (it calls a model), and stores its item and the
// digest of what it summarized in options. It reports skipped when the callback asked for
// no compaction. With nothing to drop, Summarize is not called.
func (s *TurnStore) summarizeDroppedTurns(options *CompactionOptions) (skipped bool, err error) {
	s.mu.Lock()
	plan, err := s.planCompaction(*options)
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	dropped := plan.dropped()
	if len(dropped) == 0 {
		return false, nil
	}
	item, err := options.Summarize(dropped)
	if errors.Is(err, ErrCompactionSkipped) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("summarize the compacted turns: %w", err)
	}
	if len(item) == 0 {
		return false, nil
	}
	if !json.Valid(item) {
		return false, errors.New("the compaction summary is not a JSON object")
	}
	options.summaryItem = append(json.RawMessage(nil), item...)
	options.summaryDigest = droppedDigest(dropped)
	return false, nil
}

// droppedDigest names a list of items, so the locked part of a compaction can tell that
// the turns it drops are the ones that were summarized.
func droppedDigest(items [][]byte) string {
	hash := sha256.New()
	for _, item := range items {
		var length [8]byte
		size := uint64(len(item))
		for index := range length {
			length[index] = byte(size >> (8 * index))
		}
		hash.Write(length[:])
		hash.Write(item)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
