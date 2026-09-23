package ingest

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/ocevents"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree/treetest"
)

// The fixtures of the decoder are envelopes taken off a stand; the mapper is
// run over them with a resolver that answers what the stand would.
const fixtures = "../../../common/ocevents/testdata"

// Paths the fixtures name.
const (
	folderPath  = "/folder"
	renamedPath = "/folder/renamed.mp4"
)

// fakeResolver answers a Stat from a table keyed by the path or the id of
// the reference, and records what it was asked.
type fakeResolver struct {
	byPath map[string]*cs3.ResourceInfo
	byID   map[string]*cs3.ResourceInfo
	asked  []cs3.Ref
	err    error
}

// Stat mirrors the platform: a reference by path answers with the name of
// the resource only, a reference by id answers with the full path.
func (f *fakeResolver) Stat(_ context.Context, ref cs3.Ref) (*cs3.ResourceInfo, error) {
	f.asked = append(f.asked, ref)
	if f.err != nil {
		return nil, f.err
	}
	if ref.Path != "" {
		info, ok := f.byPath[ref.Path]
		if !ok {
			return nil, cs3.ErrNotFound
		}
		copied := *info
		copied.Path = copied.Name
		return &copied, nil
	}
	if info, ok := f.byID[ref.OpaqueID]; ok {
		return info, nil
	}
	return nil, cs3.ErrNotFound
}

func load(t *testing.T, name string) ocevents.Event {
	t.Helper()

	message, err := os.ReadFile(filepath.Join(fixtures, name+".json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	event, err := ocevents.Decode(message)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return event
}

func info(space, id, path, mime string, dir bool) *cs3.ResourceInfo {
	i := &cs3.ResourceInfo{
		ID:       cs3.Ref{StorageID: "storage", SpaceID: space, OpaqueID: id},
		Path:     path,
		Name:     filepath.Base(path),
		IsDir:    dir,
		Size:     2000000,
		MimeType: mime,
		ETag:     `"etag-1"`,
		MTime:    time.Unix(1700000000, 0).UTC(),
	}
	if !dir {
		i.SHA1 = "da39a3ee5e6b4b0d3255bfef95601890afd80709"
	}
	return i
}

// newMapper returns a mapper over an empty in-memory tree.
func newMapper(resolver Resolver) *Mapper {
	mapper, _ := newMapperWithTree(resolver)
	return mapper
}

func newMapperWithTree(resolver Resolver) (*Mapper, *tree.Tree) {
	m := tree.New(treetest.New(""))
	return NewMapper(resolver, m, slog.New(slog.DiscardHandler)), m
}

// one unwraps the first entry of a mapping, the one about the item itself.
func one(mapped *Mapped, err error) (*feed.Event, error) {
	if err != nil {
		return nil, err
	}
	if len(mapped.Events) == 0 {
		return nil, errors.New("no entries")
	}
	return &mapped.Events[0], nil
}

func TestMapUploadReady(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		// A Stat by root plus relative path answers a relative path.
		"./probe.bin": info("space-a", "file-1", "probe.bin", "application/octet-stream", false),
	}}
	entry, err := one(newMapper(resolver).Map(t.Context(), load(t, "UploadReady")))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}

	if entry.Type != feed.FileCreated {
		t.Errorf("Type = %s", entry.Type)
	}
	if entry.FileID != "file-1" || entry.SpaceID != "space-a" || entry.ResourceID != "storage$space-a!file-1" {
		t.Errorf("ids = %s %s %s", entry.SpaceID, entry.FileID, entry.ResourceID)
	}
	if entry.Path == nil || *entry.Path != "/probe.bin" {
		t.Errorf("Path = %v, want /probe.bin", entry.Path)
	}
	if entry.ETag != "etag-1" {
		t.Errorf("ETag = %q, the quotes were not stripped", entry.ETag)
	}
	if entry.BlobID != "ac09e55e-8c8c-4fa7-9b51-258afaf88d44" || entry.Checksum != "sha1:da39a3ee5e6b4b0d3255bfef95601890afd80709" || entry.MTime == nil {
		t.Errorf("blob %q, checksum %q, mtime %v", entry.BlobID, entry.Checksum, entry.MTime)
	}
	if entry.Mime == "" || entry.Size == 0 || entry.IsDir {
		t.Errorf("attributes = %+v", entry)
	}
	if entry.Actor == nil || entry.Actor.Name != "alan" {
		t.Errorf("Actor = %+v, want the uploading user with a name", entry.Actor)
	}
	if entry.ID == "" || entry.TS.IsZero() {
		t.Errorf("ID = %q, TS = %s", entry.ID, entry.TS)
	}
	if len(resolver.asked) != 1 || resolver.asked[0].SpaceID == "" {
		t.Errorf("asked = %+v, want one Stat on the space root plus path", resolver.asked)
	}
}

