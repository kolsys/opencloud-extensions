package ingest

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/ocevents"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

// The space of the fixtures taken off the stand.
const space = "b1f74ec4-dd7e-11ef-a543-03775734d0f7"

var root = &ocevents.ResourceID{StorageID: "storage", SpaceID: space, OpaqueID: space}

func seed(t *testing.T, m *tree.Tree, files map[string]string) {
	t.Helper()
	for p, id := range files {
		e := tree.Entry{SpaceID: space, FileID: id, Path: p, BlobID: "blob-" + id, Size: 7, Mime: "video/mp4", ETag: "e-" + id, MTime: time.Unix(1700000000, 0).UTC(), SHA1: "sum-" + id}
		if err := m.PutFile(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

func apply(t *testing.T, mapped *Mapped) {
	t.Helper()
	if mapped.Apply == nil {
		t.Fatal("nothing to apply")
	}
	if err := mapped.Apply(t.Context()); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func pathsOf(events []feed.Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events[1:] {
		out = append(out, *e.Path)
	}
	sort.Strings(out)
	return out
}

func same(a, b []string) bool {
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

func TestUploadRecordsTheBlobInTheTree(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"./probe.bin": info(space, "file-1", "probe.bin", "application/octet-stream", false),
	}}
	mapper, m := newMapperWithTree(resolver)

	mapped, err := mapper.Map(t.Context(), load(t, "UploadReady"))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, mapped)

	e, err := m.File(t.Context(), space, "/probe.bin")
	if err != nil {
		t.Fatal(err)
	}
	if e.BlobID != "ac09e55e-8c8c-4fa7-9b51-258afaf88d44" || e.FileID != "file-1" || e.SHA1 == "" || e.Size != 2000000 {
		t.Errorf("tree entry = %+v", e)
	}
	if e.BlobKey() != space+"/ac/09/e5/5e/-8c8c-4fa7-9b51-258afaf88d44" {
		t.Errorf("blob key = %s", e.BlobKey())
	}
}

// A trashed file is looked up in the tree, not on the platform, and the
// entry carries what the tree knew.
func TestTrashedFileCarriesItsBlob(t *testing.T) {
	mapper, m := newMapperWithTree(&fakeResolver{})
	seed(t, m, map[string]string{renamedPath: "f9273736-7d7f-4491-ad89-6b24eaf314b3", "/other.mp4": "o1"})

	mapped, err := mapper.Map(t.Context(), load(t, "ItemTrashed"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Events) != 1 {
		t.Fatalf("%d entries for a trashed file", len(mapped.Events))
	}
	e := mapped.Events[0]
	if e.BlobID != "blob-f9273736-7d7f-4491-ad89-6b24eaf314b3" || e.Size != 7 || e.Checksum != "sha1:sum-f9273736-7d7f-4491-ad89-6b24eaf314b3" || e.IsDir {
		t.Errorf("entry = %+v", e)
	}
	apply(t, mapped)

	if _, err := m.File(t.Context(), space, renamedPath); err == nil {
		t.Error("the trashed file is still in the tree")
	}
	if trashed, err := m.Trashed(t.Context(), space, "f9273736-7d7f-4491-ad89-6b24eaf314b3"); err != nil || trashed.Path != renamedPath {
		t.Errorf("trash entry = %+v, %v", trashed, err)
	}
	if _, err := m.File(t.Context(), space, "/other.mp4"); err != nil {
		t.Error("the other file went too")
	}
}

func trashFolder(t *testing.T, mapper *Mapper) *Mapped {
	t.Helper()
	event := ocevents.Event{
		ID:   "trash-1",
		Type: "events.ItemTrashed",
		Payload: &ocevents.ItemTrashed{
			ID:  &ocevents.ResourceID{StorageID: "storage", SpaceID: space, OpaqueID: "d1"},
			Ref: &ocevents.Reference{ResourceID: root, Path: "./folder"},
		},
	}
	mapped, err := mapper.Map(t.Context(), event)
	if err != nil {
		t.Fatal(err)
	}
	return mapped
}

// A trashed folder becomes one entry for the folder and one per file under
// it, and the tree moves the files into the trash bin.
func TestTrashedFolderExpands(t *testing.T) {
	mapper, m := newMapperWithTree(&fakeResolver{})
	seed(t, m, map[string]string{"/folder/a.mp4": "fa", "/folder/sub/b.mp4": "fb", "/folder-2/c.mp4": "fc", "/other.mp4": "o1"})

	mapped := trashFolder(t, mapper)
	if len(mapped.Events) != 3 {
		t.Fatalf("%d entries, want the folder and two files", len(mapped.Events))
	}
	folder := mapped.Events[0]
	if folder.Type != feed.Trashed || !folder.IsDir || *folder.Path != "/folder" || folder.FileID != "d1" || folder.ID != "trash-1" {
		t.Errorf("folder entry = %+v", folder)
	}
	if got := pathsOf(mapped.Events); !same(got, []string{"/folder/a.mp4", "/folder/sub/b.mp4"}) {
		t.Errorf("file entries = %v", got)
	}
	for _, e := range mapped.Events[1:] {
		if e.Type != feed.Trashed || e.IsDir || e.BlobID != "blob-"+e.FileID || e.ID != "trash-1/"+e.FileID || e.SpaceID != space {
			t.Errorf("file entry = %+v", e)
		}
		if e.ResourceID != "storage$"+space+"!"+e.FileID {
			t.Errorf("resource id = %q", e.ResourceID)
		}
	}
	apply(t, mapped)

	if _, err := m.File(t.Context(), space, "/folder/a.mp4"); err == nil {
		t.Error("a file of the folder is still in the tree")
	}
	if _, err := m.File(t.Context(), space, "/folder-2/c.mp4"); err != nil {
		t.Error("the sibling with the same prefix went too")
	}
	trashed, err := m.Trashed(t.Context(), space, "d1")
	if err != nil || len(trashed.Children) != 2 {
		t.Errorf("folder in the trash bin = %+v, %v", trashed, err)
	}
}

func TestRestoredFolderExpands(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"./folder (1)": info(space, "d1", "folder (1)", "httpd/unix-directory", true),
	}}
	mapper, m := newMapperWithTree(resolver)
	seed(t, m, map[string]string{"/folder/a.mp4": "fa", "/folder/sub/b.mp4": "fb"})
	apply(t, trashFolder(t, mapper))

	event := ocevents.Event{
		ID:   "restore-1",
		Type: "events.ItemRestored",
		Payload: &ocevents.ItemRestored{
			Key: "d1",
			Ref: &ocevents.Reference{ResourceID: root, Path: "./folder (1)"},
		},
	}
	mapped, err := mapper.Map(t.Context(), event)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Events) != 3 || mapped.Events[0].Type != feed.Restored || !mapped.Events[0].IsDir {
		t.Fatalf("entries = %+v", mapped.Events)
	}
	if got := pathsOf(mapped.Events); !same(got, []string{"/folder (1)/a.mp4", "/folder (1)/sub/b.mp4"}) {
		t.Errorf("restored files = %v", got)
	}
	if e := mapped.Events[1]; e.Type != feed.Restored || e.BlobID != "blob-"+e.FileID {
		t.Errorf("restored file entry = %+v", e)
	}
	apply(t, mapped)

	if e, err := m.File(t.Context(), space, "/folder (1)/sub/b.mp4"); err != nil || e.BlobID != "blob-fb" {
		t.Errorf("restored in the tree = %+v, %v", e, err)
	}
	if _, err := m.Trashed(t.Context(), space, "d1"); err == nil {
		t.Error("the folder is still in the trash bin")
	}
}

