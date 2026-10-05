package tree

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree/treetest"
)

func file(id, p string) Entry {
	return Entry{SpaceID: "sp", FileID: id, Path: p, BlobID: "blob-" + id, Size: 10, MTime: time.Unix(1700000000, 0).UTC()}
}

func paths(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBlobKey(t *testing.T) {
	got := BlobKey("b1f74ec4-dd7e-11ef-a543-03775734d0f7", "ac09e55e-8c8c-4fa7-9b51-258afaf88d44")
	want := "b1f74ec4-dd7e-11ef-a543-03775734d0f7/ac/09/e5/5e/-8c8c-4fa7-9b51-258afaf88d44"
	if got != want {
		t.Errorf("BlobKey = %q, want %q", got, want)
	}
	if got := BlobKey("sp", "abc"); got != "sp/ab/c" {
		t.Errorf("short id = %q", got)
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{"./a/b": "/a/b", "/a/b/": "/a/b", "a": "/a", ".": "/", "": "/", "/": "/", "/a/../b": "/b"} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPutAndReadFile(t *testing.T) {
	m := New(treetest.New("p/"))
	ctx := context.Background()

	if err := m.PutFile(ctx, file("f1", "./movies/a.mp4")); err != nil {
		t.Fatal(err)
	}
	got, err := m.File(ctx, "sp", "/movies/a.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if got.BlobID != "blob-f1" || got.Path != "/movies/a.mp4" || got.BlobKey() != "sp/bl/ob/-f/1" {
		t.Errorf("entry %+v, key %s", got, got.BlobKey())
	}
	if _, err := m.File(ctx, "sp", "/movies/b.mp4"); err == nil {
		t.Error("unknown file found")
	}
}

func tree(t *testing.T) (*Tree, *treetest.Objects) {
	t.Helper()
	objects := treetest.New("p/")
	m := New(objects)
	for _, e := range []Entry{
		file("f1", "/movies/a.mp4"),
		file("f2", "/movies/sub/b.mp4"),
		file("f3", "/movies-2/c.mp4"),
		file("f4", "/root.mp4"),
	} {
		if err := m.PutFile(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	return m, objects
}

// A folder covers what is under its path and nothing that merely starts
// with it.
func TestWalkOfAFolder(t *testing.T) {
	m, _ := tree(t)
	var seen []Entry
	if err := m.Walk(context.Background(), "sp", "/movies", func(e Entry) error {
		seen = append(seen, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := paths(seen); !equal(got, []string{"/movies/a.mp4", "/movies/sub/b.mp4"}) {
		t.Errorf("walk of /movies = %v", got)
	}

	seen = nil
	_ = m.Walk(context.Background(), "sp", "/", func(e Entry) error { seen = append(seen, e); return nil })
	if len(seen) != 4 {
		t.Errorf("walk of the root found %d files", len(seen))
	}
}

func TestPathsOfAFolder(t *testing.T) {
	m, _ := tree(t)
	var got []string
	if err := m.Paths(context.Background(), "sp", "/movies", func(p string) error {
		got = append(got, p)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !equal(got, []string{"/movies/a.mp4", "/movies/sub/b.mp4"}) {
		t.Errorf("paths of /movies = %v", got)
	}

	got = nil
	_ = m.Paths(context.Background(), "sp", "/", func(p string) error { got = append(got, p); return nil })
	if len(got) != 4 {
		t.Errorf("paths of the root found %d files", len(got))
	}
}

func TestMoveFileAndFolder(t *testing.T) {
	m, _ := tree(t)
	ctx := context.Background()

	renamed, err := m.PlanMove(ctx, "sp", "/root.mp4", "/movies/root.mp4", false)
	if err != nil || len(renamed) != 1 || renamed[0].New.Path != "/movies/root.mp4" || renamed[0].New.BlobID != "blob-f4" {
		t.Fatalf("plan of a file move = %+v, %v", renamed, err)
	}
	if err := m.ApplyMove(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := m.File(ctx, "sp", "/root.mp4"); err == nil {
		t.Error("the old path is still there")
	}

	// The folder takes its files along, the sibling with the same prefix stays.
	renamed, err = m.PlanMove(ctx, "sp", "/movies", "/films", true)
	if err != nil {
		t.Fatal(err)
	}
	news := make([]Entry, 0, len(renamed))
	for _, r := range renamed {
		news = append(news, r.New)
	}
	if got := paths(news); !equal(got, []string{"/films/a.mp4", "/films/root.mp4", "/films/sub/b.mp4"}) {
		t.Errorf("plan of a folder move = %v", got)
	}
	if err := m.ApplyMove(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	// Applying the same plan again changes nothing.
	if err := m.ApplyMove(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := m.File(ctx, "sp", "/movies-2/c.mp4"); err != nil {
		t.Error("the sibling folder was moved")
	}
	if e, err := m.File(ctx, "sp", "/films/sub/b.mp4"); err != nil || e.FileID != "f2" {
		t.Errorf("moved file = %+v, %v", e, err)
	}

	// A move the tree knows nothing about plans nothing.
	if renamed, err := m.PlanMove(ctx, "sp", "/nowhere.mp4", "/x.mp4", false); err != nil || len(renamed) != 0 {
		t.Errorf("unknown move = %+v, %v", renamed, err)
	}
}

func TestTrashRestorePurgeOfAFolder(t *testing.T) {
	m, objects := tree(t)
	ctx := context.Background()
	folder := &Entry{SpaceID: "sp", FileID: "d1", Path: "/movies", IsDir: true}

	files, err := m.PlanTrash(ctx, "sp", "/movies", true)
	if err != nil || !equal(paths(files), []string{"/movies/a.mp4", "/movies/sub/b.mp4"}) {
		t.Fatalf("plan of a folder trash = %v, %v", paths(files), err)
	}
	if err := m.ApplyTrash(ctx, folder, files); err != nil {
		t.Fatal(err)
	}
	if _, err := m.File(ctx, "sp", "/movies/a.mp4"); err == nil {
		t.Error("a trashed file is still in the tree")
	}
	trashed, err := m.Trashed(ctx, "sp", "d1")
	if err != nil || !trashed.IsDir || len(trashed.Children) != 2 {
		t.Fatalf("folder in the trash bin = %+v, %v", trashed, err)
	}

	// An interrupted run recorded one child; the second run keeps it.
	if err := m.ApplyTrash(ctx, folder, files[:1]); err != nil {
		t.Fatal(err)
	}
	trashed, _ = m.Trashed(ctx, "sp", "d1")
	if len(trashed.Children) != 2 {
		t.Errorf("children after a partial rerun = %v", trashed.Children)
	}

	// Restored under another name: the files follow.
	item, restored, err := m.PlanRestore(ctx, "sp", "d1", "/movies (restored)")
	if err != nil || item == nil {
		t.Fatal(err)
	}
	if got := paths(restored); !equal(got, []string{"/movies (restored)/a.mp4", "/movies (restored)/sub/b.mp4"}) {
		t.Errorf("plan of a folder restore = %v", got)
	}
	if restored[0].BlobID == "" || restored[0].Children != nil || restored[0].IsDir {
		t.Errorf("restored file lost its shape: %+v", restored[0])
	}
	if err := m.ApplyRestore(ctx, item, restored); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Trashed(ctx, "sp", "d1"); err == nil {
		t.Error("the folder is still in the trash bin")
	}
	if e, err := m.File(ctx, "sp", "/movies (restored)/sub/b.mp4"); err != nil || e.BlobID != "blob-f2" {
		t.Errorf("restored file = %+v, %v", e, err)
	}

	// Trashed again and purged: gone for good.
	files, _ = m.PlanTrash(ctx, "sp", "/movies (restored)", true)
	_ = m.ApplyTrash(ctx, folder, files)
	item, purged, err := m.PlanPurge(ctx, "sp", "d1")
	if err != nil || len(purged) != 2 {
		t.Fatalf("plan of a purge = %v, %v", paths(purged), err)
	}
	if err := m.ApplyPurge(ctx, item, purged); err != nil {
		t.Fatal(err)
	}
	for _, key := range objects.Keys() {
		if strings.Contains(key, "/trash/") {
			t.Errorf("left in the trash bin: %s", key)
		}
	}

	// Nothing known: nothing planned.
	if item, files, err := m.PlanRestore(ctx, "sp", "unknown", "/x"); err != nil || item != nil || files != nil {
		t.Errorf("unknown restore = %v, %v, %v", item, files, err)
	}
}

func TestTrashAndRestoreOfAFile(t *testing.T) {
	m, _ := tree(t)
	ctx := context.Background()

	files, err := m.PlanTrash(ctx, "sp", "/root.mp4", false)
	if err != nil || len(files) != 1 {
		t.Fatal(err)
	}
	if err := m.ApplyTrash(ctx, nil, files); err != nil {
		t.Fatal(err)
	}
	item, restored, err := m.PlanRestore(ctx, "sp", "f4", "/root.mp4")
	if err != nil || item == nil || len(restored) != 1 || restored[0].Path != "/root.mp4" {
		t.Fatalf("restore of a file = %+v, %v, %v", item, restored, err)
	}
	if err := m.ApplyRestore(ctx, item, restored); err != nil {
		t.Fatal(err)
	}
	if e, err := m.File(ctx, "sp", "/root.mp4"); err != nil || e.BlobID != "blob-f4" {
		t.Errorf("after restore = %+v, %v", e, err)
	}
	if _, err := m.Trashed(ctx, "sp", "f4"); err == nil {
		t.Error("the file is still in the trash bin")
	}
}

func TestEmptyTrashAndDeleteSpace(t *testing.T) {
	m, objects := tree(t)
	ctx := context.Background()

	files, _ := m.PlanTrash(ctx, "sp", "/movies", true)
	_ = m.ApplyTrash(ctx, &Entry{SpaceID: "sp", FileID: "d1", Path: "/movies", IsDir: true}, files)

	emptied, err := m.PlanEmptyTrash(ctx, "sp")
	if err != nil || !equal(paths(emptied), []string{"/movies/a.mp4", "/movies/sub/b.mp4"}) {
		t.Fatalf("plan of an emptied trash bin = %v, %v", paths(emptied), err)
	}
	if err := m.ApplyEmptyTrash(ctx, "sp"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Trashed(ctx, "sp", "d1"); err == nil {
		t.Error("the folder survived the emptying")
	}

	if err := m.PutSpace(ctx, Space{ID: "sp", Name: "Creatives", Type: "project"}); err != nil {
		t.Fatal(err)
	}
	var spaces []Space
	_ = m.WalkSpaces(ctx, func(s Space) error { spaces = append(spaces, s); return nil })
	if len(spaces) != 1 || spaces[0].Name != "Creatives" {
		t.Errorf("spaces = %+v", spaces)
	}

	remaining, err := m.PlanDeleteSpace(ctx, "sp")
	if err != nil || !equal(paths(remaining), []string{"/movies-2/c.mp4", "/root.mp4"}) {
		t.Fatalf("plan of a space delete = %v, %v", paths(remaining), err)
	}
	if err := m.ApplyDeleteSpace(ctx, "sp"); err != nil {
		t.Fatal(err)
	}
	if keys := objects.Keys(); len(keys) != 0 {
		t.Errorf("left behind: %v", keys)
	}
}