// The path of a nested file comes from the event: the Stat by the reference
// of the event knows only the name.
func TestMapKeepsTheFullPathOfANestedFile(t *testing.T) {
	root := &ocevents.ResourceID{StorageID: "storage", SpaceID: "space-a", OpaqueID: "space-a"}
	event := ocevents.Event{
		ID:      "nested",
		Type:    "events.UploadReady",
		Payload: &ocevents.UploadReady{FileRef: &ocevents.Reference{ResourceID: root, Path: "./feed-test/deep/a.mp4"}, Filename: "a.mp4"},
	}
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"./feed-test/deep/a.mp4": info("space-a", "file-9", "feed-test/deep/a.mp4", "video/mp4", false),
	}}

	entry, err := one(newMapper(resolver).Map(t.Context(), event))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Path == nil || *entry.Path != "/feed-test/deep/a.mp4" {
		t.Errorf("Path = %v, want the full path of the event", entry.Path)
	}
	if len(resolver.asked) != 1 {
		t.Errorf("%d lookups for a path anchored at the space root, want 1", len(resolver.asked))
	}
}

// A reference anchored at a folder rather than the space root cannot be
// trusted for the path: the resolved id is looked up again.
func TestMapResolvesThePathOfAReferenceBelowAFolder(t *testing.T) {
	folder := &ocevents.ResourceID{StorageID: "storage", SpaceID: "space-a", OpaqueID: "dir-7"}
	event := ocevents.Event{
		ID:      "below",
		Type:    "events.ContainerCreated",
		Payload: &ocevents.ContainerCreated{Ref: &ocevents.Reference{ResourceID: folder, Path: "./sub"}},
	}
	resolver := &fakeResolver{
		byPath: map[string]*cs3.ResourceInfo{"./sub": info("space-a", "dir-8", "parent/sub", "httpd/unix-directory", true)},
		byID:   map[string]*cs3.ResourceInfo{"dir-8": info("space-a", "dir-8", "/parent/sub", "httpd/unix-directory", true)},
	}

	entry, err := one(newMapper(resolver).Map(t.Context(), event))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Path == nil || *entry.Path != "/parent/sub" {
		t.Errorf("Path = %v, want the path of the second Stat", entry.Path)
	}
	if len(resolver.asked) != 2 || resolver.asked[1].Path != "" {
		t.Errorf("asked = %+v, want a second Stat by id", resolver.asked)
	}
}

func TestMapUploadReadyOfANewVersionIsAnUpdate(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"./clip.mp4": info("space-a", "file-2", "clip.mp4", "video/mp4", false),
	}}
	entry, err := one(newMapper(resolver).Map(t.Context(), load(t, "UploadReady-version")))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Type != feed.FileUpdated {
		t.Errorf("Type = %s, want file_updated for IsVersion", entry.Type)
	}
}

