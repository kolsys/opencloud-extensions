package preview

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kolsys/opencloud-extensions/common/httpx"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/cache"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/metrics"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/render"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

const (
	clipPath = "/remote.php/dav/spaces/s1$sp1/movies/clip.mp4"
	clipETag = "e1"
	// proxied is what the stand-in for the webdav service answers.
	proxied = "proxied"
)

var clip = &httpx.Resource{
	FileID: "s1$sp1!f1", StorageID: "s1", SpaceID: "sp1", OpaqueID: "f1",
	ETag: clipETag, ContentType: "video/mp4", Size: 1234,
}

type fakeAuth struct {
	resources map[string]*httpx.Resource
	errs      map[string]error
	headers   http.Header
	// heads counts the lookups made by HEAD, signed counts the ones that
	// carried a signature.
	heads  int
	signed int
}

func (a *fakeAuth) PropFind(_ context.Context, davPath string, header http.Header) (*httpx.Resource, error) {
	a.headers = header
	return a.lookup(davPath)
}

func (a *fakeAuth) Head(_ context.Context, davPath string, query url.Values, header http.Header) (*httpx.Resource, error) {
	a.headers = header
	a.heads++
	if query.Get(httpx.ParamSignature) != "" {
		a.signed++
	}
	return a.lookup(davPath)
}

func (a *fakeAuth) lookup(davPath string) (*httpx.Resource, error) {
	if err, ok := a.errs[davPath]; ok {
		return nil, err
	}
	if res, ok := a.resources[davPath]; ok {
		return res, nil
	}
	return nil, httpx.StatusError{Code: http.StatusNotFound}
}

type fakeMasters struct {
	master   *store.Master
	frame    []byte
	failure  *store.Failure
	err      error
	heads    atomic.Int32
	gets     atomic.Int32
	failures atomic.Int32
}

func (m *fakeMasters) HeadMaster(context.Context, string, string) (*store.Master, error) {
	m.heads.Add(1)
	if m.err != nil {
		return nil, m.err
	}
	if m.master == nil {
		return nil, s3store.ErrNotFound
	}
	return m.master, nil
}

func (m *fakeMasters) GetMaster(context.Context, string, string) (io.ReadCloser, *store.Master, error) {
	m.gets.Add(1)
	if m.master == nil {
		return nil, nil, s3store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(m.frame)), m.master, nil
}

func (m *fakeMasters) Failure(context.Context, string, string) (*store.Failure, error) {
	m.failures.Add(1)
	if m.failure == nil {
		return nil, s3store.ErrNotFound
	}
	return m.failure, nil
}

type fakeQueue struct {
	jobs     []queue.Job
	subjects []string
	err      error
}

func (q *fakeQueue) Enqueue(_ context.Context, subject string, job queue.Job) (bool, error) {
	if q.err != nil {
		return false, q.err
	}
	q.jobs = append(q.jobs, job)
	q.subjects = append(q.subjects, subject)
	return true, nil
}

type harness struct {
	handler  *Handler
	auth     *fakeAuth
	masters  *fakeMasters
	queue    *fakeQueue
	metrics  *metrics.Metrics
	upstream *atomic.Pointer[http.Request]
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, 2, nil)
}

// newHarnessWith builds a handler with that many slots of the gate and an
// upstream of its own; nil answers proxied to everything.
func newHarnessWith(t *testing.T, generations int, respond http.HandlerFunc) *harness {
	t.Helper()

	if respond == nil {
		respond = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte(proxied))
		}
	}
	var seen atomic.Pointer[http.Request]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Clone(context.Background()))
		respond(w, r)
	}))
	t.Cleanup(upstream.Close)

	log := slog.New(slog.DiscardHandler)
	proxy, err := httpx.NewProxy(upstream.URL, log)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := cache.Open(t.TempDir(), 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	grid, err := render.ParseGrid([]string{"16x16", "32x32", "64x64", "128x128", "500x280", "280x500"})
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{
		auth:     &fakeAuth{resources: map[string]*httpx.Resource{clipPath: clip}, errs: map[string]error{}},
		masters:  &fakeMasters{},
		queue:    &fakeQueue{},
		metrics:  metrics.New(prometheus.NewRegistry()),
		upstream: &seen,
	}
	h.handler = New(proxy, h.auth, h.masters, h.queue, disk, grid, video.NewMatcher([]string{"mp4"}), generations, h.metrics, log)
	return h
}

