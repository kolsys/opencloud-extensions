package feed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/config"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/errors"
)

// duplicateWindow is how long the same event id is recognised on publish.
// The consumer of main-queue acknowledges only after publishing, so a crash
// between the two republishes the same event on restart; the window makes
// that a no-op.
const duplicateWindow = 2 * time.Hour

// Store is the stream of the feed. An event is published to the subject of
// the feed followed by the space it happened in, so that a reader of some
// spaces is served by the filter of the server.
type Store struct {
	conn    streamConn
	stream  string
	subject string
}

// streamConn is the part of the JetStream connection the store works with.
type streamConn interface {
	Bounds(ctx context.Context, stream string) (jsx.Bounds, error)
	Publish(ctx context.Context, subject, id string, data []byte) (jsx.Ack, error)
	ReadFrom(ctx context.Context, stream string, seq uint64, limit int, subjects ...string) ([]jsx.Message, error)
}

// Scope is the part of the feed a reader sees. The zero value sees nothing.
type Scope struct {
	all    bool
	spaces []string
}

// Everything is the scope of a reader of the whole feed.
func Everything() Scope {
	return Scope{all: true}
}

// InSpaces is the scope of a reader of the given spaces only.
func InSpaces(spaceIDs ...string) Scope {
	return Scope{spaces: spaceIDs}
}

// All reports whether the scope is the whole feed.
func (s Scope) All() bool {
	return s.all
}

// Spaces returns the spaces of a scope that is not the whole feed.
func (s Scope) Spaces() []string {
	return s.spaces
}

// Open converges the stream to the configuration and returns the store.
func Open(ctx context.Context, conn *jsx.Conn, cfg config.Stream) (*Store, error) {
	if _, err := conn.EnsureStream(ctx, jetstream.StreamConfig{
		Name:        cfg.Name,
		Description: "Feed of file changes published by file-activity.",
		Subjects:    []string{cfg.Subject + ".>"},
		Retention:   jetstream.LimitsPolicy,
		Storage:     jetstream.FileStorage,
		Discard:     jetstream.DiscardOld,
		MaxAge:      cfg.MaxAge,
		MaxBytes:    int64(cfg.MaxBytes),
		Duplicates:  duplicateWindow,
	}); err != nil {
		return nil, err
	}

	return &Store{conn: conn, stream: cfg.Name, subject: cfg.Subject}, nil
}

// Stream returns the name of the stream, for the consumers of the webhook.
func (s *Store) Stream() string {
	return s.stream
}

// Publish appends an event and returns its sequence number. The id of the
// event is the deduplication key: the same event published twice inside the
// duplicate window is stored once.
func (s *Store) Publish(ctx context.Context, event Event) (uint64, error) {
	event.Seq = 0
	body, err := json.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("feed: encode %s: %w", event.ID, err)
	}
	ack, err := s.conn.Publish(ctx, s.subject+"."+spaceToken(event.SpaceID), event.ID, body)
	if err != nil {
		return 0, err
	}
	return ack.Seq, nil
}

// Head reports the bounds of the feed.
func (s *Store) Head(ctx context.Context) (Head, error) {
	bounds, err := s.conn.Bounds(ctx, s.stream)
	if err != nil {
		return Head{}, err
	}
	return Head{FirstAvailable: bounds.FirstSeq, Last: bounds.LastSeq}, nil
}

// Read returns up to limit events of the scope after the cursor since. A
// cursor older than the first available event minus one means the client
// missed events the stream has already dropped: it gets ErrCursorTooOld and
// has to resync. A caught up client gets an empty page.
//
// Once nothing of the scope follows, Next moves up to the head of the feed:
// a reader of quiet spaces would otherwise keep its cursor until the
// retention passed it by.
func (s *Store) Read(ctx context.Context, since uint64, limit int, scope Scope) (Page, error) {
	head, err := s.Head(ctx)
	if err != nil {
		return Page{}, err
	}

	page := Page{Events: []Event{}, Next: since, FirstAvailable: head.FirstAvailable, Last: head.Last}
	if head.Last == 0 {
		return page, nil
	}
	if since+1 < head.FirstAvailable {
		return page, errors.ErrCursorTooOld
	}
	if since >= head.Last {
		return page, nil
	}

	// Without subjects the read would return every space.
	subjects := s.subjects(scope)
	if len(subjects) == 0 {
		page.Next = head.Last
		return page, nil
	}

	messages, err := s.conn.ReadFrom(ctx, s.stream, since+1, limit, subjects...)
	if err != nil {
		return Page{}, err
	}

	for _, message := range messages {
		event, err := Decode(message.Data, message.Seq)
		if err != nil {
			return Page{}, err
		}
		page.Events = append(page.Events, event)
		page.Next = message.Seq
	}

	// The head was read before the messages, so all of it was stored when
	// the read found nothing more.
	if len(messages) == 0 || messages[len(messages)-1].Pending == 0 {
		page.Next = max(page.Next, head.Last)
	}
	return page, nil
}

// subjects are the filter of a scope. The server refuses overlapping
// filters, so a space listed twice is listed once.
func (s *Store) subjects(scope Scope) []string {
	if scope.all {
		return []string{s.subject + ".>"}
	}

	subjects := make([]string, 0, len(scope.spaces))
	for _, spaceID := range scope.spaces {
		subjects = append(subjects, s.subject+"."+spaceToken(spaceID))
	}
	slices.Sort(subjects)
	return slices.Compact(subjects)
}

// spaceToken is a space id as the last token of a subject. An id that is not
// a safe token goes base64url encoded behind a tilde, which no id taken as
// it is starts with.
func spaceToken(spaceID string) string {
	if safeToken(spaceID) {
		return spaceID
	}
	return "~" + base64.RawURLEncoding.EncodeToString([]byte(spaceID))
}

func safeToken(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// Decode turns a stored body back into an event, with the sequence number of
// the message filled in.
func Decode(body []byte, seq uint64) (Event, error) {
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return Event{}, fmt.Errorf("feed: decode message %d: %w", seq, err)
	}
	event.Seq = seq
	return event, nil
}
