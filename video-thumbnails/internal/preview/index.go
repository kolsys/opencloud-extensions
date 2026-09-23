package preview

import (
	"container/list"
	"sync"
	"time"
)

// State of the thumbnail of one version of a file, as last seen.
type State uint8

// States a preview request ends in.
const (
	StateUnknown State = iota
	// StateReady means the master of this version is in the bucket.
	StateReady
	// StateGenerating means a job for this version was raised.
	StateGenerating
	// StateFailed means the generation of this version was given up on.
	StateFailed
)

// How long a state is trusted before the bucket is asked again. A ready
// master is checked every few minutes, so that a master replaced under the
// same version of the file, by an import or a resync, is served soon; a job
// in flight is checked often, since the urgent workers finish most of them
// within seconds.
const (
	ttlReady      = 5 * time.Minute
	ttlGenerating = 3 * time.Second
	ttlFailed     = time.Minute

	// maxStates bounds the memory of the index; the oldest entries go.
	maxStates = 100_000
)

// index remembers the state of the files recently asked for, so that a burst
// of requests for the same file costs one round trip to the bucket.
type index struct {
	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List // the front is the newest entry
	max   int
	now   func() time.Time
}

type stateEntry struct {
	fileID string
	etag   string
	state  State
	// master is the ETag of the master object of a ready state, the part of
	// the cache keys that changes when the master is replaced.
	master string
	at     time.Time
}

func newIndex(maxEntries int) *index {
	return &index{items: map[string]*list.Element{}, order: list.New(), max: maxEntries, now: time.Now}
}

// lookup returns the state of a file when it is known for this version and
// still trusted, with the master object of a ready state.
func (i *index) lookup(fileID, etag string) (State, string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	element, ok := i.items[fileID]
	if !ok {
		return StateUnknown, "", false
	}
	entry := element.Value.(*stateEntry)
	if entry.etag != etag || i.now().Sub(entry.at) > ttlOf(entry.state) {
		i.order.Remove(element)
		delete(i.items, fileID)
		return StateUnknown, "", false
	}
	return entry.state, entry.master, true
}

// set records the state of a version of a file; master is the ETag of the
// master object of a ready state.
func (i *index) set(fileID, etag string, state State, master string) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if element, ok := i.items[fileID]; ok {
		i.order.Remove(element)
	}
	i.items[fileID] = i.order.PushFront(&stateEntry{fileID: fileID, etag: etag, state: state, master: master, at: i.now()})

	for len(i.items) > i.max {
		oldest := i.order.Back()
		delete(i.items, oldest.Value.(*stateEntry).fileID)
		i.order.Remove(oldest)
	}
}

// forget drops what is known about a file.
func (i *index) forget(fileID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if element, ok := i.items[fileID]; ok {
		i.order.Remove(element)
		delete(i.items, fileID)
	}
}

func ttlOf(state State) time.Duration {
	switch state {
	case StateReady:
		return ttlReady
	case StateGenerating:
		return ttlGenerating
	case StateFailed:
		return ttlFailed
	default:
		return 0
	}
}
