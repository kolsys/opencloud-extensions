package resync

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"slices"
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

const (
	sp = "space-1"
	// blobRoot is the blob of root.mp4, which the platform never changes.
	blobRoot = "blob-root"
)

var root = cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: sp}

// fakePlatform answers listings from tables. The failing container answers
// failWith instead, failures times, or for good when failures is negative.
type fakePlatform struct {
	spaces     []cs3.Space
	containers map[string][]cs3.ResourceInfo
	recycle    []cs3.RecycleItem
	failing    string
	failures   int
	failWith   error
}

func (f *fakePlatform) ListSpaces(context.Context) ([]cs3.Space, error) { return f.spaces, nil }

func (f *fakePlatform) ListContainer(_ context.Context, ref cs3.Ref) ([]cs3.ResourceInfo, error) {
	if ref.OpaqueID == f.failing && f.failures != 0 {
		if f.failures > 0 {
			f.failures--
		}
		return nil, f.failWith
	}
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

// rootEntry is root.mp4 the way the platform has it, with its blob.
func rootEntry(p string) tree.Entry {
	return tree.Entry{SpaceID: sp, FileID: "f-root", Path: p, BlobID: blobRoot, Size: 5, Mime: "video/mp4", ETag: "e-root", MTime: time.Unix(1700000000, 0).UTC(), SHA1: sha1Of(rootData), MD5: md5Of(rootData)}
}

func seeded(t *testing.T) *tree.Tree {
	t.Helper()
	tr := tree.New(treetest.New(""))
	for _, e := range []tree.Entry{
		// Unchanged: kept with its blob.
		rootEntry("/root.mp4"),
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

func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// run resyncs and collects the files reported without a blob, sorted.
func run(t *testing.T, p *fakePlatform, tr *tree.Tree, blobs BlobIDs, opts Options) (*Report, []string, error) {
	t.Helper()
	var noBlob []string
	opts.NoBlob = func(file string) { noBlob = append(noBlob, file) }
	report, err := Run(t.Context(), p, tr, blobs, opts, discard())
	slices.Sort(noBlob)
	return report, noBlob, err
}

// quickRetries shortens the backoff for the test.
func quickRetries(t *testing.T) {
	t.Helper()
	saved := backoff
	backoff = schedule{attempts: 3, initial: time.Millisecond, max: time.Millisecond}
	t.Cleanup(func() { backoff = saved })
}

func TestRunBringsTheTreeInLine(t *testing.T) {
	tr := seeded(t)
	report, noBlob, err := run(t, platform(), tr, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Spaces != 1 || report.Files != 3 || report.Kept != 1 || report.Rewritten != 2 || report.Stale != 2 || len(report.Failed) != 0 {
		t.Errorf("report = %+v", report)
	}
	if report.NoBlob != 2 || len(noBlob) != 2 || noBlob[0] != sp+"/movies/a.mp4" || noBlob[1] != sp+"/new.mp4" {
		t.Errorf("no blob = %d, %v", report.NoBlob, noBlob)
	}

	ctx := t.Context()
	if e, err := tr.File(ctx, sp, "/root.mp4"); err != nil || e.BlobID != blobRoot {
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
	if _, _, err := run(t, platform(), tr, nil, Options{}); err != nil {
		t.Fatal(err)
	}

	blobs := fakeBlobIDs{"f-a": "blob-a2", "f-root": "blob-root-again"}
	report, noBlob, err := run(t, platform(), tr, blobs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// a.mp4 from the metadata, new.mp4 has no node there, root.mp4 keeps
	// the blob the tree knew: the version did not change.
	if report.FromMetadata != 1 || report.Rewritten != 1 || report.Kept != 2 || report.NoBlob != 1 || len(noBlob) != 1 || noBlob[0] != sp+"/new.mp4" {
		t.Errorf("report = %+v, no blob %v", report, noBlob)
	}

	ctx := t.Context()
	if e, err := tr.File(ctx, sp, "/movies/a.mp4"); err != nil || e.BlobID != "blob-a2" {
		t.Errorf("a.mp4 = %+v, %v", e, err)
	}
	if e, err := tr.File(ctx, sp, "/root.mp4"); err != nil || e.BlobID != blobRoot {
		t.Errorf("root.mp4 = %+v, %v", e, err)
	}
	if e, err := tr.File(ctx, sp, "/new.mp4"); err != nil || e.BlobID != "" {
		t.Errorf("new.mp4 = %+v, %v", e, err)
	}
}

// A second run finds the tree in line: every file is kept, and besides the
// record of the space nothing is written.
func TestRerunKeepsEverything(t *testing.T) {
	objects := treetest.New("")
	tr := tree.New(objects)
	if _, _, err := run(t, platform(), tr, nil, Options{}); err != nil {
		t.Fatal(err)
	}
	puts, deletes := objects.Puts, objects.Deletes

	report, _, err := run(t, platform(), tr, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 3 || report.Kept != 3 || report.Rewritten != 0 || report.Stale != 0 {
		t.Errorf("report = %+v", report)
	}
	if objects.Puts != puts+1 || objects.Deletes != deletes {
		t.Errorf("a rerun wrote to the tree: puts %d, deletes %d", objects.Puts-puts, objects.Deletes-deletes)
	}
}

// A file moved while the service was away keeps its blob at the new path,
// since its version did not change, and loses its record at the old one.
func TestRunCarriesTheBlobOfAMovedFile(t *testing.T) {
	tr := seeded(t)
	p := platform()
	p.containers[sp] = []cs3.ResourceInfo{folder("d1", "movies"), file("f-new", "new.mp4", "e-new", newData)}
	p.containers["d1"] = []cs3.ResourceInfo{file("f-a", "a.mp4", "e-a2", aData), folder("d2", "empty"), file("f-root", "root.mp4", "e-root", rootData)}

	report, _, err := run(t, p, tr, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 3 || report.Kept != 0 || report.Rewritten != 3 || report.Stale != 2 {
		t.Errorf("report = %+v", report)
	}
	ctx := t.Context()
	if e, err := tr.File(ctx, sp, "/movies/root.mp4"); err != nil || e.BlobID != blobRoot {
		t.Errorf("moved file = %+v, %v", e, err)
	}
	if _, err := tr.File(ctx, sp, "/root.mp4"); err == nil {
		t.Error("the old path of the moved file survived")
	}
}

// An interrupted move leaves a file at two paths; the one the platform does
// not hold is taken out without counting as stale.
func TestRunCleansTheLeftoverOfAnInterruptedMove(t *testing.T) {
	tr := seeded(t)
	if err := tr.PutFile(t.Context(), rootEntry("/old/root.mp4")); err != nil {
		t.Fatal(err)
	}

	report, _, err := run(t, platform(), tr, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 3 || report.Kept != 1 || report.Rewritten != 2 || report.Stale != 2 {
		t.Errorf("report = %+v", report)
	}
	ctx := t.Context()
	if _, err := tr.File(ctx, sp, "/old/root.mp4"); err == nil {
		t.Error("the leftover survived")
	}
	if e, err := tr.File(ctx, sp, "/root.mp4"); err != nil || e.BlobID != blobRoot {
		t.Errorf("kept file = %+v, %v", e, err)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	objects := treetest.New("")
	tr := tree.New(objects)
	_ = tr.PutFile(t.Context(), tree.Entry{SpaceID: sp, FileID: "f-never", Path: "/never.mp4", BlobID: "b"})
	puts, deletes := objects.Puts, objects.Deletes

	report, _, err := run(t, platform(), tr, nil, Options{DryRun: true})
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
	report, _, err := run(t, platform(), tree.New(treetest.New("")), nil, Options{Spaces: []string{"other"}})
	if err != nil || report.Spaces != 0 {
		t.Errorf("report = %+v, %v", report, err)
	}
	report, _, err = run(t, platform(), tree.New(treetest.New("")), nil, Options{Spaces: []string{sp}})
	if err != nil || report.Spaces != 1 || report.Files != 3 {
		t.Errorf("report = %+v, %v", report, err)
	}
}

// A space that fails is reported and the run goes on with the next one.
func TestRunGoesOnAfterAFailedSpace(t *testing.T) {
	p := platform()
	broken := cs3.Space{ID: "st$space-2", Root: cs3.Ref{StorageID: "st", SpaceID: "space-2", OpaqueID: "space-2"}, Name: "Broken", Type: "project"}
	p.spaces = append([]cs3.Space{broken}, p.spaces...)
	p.failing, p.failures, p.failWith = "space-2", -1, errors.New("boom")

	report, _, err := run(t, p, tree.New(treetest.New("")), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Spaces != 2 || report.Files != 3 || len(report.Failed) != 1 || report.Failed[0] != "space-2 (Broken)" {
		t.Errorf("report = %+v", report)
	}
}

// A listing that fails for a while is tried again; one that keeps failing
// fails the space after the attempts.
func TestRunRetriesATransientError(t *testing.T) {
	quickRetries(t)
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	p := platform()
	p.failing, p.failures, p.failWith = "d1", 2, refused

	report, _, err := run(t, p, tree.New(treetest.New("")), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 3 || len(report.Failed) != 0 {
		t.Errorf("report = %+v", report)
	}

	p.failures = -1
	report, _, err = run(t, p, tree.New(treetest.New("")), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed) != 1 {
		t.Errorf("report = %+v", report)
	}
}