func TestMapContainerCreated(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"./folder": info("space-a", "dir-1", "folder", "httpd/unix-directory", true),
	}}
	entry, err := one(newMapper(resolver).Map(t.Context(), load(t, "ContainerCreated")))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Type != feed.FolderCreated || !entry.IsDir || *entry.Path != folderPath {
		t.Errorf("entry = %+v", entry)
	}
	if entry.Actor == nil || entry.Actor.ID == "" {
		t.Error("Actor is missing, the executant was not taken")
	}
}

// A trashed item cannot be looked up any more: the ids come from the event
// and the path is the one the item had.
func TestMapItemTrashedNeedsNoStat(t *testing.T) {
	resolver := &fakeResolver{}
	entry, err := one(newMapper(resolver).Map(t.Context(), load(t, "ItemTrashed")))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Type != feed.Trashed {
		t.Errorf("Type = %s", entry.Type)
	}
	if entry.FileID != "f9273736-7d7f-4491-ad89-6b24eaf314b3" {
		t.Errorf("FileID = %q, want the id of the trashed item", entry.FileID)
	}
	if entry.SpaceID == "" {
		t.Error("SpaceID is empty")
	}
	if entry.Path == nil || *entry.Path != renamedPath {
		t.Errorf("Path = %v, want the last path of the item", entry.Path)
	}
	if len(resolver.asked) != 0 {
		t.Errorf("a trashed item was looked up: %+v", resolver.asked)
	}
}

// ItemPurged leaves ID empty on the wire; the ids are in Ref and the path is
// gone for good.
func TestMapItemPurgedTakesIDsFromRef(t *testing.T) {
	entry, err := one(newMapper(&fakeResolver{}).Map(t.Context(), load(t, "ItemPurged")))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Type != feed.Purged {
		t.Errorf("Type = %s", entry.Type)
	}
	if entry.FileID != "f9273736-7d7f-4491-ad89-6b24eaf314b3" || entry.SpaceID != "b1f74ec4-dd7e-11ef-a543-03775734d0f7" {
		t.Errorf("ids = %s / %s", entry.SpaceID, entry.FileID)
	}
	if entry.Path != nil {
		t.Errorf("Path = %q, want null for a purged item", *entry.Path)
	}
}

func TestMapItemRestored(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"./back.mp4": info("space-a", "fd142f17-47bc-4a00-9f76-81ae0cb2acc3", "back.mp4", "video/mp4", false),
	}}
	entry, err := one(newMapper(resolver).Map(t.Context(), load(t, "ItemRestored")))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Type != feed.Restored || *entry.Path != "/back.mp4" {
		t.Errorf("entry = %+v", entry)
	}
	if entry.FileID != "fd142f17-47bc-4a00-9f76-81ae0cb2acc3" {
		t.Errorf("FileID = %q", entry.FileID)
	}
}

func TestMapItemMovedKeepsBothPaths(t *testing.T) {
	resolver := &fakeResolver{byPath: map[string]*cs3.ResourceInfo{
		"/folder/renamed.mp4": info("space-a", "file-3", "/folder/renamed.mp4", "video/mp4", false),
	}}
	entry, err := one(newMapper(resolver).Map(t.Context(), load(t, "ItemMoved-2")))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Type != feed.Moved {
		t.Errorf("Type = %s", entry.Type)
	}
	if entry.Path == nil || *entry.Path != renamedPath {
		t.Errorf("Path = %v", entry.Path)
	}
	if entry.OldPath == nil || *entry.OldPath != "/renamed.mp4" {
		t.Errorf("OldPath = %v", entry.OldPath)
	}
}

