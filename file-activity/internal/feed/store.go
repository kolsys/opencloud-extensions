package feed

import (
	"context"
	"encoding/json"
	"fmt"
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

// Store is the stream of the feed.
type Store struct {
	conn    *jsx.Conn
	stream  string
	subject string
}

// Open converges the stream to the configuration and returns the store.
func Open(ctx context.Context, conn *jsx.Conn, cfg config.Stream) (*Store, error) {
	if _, err := conn.EnsureStream(ctx, jetstream.StreamConfig{
		Name:        cfg.Name,
		Description: "Feed of file changes published by file-activity.",
		Subjects:    []string{cfg.Subject},
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
	ack, err := s.conn.Publish(ctx, s.subject, event.ID, body)
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

// Read returns up to limit events after the cursor since. A cursor older
// than the first available event minus one means the client missed events
// the stream has already dropped: it gets ErrCursorTooOld and has to resync.
// A caught up client gets an empty page with Next equal to since.
func (s *Store) Read(ctx context.Context, since uint64, limit int) (Page, error) {
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

	messages, err := s.conn.ReadFrom(ctx, s.stream, since+1, limit)
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
	return page, nil
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
