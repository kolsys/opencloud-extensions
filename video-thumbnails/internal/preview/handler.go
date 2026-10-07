// Package preview serves the previews the proxy of the platform routes to the
// extension: the ones of videos from the masters in the bucket, the rest by
// passing the request on to the webdav service of the platform, through a
// gate that bounds what the platform generates at once.
package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/kolsys/opencloud-extensions/common/httpx"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/cache"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/metrics"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/render"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

// Routes of the metrics: a preview of a video served here, everything else
// passed on to the platform.
const (
	RouteVideo = "preview"
	RouteProxy = "proxy"
)

// Levels of the cache hits metric.
const (
	LevelMemory = "memory"
	LevelDisk   = "disk"
	LevelS3     = "s3"
)

const (
	// retryAfter is the pause the web takes before asking again for a
	// thumbnail that is being generated.
	retryAfter = 5

	// maxAge is how long a browser keeps a preview; the URL carries the
	// version of the file, so a new version is a new URL.
	maxAge = 24 * time.Hour

	contentTypeJPEG = "image/jpeg"
)

// Authorizer asks the platform about a file with the credentials of the
// client: the verdict on the request and the ids at once. A request signed
// for a public link is asked with a HEAD, the platform takes the signature
// on nothing else.
type Authorizer interface {
	PropFind(ctx context.Context, davPath string, header http.Header) (*httpx.Resource, error)
	Head(ctx context.Context, davPath string, query url.Values, header http.Header) (*httpx.Resource, error)
}

// Masters is the bucket side the previews need.
type Masters interface {
	HeadMaster(ctx context.Context, spaceID, fileID string) (*store.Master, error)
	GetMaster(ctx context.Context, spaceID, fileID string) (io.ReadCloser, *store.Master, error)
	Failure(ctx context.Context, spaceID, fileID string) (*store.Failure, error)
}

// Enqueuer raises the job for a thumbnail the web asked for and missed.
type Enqueuer interface {
	Enqueue(ctx context.Context, subject string, job queue.Job) (bool, error)
}

// Handler answers the preview requests of the platform.
type Handler struct {
	proxy   http.Handler
	auth    Authorizer
	masters Masters
	queue   Enqueuer
	cache   *cache.Cache
	grid    render.Grid
	video   *video.Matcher
	states  *index
	gate    *gate
	flight  singleflight.Group
	metrics *metrics.Metrics
	log     *slog.Logger
}

// New returns a handler. The proxy takes everything that is not the preview
// of a video; generations is how many previews the platform makes at once
// for what is passed on to it, zero for no gate.
func New(proxy http.Handler, auth Authorizer, masters Masters, q Enqueuer, disk *cache.Cache, grid render.Grid, matcher *video.Matcher, generations int, m *metrics.Metrics, log *slog.Logger) *Handler {
	h := &Handler{
		proxy:   proxy,
		auth:    auth,
		masters: masters,
		queue:   q,
		cache:   disk,
		grid:    grid,
		video:   matcher,
		states:  newIndex(maxStates),
		metrics: m,
		log:     log,
	}
	if generations > 0 {
		h.gate = newGate(generations)
	}
	return h
}

// ServeHTTP tells the previews of videos from the rest, authorises them
// with a PROPFIND of the client and answers from the bucket, the disk cache
// or with the status of the generation.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req, err := parse(r)
	switch {
	case errors.Is(err, errNotPreview):
		h.passOn(w, r)
		return
	case err != nil:
		h.metrics.HTTPRequests.WithLabelValues(RouteVideo, strconv.Itoa(http.StatusBadRequest)).Inc()
		httpx.WriteDAVError(w, http.StatusBadRequest, err.Error())
		return
	case !h.video.MatchName(req.name):
		h.passPreview(w, r, req)
		return
	}

	recorder := &statusRecorder{ResponseWriter: w}
	h.serve(recorder, r, req)
	h.metrics.HTTPRequests.WithLabelValues(RouteVideo, strconv.Itoa(recorder.status())).Inc()
}

// passOn hands a request to the platform as it came.
func (h *Handler) passOn(w http.ResponseWriter, r *http.Request) {
	recorder := &statusRecorder{ResponseWriter: w}
	h.proxy.ServeHTTP(recorder, r)
	h.metrics.HTTPRequests.WithLabelValues(RouteProxy, strconv.Itoa(recorder.status())).Inc()
}

