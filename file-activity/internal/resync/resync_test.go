package resync

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"log/slog"
	"testing"
	"time"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree/treetest"
)

// fakeBlobIDs answers the blob of a node from a table, like the metadata
// of the platform would.
type fakeBlobIDs map[string]string

func (f fakeBlobIDs) BlobID(_ context.Context, _, nodeID string) (string, error) {
	blobID, ok := f[nodeID]
	if !ok {
		return "", ErrNoMetadata
	}
	return blobID, nil
}

func sha1Of(data string) string {
	sum := sha1.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}

func md5Of(data string) string {
	sum := md5.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}

const sp = "space-1"

var root = cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: sp}

// fakePlatform answers listings from tables.
type fakePlatform struct {
	spaces     []cs3.Space
	containers map[string][]cs3.ResourceInfo
	recycle    []cs3.RecycleItem
}

func (f *fakePlatform) ListSpaces(context.Context) ([]cs3.Space, error) { return f.spaces, nil }

func (f *fakePlatform) ListContainer(_ context.Context, ref cs3.Ref) ([]cs3.ResourceInfo, error) {
	infos, ok := f.containers[ref.OpaqueID]
	if !ok {
		return nil, cs3.ErrNotFound
	}
	return infos, nil
}

func (f *fakePlatform) ListRecycle(context.Context, cs3.Ref) ([]cs3.RecycleItem, error) {
	return f.recycle, nil
}

// Contents of the files of the platform, five bytes each.
const (
	rootData = "rrrrr"
	aData    = "aaaa2"
	newData  = "nnnnn"
)

func file(id, name, etag, data string) cs3.ResourceInfo {
	return cs3.ResourceInfo{ID: cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: id}, Name: name, Size: uint64(len(data)), MimeType: "video/mp4", ETag: `"` + etag + `"`, MTime: time.Unix(1700000000, 0).UTC(), SHA1: sha1Of(data), MD5: md5Of(data)}
}

func folder(id, name string) cs3.ResourceInfo {
	return cs3.ResourceInfo{ID: cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: id}, Name: name, IsDir: true}
}

func platform() *fakePlatform {
	return &fakePlatform{
		spaces: []cs3.Space{
			{ID: "st$" + sp, Root: root, Name: "Alan", Type: "personal", Owner: "alan"},
			{ID: "st$shares", Root: cs3.Ref{SpaceID: "shares"}, Name: "Shares", Type: "virtual"},
		},
		containers: map[string][]cs3.ResourceInfo{
			sp:     {folder("d1", "movies"), file("f-root", "root.mp4", "e-root", rootData), file("f-new", "new.mp4", "e-new", newData)},
			"d1":   {file("f-a", "a.mp4", "e-a2", aData), folder("d2", "empty")},
			"d2":   {},
			"gone": nil,
		},
		recycle: []cs3.RecycleItem{{Key: "f-old", Path: "/old.mp4"}},
	}
}