func (h *harness) ready() {
	h.masters.master = &store.Master{ETag: clipETag, Mime: "video/mp4", ObjectETag: "object-1"}
	h.masters.frame = frame(1280, 720)
}

func (h *harness) get(t *testing.T, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodGet, target, header...)
}

func (h *harness) do(t *testing.T, method, target string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Authorization", "Bearer token")
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func TestPassesOnWhatIsNotAVideoPreview(t *testing.T) {
	h := newHarness(t)

	for _, target := range []string{
		"/remote.php/dav/spaces/s1$sp1/photo.jpg?preview=1&x=64&y=64",
		"/dav/spaces/s1$sp1?preview=1",
		clipPath + "?x=64",
		"/remote.php/dav/spaces/s1$sp1/movies/clip.mp4.png?preview=1",
	} {
		w := h.get(t, target)
		if w.Code != http.StatusOK || w.Body.String() != proxied {
			t.Errorf("%s: %d %q, want the answer of the platform", target, w.Code, w.Body.String())
		}
		seen := h.upstream.Load()
		if seen == nil || seen.URL.RequestURI() != target {
			t.Errorf("%s: the platform saw %v", target, seen)
		}
	}
	if h.auth.headers != nil {
		t.Error("a PROPFIND was made for a file that is not a video")
	}
	if got := testutil.ToFloat64(h.metrics.HTTPRequests.WithLabelValues(RouteProxy, "200")); got != 4 {
		t.Errorf("proxy metric = %v, want 4", got)
	}
}

func TestForwardsTheVerdictOfThePlatform(t *testing.T) {
	h := newHarness(t)

	for code, exception := range map[int]string{
		http.StatusUnauthorized: `Sabre\DAV\Exception\NotAuthenticated`,
		http.StatusForbidden:    "",
		http.StatusNotFound:     `Sabre\DAV\Exception\NotFound`,
	} {
		h.auth.errs[clipPath] = httpx.StatusError{Code: code}
		w := h.get(t, clipPath+"?preview=1")
		if w.Code != code {
			t.Errorf("platform said %d, we said %d", code, w.Code)
		}
		if !strings.Contains(w.Body.String(), `xmlns:d="DAV"`) || !strings.Contains(w.Body.String(), exception) {
			t.Errorf("%d: body %q", code, w.Body.String())
		}
	}
	h.auth.errs[clipPath] = httpx.StatusError{Code: http.StatusNotFound}
	if !strings.Contains(h.get(t, clipPath+"?preview=1").Body.String(), "File with name clip.mp4 could not be located") {
		t.Error("404 without the wording of the platform")
	}
	if got := h.auth.headers.Get("Authorization"); got != "Bearer token" {
		t.Errorf("the PROPFIND carried Authorization %q", got)
	}

	delete(h.auth.errs, clipPath)
	h.auth.errs[clipPath] = errors.New("boom")
	if w := h.get(t, clipPath+"?preview=1"); w.Code != http.StatusBadGateway {
		t.Errorf("platform down: %d", w.Code)
	}
}

// A file still in postprocessing: the platform says 425 on the PROPFIND, the
// answer is the same 425 the web knows how to wait on, and no job is raised
// because the event of the upload is on its way.
func TestWaitsForPostprocessing(t *testing.T) {
	h := newHarness(t)
	h.auth.errs[clipPath] = httpx.StatusError{Code: http.StatusTooEarly}

	w := h.get(t, clipPath+"?preview=1&x=64&y=64")
	if w.Code != http.StatusTooEarly || w.Header().Get("Retry-After") != "5" {
		t.Errorf("%d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if len(h.queue.jobs) != 0 || h.masters.heads.Load() != 0 {
		t.Error("the bucket or the queue were touched for a file in processing")
	}
}

// A request signed for a public link, the way the web asks for the preview
// of a shared video without any header, is authorised with a HEAD: the
// platform takes the signature on GET and HEAD only.
func TestSignedRequestIsAuthorisedByHead(t *testing.T) {
	h := newHarness(t)
	h.ready()
	public := "/remote.php/dav/public-files/tok3n/clip.mp4"
	h.auth.resources[public] = clip

	w := h.get(t, public+"?signature=abc&expiration=2030-01-01T00%3A00%3A00Z&preview=1&x=64&y=64")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if h.auth.heads != 1 || h.auth.signed != 1 {
		t.Errorf("heads %d, signed %d, want one signed HEAD", h.auth.heads, h.auth.signed)
	}

	// Without a signature the PROPFIND stays.
	h.get(t, clipPath+"?preview=1&x=64&y=64")
	if h.auth.heads != 1 {
		t.Errorf("an unsigned request was authorised by HEAD")
	}
}

func TestRefusesBadDimensions(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{"x=0", "y=-1", "x=abc"} {
		w := h.get(t, clipPath+"?preview=1&"+q)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Cannot set") {
			t.Errorf("%s: %d %q", q, w.Code, w.Body.String())
		}
	}
}

func TestRaisesAnUrgentJobAndAnswersTooEarly(t *testing.T) {
	h := newHarness(t)

	w := h.get(t, clipPath+"?preview=1&x=64&y=64")
	if w.Code != http.StatusTooEarly || w.Header().Get("Retry-After") != "5" {
		t.Fatalf("%d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if len(h.queue.jobs) != 1 || h.queue.subjects[0] != queue.SubjectUrgent {
		t.Fatalf("jobs %v on %v", h.queue.jobs, h.queue.subjects)
	}
	job := h.queue.jobs[0]
	if job.StorageID != "s1" || job.SpaceID != "sp1" || job.FileID != "f1" || job.ETag != clipETag || job.Name != "clip.mp4" || job.Size != 1234 {
		t.Errorf("job %+v", job)
	}

	// The same file asked again within the window: no second look, no
	// second job.
	w = h.get(t, clipPath+"?preview=1&x=128&y=128")
	if w.Code != http.StatusTooEarly || len(h.queue.jobs) != 1 || h.masters.heads.Load() != 1 {
		t.Errorf("second request: %d, jobs %d, heads %d", w.Code, len(h.queue.jobs), h.masters.heads.Load())
	}
	if got := testutil.ToFloat64(h.metrics.CacheHits.WithLabelValues(LevelMemory)); got != 1 {
		t.Errorf("memory hits = %v, want 1", got)
	}

	// Ready now: the index is bypassed once its window is over.
	h.handler.states.forget("f1")
	h.ready()
	if w := h.get(t, clipPath+"?preview=1&x=64&y=64"); w.Code != http.StatusOK {
		t.Errorf("after generation: %d %s", w.Code, w.Body.String())
	}
}

func TestServesTheVariantAndCachesIt(t *testing.T) {
	h := newHarness(t)
	h.ready()

	w := h.get(t, clipPath+"?preview=1&x=36&y=36&processor=fit")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, max-age=86400" {
		t.Errorf("Cache-Control %q", cc)
	}
	etag := w.Header().Get("ETag")
	if etag != `"e1-fit-36x36"` {
		t.Errorf("ETag %q", etag)
	}
	// 36 snaps to 64 on the grid, fit keeps the aspect ratio.
	if size, err := render.Size(w.Body.Bytes()); err != nil || size != image.Pt(64, 36) {
		t.Errorf("variant is %v, %v", size, err)
	}

	// The browser has it.
	w = h.get(t, clipPath+"?preview=1&x=36&y=36&processor=fit", "If-None-Match", etag)
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d, %d bytes", w.Code, w.Body.Len())
	}

	// Another size of the same file: the master comes from the disk, the
	// bucket is read once.
	w = h.get(t, clipPath+"?preview=1&x=64&y=64")
	if w.Code != http.StatusOK {
		t.Fatalf("second size: %d", w.Code)
	}
	if size, _ := render.Size(w.Body.Bytes()); size != image.Pt(64, 64) {
		t.Errorf("thumbnail processor gave %v, want 64x64", size)
	}
	if h.masters.gets.Load() != 1 {
		t.Errorf("the master was read %d times from the bucket", h.masters.gets.Load())
	}

	// The same size again: the variant comes from the disk.
	before := testutil.ToFloat64(h.metrics.CacheHits.WithLabelValues(LevelDisk))
	if w := h.get(t, clipPath+"?preview=1&x=64&y=64"); w.Code != http.StatusOK {
		t.Fatalf("third: %d", w.Code)
	}
	if after := testutil.ToFloat64(h.metrics.CacheHits.WithLabelValues(LevelDisk)); after <= before {
		t.Errorf("disk hits %v -> %v", before, after)
	}

	// HEAD carries the headers and no body.
	w = h.do(t, http.MethodHead, clipPath+"?preview=1&x=64&y=64")
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("ETag") == "" {
		t.Errorf("HEAD: %d, %d bytes, ETag %q", w.Code, w.Body.Len(), w.Header().Get("ETag"))
	}
}