func TestMovedFolderExpands(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"/films": info(space, "d1", "/films", "httpd/unix-directory", true),
	}}
	mapper, m := newMapperWithTree(resolver)
	seed(t, m, map[string]string{"/folder/a.mp4": "fa", "/folder/sub/b.mp4": "fb", "/other.mp4": "o1"})

	event := ocevents.Event{
		ID:   "move-1",
		Type: "events.ItemMoved",
		Payload: &ocevents.ItemMoved{
			Ref:          &ocevents.Reference{ResourceID: root, Path: "/films"},
			OldReference: &ocevents.Reference{ResourceID: root, Path: "/folder"},
		},
	}
	mapped, err := mapper.Map(t.Context(), event)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Events) != 3 || *mapped.Events[0].OldPath != "/folder" || *mapped.Events[0].Path != "/films" {
		t.Fatalf("entries = %+v", mapped.Events)
	}
	for _, e := range mapped.Events[1:] {
		if e.Type != feed.Moved || e.OldPath == nil || e.BlobID == "" {
			t.Errorf("moved file entry = %+v", e)
		}
		if *e.OldPath == "/folder/sub/b.mp4" && *e.Path != "/films/sub/b.mp4" {
			t.Errorf("moved to %s", *e.Path)
		}
	}
	apply(t, mapped)

	if _, err := m.File(t.Context(), space, "/folder/a.mp4"); err == nil {
		t.Error("the old path is still in the tree")
	}
	if e, err := m.File(t.Context(), space, "/films/a.mp4"); err != nil || e.BlobID != "blob-fa" {
		t.Errorf("moved in the tree = %+v, %v", e, err)
	}
}

