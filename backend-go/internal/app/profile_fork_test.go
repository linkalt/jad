package app

import "testing"

func TestSetTopLevelJSONKeyKeepsTheOrderOfTheOtherKeys(t *testing.T) {
	got, err := setTopLevelJSONKey([]byte(`{"b":1,"name":"old","a":{"x":2.50}}`), "name", "new")
	if err != nil || string(got) != "{\n  \"b\": 1,\n  \"name\": \"new\",\n  \"a\": {\n    \"x\": 2.50\n  }\n}\n" {
		t.Fatalf("replace: %q %v", got, err)
	}
	got, err = setTopLevelJSONKey([]byte(`{"b":1}`), "name", "added")
	if err != nil || string(got) != "{\n  \"b\": 1,\n  \"name\": \"added\"\n}\n" {
		t.Fatalf("add: %q %v", got, err)
	}
	if _, err := setTopLevelJSONKey([]byte(`[1]`), "name", "x"); err == nil {
		t.Fatal("an array was edited")
	}
}
