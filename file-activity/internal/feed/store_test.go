package feed

import (
	"context"
	stderrors "errors"
	"slices"
	"strings"
	"testing"

	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/errors"
)

// fakeStream keeps messages the way the server does: in order, with a
// sequence number each, read through filter subjects with a pending count.
type fakeStream struct {
	first    uint64
	messages []stored
	reads    [][]string
}

type stored struct {
	seq     uint64
	subject string
	data    []byte
}

func (f *fakeStream) Bounds(context.Context, string) (jsx.Bounds, error) {
	bounds := jsx.Bounds{FirstSeq: f.first, Messages: uint64(len(f.messages))}
	if len(f.messages) > 0 {
		bounds.LastSeq = f.messages[len(f.messages)-1].seq
	}
	return bounds, nil
}

func (f *fakeStream) Publish(_ context.Context, subject, _ string, data []byte) (jsx.Ack, error) {
	seq := f.first
	if len(f.messages) > 0 {
		seq = f.messages[len(f.messages)-1].seq + 1
	}
	f.messages = append(f.messages, stored{seq: seq, subject: subject, data: data})
	return jsx.Ack{Seq: seq}, nil
}

func (f *fakeStream) ReadFrom(_ context.Context, _ string, seq uint64, limit int, subjects ...string) ([]jsx.Message, error) {
	f.reads = append(f.reads, subjects)

	var matching []stored
	for _, message := range f.messages {
		if message.seq >= seq && matches(message.subject, subjects) {
			matching = append(matching, message)
		}
	}

	out := []jsx.Message{}
	for i, message := range matching {
		if i == limit {
			break
		}
		out = append(out, jsx.Message{Seq: message.seq, Data: message.data, Pending: uint64(len(matching) - i - 1)})
	}
	return out, nil
}

func matches(subject string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	return slices.ContainsFunc(filters, func(filter string) bool {
		if prefix, ok := strings.CutSuffix(filter, ".>"); ok {
			return strings.HasPrefix(subject, prefix+".")
		}
		return subject == filter
	})
}

func newStore(t *testing.T, spaces ...string) (*Store, *fakeStream) {
	t.Helper()

	stream := &fakeStream{first: 1}
	store := &Store{conn: stream, stream: "file-activity", subject: "file-activity.events"}
	for _, space := range spaces {
		if _, err := store.Publish(context.Background(), Event{ID: "event", Type: FileCreated, SpaceID: space}); err != nil {
			t.Fatal(err)
		}
	}
	return store, stream
}

func seqs(page Page) []uint64 {
	out := make([]uint64, 0, len(page.Events))
	for _, event := range page.Events {
		out = append(out, event.Seq)
	}
	return out
}

func TestPublishGoesToTheSubjectOfTheSpace(t *testing.T) {
	_, stream := newStore(t, "b1f74ec4-dd7e-11ef-a543-03775734d0f7", "a.b")

	if got := stream.messages[0].subject; got != "file-activity.events.b1f74ec4-dd7e-11ef-a543-03775734d0f7" {
		t.Errorf("subject = %s", got)
	}
	if got := stream.messages[1].subject; got != "file-activity.events.~YS5i" {
		t.Errorf("subject of an id with a dot = %s", got)
	}
}

// A token is the id itself when that is safe, and never one another id maps
// to.
func TestSpaceToken(t *testing.T) {
	for id, want := range map[string]string{
		"b1f74ec4-dd7e-11ef-a543-03775734d0f7": "b1f74ec4-dd7e-11ef-a543-03775734d0f7",
		"user_1":                               "user_1",
		"a.b":                                  "~YS5i",
		"a*":                                   "~YSo",
		"a b":                                  "~YSBi",
		"~YS5i":                                "~fllTNWk",
		"":                                     "~",
	} {
		token := spaceToken(id)
		if token != want {
			t.Errorf("spaceToken(%q) = %q, want %q", id, token, want)
		}
		if strings.ContainsAny(token, ".*> \t") || token == "" {
			t.Errorf("spaceToken(%q) = %q is not a token", id, token)
		}
	}
}