// A file the tree never saw is recorded at its new path without a blob,
// so that restore can report it.
func TestMovedUnknownFileIsRecorded(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"/folder/renamed.mp4": info(space, "file-3", "/folder/renamed.mp4", "video/mp4", false),
	}}
	mapper, m := newMapperWithTree(resolver)

	mapped, err := mapper.Map(t.Context(), load(t, "ItemMoved-2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Events) != 1 || mapped.Events[0].BlobID != "" {
		t.Errorf("entries = %+v", mapped.Events)
	}
	apply(t, mapped)
	if e, err := m.File(t.Context(), space, renamedPath); err != nil || e.BlobID != "" || e.FileID != "file-3" {
		t.Errorf("recorded = %+v, %v", e, err)
	}
}

func TestPurgedFileFromTheMirror(t *testing.T) {
	mapper, m := newMapperWithTree(&fakeResolver{})
	seed(t, m, map[string]string{renamedPath: "f9273736-7d7f-4491-ad89-6b24eaf314b3"})
	trashed, err := mapper.Map(t.Context(), load(t, "ItemTrashed"))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, trashed)

	mapped, err := mapper.Map(t.Context(), load(t, "ItemPurged"))
	if err != nil {
		t.Fatal(err)
	}
	e := mapped.Events[0]
	if e.Type != feed.Purged || e.Path == nil || *e.Path != renamedPath || e.BlobID == "" {
		t.Errorf("purged entry = %+v", e)
	}
	apply(t, mapped)
	if _, err := m.Trashed(t.Context(), space, e.FileID); err == nil {
		t.Error("the purged file is still in the trash bin")
	}
}

func TestEmptiedTrashBinExpands(t *testing.T) {
	mapper, m := newMapperWithTree(&fakeResolver{})
	seed(t, m, map[string]string{"/folder/a.mp4": "fa", "/folder/sub/b.mp4": "fb", "/kept.mp4": "k1"})
	apply(t, trashFolder(t, mapper))

	// The fixture was taken while emptying the trash bin of this space.
	mapped, err := mapper.Map(t.Context(), load(t, "TrashbinPurged"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped.Events) != 2 {
		t.Fatalf("%d entries, want one per file in the trash bin", len(mapped.Events))
	}
	for _, e := range mapped.Events {
		if e.Type != feed.Purged || e.IsDir || e.BlobID == "" || e.Path == nil {
			t.Errorf("entry = %+v", e)
		}
	}
	apply(t, mapped)
	if _, err := m.Trashed(t.Context(), space, "d1"); err == nil {
		t.Error("the trash bin was not emptied")
	}
	if _, err := m.File(t.Context(), space, "/kept.mp4"); err != nil {
		t.Error("a file of the tree went with the trash bin")
	}
}

func TestSpaceEventsKeepTheMirror(t *testing.T) {
	mapper, m := newMapperWithTree(&fakeResolver{})

	created, err := mapper.Map(t.Context(), load(t, "SpaceCreated"))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, created)
	s, err := m.Space(t.Context(), created.Events[0].SpaceID)
	if err != nil || s.Name != "Admin" || s.Type != "personal" || s.Owner == "" {
		t.Errorf("space = %+v, %v", s, err)
	}

	renamed, err := mapper.Map(t.Context(), load(t, "SpaceRenamed"))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, renamed)
	if s, err := m.Space(t.Context(), "5b356174-308d-4424-b6f9-60cd9cf282ec"); err != nil || s.Name != "Fixtures renamed" {
		t.Errorf("renamed space = %+v, %v", s, err)
	}

	// The deleted space takes its files with it, each reported.
	for p, id := range map[string]string{"/a.mp4": "a", "/b/c.mp4": "c"} {
		_ = m.PutFile(t.Context(), tree.Entry{SpaceID: "5b356174-308d-4424-b6f9-60cd9cf282ec", FileID: id, Path: p, BlobID: "blob-" + id})
	}
	deleted, err := mapper.Map(t.Context(), load(t, "SpaceDeleted"))
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted.Events) != 3 || deleted.Events[0].Type != feed.SpaceDeleted || deleted.Events[1].Type != feed.Purged {
		t.Fatalf("entries = %+v", deleted.Events)
	}
	apply(t, deleted)
	if _, err := m.Space(t.Context(), "5b356174-308d-4424-b6f9-60cd9cf282ec"); err == nil {
		t.Error("the space survived")
	}
	var left int
	_ = m.Walk(t.Context(), "5b356174-308d-4424-b6f9-60cd9cf282ec", "/", func(tree.Entry) error { left++; return nil })
	if left != 0 {
		t.Errorf("%d files left in the deleted space", left)
	}
}