func seeded(t *testing.T) *tree.Tree {
	t.Helper()
	tr := tree.New(treetest.New(""))
	for _, e := range []tree.Entry{
		// Unchanged: kept with its blob.
		{SpaceID: sp, FileID: "f-root", Path: "/root.mp4", BlobID: "blob-root", Size: 5, Mime: "video/mp4", ETag: "e-root", MTime: time.Unix(1700000000, 0).UTC(), SHA1: sha1Of(rootData), MD5: md5Of(rootData)},
		// A newer version went by: the blob is not known any more.
		{SpaceID: sp, FileID: "f-a", Path: "/movies/a.mp4", BlobID: "blob-a1", Size: 4, ETag: "e-a1"},
		// In the trash bin of the platform now: the record follows.
		{SpaceID: sp, FileID: "f-old", Path: "/old.mp4", BlobID: "blob-old", Size: 5, ETag: "e-old"},
		// Gone for good.
		{SpaceID: sp, FileID: "f-never", Path: "/never.mp4", BlobID: "blob-never", Size: 5, ETag: "e-never"},
	} {
		if err := tr.PutFile(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

func TestRunBringsTheTreeInLine(t *testing.T) {
	tr := seeded(t)
	report, err := Run(t.Context(), platform(), tr, nil, Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if report.Spaces != 1 || report.Files != 3 || report.Kept != 1 || report.Rewritten != 2 || report.Stale != 2 {
		t.Errorf("report = %+v", report)
	}
	if len(report.NoBlob) != 2 || report.NoBlob[0] != sp+"/movies/a.mp4" || report.NoBlob[1] != sp+"/new.mp4" {
		t.Errorf("no blob = %v", report.NoBlob)
	}

	ctx := t.Context()
	if e, err := tr.File(ctx, sp, "/root.mp4"); err != nil || e.BlobID != "blob-root" {
		t.Errorf("kept file = %+v, %v", e, err)
	}
	if e, err := tr.File(ctx, sp, "/movies/a.mp4"); err != nil || e.BlobID != "" || e.ETag != "e-a2" || e.Size != 5 {
		t.Errorf("rewritten file = %+v, %v", e, err)
	}
	if e, err := tr.File(ctx, sp, "/new.mp4"); err != nil || e.BlobID != "" || e.FileID != "f-new" {
		t.Errorf("new file = %+v, %v", e, err)
	}
	if _, err := tr.File(ctx, sp, "/never.mp4"); err == nil {
		t.Error("the file that is gone survived")
	}
	if _, err := tr.File(ctx, sp, "/old.mp4"); err == nil {
		t.Error("the trashed file is still in the tree")
	}
	if e, err := tr.Trashed(ctx, sp, "f-old"); err != nil || e.BlobID != "blob-old" {
		t.Errorf("trashed record = %+v, %v", e, err)
	}
	if s, err := tr.Space(ctx, sp); err != nil || s.Name != "Alan" || s.Type != "personal" || s.Owner != "alan" {
		t.Errorf("space = %+v, %v", s, err)
	}
}

// With the metadata of the platform at hand, the blobs of the unknown
// files are read from it and written even though nothing else about the
// files changed; a file the metadata has no node for stays without one.
func TestRunReadsTheBlobsFromTheMetadata(t *testing.T) {
	tr := seeded(t)
	if _, err := Run(t.Context(), platform(), tr, nil, Options{}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	blobs := fakeBlobIDs{"f-a": "blob-a2", "f-root": "blob-root-again"}
	report, err := Run(t.Context(), platform(), tr, blobs, Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	// a.mp4 from the metadata, new.mp4 has no node there, root.mp4 keeps
	// the blob the tree knew: the version did not change.
	if report.FromMetadata != 1 || report.Rewritten != 1 || report.Kept != 2 || len(report.NoBlob) != 1 || report.NoBlob[0] != sp+"/new.mp4" {
		t.Errorf("report = %+v", report)
	}

	ctx := t.Context()
	if e, err := tr.File(ctx, sp, "/movies/a.mp4"); err != nil || e.BlobID != "blob-a2" {
		t.Errorf("a.mp4 = %+v, %v", e, err)
	}
	if e, err := tr.File(ctx, sp, "/root.mp4"); err != nil || e.BlobID != "blob-root" {
		t.Errorf("root.mp4 = %+v, %v", e, err)
	}
	if e, err := tr.File(ctx, sp, "/new.mp4"); err != nil || e.BlobID != "" {
		t.Errorf("new.mp4 = %+v, %v", e, err)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	objects := treetest.New("")
	tr := tree.New(objects)
	_ = tr.PutFile(t.Context(), tree.Entry{SpaceID: sp, FileID: "f-never", Path: "/never.mp4", BlobID: "b"})
	puts, deletes := objects.Puts, objects.Deletes

	report, err := Run(t.Context(), platform(), tr, nil, Options{DryRun: true}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if report.Rewritten != 3 || report.Stale != 1 {
		t.Errorf("report = %+v", report)
	}
	if objects.Puts != puts || objects.Deletes != deletes {
		t.Error("a dry run wrote to the tree")
	}
}

func TestSpaceFilter(t *testing.T) {
	report, err := Run(t.Context(), platform(), tree.New(treetest.New("")), nil, Options{Spaces: []string{"other"}}, slog.New(slog.DiscardHandler))
	if err != nil || report.Spaces != 0 {
		t.Errorf("report = %+v, %v", report, err)
	}
	report, err = Run(t.Context(), platform(), tree.New(treetest.New("")), nil, Options{Spaces: []string{sp}}, slog.New(slog.DiscardHandler))
	if err != nil || report.Spaces != 1 || report.Files != 3 {
		t.Errorf("report = %+v, %v", report, err)
	}
}