// A master replaced under the same version of the file, by an import, is
// served once the index asks the bucket again: the caches are keyed by the
// master object too.
func TestReplacedMasterIsServed(t *testing.T) {
	h := newHarness(t)
	h.ready()

	first := h.get(t, clipPath+"?preview=1&x=64&y=64&processor=fit")
	if size, _ := render.Size(first.Body.Bytes()); size != image.Pt(64, 36) {
		t.Fatalf("first variant is %v", size)
	}

	// The import stores a portrait master; the index still trusts the old one.
	h.masters.master = &store.Master{ETag: clipETag, Mime: "video/mp4", ObjectETag: "object-2"}
	h.masters.frame = frame(720, 1280)
	if size, _ := render.Size(h.get(t, clipPath+"?preview=1&x=64&y=64&processor=fit").Body.Bytes()); size != image.Pt(64, 36) {
		t.Errorf("served %v before the index expired, want the cached one", size)
	}

	h.handler.states.forget("f1")
	if size, _ := render.Size(h.get(t, clipPath+"?preview=1&x=64&y=64&processor=fit").Body.Bytes()); size != image.Pt(36, 64) {
		t.Errorf("served %v after the index expired, want the new master", size)
	}
}

func TestAnswersNotFoundForAFailedGeneration(t *testing.T) {
	h := newHarness(t)
	h.masters.master = &store.Master{ETag: "old"}
	h.masters.failure = &store.Failure{ETag: clipETag, Error: "no frame"}

	w := h.get(t, clipPath+"?preview=1")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "clip.mp4") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	if len(h.queue.jobs) != 0 {
		t.Error("a job was raised for a version that failed")
	}

	// A failure of an older version does not count.
	h.handler.states.forget("f1")
	h.masters.failure.ETag = "older"
	if w := h.get(t, clipPath+"?preview=1"); w.Code != http.StatusTooEarly || len(h.queue.jobs) != 1 {
		t.Errorf("old failure: %d, jobs %d", w.Code, len(h.queue.jobs))
	}
}

func TestBucketAndQueueOutages(t *testing.T) {
	h := newHarness(t)

	h.masters.err = errors.New("s3 down")
	if w := h.get(t, clipPath+"?preview=1"); w.Code != http.StatusBadGateway {
		t.Errorf("bucket down: %d", w.Code)
	}

	h.masters.err = nil
	h.queue.err = errors.New("nats down")
	if w := h.get(t, clipPath+"?preview=1"); w.Code != http.StatusBadGateway {
		t.Errorf("queue down: %d", w.Code)
	}
}

func TestPassesOnADirectory(t *testing.T) {
	h := newHarness(t)
	h.auth.resources["/dav/spaces/s1$sp1/dir.mp4"] = &httpx.Resource{FileID: "s1$sp1!d", SpaceID: "sp1", OpaqueID: "d", IsDir: true}

	if w := h.get(t, "/dav/spaces/s1$sp1/dir.mp4?preview=1"); w.Body.String() != proxied {
		t.Errorf("a directory named like a video: %d %q", w.Code, w.Body.String())
	}
}

func frame(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, h/2, color.RGBA{R: uint8(x), A: 255})
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}