// passPreview hands the preview of a file that is not a video to the
// platform, through the gate when there is one. A 200 means the platform has
// the variant from now on, by GET or by HEAD alike.
func (h *Handler) passPreview(w http.ResponseWriter, r *http.Request, req *request) {
	if h.gate == nil {
		h.passOn(w, r)
		return
	}

	outcome, release := h.gate.admit(r.Context(), req.key())
	h.metrics.Gate.WithLabelValues(outcome).Inc()
	switch outcome {
	case GateSlot:
		recorder := &statusRecorder{ResponseWriter: w}
		// The proxy aborts with a panic when the client hangs up mid-body;
		// the slot goes back all the same.
		defer func() {
			release(recorder.status() == http.StatusOK)
			h.metrics.HTTPRequests.WithLabelValues(RouteProxy, strconv.Itoa(recorder.status())).Inc()
		}()
		h.proxy.ServeHTTP(recorder, r)
	case GateBusy:
		h.log.Debug("gate busy", slog.String("path", req.davPath))
		h.metrics.HTTPRequests.WithLabelValues(RouteProxy, strconv.Itoa(http.StatusTooManyRequests)).Inc()
		httpx.WriteTooManyRequests(w, retryAfterBusy)
	case GateGone:
		// The client hung up while waiting; there is nobody to answer.
	default:
		h.passOn(w, r)
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, req *request) {
	ctx := r.Context()
	log := h.log.With(slog.String("path", req.davPath))

	resource, err := h.authorize(ctx, r, req)
	if err != nil {
		code, ok := httpx.Status(err)
		switch {
		case ok && code == http.StatusTooEarly:
			// Still in postprocessing after the upload: the event that raises
			// the job is on its way, the web asks again.
			httpx.WriteTooEarly(w, retryAfter)
		case ok:
			// The verdict of the platform on the credentials or the path
			// reaches the client as it is.
			h.writeStatus(w, code, req.name)
		default:
			log.Error("authorisation failed", slog.Any("error", err))
			httpx.WriteDAVError(w, http.StatusBadGateway, "the platform did not answer")
		}
		return
	}
	if resource.IsDir {
		h.proxy.ServeHTTP(w, r)
		return
	}

	etag := req.etagOf(resource.ETag)
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	state, masterETag, err := h.resolve(ctx, resource, req.name, log)
	switch {
	case err != nil:
		log.Error("state of the thumbnail unknown", slog.Any("error", err))
		httpx.WriteDAVError(w, http.StatusBadGateway, "the thumbnail store did not answer")
	case state == StateGenerating:
		httpx.WriteTooEarly(w, retryAfter)
	case state == StateFailed:
		httpx.WriteNotFound(w, req.name)
	default:
		h.render(w, r, req, resource, masterETag, etag, log)
	}
}

// authorize asks the platform about the file with what the client sent: the
// signature of a public link when there is one, the headers otherwise.
func (h *Handler) authorize(ctx context.Context, r *http.Request, req *request) (*httpx.Resource, error) {
	if httpx.Signed(r.URL.Query()) {
		return h.auth.Head(ctx, req.davPath, r.URL.Query(), r.Header)
	}
	return h.auth.PropFind(ctx, req.davPath, r.Header)
}

// resolve finds out where the thumbnail of this version stands, asking the
// index first and the bucket second, and raises the job when there is
// nothing yet. A ready state comes with the ETag of the master object.
func (h *Handler) resolve(ctx context.Context, res *httpx.Resource, name string, log *slog.Logger) (State, string, error) {
	if state, master, ok := h.states.lookup(res.OpaqueID, res.ETag); ok {
		h.metrics.CacheHits.WithLabelValues(LevelMemory).Inc()
		return state, master, nil
	}

	master, err := h.masters.HeadMaster(ctx, res.SpaceID, res.OpaqueID)
	if err != nil && !errors.Is(err, s3store.ErrNotFound) {
		return StateUnknown, "", err
	}
	if master.Current(res.ETag) {
		h.states.set(res.OpaqueID, res.ETag, StateReady, master.ObjectETag)
		return StateReady, master.ObjectETag, nil
	}

	failure, err := h.masters.Failure(ctx, res.SpaceID, res.OpaqueID)
	if err != nil && !errors.Is(err, s3store.ErrNotFound) {
		return StateUnknown, "", err
	}
	if failure != nil && failure.ETag == res.ETag {
		h.states.set(res.OpaqueID, res.ETag, StateFailed, "")
		return StateFailed, "", nil
	}

	job := queue.Job{
		StorageID: res.StorageID,
		SpaceID:   res.SpaceID,
		FileID:    res.OpaqueID,
		ETag:      res.ETag,
		Mime:      res.ContentType,
		Size:      uint64(max(res.Size, 0)),
		Name:      name,
	}
	stored, err := h.queue.Enqueue(ctx, queue.SubjectUrgent, job)
	if err != nil {
		return StateUnknown, "", err
	}
	if stored {
		log.Info("urgent job enqueued", slog.String("job", job.ID()))
	} else {
		log.Debug("urgent job already queued", slog.String("job", job.ID()))
	}
	h.states.set(res.OpaqueID, res.ETag, StateGenerating, "")
	return StateGenerating, "", nil
}

// render answers with the variant of the master a request asks for, from the
// disk cache when it is there. The cache keys carry the version of the file
// and the master object, so a master replaced under the same version is not
// served from what the old one left behind.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, req *request, res *httpx.Resource, masterETag, etag string, log *slog.Logger) {
	ctx := r.Context()
	dir := res.SpaceID + "/" + res.OpaqueID + "/" + res.ETag + "/" + masterETag + "/"

	master, err := h.master(ctx, res, dir+store.MasterName)
	if err != nil {
		if errors.Is(err, s3store.ErrNotFound) {
			// Gone between the check and the read: the file was replaced or
			// removed, the next request starts over.
			h.states.forget(res.OpaqueID)
			httpx.WriteTooEarly(w, retryAfter)
			return
		}
		log.Error("master not readable", slog.Any("error", err))
		httpx.WriteDAVError(w, http.StatusBadGateway, "the thumbnail store did not answer")
		return
	}

	size, err := render.Size(master)
	if err != nil {
		log.Error("master not decodable", slog.Any("error", err))
		httpx.WriteDAVError(w, http.StatusInternalServerError, "the master frame is not an image")
		return
	}
	box := h.grid.Snap(req.box, size)
	processor := processorOf(req.processor)
	key := fmt.Sprintf("%s%s-%dx%d@%s", dir, processor, box.X, box.Y, render.Revision)

	path, ok := h.cache.Get(key)
	if ok {
		h.metrics.CacheHits.WithLabelValues(LevelDisk).Inc()
	} else {
		rendered, err, _ := h.flight.Do(key, func() (any, error) {
			variant, err := render.Variant(master, box, processor)
			if err != nil {
				return nil, err
			}
			return h.cache.Put(key, variant)
		})
		if err != nil {
			log.Error("variant not rendered", slog.Any("error", err))
			httpx.WriteDAVError(w, http.StatusInternalServerError, "the thumbnail could not be rendered")
			return
		}
		path = rendered.(string)
	}

	file, err := os.Open(path) //nolint:gosec // G703: the path is the one the cache built from a hash
	if err != nil {
		log.Error("variant not readable", slog.Any("error", err))
		httpx.WriteDAVError(w, http.StatusInternalServerError, "the thumbnail could not be read")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		httpx.WriteDAVError(w, http.StatusInternalServerError, "the thumbnail could not be read")
		return
	}

	w.Header().Set("Content-Type", contentTypeJPEG)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(maxAge.Seconds())))
	http.ServeContent(w, r, "", info.ModTime(), file)
}

