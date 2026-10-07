package preview

import (
	"context"
	"crypto/sha256"
	"strconv"
	"sync"
	"time"
)

// Outcomes of the gate, the label of its metric.
const (
	// GateFree is a variant the platform has answered before: no slot taken.
	GateFree = "free"
	// GateSlot is a variant the platform sees for the first time.
	GateSlot = "slot"
	// GateBusy is a request turned away: no slot came free in time.
	GateBusy = "busy"
	// GateGone is a client that hung up while waiting.
	GateGone = "gone"
)

const (
	// gateWait is how long a request waits for a slot, or for the same
	// variant in flight, before it is told to come back.
	gateWait = 5 * time.Second
	// retryAfterBusy is the pause a client takes after the gate turned it
	// away: a slot frees within a second or two, the web polls no faster.
	retryAfterBusy = 2

	// seenPerGeneration bounds the memory of the known variants: two
	// generations of this many 16-byte keys stay within a few megabytes.
	seenPerGeneration = 50_000
	// seenTTL is how long a variant is trusted to sit in the cache of the
	// platform; after it the request takes a slot once more, for the time of
	// a cache hit there. A cache wiped on the platform is forgotten here
	// within two of these, or at a restart.
	seenTTL = 12 * time.Hour
)

// variantKey names one variant of one version of a file the way the platform
// caches it.
type variantKey [16]byte

// gate bounds how many previews the platform generates at once for the
// requests passed on to it. The limit of the platform counts every preview
// request alike, cached or not, so it stays off and the gate does the job:
// a variant the platform has answered before goes through freely, a new one
// takes a slot, and the other requests for the same new variant wait for the
// first instead of taking a slot each.
type gate struct {
	slots chan struct{}
	wait  time.Duration
	seen  *seenSet

	mu      sync.Mutex
	flights map[variantKey]chan struct{}
}

func newGate(slots int) *gate {
	return &gate{
		slots:   make(chan struct{}, slots),
		wait:    gateWait,
		seen:    newSeenSet(seenPerGeneration, seenTTL),
		flights: map[variantKey]chan struct{}{},
	}
}

// admit decides how a request goes to the platform. With GateSlot the caller
// passes the request on and calls release with whether the platform answered
// the variant; every other outcome comes without a release.
func (g *gate) admit(ctx context.Context, key variantKey) (string, func(answered bool)) {
	timer := time.NewTimer(g.wait)
	defer timer.Stop()

	for {
		if g.seen.has(key) {
			return GateFree, nil
		}

		g.mu.Lock()
		flight, inFlight := g.flights[key]
		if !inFlight {
			flight = make(chan struct{})
			g.flights[key] = flight
		}
		g.mu.Unlock()

		if inFlight {
			// Somebody asks the platform for this variant right now: once it
			// is answered, this request goes through as a known one.
			select {
			case <-flight:
				continue
			case <-ctx.Done():
				return GateGone, nil
			case <-timer.C:
				return GateBusy, nil
			}
		}

		select {
		case g.slots <- struct{}{}:
			return GateSlot, func(answered bool) {
				if answered {
					g.seen.add(key)
				}
				<-g.slots
				g.land(key, flight)
			}
		case <-ctx.Done():
			g.land(key, flight)
			return GateGone, nil
		case <-timer.C:
			g.land(key, flight)
			return GateBusy, nil
		}
	}
}

// land ends the flight of a variant and wakes the requests waiting for it.
func (g *gate) land(key variantKey, flight chan struct{}) {
	g.mu.Lock()
	delete(g.flights, key)
	g.mu.Unlock()
	close(flight)
}

// seenSet remembers the variants the platform has answered, in two
// generations: new entries go to the current one, both are consulted, and
// the previous one goes when the current one is full or old enough.
type seenSet struct {
	mu       sync.Mutex
	current  map[variantKey]struct{}
	previous map[variantKey]struct{}
	since    time.Time
	max      int
	ttl      time.Duration
	now      func() time.Time
}

func newSeenSet(maxEntries int, ttl time.Duration) *seenSet {
	return &seenSet{
		current:  map[variantKey]struct{}{},
		previous: map[variantKey]struct{}{},
		since:    time.Now(),
		max:      maxEntries,
		ttl:      ttl,
		now:      time.Now,
	}
}

func (s *seenSet) has(key variantKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotateIfDue()
	if _, ok := s.current[key]; ok {
		return true
	}
	_, ok := s.previous[key]
	return ok
}

func (s *seenSet) add(key variantKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotateIfDue()
	s.current[key] = struct{}{}
	if len(s.current) >= s.max {
		s.rotate()
	}
}

// rotateIfDue ages the generations by time: an entry lives between one and
// two ttl, and a quiet set does not keep it any longer.
func (s *seenSet) rotateIfDue() {
	switch age := s.now().Sub(s.since); {
	case age >= 2*s.ttl:
		s.rotate()
		s.rotate()
	case age >= s.ttl:
		s.rotate()
	}
}

func (s *seenSet) rotate() {
	s.previous = s.current
	s.current = map[variantKey]struct{}{}
	s.since = s.now()
}

// key names the variant the platform caches for this request: the path and
// the version of the file, the size and the processor. The signature of a
// public link is left out, it changes with every link and every day.
func (r *request) key() variantKey {
	h := sha256.New()
	for _, part := range []string{r.davPath, r.version, strconv.Itoa(r.box.X), strconv.Itoa(r.box.Y), r.processor} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	var key variantKey
	copy(key[:], h.Sum(nil))
	return key
}
