package sessiongate

import "testing"

func TestBeginRefusesAHeldKeyUntilEnd(t *testing.T) {
	var g Gate
	if !g.Begin("a") {
		t.Fatal("first Begin refused")
	}
	if g.Begin("a") {
		t.Fatal("second Begin of a held key took the gate")
	}
	if !g.Begin("b") {
		t.Fatal("another key was refused")
	}
	g.End("a")
	if g.Held("a") || !g.Begin("a") {
		t.Fatal("End did not free the key")
	}
}

func TestWhileFreeRunsOnlyWhenTheKeyIsFree(t *testing.T) {
	var g Gate
	ran := false
	fn := func() error { ran = true; return nil }
	if free, err := g.WhileFree("a", fn); !free || err != nil || !ran {
		t.Fatalf("free key: free=%t err=%v ran=%t", free, err, ran)
	}
	g.Begin("a")
	ran = false
	if free, _ := g.WhileFree("a", fn); free || ran {
		t.Fatalf("held key: free=%t ran=%t, want false and false", free, ran)
	}
}

func TestMarkRestoringIsClearedByItsFunction(t *testing.T) {
	var g Gate
	clear := g.MarkRestoring("a")
	if !g.Restoring("a") || g.Restoring("b") {
		t.Fatal("MarkRestoring did not mark only its key")
	}
	clear()
	if g.Restoring("a") {
		t.Fatal("the clear function did not unmark the key")
	}
}

func TestLockSerializesOneSession(t *testing.T) {
	var g Gate
	unlock := g.Lock("s")
	acquired := make(chan struct{})
	go func() {
		release := g.Lock("s")
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("a second Lock of the session did not wait")
	default:
	}
	unlock()
	<-acquired
}