// master returns the bytes of the master of a version, from the disk cache
// or from the bucket. Concurrent misses share one read.
func (h *Handler) master(ctx context.Context, res *httpx.Resource, key string) ([]byte, error) {
	if path, ok := h.cache.Get(key); ok {
		data, err := os.ReadFile(path) //nolint:gosec // G703: the path is the one the cache built from a hash
		if err == nil {
			h.metrics.CacheHits.WithLabelValues(LevelDisk).Inc()
			return data, nil
		}
	}

	data, err, _ := h.flight.Do(key, func() (any, error) {
		body, master, err := h.masters.GetMaster(ctx, res.SpaceID, res.OpaqueID)
		if err != nil {
			return nil, err
		}
		defer body.Close()
		if !master.Current(res.ETag) {
			return nil, s3store.ErrNotFound
		}
		data, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("preview: read master: %w", err)
		}
		h.metrics.CacheHits.WithLabelValues(LevelS3).Inc()
		if _, err := h.cache.Put(key, data); err != nil {
			// The cache is a convenience; the preview is served without it.
			h.log.Warn("master not cached", slog.Any("error", err))
		}
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return data.([]byte), nil
}

// writeStatus answers with the verdict of the platform in its own format.
func (h *Handler) writeStatus(w http.ResponseWriter, code int, name string) {
	if code == http.StatusNotFound {
		httpx.WriteNotFound(w, name)
		return
	}
	httpx.WriteDAVError(w, code, http.StatusText(code))
}

func processorOf(name string) string {
	return render.Processor(name)
}

// statusRecorder keeps the status code for the metrics.
type statusRecorder struct {
	http.ResponseWriter

	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}
