package preview

import (
	"testing"
	"time"
)

func TestIndexTrustsAStateForItsWindow(t *testing.T) {
	now := time.Now()
	idx := newIndex(10)
	idx.now = func() time.Time { return now }

	if _, _, ok := idx.lookup("f", "e"); ok {
		t.Fatal("hit on an empty index")
	}

	idx.set("f", "e", StateGenerating, "")
	if state, _, ok := idx.lookup("f", "e"); !ok || state != StateGenerating {
		t.Errorf("lookup = %v, %v", state, ok)
	}
	if _, _, ok := idx.lookup("f", "other"); ok {
		t.Error("hit for another version")
	}
	// The mismatch dropped the entry.
	if _, _, ok := idx.lookup("f", "e"); ok {
		t.Error("the entry survived a lookup of another version")
	}

	idx.set("f", "e", StateGenerating, "")
	now = now.Add(ttlGenerating + time.Millisecond)
	if _, _, ok := idx.lookup("f", "e"); ok {
		t.Error("generating trusted past its window")
	}

	idx.set("f", "e", StateReady, "m1")
	now = now.Add(ttlFailed * 2)
	if state, master, ok := idx.lookup("f", "e"); !ok || state != StateReady || master != "m1" {
		t.Errorf("ready = %v, %q, %v", state, master, ok)
	}
	now = now.Add(ttlReady)
	if _, _, ok := idx.lookup("f", "e"); ok {
		t.Error("ready trusted past its window")
	}
}

func TestIndexIsBounded(t *testing.T) {
	idx := newIndex(2)
	idx.set("a", "e", StateReady, "")
	idx.set("b", "e", StateReady, "")
	idx.set("c", "e", StateReady, "")

	if _, _, ok := idx.lookup("a", "e"); ok {
		t.Error("the oldest entry survived")
	}
	if _, _, ok := idx.lookup("c", "e"); !ok {
		t.Error("the newest entry went")
	}

	idx.forget("c")
	if _, _, ok := idx.lookup("c", "e"); ok {
		t.Error("forget kept the entry")
	}
	idx.forget("unknown")
}
