package preview

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/cache"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/metrics"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/render"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

const (
	photoA = "/remote.php/dav/spaces/s1$sp1/photos/a.jpg?preview=1&x=64&y=64&c=e1"
	photoB = "/remote.php/dav/spaces/s1$sp1/photos/b.jpg?preview=1&x=64&y=64&c=e2"
	photoC = "/remote.php/dav/spaces/s1$sp1/photos/c.jpg?preview=1&x=64&y=64&c=e3"
)

// holdingUpstream stands for the webdav service: it answers at once, except
// the requests with hold=1, which it keeps until released.
type holdingUpstream struct {
	release  chan struct{}
	received atomic.Int32
	status   atomic.Int32
}

func newHoldingUpstream() *holdingUpstream {
	u := &holdingUpstream{release: make(chan struct{})}
	u.status.Store(http.StatusOK)
	return u
}

func (u *holdingUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.received.Add(1)
	if r.URL.Query().Get("hold") == "1" {
		<-u.release
	}
	w.WriteHeader(int(u.status.Load()))
	_, _ = w.Write([]byte(proxied))
}

func (u *holdingUpstream) waitReceived(t *testing.T, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for u.received.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the platform received %d requests, want %d", u.received.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *harness) gateCounts(t *testing.T, want map[string]float64) {
	t.Helper()
	for _, outcome := range []string{GateFree, GateSlot, GateBusy, GateGone} {
		if got := testutil.ToFloat64(h.metrics.Gate.WithLabelValues(outcome)); got != want[outcome] {
			t.Errorf("gate %s = %v, want %v", outcome, got, want[outcome])
		}
	}
}

// One slot, held by b: a is known and passes, c is new and is told to come
// back, and comes back to a free slot.
func TestGateLetsAKnownVariantThroughAndTurnsANewOneAway(t *testing.T) {
	up := newHoldingUpstream()
	h := newHarnessWith(t, 1, up.serve)
	h.handler.gate.wait = 50 * time.Millisecond

	if w := h.get(t, photoA); w.Code != http.StatusOK {
		t.Fatalf("first request for a: %d", w.Code)
	}

	held := make(chan *httptest.ResponseRecorder, 1)
	go func() { held <- h.get(t, photoB+"&hold=1") }()
	up.waitReceived(t, 2)

	if w := h.get(t, photoA); w.Code != http.StatusOK {
		t.Errorf("known variant while the slot is held: %d", w.Code)
	}
	w := h.get(t, photoC)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "2" {
		t.Errorf("new variant while the slot is held: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if !strings.Contains(w.Body.String(), `xmlns:d="DAV"`) {
		t.Error("429 without the body of the platform")
	}
	if got := up.received.Load(); got != 3 {
		t.Errorf("the platform received %d requests, want 3: the turned away one must not reach it", got)
	}

	close(up.release)
	if w := <-held; w.Code != http.StatusOK {
		t.Errorf("held request: %d", w.Code)
	}
	if w := h.get(t, photoC); w.Code != http.StatusOK {
		t.Errorf("c once the slot is free: %d", w.Code)
	}

	h.gateCounts(t, map[string]float64{GateSlot: 3, GateFree: 1, GateBusy: 1})
	if got := testutil.ToFloat64(h.metrics.HTTPRequests.WithLabelValues(RouteProxy, "429")); got != 1 {
		t.Errorf("proxy 429 metric = %v, want 1", got)
	}
}

// Two requests for the same new variant: the second waits for the first and
// goes through as a known one, without a slot of its own.
func TestGateSharesAVariantInFlight(t *testing.T) {
	up := newHoldingUpstream()
	h := newHarnessWith(t, 1, up.serve)

	leader := make(chan *httptest.ResponseRecorder, 1)
	go func() { leader <- h.get(t, photoA+"&hold=1") }()
	up.waitReceived(t, 1)

	follower := make(chan *httptest.ResponseRecorder, 1)
	go func() { follower <- h.get(t, photoA) }()
	time.Sleep(50 * time.Millisecond)
	if got := up.received.Load(); got != 1 {
		t.Fatalf("the follower reached the platform while the leader was in flight")
	}

	close(up.release)
	if w := <-leader; w.Code != http.StatusOK {
		t.Errorf("leader: %d", w.Code)
	}
	if w := <-follower; w.Code != http.StatusOK {
		t.Errorf("follower: %d", w.Code)
	}
	if got := up.received.Load(); got != 2 {
		t.Errorf("the platform received %d requests, want 2", got)
	}
	h.gateCounts(t, map[string]float64{GateSlot: 1, GateFree: 1})
}

func TestGateForgetsWhatThePlatformDidNotAnswer(t *testing.T) {
	up := newHoldingUpstream()
	up.status.Store(http.StatusNotFound)
	h := newHarnessWith(t, 2, up.serve)

	for range 2 {
		if w := h.get(t, photoA); w.Code != http.StatusNotFound {
			t.Errorf("the verdict of the platform was %d, want 404", w.Code)
		}
	}
	h.gateCounts(t, map[string]float64{GateSlot: 2})

	up.status.Store(http.StatusOK)
	h.get(t, photoA)
	h.get(t, photoA)
	h.gateCounts(t, map[string]float64{GateSlot: 3, GateFree: 1})
}

// The web asks for the preview of a public link with a HEAD first; the
// platform generates on it like on a GET.
func TestGateTrustsAHead(t *testing.T) {
	h := newHarnessWith(t, 1, nil)

	if w := h.do(t, http.MethodHead, photoA); w.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", w.Code)
	}
	if w := h.get(t, photoA); w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	h.gateCounts(t, map[string]float64{GateSlot: 1, GateFree: 1})
}