// FileVersionRestored points at the file by id only.
func TestMapFileVersionRestoredStatsByID(t *testing.T) {
	resolver := &fakeResolver{byID: map[string]*cs3.ResourceInfo{
		"fd142f17-47bc-4a00-9f76-81ae0cb2acc3": info("space-a", "fd142f17-47bc-4a00-9f76-81ae0cb2acc3", "/back.mp4", "video/mp4", false),
	}}
	entry, err := one(newMapper(resolver).Map(t.Context(), load(t, "FileVersionRestored")))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if entry.Type != feed.FileUpdated || *entry.Path != "/back.mp4" {
		t.Errorf("entry = %+v", entry)
	}
	if len(resolver.asked) != 1 || resolver.asked[0].Path != "" {
		t.Errorf("asked = %+v, want a Stat by id", resolver.asked)
	}
}

func TestMapSpaceEvents(t *testing.T) {
	mapper := newMapper(&fakeResolver{})

	created, err := one(mapper.Map(t.Context(), load(t, "SpaceCreated")))
	if err != nil {
		t.Fatalf("Map SpaceCreated: %v", err)
	}
	if created.Type != feed.SpaceCreated || created.SpaceID == "" || created.SpaceName == "" {
		t.Errorf("SpaceCreated = %+v", created)
	}

	renamed, err := one(mapper.Map(t.Context(), load(t, "SpaceRenamed")))
	if err != nil {
		t.Fatalf("Map SpaceRenamed: %v", err)
	}
	if renamed.Type != feed.SpaceRenamed || renamed.SpaceID != "5b356174-308d-4424-b6f9-60cd9cf282ec" || renamed.SpaceName != "Fixtures renamed" {
		t.Errorf("SpaceRenamed = %+v", renamed)
	}

	deleted, err := one(mapper.Map(t.Context(), load(t, "SpaceDeleted")))
	if err != nil {
		t.Fatalf("Map SpaceDeleted: %v", err)
	}
	if deleted.Type != feed.SpaceDeleted || deleted.SpaceID != "5b356174-308d-4424-b6f9-60cd9cf282ec" {
		t.Errorf("SpaceDeleted = %+v", deleted)
	}
	if deleted.TS.IsZero() {
		t.Error("SpaceDeleted has no timestamp, the plain time was not taken")
	}
}

func TestMapSkips(t *testing.T) {
	mapper := newMapper(&fakeResolver{})

	for name, reason := range map[string]string{
		"FileUploaded":   ReasonIgnored,
		"FileDownloaded": ReasonIgnored,
		"SendSSE":        ReasonUnknown,
	} {
		_, err := one(mapper.Map(t.Context(), load(t, name)))
		var skip *SkipError
		if !errors.As(err, &skip) || skip.Reason != reason {
			t.Errorf("Map(%s): %v, want a skip with reason %s", name, err, reason)
		}
	}

	// The file of the event is gone by the time it is looked up.
	_, err := one(mapper.Map(t.Context(), load(t, "UploadReady")))
	var skip *SkipError
	if !errors.As(err, &skip) || skip.Reason != ReasonGone {
		t.Errorf("Map of a vanished file: %v, want a skip with reason gone", err)
	}

	// The gateway is unreachable: this one has to be retried, not skipped.
	_, err = newMapper(&fakeResolver{err: errors.New("connection refused")}).Map(t.Context(), load(t, "UploadReady"))
	if err == nil || errors.As(err, &skip) {
		t.Errorf("Map with an unreachable gateway: %v, want a plain error", err)
	}
}

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"./probe.bin":         "/probe.bin",
		"/renamed.mp4":        "/renamed.mp4",
		"probe.bin":           "/probe.bin",
		"./folder/a/../b.mp4": "/folder/b.mp4",
		".":                   "/",
		"":                    "/",
	} {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpaceIDOf(t *testing.T) {
	for in, want := range map[string]string{
		"storage$space!space": "space",
		"storage$space":       "space",
		"space":               "space",
	} {
		if got := spaceIDOf(&ocevents.StorageSpaceID{OpaqueID: in}); got != want {
			t.Errorf("spaceIDOf(%q) = %q, want %q", in, got, want)
		}
	}
	if got := spaceIDOf(nil); got != "" {
		t.Errorf("spaceIDOf(nil) = %q", got)
	}
}
