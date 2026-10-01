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

	// callerTTL is how long the answer of the platform about a credential,
	// who it is and which spaces it is a member of, is reused. A client polls
	// often; asking the platform every time would double its load for
	// nothing. It is also how long a space stays readable after the member
	// left it.
	callerTTL = time.Minute
)

// Routes of the API, as the proxy of the platform forwards them.
const (
	RouteFeed = "/api/file-activity"
	RouteHead = "/api/file-activity/head"
)

// Feed is what the handler reads. The store of the feed implements it.
type Feed interface {
	Read(ctx context.Context, since uint64, limit int, scope feed.Scope) (feed.Page, error)
	Head(ctx context.Context) (feed.Head, error)
}

// Platform asks the platform who the caller is and which spaces it is a
// member of. The platform client implements it.
type Platform interface {
	Whoami(ctx context.Context, header http.Header) (*httpx.Identity, error)
	Drives(ctx context.Context, header http.Header) ([]httpx.Drive, error)
}

// Access says who may read the feed and how much of it.
type Access struct {
	// Allowed are the user names that may read; empty lets everyone in.
	Allowed []string
	// FullFeed are the user names that read every space, allowed or not.
	// Everyone else reads the spaces they are a member of.
	FullFeed []string
}

// Handler answers the requests of the feed.
type Handler struct {
	store    Feed
	platform Platform
	allowed  map[string]struct{}
	fullFeed map[string]struct{}
	metrics  *metrics.Metrics
	log      *slog.Logger

	mu      sync.Mutex
	callers map[string]caller
	swept   time.Time
}

// caller is what the platform said about a credential. The user name is
// asked for only when a list of users needs it, the spaces only for a read;
// listed says the spaces are known or not needed.
type caller struct {
	allowed bool
	full    bool
	listed  bool
	spaces  []feed.Space
	expires time.Time
}

// New returns a handler.
func New(store Feed, platform Platform, access Access, m *metrics.Metrics, log *slog.Logger) *Handler {
	return &Handler{
		store:    store,
		platform: platform,
		allowed:  set(access.Allowed),
		fullFeed: set(access.FullFeed),
		metrics:  m,
		log:      log,
		callers:  map[string]caller{},
	}
}

// set returns nil for an empty list, which is how an absent list is told
// from one nobody is on.
func set(users []string) map[string]struct{} {
	if len(users) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(users))
	for _, user := range users {
		out[user] = struct{}{}
	}
	return out
}

// Register mounts the routes on the mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET "+RouteFeed, h.counted(h.feed))
	mux.Handle("GET "+RouteHead, h.counted(h.head))
}

func (h *Handler) feed(w http.ResponseWriter, r *http.Request) {
	who, ok := h.authorize(w, r, true)
	if !ok {
		return
	}

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

	scope := feed.Everything()
	if !who.full {
		ids := make([]string, 0, len(who.spaces))
		for _, space := range who.spaces {
			ids = append(ids, space.ID)
		}
		scope = feed.InSpaces(ids...)
	}

	page, err := h.store.Read(r.Context(), since, int(limit), scope)
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

	if !who.full {
		page.Spaces = who.spaces
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) head(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r, false); !ok {
		return
	}

	head, err := h.store.Head(r.Context())
	if err != nil {
		h.log.Error("read head failed", slog.Any("error", err))
		writeError(w, http.StatusBadGateway, "the feed is not available")
		return
	}
	writeJSON(w, http.StatusOK, head)
}

// authorize answers the request itself when the caller may not read, and
// returns what is known about the caller otherwise. The proxy of the
// platform has already authenticated the caller; this asks the platform who
// it is and, for a read, which spaces it is a member of.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, read bool) (caller, bool) {
	who, err := h.lookup(r.Context(), r.Header, read)
	if err != nil {
		if code, ok := httpx.Status(err); ok {
			writeError(w, code, http.StatusText(code))
			return caller{}, false
		}
		h.log.Error("identify caller failed", slog.Any("error", err))
		writeError(w, http.StatusBadGateway, "the platform did not answer")
		return caller{}, false
	}

	if !who.allowed {
		writeError(w, http.StatusForbidden, errors.ErrNotAllowed.Error())
		return caller{}, false
	}
	return who, true
}

// lookup returns what the platform says about the credentials of the
// request, from the cache when the same credential was seen recently.
func (h *Handler) lookup(ctx context.Context, header http.Header, read bool) (caller, error) {
	named := h.allowed != nil || h.fullFeed != nil
	if !named && !read {
		return caller{allowed: true}, nil
	}

	key := credentialKey(header)
	h.mu.Lock()
	cached, ok := h.callers[key]
	h.mu.Unlock()
	if ok && time.Now().Before(cached.expires) && (cached.listed || !read) {
		return cached, nil
	}

	who := caller{allowed: h.allowed == nil}
	if named {
		identity, err := h.platform.Whoami(ctx, header)
		if err != nil {
			return caller{}, err
		}
		_, who.full = h.fullFeed[identity.Username]
		_, listed := h.allowed[identity.Username]
		who.allowed = who.allowed || listed || who.full
	}
	if read && who.allowed && !who.full {
		drives, err := h.platform.Drives(ctx, header)
		if err != nil {
			return caller{}, err
		}
		who.spaces = spacesOf(drives)
	}
	who.listed = read || who.full || !who.allowed
	who.expires = time.Now().Add(callerTTL)

	h.remember(key, who)
	return who, nil
}

// remember caches what was learnt about a credential. The entries that ran
// out go once per TTL, so that the cache holds the credentials of the last
// minutes only.
func (h *Handler) remember(key string, who caller) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	if now.Sub(h.swept) > callerTTL {
		for stale, cached := range h.callers {
			if now.After(cached.expires) {
				delete(h.callers, stale)
			}
		}
		h.swept = now
	}
	h.callers[key] = who
}

// spacesOf keeps the drives that are spaces of their own. A share the caller
// received is a part of a space of somebody else and is not followed.
func spacesOf(drives []httpx.Drive) []feed.Space {
	spaces := []feed.Space{}
	for _, drive := range drives {
		if drive.SpaceID == "" || (drive.Type != httpx.DrivePersonal && drive.Type != httpx.DriveProject) {
			continue
		}
		spaces = append(spaces, feed.Space{ID: drive.SpaceID, Name: drive.Name, Type: drive.Type})
	}
	return spaces
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
