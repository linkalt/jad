package transcript

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
)

// itemIdentity is what a provider says about which output item an object is: its type
// and its stable ids. A done event and the completed response describe the same item
// with the same ids, whatever index each one gives it.
type itemIdentity struct {
	typ    string
	id     string
	callID string
}

// completedItem is one item of the completed response's output, in output order.
type completedItem struct {
	raw      []byte
	identity itemIdentity
}

// identityOfItem validates an output item and reads its identity. An id that is not a
// string counts as missing.
func identityOfItem(raw []byte) (itemIdentity, error) {
	fields, err := objectFields(raw)
	if err != nil {
		return itemIdentity{}, errors.New("output item must be an object")
	}
	return identityOfFields(fields)
}

func identityOfFields(fields []jsonField) (itemIdentity, error) {
	if err := validateOutputItemType(fields); err != nil {
		return itemIdentity{}, err
	}
	typeRaw, _ := fieldValue(fields, "type")
	identity := itemIdentity{}
	identity.typ, _ = stringValue(typeRaw)
	if raw, ok := fieldValue(fields, "id"); ok {
		identity.id, _ = stringValue(raw)
	}
	if raw, ok := fieldValue(fields, "call_id"); ok {
		identity.callID, _ = stringValue(raw)
	}
	return identity, nil
}

// compatible reports that the two could be one item: the same type, and no id that both
// give and that differs.
func (a itemIdentity) compatible(b itemIdentity) bool {
	return a.typ == b.typ &&
		(a.id == "" || b.id == "" || a.id == b.id) &&
		(a.callID == "" || b.callID == "" || a.callID == b.callID)
}

func (a itemIdentity) String() string {
	text := a.typ
	if a.id != "" {
		text += " id " + a.id
	}
	if a.callID != "" {
		text += " call_id " + a.callID
	}
	return text
}

// stagedOrder returns the staged output indexes in the order of the completed response's
// output, so item n of the committed line is the staged item that the completed output
// has at n. The completed output is the authority on order and on what the response
// holds; the wire output_index only names a staged item.
//
//   - Without an output array there is no order to follow: the staged items go in index
//     order, gaps and all.
//   - Staged indexes 0..n-1 whose items fit the output item at the same position (same
//     type, no conflicting id) are paired by position, as always.
//   - Anything else (a gap, an offset, a reorder) is paired by stable id: every output
//     item must match exactly one staged item sharing its id or call_id. Items without
//     an id on either side are paired in order only when the staged item has the same
//     index or the same content. Any other result is an error naming the items, and
//     nothing is shifted or guessed.
func (t *ResponseTurn) stagedOrder(plan completionPlan) ([]int64, error) {
	if !plan.hasOutput {
		order := make([]int64, 0, len(t.itemIndex))
		for index := range t.itemIndex {
			order = append(order, index)
		}
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
		return order, nil
	}
	count := len(plan.outputs)
	order := make([]int64, count)
	positional := true
	for position := range plan.outputs {
		entry, staged := t.itemIndex[int64(position)]
		if !staged || !entry.identity.compatible(plan.outputs[position].identity) {
			positional = false
			break
		}
		order[position] = int64(position)
	}
	if !positional {
		var err error
		if order, err = t.matchOutput(plan.outputs); err != nil {
			return nil, err
		}
	}
	if err := t.checkFunctionCalls(plan.outputs, order); err != nil {
		return nil, err
	}
	return order, nil
}

