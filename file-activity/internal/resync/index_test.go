package resync

import (
	"testing"

	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

const (
	uuidA = "b1f74ec4-ac09-4e55-8c8c-4fa7b9512580"
	uuidB = "00000000-0000-4000-8000-ffffffffffff"
)

func TestPackUUID(t *testing.T) {
	for _, id := range []string{uuidA, uuidB, uuidText} {
		packed, ok := packUUID(id)
		if !ok || unpackUUID(packed) != id {
			t.Errorf("%s: packed %v, %v", id, ok, unpackUUID(packed))
		}
	}
	// Anything the round trip would not give back is refused.
	for _, s := range []string{"", "f-root", "B1F74EC4-AC09-4E55-8C8C-4FA7B9512580", "b1f74ec4ac094e558c8c4fa7b9512580", "b1f74ec4-ac09-4e55-8c8c-4fa7b951258g"} {
		if _, ok := packUUID(s); ok {
			t.Errorf("%q: packed", s)
		}
	}
	if packID("f-root") == packID("f-roof") {
		t.Error("ids that are not uuids are not told apart")
	}
	if packed, _ := packUUID(uuidA); packID(uuidA) != packed {
		t.Error("an id that is a uuid is not packed as one")
	}
}

func TestIndexLookups(t *testing.T) {
	x := newIndex()
	// One file at two paths, with a blob that is not a uuid at the first.
	x.add(tree.Entry{FileID: "f-1", Path: "/a", BlobID: "blob-1", ETag: "e1"})
	x.add(tree.Entry{FileID: "f-1", Path: "/b", BlobID: uuidA, ETag: "e1"})
	x.add(tree.Entry{FileID: uuidB, Path: "/c", ETag: "e2"})
	x.finish()
	if x.size() != 3 {
		t.Fatalf("size = %d", x.size())
	}

	// By id the record at the path wins, the first of the tree otherwise.
	if i := x.lookup("f-1", x.hash("/b")); i == none || x.blobOf(i) != uuidA {
		t.Errorf("lookup at /b = %d", i)
	}
	if i := x.lookup("f-1", x.hash("/nowhere")); i == none || x.blobOf(i) != "blob-1" {
		t.Errorf("lookup elsewhere = %d", i)
	}
	if i := x.lookup("f-3", 0); i != none {
		t.Errorf("lookup of an unknown id = %d", i)
	}

	// By path.
	if i := x.atPath("/c"); i == none || x.blobOf(i) != "" || x.rec(i).etag != x.hash("e2") {
		t.Errorf("at /c = %d", i)
	}
	if i := x.atPath("/d"); i != none {
		t.Errorf("at /d = %d", i)
	}

	// Seen at one path tells on the other record of the id.
	x.see(x.atPath("/b"), x.hash("/b"))
	if !x.seenElsewhere(x.atPath("/a")) || x.seenElsewhere(x.atPath("/b")) || x.seenElsewhere(x.atPath("/c")) {
		t.Error("seen elsewhere is wrong")
	}

	// Same is the path, the attributes and the blob.
	e := tree.Entry{FileID: uuidB, Path: "/c", ETag: "e2"}
	if !x.same(x.atPath("/c"), e, x.hash("/c")) {
		t.Error("the same entry is not the same")
	}
	if x.same(x.atPath("/c"), e, x.hash("/d")) {
		t.Error("another path is the same")
	}
	e.Size = 1
	if x.same(x.atPath("/c"), e, x.hash("/c")) {
		t.Error("another size is the same")
	}
	e.Size, e.BlobID = 0, uuidA
	if x.same(x.atPath("/c"), e, x.hash("/c")) {
		t.Error("another blob is the same")
	}
}
