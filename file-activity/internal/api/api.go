// Package api serves the feed over HTTP: a page of events after a cursor and
// the head of the feed.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/kolsys/opencloud-extensions/common/httpx"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/errors"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/metrics"
)

// Limits of a read.
const (
	DefaultLimit = 100
	MaxLimit     = 1000

	// identityTTL is how long the answer of the platform about a credential
	// is reused. A client polls often; asking the platform every time would
	// double its load for nothing.
	identityTTL = time.Minute
)

// Routes of the API, as the proxy of the platform forwards them.
const (
	RouteFeed = "/api/file-activity"
	RouteHead = "/api/file-activity/head"
)

// Feed is what the handler reads. The store of the feed implements it.
type Feed interface {
	Read(ctx context.Context, since uint64, limit int) (feed.Page, error)
	Head(ctx context.Context) (feed.Head, error)
}

// Identifier asks the platform who the caller is. The platform client
// implements it.
type Identifier interface {
	Whoami(ctx context.Context, header http.Header) (*httpx.Identity, error)
}

// Handler answers the requests of the feed.
type Handler struct {
	store    Feed
	platform Identifier
	allowed  map[string]struct{}
	metrics  *metrics.Metrics
	log      *slog.Logger

	mu         sync.Mutex
	identities map[string]cachedIdentity
}

type cachedIdentity struct {
	username string
	expires  time.Time
}

// New returns a handler. An empty allowed list lets every authenticated user
// read the feed.
func New(store Feed, platform Identifier, allowed []string, m *metrics.Metrics, log *slog.Logger) *Handler {
	h := &Handler{
		store:      store,
		platform:   platform,
		metrics:    m,
		log:        log,
		identities: map[string]cachedIdentity{},
	}
	if len(allowed) > 0 {
		h.allowed = make(map[string]struct{}, len(allowed))
		for _, user := range allowed {
			h.allowed[user] = struct{}{}
		}
	}
	return h
}

// Register mounts the routes on the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET "+RouteFeed, h.counted(h.authorized(h.feed)))
	mux.Handle("GET "+RouteHead, h.counted(h.authorized(h.head)))
}

func (h *Handler) feed(w http.ResponseWriter, r *http.Request) {
	since, err := parseUint(r.URL.Query().Get("since"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "since must be a non-negative integer")
		return
	}
	limit, err := parseUint(r.URL.Query().Get("limit"), DefaultLimit)
	if err != nil || limit < 1 || limit > MaxLimit {
		writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(MaxLimit))
		return
	}

	page, err := h.store.Read(r.Context(), since, int(limit))
	switch {
	case stderrors.Is(err, errors.ErrCursorTooOld):
		writeJSON(w, http.StatusGone, struct {
			FirstAvailable uint64 `json:"first_available"`
			Last           uint64 `json:"last"`
		}{page.FirstAvailable, page.Last})
		return
	case err != nil:
		h.log.Error("read feed failed", slog.Uint64("since", since), slog.Any("error", err))
		writeError(w, http.StatusBadGateway, "the feed is not available")
		return
	}

	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) head(w http.ResponseWriter, r *http.Request) {
	head, err := h.store.Head(r.Context())
	if err != nil {
		h.log.Error("read head failed", slog.Any("error", err))
		writeError(w, http.StatusBadGateway, "the feed is not available")
		return
	}
	writeJSON(w, http.StatusOK, head)
}

// authorized enforces the list of allowed users. The proxy of the platform
// has already authenticated the caller; this asks the platform who it is.
func (h *Handler) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.allowed == nil {
			next(w, r)
			return
		}

		username, err := h.identify(r.Context(), r.Header)
		if err != nil {
			if code, ok := httpx.Status(err); ok {
				writeError(w, code, http.StatusText(code))
				return
			}
			h.log.Error("identify caller failed", slog.Any("error", err))
			writeError(w, http.StatusBadGateway, "the platform did not answer")
			return
		}

		if _, ok := h.allowed[username]; !ok {
			writeError(w, http.StatusForbidden, errors.ErrNotAllowed.Error())
			return
		}
		next(w, r)
	}
}

// identify returns the user name behind the credentials of the request,
// from the cache when the same credential was seen recently.
func (h *Handler) identify(ctx context.Context, header http.Header) (string, error) {
	key := credentialKey(header)

	h.mu.Lock()
	cached, ok := h.identities[key]
	h.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cached.username, nil
	}

	identity, err := h.platform.Whoami(ctx, header)
	if err != nil {
		return "", err
	}

	h.mu.Lock()
	h.identities[key] = cachedIdentity{username: identity.Username, expires: time.Now().Add(identityTTL)}
	h.mu.Unlock()
	return identity.Username, nil
}

// credentialKey hashes the credentials of a request, so that the cache never
// holds a token in clear.
func credentialKey(header http.Header) string {
	sum := sha256.New()
	for _, name := range httpx.ForwardedHeaders {
		sum.Write([]byte(name))
		sum.Write([]byte{0})
		sum.Write([]byte(header.Get(name)))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// counted records the status code of every answer.
func (h *Handler) counted(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next(recorder, r)
		h.metrics.HTTPRequests.WithLabelValues(strconv.Itoa(recorder.code)).Inc()
	})
}

type statusRecorder struct {
	http.ResponseWriter

	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func parseUint(value string, def uint64) (uint64, error) {
	if value == "" {
		return def, nil
	}
	return strconv.ParseUint(value, 10, 64)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(encoded) //nolint:gosec // G705: JSON of our own structs, not user input
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}