func TestGateOffPassesEverythingOn(t *testing.T) {
	h := newHarnessWith(t, 0, nil)

	for range 2 {
		if w := h.get(t, photoA); w.Code != http.StatusOK || w.Body.String() != proxied {
			t.Errorf("%d %q, want the answer of the platform", w.Code, w.Body.String())
		}
	}
	h.gateCounts(t, map[string]float64{})
	if got := testutil.ToFloat64(h.metrics.HTTPRequests.WithLabelValues(RouteProxy, "200")); got != 2 {
		t.Errorf("proxy metric = %v, want 2", got)
	}
}

func TestGateLetsAClientGo(t *testing.T) {
	up := newHoldingUpstream()
	h := newHarnessWith(t, 1, up.serve)

	held := make(chan *httptest.ResponseRecorder, 1)
	go func() { held <- h.get(t, photoA+"&hold=1") }()
	up.waitReceived(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodGet, photoB, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	h.handler.ServeHTTP(w, r)
	if w.Body.Len() != 0 || w.Header().Get("Retry-After") != "" {
		t.Errorf("answered a client that left: %d %q", w.Code, w.Body.String())
	}

	close(up.release)
	<-held
	h.gateCounts(t, map[string]float64{GateSlot: 1, GateGone: 1})
}

func TestVariantKeyNamesWhatThePlatformCaches(t *testing.T) {
	key := func(target string) variantKey {
		req, err := parse(httptest.NewRequest(http.MethodGet, target, nil))
		if err != nil {
			t.Fatal(err)
		}
		return req.key()
	}

	base := "/dav/public-files/tok/a.jpg?preview=1&x=64&y=64&c=e1&processor=fit"
	same := key(base)
	if key(base+"&signature=s1&expiration=x1") != same {
		t.Error("the signature of a public link changed the key")
	}
	if key(base+"&a=1&scalingup=0") != same {
		t.Error("parameters the platform does not read changed the key")
	}
	for _, other := range []string{
		"/dav/public-files/tok/a.jpg?preview=1&x=64&y=64&c=e2&processor=fit",
		"/dav/public-files/tok/a.jpg?preview=1&x=128&y=64&c=e1&processor=fit",
		"/dav/public-files/tok/a.jpg?preview=1&x=64&y=64&c=e1&processor=thumbnail",
		"/dav/public-files/tok/b.jpg?preview=1&x=64&y=64&c=e1&processor=fit",
	} {
		if key(other) == same {
			t.Errorf("%s: the same key as %s", other, base)
		}
	}
	if key("/dav/a.jpg?preview=1") != key("/dav/a.jpg?preview=1&x=32&y=32") {
		t.Error("a request without sizes is not the 32x32 of the platform")
	}
}

func TestSeenSetAgesInGenerations(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := newSeenSet(2, time.Hour)
	s.now = func() time.Time { return now }
	s.since = now
	k := func(i byte) variantKey { return variantKey{i} }

	s.add(k(1))
	s.add(k(2))
	s.add(k(3))
	for _, i := range []byte{1, 2, 3} {
		if !s.has(k(i)) {
			t.Errorf("%d gone after one rotation", i)
		}
	}
	s.add(k(4))
	if s.has(k(1)) || s.has(k(2)) {
		t.Error("the oldest generation survived two rotations")
	}
	if !s.has(k(3)) || !s.has(k(4)) {
		t.Error("the previous generation is gone")
	}

	s.add(k(5))
	now = now.Add(time.Hour)
	if !s.has(k(5)) {
		t.Error("an entry one ttl old is gone")
	}
	if s.has(k(3)) {
		t.Error("an entry of the generation before survived a ttl")
	}
	now = now.Add(2 * time.Hour)
	if s.has(k(5)) {
		t.Error("an entry two ttl old survived")
	}
}

// The reverse proxy aborts the handler with a panic when the client hangs up
// while the body is on its way; the slot and the flight must not leak.
func TestGateReturnsTheSlotWhenTheProxyAborts(t *testing.T) {
	aborting := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic(http.ErrAbortHandler)
	})
	disk, err := cache.Open(t.TempDir(), 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	grid, err := render.ParseGrid([]string{"32x32"})
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New(prometheus.NewRegistry())
	h := New(aborting, &fakeAuth{}, &fakeMasters{}, &fakeQueue{}, disk, grid, video.NewMatcher([]string{"mp4"}), 1, m, slog.New(slog.DiscardHandler))
	h.gate.wait = 50 * time.Millisecond

	func() {
		defer func() {
			if r := recover(); r == nil || !errors.Is(r.(error), http.ErrAbortHandler) {
				t.Errorf("recovered %v, want the abort of the proxy", r)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, photoA, nil))
	}()

	// The platform answered a before the client left: a is known, b needs
	// the slot a held.
	for _, target := range []string{photoA, photoB} {
		func() {
			defer func() { _ = recover() }()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
		}()
	}
	for outcome, want := range map[string]float64{GateSlot: 2, GateFree: 1, GateBusy: 0} {
		if got := testutil.ToFloat64(m.Gate.WithLabelValues(outcome)); got != want {
			t.Errorf("gate %s = %v, want %v", outcome, got, want)
		}
	}
	h.gate.mu.Lock()
	defer h.gate.mu.Unlock()
	if len(h.gate.flights) != 0 {
		t.Errorf("%d flights left behind", len(h.gate.flights))
	}
}
