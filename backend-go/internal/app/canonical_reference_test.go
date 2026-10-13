package app

import (
	"testing"

	"jad/backend-go/internal/sessionfs"
)

func TestCanonicalSessionReferenceIsOneNamePerSession(t *testing.T) {
	one := &Resolver{roots: []sessionfs.Root{{ID: "local", Directory: t.TempDir()}}}
	for _, spelling := range []string{"demo", "local:demo"} {
		got, err := one.CanonicalSessionReference(spelling)
		if err != nil || got != "demo" {
			t.Fatalf("one root, %q = %q, %v; want demo", spelling, got, err)
		}
	}
	two := &Resolver{roots: []sessionfs.Root{{ID: "a", Directory: t.TempDir()}, {ID: "b", Directory: t.TempDir()}}}
	got, err := two.CanonicalSessionReference("b:demo")
	if err != nil || got != "b:demo" {
		t.Fatalf("two roots, b:demo = %q, %v; want b:demo", got, err)
	}
	if _, err := two.CanonicalSessionReference("../x"); err == nil {
		t.Fatal("an invalid reference was accepted")
	}
}