func TestReadInSpaces(t *testing.T) {
	store, _ := newStore(t, "a", "b", "a", "c", "b", "a")

	page, err := store.Read(context.Background(), 0, 10, InSpaces("a", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if got := seqs(page); !slices.Equal(got, []uint64{1, 3, 4, 6}) {
		t.Errorf("events = %v, want the ones of a and c in stream order", got)
	}
	for _, event := range page.Events {
		if event.SpaceID != "a" && event.SpaceID != "c" {
			t.Errorf("event %d of space %s", event.Seq, event.SpaceID)
		}
	}
	if page.Next != 6 || page.Last != 6 {
		t.Errorf("next = %d, last = %d", page.Next, page.Last)
	}
}

// A full page stops at its last event; the cursor moves to the head only
// once the read found nothing more of the scope.
func TestReadPagesThroughTheScope(t *testing.T) {
	store, _ := newStore(t, "a", "b", "a", "b", "b", "b")

	first, err := store.Read(context.Background(), 0, 1, InSpaces("a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := seqs(first); !slices.Equal(got, []uint64{1}) || first.Next != 1 {
		t.Errorf("first page = %v, next %d", got, first.Next)
	}

	second, err := store.Read(context.Background(), first.Next, 1, InSpaces("a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := seqs(second); !slices.Equal(got, []uint64{3}) || second.Next != 6 {
		t.Errorf("second page = %v, next %d, want 3 and the head", got, second.Next)
	}

	third, err := store.Read(context.Background(), second.Next, 1, InSpaces("a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Events) != 0 || third.Next != 6 {
		t.Errorf("caught up = %v, next %d", seqs(third), third.Next)
	}
}

// A reader whose spaces stay quiet still moves its cursor with the feed, so
// the retention does not pass it by and answer it with a 410.
func TestQuietSpacesKeepTheCursorInsideTheRetention(t *testing.T) {
	store, stream := newStore(t, "a", "b", "b", "b")

	page, err := store.Read(context.Background(), 1, 10, InSpaces("a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 || page.Next != 4 {
		t.Errorf("page = %v, next %d, want empty and the head", seqs(page), page.Next)
	}

	// The retention drops what the reader was told it can skip.
	stream.first = 4
	stream.messages = stream.messages[3:]
	if _, err := store.Read(context.Background(), page.Next, 10, InSpaces("a")); err != nil {
		t.Errorf("read from the advanced cursor: %v", err)
	}
	if _, err := store.Read(context.Background(), 1, 10, InSpaces("a")); !stderrors.Is(err, errors.ErrCursorTooOld) {
		t.Errorf("read from the old cursor: %v, want ErrCursorTooOld", err)
	}
}

// The zero scope and a member of no space read nothing, and the store never
// asks the server without a filter, which would return every space.
func TestEmptyScopeReadsNothing(t *testing.T) {
	store, stream := newStore(t, "a", "b")

	for name, scope := range map[string]Scope{"zero": {}, "no spaces": InSpaces()} {
		page, err := store.Read(context.Background(), 0, 10, scope)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 0 || page.Next != 2 {
			t.Errorf("%s: page = %v, next %d", name, seqs(page), page.Next)
		}
	}
	if len(stream.reads) != 0 {
		t.Errorf("the server was read with %v", stream.reads)
	}
}

func TestEverythingReadsEverySpace(t *testing.T) {
	store, stream := newStore(t, "a", "b", "c")

	page, err := store.Read(context.Background(), 0, 10, Everything())
	if err != nil {
		t.Fatal(err)
	}
	if got := seqs(page); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Errorf("events = %v", got)
	}
	if want := [][]string{{"file-activity.events.>"}}; !slices.EqualFunc(stream.reads, want, slices.Equal) {
		t.Errorf("filters = %v, want %v", stream.reads, want)
	}
}

// The server refuses a consumer whose filters overlap.
func TestSubjectsOfASpaceListedTwice(t *testing.T) {
	store, _ := newStore(t)
	got := store.subjects(InSpaces("b", "a", "b"))
	if want := []string{"file-activity.events.a", "file-activity.events.b"}; !slices.Equal(got, want) {
		t.Errorf("subjects = %v, want %v", got, want)
	}
}
