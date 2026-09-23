package resync

import (
	"context"
	"log/slog"
	"sort"
	"testing"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

const sp = "space-1"

var root = cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: sp}

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

// fakeThumbs is the bucket of masters in a map keyed by space/file, with
// the etag of each master.
type fakeThumbs struct {
	masters map[string]string
	deleted []string
}

func (f *fakeThumbs) WalkMasters(_ context.Context, fn func(spaceID, fileID string) error) error {
	keys := make([]string, 0, len(f.masters))
	for key := range f.masters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		space, file := split(key)
		if err := fn(space, file); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeThumbs) HeadMaster(_ context.Context, spaceID, fileID string) (*store.Master, error) {
	etag, ok := f.masters[spaceID+"/"+fileID]
	if !ok {
		return nil, s3store.ErrNotFound
	}
	return &store.Master{ETag: etag}, nil
}

func (f *fakeThumbs) DeleteFile(_ context.Context, spaceID, fileID string) (int, error) {
	delete(f.masters, spaceID+"/"+fileID)
	f.deleted = append(f.deleted, spaceID+"/"+fileID)
	return 1, nil
}

func split(key string) (string, string) {
	for i := range key {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

type fakeQueue struct{ jobs []queue.Job }

func (q *fakeQueue) Enqueue(_ context.Context, _ string, job queue.Job) (bool, error) {
	q.jobs = append(q.jobs, job)
	return true, nil
}

func file(id, name, mime, etag string) cs3.ResourceInfo {
	return cs3.ResourceInfo{ID: cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: id}, Name: name, MimeType: mime, ETag: `"` + etag + `"`, Size: 9}
}

func folder(id, name string) cs3.ResourceInfo {
	return cs3.ResourceInfo{ID: cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: id}, Name: name, IsDir: true}
}

func platform() *fakePlatform {
	return &fakePlatform{
		spaces: []cs3.Space{
			{ID: "st$" + sp, Root: root, Name: "Creatives", Type: "project"},
			{ID: "st$shares", Root: cs3.Ref{SpaceID: "shares"}, Name: "Shares", Type: "virtual"},
		},
		containers: map[string][]cs3.ResourceInfo{
			sp:   {folder("d1", "movies"), file("v-current", "current.mp4", "video/mp4", "e1"), file("v-missing", "missing.mp4", "video/mp4", "e2"), file("pic", "pic.jpg", "image/jpeg", "e3")},
			"d1": {file("v-stale", "stale.mp4", "video/mp4", "e-new"), file("v-ext", "raw.mkv", "application/octet-stream", "e5")},
		},
		recycle: []cs3.RecycleItem{{Key: "v-trashed", Path: "/old.mp4"}},
	}
}

func thumbs() *fakeThumbs {
	return &fakeThumbs{masters: map[string]string{
		sp + "/v-current": "e1",
		sp + "/v-stale":   "e-old",
		sp + "/v-trashed": "e4",
		sp + "/v-gone":    "e6",
		"space-gone/v-x":  "e7",
	}}
}

func run(t *testing.T, th *fakeThumbs, q *fakeQueue, opts Options) *Report {
	t.Helper()
	report, err := Run(t.Context(), platform(), th, q, video.NewMatcher([]string{"mp4", "mkv"}), opts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestRunRaisesJobsAndFindsOrphans(t *testing.T) {
	th, q := thumbs(), &fakeQueue{}
	report := run(t, th, q, Options{})

	if report.Spaces != 1 || report.Videos != 4 || report.Masters != 5 || report.Current != 1 || report.Missing != 2 || report.Stale != 1 {
		t.Errorf("report = %+v", report)
	}
	// The video without a master and the one the mime type does not name
	// are missing; the one with the master of an older version is stale.
	jobs := map[string]bool{}
	for _, job := range q.jobs {
		jobs[job.FileID] = true
	}
	if len(jobs) != 3 || !jobs["v-missing"] || !jobs["v-ext"] || !jobs["v-stale"] {
		t.Errorf("jobs = %v", jobs)
	}
	for _, job := range q.jobs {
		if job.FileID == "v-stale" && (job.ETag != "e-new" || job.Name != "stale.mp4" || job.SpaceID != sp || job.StorageID != "st") {
			t.Errorf("stale job = %+v", job)
		}
	}
	// The master of the trashed file is kept; the gone file and the gone
	// space are orphans, listed and not removed.
	if report.Orphans != 2 || report.Deleted != 0 || len(th.deleted) != 0 {
		t.Errorf("orphans = %+v, deleted %v", report, th.deleted)
	}
	if len(report.OrphanKeys) != 2 || report.OrphanKeys[0] != "space-1/v-gone" || report.OrphanKeys[1] != "space-gone/v-x" {
		t.Errorf("orphan keys = %v", report.OrphanKeys)
	}
}

func TestDeleteOrphansAndDryRun(t *testing.T) {
	th, q := thumbs(), &fakeQueue{}
	report := run(t, th, q, Options{DryRun: true, DeleteOrphans: true})
	if len(q.jobs) != 0 || report.Deleted != 0 || len(th.deleted) != 0 || report.Missing != 2 || report.Orphans != 2 {
		t.Errorf("dry run did something: %+v, jobs %d, deleted %v", report, len(q.jobs), th.deleted)
	}

	report = run(t, th, q, Options{DeleteOrphans: true})
	if report.Deleted != 2 || len(th.deleted) != 2 {
		t.Errorf("delete: %+v, deleted %v", report, th.deleted)
	}
	if _, ok := th.masters[sp+"/v-trashed"]; !ok {
		t.Error("the master of a trashed file was removed")
	}
	if _, ok := th.masters[sp+"/v-current"]; !ok {
		t.Error("a current master was removed")
	}
}

func TestSpaceFilterLeavesOtherSpacesAlone(t *testing.T) {
	th, q := thumbs(), &fakeQueue{}
	report := run(t, th, q, Options{Spaces: []string{sp}, DeleteOrphans: true})
	if report.Spaces != 1 || report.Deleted != 1 || th.deleted[0] != "space-1/v-gone" {
		t.Errorf("filtered: %+v, deleted %v", report, th.deleted)
	}
	if _, ok := th.masters["space-gone/v-x"]; !ok {
		t.Error("a master of a space outside the filter was removed")
	}

	report = run(t, thumbs(), &fakeQueue{}, Options{Spaces: []string{"other"}})
	if report.Spaces != 0 || report.Videos != 0 {
		t.Errorf("no space: %+v", report)
	}
}
