package native

import (
	"sort"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// MemoryGroup counts the records of one type that share a value at a field path.
type MemoryGroup struct {
	Value     any
	Canonical string
	Count     int
}

// MemoryGroupResult is a bounded count of matching records by field value.
// Ungrouped counts matching records without the field. AtLeast is true when a
// scan limit stopped the scan, so every count is a lower bound.
type MemoryGroupResult struct {
	Groups    []MemoryGroup
	Ungrouped int
	AtLeast   bool
}

// GroupBy scans the records of recordType that match filter and excludeAny and
// counts them by the value at path. The scan is bounded by the policy's
// MaxScanRecords and MaxScanBytes. Groups are ordered by count, then by value.
func (s *MemoryStore) GroupBy(recordType string, filter map[string]any, excludeAny []map[string]any, path string, policy sessionfs.NativeMemorySettings) (MemoryGroupResult, error) {
	var result MemoryGroupResult
	err := s.withLock(func() error {
		if err := s.checkType(recordType, policy); err != nil {
			return err
		}
		names, err := s.files(recordType)
		if err != nil {
			return err
		}
		sort.Strings(names)
		counted := map[string]*MemoryGroup{}
		var scanned, scannedBytes int64
		for _, name := range names {
			if scanned >= policy.MaxScanRecords {
				result.AtLeast = true
				break
			}
			scanned++
			text, readErr := s.readRecordText(recordType, name)
			if readErr != nil {
				continue
			}
			if scannedBytes+int64(len(text)) > policy.MaxScanBytes {
				result.AtLeast = true
				break
			}
			scannedBytes += int64(len(text))
			value, parseErr := parseMemoryJSON([]byte(text))
			if parseErr != nil || value.kind != memoryObject {
				continue
			}
			data := memoryGoMap(value)
			if !memoryMatches(data, filter, excludeAny) {
				continue
			}
			fieldValue, found := memoryValueAt(data, path)
			if !found {
				result.Ungrouped++
				continue
			}
			canonical, canonicalErr := memoryJSONCanonical(memoryValueFromGo(fieldValue))
			if canonicalErr != nil {
				result.Ungrouped++
				continue
			}
			group := counted[canonical]
			if group == nil {
				group = &MemoryGroup{Value: fieldValue, Canonical: canonical}
				counted[canonical] = group
			}
			group.Count++
		}
		result.Groups = make([]MemoryGroup, 0, len(counted))
		for _, group := range counted {
			result.Groups = append(result.Groups, *group)
		}
		sort.Slice(result.Groups, func(i, j int) bool {
			if result.Groups[i].Count != result.Groups[j].Count {
				return result.Groups[i].Count > result.Groups[j].Count
			}
			return result.Groups[i].Canonical < result.Groups[j].Canonical
		})
		return nil
	})
	return result, err
}
