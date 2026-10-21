package webui

import (
	"reflect"
	"testing"
)

func TestPartsEqualCoversEveryPartField(t *testing.T) {
	want := []string{"Type", "Text", "CallID", "Name", "Arguments", "Output", "Status", "Target", "Raw", "Citations", "Images", "InlineImages", "Files", "Partial", "Truncated"}
	partType := reflect.TypeOf(Part{})
	if partType.NumField() != len(want) {
		t.Fatalf("Part has %d fields, partsEqual covers %d: compare the new field in partsEqual and update this list", partType.NumField(), len(want))
	}
	for index, name := range want {
		if partType.Field(index).Name != name {
			t.Fatalf("Part field %d = %s, want %s: update partsEqual and this list", index, partType.Field(index).Name, name)
		}
	}
}

func partVariants() []Part {
	str := func(s string) *string { return &s }
	return []Part{
		{},
		{Type: "text", Text: "a"},
		{Type: "text", Text: "b"},
		{CallID: "c"},
		{Name: "n"},
		{Arguments: str("")},
		{Arguments: str("{}")},
		{Arguments: str("{}")},
		{Output: "o"},
		{Status: "s"},
		{Target: "t"},
		{Raw: "r"},
		{Citations: []Citation{}},
		{Citations: []Citation{{URL: "u"}}},
		{Citations: []Citation{{URL: "u", Title: "t"}}},
		{Images: []string{}},
		{Images: []string{"a.png"}},
		{Images: []string{"b.png"}},
		{InlineImages: 2},
		{Files: []string{}},
		{Files: []string{"f"}},
		{Partial: true},
		{Truncated: &PartTruncation{}},
		{Truncated: &PartTruncation{Bytes: 5}},
		{Truncated: &PartTruncation{Bytes: 5}},
		{Type: "tool_call", CallID: "c", Name: "n", Arguments: str("{"), Partial: true},
	}
}

func TestPartsEqualMatchesDeepEqual(t *testing.T) {
	variants := partVariants()
	for i, a := range variants {
		for j, b := range variants {
			if got, want := partsEqual(a, b), reflect.DeepEqual(a, b); got != want {
				t.Fatalf("partsEqual(variant %d, variant %d) = %v, DeepEqual = %v", i, j, got, want)
			}
		}
	}
}

func TestSliceComparisonsMatchDeepEqual(t *testing.T) {
	steps := [][]Step{nil, {}, {{ID: "a"}}, {{ID: "a"}}, {{ID: "a", EndPart: 1}}, {{ID: "a"}, {ID: "b"}}}
	for i, a := range steps {
		for j, b := range steps {
			if got, want := stepsEqual(a, b), reflect.DeepEqual(a, b); got != want {
				t.Fatalf("stepsEqual(%d, %d) = %v, DeepEqual = %v", i, j, got, want)
			}
		}
	}
	citations := [][]Citation{nil, {}, {{URL: "a"}}, {{URL: "a"}}, {{URL: "a", Title: "t"}}, {{URL: "a"}, {URL: "b"}}}
	for i, a := range citations {
		for j, b := range citations {
			if got, want := citationsEqual(a, b), reflect.DeepEqual(a, b); got != want {
				t.Fatalf("citationsEqual(%d, %d) = %v, DeepEqual = %v", i, j, got, want)
			}
		}
	}
}
