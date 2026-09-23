package events

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/ocevents"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/metrics"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

const fixtures = "../../../common/ocevents/testdata"

type fakeResolver struct {
	info *cs3.ResourceInfo
	err  error
}

func (f *fakeResolver) Stat(context.Context, cs3.Ref) (*cs3.ResourceInfo, error) {
	return f.info, f.err
}

type fakeQueue struct {
	jobs []queue.Job
}

func (f *fakeQueue) Enqueue(_ context.Context, subject string, job queue.Job) (bool, error) {
	if subject != queue.SubjectJobs {
		panic("events go to the jobs subject")
	}
	f.jobs = append(f.jobs, job)
	return true, nil
}

type fakeCleaner struct {
	files  []string
	spaces []string
}

func (f *fakeCleaner) DeleteFile(_ context.Context, spaceID, fileID string) (int, error) {
	f.files = append(f.files, spaceID+"/"+fileID)
	return 1, nil
}

func (f *fakeCleaner) DeleteSpace(_ context.Context, spaceID string) (int, error) {
	f.spaces = append(f.spaces, spaceID)
	return 1, nil
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

func reactor(resolver Resolver, q Enqueuer, cleaner Cleaner) *Reactor {
	return New(nil, resolver, q, cleaner, video.NewMatcher([]string{"mp4", "mov", ".mkv"}), metrics.New(prometheus.NewRegistry()), "test", slog.New(slog.DiscardHandler))
}

func infoOf(mime, name string) *cs3.ResourceInfo {
	return &cs3.ResourceInfo{
		ID:       cs3.Ref{StorageID: "storage", SpaceID: "space-a", OpaqueID: "file-1"},
		Name:     name,
		MimeType: mime,
		Size:     734003200,
		ETag:     `"etag-1"`,
	}
}

func TestUploadReadyOfAVideoBecomesAJob(t *testing.T) {
	q := &fakeQueue{}
	if err := reactor(&fakeResolver{info: infoOf("video/mp4", "clip.mp4")}, q, &fakeCleaner{}).react(t.Context(), load(t, "UploadReady"), slog.Default()); err != nil {
		t.Fatalf("react: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Fatalf("%d jobs, want 1", len(q.jobs))
	}
	job := q.jobs[0]
	if job.FileID != "file-1" || job.SpaceID != "space-a" || job.ETag != "etag-1" || job.ID() != "file-1:etag-1" {
		t.Errorf("job = %+v", job)
	}
	if job.Size == 0 || job.Name != "clip.mp4" || job.Mime != "video/mp4" {
		t.Errorf("job attributes = %+v", job)
	}
}

func TestUploadReadyOfAnythingElseIsIgnored(t *testing.T) {
	for _, info := range []*cs3.ResourceInfo{
		infoOf("image/png", "pic.png"),
		infoOf("application/octet-stream", "probe.bin"),
		{ID: cs3.Ref{SpaceID: "s", OpaqueID: "d"}, Name: "folder", IsDir: true, MimeType: "httpd/unix-directory"},
	} {
		q := &fakeQueue{}
		if err := reactor(&fakeResolver{info: info}, q, &fakeCleaner{}).react(t.Context(), load(t, "UploadReady"), slog.Default()); err != nil {
			t.Fatalf("react(%s): %v", info.Name, err)
		}
		if len(q.jobs) != 0 {
			t.Errorf("%s raised a job", info.Name)
		}
	}
}

// The platform reports octet-stream for containers it does not know; the
// extension decides then.
func TestExtensionDecidesWhenTheMimeTypeDoesNot(t *testing.T) {
	q := &fakeQueue{}
	if err := reactor(&fakeResolver{info: infoOf("application/octet-stream", "raw.MKV")}, q, &fakeCleaner{}).react(t.Context(), load(t, "UploadReady"), slog.Default()); err != nil {
		t.Fatalf("react: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Errorf("%d jobs, want the extension to win", len(q.jobs))
	}
}

func TestAFileGoneBeforeTheLookupIsNoError(t *testing.T) {
	q := &fakeQueue{}
	if err := reactor(&fakeResolver{err: cs3.ErrNotFound}, q, &fakeCleaner{}).react(t.Context(), load(t, "UploadReady"), slog.Default()); err != nil {
		t.Errorf("react: %v, want nil for a vanished file", err)
	}
	if len(q.jobs) != 0 {
		t.Error("a vanished file raised a job")
	}
}

func TestFailedUploadIsIgnored(t *testing.T) {
	event := load(t, "UploadReady")
	event.Payload.(*ocevents.UploadReady).Failed = true
	resolver := &fakeResolver{err: cs3.ErrPermissionDenied}
	if err := reactor(resolver, &fakeQueue{}, &fakeCleaner{}).react(t.Context(), event, slog.Default()); err != nil {
		t.Errorf("react: %v, a failed upload must not even be looked up", err)
	}
}

func TestFileVersionRestoredBecomesAJob(t *testing.T) {
	q := &fakeQueue{}
	if err := reactor(&fakeResolver{info: infoOf("video/mp4", "back.mp4")}, q, &fakeCleaner{}).react(t.Context(), load(t, "FileVersionRestored"), slog.Default()); err != nil {
		t.Fatalf("react: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Errorf("%d jobs, want 1", len(q.jobs))
	}
}

// A restore from the trash bin can hand the file a new id: the restored
// file is looked up and gets a job of its own.
func TestItemRestoredBecomesAJob(t *testing.T) {
	q := &fakeQueue{}
	if err := reactor(&fakeResolver{info: infoOf("video/quicktime", "clip.mov")}, q, &fakeCleaner{}).react(t.Context(), load(t, "ItemRestored"), slog.Default()); err != nil {
		t.Fatalf("react: %v", err)
	}
	if len(q.jobs) != 1 {
		t.Errorf("%d jobs, want 1", len(q.jobs))
	}
}

// The ids of a purged item are in Ref; ID is empty on the wire.
func TestItemPurgedRemovesTheThumbnailsOfTheFile(t *testing.T) {
	cleaner := &fakeCleaner{}
	if err := reactor(&fakeResolver{}, &fakeQueue{}, cleaner).react(t.Context(), load(t, "ItemPurged"), slog.Default()); err != nil {
		t.Fatalf("react: %v", err)
	}
	if len(cleaner.files) != 1 || cleaner.files[0] != "b1f74ec4-dd7e-11ef-a543-03775734d0f7/f9273736-7d7f-4491-ad89-6b24eaf314b3" {
		t.Errorf("deleted = %v", cleaner.files)
	}
}

func TestSpaceDeletedRemovesTheSpace(t *testing.T) {
	cleaner := &fakeCleaner{}
	if err := reactor(&fakeResolver{}, &fakeQueue{}, cleaner).react(t.Context(), load(t, "SpaceDeleted"), slog.Default()); err != nil {
		t.Fatalf("react: %v", err)
	}
	if len(cleaner.spaces) != 1 || cleaner.spaces[0] != "5b356174-308d-4424-b6f9-60cd9cf282ec" {
		t.Errorf("deleted spaces = %v", cleaner.spaces)
	}
}

func TestOtherEventsDoNothing(t *testing.T) {
	for _, name := range []string{"ItemTrashed", "ItemMoved", "ContainerCreated", "FileDownloaded", "SendSSE"} {
		q, cleaner := &fakeQueue{}, &fakeCleaner{}
		resolver := &fakeResolver{err: cs3.ErrPermissionDenied}
		if err := reactor(resolver, q, cleaner).react(t.Context(), load(t, name), slog.Default()); err != nil {
			t.Errorf("react(%s): %v", name, err)
		}
		if len(q.jobs)+len(cleaner.files)+len(cleaner.spaces) != 0 {
			t.Errorf("%s had an effect", name)
		}
	}
}