func (t *ResponseTurn) matchOutput(outputs []completedItem) ([]int64, error) {
	staged := make([]itemIndexEntry, 0, len(t.itemIndex))
	for _, entry := range t.itemIndex {
		staged = append(staged, entry)
	}
	sort.Slice(staged, func(i, j int) bool { return staged[i].outputIndex < staged[j].outputIndex })
	byID := make(map[string][]int)
	byCall := make(map[string][]int)
	for position, entry := range staged {
		if entry.identity.id != "" {
			byID[entry.identity.id] = append(byID[entry.identity.id], position)
		}
		if entry.identity.callID != "" {
			byCall[entry.identity.callID] = append(byCall[entry.identity.callID], position)
		}
	}

	owner := make([]int, len(staged))
	for position := range owner {
		owner[position] = -1
	}
	paired := make([]int, len(outputs))
	for position := range paired {
		paired[position] = -1
	}
	// Pass 1: stable ids.
	for position, item := range outputs {
		candidates := make([]int, 0, 2)
		add := func(list []int) {
			for _, stagedPosition := range list {
				if !item.identity.compatible(staged[stagedPosition].identity) {
					continue
				}
				if !containsInt(candidates, stagedPosition) {
					candidates = append(candidates, stagedPosition)
				}
			}
		}
		if item.identity.id != "" {
			add(byID[item.identity.id])
		}
		if item.identity.callID != "" {
			add(byCall[item.identity.callID])
		}
		switch len(candidates) {
		case 0:
		case 1:
			chosen := candidates[0]
			if owner[chosen] >= 0 {
				return nil, fmt.Errorf("completed output items %d and %d both match staged output_index %d (%s)", owner[chosen], position, staged[chosen].outputIndex, item.identity)
			}
			owner[chosen], paired[position] = position, chosen
		default:
			return nil, fmt.Errorf("completed output item %d (%s) matches %d staged items; the mapping is ambiguous", position, item.identity, len(candidates))
		}
	}
	// Pass 2: what is left, in order. Both sides have the same number left, because the
	// totals are equal and every pair above took one from each.
	var restOutputs, restStaged []int
	for position, chosen := range paired {
		if chosen < 0 {
			restOutputs = append(restOutputs, position)
		}
	}
	for position, ownerPosition := range owner {
		if ownerPosition < 0 {
			restStaged = append(restStaged, position)
		}
	}
	if len(restOutputs) != len(restStaged) {
		return nil, fmt.Errorf("staged item count %d does not match completed output count %d", len(staged), len(outputs))
	}
	for rank, position := range restOutputs {
		item := outputs[position]
		entry := staged[restStaged[rank]]
		if !item.identity.compatible(entry.identity) {
			return nil, fmt.Errorf("completed output item %d (%s) has no staged item: staged output_index %d is %s", position, item.identity, entry.outputIndex, entry.identity)
		}
		if entry.outputIndex != int64(position) {
			same, err := t.sameContent(entry, item.raw)
			if err != nil {
				return nil, err
			}
			if !same {
				return nil, fmt.Errorf("completed output item %d (%s) cannot be matched to staged output_index %d (%s): no shared id and different content", position, item.identity, entry.outputIndex, entry.identity)
			}
		}
		paired[position] = restStaged[rank]
	}
	order := make([]int64, len(outputs))
	for position, chosen := range paired {
		order[position] = staged[chosen].outputIndex
	}
	return order, nil
}

func containsInt(list []int, value int) bool {
	for _, candidate := range list {
		if candidate == value {
			return true
		}
	}
	return false
}

func (t *ResponseTurn) sameContent(entry itemIndexEntry, completed []byte) (bool, error) {
	stagedItem, err := t.readStagedItem(entry)
	if err != nil {
		return false, err
	}
	canonical, err := canonicalJSON(completed)
	if err != nil {
		return false, fmt.Errorf("serialize completed output item: %w", err)
	}
	return bytes.Equal(stagedItem, canonical), nil
}

// checkFunctionCalls compares each function call of the completed output with the staged
// item it was paired with. The run executes the calls of the completed output and the
// transcript keeps the staged items, so a call that differs between the two would run
// as one thing and be recorded as another.
func (t *ResponseTurn) checkFunctionCalls(outputs []completedItem, order []int64) error {
	for position, item := range outputs {
		if item.identity.typ != "function_call" {
			continue
		}
		entry := t.itemIndex[order[position]]
		stagedItem, err := t.readStagedItem(entry)
		if err != nil {
			return err
		}
		stagedFields, err := objectFieldsValid(stagedItem)
		if err != nil {
			return fmt.Errorf("decode staged item: %w", err)
		}
		completedFields, err := objectFields(item.raw)
		if err != nil {
			return errors.New("output item must be an object")
		}
		for _, key := range []string{"call_id", "name", "arguments"} {
			left, leftOK := fieldValue(stagedFields, key)
			right, rightOK := fieldValue(completedFields, key)
			if !leftOK || !rightOK {
				continue
			}
			leftCanonical, leftErr := canonicalJSON(left)
			rightCanonical, rightErr := canonicalJSON(right)
			if leftErr != nil || rightErr != nil || !bytes.Equal(leftCanonical, rightCanonical) {
				return fmt.Errorf("function call %d of the completed output differs from staged output_index %d in %s", position, entry.outputIndex, key)
			}
		}
	}
	return nil
}
